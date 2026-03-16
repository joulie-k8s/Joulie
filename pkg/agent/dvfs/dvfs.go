// Package dvfs implements CPU frequency scaling (DVFS) based power capping.
//
// The DVFS controller monitors power consumption via RAPL or an external
// telemetry endpoint and adjusts CPU frequency scaling to stay within a target
// power cap. It uses an exponential moving average (EMA) of power readings and
// a trip-count mechanism to avoid oscillation.
package dvfs

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/agent/control"
	"github.com/prometheus/client_golang/prometheus"
)

// CPU holds the cpufreq file paths and frequency limits for one logical CPU.
type CPU struct {
	Index   int
	MaxFile string
	CurFile string
	MinKHz  int64
	MaxKHz  int64
}

type energySample struct {
	LastUJ   int64
	LastTime time.Time
	RangeUJ  int64
}

// Metrics holds prometheus metric references used by the DVFS controller.
type Metrics struct {
	Node             string
	ObservedPowerW   *prometheus.GaugeVec
	EMAPowerW        *prometheus.GaugeVec
	ThrottlePct      *prometheus.GaugeVec
	TripAbove        *prometheus.GaugeVec
	TripBelow        *prometheus.GaugeVec
	CPUCurFreqKHz    *prometheus.GaugeVec
	CPUMaxFreqKHz    *prometheus.GaugeVec
	ActionsTotal     *prometheus.CounterVec
	RaplEnergyUJ     *prometheus.GaugeVec
	RaplPowerWatts   *prometheus.GaugeVec
	RaplPackageTotalW *prometheus.GaugeVec
}

// PowerReader abstracts power reading from RAPL or HTTP telemetry.
type PowerReader interface {
	ReadPowerWatts() (float64, bool, error)
}

// Controller implements DVFS-based power capping.
type Controller struct {
	Cpus []CPU

	Samples map[string]energySample

	EmaAlpha    float64
	EmaPowerW   float64
	EmaInit     bool
	HighMarginW float64
	LowMarginW  float64

	StepPct     int
	ThrottlePct int
	MinFreqKHz  int64

	Cooldown   time.Duration
	LastAction time.Time
	AboveCount int
	BelowCount int
	TripCount  int

	Warned  bool
	Metrics *Metrics

	PowerReader PowerReader
}

// Config holds initialization parameters for the DVFS controller.
type Config struct {
	EMAAlpha    float64
	HighMarginW float64
	LowMarginW  float64
	StepPct     int
	MinFreqKHz  int64
	Cooldown    time.Duration
	TripCount   int
}

// New creates a DVFS controller with the given config and metrics.
func New(cfg Config, metrics *Metrics) (*Controller, error) {
	cpus, err := CPUFreqList()
	if err != nil {
		return nil, err
	}
	if len(cpus) == 0 {
		log.Printf("warning: no cpufreq files found under /host-sys/devices/system/cpu; host DVFS writes disabled, HTTP control can still be used")
	}
	return &Controller{
		Cpus:        cpus,
		Samples:     map[string]energySample{},
		EmaAlpha:    cfg.EMAAlpha,
		HighMarginW: cfg.HighMarginW,
		LowMarginW:  cfg.LowMarginW,
		StepPct:     cfg.StepPct,
		MinFreqKHz:  cfg.MinFreqKHz,
		Cooldown:    cfg.Cooldown,
		TripCount:   cfg.TripCount,
		Metrics:     metrics,
	}, nil
}

// Active returns true if DVFS throttling is currently engaged.
func (d *Controller) Active() bool {
	return d.ThrottlePct > 0
}

// HasHostControl returns true if cpufreq scaling files were found on the host.
func (d *Controller) HasHostControl() bool {
	return len(d.Cpus) > 0
}

// SetPowerReader configures an external power telemetry source.
func (d *Controller) SetPowerReader(reader PowerReader) {
	d.PowerReader = reader
}

// ReadPowerWatts returns the current power reading from RAPL or an external source.
func (d *Controller) ReadPowerWatts() (float64, bool, error) {
	return d.readPowerWatts()
}

