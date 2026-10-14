package hwemu

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// CPU governor emulation, profile side: the checks of the cpufreq governor
// facts (driverMode, hwp, epp, boostScope, tunables, uncore) and the files
// they render beyond the policies render_cpufreq.go writes, such as
// intel_pstate/, amd_pstate/, the governor tunables and
// intel_uncore_frequency/. A profile that sets none of these facts renders
// and runs as it did before they existed: amd-pstate-epp keeps its EPP pair,
// a set boost stays the global cpufreq/boost, and nothing else appears.
//
// Kernel citations in this file and in node_governor.go are against Linux
// v6.12 unless they say otherwise.

// Boost scopes, the values of cpufreq.boostScope, spelled as pkg/api
// spells cpu.governor.boost.scope: the global cpufreq/boost of acpi-cpufreq
// and the generic drivers, the per-policy boost of amd-pstate
// (cpufreq.c:616-650, local_boost), and the inverted intel_pstate/no_turbo
// of both intel_pstate modes.
const (
	cpuGovBoostGlobal  = api.CPUBoostScopeGlobal
	cpuGovBoostPerCPU  = api.CPUBoostScopePerCPU
	cpuGovBoostNoTurbo = api.CPUBoostScopeNoTurbo
)

// Uncore layouts, the values of cpufreq.uncore.layout: one domain per die
// (the MSR interface, uncore-frequency.c) or ClustersPerDie per die (TPMI,
// uncore-frequency-tpmi.c).
const (
	cpuGovUncorePerDie     = "per-die"
	cpuGovUncorePerCluster = "per-cluster"
)

// cpuGovDriverModes are the modes each driver with modes runs in, as its
// status file shows them (intel_pstate.c:1420-1430; amd-pstate.rst "status").
var cpuGovDriverModes = map[string][]string{
	"intel_pstate":   {"active"},
	"intel_cpufreq":  {"passive"},
	"amd-pstate-epp": {"active"},
	"amd-pstate":     {"passive", "guided"},
}

// cpuGovEPPValues is the EPP each preference word sets: epp_values of
// intel_pstate.c:702-708 (HWP_EPP_*) and the AMD_CPPC_EPP_* values of
// amd-pstate.c, which differ only in balance_power (192 against 191); only
// zero against non-zero matters here. "default" sets the boot value and has
// no number of its own.
var cpuGovEPPValues = map[string]int{"performance": 0, "balance_performance": 128, "balance_power": 192, "power": 255}

// cpuGovMinSamplingUS is CPUFREQ_DBS_MIN_SAMPLING_INTERVAL, two ticks in us
// (cpufreq_governor.c:22), at CONFIG_HZ=1000 (assumed: the distribution
// kernels the profiles stand for).
const cpuGovMinSamplingUS = 2000

// cpuGovUncoreUnitKHz is one uncore ratio step (UNCORE_FREQ_KHZ_MULTIPLIER,
// uncore-frequency.c:38), and cpuGovUncoreMaxRatio the largest ratio the
// 7-bit field holds (uncore-frequency.c:72-74).
const (
	cpuGovUncoreUnitKHz  = 100000
	cpuGovUncoreMaxRatio = 0x7f
)

// cpuGovModeled reports whether f sets any governor fact, which switches the
// governor emulation on for the profile.
func cpuGovModeled(f *CPUFreqSpec) bool {
	return f != nil && !f.Minimal && (f.DriverMode.Src != "" || f.HWP.Src != "" || f.EPP != nil ||
		f.BoostScope.Src != "" || f.Tunables != nil || f.Uncore != nil)
}

// cpuGovIntel reports whether driver is intel_pstate in either mode.
func cpuGovIntel(driver string) bool { return driver == "intel_pstate" || driver == "intel_cpufreq" }

// cpuGovHasEPP reports whether the policies have the EPP pair:
// amd-pstate-epp always, intel_pstate in either mode with HWP
// (intel_pstate.c hwp_cpufreq_attrs).
func cpuGovHasEPP(f *CPUFreqSpec) bool {
	return f.Driver.V == "amd-pstate-epp" || f.HWP.V && cpuGovIntel(f.Driver.V)
}

