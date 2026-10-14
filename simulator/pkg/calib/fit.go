package calib

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// Profile paths of the facts a fit can produce, as a profile file spells
// them.
const (
	pathCPUIdle      = "physics.cpu.idlePkgW"
	pathCPUMax       = "physics.cpu.maxPkgW"
	pathCPUAlpha     = "physics.cpu.alphaUtil"
	pathCPUBeta      = "physics.cpu.betaFreq"
	pathCPUFreqRamp  = "physics.cpu.freqRampMS"
	pathDRAM         = "physics.dram.wattsPerSocket"
	pathPlatform     = "physics.platform.otherW"
	pathGPUIdle      = "physics.gpu.idleW"
	pathGPUMax       = "physics.gpu.naturalMaxW"
	pathGPUCapSettle = "physics.gpu.capSettleTauMS"
	pathHSMPSettle   = "physics.hsmp.settleTauMS"
	pathRAPLSettle   = "physics.rapl.settleTau"
)

// Fit names, the location of each fitted fact's measured:sweep#<fit>
// provenance.
const (
	fitCPUIdle      = "cpu-idle"
	fitCPUOLS       = "cpu-ols"
	fitDRAM         = "dram-mean"
	fitPlatform     = "platform-idle"
	fitGPUIdle      = "gpu-idle"
	fitGPUInversion = "gpu-inversion"
	fitFreqStep     = "cpu-frequency-step"
	fitCPULimitStep = "cpu-limit-step"
	fitGPULimitStep = "gpu-limit-step"
)

// Thresholds of the fit.
const (
	// minDrop is the least time dropped at the start of a segment.
	minDrop = 10 * time.Second
	// minSteady is the least number of samples a steady window keeps.
	minSteady = 5
	// idleUtil is the utilization below which a component counts as idle.
	idleUtil = 0.02
	// cappedShare: a component drawing this share of its enforced limit or
	// more is capped, and its power says nothing about its natural power.
	cappedShare = 0.98
	// flatLnF: below this variance of ln F the sweep held one frequency.
	flatLnF = 1e-4
	// maxCorr: above this |corr(ln U, ln F)| frequency followed load, and
	// the two exponents cannot be told apart.
	maxCorr = 0.95
	// Exponent bounds, open.
	minExponent, maxExponent = 0.3, 3.0
	// minStep: a setpoint change moves a response by at least this share of
	// its value to count as a step.
	minStep = 0.01
	// tailShare: points of a step response closer to the final value than
	// this share of the step are noise, not decay.
	tailShare = 1e-3
)

// Options tune a fit.
type Options struct {
	// Substep is the fixed physics substep of the source, such as the
	// simulator's 100 ms, or zero for hardware, which settles continuously.
	// A source that moves a value by the Euler step (target-v)*h/tau every
	// substep h decays like exp(-t/tau_eff) with
	// tau = h / (1 - exp(-h/tau_eff)), so each settling time is corrected
	// by that formula.
	Substep time.Duration
}

// Param is one value of a fit: the profile fact at Path, the base profile's
// value, and the fitted value when Fitted. Fit names the method, N counts
// the samples or steps behind it, and Reason says why it was not fitted.
// A Param that is not Written is only reported, never put in a profile.
type Param struct {
	Path    string
	Unit    string
	Base    float64
	Value   float64
	Fitted  bool
	Written bool
	Fit     string
	N       int
	Reason  string
}

// Result is a fit: every Param in a fixed order, the segments of the sweep
// and how many had a steady window, and notes on what the fit assumed.
type Result struct {
	Node     string
	Base     string
	Segments int
	Steady   int
	Params   []Param
	Notes    []string
	// cpuModel is set when the CPU exponents were fitted against a base
	// whose CPU model is not analytic: the fitted facts only act under the
	// analytic model.
	cpuModel string
}

// Fitted reports whether any parameter that goes into a profile was fitted.
func (r *Result) Fitted() bool {
	for _, p := range r.Params {
		if p.Fitted && p.Written {
			return true
		}
	}
	return false
}

