package calib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The on-node sweep collector, for hwcalib sweep. It holds the node at each
// step of a sweep, a CPU load and optionally a frequency ceiling and a
// package power limit, and records every sample as one MetricsData line in
// the names hwcalib fit reads: the RAPL energy of each package and DRAM
// zone and the HSMP socket power, cpufreq speed and limits, the package
// power limit, the utilization of every logical CPU from /proc/stat with
// cpu.logical_number, and the setpoints it held. It reads only what the
// kernel shows under the sys and proc roots, so a whole-host meter (a BMC,
// a PDU) is not read; a converter can add hw.host.* to the file.
//
// Reading energy_uj needs root: the file is mode 0400 since Linux commit
// 949dd0104c49 ("powercap: restrict energy meter to root access"). A sweep
// writes scaling_max_freq and power limits only when SweepOptions.WriteLimits
// is set, and puts back what it found when it returns.

const (
	// SweepServiceName is service.name of the collector's resource.
	SweepServiceName = "hwcalib"
	// SweepScopeName is the instrumentation scope of the collector's metrics.
	SweepScopeName = "github.com/matbun/joulie/simulator/pkg/calib"
)

// sweepServiceNamespace is service.namespace, the simulator's.
const sweepServiceNamespace = "joulie"

// SweepStep is one setpoint of a sweep: the busy fraction of every logical
// CPU, 0 to 1, and, when positive, the scaling_max_freq written to every
// cpufreq policy, in Hz, and the power limit written to every package, in W.
// Zero leaves the frequency ceiling or the limit as the step before left it.
type SweepStep struct {
	CPUUtilization float64
	FrequencyMaxHz float64
	PowerLimitW    float64
}

// SweepGrid is the full-factorial sweep of the three knobs: power limits
// outermost, then frequency ceilings, then utilizations. An empty list of
// limits or frequencies holds that knob where it is.
func SweepGrid(utils, freqsHz, limitsW []float64) []SweepStep {
	if len(freqsHz) == 0 {
		freqsHz = []float64{0}
	}
	if len(limitsW) == 0 {
		limitsW = []float64{0}
	}
	var out []SweepStep
	for _, l := range limitsW {
		for _, f := range freqsHz {
			for _, u := range utils {
				out = append(out, SweepStep{CPUUtilization: u, FrequencyMaxHz: f, PowerLimitW: l})
			}
		}
	}
	return out
}

// LoadDriver runs the CPU load of a sweep. SetUtilization keeps every CPU
// busy for the share u of the time from now on; Stop ends the load.
type LoadDriver interface {
	SetUtilization(u float64) error
	Stop()
}

// Clock is the time of a sweep. Sleep returns after d, or with the
// context's error when it ends first.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// WallClock is the host's clock.
type WallClock struct{}

// Now is time.Now.
func (WallClock) Now() time.Time { return time.Now() }