// cpuGovEPPWords is energy_performance_available_preferences outside the
// performance policy: epp.available, or renderEPPPreferences without it.
func cpuGovEPPWords(f *CPUFreqSpec) []string {
	if f.EPP != nil && f.EPP.Available.V != "" {
		return strings.Fields(f.EPP.Available.V)
	}
	return strings.Fields(renderEPPPreferences)
}

// cpuGovEPPDefault is the preference a policy boots with: epp.default, or
// balance_performance (assumed) without it.
func cpuGovEPPDefault(f *CPUFreqSpec) string {
	if f.EPP != nil && f.EPP.Default.V != "" {
		return f.EPP.Default.V
	}
	return "balance_performance"
}

// cpuGovEPPList formats preference words as both drivers print them, each
// followed by a space (intel_pstate.c:808-819, amd-pstate.c:1136-1152).
func cpuGovEPPList(words []string) string {
	return strings.Join(words, " ") + " \n"
}

// cpuGovDriverBoostScope is where driver keeps its boost switch.
func cpuGovDriverBoostScope(driver string) string {
	switch {
	case cpuGovIntel(driver):
		return cpuGovBoostNoTurbo
	case driver == "amd-pstate" || driver == "amd-pstate-epp":
		return cpuGovBoostPerCPU
	}
	return cpuGovBoostGlobal
}

// cpuGovBoostScope is where f keeps boost, the copy render_cpufreq.go
// writes: the driver's when the governor emulation is on, else the global
// cpufreq/boost of a profile that sets boost, as before the governor facts
// existed. With the governor facts, renderCPUGovernor adds the other copy
// where the driver has set_boost (cpuGovSetBoost).
func cpuGovBoostScope(f *CPUFreqSpec) string {
	if cpuGovModeled(f) {
		return cpuGovDriverBoostScope(f.Driver.V)
	}
	return cpuGovBoostGlobal
}

// cpuGovPolicyBootBoost is what each policy's boost file shows at boot
// where the switch is global (cpuGovBoostGlobal) and the driver has
// set_boost: the policyBoost fact, else 0. The file shows the core's
// per-policy flag (policy->boost_enabled), which the captured acpi-cpufreq
// nodes leave at 0 while cpufreq/boost reads 1 and the CPUs boost.
func cpuGovPolicyBootBoost(f *CPUFreqSpec) int {
	if f.PolicyBoost.Src != "" {
		return f.PolicyBoost.V
	}
	return 0
}

// cpuGovBootBoost is the boost state a policy boots with: the boost fact
// when set, else on.
func cpuGovBootBoost(f *CPUFreqSpec) int {
	if f.Boost.Src != "" {
		return f.Boost.V
	}
	return 1
}

// cpuGovBrandGHz is the base frequency an Intel brand string ends with, as
// in "Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz".
var cpuGovBrandGHz = regexp.MustCompile(`@\s*([0-9]+(?:\.[0-9]+)?)\s*GHz\s*$`)

// cpuGovBaseKHz is the top non-turbo frequency of p's CPUs, the maximum a
// policy keeps with turbo off: the cpufreq.baseKHz fact, else the top entry
// of a frequency table, else the base frequency of the brand string. 0
// means unknown, and turbo off then lowers nothing.
func cpuGovBaseKHz(p *Profile) int64 {
	f := p.CPUFreq
	if f != nil && f.BaseKHz.Src != "" {
		return f.BaseKHz.V
	}
	if f != nil && len(f.AvailableKHz.V) > 0 {
		return slices.Max(f.AvailableKHz.V)
	}
	if m := cpuGovBrandGHz.FindStringSubmatch(p.CPU.ModelName.V); m != nil {
		if ghz, err := strconv.ParseFloat(m[1], 64); err == nil {
			return int64(math.Round(ghz * 1e6))
		}
	}
	return 0
}

