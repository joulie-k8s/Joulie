package governor

import (
	"fmt"
	"slices"
	"strconv"

	joulie "github.com/matbun/joulie/pkg/api"
)

// Native cpufreq words: scaling_governor values and
// energy_performance_preference values.
const (
	govPerformance  = "performance"
	govPowersave    = "powersave"
	govSchedutil    = "schedutil"
	govOndemand     = "ondemand"
	govConservative = "conservative"

	eppBalancePerformance = "balance_performance"
	eppBalancePower       = "balance_power"
)

// cpufreqDir is the cpufreq directory relative to the host's /sys.
const cpufreqDir = "devices/system/cpu/cpufreq"

// cpuChoice is the native state one preset maps to on one node: the
// governor, the energy-performance preference (empty when the plan writes
// none) and the result the plan gives.
type cpuChoice struct {
	governor, epp, result string
}

// cpuActiveDriver reports whether the driver runs its own P-state selection
// algorithms behind the governor words performance and powersave
// (intel_pstate in active mode, amd-pstate-epp), rather than the generic
// cpufreq governors.
func cpuActiveDriver(driver string) bool {
	return driver == "intel_pstate" || driver == "amd-pstate-epp"
}

// cpuRowHasEPP reports whether the driver's row of the mapping table sets an
// energy-performance preference for balanced and efficiency: amd-pstate-epp
// always, intel_pstate and intel_cpufreq with hardware P-states.
func cpuRowHasEPP(f CPUFacts) bool {
	switch f.Driver {
	case "amd-pstate-epp":
		return true
	case "intel_pstate", "intel_cpufreq":
		return f.HWP
	}
	return false
}

// cpuGovernorWords lists the governor words of a preset in order of
// preference. The first one in scaling_available_governors wins; any but the
// first makes the preset equivalent.
func cpuGovernorWords(driver, preset string) []string {
	switch {
	case preset == joulie.CPUGovernorPresetPerformance:
		return []string{govPerformance}
	case cpuActiveDriver(driver):
		return []string{govPowersave}
	case preset == joulie.CPUGovernorPresetBalanced:
		return []string{govSchedutil, govOndemand}
	default:
		return []string{govConservative, govSchedutil}
	}
}

// cpuEPPWord is the energy-performance preference of a preset on a row that
// sets one. performance has none: the kernel rejects a non-zero preference
// under the performance governor.
func cpuEPPWord(preset string) string {
	switch preset {
	case joulie.CPUGovernorPresetBalanced:
		return eppBalancePerformance
	case joulie.CPUGovernorPresetEfficiency:
		return eppBalancePower
	}
	return ""
}

// chooseCPU maps a known preset to its native state on f.
func chooseCPU(f CPUFacts, preset string) cpuChoice {
	if f.Driver == "" || len(f.Policies) == 0 {
		return cpuChoice{result: joulie.GovernorResultUnsupported}
	}
	c := cpuChoice{result: joulie.GovernorResultApplied}
	for i, w := range cpuGovernorWords(f.Driver, preset) {
		if slices.Contains(f.Governors, w) {
			c.governor = w
			if i > 0 {
				c.result = joulie.GovernorResultEquivalent
			}
			break
		}
	}
	if c.governor == "" {
		return cpuChoice{result: joulie.GovernorResultUnsupported}
	}
	if want := cpuEPPWord(preset); want != "" && cpuRowHasEPP(f) {
		if slices.Contains(f.EPPs, want) {
			c.epp = want
		} else {
			c.result = joulie.GovernorResultEquivalent
		}
	}
	// On a driver with its own algorithms, efficiency is powersave with an
	// energy bias; without the preference it is balanced's state.
	if cpuActiveDriver(f.Driver) && preset == joulie.CPUGovernorPresetEfficiency && c.epp == "" {
		c.result = joulie.GovernorResultEquivalent
	}
	return c
}

// policyPath is the attribute file of cpufreq policy n.
func policyPath(n int, file string) string {
	return cpufreqDir + "/policy" + strconv.Itoa(n) + "/" + file
}

// sortedPolicies returns the policy numbers sorted, without duplicates.
func sortedPolicies(ps []int) []int {
	out := slices.Clone(ps)
	slices.Sort(out)
	return slices.Compact(out)
}

