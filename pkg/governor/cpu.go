package governor

import (
	"fmt"
	"slices"

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

// sortedPolicies returns the policy numbers sorted, without duplicates.
func sortedPolicies(ps []int) []int {
	out := slices.Clone(ps)
	slices.Sort(out)
	return slices.Compact(out)
}

// boostBit is the content of a boost file for a boost word: 1 for on, the
// inverse in an inverted file (intel_pstate's no_turbo).
func boostBit(boost string, inverted bool) string {
	if (boost == joulie.CPUGovernorBoostOn) != inverted {
		return "1"
	}
	return "0"
}

// CPUBoostWrites plans the boost word (a pkg/api CPUGovernorBoost* value) on
// f: the global boost file, the inverted intel_pstate no_turbo, or the boost
// of each of policies. It is nil when the node has no boost file.
func CPUBoostWrites(f CPUFacts, policies []int, boost string) []Write {
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		return []Write{{RelPath: GlobalBoostRelPath, Value: boostBit(boost, false)}}
	case joulie.CPUBoostScopeNoTurbo:
		return []Write{{RelPath: NoTurboRelPath, Value: boostBit(boost, true)}}
	case joulie.CPUBoostScopePerCPU:
		var writes []Write
		for _, n := range policies {
			writes = append(writes, Write{RelPath: PolicyRelPath(n, PolicyBoostFile), Value: boostBit(boost, false)})
		}
		return writes
	}
	return nil
}

// boostNative spells the boost word on a node with scope: boost=<0|1>, or
// no_turbo=<0|1> for the inverted file.
func boostNative(scope, boost string) string {
	if scope == joulie.CPUBoostScopeNoTurbo {
		return "no_turbo=" + boostBit(boost, true)
	}
	return "boost=" + boostBit(boost, false)
}

// NativeCPU spells a cpufreq governor and energy-performance preference (empty
// for none) as Plan.Native does.
func NativeCPU(governor, epp string) string {
	if epp == "" {
		return governor
	}
	return governor + " epp=" + epp
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
	if boost != "" {
		if boostWrites = CPUBoostWrites(f, policies, boost); boostWrites == nil {
			return Plan{Result: joulie.GovernorResultUnsupported}, nil
		}
	}
	p := Plan{State: State{Governor: c.governor, EPP: c.epp, Boost: boost}, Native: NativeCPU(c.governor, c.epp), Result: c.result}
	for _, n := range policies {
		p.Writes = append(p.Writes, Write{RelPath: PolicyRelPath(n, PolicyGovernorFile), Value: c.governor})
	}
	if c.epp != "" {
		for _, n := range policies {
			p.Writes = append(p.Writes, Write{RelPath: PolicyRelPath(n, PolicyEPPFile), Value: c.epp})
		}
	}
	if boost != "" {
		p.Writes = append(p.Writes, boostWrites...)
		p.Native += " " + boostNative(f.BoostScope, boost)
	}
	return p, nil
}

// CPUGroup is a set of cpufreq policies that share one state: a governor,
// an energy-performance preference and a boost word, each empty to leave
// it alone.
type CPUGroup struct {
	Policies             []int
	Governor, EPP, Boost string
}

// PlanCPUGroups plans the writes that bring each group of policies on f to
// its state, in three phases the caller writes in order: every group's
// governor, then every preference (which a governor change can reset), then
// boost. Boost is per policy where the node sets it per policy; elsewhere
// one file holds it for the node, and the first group with a boost word sets
// it.
func PlanCPUGroups(f CPUFacts, groups []CPUGroup) (governors, epps, boost []Write) {
	for _, g := range groups {
		for _, n := range sortedPolicies(g.Policies) {
			if g.Governor != "" {
				governors = append(governors, Write{RelPath: PolicyRelPath(n, PolicyGovernorFile), Value: g.Governor})
			}
			if g.EPP != "" {
				epps = append(epps, Write{RelPath: PolicyRelPath(n, PolicyEPPFile), Value: g.EPP})
			}
		}
	}
	if f.BoostScope == joulie.CPUBoostScopePerCPU {
		for _, g := range groups {
			if g.Boost != "" {
				boost = append(boost, CPUBoostWrites(f, sortedPolicies(g.Policies), g.Boost)...)
			}
		}
		return governors, epps, boost
	}
	for _, g := range groups {
		if g.Boost != "" {
			return governors, epps, CPUBoostWrites(f, nil, g.Boost)
		}
	}
	return governors, epps, nil
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