// cpuGovSetBoost reports whether f's driver has set_boost, for which the
// cpufreq core creates both the global cpufreq/boost (cpufreq.c:2944-2945)
// and every policy's boost (cpufreq.c:1085-1086): amd-pstate in both modes
// (amd-pstate.c:1736, 1753), acpi-cpufreq where the CPU can boost
// (acpi-cpufreq.c:963-969) and the generic drivers, both only with the
// boost fact, and intel_pstate in neither mode, which has no_turbo instead.
func cpuGovSetBoost(f *CPUFreqSpec) bool {
	switch {
	case cpuGovIntel(f.Driver.V):
		return false
	case f.Driver.V == "amd-pstate" || f.Driver.V == "amd-pstate-epp":
		return true
	}
	return f.Boost.Src != ""
}

// cpuGovPstateUnitKHz is one intel_pstate P-state on the CPUs the profiles
// model (core_get_scaling, intel_pstate.c:311-314).
const cpuGovPstateUnitKHz = 100000

// cpuGovTurboPct is intel_pstate/num_pstates, the P-states from the lowest
// to the top turbo one, and intel_pstate/turbo_pct, the share of them above
// the base frequency, as show_num_pstates and show_turbo_pct compute them in
// 8-bit fixed point (intel_pstate.c:51-53, 62-70, 1445-1490). ok is false
// while the base frequency is unknown.
func cpuGovTurboPct(p *Profile) (turboPct, numPstates int64, ok bool) {
	f, base := p.CPUFreq, cpuGovBaseKHz(p)
	lo, hi := f.CPUInfoMinKHz.V/cpuGovPstateUnitKHz, f.CPUInfoMaxKHz.V/cpuGovPstateUnitKHz
	mid := base / cpuGovPstateUnitKHz
	if base <= 0 || lo <= 0 || mid < lo || hi < mid {
		return 0, 0, false
	}
	total, noTurbo := hi-lo+1, mid-lo+1
	turboFP := (noTurbo << 8) / total
	return 100 - (turboFP*(100<<8)>>8)>>8, total, true
}

// cpuGovTunable is one governor tunable of the profile: the governor, its
// sysfs attribute and the fact that sets it.
type cpuGovTunable struct {
	gov, name string
	fact      Fact[int64]
}

// cpuGovTunables lists the tunables t sets, in the order they render.
func cpuGovTunables(t *GovernorTunablesSpec) []cpuGovTunable {
	if t == nil {
		return nil
	}
	all := []cpuGovTunable{
		{"conservative", "down_threshold", t.Conservative.DownThreshold},
		{"conservative", "freq_step", t.Conservative.FreqStep},
		{"conservative", "up_threshold", t.Conservative.UpThreshold},
		{"ondemand", "sampling_rate", t.Ondemand.SamplingRate},
		{"ondemand", "up_threshold", t.Ondemand.UpThreshold},
		{"schedutil", "rate_limit_us", t.Schedutil.RateLimitUS},
	}
	var out []cpuGovTunable
	for _, x := range all {
		if x.fact.Src != "" {
			out = append(out, x)
		}
	}
	return out
}

// cpuGovTunableKey names a tunable in nodeCPUGovernor.tunables.
func cpuGovTunableKey(gov, name string) string { return gov + "/" + name }

// cpuGovTunableCheck is the kernel's check of a tunable write: the value in
// effect after it and whether the kernel refuses it (EINVAL). up and down
// are conservative's thresholds in effect. schedutil takes any 32-bit value
// (cpufreq_schedutil.c:567-576); ondemand's up_threshold lies in [1, 100]
// (cpufreq_ondemand.c:25-26, 225-235) and sampling_rate is at least two
// ticks (cpufreq_governor.c:22, 53); conservative caps freq_step at 100
// (cpufreq_conservative.c:222-235) and keeps 1 <= down_threshold <
// up_threshold <= 100 (cpufreq_conservative.c:164-194).
func cpuGovTunableCheck(gov, name string, v, up, down int64) (int64, bool) {
	switch cpuGovTunableKey(gov, name) {
	case "ondemand/up_threshold":
		return v, v >= 1 && v <= 100
	case "ondemand/sampling_rate":
		return v, v >= cpuGovMinSamplingUS
	case "conservative/freq_step":
		return min(v, 100), true
	case "conservative/up_threshold":
		return v, v <= 100 && v > down
	case "conservative/down_threshold":
		return v, v >= 1 && v < up
	}
	return v, v <= math.MaxUint32
}

