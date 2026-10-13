package hwemu

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// Zone roles. A package zone in any control type counts its package's
// energy, so an intel-rapl-mmio package-0 mirrors package 0, and its limits
// apply together with the MSR ones, the lowest winning (assumed). A die zone
// carries an equal share of its package (assumed), and its core and DRAM
// children the same share of their socket's core and DRAM; a psys zone
// carries the packages, DRAM and the rest of the platform (assumed); an
// unknown zone, such as uncore, draws nothing. Limits of psys, DRAM and core
// zones are read back but not enforced (assumed: platform and DRAM limiting is
// not modelled).
const (
	nodeRolePackage = iota
	nodeRoleDie
	nodeRolePsys
	nodeRoleDRAM
	nodeRoleCore
	nodeRoleOther
)

// Constraint kinds, by the names intel_rapl_common.c gives them (60-62).
const (
	nodePL1 = iota // long_term: settles with its time window
	nodePL2        // short_term: an instantaneous ceiling
	nodePL4        // peak_power: an instantaneous ceiling
	nodePLOther
)

// nodeChannel is one power draw and its energy: J joules up to the start of
// the current segment, and W watts since. Power only changes at substep
// boundaries and when an input changes, so energy is exact between them.
type nodeChannel struct {
	W float64
	J float64
}

// nodeConstraint is constraint_N of a powercap zone.
type nodeConstraint struct {
	kind         int
	limitUW      int64 // the readback: quantized, not clamped
	maxUW        int64
	windowUS     int64
	rejectWrites string
	settledW     float64 // PL1 only: the cap in effect, settling toward targetW
}

// enforcedUW is the limit the hardware applies: the written one, clamped to
// max_power_uw when the zone has one (assumed: hardware clamp).
func (c *nodeConstraint) enforcedUW() int64 {
	if c.maxUW > 0 && c.limitUW > c.maxUW {
		return c.maxUW
	}
	return c.limitUW
}

func (c *nodeConstraint) targetW() float64 { return float64(c.enforcedUW()) / 1e6 }

// nodeZone is one powercap zone of the tree. rel is its directory relative
// to sys/, and component the hw.id its files belong to (stateZoneComponent).
type nodeZone struct {
	rel         string
	component   string
	role        int
	socket      int
	dies        int       // die zones of this zone's package in its control type
	parent      *nodeZone // nil for a top-level zone
	enabled     int
	powerUnitUW int64
	counter     nodeCounter
	energy      *nodeSFile
	seedUJ      int64
	constraints []*nodeConstraint
}

// enforces reports whether c limits the zone: the zone is enabled (the
// sysfs-class-powercap ABI: enabled applies to the zone and its children) and
// the constraint is not one firmware disabled.
func (z *nodeZone) enforces(c *nodeConstraint) bool {
	return z.enabled == 1 && c.rejectWrites != "disabled"
}

// nodePolicy is one cpufreq policy.
type nodePolicy struct {
	index     int
	socket    int
	cpus      []int
	minKHz    int64 // cpuinfo_min_freq
	maxKHz    int64 // cpuinfo_max_freq
	table     []int64
	setpolicy bool // intel_pstate or amd-pstate-epp: powersave follows load
	governors []string
	governor  string
	// boost is the boost state the policy runs with: the global
	// cpufreq/boost, shared by every policy, or, where the driver has
	// set_boost, the policy's own boost file (buildCPUGovernor).
	boost     *int
	epp       string
	reqMinKHz int64 // as written
	reqMaxKHz int64
	curKHz    float64 // the running frequency, ramping toward targetKHz
	targetKHz float64 // the governor's frequency with boost (boostedKHz)
	shownKHz  float64 // curKHz after a package cap, as scaling_cur_freq shows it
	cur       *nodeSFile
	curSeeded bool

	// The governor between substeps (phys.GovernorStep): govReqKHz is the
	// request it keeps, the CurKHz of its next step, and govSetKHz the
	// frequency it set, a table entry on a table driver, before boost.
	// govDemand is the demand it last read, and govSeeded false until it
	// first ran.
	govReqKHz, govSetKHz int64
	govDemand            float64
	govSeeded            bool
}

