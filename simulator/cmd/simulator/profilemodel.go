package main

// The profile adapter: the legacy model of a node an hwemu profile describes,
// so that standalone mode runs the hardware emulate mode emulates.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

const (
	// profileMMIOControlType is the powercap control type that mirrors the
	// package zones of intel-rapl with limits of its own. The legacy model
	// has one CPU cap per node and cannot hold a second set.
	profileMMIOControlType = "intel-rapl-mmio"
	// profileCapMinW is the lowest CPU and GPU cap of a profile node whose
	// profile gives none: RAPL has no minimum (no get_min_power op) and an
	// MI300X reports a minimum of 0, while the legacy model needs one above 0.
	profileCapMinW = 1
	// profileDefaultMinScale is the lowest frequency scale of a profile
	// without cpufreq, the one hwemu assumes for it.
	profileDefaultMinScale = 0.35
	// profileGPUTelemetryWindowMS is the GPU power average of a profile that
	// sets none: NVML averages over 1 s, and hwemu assumes it for amdgpu.
	profileGPUTelemetryWindowMS = 1000
)

var (
	profilePackageZone    = regexp.MustCompile(`^package-(\d+)$`)
	profilePackageDieZone = regexp.MustCompile(`^package-(\d+)-die-\d+$`)
)

// simModelFromProfile is the legacy model of a node of profile p, and the
// only adapter from an hwemu profile to the legacy model (X10). The profile
// sets the CPU and GPU power, the frequency range, the caps and their
// settling times; base sets what a profile has no fact for: the RAPL
// headroom, the CPU telemetry window and the thermal constants. Thermal
// throttling is off, as hwemu models none. Per profile:
//   - idle and peak are sockets times idlePkgW and maxPkgW, and a
//     catalog-curve CPU takes the node2S curve of its catalogue entry, scaled
//     by sockets/2;
//   - the frequency range comes from cpuinfo_min_freq and cpuinfo_max_freq,
//     and the knee is knee times the maximum;
//   - the CPU cap range and default are the sums over the sockets of the PL1
//     (long_term) limits, else of the HSMP caps; a node with neither is
//     capped at its peak, which never binds;
//   - the GPU range is limits.minW (1 W when 0) to naturalMaxW.
//
// A profile without CPU package power (maxPkgW 0, a VM), one with
// intel-rapl-mmio, and one whose naturalMaxW differs from limits.maxW are
// errors.
func simModelFromProfile(p *hwemu.Profile, base simModel) (simModel, error) {
	if p == nil {
		return simModel{}, fmt.Errorf("no profile")
	}
	cpu := p.Physics.CPU
	if cpu.MaxPkgW.V <= 0 {
		return simModel{}, fmt.Errorf("profile %s: physics.cpu.maxPkgW is %g: the node has no CPU package power to model", p.Name, cpu.MaxPkgW.V)
	}
	if p.Powercap != nil {
		for _, ct := range p.Powercap.ControlTypes {
			if ct.Name == profileMMIOControlType {
				return simModel{}, fmt.Errorf("profile %s: powercap control type %s has package limits of its own, and the legacy model has one CPU cap", p.Name, ct.Name)
			}
		}
	}
	sockets := max(1, p.CPU.Sockets.V)
	n := float64(sockets)

	m := base
	m.Defaulted = hw.Defaulted{}
	m.CPUModel = p.CPU.ModelName.V
	m.CPUProvenance = profileProvenance(p)
	m.CPUProxyFrom = ""
	m.CPUSockets = sockets
	m.BaseIdleW = n * cpu.IdlePkgW.V
	m.PMaxW = n * cpu.MaxPkgW.V
	// A zero exponent means 1, as in phys.AnalyticCPUModel.
	m.AlphaUtil = profileExponent(cpu.AlphaUtil.V)
	m.BetaFreq = profileExponent(cpu.BetaFreq.V)
	m.CPUPowerCurve, m.CPUCurveSource = nil, ""
	if cpu.Model == "catalog-curve" {
		points, source, err := profileCatalogCurve(p.CPU.ModelName.V, sockets)
		if err != nil {
			return simModel{}, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		m.CPUPowerCurve, m.CPUCurveSource = points, source
	}
	m.FMinMHz, m.FMaxMHz = profileFrequencyMHz(p, base.FMaxMHz)
	m.CPULowestNonlinearFreqMHz = cpu.Knee.V * m.FMaxMHz
	m.DvfsRampMS = cpu.FreqRampMS.V
	m.CPUDriverFamily = ""
	if p.CPUFreq != nil {
		m.CPUDriverFamily = p.CPUFreq.Driver.V
	}

	lim, err := profileCPULimits(p)
	if err != nil {
		return simModel{}, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	m.RaplCapMinW = profileCapMinW
	if lim.ok {
		m.RaplCapMaxW, m.DefaultCapW, m.CPUCapApplyTauMS = lim.maxW, lim.defaultW, lim.tauMS
	} else {
		peak := profilePeakW(m)
		m.RaplCapMaxW, m.DefaultCapW = peak, peak
	}
	m.CPUSocketCapMinW = 0
	m.CPUSocketCapMaxW = m.RaplCapMaxW / n

	m.CPUThermalThrottleStartC, m.CPUThermalThrottleFullC = 0, 0
	if m.CPUWattsPerDeltaC <= 0 {
		m.CPUWattsPerDeltaC = 8
	}
	gpu, err := profileGPU(p, base.GPU)
	if err != nil {
		return simModel{}, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	m.GPU = gpu
	if err := hw.ValidateProfile(m); err != nil {
		return simModel{}, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	return m, nil
}

// profileHasWritablePL1 reports whether the agent can write a PL1 limit of
// every package of a profile node, so that eco caps its CPU power. An HSMP
// cap does not count: the agent has no HSMP path.
func profileHasWritablePL1(p *hwemu.Profile) bool {
	lim, err := profileCPULimits(p)
	return err == nil && lim.ok && lim.writablePL1
}

func profileProvenance(p *hwemu.Profile) string { return "profile:" + p.Name }

func profileExponent(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}

// profileFrequencyMHz is the frequency range of a profile node: its cpuinfo
// limits, or without them fallbackMaxMHz and the minimum scale hwemu assumes.
func profileFrequencyMHz(p *hwemu.Profile, fallbackMaxMHz float64) (minMHz, maxMHz float64) {
	if cf := p.CPUFreq; cf != nil && cf.CPUInfoMinKHz.V > 0 && cf.CPUInfoMaxKHz.V > 0 {
		return float64(cf.CPUInfoMinKHz.V) / 1000, float64(cf.CPUInfoMaxKHz.V) / 1000
	}
	return profileDefaultMinScale * fallbackMaxMHz, fallbackMaxMHz
}

// profileCatalogCurve is the node curve of a catalog-curve CPU: the node2S
// curve of its catalogue entry, which hwemu splits over two packages, times
// sockets packages. It reads the embedded catalogue, as hwemu does.
func profileCatalogCurve(modelName string, sockets int) ([]hw.PowerPoint, string, error) {
	cat, err := hw.LoadCatalog("")
	if err != nil {
		return nil, "", err
	}
	_, spec, ok := cat.MatchCPU(modelName)
	if !ok || spec.MeasuredCurves == nil || spec.MeasuredCurves.Node2S == nil || len(spec.MeasuredCurves.Node2S.Points) == 0 {
		return nil, "", fmt.Errorf("no catalogue curve for cpu %q", modelName)
	}
	curve := spec.MeasuredCurves.Node2S
	scale := float64(sockets) / 2
	points := make([]hw.PowerPoint, 0, len(curve.Points))
	for _, pt := range curve.Points {
		points = append(points, hw.PowerPoint{LoadPct: pt.LoadPct, PowerW: pt.PowerW * scale})
	}
	return points, curve.Source, nil
}

// profilePeakW is the highest CPU power of m: its curve's last point, or its
// analytic peak.
func profilePeakW(m simModel) float64 {
	peak := m.PMaxW
	if k := len(m.CPUPowerCurve); k > 0 {
		peak = math.Max(peak, m.CPUPowerCurve[k-1].PowerW)
	}
	return peak
}

// profileCPULimit is the CPU package power limit of a profile node, summed
// over its sockets. ok is false when some package has neither a PL1 nor an
// HSMP cap.
type profileCPULimit struct {
	ok          bool
	maxW        float64 // the highest limit that can be set
	defaultW    float64 // the limit in force when the node starts
	tauMS       int     // the settling time of the limit
	writablePL1 bool    // the limit is a PL1 the agent can write
}

// profileCPULimits reads the CPU limit of a profile the way hwemu enforces
// it: per package, the lowest PL1 of its enabled package zones, with a die
// zone's limit counted once per die, in force unless the constraint is
// disabled; without one, the HSMP cap. A PL1 settles over its time window,
// or physics.rapl.settleTau when that is a duration, and HSMP over
// physics.hsmp.settleTauMS.
func profileCPULimits(p *hwemu.Profile) (profileCPULimit, error) {
	sockets := max(1, p.CPU.Sockets.V)
	dies := max(1, p.CPU.DiesPerPackage.V)
	maxW := make([]float64, sockets)
	defW := make([]float64, sockets)
	for i := range maxW {
		maxW[i], defW[i] = math.Inf(1), math.Inf(1)
	}
	writable := true
	windowUS := int64(-1)
	if p.Powercap != nil {
		for _, ct := range p.Powercap.ControlTypes {
			for _, zs := range ct.Zones {
				if zs.Enabled.V != 1 {
					continue
				}
				for _, z := range profilePackageZones(zs, sockets, dies) {
					for _, c := range zs.Constraints {
						if c.Name != "long_term" || c.RejectWrites.V == "disabled" {
							continue
						}
						limitUW, maxUW := c.PowerLimitUW.V, c.MaxPowerUW.V
						if maxUW > 0 && limitUW > maxUW {
							limitUW = maxUW // the hardware clamp hwemu applies
						}
						if maxUW <= 0 {
							maxUW = limitUW
						}
						maxW[z.socket] = math.Min(maxW[z.socket], float64(maxUW)/1e6*z.mult)
						defW[z.socket] = math.Min(defW[z.socket], float64(limitUW)/1e6*z.mult)
						writable = writable && c.RejectWrites.V == ""
						if windowUS < 0 {
							windowUS = c.TimeWindowUS.V
						}
					}
				}
			}
		}
	}
	if lim, ok := profileSumSockets(maxW, defW); ok {
		lim.writablePL1 = writable
		lim.tauMS = int(math.Round(float64(windowUS) / 1000))
		switch tau := p.Physics.RAPL.SettleTau; tau {
		case "", "timeWindow":
		default:
			d, err := time.ParseDuration(tau)
			if err != nil || d < 0 {
				return profileCPULimit{}, fmt.Errorf("physics.rapl.settleTau %q is neither timeWindow nor a duration", tau)
			}
			lim.tauMS = int(d.Milliseconds())
		}
		return lim, nil
	}
	if h := p.HSMP; h != nil && h.CapMaxUW.V > 0 {
		defUW := h.CapDefaultUW.V
		if defUW <= 0 || defUW > h.CapMaxUW.V {
			defUW = h.CapMaxUW.V
		}
		n := float64(sockets)
		return profileCPULimit{
			ok:       true,
			maxW:     n * float64(h.CapMaxUW.V) / 1e6,
			defaultW: n * float64(defUW) / 1e6,
			tauMS:    p.Physics.HSMP.SettleTauMS.V,
		}, nil
	}
	return profileCPULimit{}, nil
}

// profileSumSockets sums the per-socket limits; ok is false when a socket
// has none.
func profileSumSockets(maxW, defW []float64) (profileCPULimit, bool) {
	var lim profileCPULimit
	for i := range maxW {
		if math.IsInf(maxW[i], 1) || math.IsInf(defW[i], 1) {
			return profileCPULimit{}, false
		}
		lim.maxW += maxW[i]
		lim.defaultW += defW[i]
	}
	lim.ok = true
	return lim, true
}

// profileZone is one copy of a package or die zone: its socket, and how many
// times its limit counts toward the package's.
type profileZone struct {
	socket int
	mult   float64
}

// profilePackageZones lists the copies of zs that limit a package, as hwemu
// lays them out: once per socket, once per die, or once when its name is
// package-N or package-N-die-M. Other zones (psys, dram) limit no package.
func profilePackageZones(zs hwemu.ZoneSpec, sockets, dies int) []profileZone {
	var out []profileZone
	switch zs.Repeat {
	case "per-package":
		for x := 0; x < sockets; x++ {
			out = append(out, profileZone{socket: x, mult: 1})
		}
	case "per-die":
		for x := 0; x < sockets; x++ {
			for y := 0; y < dies; y++ {
				out = append(out, profileZone{socket: x, mult: float64(dies)})
			}
		}
	case "once", "":
		name := strings.ReplaceAll(zs.NameFormat, "%d", "0")
		if m := profilePackageZone.FindStringSubmatch(name); m != nil {
			if s, err := strconv.Atoi(m[1]); err == nil && s < sockets {
				out = append(out, profileZone{socket: s, mult: 1})
			}
		} else if m := profilePackageDieZone.FindStringSubmatch(name); m != nil {
			if s, err := strconv.Atoi(m[1]); err == nil && s < sockets {
				out = append(out, profileZone{socket: s, mult: float64(dies)})
			}
		}
	}
	return out
}

// profileGPU is the GPU part of a profile node's model. base gives the
// thermal constants, with throttling off.
func profileGPU(p *hwemu.Profile, base hw.GPUProfile) (hw.GPUProfile, error) {
	out := hw.GPUProfile{
		AmbientTempC:   base.AmbientTempC,
		ThermalTauMS:   base.ThermalTauMS,
		WattsPerDeltaC: base.WattsPerDeltaC,
	}
	if out.WattsPerDeltaC <= 0 {
		out.WattsPerDeltaC = 12
	}
	g := p.GPUs
	if g == nil || g.Count.V <= 0 {
		return out, nil
	}
	ph := p.Physics.GPU
	if ph.NaturalMaxW.V != g.Limits.MaxW.V {
		return hw.GPUProfile{}, fmt.Errorf("physics.gpu.naturalMaxW %g differs from gpus.limits.maxW %g, and the legacy model has one GPU maximum", ph.NaturalMaxW.V, g.Limits.MaxW.V)
	}
	minW := g.Limits.MinW.V
	if minW <= 0 {
		minW = profileCapMinW
	}
	window := ph.TelemetryWindowMS.V
	if window <= 0 {
		window = profileGPUTelemetryWindowMS
	}
	out.Vendor = g.Vendor
	out.Product = g.Product.V
	out.Count = g.Count.V
	out.IdleWattsPerGPU = ph.IdleW.V
	out.MaxWattsPerGPU = ph.NaturalMaxW.V
	out.MinCapWattsPerGPU = minW
	out.CapApplyTauMS = ph.CapSettleTauMS.V
	out.TelemetryWindowMS = window
	out.ComputeGamma = ph.ComputeGamma.V
	out.MemoryEpsilon = ph.MemoryEpsilon.V
	out.MemoryGamma = ph.MemoryGamma.V
	out.Provenance = profileProvenance(p)
	return out, nil
}