// cpuGovUncoreDomain is one uncore domain: its directory under sys/, the
// package it belongs to and, on the TPMI layout, its die and cluster, or
// root for the package's root domain.
type cpuGovUncoreDomain struct {
	dir                   string
	socket, die, cluster  int
	perCluster, root      bool
	domainID, uncoreIndex int
}

// cpuGovUncoreDomains lists the uncore domains of p, each package's in the
// order the driver adds them. On the TPMI layout with one die per package,
// each package also has a root domain, package_PP_die_00, added after its
// clusters (uncore-frequency-tpmi.c:584-592; its name,
// uncore-frequency-common.c:232-241).
func cpuGovUncoreDomains(p *Profile) []cpuGovUncoreDomain {
	u := p.CPUFreq.Uncore
	sockets, dies := max(1, p.CPU.Sockets.V), max(1, p.CPU.DiesPerPackage.V)
	var out []cpuGovUncoreDomain
	if u.Layout.V == cpuGovUncorePerCluster {
		id := 0
		for s := 0; s < sockets; s++ {
			for d := 0; d < dies; d++ {
				for c := 0; c < max(1, u.ClustersPerDie.V); c++ {
					out = append(out, cpuGovUncoreDomain{dir: layout.IntelUncoreClusterDir(id), socket: s, die: d, cluster: c, perCluster: true, domainID: d, uncoreIndex: id})
					id++
				}
			}
			if dies == 1 {
				out = append(out, cpuGovUncoreDomain{dir: layout.IntelUncoreDieDir(s, 0), socket: s, root: true})
			}
		}
		return out
	}
	for s := 0; s < sockets; s++ {
		for d := 0; d < dies; d++ {
			out = append(out, cpuGovUncoreDomain{dir: layout.IntelUncoreDieDir(s, d), socket: s, die: d})
		}
	}
	return out
}

// cpuGovELC are the efficiency latency control attributes of an uncore
// domain with ELC and the values they start at (assumed: the firmware
// programs them and no source gives the defaults; the floor starts at the
// domain's initial minimum).
func cpuGovELC(u *UncoreSpec) []struct {
	name string
	v    int64
} {
	return []struct {
		name string
		v    int64
	}{
		{"elc_floor_freq_khz", u.InitialMinKHz.V},
		{"elc_high_threshold_enable", 1},
		{"elc_high_threshold_percent", 95},
		{"elc_low_threshold_percent", 10},
	}
}

// cpuGovMinPerfPct is min_perf_pct at boot: the lowest P-state as a
// percentage of the turbo one, rounded down (intel_pstate.c:608-615, 3391).
func cpuGovMinPerfPct(f *CPUFreqSpec) int64 {
	if f.CPUInfoMaxKHz.V <= 0 {
		return 0
	}
	return f.CPUInfoMinKHz.V * 100 / f.CPUInfoMaxKHz.V
}