func (p *nodePolicy) clampKHz(v int64) int64 {
	if p.maxKHz > 0 && v > p.maxKHz {
		v = p.maxKHz
	}
	if v < p.minKHz {
		v = p.minKHz
	}
	return v
}

// resolveKHz maps v to a table entry: the highest at or below v when high,
// else the lowest at or above v, or the nearest end of the table when none
// qualifies (cpufreq.c:474-490). Without a table v stands.
func (p *nodePolicy) resolveKHz(v int64, high bool) int64 {
	if len(p.table) == 0 {
		return v
	}
	best, found := int64(0), false
	for _, f := range p.table {
		if (high && f <= v || !high && f >= v) && (!found || high && f > best || !high && f < best) {
			best, found = f, true
		}
	}
	if found {
		return best
	}
	best = p.table[0]
	for _, f := range p.table {
		if high && f < best || !high && f > best {
			best = f
		}
	}
	return best
}

func (p *nodePolicy) effMaxKHz() int64 { return p.resolveKHz(p.clampKHz(p.reqMaxKHz), true) }

// ceilingKHz is the highest frequency the policy runs at. A table driver with
// boost on runs above its top entry, up to cpuinfo_max_freq, while the policy
// allows that entry: the hardware boosts only from P0 (AMD core performance
// boost, acpi-cpufreq.c:79-120; the EPYC dumps list a 2.4 GHz top entry under
// a 3.7 GHz cpuinfo_max_freq). A lower scaling_max_freq leaves no boost.
func (p *nodePolicy) ceilingKHz() int64 {
	hi := p.effMaxKHz()
	if len(p.table) == 0 || p.boost == nil || *p.boost != 1 || hi >= p.maxKHz {
		return hi
	}
	top := p.table[0]
	for _, f := range p.table {
		top = max(top, f)
	}
	if hi >= top {
		return p.maxKHz
	}
	return hi
}

func (p *nodePolicy) effMinKHz() int64 {
	return min(p.resolveKHz(p.clampKHz(p.reqMinKHz), false), p.effMaxKHz())
}

// boostedKHz is the frequency the policy runs at when its governor sets
// set: a table driver with boost on runs above its top entry, up to
// cpuinfo_max_freq, when the governor sets the top entry and the policy
// allows it (ceilingKHz); any other frequency stands.
func (p *nodePolicy) boostedKHz(set int64) int64 {
	if hi, ceil := p.effMaxKHz(), p.ceilingKHz(); ceil > hi && set >= hi {
		return ceil
	}
	return set
}

// target is where a profile without governor facts holds the policy at
// util, as before the governors were modelled. performance sits at the
// maximum at any load; powersave on a driver with governors sits at the
// minimum (cpufreq.rst:372-389). The powersave of intel_pstate and
// amd-pstate-epp, schedutil, ondemand and any other governor scale linearly
// with load between the two (assumed).
func (p *nodePolicy) target(util float64) float64 {
	lo, hi := float64(p.effMinKHz()), float64(p.ceilingKHz())
	switch {
	case p.governor == "performance":
		return hi
	case p.governor == "powersave" && !p.setpolicy:
		return lo
	}
	return lo + (hi-lo)*math.Max(0, math.Min(1, util))
}

// effUtil is the busy share of demand u on pol. With the governor facts work
// is counted in cycles (decision D10): u is the share the work keeps the
// policy busy at the frequency performance runs it at under the present
// limits (ceilingKHz), so a governor holding a lower frequency sees the same
// work as a busier CPU, up to 1. Under performance it is u, and so it is on
// a profile without the governor facts. assumed: the governor's frequency
// (targetKHz), not the ramp toward it, sets the cycles, and a package cap
// does not raise them, so the cap solve stays as it was.
func (n *Node) effUtil(pol *nodePolicy, u float64) float64 {
	u = math.Max(0, math.Min(1, u))
	if !n.cpuGov.on || pol.targetKHz <= 0 {
		return u
	}
	return math.Min(1, u*float64(pol.ceilingKHz())/pol.targetKHz)
}

// nodeHSMP is the HSMP socket power cap of one package.
type nodeHSMP struct {
	capUW    int64
	capMaxUW int64
	settledW float64
	input    *nodeSFile
}