// Sleep waits d or until ctx ends.
func (WallClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SweepOptions configure Sweep. SysRoot and ProcRoot default to /sys and
// /proc. Node names the resource (service.instance.id and host.name). Each
// step lasts Hold and is sampled every Period, the first sample one Period
// into it. Load runs the CPU load, and Sweep stops it when it returns; nil
// is a DutyCycleLoad with DutyPeriod on every CPU /proc/stat lists. The
// setpoint record describes the load with no class (the models' default
// weights) and memory intensity 0, as a DutyCycleLoad spinning on
// registers is. Clock defaults to WallClock. Out receives one MetricsData
// per line; Log, when set, a line per step and any warning.
type SweepOptions struct {
	SysRoot     string
	ProcRoot    string
	Node        string
	Steps       []SweepStep
	Hold        time.Duration
	Period      time.Duration
	WriteLimits bool
	Load        LoadDriver
	DutyPeriod  time.Duration
	Clock       Clock
	Out         io.Writer
	Log         io.Writer
}

// Sweep runs the sweep o describes and returns the number of samples it
// wrote. It reads the host before it starts and fails when no package
// energy or power can be read, naming root when a file is not readable. A
// step that sets a frequency ceiling or a power limit needs o.WriteLimits.
// Every file it wrote is put back as it was found when it returns, also
// when ctx ends or a step fails; the lines written so far stay valid.
func Sweep(ctx context.Context, o SweepOptions) (samples int, err error) {
	// The load stops first, then every file written is put back.
	var h *sweepHost
	defer func() {
		if o.Load != nil {
			o.Load.Stop()
		}
		if h != nil {
			if rerr := h.restore(); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err := o.Validate(); err != nil {
		return 0, err
	}
	if o.Out == nil {
		return 0, errors.New("sweep: no output")
	}
	if o.SysRoot == "" {
		o.SysRoot = "/sys"
	}
	if o.ProcRoot == "" {
		o.ProcRoot = "/proc"
	}
	if o.Clock == nil {
		o.Clock = WallClock{}
	}
	// The load's goroutines may warn while the sweep logs a step.
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		if o.Log != nil {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	h, err = sweepDiscover(o.SysRoot, o.ProcRoot)
	if err != nil {
		return 0, err
	}
	if err := h.checkKnobs(o.Steps); err != nil {
		return 0, err
	}
	if o.Load == nil {
		o.Load = NewDutyCycleLoad(h.cpus, o.DutyPeriod, func(err error) {
			logf("warning: the load runs unpinned: %v", err)
		})
	}

	res := otlpjson.Resource{Attributes: []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrServiceName, SweepServiceName),
		otlpjson.String(semconv.AttrServiceNamespace, sweepServiceNamespace),
		otlpjson.String(semconv.AttrServiceInstanceID, o.Node),
		otlpjson.String(semconv.AttrHostName, o.Node),
	}}
	scope := otlpjson.Scope{Name: SweepScopeName}
	enc := json.NewEncoder(o.Out)
	enc.SetEscapeHTML(false)

	start := o.Clock.Now()
	if err := h.read(start); err != nil {
		return 0, err
	}
	n := int(o.Hold / o.Period)
	var held SweepStep
	for i, st := range o.Steps {
		if st.FrequencyMaxHz > 0 && st.FrequencyMaxHz != held.FrequencyMaxHz {
			if err := h.writeFrequencyMax(st.FrequencyMaxHz); err != nil {
				return samples, err
			}
			held.FrequencyMaxHz = st.FrequencyMaxHz
		}
		if st.PowerLimitW > 0 && st.PowerLimitW != held.PowerLimitW {
			if err := h.writePowerLimit(st.PowerLimitW); err != nil {
				return samples, err
			}
			held.PowerLimitW = st.PowerLimitW
		}
		held.CPUUtilization = st.CPUUtilization
		if err := o.Load.SetUtilization(st.CPUUtilization); err != nil {
			return samples, fmt.Errorf("step %d: %w", i+1, err)
		}
		logf("step %d of %d: %s for %v", i+1, len(o.Steps), sweepDescribe(held), o.Hold)
		stepStart := o.Clock.Now()
		for k := 1; k <= n; k++ {
			if err := o.Clock.Sleep(ctx, stepStart.Add(time.Duration(k)*o.Period).Sub(o.Clock.Now())); err != nil {
				return samples, err
			}
			at := o.Clock.Now()
			if err := h.read(at); err != nil {
				return samples, err
			}
			md := otlpjson.NewMetricsData(res, scope, h.metrics(start, at, held)...)
			if err := enc.Encode(md); err != nil {
				return samples, fmt.Errorf("write sample: %w", err)
			}
			samples++
		}
	}
	return samples, nil
}

// Validate checks the plan of o (steps, timing, node) before anything is
// read or written.
func (o SweepOptions) Validate() error {
	switch {
	case len(o.Steps) == 0:
		return errors.New("no steps")
	case o.Period <= 0:
		return fmt.Errorf("sample period %v is not positive", o.Period)
	case o.Hold < o.Period:
		return fmt.Errorf("hold %v is shorter than the sample period %v", o.Hold, o.Period)
	case o.Node == "":
		return errors.New("no node name")
	}
	for i, st := range o.Steps {
		switch {
		case !(st.CPUUtilization >= 0 && st.CPUUtilization <= 1):
			return fmt.Errorf("step %d: utilization %v is outside 0..1", i+1, st.CPUUtilization)
		case !(st.FrequencyMaxHz >= 0) || math.IsInf(st.FrequencyMaxHz, 0):
			return fmt.Errorf("step %d: frequency ceiling %v Hz is not a positive number", i+1, st.FrequencyMaxHz)
		case !(st.PowerLimitW >= 0) || math.IsInf(st.PowerLimitW, 0):
			return fmt.Errorf("step %d: power limit %v W is not a positive number", i+1, st.PowerLimitW)
		case !o.WriteLimits && (st.FrequencyMaxHz > 0 || st.PowerLimitW > 0):
			return fmt.Errorf("step %d sets a frequency ceiling or a power limit, which needs write-limits", i+1)
		}
	}
	return nil
}

// sweepDescribe is a step as the log shows it.
func sweepDescribe(st SweepStep) string {
	s := fmt.Sprintf("utilization %g", st.CPUUtilization)
	if st.FrequencyMaxHz > 0 {
		s += fmt.Sprintf(", scaling_max_freq %g MHz", st.FrequencyMaxHz/1e6)
	}
	if st.PowerLimitW > 0 {
		s += fmt.Sprintf(", package limit %g W", st.PowerLimitW)
	}
	return s
}

// metrics are the readings of the last sample in the name table's names,
// with the setpoints held. The CPU load is described with no class and
// memory intensity 0 (SweepOptions).
func (h *sweepHost) metrics(start, at time.Time, held SweepStep) []otlpjson.Metric {
	var energy, power, limits []otlpjson.NumberDataPoint
	for _, c := range h.comps {
		attrs := []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrHwID, c.id),
			otlpjson.String(semconv.AttrHwType, c.typ),
			otlpjson.String(semconv.AttrHwName, c.name),
		}
		if c.parent != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwParent, c.parent))
		}
		if len(c.counters) > 0 {
			j := 0.0
			for _, k := range c.counters {
				j += k.joules
			}
			energy = append(energy, otlpjson.DoublePoint(start, at, j, attrs...))
		}
		if c.hasPower {
			power = append(power, otlpjson.DoublePoint(time.Time{}, at, c.powerW, attrs...))
		}
		lim := func(state string, w float64) otlpjson.NumberDataPoint {
			return otlpjson.DoublePoint(time.Time{}, at, w,
				otlpjson.String(semconv.AttrHwID, c.id),
				otlpjson.String(semconv.AttrHwType, c.typ),
				otlpjson.String(semconv.AttrJoulieLimitState, state))
		}
		if c.wroteLimits {
			limits = append(limits, lim(semconv.LimitStateRequested, c.requestedW))
		}
		if c.hasLimit {
			limits = append(limits, lim(semconv.LimitStateEnforced, c.limitW))
		}
	}

	var speed, speedLimit []otlpjson.NumberDataPoint
	for _, c := range h.comps {
		if c.typ != semconv.HwTypeCPU {
			continue
		}
		var cur, lim float64
		var maxKHz int64
		k := 0
		for _, p := range h.policies {
			if p.socket == c.socket && p.ok {
				cur += float64(p.curKHz)
				lim += float64(p.limKHz)
				maxKHz = max(maxKHz, p.maxKHz)
				k++
			}
		}
		if k == 0 {
			continue
		}
		attrs := []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, c.id), otlpjson.String(semconv.AttrHwName, c.name)}
		if h.vendor != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwVendor, h.vendor))
		}
		if h.model != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwModel, h.model))
		}
		speed = append(speed, otlpjson.DoublePoint(time.Time{}, at, cur/float64(k)*1000, attrs...))
		speedLimit = append(speedLimit,
			otlpjson.DoublePoint(time.Time{}, at, float64(maxKHz)*1000, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeMax))...),
			otlpjson.DoublePoint(time.Time{}, at, lim/float64(k)*1000, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeThrottled))...))
	}

	idle := otlpjson.String(semconv.AttrCPUMode, semconv.CPUModeIdle)
	var util []otlpjson.NumberDataPoint
	for _, c := range h.cpus {
		if v, ok := h.idle[c]; ok {
			util = append(util, otlpjson.DoublePoint(time.Time{}, at, v, idle, otlpjson.Int(semconv.AttrCPULogicalNumber, int64(c))))
		}
	}

	knob := func(k string, v float64, extra ...otlpjson.KeyValue) otlpjson.NumberDataPoint {
		return otlpjson.DoublePoint(time.Time{}, at, v, append([]otlpjson.KeyValue{otlpjson.String(semconv.AttrCalibrationKnob, k)}, extra...)...)
	}
	cpuLoad := []otlpjson.KeyValue{otlpjson.Double(semconv.AttrCalibrationMemoryIntensity, 0)}
	setpoints := []otlpjson.NumberDataPoint{knob(semconv.KnobCPUUtilization, held.CPUUtilization, cpuLoad...)}
	if held.FrequencyMaxHz > 0 {
		setpoints = append(setpoints, knob(semconv.KnobCPUFrequencyMax, held.FrequencyMaxHz))
	}
	if held.PowerLimitW > 0 {
		setpoints = append(setpoints, knob(semconv.KnobCPUPowerLimit, held.PowerLimitW))
	}

	var out []otlpjson.Metric
	if len(energy) > 0 {
		out = append(out, otlpjson.SumMetric(semconv.HwEnergy, energy...))
	}
	if len(power) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.HwPower, power...))
	}
	if len(speed) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.HwCPUSpeed, speed...), otlpjson.GaugeMetric(semconv.HwCPUSpeedLimit, speedLimit...))
	}
	if len(limits) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.JoulieHwPowerLimit, limits...))
	}
	if len(util) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.SystemCPUUtilization, util...))
	}
	return append(out, otlpjson.GaugeMetric(semconv.JoulieCalibrationSetpoint, setpoints...))
}