// renderValidateCPUGovernor returns every problem with the governor facts of
// p.CPUFreq: a driver mode the driver does not run in, hwp on a driver that
// is not intel_pstate, EPP facts on policies without EPP or with a default
// they do not offer, a boost scope that is not the driver's, a tunable the
// kernel would refuse, an uncore block off Intel or with a range the ratio
// field cannot hold, and a base frequency outside the cpuinfo range.
func renderValidateCPUGovernor(p *Profile) []error {
	f := p.CPUFreq
	if f == nil || f.Minimal {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: cpufreq."+format, append([]any{p.Name}, args...)...))
	}
	driver := f.Driver.V
	if m := f.DriverMode.V; m != "" {
		modes, ok := cpuGovDriverModes[driver]
		switch {
		case !ok:
			fail("driverMode %q is set, but %s has no modes", m, driver)
		case !slices.Contains(modes, m):
			fail("driverMode %q is not a mode of %s (%s)", m, driver, strings.Join(modes, ", "))
		}
	}
	if b := f.BaseKHz; b.Src != "" && (b.V < f.CPUInfoMinKHz.V || b.V > f.CPUInfoMaxKHz.V) {
		fail("baseKHz %d is outside [cpuinfoMinKHz, cpuinfoMaxKHz] [%d, %d]", b.V, f.CPUInfoMinKHz.V, f.CPUInfoMaxKHz.V)
	}
	if f.HWP.V && !cpuGovIntel(driver) {
		fail("hwp is true on %s, which is not an Intel driver (intel_pstate or intel_cpufreq)", driver)
	}
	if e := f.EPP; e != nil {
		if !cpuGovHasEPP(f) {
			fail("epp is set, but %s policies have no energy_performance_preference (amd-pstate-epp, or an Intel driver with hwp)", driver)
		}
		words := strings.Fields(e.Available.V)
		if len(words) == 0 {
			fail("epp.available is empty")
		}
		for _, w := range words {
			if _, ok := cpuGovEPPValues[w]; !ok && w != "default" {
				fail("epp.available word %q is not a preference the drivers know", w)
			}
		}
		if d := e.Default.V; d == "default" || !slices.Contains(words, d) {
			fail("epp.default %q is not a preference of epp.available %q", d, e.Available.V)
		}
	}
	if s := f.BoostScope.V; s != "" {
		switch {
		case s != cpuGovBoostGlobal && s != cpuGovBoostPerCPU && s != cpuGovBoostNoTurbo:
			fail("boostScope %q is not %s, %s or %s", s, cpuGovBoostGlobal, cpuGovBoostPerCPU, cpuGovBoostNoTurbo)
		case s != cpuGovDriverBoostScope(driver):
			fail("boostScope %q is not where %s keeps boost (%s)", s, driver, cpuGovDriverBoostScope(driver))
		}
	}
	t := f.Tunables
	if t != nil {
		up, down := t.Conservative.UpThreshold.V, t.Conservative.DownThreshold.V
		for _, x := range cpuGovTunables(t) {
			if got, ok := cpuGovTunableCheck(x.gov, x.name, x.fact.V, up, down); !ok || got != x.fact.V || x.fact.V < 0 {
				fail("tunables.%s.%s %d is a value the kernel refuses", x.gov, x.name, x.fact.V)
			}
		}
	}
	if u := f.Uncore; u != nil {
		if !cpuGovIntel(driver) {
			fail("uncore is set on %s; intel_uncore_frequency needs an Intel CPU", driver)
		}
		switch u.Layout.V {
		case cpuGovUncorePerDie:
		case cpuGovUncorePerCluster:
			if u.ClustersPerDie.V < 1 {
				fail("uncore.clustersPerDie %d, want at least 1 on the %s layout", u.ClustersPerDie.V, cpuGovUncorePerCluster)
			}
		default:
			fail("uncore.layout %q is not %s or %s", u.Layout.V, cpuGovUncorePerDie, cpuGovUncorePerCluster)
		}
		lo, hi := u.InitialMinKHz.V, u.InitialMaxKHz.V
		if lo < cpuGovUncoreUnitKHz || hi < lo || hi > cpuGovUncoreMaxRatio*cpuGovUncoreUnitKHz || lo%cpuGovUncoreUnitKHz != 0 || hi%cpuGovUncoreUnitKHz != 0 {
			fail("uncore needs 100 MHz <= initialMinKHz <= initialMaxKHz <= 12.7 GHz in whole 100 MHz ratios, got %d and %d", lo, hi)
		}
	}
	return errs
}