// cpuBoostWrites plans the boost word on f: the global boost file, the
// inverted intel_pstate no_turbo, or each policy's boost. ok is false when
// the node has no boost file.
func cpuBoostWrites(f CPUFacts, policies []int, boost string) (writes []Write, native string, ok bool) {
	on := boost == joulie.CPUGovernorBoostOn
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		v := bit(on)
		return []Write{{RelPath: cpufreqDir + "/boost", Value: v}}, "boost=" + v, true
	case joulie.CPUBoostScopeNoTurbo:
		v := bit(!on)
		return []Write{{RelPath: "devices/system/cpu/intel_pstate/no_turbo", Value: v}}, "no_turbo=" + v, true
	case joulie.CPUBoostScopePerCPU:
		v := bit(on)
		for _, n := range policies {
			writes = append(writes, Write{RelPath: policyPath(n, "boost"), Value: v})
		}
		return writes, "boost=" + v, true
	}
	return nil, "", false
}

// PlanCPU plans the CPU preset (a pkg/api CPUGovernorPreset* value) and the
// boost word (CPUGovernorBoost*, or empty to leave boost alone) for every
// policy in f. An unknown preset or boost word is an error.
//
// The governor is written to every policy first, then the
// energy-performance preference where the driver's row sets one (never
// under performance), then boost. A boost word on a node without a boost
// file makes the whole plan unsupported.
func PlanCPU(f CPUFacts, preset, boost string) (Plan, error) {
	if !slices.Contains(joulie.CPUGovernorPresetValues, preset) {
		return Plan{}, fmt.Errorf("governor: unknown CPU preset %q", preset)
	}
	if boost != "" && !slices.Contains(joulie.CPUGovernorBoostValues, boost) {
		return Plan{}, fmt.Errorf("governor: unknown CPU boost %q", boost)
	}
	c := chooseCPU(f, preset)
	if c.result == joulie.GovernorResultUnsupported {
		return Plan{Result: joulie.GovernorResultUnsupported}, nil
	}
	policies := sortedPolicies(f.Policies)
	var boostWrites []Write
	var boostNative string
	if boost != "" {
		var ok bool
		if boostWrites, boostNative, ok = cpuBoostWrites(f, policies, boost); !ok {
			return Plan{Result: joulie.GovernorResultUnsupported}, nil
		}
	}
	p := Plan{Native: c.governor, Result: c.result}
	for _, n := range policies {
		p.Writes = append(p.Writes, Write{RelPath: policyPath(n, "scaling_governor"), Value: c.governor})
	}
	if c.epp != "" {
		for _, n := range policies {
			p.Writes = append(p.Writes, Write{RelPath: policyPath(n, "energy_performance_preference"), Value: c.epp})
		}
		p.Native += " epp=" + c.epp
	}
	if boostNative != "" {
		p.Writes = append(p.Writes, boostWrites...)
		p.Native += " " + boostNative
	}
	return p, nil
}

// ReverseCPU names the preset whose plan on f gives the native governor and
// energy-performance preference epp (empty without an EPP file). ok is false
// when no preset or more than one does. A preset whose plan writes no
// preference matches any epp: under performance the kernel sets its own.
func ReverseCPU(f CPUFacts, governor, epp string) (preset string, ok bool) {
	n := 0
	for _, p := range joulie.CPUGovernorPresetValues {
		c := chooseCPU(f, p)
		if c.result == joulie.GovernorResultUnsupported || c.governor != governor {
			continue
		}
		if c.epp != "" && c.epp != epp {
			continue
		}
		preset = p
		n++
	}
	if n != 1 {
		return "", false
	}
	return preset, true
}

// PresetsCPU lists the CPU presets f can express, applied or equivalent; an
// unsupported preset is left out.
func PresetsCPU(f CPUFacts) []string {
	var out []string
	for _, p := range joulie.CPUGovernorPresetValues {
		if chooseCPU(f, p).result != joulie.GovernorResultUnsupported {
			out = append(out, p)
		}
	}
	return out
}

// AMDPstateEPPPreferences is the preference set amd-pstate-epp accepts
// outside the performance governor: energy_perf_strings of amd-pstate.c.
var AMDPstateEPPPreferences = []string{"default", "performance", "balance_performance", "balance_power", "power"}

// EPPSupport is the energy-performance preferences the policies accept under
// some governor, given those energy_performance_available_preferences lists
// under the current governor gov. amd-pstate-epp lists only performance while
// the governor is performance and its full set under powersave
// (show_energy_performance_available_preferences), so the support a preset
// has would change with the governor Joulie writes; there the set is the
// driver's own. The agent's discovery and govsweep both read preferences
// through it, so they plan the same native state.
func EPPSupport(driver, gov string, listed []string) []string {
	if driver == "amd-pstate-epp" && gov == govPerformance && slices.Equal(listed, []string{govPerformance}) {
		return slices.Clone(AMDPstateEPPPreferences)
	}
	return listed
}
