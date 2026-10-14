package hwemu

import (
	"math"
	"strconv"

	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// The cpufreq governors of the physics: each policy's governor run at the
// substep boundaries (phys.GovernorStep), what it reads, the governor state
// State reports and the idle cost of a governor that holds the top
// frequency. The governor files themselves are node_governor.go's.

// governorTick runs the governor of policy i, n.policies[i], for one step
// of periodS seconds (phys.GovernorStep), and targetKHz becomes the
// frequency it sets, with boost. The governor reads the busy share of the
// step that ended, measured at the frequency the policy ran at: the one
// scaling_cur_freq showed, ramp, boost and a binding cap included. Reading
// it one step late stands in for rate_limit_us and the sampling period
// (assumed). A profile without the governor facts keeps target
// (node_governor.go).
func (n *Node) governorTick(i int, pol *nodePolicy, periodS float64) {
	u := n.policyDemand(pol)
	if !n.cpuGov.on {
		pol.targetKHz, pol.govDemand, pol.govSeeded = pol.target(u), u, true
		return
	}
	req, set := phys.GovernorStep(n.governorInput(i, pol, u, periodS), pol.govSetKHz)
	pol.govReqKHz, pol.govSetKHz, pol.govDemand, pol.govSeeded = req, set, u, true
	pol.targetKHz = float64(pol.boostedKHz(set))
}

// governorInput is what the governor of policy i sees at a step of periodS
// seconds with demand u. Util is that demand with work counted in cycles:
// Load.CPUUtil is the share the work keeps a CPU busy at the frequency
// performance runs it at under the present limits (ceilingKHz), so at a
// lower frequency the same work is a higher share, up to 1. Before the
// policy ran, Util is the demand itself. CurKHz is the frequency the policy
// ran at for schedutil, the setting of the userspace governor, and the
// governor's own request otherwise.
//
// powersave on intel_pstate or amd-pstate-epp is the driver's own
// algorithm: with an energy_performance_preference it is the HWP shape of
// phys, at the preference's EPP; without one, as on intel_pstate without
// HWP, it is taken as 1.25 x the maximum frequency x the busy share, the
// shape of intel_pstate's get_target_pstate (assumed, not checked against
// a kernel tree), which is schedutil read at MaxKHz.
func (n *Node) governorInput(i int, pol *nodePolicy, u, periodS float64) phys.GovernorInput {
	ph := n.profile.Physics.CPU
	in := phys.GovernorInput{
		Governor:       pol.governor,
		MinKHz:         pol.effMinKHz(),
		MaxKHz:         pol.effMaxKHz(),
		TableKHz:       pol.table,
		CurKHz:         pol.govReqKHz,
		PeriodS:        periodS,
		EPPGain:        phys.DefaultEPPGain,
		EPPCeilingDrop: phys.DefaultEPPCeilingDrop,
	}
	if ph.EPPGain != nil {
		in.EPPGain = ph.EPPGain.V
	}
	if ph.EPPCeilingDrop != nil {
		in.EPPCeilingDrop = ph.EPPCeilingDrop.V
	}
	in.Util = u
	if ran := pol.shownKHz; ran > 0 {
		in.Util = math.Min(1, u*float64(pol.ceilingKHz())/ran)
	}
	switch pol.governor {
	case "ondemand", "conservative":
		in.Tunables = n.governorTunables()
	case "schedutil":
		in.CurKHz = int64(math.Round(pol.shownKHz))
	case "userspace":
		if gp := n.govPolicy(i, pol); gp != nil && gp.setspeedKHz > 0 {
			in.CurKHz = gp.setspeedKHz
		}
	case "powersave":
		if !pol.setpolicy {
			break
		}
		if epp, ok := policyEPP(pol.epp); ok {
			in.HWP, in.EPP = true, epp
		} else {
			in.Governor, in.CurKHz = "schedutil", 0
		}
	}
	return in
}

// policyDemand is the mean Load.CPUUtil over the policy's CPUs.
func (n *Node) policyDemand(pol *nodePolicy) float64 {
	u := 0.0
	for _, c := range pol.cpus {
		if c >= 0 && c < len(n.cpuUtil) {
			u += n.cpuUtil[c]
		}
	}
	if len(pol.cpus) > 0 {
		u /= float64(len(pol.cpus))
	}
	return u
}

// policyEPP is the EPP of an energy_performance_preference word: its value
// in epp_values, or the raw number intel_pstate also takes. ok is false
// without a preference. A word the table lacks, such as default, counts as
// balance_performance, the boot value the profiles assume.
func policyEPP(word string) (int, bool) {
	if word == "" {
		return 0, false
	}
	if v, ok := cpuGovEPPValues[word]; ok {
		return v, true
	}
	if v, err := strconv.Atoi(word); err == nil && v >= 0 && v <= 255 {
		return v, true
	}
	return cpuGovEPPValues["balance_performance"], true
}

// govPolicy is the governor state node_governor.go keeps for policy i, nil
// on a profile without governor facts.
func (n *Node) govPolicy(i int, pol *nodePolicy) *nodeGovPolicy {
	if g := n.cpuGov.pols; i < len(g) && g[i].p == pol {
		return g[i]
	}
	return nil
}

// governorTunables are the tunables of ondemand and conservative in effect,
// as the tunables files hold them; a zero stands for the kernel default.
// schedutil's rate_limit_us is left to the one-step lag of governorTick.
func (n *Node) governorTunables() phys.GovernorTunables {
	t := n.cpuGov.tunables
	get := func(gov, name string) int64 { return t[cpuGovTunableKey(gov, name)] }
	return phys.GovernorTunables{
		OndemandUpThreshold:       get("ondemand", "up_threshold"),
		OndemandSamplingRateUS:    get("ondemand", "sampling_rate"),
		ConservativeFreqStep:      get("conservative", "freq_step"),
		ConservativeUpThreshold:   get("conservative", "up_threshold"),
		ConservativeDownThreshold: get("conservative", "down_threshold"),
	}
}

// deriveGovernorPresets sets each package's govPreset: the CPU preset whose
// plan gives the governor and preference its policies run, through
// governor.ReverseCPU, and empty when the policies disagree (govNative
// empty) or no single preset maps, as for a hand-written ondemand.
func (n *Node) deriveGovernorPresets() {
	for _, pk := range n.pkgs {
		if pk.govNative == pk.govPresetOf {
			continue
		}
		pk.govPresetOf, pk.govPreset = pk.govNative, ""
		if pk.govNative == "" {
			continue
		}
		for _, pol := range n.policies {
			if pol.socket != pk.socket {
				continue
			}
			if p, ok := governor.ReverseCPU(n.cpuFacts(), pol.governor, pol.epp); ok {
				pk.govPreset = p
			}
			break
		}
	}
}

// cpuFacts are the node's cpufreq facts as the agent's discovery reads
// them (pkg/governor.CPUFacts), from the profile and the policies.
func (n *Node) cpuFacts() governor.CPUFacts {
	cf := n.profile.CPUFreq
	if cf == nil {
		return governor.CPUFacts{}
	}
	f := governor.CPUFacts{Driver: cf.Driver.V, DriverMode: cf.DriverMode.V, HWP: cf.HWP.V}
	for _, pol := range n.policies {
		if f.Governors == nil {
			f.Governors = pol.governors
		}
		f.Policies = append(f.Policies, pol.index)
	}
	if g := &n.cpuGov; g.on {
		f.BoostScope = g.scope
		for _, gp := range g.pols {
			if gp.hasEPP {
				f.EPPs = g.eppWords
				break
			}
		}
	}
	return f
}

// idleGovernorW is physics.cpu.idleGovernorDeltaW while every policy of pk
// runs at the highest frequency it may (targetKHz at ceilingKHz), as
// performance holds it, and 0 otherwise, on a package without cpufreq or on
// a profile without the governor facts.
// assumed: a governor holding the top frequency keeps the idle cores out of
// their deepest states; the default 0 changes no profile.
func (n *Node) idleGovernorW(pk *nodePackage) float64 {
	d := n.profile.Physics.CPU.IdleGovernorDeltaW.V
	if d == 0 || !n.cpuGov.on {
		return 0
	}
	k := 0
	for _, pol := range n.policies {
		if pol.socket != pk.socket {
			continue
		}
		if pol.maxKHz <= 0 || pol.targetKHz < float64(pol.ceilingKHz()) {
			return 0
		}
		k++
	}
	if k == 0 {
		return 0
	}
	return d
}