// renderCPUGovernor writes the governor files of p that render_cpufreq.go
// does not: intel_pstate/ (status R; no_turbo, max_perf_pct, min_perf_pct A;
// hwp_dynamic_boost A in active mode with HWP; turbo_pct and num_pstates R),
// amd_pstate/status (R), the copy of boost render_cpufreq.go leaves out
// where the driver has set_boost (A), the tunables of each available
// governor under cpufreq/<governor> (A; x86 drivers keep them global,
// cpufreq.rst "Policy Interface in sysfs"), and intel_uncore_frequency/
// with cpufreq.uncore. Every tunable directory is rendered, where the
// kernel creates one only while a policy uses its governor (assumed: one
// tree for the node's whole life).
func renderCPUGovernor(b *renderBuilder, p *Profile, o RenderOptions) error {
	f := p.CPUFreq
	if !cpuGovModeled(f) {
		return nil
	}
	driver := f.Driver.V
	if cpuGovIntel(driver) {
		// status prints the mode (intel_pstate.c:1420-1430) and is root
		// writable, like every global intel_pstate attribute; the
		// hwp_dynamic_boost file exists in active mode with HWP only
		// (intel_pstate.c:1810-1822, 3372-3373). min_perf_pct starts at
		// the lowest P-state (intel_pstate.c:3391).
		mode := "passive"
		if driver == "intel_pstate" {
			mode = "active"
		}
		entries := []renderEntry{
			{name: "status", content: mode + "\n", owner: layout.OwnerRender, mode: 0o644},
			{name: "no_turbo", content: renderUint(1 - cpuGovBootBoost(f)), owner: layout.OwnerAgent, mode: 0o644},
			{name: "max_perf_pct", content: "100\n", owner: layout.OwnerAgent, mode: 0o644},
			{name: "min_perf_pct", content: renderUint(cpuGovMinPerfPct(f)), owner: layout.OwnerAgent, mode: 0o644},
		}
		if driver == "intel_pstate" && f.HWP.V {
			entries = append(entries, renderEntry{name: "hwp_dynamic_boost", content: "0\n", owner: layout.OwnerAgent, mode: 0o644})
		}
		// turbo_pct and num_pstates, 0444, exist in both modes on every
		// CPU that is not hybrid (intel_pstate.c:1729-1730, 1764-1770),
		// which no profile models.
		if pct, num, ok := cpuGovTurboPct(p); ok {
			entries = append(entries,
				renderEntry{name: "turbo_pct", content: renderUint(pct), owner: layout.OwnerRender, mode: 0o444},
				renderEntry{name: "num_pstates", content: renderUint(num), owner: layout.OwnerRender, mode: 0o444},
			)
		}
		if err := b.writeEntries(renderSys(layout.IntelPstateDir()), entries); err != nil {
			return err
		}
	}
	if cpuGovSetBoost(f) {
		// render_cpufreq.go writes boost where the driver keeps it
		// (cpuGovBoostScope); set_boost makes the core create the other
		// copy too. amd-pstate's global copy starts at the boost fact, the
		// policies' boost support (amd-pstate.c:995, 1464), assumed equal
		// to the global state it sets where the CPU has CPB
		// (amd-pstate.c:747). acpi-cpufreq's policies show the per-policy
		// flag, cpuGovPolicyBootBoost.
		switch cpuGovBoostScope(f) {
		case cpuGovBoostPerCPU:
			if err := b.file(renderSys(path.Join(layout.CPUFreqDir(), "boost")), renderUint(cpuGovBootBoost(f)), layout.OwnerAgent, 0o644); err != nil {
				return err
			}
		case cpuGovBoostGlobal:
			boot := renderUint(cpuGovPolicyBootBoost(f))
			for _, pol := range renderPolicies(p, renderCPUs(p)) {
				if err := b.file(renderSys(path.Join(layout.PolicyDir(pol.ID), "boost")), boot, layout.OwnerAgent, 0o644); err != nil {
					return err
				}
			}
		}
	}
	if driver == "amd-pstate" || driver == "amd-pstate-epp" {
		mode := f.DriverMode.V
		if mode == "" {
			mode = cpuGovDriverModes[driver][0]
		}
		if err := b.file(renderSys(path.Join(layout.AMDPstateDir(), "status")), mode+"\n", layout.OwnerRender, 0o644); err != nil {
			return err
		}
	}
	govs := strings.Fields(f.AvailableGovernors.V)
	for _, x := range cpuGovTunables(f.Tunables) {
		if !slices.Contains(govs, x.gov) {
			continue
		}
		if err := b.file(renderSys(path.Join(layout.GovernorTunablesDir(x.gov), x.name)), renderUint(x.fact.V), layout.OwnerAgent, 0o644); err != nil {
			return err
		}
	}
	if f.Uncore != nil {
		return renderUncore(b, p)
	}
	return nil
}