func (h *nodeHSMP) targetW() float64 {
	uw := h.capUW
	if h.capMaxUW > 0 && uw > h.capMaxUW {
		uw = h.capMaxUW
	}
	return float64(uw) / 1e6
}

// nodePackage is one CPU package.
type nodePackage struct {
	socket   int
	cpus     []int
	limits   []*nodeZone // package and die zones in every control type
	hsmp     *nodeHSMP
	minScale float64
	seedUJ   int64
	pwr      nodeChannel
	dram     nodeChannel
	next     float64

	util       float64
	natScale   float64
	effScale   float64
	capped     bool
	capW       float64
	requestedW float64

	// govNative is the governor of the package's policies in the driver's
	// own words, and govPreset the preset it maps to; empty when the
	// policies disagree or no single preset maps. govPresetOf is the
	// govNative govPreset was derived from.
	govNative, govPreset string
	govPresetOf          string
}

// nodeGPU is one GPU. limitRaw is the limit file's value: mW in NVML state, uW
// in amdgpu hwmon. defaultMW is the NVML default limit, where persistReset
// puts the limit back while persistence, the persistence_mode word, is
// Disabled. uuid and pciBusID are the GPU's identity as its tools print it,
// and sysfsPath its PCI device relative to sys/, when the tree has one.
type nodeGPU struct {
	index    int
	nvml     bool
	limitRaw int64
	minW     float64
	maxW     float64
	settledW float64
	avgW     float64
	avgSeed  bool
	seedMJ   int64
	pwr      nodeChannel
	next     float64

	defaultMW   int64
	persistence string
	limitFile   *nodeAFile
	uuid        string
	pciBusID    string
	sysfsPath   string

	// NVML only: the energy of each substep of the last telemetry window, a
	// ring starting at winAt, and the channel energy at the last boundary.
	win   []float64
	winAt int
	winJ  float64

	input, average                   *nodeSFile // amdgpu hwmon
	draw, enforced, energy, utilFile *nodeSFile // NVML state

	// clock is the governor and clock state the physics reads
	// (clockstate.go); dpm and nvclock the vendor state behind it
	// (node_dpm.go, node_nvclock.go).
	clock   gpuClockState
	dpm     nodeGPUDPM
	nvclock nodeGPUNVClock

	// rx and tx are the peer transfer bytes the GPU receives and sends: W
	// the rate in bytes per second, J the bytes up to the start of the
	// current segment, integrated as energy is. nextRx and nextTx wait for
	// adopt as next does.
	rx, tx         nodeChannel
	nextRx, nextTx float64
}

// id is the GPU's hw.id, gpu<index>.
func (g *nodeGPU) id() string { return stateID(semconv.HwTypeGPU, g.index) }

// limitW is the limit as set, in W.
func (g *nodeGPU) limitW() float64 {
	if g.nvml {
		return float64(g.limitRaw) / 1000
	}
	return float64(g.limitRaw) / 1e6
}

// targetMW is the NVML limit the GPU enforces: the written one within
// [min, max].
func (g *nodeGPU) targetMW() int64 {
	lo, hi := int64(math.Round(g.minW*1000)), int64(math.Round(g.maxW*1000))
	v := g.limitRaw
	if hi > 0 && v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}

func (g *nodeGPU) targetW() float64 {
	if g.nvml {
		return float64(g.targetMW()) / 1000
	}
	return g.limitW()
}

// nodeMinGPUCapW stands in for a zero cap, which CappedBoardGPUModel would
// read as no cap at all (phys.CappedBoardGPUModel.Power).
const nodeMinGPUCapW = 1e-3

// nodeIdleUtil is the utilization below which a package counts as idle for
// physics.cpu.idleGovernorDeltaW, the same threshold as the GPU clock
// model's (assumed).
const nodeIdleUtil = 0.05