// ApplyThrottlePct applies a throttle percentage to CPUs via cpufreq or HTTP control.
func (d *Controller) ApplyThrottlePct(pct int, controlClient *control.HTTPControlClient, capWatts float64) (int, error) {
	return d.applyThrottlePct(pct, controlClient, capWatts)
}

// SetThrottlePct sets the current throttle percentage state.
func (d *Controller) SetThrottlePct(pct int) {
	d.ThrottlePct = pct
}

// UpdateCPUFreqMetrics updates cpufreq-related prometheus metrics.
func (d *Controller) UpdateCPUFreqMetrics() {
	d.updateCPUFreqMetrics()
}

// SetThrottlePctMetric updates the throttle pct metric to the given value.
func (d *Controller) SetThrottlePctMetric(pct float64) {
	if d.Metrics != nil && d.Metrics.ThrottlePct != nil {
		d.Metrics.ThrottlePct.WithLabelValues(d.Metrics.Node).Set(pct)
	}
}


// Reconcile runs one iteration of the DVFS control loop.
func (d *Controller) Reconcile(capWatts float64, controlClient *control.HTTPControlClient) (string, error) {
	if capWatts <= 0 {
		return "", nil
	}
	if d.Metrics == nil {
		return "", fmt.Errorf("dvfs: Metrics not initialized")
	}
	powerW, hasPower, err := d.readPowerWatts()
	if err != nil {
		return "", err
	}
	if !hasPower {
		return "", nil
	}
	if !d.EmaInit {
		d.EmaPowerW = powerW
		d.EmaInit = true
	} else {
		d.EmaPowerW = d.EmaAlpha*powerW + (1.0-d.EmaAlpha)*d.EmaPowerW
	}
	d.Metrics.ObservedPowerW.WithLabelValues(d.Metrics.Node).Set(powerW)
	d.Metrics.EMAPowerW.WithLabelValues(d.Metrics.Node).Set(d.EmaPowerW)
	d.Metrics.ThrottlePct.WithLabelValues(d.Metrics.Node).Set(float64(d.ThrottlePct))

	now := time.Now()
	if now.Sub(d.LastAction) < d.Cooldown {
		d.Metrics.TripAbove.WithLabelValues(d.Metrics.Node).Set(float64(d.AboveCount))
		d.Metrics.TripBelow.WithLabelValues(d.Metrics.Node).Set(float64(d.BelowCount))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW throttlePct=%d action=hold(cooldown)", powerW, d.EmaPowerW, d.ThrottlePct), nil
	}

	upper := capWatts + d.HighMarginW
	lower := capWatts - d.LowMarginW
	if d.EmaPowerW > upper {
		d.AboveCount++
		d.BelowCount = 0
	} else if d.EmaPowerW < lower {
		d.BelowCount++
		d.AboveCount = 0
	} else {
		d.AboveCount = 0
		d.BelowCount = 0
	}
	d.Metrics.TripAbove.WithLabelValues(d.Metrics.Node).Set(float64(d.AboveCount))
	d.Metrics.TripBelow.WithLabelValues(d.Metrics.Node).Set(float64(d.BelowCount))

	if d.AboveCount >= d.TripCount {
		oldPct := d.ThrottlePct
		d.ThrottlePct = min(100, d.ThrottlePct+d.StepPct)
		d.AboveCount = 0
		d.LastAction = now
		written, err := d.applyThrottlePct(d.ThrottlePct, controlClient, capWatts)
		if err != nil {
			return "", err
		}
		d.Metrics.ActionsTotal.WithLabelValues(d.Metrics.Node, "throttle_up").Inc()
		d.Metrics.ThrottlePct.WithLabelValues(d.Metrics.Node).Set(float64(d.ThrottlePct))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW upper=%.2fW action=throttle-up pct=%d->%d cpus=%d", powerW, d.EmaPowerW, upper, oldPct, d.ThrottlePct, written), nil
	}

	if d.BelowCount >= d.TripCount {
		oldPct := d.ThrottlePct
		d.ThrottlePct = max(0, d.ThrottlePct-d.StepPct)
		d.BelowCount = 0
		d.LastAction = now
		written, err := d.applyThrottlePct(d.ThrottlePct, controlClient, capWatts)
		if err != nil {
			return "", err
		}
		d.Metrics.ActionsTotal.WithLabelValues(d.Metrics.Node, "throttle_down").Inc()
		d.Metrics.ThrottlePct.WithLabelValues(d.Metrics.Node).Set(float64(d.ThrottlePct))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW lower=%.2fW action=throttle-down pct=%d->%d cpus=%d", powerW, d.EmaPowerW, lower, oldPct, d.ThrottlePct, written), nil
	}

	d.updateCPUFreqMetrics()
	return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW throttlePct=%d action=hold", powerW, d.EmaPowerW, d.ThrottlePct), nil
}

