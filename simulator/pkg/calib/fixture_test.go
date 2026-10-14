package calib

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

// The sweeps of these tests come from an emulated node: a builtin hwemu
// profile is the truth, the node runs each setpoint for a while, and every
// sample is exported the way the simulator exports it (telemetry.NodeRequest)
// plus the two names a sweep collector adds, system.cpu.utilization and
// joulie.calibration.setpoint. A fit of the sweep must give the profile back.

// fixtureNode names the emulated node; it is no real machine.
const fixtureNode = "sweep-node-0"

// fixtureStart is the node's start, on its substep grid.
var fixtureStart = time.Unix(1_760_000_000, 0)

// sweepStep is one setpoint of a sweep: the knobs it sets (knob to value, in
// the unit semconv gives each knob), the classes and memory intensity of the
// load, and how many samples it lasts.
type sweepStep struct {
	knobs              map[string]float64
	cpuClass, gpuClass string
	mem                float64
	samples            int
}

// sweepSpec is a whole sweep: a builtin profile, changed by mutate when set,
// the sampling period, whether utilization is reported per logical CPU, and
// the steps.
type sweepSpec struct {
	profile string
	mutate  func(*hwemu.Profile)
	period  time.Duration
	perCPU  bool
	steps   []sweepStep
}

// sweep is a generated sweep: its OTLP JSON lines and the profile, with any
// mutation, the node ran.
type sweep struct {
	lines   [][]byte
	profile *hwemu.Profile
}

// jsonl joins the lines into a JSON Lines file.
func (s sweep) jsonl() []byte {
	return append(bytes.Join(s.lines, []byte("\n")), '\n')
}

// data reads the sweep back as hwcalib does.
func (s sweep) data(t testing.TB) *Data {
	t.Helper()
	d, err := ReadOTLP(bytes.NewReader(s.jsonl()), fixtureNode)
	if err != nil {
		t.Fatalf("read the sweep: %v", err)
	}
	return d
}

// grid steps a full-factorial sweep of two knobs, a outer and b inner, each
// step lasting samples samples.
func grid(knobA string, as []float64, knobB string, bs []float64, samples int, cpuClass, gpuClass string) []sweepStep {
	var out []sweepStep
	for _, a := range as {
		for _, b := range bs {
			out = append(out, sweepStep{knobs: map[string]float64{knobA: a, knobB: b}, cpuClass: cpuClass, gpuClass: gpuClass, samples: samples})
		}
	}
	return out
}

// generateSweep runs spec on an emulated node.
func generateSweep(t testing.TB, spec sweepSpec) sweep {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatalf("builtin profiles: %v", err)
	}
	p, ok := profiles[spec.profile]
	if !ok {
		t.Fatalf("no builtin profile %q", spec.profile)
	}
	if spec.mutate != nil {
		spec.mutate(p)
	}
	root, err := os.MkdirTemp("", "calib-sweep-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	tree, err := hwemu.Render(p, filepath.Join(root, "node"), hwemu.RenderOptions{NodeName: fixtureNode, Seed: 1})
	if err != nil {
		t.Fatalf("render %s: %v", p.Name, err)
	}
	node, err := hwemu.NewNode(p, tree, hwemu.Options{Start: fixtureStart, RenderCheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("start %s: %v", p.Name, err)
	}
	cpus := p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * max(1, p.CPU.ThreadsPerCore.V)
	res := telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "calib-test"}
	scope := otlpjson.Scope{Name: "github.com/matbun/joulie/simulator/pkg/calib/fixture"}

	var out sweep
	out.profile = p
	now := fixtureStart
	written := map[string]float64{}
	for _, st := range spec.steps {
		load := hwemu.Load{CPUClass: st.cpuClass, GPUClass: st.gpuClass, MemoryIntensity: st.mem}
		if u, ok := st.knobs[semconv.KnobCPUUtilization]; ok {
			load.CPUUtil = []float64{u}
		}
		if u, ok := st.knobs[semconv.KnobGPUUtilization]; ok {
			load.GPUUtil = []float64{u}
		}
		node.SetLoad(load)
		for knob, v := range st.knobs {
			if w, ok := written[knob]; ok && w == v {
				continue
			}
			written[knob] = v
			writeKnob(t, tree, p, knob, v)
		}
		for k := 0; k < st.samples; k++ {
			now = now.Add(spec.period)
			if err := node.AdvanceTo(now); err != nil {
				t.Fatalf("advance to %v: %v", now, err)
			}
			state := node.State()
			md := fixtureSample(t, state, res, now)
			md.ResourceMetrics[0].ScopeMetrics = append(md.ResourceMetrics[0].ScopeMetrics, otlpjson.ScopeMetrics{
				Scope:     scope,
				SchemaURL: semconv.SchemaURL,
				Metrics: []otlpjson.Metric{
					otlpjson.GaugeMetric(semconv.SystemCPUUtilization, utilizationPoints(now, stateCPUUtil(state), spec.perCPU, cpus)...),
					otlpjson.GaugeMetric(semconv.JoulieCalibrationSetpoint, setpointPoints(now, st)...),
				},
			})
			b, err := json.Marshal(md)
			if err != nil {
				t.Fatal(err)
			}
			out.lines = append(out.lines, b)
		}
	}
	return out
}