// derive recomputes everything that follows from the state at the last
// substep boundary and the current inputs: natural and capped package power,
// the frequency scaling_cur_freq shows, the governor state State reports,
// GPU clocks and power, and the peer transfer rates. The new powers and
// rates wait in next until adopt or a substep takes them. Governors run at
// substep boundaries (governorTick); derive runs one for a single sampling
// period only when it never ran or its demand changed since, so a new load
// moves the target at once, as it did before the governors were modelled,
// and the frequency still ramps toward it.
func (n *Node) derive() {
	for i, pol := range n.policies {
		if !pol.govSeeded || n.policyDemand(pol) != pol.govDemand {
			n.governorTick(i, pol, 0)
		}
		if !pol.curSeeded {
			pol.curKHz, pol.curSeeded = pol.targetKHz, true
		}
	}
	for _, pk := range n.pkgs {
		n.derivePackage(pk)
	}
	for _, pol := range n.policies {
		pol.shownKHz = pol.curKHz
		if pk := n.pkgs[pol.socket]; pk.capped && pol.maxKHz > 0 {
			if f := pk.effScale * float64(pol.maxKHz); f < pol.shownKHz {
				pol.shownKHz = math.Max(f, float64(pol.minKHz))
			}
		}
	}
	n.deriveCPUGovernor()
	n.deriveGovernorPresets()
	n.deriveGPUDPM()
	n.deriveNVClock()
	for _, g := range n.gpus {
		g.next = n.gpuPowerW(g, n.gpuDeviceState(g))
		if !g.avgSeed {
			g.avgW, g.avgSeed = g.next, true
		}
	}
	n.derivePeers()
}

// gpuDeviceState is what the GPU model reads for g: its utilization, the
// enforced limit and the load's class and memory intensity.
func (n *Node) gpuDeviceState(g *nodeGPU) phys.DeviceState {
	return phys.DeviceState{
		Utilization:     n.gpuUtil(g.index),
		CapWatts:        math.Max(g.settledW, nodeMinGPUCapW),
		MemoryIntensity: n.load.MemoryIntensity,
		Class:           n.load.GPUClass,
	}
}

// governorTick runs the governor of policy i, n.policies[i], for one step
// of periodS seconds (phys.GovernorStep), and targetKHz becomes the
// frequency it sets, with boost. The governor reads the busy share of the
// step that ended, measured at the frequency the policy ran at: the one
// scaling_cur_freq showed, ramp, boost and a binding cap included. Reading
// it one step late stands in for rate_limit_us and the sampling period
// (assumed). A profile without the governor facts keeps target, as it did
// before them (node_governor.go).
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

// gpuPhysClock is g's clock state as the phys clock model takes it: the
// governor's compute ceiling (none when it is at or above the highest clock
// the GPU sustains), the data-path level and the gating flag. own asks for
// the view of the GPU's own work: a data path that follows demand rises for
// the GPU's own kernels, so its own work always meets it at its maximum;
// only inbound peer transfers, which the governor does not see, meet the
// level it holds. A GPU without a clock model gives the zero GPUClock,
// which the phys functions read as no model.
func (n *Node) gpuPhysClock(g *nodeGPU, own bool) phys.GPUClock {
	c := &g.clock
	if !c.modelled || c.computeMaxMHz <= 0 {
		return phys.GPUClock{}
	}
	top := c.computeMaxMHz
	if v := g.nvclock.spec; v != nil {
		if s := float64(v.SM.SustainedMaxMHz.V); s > 0 && s < top {
			top = s
		}
	}
	out := phys.GPUClock{ComputeMaxMHz: top, DataPath: c.dataPath, DataPathAuto: c.dataPathAuto, GatingOff: c.gatingOff}
	if ceil := c.computeCeilMHz; ceil > 0 && ceil < top {
		out.ComputeMHz = ceil
	}
	if own && c.dataPathAuto {
		out.DataPath = phys.DataPathMax
	}
	return out
}

// gpuClockFacts are the profile's physics.gpu clock parameters.
func (n *Node) gpuClockFacts() phys.GPUClockFacts {
	g := &n.profile.Physics.GPU
	return phys.GPUClockFacts{
		ClockBeta:       g.ClockBeta.V,
		MemoryKneeFrac:  g.MemoryKneeFrac.V,
		MemoryKneeGamma: g.MemoryKneeGamma.V,
		DataPathHoldW:   g.DataPathHoldW.V,
		GatingOffIdleW:  g.GatingOffIdleW.V,
		DataPathGamma:   g.DataPathGamma.V,
	}
}