// RestoreAllMax resets all CPUs to their maximum frequency.
func (d *Controller) RestoreAllMax() (int, error) {
	written, err := d.applyThrottlePct(0, nil, 0)
	if err == nil {
		d.ThrottlePct = 0
		if d.Metrics != nil {
			d.Metrics.ThrottlePct.WithLabelValues(d.Metrics.Node).Set(0)
		}
		d.updateCPUFreqMetrics()
	}
	return written, err
}

func (d *Controller) applyThrottlePct(pct int, controlClient *control.HTTPControlClient, capWatts float64) (int, error) {
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("invalid throttle pct %d", pct)
	}
	if controlClient != nil {
		if err := controlClient.ApplyCPUControl("dvfs.set_throttle_pct", capWatts, pct); err != nil {
			return 0, err
		}
		return 1, nil
	}
	count := len(d.Cpus)
	if count == 0 {
		return 0, fmt.Errorf("no cpufreq scaling_max_freq files found under /host-sys/devices/system/cpu")
	}
	throttleCount := int(math.Ceil(float64(count) * float64(pct) / 100.0))
	written := 0
	for i, c := range d.Cpus {
		target := c.MaxKHz
		if i < throttleCount {
			target = max(c.MinKHz, min(c.MaxKHz, d.MinFreqKHz))
		}
		if err := os.WriteFile(c.MaxFile, []byte(strconv.FormatInt(target, 10)), 0); err != nil {
			return written, fmt.Errorf("write %s: %w", c.MaxFile, err)
		}
		written++
	}
	return written, nil
}

func (d *Controller) updateCPUFreqMetrics() {
	if d.Metrics == nil {
		return
	}
	for _, c := range d.Cpus {
		cpuLabel := strconv.Itoa(c.Index)
		if maxKHz, err := readInt64(c.MaxFile); err == nil {
			d.Metrics.CPUMaxFreqKHz.WithLabelValues(d.Metrics.Node, cpuLabel).Set(float64(maxKHz))
		}
		if curKHz, err := readInt64(c.CurFile); err == nil {
			d.Metrics.CPUCurFreqKHz.WithLabelValues(d.Metrics.Node, cpuLabel).Set(float64(curKHz))
		}
	}
}

