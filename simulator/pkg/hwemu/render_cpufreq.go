package hwemu

import (
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderSetpolicyDrivers are the drivers without a target callback. For them
// scaling_available_governors is the fixed "performance powersave" with no
// trailing space (cpufreq.c:847-849); for the others it lists each governor
// followed by a space.
var renderSetpolicyDrivers = map[string]bool{"intel_pstate": true, "amd-pstate-epp": true}

// renderTransitionLatencyNS is cpuinfo_transition_latency per driver, in ns.
// amd-pstate, the passive and guided driver, falls back to
// AMD_PSTATE_TRANSITION_LATENCY without a CPPC value (amd_pstate_cpu_init,
// amd-pstate.c:54, 968-976, 1086). amd-pstate-epp never sets it
// (amd_pstate_epp_cpu_init, amd-pstate.c:1875-1972), so the zeroed policy
// (cpufreq.c:1248) reads 0 (cpufreq.c:699). acpi-cpufreq takes it from the
// firmware's _PSS table (acpi-cpufreq.c:831-842), so its value is assumed,
// and so is intel_pstate's.
var renderTransitionLatencyNS = map[string]int{
	"acpi-cpufreq":   10000,
	"amd-pstate":     20000,
	"amd-pstate-epp": 0,
	"intel_pstate":   0,
}

// renderEPPPreferences is energy_performance_available_preferences of
// amd-pstate-epp outside the performance policy: energy_perf_strings, each
// followed by a space (amd-pstate.c:121-129, 1392-1406). The custom and
// dynamic entries of later kernels are left out (assumed: kernel 6.16).
const renderEPPPreferences = "default performance balance_performance balance_power power \n"

// renderPolicy is one cpufreq policy and the CPUs it covers.
type renderPolicy struct {
	ID   int
	CPUs []int
}

// renderPolicies groups CPUs into policies. per-cpu gives one policy per CPU.
// per-core gives one per core, named after the core's lowest CPU (assumed),
// so with socket-major-smt-last numbering CPU k and its sibling k+S*C share
// policy k.
func renderPolicies(p *Profile, cpus []renderCPU) []renderPolicy {
	if p.CPUFreq.PolicyGrouping.V != "per-core" {
		out := make([]renderPolicy, 0, len(cpus))
		for _, c := range cpus {
			out = append(out, renderPolicy{ID: c.ID, CPUs: []int{c.ID}})
		}
		return out
	}
	byCore := map[[2]int][]int{}
	for _, c := range cpus {
		key := [2]int{c.Socket, c.Core}
		byCore[key] = append(byCore[key], c.ID)
	}
	out := make([]renderPolicy, 0, len(byCore))
	for _, ids := range byCore {
		sort.Ints(ids)
		out = append(out, renderPolicy{ID: ids[0], CPUs: ids})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// renderCPUList is affected_cpus and related_cpus: the CPUs separated by
// spaces, without a trailing one (cpufreq.c:865-881).
func renderCPUList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, " ") + "\n"
}

// renderCPUFreq writes devices/system/cpu/cpufreq/policyX with the driver's
// attribute set and a cpuN/cpufreq link to it from every CPU it covers
// (cpufreq.rst:202-210).
//
// Every driver has the cpufreq core attributes (cpufreq.c:944-970):
// scaling_driver, scaling_available_governors, cpuinfo_min_freq,
// cpuinfo_max_freq, cpuinfo_transition_latency, affected_cpus and
// related_cpus (R); scaling_governor, scaling_min_freq and scaling_max_freq
// (A); scaling_cur_freq (S); scaling_setspeed (R, <unsupported> without the
// userspace governor, cpufreq.c:919-925). A table driver adds
// scaling_available_frequencies, amd-pstate-epp its EPP pair, and a profile
// with boost the global cpufreq/boost (A). A minimal profile, from a capture
// that recorded the driver only, has scaling_driver alone.
func renderCPUFreq(b *renderBuilder, p *Profile, o RenderOptions) error {
	f := p.CPUFreq
	if f == nil {
		return nil
	}
	cpus := renderCPUs(p)
	for _, pol := range renderPolicies(p, cpus) {
		dir := renderSys(layout.PolicyDir(pol.ID))
		entries := []renderEntry{{name: "scaling_driver", content: f.Driver.V + "\n", owner: layout.OwnerRender, mode: 0o444}}
		if !f.Minimal {
			entries = append(entries, renderPolicyEntries(f, pol)...)
		}
		if err := b.writeEntries(dir, entries); err != nil {
			return err
		}
		for _, id := range pol.CPUs {
			link, target := layout.CPUFreqLink(id, pol.ID)
			if err := b.link(renderSys(link), target); err != nil {
				return err
			}
		}
	}
	if f.Boost.Src != "" && !f.Minimal {
		boost := renderSys(path.Join(path.Dir(layout.PolicyDir(0)), "boost"))
		if err := b.file(boost, renderUint(f.Boost.V), layout.OwnerAgent, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func renderPolicyEntries(f *CPUFreqSpec, pol renderPolicy) []renderEntry {
	govs := strings.Fields(f.AvailableGovernors.V)
	available := strings.Join(govs, " ") + "\n"
	if !renderSetpolicyDrivers[f.Driver.V] {
		available = strings.Join(govs, " ") + " \n"
	}
	// The CPU idles at the top under performance and at the bottom
	// otherwise until the first step moves it (cpufreq.rst:372-389).
	cur := f.CPUInfoMinKHz.V
	if f.Governor.V == "performance" {
		cur = f.CPUInfoMaxKHz.V
	}
	cpuList := renderCPUList(pol.CPUs)
	entries := []renderEntry{
		{name: "scaling_available_governors", content: available, owner: layout.OwnerRender, mode: 0o444},
		{name: "scaling_governor", content: f.Governor.V + "\n", owner: layout.OwnerAgent, mode: 0o644},
		{name: "cpuinfo_min_freq", content: renderUint(f.CPUInfoMinKHz.V), owner: layout.OwnerRender, mode: 0o444},
		{name: "cpuinfo_max_freq", content: renderUint(f.CPUInfoMaxKHz.V), owner: layout.OwnerRender, mode: 0o444},
		{name: "cpuinfo_transition_latency", content: renderUint(renderTransitionLatencyNS[f.Driver.V]), owner: layout.OwnerRender, mode: 0o444},
		{name: "scaling_min_freq", content: renderUint(f.CPUInfoMinKHz.V), owner: layout.OwnerAgent, mode: 0o644},
		{name: "scaling_max_freq", content: renderUint(f.CPUInfoMaxKHz.V), owner: layout.OwnerAgent, mode: 0o644},
		{name: "scaling_cur_freq", content: renderUint(cur), owner: layout.OwnerEmulator, mode: 0o444},
		{name: "scaling_setspeed", content: "<unsupported>\n", owner: layout.OwnerRender, mode: 0o644},
		{name: "affected_cpus", content: cpuList, owner: layout.OwnerRender, mode: 0o444},
		{name: "related_cpus", content: cpuList, owner: layout.OwnerRender, mode: 0o444},
	}
	if len(f.AvailableKHz.V) > 0 {
		// freq_table.c:253-255: each entry followed by a space.
		var sb strings.Builder
		for _, khz := range f.AvailableKHz.V {
			sb.WriteString(strconv.FormatInt(khz, 10) + " ")
		}
		entries = append(entries, renderEntry{name: "scaling_available_frequencies", content: sb.String() + "\n", owner: layout.OwnerRender, mode: 0o444})
	}
	if f.Driver.V == "amd-pstate-epp" {
		// The preference starts at balance_performance (assumed). Under
		// performance the driver programs EPP 0, which reads performance, and
		// offers nothing else (amd-pstate.c:1398-1400, 2006-2007).
		prefs, epp := renderEPPPreferences, "balance_performance\n"
		if f.Governor.V == "performance" {
			prefs, epp = "performance\n", "performance\n"
		}
		entries = append(entries,
			renderEntry{name: "energy_performance_preference", content: epp, owner: layout.OwnerAgent, mode: 0o644},
			renderEntry{name: "energy_performance_available_preferences", content: prefs, owner: layout.OwnerRender, mode: 0o444},
		)
	}
	return entries
}