// gpuPowerW is g's board power running s: the GPU model's, through the
// clock model when g has one (phys.GPUPowerWithClock), which at the maximum
// compute clock, the data path at its maximum and idle gating kept is the
// same.
func (n *Node) gpuPowerW(g *nodeGPU, s phys.DeviceState) float64 {
	m, ok := n.gpuModel.(phys.CappedBoardGPUModel)
	clk := n.gpuPhysClock(g, false)
	if !ok || clk.ComputeMaxMHz <= 0 {
		return n.gpuModel.Power(s)
	}
	return phys.GPUPowerWithClock(m, s, clk, n.gpuClockFacts())
}

// gpuByIndex is the GPU with index i, nil when the node has none.
func (n *Node) gpuByIndex(i int) *nodeGPU {
	for _, g := range n.gpus {
		if g.index == i {
			return g
		}
	}
	return nil
}

// nodeReceiverGate is the receiver gate a node's peer transfers go through.
// physics_gate.go sets it from the profile and Options.ReceiverGate;
// without that file no gate is on.
var nodeReceiverGate = func(*Node) phys.ReceiverGate { return phys.ReceiverGate{} }

// isPeer reports whether t is a peer transfer: a copy between two distinct
// GPUs of the node, over the peer link.
func (n *Node) isPeer(t PeerTransfer) bool {
	return t.Src != t.Dst && n.gpuByIndex(t.Src) != nil && n.gpuByIndex(t.Dst) != nil
}

// peerRates is the rate, in bytes per second, each transfer of
// Load.PeerTransfers gets, in order. A peer transfer alone gets phys.PeerRate
// over gpus.transfer.peerLinkGBps (GB/s, 1e9 bytes) into a receiver at its
// clock state, through the receiver gate: at most link x g, the fraction g
// the receiver's data path lets through from that source. The copies into
// one GPU share its link by time: each needs its demand over link x g of
// the link's time, and when together they need more than all of it, each
// gets its demand over that total, so a gated receiver takes in at most
// link x g whatever the number of sources. Any other transfer, with a host
// or NIC end (a negative Src or Dst, phys.HostSource) or naming no two GPUs
// of the node, is not on the peer link and gets its demand, as does every
// transfer on a profile without gpus.transfer, which does not model the
// link.
func (n *Node) peerRates() []float64 {
	ts := n.load.PeerTransfers
	if len(ts) == 0 {
		return nil
	}
	out := make([]float64, len(ts))
	var link float64
	if gs := n.profile.GPUs; gs != nil && gs.Transfer != nil {
		link = gs.Transfer.PeerLinkGBps.V * 1e9
	}
	gate := nodeReceiverGate(n)
	busy := map[int]float64{} // the share of each receiver's link time the copies into it need
	for i, t := range ts {
		if !n.isPeer(t) || link <= 0 {
			out[i] = phys.PeerRate(t.BytesPerS, 0, phys.GPUClock{}, phys.ReceiverGate{}, phys.HostSource, phys.HostSource)
			continue
		}
		clk := n.gpuPhysClock(n.gpuByIndex(t.Dst), false)
		out[i] = phys.PeerRate(t.BytesPerS, link, clk, gate, t.Src, t.Dst)
		if pair := phys.PeerRate(math.Inf(1), link, clk, gate, t.Src, t.Dst); pair > 0 && t.BytesPerS > 0 {
			busy[t.Dst] += t.BytesPerS / pair
		}
	}
	for i, t := range ts {
		if b := busy[t.Dst]; b > 1 && link > 0 && t.BytesPerS > 0 && n.isPeer(t) {
			out[i] = t.BytesPerS / b
		}
	}
	return out
}

// derivePeers sets each GPU's next peer receive and send rates from
// peerRates. Only peer transfers count, and only on a profile with
// gpus.transfer: State's PeerRxBytes and PeerTxBytes count the peer link.
func (n *Node) derivePeers() {
	for _, g := range n.gpus {
		g.nextRx, g.nextTx = 0, 0
	}
	if gs := n.profile.GPUs; gs == nil || gs.Transfer == nil {
		return
	}
	for i, r := range n.peerRates() {
		if t := n.load.PeerTransfers[i]; n.isPeer(t) {
			n.gpuByIndex(t.Dst).nextRx += r
			n.gpuByIndex(t.Src).nextTx += r
		}
	}
}