// Fit fits the physics of base to the sweep d. Base supplies the topology
// that maps logical CPUs to packages, the settling times that size the
// steady windows, and the values a parameter keeps when the sweep cannot
// fit it. Fit fails only when the sweep has no segments or nothing could be
// fitted; each parameter it cannot fit is reported with the reason.
func Fit(d *Data, base *hwemu.Profile, o Options) (*Result, error) {
	if d == nil || base == nil {
		return nil, errors.New("fit: no sweep or no base profile")
	}
	if base.CPU.Hypervisor {
		return nil, fmt.Errorf("fit: profile %s is a hypervisor guest, which exports no hardware readings to fit", base.Name)
	}
	segs, err := Segments(d)
	if err != nil {
		return nil, fmt.Errorf("fit: %w", err)
	}
	f := &fitter{d: d, base: base, segs: segs, o: o, r: &Result{Node: d.Node, Base: base.Name, Segments: len(segs)}}
	f.setup()
	f.fitCPU()
	f.fitDRAM()
	f.fitGPU()
	f.fitPlatform()
	f.fitSettling()
	if !f.r.Fitted() {
		var why []string
		for _, p := range f.r.Params {
			if p.Written && p.Reason != "" {
				why = append(why, p.Path+": "+p.Reason)
			}
		}
		return f.r, fmt.Errorf("fit: nothing could be fitted from %d segments:\n  %s", len(segs), strings.Join(why, "\n  "))
	}
	return f.r, nil
}

// fitter holds one fit in progress.
type fitter struct {
	d    *Data
	base *hwemu.Profile
	segs []Segment
	o    Options
	r    *Result

	sockets, cores int
	cpuDrop        time.Duration
	gpuDrop        time.Duration
	gpus           []string

	// utilization: one series per logical CPU, or one for the whole host.
	cpuIdle  map[int]*Series
	hostIdle *Series

	// per segment, per package: the steady CPU readings; nil when the
	// segment has no steady window.
	cpu [][]cpuReading
	// per segment, per GPU.
	gpu [][]gpuReading
}

type cpuReading struct {
	ok      bool // power, utilization and frequency were all read
	u, f, w float64
	mem     float64
	capped  bool
}

type gpuReading struct {
	ok     bool
	u, w   float64
	class  string
	mem    float64
	capped bool
}

// add appends a parameter to the result. A fitted value that is not a
// finite number, from a reading that is not one, is reported as not fitted:
// a mean over a window with a NaN reading is NaN, and the profile could not
// hold it.
func (f *fitter) add(p Param) {
	if p.Fitted && (math.IsNaN(p.Value) || math.IsInf(p.Value, 0)) {
		p.Fitted = false
		p.Reason = fmt.Sprintf("the fit gave %v: a reading in a steady window is not a finite number", p.Value)
	}
	f.r.Params = append(f.r.Params, p)
}

func (f *fitter) note(format string, args ...any) {
	f.r.Notes = append(f.r.Notes, fmt.Sprintf(format, args...))
}

// setup reads the topology and the settling times of the base profile and
// sizes the steady windows: max(10 s, 3 times the largest settling time of
// the domain).
func (f *fitter) setup() {
	c := f.base.CPU
	f.sockets, f.cores = max(1, c.Sockets.V), max(1, c.CoresPerSocket.V)
	ph := f.base.Physics
	cpuTau := time.Duration(max(ph.CPU.FreqRampMS.V, ph.HSMP.SettleTauMS.V)) * time.Millisecond
	if pc := f.base.Powercap; pc != nil {
		for _, ct := range pc.ControlTypes {
			cpuTau = max(cpuTau, zonesWindow(ct.Zones))
		}
	}
	gpuTau := time.Duration(max(ph.GPU.CapSettleTauMS.V, ph.GPU.TelemetryWindowMS.V)) * time.Millisecond
	f.cpuDrop, f.gpuDrop = max(minDrop, 3*cpuTau), max(minDrop, 3*gpuTau)

	f.cpuIdle = map[int]*Series{}
	for _, s := range f.d.Select(semconv.SystemCPUUtilization.Name, map[string]string{semconv.AttrCPUMode: semconv.CPUModeIdle}) {
		txt, ok := s.Attrs[semconv.AttrCPULogicalNumber]
		if !ok {
			f.hostIdle = s
			continue
		}
		if n, err := strconv.Atoi(txt); err == nil && n >= 0 {
			f.cpuIdle[n] = s
		}
	}
	switch {
	case len(f.cpuIdle) > 0:
		f.note("CPU utilization: system.cpu.utilization of %d logical CPUs, mapped to %d packages by the profile's numbering", len(f.cpuIdle), f.sockets)
	case f.hostIdle != nil:
		f.note("CPU utilization: system.cpu.utilization of the whole host, the same for every package")
	default:
		f.note("CPU utilization: no system.cpu.utilization{cpu.mode=idle}; the cpu.utilization setpoint stands in for it")
	}

	ids := map[string]bool{}
	for _, s := range f.d.Series {
		if s.Attrs[semconv.AttrHwType] == semconv.HwTypeGPU || s.Metric == semconv.HwGPUUtilization.Name {
			if id := s.Attrs[semconv.AttrHwID]; id != "" {
				ids[id] = true
			}
		}
	}
	for id := range ids {
		f.gpus = append(f.gpus, id)
	}
	sort.Slice(f.gpus, func(i, j int) bool { return idLess(f.gpus[i], f.gpus[j]) })

	f.cpu = make([][]cpuReading, len(f.segs))
	f.gpu = make([][]gpuReading, len(f.segs))
	for i, seg := range f.segs {
		steady := false
		if a, b, n := seg.Steady(f.cpuDrop); n >= minSteady {
			steady = true
			f.cpu[i] = make([]cpuReading, f.sockets)
			for k := range f.cpu[i] {
				f.cpu[i][k] = f.readCPU(seg, k, a, b)
			}
		}
		if a, b, n := seg.Steady(f.gpuDrop); n >= minSteady && len(f.gpus) > 0 {
			steady = true
			f.gpu[i] = make([]gpuReading, len(f.gpus))
			for g, id := range f.gpus {
				f.gpu[i][g] = f.readGPU(seg, id, a, b)
			}
		}
		if steady {
			f.r.Steady++
		}
	}
	f.note("steady windows: %d of %d segments keep at least %d samples after dropping %v (CPU) and %v (GPU)",
		f.r.Steady, len(f.segs), minSteady, f.cpuDrop, f.gpuDrop)
}