// fixtureSample is the simulator's export of one node state.
func fixtureSample(t testing.TB, st hwemu.State, res telemetry.Resource, at time.Time) otlpjson.MetricsData {
	t.Helper()
	snap := emulate.NodeSnapshot{
		Name: fixtureNode, StartTime: fixtureStart,
		Host:       emulate.HostReading{Measured: st.Host.Measured, PowerW: st.Host.PowerW, EnergyJ: st.Host.EnergyJ, OtherW: st.Host.OtherW, InletC: 24},
		Components: append([]emulate.ComponentReading(nil), st.Components...),
	}
	md, dropped := telemetry.NodeRequest(snap, "calib-test", at, res)
	if dropped != 0 || len(md.ResourceMetrics) != 1 {
		t.Fatalf("export at %v: %d resources, %d values dropped", at, len(md.ResourceMetrics), dropped)
	}
	return md
}

// stateCPUUtil is the busy share a collector reads on the node: the mean
// Utilization of its CPU packages in State. It is the load setpoint under
// performance, and more under a governor that runs the work slower, which
// counts it in cycles, as /proc/stat counts busy time.
func stateCPUUtil(st hwemu.State) float64 {
	sum, n := 0.0, 0
	for _, c := range st.Components {
		if c.Type == semconv.HwTypeCPU {
			sum += c.Utilization
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// utilizationPoints are the idle shares of the CPUs at busy share u: one
// point per logical CPU with cpu.logical_number, or one for the whole host.
func utilizationPoints(at time.Time, u float64, perCPU bool, cpus int) []otlpjson.NumberDataPoint {
	idle := otlpjson.String(semconv.AttrCPUMode, semconv.CPUModeIdle)
	if !perCPU {
		return []otlpjson.NumberDataPoint{otlpjson.DoublePoint(time.Time{}, at, 1-u, idle)}
	}
	out := make([]otlpjson.NumberDataPoint, 0, cpus)
	for c := 0; c < cpus; c++ {
		out = append(out, otlpjson.DoublePoint(time.Time{}, at, 1-u, idle, otlpjson.Int(semconv.AttrCPULogicalNumber, int64(c))))
	}
	return out
}

// setpointPoints are the knobs of st, one point each, in knob order.
func setpointPoints(at time.Time, st sweepStep) []otlpjson.NumberDataPoint {
	knobs := make([]string, 0, len(st.knobs))
	for k := range st.knobs {
		knobs = append(knobs, k)
	}
	sort.Strings(knobs)
	var out []otlpjson.NumberDataPoint
	for _, k := range knobs {
		attrs := []otlpjson.KeyValue{otlpjson.String(semconv.AttrCalibrationKnob, k)}
		switch k {
		case semconv.KnobCPUUtilization:
			if st.cpuClass != "" {
				attrs = append(attrs, otlpjson.String(semconv.AttrCalibrationClass, st.cpuClass))
			}
			attrs = append(attrs, otlpjson.Double(semconv.AttrCalibrationMemoryIntensity, st.mem))
		case semconv.KnobGPUUtilization:
			if st.gpuClass != "" {
				attrs = append(attrs, otlpjson.String(semconv.AttrCalibrationClass, st.gpuClass))
			}
			attrs = append(attrs, otlpjson.Double(semconv.AttrCalibrationMemoryIntensity, st.mem))
		}
		out = append(out, otlpjson.DoublePoint(time.Time{}, at, st.knobs[k], attrs...))
	}
	return out
}

// writeKnob writes a file knob as an agent would: scaling_max_freq of every
// policy, the HSMP cap of every package, the NVML limit of every GPU.
func writeKnob(t testing.TB, tree *hwemu.Tree, p *hwemu.Profile, knob string, v float64) {
	t.Helper()
	var files []string
	var content string
	switch knob {
	case semconv.KnobCPUFrequencyMax:
		m, err := filepath.Glob(filepath.Join(tree.SysRoot, filepath.FromSlash(filepath.Dir(layout.PolicyDir(0))), "policy*", "scaling_max_freq"))
		if err != nil || len(m) == 0 {
			t.Fatalf("%s has no cpufreq policies: %v", p.Name, err)
		}
		files, content = m, strconv.FormatInt(int64(v/1000), 10)
	case semconv.KnobCPUPowerLimit:
		for k := 0; k < p.CPU.Sockets.V; k++ {
			files = append(files, filepath.Join(tree.SysRoot, filepath.FromSlash(layout.HSMPHwmonDir(k)), "power1_cap"))
		}
		content = strconv.FormatInt(int64(v*1e6), 10)
	case semconv.KnobGPUPowerLimit:
		for g := 0; g < p.GPUs.Count.V; g++ {
			files = append(files, filepath.Join(tree.StateRoot, filepath.FromSlash(layout.NVMLGPUDir(g)), "power_limit_mw"))
		}
		content = strconv.FormatInt(int64(v*1000), 10)
	default:
		return
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte(content+"\n"), 0); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
}

// csvFromLines converts OTLP lines to the CSV form, independently of
// ReadOTLP: one row per point, with the documented columns plus
// joulie.hw.limit.state, values in the shortest form that reads back the
// same. Counters stay cumulative.
func csvFromLines(t testing.TB, lines [][]byte) []byte {
	t.Helper()
	attrCols := []string{
		semconv.AttrHwID, semconv.AttrHwType, semconv.AttrHwLimitType, semconv.AttrJoulieLimitState,
		semconv.AttrCPULogicalNumber, semconv.AttrCPUMode,
		semconv.AttrCalibrationKnob, semconv.AttrCalibrationClass, semconv.AttrCalibrationMemoryIntensity,
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append([]string{csvTime, csvMetric, csvUnit, csvValue}, attrCols...)); err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		md, err := otlpjson.DecodeMetricsStrict(line)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, rm := range md.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					var pts []otlpjson.NumberDataPoint
					switch {
					case m.Sum != nil:
						pts = m.Sum.DataPoints
					case m.Gauge != nil:
						pts = m.Gauge.DataPoints
					}
					for _, p := range pts {
						v := 0.0
						if p.AsDouble != nil {
							v = *p.AsDouble
						} else {
							v = float64(*p.AsInt)
						}
						row := []string{strconv.FormatUint(uint64(p.TimeUnixNano), 10), m.Name, m.Unit, strconv.FormatFloat(v, 'g', -1, 64)}
						vals := map[string]string{}
						for _, kv := range p.Attributes {
							switch {
							case kv.Value.String != nil:
								vals[kv.Key] = *kv.Value.String
							case kv.Value.Int != nil:
								vals[kv.Key] = fmt.Sprint(int64(*kv.Value.Int))
							case kv.Value.Double != nil:
								vals[kv.Key] = strconv.FormatFloat(*kv.Value.Double, 'g', -1, 64)
							}
						}
						for _, c := range attrCols {
							row = append(row, vals[c])
						}
						if err := w.Write(row); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// epycAnalytic sets governor on the EPYC profile and makes its CPU the
// analytic model of the profile's facts, which a fit can give back, in
// place of the catalog curve the profile runs.
func epycAnalytic(governor string) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		p.CPUFreq.Governor.V = governor
		p.Physics.CPU.Model = "analytic"
	}
}

// The EPYC performance sweep serves several tests, so it is generated once.
var (
	epycOnce  sync.Once
	epycSweep sweep
	epycErr   string
)

// epycPerformanceSweep is amd-epyc-2s-energy-only under the performance
// governor, with the analytic model of its own facts in place of its
// catalog curve, so a fit has a ground truth to give back (epycAnalytic):
// scaling_max_freq at 2.4 GHz (boost to 3.7 GHz), 1.9 and 1.5 GHz,
// and at each the CPU load at 0, 0.2, ..., 1, 22 s per setpoint, sampled
// every 2 s, utilization per logical CPU.
func epycPerformanceSweep(t *testing.T) sweep {
	t.Helper()
	epycOnce.Do(func() {
		rt := &recordT{TB: t}
		func() {
			// recordT stops the generator with a panic; any other panic
			// is recorded too, rather than leaving an empty sweep.
			defer func() {
				if v := recover(); v != nil && rt.msg == "" {
					rt.msg = fmt.Sprint("panic: ", v)
				}
			}()
			epycSweep = generateSweep(rt, sweepSpec{
				profile: "amd-epyc-2s-energy-only",
				mutate:  epycAnalytic("performance"),
				period:  2 * time.Second,
				perCPU:  true,
				steps: grid(semconv.KnobCPUFrequencyMax, []float64{2.4e9, 1.9e9, 1.5e9},
					semconv.KnobCPUUtilization, []float64{0, 0.2, 0.4, 0.6, 0.8, 1}, 11, "", ""),
			})
		}()
		epycErr = rt.msg
	})
	if epycErr != "" {
		t.Fatalf("generate the EPYC sweep: %s", epycErr)
	}
	return epycSweep
}

// recordT records a fatal error of a generator run once for many tests,
// rather than failing only the test that happened to run it first.
type recordT struct {
	testing.TB
	msg string
}

func (r *recordT) Helper() {}

func (r *recordT) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	panic(r.msg)
}

func (r *recordT) Fatal(args ...any) {
	r.msg = fmt.Sprint(args...)
	panic(r.msg)
}