// derivePackage is steps 3 and 4 for one package. Natural power comes from the
// CPU model at the package's mean frequency scale and mean busy share, work
// counted in cycles (nodePolicy.effUtil), plus idleGovernorW while the
// package idles. Above the cap the scale is
// solved for the cap, and power is max(min(natural, cap), floorW, power at
// minScale): a cap below what the lowest frequency reaches shows as power
// above the limit, never as a changed readback. Power never exceeds
// natural, so floorW only binds a capped package.
func (n *Node) derivePackage(pk *nodePackage) {
	pk.util = 0
	pk.natScale = 0
	for _, c := range pk.cpus {
		u, scale := n.cpuUtil[c], 1.0
		if pol := n.cpuPolicy[c]; pol != nil && pol.maxKHz > 0 {
			u = n.effUtil(pol, u)
			scale = pol.curKHz / float64(pol.maxKHz)
		}
		pk.util += u
		pk.natScale += scale
	}
	if len(pk.cpus) > 0 {
		pk.util /= float64(len(pk.cpus))
		pk.natScale /= float64(len(pk.cpus))
	} else {
		pk.natScale = 1
	}
	capW, requestedW := n.packageCap(pk)
	s := phys.DeviceState{
		Utilization:     pk.util,
		FreqScale:       pk.natScale,
		MemoryIntensity: n.load.MemoryIntensity,
		Class:           n.load.CPUClass,
	}
	natural := n.cpuModel.Power(s)
	if pk.util < nodeIdleUtil {
		natural += n.idleGovernorW(pk)
	}
	pk.next, pk.effScale, pk.capped = natural, pk.natScale, false
	if natural > capW {
		lowest := s
		lowest.FreqScale = pk.minScale
		floor := math.Max(n.floorW, n.cpuModel.Power(lowest))
		pk.next = math.Min(natural, math.Max(capW, floor))
		scale := phys.SolveFreqScaleForCap(n.cpuModel, s, capW, pk.minScale)
		pk.effScale = math.Max(pk.minScale, math.Min(scale, pk.natScale))
		pk.capped = true
	}
	pk.capW, pk.requestedW = 0, 0
	if !math.IsInf(capW, 1) {
		pk.capW = capW
	}
	if !math.IsInf(requestedW, 1) {
		pk.requestedW = requestedW
	}
}

// packageCap is the cap in effect on pk and the lowest PL1 or HSMP limit
// written for it, both in W and +Inf when there is none. PL1 counts with its
// settled value, PL2 and PL4 as written; a die zone's limit is per die, and
// with power split equally the package can draw dies times the lowest.
func (n *Node) packageCap(pk *nodePackage) (capW, requestedW float64) {
	capW, requestedW = math.Inf(1), math.Inf(1)
	for _, z := range pk.limits {
		mult := 1.0
		if z.role == nodeRoleDie && z.dies > 0 {
			mult = float64(z.dies)
		}
		for _, c := range z.constraints {
			switch c.kind {
			case nodePL1:
				requestedW = math.Min(requestedW, float64(c.limitUW)/1e6*mult)
				if z.enforces(c) {
					capW = math.Min(capW, c.settledW*mult)
				}
			case nodePL2, nodePL4:
				if z.enforces(c) {
					capW = math.Min(capW, c.targetW()*mult)
				}
			}
		}
	}
	if h := pk.hsmp; h != nil {
		requestedW = math.Min(requestedW, float64(h.capUW)/1e6)
		capW = math.Min(capW, h.settledW)
	}
	return capW, requestedW
}

// adopt makes the derived powers and peer rates current at t. When one
// changed, energy and bytes are first committed at t with the old ones, so a
// counter never runs backwards and a step that changes nothing leaves the
// integration untouched.
func (n *Node) adopt(t time.Time) {
	changed := false
	for _, pk := range n.pkgs {
		changed = changed || pk.next != pk.pwr.W
	}
	for _, g := range n.gpus {
		changed = changed || g.next != g.pwr.W || g.nextRx != g.rx.W || g.nextTx != g.tx.W
	}
	if changed {
		n.commit(t)
	}
	n.assign()
}