func (d *Controller) readPowerWatts() (float64, bool, error) {
	if d.PowerReader != nil {
		return d.PowerReader.ReadPowerWatts()
	}
	files, err := EnergyFiles()
	if err != nil {
		return 0, false, err
	}
	if len(files) == 0 {
		return 0, false, nil
	}

	totalW := 0.0
	count := 0
	now := time.Now()
	for _, f := range files {
		zone := filepath.Base(filepath.Dir(f))
		currentUJ, err := readInt64(f)
		if err != nil {
			continue
		}
		if d.Metrics != nil {
			d.Metrics.RaplEnergyUJ.WithLabelValues(d.Metrics.Node, zone).Set(float64(currentUJ))
		}
		s, ok := d.Samples[f]
		if !ok {
			rangeUJ, _ := readInt64(filepath.Join(filepath.Dir(f), "max_energy_range_uj"))
			d.Samples[f] = energySample{LastUJ: currentUJ, LastTime: now, RangeUJ: rangeUJ}
			continue
		}

		deltaUJ := currentUJ - s.LastUJ
		if deltaUJ < 0 && s.RangeUJ > 0 {
			deltaUJ += s.RangeUJ
		}
		dt := now.Sub(s.LastTime).Seconds()
		s.LastUJ = currentUJ
		s.LastTime = now
		d.Samples[f] = s
		if dt <= 0 || deltaUJ < 0 {
			continue
		}
		w := (float64(deltaUJ) / 1_000_000.0) / dt
		d.Metrics.RaplPowerWatts.WithLabelValues(d.Metrics.Node, zone).Set(w)
		totalW += w
		count++
	}
	if count == 0 {
		return 0, false, nil
	}
	d.Metrics.RaplPackageTotalW.WithLabelValues(d.Metrics.Node).Set(totalW)
	return totalW, true, nil
}

// CPUFreqList enumerates cpufreq scaling entries on the host.
func CPUFreqList() ([]CPU, error) {
	matches := make([]string, 0)
	cpuMatches, err := filepath.Glob("/host-sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq")
	if err != nil {
		return nil, err
	}
	policyMatches, err := filepath.Glob("/host-sys/devices/system/cpu/cpufreq/policy*/scaling_max_freq")
	if err != nil {
		return nil, err
	}
	matches = append(matches, cpuMatches...)
	matches = append(matches, policyMatches...)

	cpus := make([]CPU, 0, len(matches))
	for _, maxf := range matches {
		dir := filepath.Dir(maxf)
		idx, ok := CPUIndexFromPath(dir)
		if !ok {
			continue
		}
		minKHz, err := readInt64(filepath.Join(dir, "cpuinfo_min_freq"))
		if err != nil {
			continue
		}
		maxKHz, err := readInt64(filepath.Join(dir, "cpuinfo_max_freq"))
		if err != nil {
			continue
		}
		cpus = append(cpus, CPU{Index: idx, MaxFile: maxf, CurFile: filepath.Join(dir, "scaling_cur_freq"), MinKHz: minKHz, MaxKHz: maxKHz})
	}
	sort.Slice(cpus, func(i, j int) bool { return cpus[i].Index < cpus[j].Index })
	return cpus, nil
}

// CPUIndexFromPath extracts the CPU index from a cpufreq directory path.
func CPUIndexFromPath(cpufreqDir string) (int, bool) {
	base := filepath.Base(cpufreqDir)
	if strings.HasPrefix(base, "policy") {
		v, err := strconv.Atoi(strings.TrimPrefix(base, "policy"))
		if err != nil {
			return 0, false
		}
		return v, true
	}
	if strings.HasPrefix(base, "cpufreq") {
		base = filepath.Base(filepath.Dir(cpufreqDir))
	}
	if !strings.HasPrefix(base, "cpu") {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimPrefix(base, "cpu"))
	if err != nil {
		return 0, false
	}
	return v, true
}

// EnergyFiles returns RAPL energy counter file paths.
func EnergyFiles() ([]string, error) {
	patterns := []string{
		"/host-sys/class/powercap/*/energy_uj",
		"/host-sys/class/powercap/*:*/energy_uj",
		"/host-sys/class/powercap/*:*:*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/*/energy_uj",
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	filtered := make([]string, 0, len(out))
	for _, f := range out {
		if IsPackageEnergyFile(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered, nil
}

// IsPackageEnergyFile returns true if the energy file is a package-level counter.
func IsPackageEnergyFile(path string) bool {
	zone := filepath.Base(filepath.Dir(path))
	return strings.Count(zone, ":") == 1
}

// RAPLCapFiles returns RAPL power cap file paths.
func RAPLCapFiles() ([]string, error) {
	patterns := []string{
		"/host-sys/class/powercap/*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*:*/constraint_0_power_limit_uw",
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

func readInt64(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