// zonesWindow is the longest constraint time window of zones and their
// children.
func zonesWindow(zones []hwemu.ZoneSpec) time.Duration {
	var w time.Duration
	for _, z := range zones {
		for _, c := range z.Constraints {
			w = max(w, time.Duration(c.TimeWindowUS.V)*time.Microsecond)
		}
		w = max(w, zonesWindow(z.Children))
	}
	return w
}

// idLess orders hw.id values by their type, then numerically by index, so
// gpu10 follows gpu9.
func idLess(a, b string) bool {
	ta, na := splitID(a)
	tb, nb := splitID(b)
	if ta != tb {
		return ta < tb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

func splitID(id string) (string, int) {
	i := len(id)
	for i > 0 && id[i-1] >= '0' && id[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(id[i:])
	if err != nil {
		return id, -1
	}
	return id[:i], n
}

// socketOf is the package of logical CPU cpu under the numbering
// socket-major-smt-last, the only one profiles have: CPU k sits on package
// (k mod sockets*cores)/cores, its SMT siblings sockets*cores apart.
func (f *fitter) socketOf(cpu int) int { return (cpu % (f.sockets * f.cores)) / f.cores }

// readCPU reads package k over [a, b].
func (f *fitter) readCPU(seg Segment, k int, a, b time.Time) cpuReading {
	id := "cpu" + strconv.Itoa(k)
	sel := map[string]string{semconv.AttrHwType: semconv.HwTypeCPU, semconv.AttrHwID: id}
	w, _, okW := power(f.d.Select(semconv.HwEnergy.Name, sel), f.d.Select(semconv.HwPower.Name, sel), a, b)
	u, okU := f.packageUtil(seg, k, a, b)
	fs, okF := f.freqScale(id, a, b)
	r := cpuReading{ok: okW && okU && okF, u: u, f: fs, w: w, mem: seg.Memory[semconv.KnobCPUUtilization]}
	if lim, ok := f.enforcedLimit(semconv.HwTypeCPU, id, a, b); ok && lim > 0 && w >= cappedShare*lim {
		r.capped = true
	}
	return r
}

// packageUtil is the mean utilization of package k over [a, b]: one minus
// the idle share of its logical CPUs, of the whole host, or else the
// cpu.utilization setpoint.
func (f *fitter) packageUtil(seg Segment, k int, a, b time.Time) (float64, bool) {
	if len(f.cpuIdle) > 0 {
		sum, n := 0.0, 0
		for cpu, s := range f.cpuIdle {
			if f.socketOf(cpu) != k {
				continue
			}
			if v, m := s.mean(a, b); m > 0 {
				sum += 1 - v
				n++
			}
		}
		if n == 0 {
			return 0, false
		}
		return sum / float64(n), true
	}
	if f.hostIdle != nil {
		v, m := f.hostIdle.mean(a, b)
		return 1 - v, m > 0
	}
	u, ok := seg.Setpoints[semconv.KnobCPUUtilization]
	return u, ok
}

// freqScale is the mean frequency of a package over [a, b] as a share of its
// hardware maximum: hw.cpu.speed over hw.cpu.speed.limit{hw.limit_type=max},
// or over the base profile's cpuinfo_max_freq. A package without
// hw.cpu.speed runs at a scale of 1.
func (f *fitter) freqScale(id string, a, b time.Time) (float64, bool) {
	speeds := f.d.Select(semconv.HwCPUSpeed.Name, map[string]string{semconv.AttrHwID: id})
	if len(speeds) == 0 {
		return 1, true
	}
	hz, n := speeds[0].mean(a, b)
	if n == 0 {
		return 0, false
	}
	maxHz := 0.0
	if lims := f.d.Select(semconv.HwCPUSpeedLimit.Name, map[string]string{semconv.AttrHwID: id, semconv.AttrHwLimitType: semconv.LimitTypeMax}); len(lims) > 0 {
		maxHz, _ = lims[0].mean(a, b)
	}
	if maxHz <= 0 && f.base.CPUFreq != nil {
		maxHz = float64(f.base.CPUFreq.CPUInfoMaxKHz.V) * 1000
	}
	if maxHz <= 0 {
		return 0, false
	}
	return hz / maxHz, true
}

// enforcedLimit is the mean enforced power limit of a component over [a, b].
func (f *fitter) enforcedLimit(typ, id string, a, b time.Time) (float64, bool) {
	for _, s := range f.d.Select(semconv.JoulieHwPowerLimit.Name, map[string]string{
		semconv.AttrHwType: typ, semconv.AttrHwID: id, semconv.AttrJoulieLimitState: semconv.LimitStateEnforced,
	}) {
		if v, n := s.mean(a, b); n > 0 {
			return v, true
		}
	}
	return 0, false
}

// readGPU reads GPU id over [a, b].
func (f *fitter) readGPU(seg Segment, id string, a, b time.Time) gpuReading {
	sel := map[string]string{semconv.AttrHwType: semconv.HwTypeGPU, semconv.AttrHwID: id}
	w, _, okW := power(f.d.Select(semconv.HwEnergy.Name, sel), f.d.Select(semconv.HwPower.Name, sel), a, b)
	r := gpuReading{w: w, class: seg.Classes[semconv.KnobGPUUtilization], mem: seg.Memory[semconv.KnobGPUUtilization]}
	okU := false
	if us := f.d.Select(semconv.HwGPUUtilization.Name, map[string]string{semconv.AttrHwID: id}); len(us) > 0 {
		var n int
		r.u, n = us[0].mean(a, b)
		okU = n > 0
	} else {
		r.u, okU = seg.Setpoints[semconv.KnobGPUUtilization]
	}
	r.ok = okW && okU
	if lim, ok := f.enforcedLimit(semconv.HwTypeGPU, id, a, b); ok && lim > 0 && w >= cappedShare*lim {
		r.capped = true
	}
	return r
}

// cpuActivity is the share of utilization the analytic CPU model turns into
// power at memory intensity mem: phys.AnalyticCPUModel's activity factor,
// read through the model with a unit dynamic range and exponents of 1.
func cpuActivity(mem float64) float64 {
	return phys.AnalyticCPUModel{MaxW: 1, AlphaUtil: 1, BetaFreq: 1}.Power(phys.DeviceState{Utilization: 1, FreqScale: 1, MemoryIntensity: mem})
}

// gpuShape is the share of its dynamic range a GPU draws at utilization u for
// a load of class and memory intensity mem: phys.CappedBoardGPUModel's
// natural power with an idle power of 0 and a range of 1, so that
// P = idle + (naturalMax - idle) * gpuShape(u). At u = 1 it is
// w_compute + 0.65 w_memory.
func gpuShape(u float64, class string, mem float64) float64 {
	return phys.CappedBoardGPUModel{MaxW: 1}.Power(phys.DeviceState{Utilization: u, CapWatts: math.MaxFloat64, Class: class, MemoryIntensity: mem})
}

// fitCPU fits the analytic package model
// P = idle + (max - idle) * (U*activity)^alpha * F^beta: idle is the mean
// power below 2 % utilization, then an OLS of ln(P - idle) on
// [1, ln(U*activity), ln F] gives ln(max - idle), alpha and beta. Beta is
// not fitted when the sweep held one frequency or when frequency followed
// load (|corr| > 0.95); the base beta then stands, and the OLS fits the
// other two with it. Packages are pooled, as the profile has one value for
// all. Capped samples are left out.
func (f *fitter) fitCPU() {
	ph := f.base.Physics.CPU
	idle := Param{Path: pathCPUIdle, Unit: "W", Base: ph.IdlePkgW.V, Written: true, Fit: fitCPUIdle}
	maxP := Param{Path: pathCPUMax, Unit: "W", Base: ph.MaxPkgW.V, Written: true, Fit: fitCPUOLS}
	alpha := Param{Path: pathCPUAlpha, Unit: "1", Base: ph.AlphaUtil.V, Written: true, Fit: fitCPUOLS}
	beta := Param{Path: pathCPUBeta, Unit: "1", Base: ph.BetaFreq.V, Written: true, Fit: fitCPUOLS}
	defer func() { f.add(idle); f.add(maxP); f.add(alpha); f.add(beta) }()

	var idleSum float64
	var loaded []cpuReading
	for _, seg := range f.cpu {
		for _, r := range seg {
			switch {
			case !r.ok || r.capped:
			case r.u < idleUtil:
				idleSum += r.w
				idle.N++
			default:
				loaded = append(loaded, r)
			}
		}
	}
	idleW := idle.Base
	if idle.N > 0 {
		idle.Value, idle.Fitted = idleSum/float64(idle.N), true
		idleW = idle.Value
	} else {
		idle.Reason = "no steady package reading below 2 % utilization"
	}

	var x1, x2, y []float64
	for _, r := range loaded {
		u := r.u * cpuActivity(r.mem)
		if r.w-idleW <= 0 || u <= 0 || r.f <= 0 {
			continue
		}
		x1 = append(x1, math.Log(u))
		x2 = append(x2, math.Log(r.f))
		y = append(y, math.Log(r.w-idleW))
	}
	fail := func(why string) {
		for _, p := range []*Param{&maxP, &alpha, &beta} {
			p.Reason = why
		}
	}
	if len(y) < 3 {
		fail(fmt.Sprintf("%d steady package readings above 2 %% utilization and above idle power, want 3", len(y)))
		return
	}
	if variance(x1) <= 0 {
		fail("the sweep held one utilization")
		return
	}
	fitBeta := ""
	switch {
	case variance(x2) < flatLnF:
		fitBeta = fmt.Sprintf("not fitted: the sweep held one frequency (var(ln F) = %.2g)", variance(x2))
	case math.Abs(correlation(x1, x2)) > maxCorr:
		fitBeta = fmt.Sprintf("not fitted: frequency followed load (|corr(ln U, ln F)| = %.3f > %.2f), so the two exponents cannot be told apart", math.Abs(correlation(x1, x2)), maxCorr)
	}
	var c, a, bb float64
	if fitBeta == "" {
		c, a, bb = ols2(x1, x2, y)
		if !inExponentBounds(bb) {
			fitBeta = fmt.Sprintf("not fitted: beta = %.3f is outside (%.1f, %.0f)", bb, minExponent, maxExponent)
		}
	}
	if fitBeta != "" {
		beta.Reason = strings.TrimPrefix(fitBeta, "not fitted: ")
		// The analytic model reads an exponent of 0 as 1, and so does the
		// profile this fit writes, which inherits the base beta.
		bb = beta.Base
		if bb <= 0 {
			bb = 1
		}
		yy := make([]float64, len(y))
		for i := range y {
			yy[i] = y[i] - bb*x2[i]
		}
		c, a = ols1(x1, yy)
	}
	if !inExponentBounds(a) {
		fail(fmt.Sprintf("alpha = %.3f is outside (%.1f, %.0f)", a, minExponent, maxExponent))
		return
	}
	maxP.Value, maxP.Fitted, maxP.N = idleW+math.Exp(c), true, len(y)
	alpha.Value, alpha.Fitted, alpha.N = a, true, len(y)
	if fitBeta == "" {
		beta.Value, beta.Fitted, beta.N = bb, true, len(y)
	}
	if !idle.Fitted {
		f.note("CPU: the OLS used the base idlePkgW %.4g W", idleW)
	}
	if ph.Model != "" && ph.Model != "analytic" {
		f.r.cpuModel = "analytic"
		f.note("CPU: the base model is %s; the fitted facts describe the analytic model, which the profile now selects", ph.Model)
	}
}

// inExponentBounds reports whether a fitted exponent lies in the open
// interval (0.3, 3). NaN, from a reading that is not a number, lies in none.
func inExponentBounds(v float64) bool { return v > minExponent && v < maxExponent }

// fitDRAM is the mean DRAM power per socket over every steady window.
func (f *fitter) fitDRAM() {
	p := Param{Path: pathDRAM, Unit: "W", Base: f.base.Physics.DRAM.WattsPerSocket.V, Written: true, Fit: fitDRAM}
	sum := 0.0
	for i, seg := range f.segs {
		if f.cpu[i] == nil {
			continue
		}
		a, b, _ := seg.Steady(f.cpuDrop)
		for k := 0; k < f.sockets; k++ {
			sel := map[string]string{semconv.AttrHwType: semconv.HwTypeMemory, semconv.AttrHwID: "memory" + strconv.Itoa(k)}
			if w, _, ok := power(f.d.Select(semconv.HwEnergy.Name, sel), f.d.Select(semconv.HwPower.Name, sel), a, b); ok {
				sum += w
				p.N++
			}
		}
	}
	if p.N == 0 {
		p.Reason = "no hw.energy or hw.power of a memory component: the DRAM zones are not measured"
	} else {
		p.Value, p.Fitted = sum/float64(p.N), true
	}
	f.add(p)
}

// fitGPU fits CappedBoardGPUModel: idle is the mean power below 2 %
// utilization, and the natural maximum inverts the model at the class and
// memory intensity each segment recorded, P - idle = (max - idle) * shape(U),
// by least squares through the origin over the uncapped loaded readings.
// The raw power at full load is w_compute + 0.65 w_memory of the range only.
func (f *fitter) fitGPU() {
	ph := f.base.Physics.GPU
	idle := Param{Path: pathGPUIdle, Unit: "W", Base: ph.IdleW.V, Written: true, Fit: fitGPUIdle}
	natural := Param{Path: pathGPUMax, Unit: "W", Base: ph.NaturalMaxW.V, Written: true, Fit: fitGPUInversion}
	defer func() { f.add(idle); f.add(natural) }()
	if len(f.gpus) == 0 {
		idle.Reason, natural.Reason = "the sweep has no GPU", "the sweep has no GPU"
		return
	}
	var idleSum float64
	var loaded []gpuReading
	for _, seg := range f.gpu {
		for _, r := range seg {
			switch {
			case !r.ok:
			case r.u < idleUtil:
				idleSum += r.w
				idle.N++
			case !r.capped:
				loaded = append(loaded, r)
			}
		}
	}
	idleW := idle.Base
	if idle.N > 0 {
		idle.Value, idle.Fitted = idleSum/float64(idle.N), true
		idleW = idle.Value
	} else {
		idle.Reason = "no steady GPU reading below 2 % utilization"
	}
	var sxy, sxx float64
	for _, r := range loaded {
		s := gpuShape(r.u, r.class, r.mem)
		sxy += s * (r.w - idleW)
		sxx += s * s
		natural.N++
	}
	if natural.N == 0 || sxx <= 0 {
		natural.N = 0
		natural.Reason = "no steady uncapped GPU reading above 2 % utilization"
		return
	}
	natural.Value, natural.Fitted = idleW+sxy/sxx, true
}

// fitPlatform is the mean of host power minus every component over the
// idle segments: those where every package and every GPU is below 2 %
// utilization. DRAM that is not measured counts with the base profile's
// wattsPerSocket, since the host still draws it.
func (f *fitter) fitPlatform() {
	p := Param{Path: pathPlatform, Unit: "W", Base: f.base.Physics.Platform.OtherW.V, Written: true, Fit: fitPlatform}
	defer func() { f.add(p) }()
	drop := max(f.cpuDrop, f.gpuDrop)
	host := map[string]string{semconv.AttrHwID: semconv.HwIDHost}
	hostE, hostP := f.d.Select(semconv.HwHostEnergy.Name, host), f.d.Select(semconv.HwHostPower.Name, host)
	if len(hostE)+len(hostP) == 0 {
		p.Reason = "no hw.host.energy or hw.host.power"
		return
	}
	sum, assumedDRAM := 0.0, false
	for _, seg := range f.segs {
		a, b, n := seg.Steady(drop)
		if n < minSteady {
			continue
		}
		idle := true
		parts := 0.0
		for k := 0; k < f.sockets; k++ {
			r := f.readCPU(seg, k, a, b)
			idle = idle && r.ok && r.u < idleUtil
			parts += r.w
		}
		for _, id := range f.gpus {
			r := f.readGPU(seg, id, a, b)
			idle = idle && r.ok && r.u < idleUtil
			parts += r.w
		}
		if !idle {
			continue
		}
		hw, _, ok := power(hostE, hostP, a, b)
		if !ok {
			continue
		}
		for k := 0; k < f.sockets; k++ {
			sel := map[string]string{semconv.AttrHwType: semconv.HwTypeMemory, semconv.AttrHwID: "memory" + strconv.Itoa(k)}
			if w, _, ok := power(f.d.Select(semconv.HwEnergy.Name, sel), f.d.Select(semconv.HwPower.Name, sel), a, b); ok {
				parts += w
			} else {
				parts += f.base.Physics.DRAM.WattsPerSocket.V
				assumedDRAM = true
			}
		}
		sum += hw - parts
		p.N++
	}
	if p.N == 0 {
		p.Reason = "no steady idle segment with host and component readings"
		return
	}
	p.Value, p.Fitted = sum/float64(p.N), true
	if assumedDRAM {
		f.note("platform: DRAM is not measured; the base wattsPerSocket %.4g W per socket was subtracted from host power", f.base.Physics.DRAM.WattsPerSocket.V)
	}
}

// fitSettling fits the settling times from the steps of the sweep: the GPU
// limit (gpu.capSettleTauMS), the CPU package limit (hsmp.settleTauMS with
// HSMP, otherwise the RAPL PL1 settling, which is reported only, since
// physics.rapl.settleTau is a plain string without provenance) and the CPU
// frequency (cpu.freqRampMS, from steps that change no CPU limit).
func (f *fitter) fitSettling() {
	ph := f.base.Physics
	ramp := Param{Path: pathCPUFreqRamp, Unit: "ms", Base: float64(ph.CPU.FreqRampMS.V), Written: true, Fit: fitFreqStep}
	f.settle(&ramp, f.d.Select(semconv.HwCPUSpeed.Name, nil), func(prev, cur Segment) bool {
		return prev.Setpoints[semconv.KnobCPUPowerLimit] == cur.Setpoints[semconv.KnobCPUPowerLimit]
	})
	f.add(ramp)

	limits := f.d.Select(semconv.JoulieHwPowerLimit.Name, map[string]string{semconv.AttrHwType: semconv.HwTypeCPU, semconv.AttrJoulieLimitState: semconv.LimitStateEnforced})
	if f.base.HSMP != nil {
		hsmp := Param{Path: pathHSMPSettle, Unit: "ms", Base: float64(ph.HSMP.SettleTauMS.V), Written: true, Fit: fitCPULimitStep}
		f.settle(&hsmp, limits, nil)
		f.add(hsmp)
	} else {
		rapl := Param{Path: pathRAPLSettle, Unit: "ms", Fit: fitCPULimitStep}
		f.settle(&rapl, limits, nil)
		if rapl.Fitted {
			f.note("RAPL: PL1 settles with tau %.0f ms; reported only, since physics.rapl.settleTau (%q) is a plain string without provenance", rapl.Value, ph.RAPL.SettleTau)
		}
		f.add(rapl)
	}

	gpu := Param{Path: pathGPUCapSettle, Unit: "ms", Base: float64(ph.GPU.CapSettleTauMS.V), Written: true, Fit: fitGPULimitStep}
	if len(f.gpus) == 0 {
		gpu.Reason = "the sweep has no GPU"
	} else {
		f.settle(&gpu, f.d.Select(semconv.JoulieHwPowerLimit.Name, map[string]string{semconv.AttrHwType: semconv.HwTypeGPU, semconv.AttrJoulieLimitState: semconv.LimitStateEnforced}), nil)
	}
	f.add(gpu)
}

// settle fits p, a settling time in ms, from the response of series to the
// setpoint changes accept lets through (all when nil). Each step is fitted
// on its own: from the last sample of the old segment to the end of the new
// one, the response must move by at least 1 % and settle; ln|v - final| of
// the samples after the change, until they come within 0.1 % of the step,
// is a line of slope -1/tau_eff. The median over all steps is corrected for
// a fixed substep (Options.Substep).
func (f *fitter) settle(p *Param, series []*Series, accept func(prev, cur Segment) bool) {
	var taus []float64
	sampled, short := false, false
	for _, s := range series {
		for i := 1; i < len(f.segs); i++ {
			prev, cur := f.segs[i-1], f.segs[i]
			if accept != nil && !accept(prev, cur) {
				continue
			}
			tau, ok, fast := stepTau(s.Points, prev.End(), cur.End())
			short = short || fast
			if ok {
				taus = append(taus, tau)
			}
			sampled = true
		}
	}
	if len(taus) == 0 {
		switch {
		case short:
			p.Reason = "every step settled within one or two samples; sample faster than the settling time"
		case sampled:
			p.Reason = "no setpoint change moved the response"
		default:
			p.Reason = "the sweep has no response series or no setpoint change"
		}
		return
	}
	sort.Float64s(taus)
	tauEff := taus[len(taus)/2]
	if len(taus)%2 == 0 {
		tauEff = (taus[len(taus)/2-1] + taus[len(taus)/2]) / 2
	}
	tau := tauEff
	if h := f.o.Substep.Seconds(); h > 0 {
		tau = h / (1 - math.Exp(-h/tauEff))
	}
	p.Value, p.Fitted, p.N = tau*1000, true, len(taus)
}

// stepTau fits the decay of a response after a setpoint change: the points
// of pts from the last one at or before from to the last one at or before
// to. ok is false when there is no step; fast is set when there is one but
// it settled too quickly for two samples to show the decay.
func stepTau(pts []Point, from, to time.Time) (tau float64, ok, fast bool) {
	first := -1
	last := -1
	for i, p := range pts {
		if !p.Time.After(from) {
			first = i
		}
		if !p.Time.After(to) {
			last = i
		}
	}
	if first < 0 || last-first < 3 {
		return 0, false, false
	}
	e0, final := pts[first].Value, pts[last].Value
	step := e0 - final
	if math.Abs(step) < minStep*math.Max(math.Abs(e0), math.Abs(final)) || step == 0 {
		return 0, false, false
	}
	// The response must have settled: the last two samples agree within
	// the tail share of the step.
	if math.Abs(pts[last-1].Value-final) > tailShare*math.Abs(step) && math.Abs(pts[last-1].Value-final) > 1e-9*math.Abs(final) {
		return 0, false, false
	}
	var ts, ys []float64
	for i := first + 1; i < last; i++ {
		d := pts[i].Value - final
		if d*step <= 0 || math.Abs(d) <= tailShare*math.Abs(step) {
			break
		}
		ts = append(ts, pts[i].Time.Sub(pts[first].Time).Seconds())
		ys = append(ys, math.Log(math.Abs(d)))
	}
	if len(ts) < 2 {
		return 0, false, true
	}
	_, slope := ols1(ts, ys)
	if slope >= 0 {
		return 0, false, false
	}
	return -1 / slope, true, false
}

// mean, variance and correlation of samples.
func mean(x []float64) float64 {
	s := 0.0
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func variance(x []float64) float64 {
	m, s := mean(x), 0.0
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return s / float64(len(x))
}

func covariance(x, y []float64) float64 {
	mx, my, s := mean(x), mean(y), 0.0
	for i := range x {
		s += (x[i] - mx) * (y[i] - my)
	}
	return s / float64(len(x))
}

func correlation(x, y []float64) float64 {
	vx, vy := variance(x), variance(y)
	if vx <= 0 || vy <= 0 {
		return 0
	}
	return covariance(x, y) / math.Sqrt(vx*vy)
}

// ols1 is the least squares line y = c + a*x.
func ols1(x, y []float64) (c, a float64) {
	a = covariance(x, y) / variance(x)
	return mean(y) - a*mean(x), a
}

// ols2 is the least squares plane y = c + a*x1 + b*x2, solved on centred
// data.
func ols2(x1, x2, y []float64) (c, a, b float64) {
	s11, s22, s12 := variance(x1), variance(x2), covariance(x1, x2)
	s1y, s2y := covariance(x1, y), covariance(x2, y)
	det := s11*s22 - s12*s12
	a = (s22*s1y - s12*s2y) / det
	b = (s11*s2y - s12*s1y) / det
	return mean(y) - a*mean(x1) - b*mean(x2), a, b
}