func (n *Node) assign() {
	for _, pk := range n.pkgs {
		pk.pwr.W = pk.next
	}
	for _, g := range n.gpus {
		g.pwr.W = g.next
		g.rx.W, g.tx.W = g.nextRx, g.nextTx
	}
}

// commit integrates every channel up to t and starts a new segment there.
func (n *Node) commit(t time.Time) {
	dt := t.Sub(n.segT).Seconds()
	if dt > 0 && !n.frozen {
		for _, pk := range n.pkgs {
			pk.pwr.J += pk.pwr.W * dt
			pk.dram.J += pk.dram.W * dt
		}
		n.other.J += n.other.W * dt
		for _, g := range n.gpus {
			g.pwr.J += g.pwr.W * dt
			g.rx.J += g.rx.W * dt
			g.tx.J += g.tx.W * dt
		}
	}
	n.segT = t
}

// substep advances the physics by one substep, to the boundary t: energy up
// to t at the powers of the substep, then caps, averages and frequencies
// settle (step 2 and 3), the governors read the substep that ended, and the
// powers of the next substep follow (step 4).
// Substep boundaries sit on a fixed grid from Options.Start, so the state at
// any time is the same however AdvanceTo cut the way there.
func (n *Node) substep(t time.Time) {
	h := t.Sub(n.grid).Seconds()
	n.commit(t)
	for _, z := range n.zones {
		for _, c := range z.constraints {
			if c.kind == nodePL1 {
				c.settledW = phys.FirstOrderToward(c.settledW, c.targetW(), h, n.raplTau(c))
			}
		}
	}
	for _, pk := range n.pkgs {
		if hs := pk.hsmp; hs != nil {
			hs.settledW = phys.FirstOrderToward(hs.settledW, hs.targetW(), h, n.hsmpTau)
		}
	}
	for _, g := range n.gpus {
		if g.win != nil {
			g.avgW = g.windowMean(h)
		} else {
			g.avgW = phys.FirstOrderToward(g.avgW, g.pwr.W, h, n.gpuWindow)
		}
		g.settledW = phys.FirstOrderToward(g.settledW, g.targetW(), h, n.gpuTau)
	}
	for _, pol := range n.policies {
		pol.curKHz = phys.FirstOrderToward(pol.curKHz, pol.targetKHz, h, n.rampTau)
	}
	for i, pol := range n.policies {
		n.governorTick(i, pol, h)
	}
	n.substepCPUGovernor(h)
	n.substepGPUDPM(h)
	n.substepNVClock(h)
	n.grid = t
	n.derive()
	n.assign()
}

// seedWindows fills the NVML window of every GPU as if it had drawn its
// average power for the whole window, so a Node started over a live tree
// keeps power_draw_mw where it was. The window is the last
// telemetryWindowMS in whole substeps.
func (n *Node) seedWindows() {
	h := n.substepD.Seconds()
	slots := max(1, int(math.Round(n.gpuWindow/h)))
	for _, g := range n.gpus {
		if !g.nvml {
			continue
		}
		g.win = make([]float64, slots)
		for i := range g.win {
			g.win[i] = g.avgW * h
		}
	}
}

// windowMean records the energy of the substep of length h that just ended
// and returns the mean power over the window: NVML reports power averaged
// over the last second (NVML device queries documentation), a boxcar rather
// than a first-order lag, so a step in power shows in full one window later.
// amdgpu power1_average keeps the first-order average.
func (g *nodeGPU) windowMean(h float64) float64 {
	g.win[g.winAt] = g.pwr.J - g.winJ
	g.winJ = g.pwr.J
	g.winAt = (g.winAt + 1) % len(g.win)
	sum := 0.0
	for _, e := range g.win {
		sum += e
	}
	return sum / (float64(len(g.win)) * h)
}

// raplTau is the PL1 settling time: the constraint's own time window
// (powercap.rst), unless physics.rapl.settleTau fixes one.
func (n *Node) raplTau(c *nodeConstraint) float64 {
	if n.raplFixedTau >= 0 {
		return n.raplFixedTau
	}
	return float64(c.windowUS) / 1e6
}