// renderUncore writes one directory per uncore domain
// (uncore-frequency-common.c:164-206): min_freq_khz and max_freq_khz (A,
// 0644), initial_min_freq_khz and initial_max_freq_khz (R, 0444),
// current_freq_khz (S, 0400, root only), on the TPMI layout the domain's
// package_id, domain_id and fabric_cluster_id (R, 0400), and with ELC its
// four efficiency latency control attributes (A, 0644). Every domain starts
// at its initial range and current at the minimum. A TPMI root domain has
// the four limit files only: it has no domain id, and the driver answers
// ENODATA for its current frequency and ELC, so the core leaves those out
// (uncore-frequency-common.c:175-207; uncore-frequency-tpmi.c:105-106,
// 341-342).
func renderUncore(b *renderBuilder, p *Profile) error {
	u := p.CPUFreq.Uncore
	for _, d := range cpuGovUncoreDomains(p) {
		if d.root {
			if err := b.writeEntries(renderSys(d.dir), []renderEntry{
				{name: "max_freq_khz", content: renderUint(u.InitialMaxKHz.V), owner: layout.OwnerAgent, mode: 0o644},
				{name: "min_freq_khz", content: renderUint(u.InitialMinKHz.V), owner: layout.OwnerAgent, mode: 0o644},
				{name: "initial_min_freq_khz", content: renderUint(u.InitialMinKHz.V), owner: layout.OwnerRender, mode: 0o444},
				{name: "initial_max_freq_khz", content: renderUint(u.InitialMaxKHz.V), owner: layout.OwnerRender, mode: 0o444},
			}); err != nil {
				return err
			}
			continue
		}
		var entries []renderEntry
		if d.perCluster {
			entries = append(entries,
				renderEntry{name: "domain_id", content: renderUint(d.domainID), owner: layout.OwnerRender, mode: 0o400},
				renderEntry{name: "fabric_cluster_id", content: renderUint(d.cluster), owner: layout.OwnerRender, mode: 0o400},
				renderEntry{name: "package_id", content: renderUint(d.socket), owner: layout.OwnerRender, mode: 0o400},
			)
		}
		entries = append(entries,
			renderEntry{name: "max_freq_khz", content: renderUint(u.InitialMaxKHz.V), owner: layout.OwnerAgent, mode: 0o644},
			renderEntry{name: "min_freq_khz", content: renderUint(u.InitialMinKHz.V), owner: layout.OwnerAgent, mode: 0o644},
			renderEntry{name: "initial_min_freq_khz", content: renderUint(u.InitialMinKHz.V), owner: layout.OwnerRender, mode: 0o444},
			renderEntry{name: "initial_max_freq_khz", content: renderUint(u.InitialMaxKHz.V), owner: layout.OwnerRender, mode: 0o444},
			renderEntry{name: "current_freq_khz", content: renderUint(u.InitialMinKHz.V), owner: layout.OwnerEmulator, mode: 0o400},
		)
		if u.ELC.V {
			for _, e := range cpuGovELC(u) {
				entries = append(entries, renderEntry{name: e.name, content: renderUint(e.v), owner: layout.OwnerAgent, mode: 0o644})
			}
		}
		if err := b.writeEntries(renderSys(d.dir), entries); err != nil {
			return err
		}
	}
	return nil
}