// share is the part of its socket a zone's parent covers: 1/dies under a die
// zone, whose package power is split equally (assumed), and 1 otherwise. A core
// or DRAM child of a die then counts that die's share, so a core never counts
// more than its die and the DRAM of a socket is counted once over its dies.
func (z *nodeZone) share() float64 {
	for p := z.parent; p != nil; p = p.parent {
		if p.role == nodeRoleDie && p.dies > 0 {
			return 1 / float64(p.dies)
		}
	}
	return 1
}

// zoneJ is the energy a zone counted since the Node started, from the channel
// energies at the time being written.
func (n *Node) zoneJ(z *nodeZone, pkgJ, dramJ []float64, otherJ float64) float64 {
	inRange := z.socket >= 0 && z.socket < len(pkgJ)
	switch z.role {
	case nodeRolePackage:
		if inRange {
			return pkgJ[z.socket]
		}
	case nodeRoleDie:
		if inRange && z.dies > 0 {
			return pkgJ[z.socket] / float64(z.dies)
		}
	case nodeRoleCore:
		if inRange {
			return z.share() * n.coreFraction * pkgJ[z.socket]
		}
	case nodeRoleDRAM:
		if inRange {
			return z.share() * dramJ[z.socket]
		}
	case nodeRolePsys:
		sum := otherJ
		for i := range pkgJ {
			sum += pkgJ[i] + dramJ[i]
		}
		return sum
	}
	return 0
}

// writeOutputs is step 7: every S file whose content changed is replaced.
func (n *Node) writeOutputs() error {
	dt := n.now.Sub(n.segT).Seconds()
	pkgJ := make([]float64, len(n.pkgs))
	dramJ := make([]float64, len(n.pkgs))
	for i, pk := range n.pkgs {
		pkgJ[i] = pk.pwr.J + pk.pwr.W*dt
		dramJ[i] = pk.dram.J + pk.dram.W*dt
	}
	otherJ := n.other.J + n.other.W*dt

	var errs []error
	put := func(f *nodeSFile, v int64) {
		if err := n.writeS(f, nodeItoa(v)+"\n"); err != nil {
			errs = append(errs, fmt.Errorf("hwemu: write %s: %w", f.rel, err))
		}
	}
	for _, z := range n.zones {
		if z.energy != nil {
			put(z.energy, z.counter.energyUJ(n.zoneJ(z, pkgJ, dramJ, otherJ)))
		}
	}
	for _, pol := range n.policies {
		if pol.cur != nil {
			put(pol.cur, int64(math.Round(pol.shownKHz)))
		}
	}
	for _, pk := range n.pkgs {
		if pk.hsmp != nil && pk.hsmp.input != nil {
			// Package power truncated to mW, times 1000 (hsmp/hwmon.c:74).
			put(pk.hsmp.input, nodeMicroWatts(pk.pwr.W))
		}
	}
	for _, g := range n.gpus {
		if g.input != nil {
			put(g.input, nodeMicroWatts(g.pwr.W))
		}
		if g.average != nil {
			put(g.average, nodeMicroWatts(g.avgW))
		}
		if g.draw != nil {
			put(g.draw, int64(math.Floor(g.avgW*1000)))
		}
		if g.enforced != nil {
			put(g.enforced, int64(math.Round(g.settledW*1000)))
		}
		if g.energy != nil {
			put(g.energy, nodeMilliJoules(g.seedMJ, g.pwr.J+g.pwr.W*dt))
		}
		if g.utilFile != nil {
			put(g.utilFile, int64(math.Round(100*n.gpuUtil(g.index))))
		}
	}
	errs = append(errs, n.writeCPUGovernor(), n.writeGPUDPM(), n.writeNVClock())
	n.settlePeerBytes(dt)
	return errors.Join(errs...)
}

// settlePeerBytes brings the peer byte counts State reports up to the
// node's time, dt seconds into the current segment.
func (n *Node) settlePeerBytes(dt float64) {
	for _, g := range n.gpus {
		g.clock.peerRxBytes = g.rx.J + g.rx.W*dt
		g.clock.peerTxBytes = g.tx.J + g.tx.W*dt
	}
}

// nodeMicroWatts is a hwmon power reading: whole mW, times 1000
// (hsmp/hwmon.c:74; amdgpu_pm.c:3363-3364).
func nodeMicroWatts(w float64) int64 {
	return int64(math.Floor(math.Max(0, w)*1000)) * 1000
}
