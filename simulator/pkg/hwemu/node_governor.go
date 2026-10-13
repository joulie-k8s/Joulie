package hwemu

import (
	"errors"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// CPU governor emulation, Node side. NewNode, derive, substep and
// writeOutputs call these hooks; they do nothing on a profile without the
// governor facts (cpuGovModeled), which runs as it did before them. The
// policies themselves (governor, EPP, boost, frequency bounds) are
// nodePolicy; nodePackage.govNative is the governor State reports for a
// package. Kernel citations are against Linux v6.12.
//
// What the files do, beyond what buildCPUFreq already handles:
//   - energy_performance_preference takes the words of epp.available, a raw
//     0 to 255 on intel_pstate, and "default" for the boot value; a
//     non-zero EPP under the performance policy of a setpolicy driver is
//     refused with EBUSY (intel_pstate.c:790-797, amd-pstate.c:299-302).
//     Entering performance shows EPP 0; leaving it, intel_pstate restores
//     the preference it saved (intel_pstate.c:1174-1198) and
//     amd-pstate-epp keeps 0 (amd-pstate.c:1550-1572).
//   - intel_pstate/no_turbo, and the per-policy boost of amd-pstate, lower
//     each policy's maximum to the top non-turbo frequency and keep the
//     scaling_max_freq request, so turning turbo back on restores it
//     (intel_pstate.c:1351-1361, amd-pstate.c:665-702 with
//     refresh_frequency_limits). cpuinfo_max_freq, which the kernel lowers
//     too, stays as rendered. A Node started over a tree with turbo off
//     cannot read the request the file hides under the cap, and takes the
//     kernel's default, no limit below cpuinfo_max_freq.
//   - where the driver has set_boost, the global cpufreq/boost and each
//     policy's boost are both there: a global write of a new state sets
//     every policy, and a policy's boost refuses any write with EINVAL while
//     the global one is off (cpufreq.c:595-650, 2781-2800).
//   - scaling_setspeed takes a frequency under userspace and shows it
//     clamped to the policy's limits, as the governor's target
//     (cpufreq.c:908-924; cpufreq_userspace.c:42-44, 88, 105-120).
//   - the tunables, max_perf_pct, min_perf_pct, hwp_dynamic_boost and the
//     uncore limits are stored and checked as the kernel checks them; they
//     move no reading. A TPMI root domain reads its clusters and writes all
//     of them (nodeUncoreRoot).

// nodeCPUGovernor is the node's CPU governor state that nodePolicy does not
// hold, such as intel_pstate/no_turbo, the governor tunables and the uncore
// domains.
type nodeCPUGovernor struct {
	on       bool   // the profile sets governor facts
	active   bool   // a setpolicy driver: intel_pstate or amd-pstate-epp
	intel    bool   // intel_pstate in either mode
	scope    string // cpuGovBoost*
	baseKHz  int64  // top non-turbo frequency, 0 when unknown
	turboKHz int64  // cpuinfo_max_freq as rendered

	// seeding is true while buildCPUGovernor seeds the model from the
	// tree, where a file shows state that already holds, not a write.
	seeding bool
	// setBoost: the driver has set_boost and the tree the global
	// cpufreq/boost, whose state is boost.
	setBoost bool
	boost    int

	eppWords   []string
	eppDefault string

	pols []*nodeGovPolicy

	noTurbo       int
	maxPerfPct    int64
	minPerfPct    int64
	minPerfPctMin int64
	hwpBoost      int

	tunables map[string]int64 // by cpuGovTunableKey
	uncore   []*nodeUncore
}

// nodeGovPolicy is the governor state of one policy beyond nodePolicy.
// userMaxKHz is scaling_max_freq as last written, before the turbo cap;
// boost the policy's own boost (amd-pstate); setspeedKHz the userspace
// governor's frequency, 0 outside userspace; eppSaved the preference
// intel_pstate saved on entering performance. eppAvail is amd-pstate-epp's
// list of preferences, which follows the governor.
type nodeGovPolicy struct {
	p           *nodePolicy
	hasEPP      bool
	userMaxKHz  int64
	boost       int
	setspeedKHz int64
	eppSaved    string
	eppAvail    *nodeSFile
}

// nodeUncore is one Intel uncore domain: its limits in kHz, its initial
// range, current_freq_khz, and on the TPMI layout the root domain of its
// package, nil without one.
type nodeUncore struct {
	socket                 int
	minKHz, maxKHz         int64
	initMinKHz, initMaxKHz int64
	cur                    *nodeSFile
	root                   *nodeUncoreRoot
}

// nodeUncoreRoot is the root domain TPMI adds to a package with one die,
// package_PP_die_00 (uncore-frequency-tpmi.c:584-592): its clusters, and
// the ratios last written to it, 0 while unset, which a cluster cannot go
// past (uncore-frequency-tpmi.c:319-325).
type nodeUncoreRoot struct {
	clusters           []*nodeUncore
	minRatio, maxRatio int64
}

// buildCPUGovernor registers the governor files of the tree and seeds the
// state from them. The policy files buildCPUFreq registered get the
// governor's handlers here and are seeded again from disk.
func (n *Node) buildCPUGovernor() error {
	cf := n.profile.CPUFreq
	if !cpuGovModeled(cf) {
		return nil
	}
	g := &n.cpuGov
	*g = nodeCPUGovernor{
		on:         true,
		active:     renderSetpolicyDrivers[cf.Driver.V],
		intel:      cpuGovIntel(cf.Driver.V),
		scope:      cpuGovBoostScope(cf),
		baseKHz:    cpuGovBaseKHz(n.profile),
		turboKHz:   cf.CPUInfoMaxKHz.V,
		eppWords:   cpuGovEPPWords(cf),
		eppDefault: cpuGovEPPDefault(cf),
		tunables:   map[string]int64{},
		maxPerfPct: 100,
	}
	g.minPerfPctMin = cpuGovMinPerfPct(cf)
	g.minPerfPct = g.minPerfPctMin
	g.seeding = true
	defer func() { g.seeding = false }()

	if g.intel {
		dir := n.sysPath(layout.IntelPstateDir())
		h, cur := g.noTurboHandler(n)
		n.addAFile(filepath.Join(dir, "no_turbo"), "", h, cur)
		h, cur = g.perfPctHandler(&g.maxPerfPct, true)
		n.addAFile(filepath.Join(dir, "max_perf_pct"), "", h, cur)
		h, cur = g.perfPctHandler(&g.minPerfPct, false)
		n.addAFile(filepath.Join(dir, "min_perf_pct"), "", h, cur)
		h, cur = nodeFlagHandler(&g.hwpBoost)
		n.addAFile(filepath.Join(dir, "hwp_dynamic_boost"), "", h, cur)
	}

	afiles := map[string]*nodeAFile{}
	for _, f := range n.afiles {
		afiles[f.rel] = f
	}
	// buildCPUFreq gave the global boost one state for every policy; with
	// set_boost each policy has its own, which the global one sets.
	g.boost = 1
	if f := afiles[n.relPath(n.sysPath(path.Join(layout.CPUFreqDir(), "boost")))]; f != nil && cpuGovSetBoost(cf) {
		g.setBoost, g.boost = true, cpuGovBootBoost(cf)
		h, cur := g.globalBoostHandler()
		n.rehandle(f, h, cur)
	}
	for _, p := range n.policies {
		gp := &nodeGovPolicy{p: p, boost: g.boost, userMaxKHz: p.reqMaxKHz}
		g.pols = append(g.pols, gp)
		dir := n.sysPath(layout.PolicyDir(p.index))
		cpu := stateID(semconv.HwTypeCPU, p.socket)
		if g.scope == cpuGovBoostPerCPU || g.setBoost {
			h, cur := n.policyBoostHandler(gp, n.relPath(filepath.Join(dir, "boost")))
			n.addAFile(filepath.Join(dir, "boost"), cpu, h, cur)
		}
		if g.setBoost {
			p.boost = &gp.boost // read by nodePolicy.ceilingKHz
		}
		rel := func(name string) *nodeAFile { return afiles[n.relPath(filepath.Join(dir, name))] }
		if f := rel("scaling_max_freq"); f != nil {
			h, cur := n.policyMaxHandler(gp, f.handle)
			n.rehandle(f, h, cur)
		}
		if f := rel("scaling_governor"); f != nil {
			// buildCPUFreq seeded the governor; only its changes are new.
			f.handle = n.policyGovernorHandler(gp, f.handle)
		}
		if f := rel("scaling_setspeed"); f != nil {
			h, cur := gp.setspeedHandler()
			n.rehandle(f, h, cur)
		}
		if f := rel("energy_performance_preference"); f != nil {
			gp.hasEPP = true
			if g.active && p.governor == "performance" {
				gp.eppSaved = g.eppDefault
			}
			h, cur := n.policyEPPHandler(gp, f.rel)
			n.rehandle(f, h, cur)
			if cf.Driver.V == "amd-pstate-epp" {
				gp.eppAvail = n.sfile(filepath.Join(dir, "energy_performance_available_preferences"))
			}
		}
		g.applyTurbo(gp)
	}

	govs := strings.Fields(cf.AvailableGovernors.V)
	t := cf.Tunables
	for _, x := range cpuGovTunables(t) {
		if !slices.Contains(govs, x.gov) {
			continue
		}
		g.tunables[cpuGovTunableKey(x.gov, x.name)] = x.fact.V
		h, cur := g.tunableHandler(x.gov, x.name)
		n.addAFile(n.sysPath(path.Join(layout.GovernorTunablesDir(x.gov), x.name)), "", h, cur)
	}

	if u := cf.Uncore; u != nil {
		for _, d := range cpuGovUncoreDomains(n.profile) {
			dir := n.sysPath(d.dir)
			cpu := stateID(semconv.HwTypeCPU, d.socket)
			if d.root {
				// Its package's clusters come before it.
				r := &nodeUncoreRoot{}
				for _, c := range g.uncore {
					if c.socket == d.socket {
						r.clusters = append(r.clusters, c)
						c.root = r
					}
				}
				for _, high := range []bool{true, false} {
					abs := filepath.Join(dir, "min_freq_khz")
					if high {
						abs = filepath.Join(dir, "max_freq_khz")
					}
					h, cur := g.rootLimitHandler(r, high)
					n.addAFile(abs, cpu, h, cur)
				}
				continue
			}
			dom := &nodeUncore{socket: d.socket, minKHz: u.InitialMinKHz.V, maxKHz: u.InitialMaxKHz.V, initMinKHz: u.InitialMinKHz.V, initMaxKHz: u.InitialMaxKHz.V}
			for _, high := range []bool{true, false} {
				abs := filepath.Join(dir, "min_freq_khz")
				if high {
					abs = filepath.Join(dir, "max_freq_khz")
				}
				h, cur := dom.limitHandler(n, n.relPath(abs), high)
				n.addAFile(abs, cpu, h, cur)
			}
			if u.ELC.V {
				for _, e := range cpuGovELC(u) {
					v := e.v
					h, cur := nodeELCHandler(e.name, &v)
					n.addAFile(filepath.Join(dir, e.name), cpu, h, cur)
				}
			}
			dom.cur = n.sfile(filepath.Join(dir, "current_freq_khz"))
			g.uncore = append(g.uncore, dom)
		}
	}
	return nil
}

// rehandle gives the A file f a new handler and seeds the model from the
// file again, as addAFile does.
func (n *Node) rehandle(f *nodeAFile, handle func(string) (string, string), current func() string) {
	f.handle, f.current = handle, current
	if b, err := os.ReadFile(f.path); err == nil && len(b) > 0 {
		handle(strings.TrimSuffix(string(b), "\n"))
	}
	f.held = current()
}

// turboOff reports whether turbo is off for gp: intel_pstate/no_turbo set,
// or the policy's own boost off.
func (g *nodeCPUGovernor) turboOff(gp *nodeGovPolicy) bool {
	switch g.scope {
	case cpuGovBoostNoTurbo:
		return g.noTurbo == 1
	case cpuGovBoostPerCPU:
		return gp.boost == 0
	}
	return false
}

// applyTurbo sets the policy's maximum request to what was written, capped
// at the top non-turbo frequency while turbo is off.
func (g *nodeCPUGovernor) applyTurbo(gp *nodeGovPolicy) {
	limit := gp.userMaxKHz
	if g.turboOff(gp) && g.baseKHz > 0 {
		limit = min(limit, g.baseKHz)
	}
	gp.p.reqMaxKHz = limit
}

// policyMaxHandler wraps freqHandler(true) of scaling_max_freq: the written
// value is the request turbo leaves alone, and the maximum in effect is
// capped while turbo is off, a write above the cap being clamped. Seeding
// with turbo off, a file at the cap hides the request: the policy's own
// request then stands, its cpuinfo maximum, the kernel's default, so turbo
// back on restores the uncapped maximum (assumed: a request written at the
// cap exactly is taken for the default).
func (n *Node) policyMaxHandler(gp *nodeGovPolicy, inner func(string) (string, string)) (func(string) (string, string), func() string) {
	p, g := gp.p, &n.cpuGov
	handle := func(v string) (string, string) {
		kind, eff := inner(v)
		if kind == EventParseError {
			return kind, eff
		}
		gp.userMaxKHz = p.reqMaxKHz
		if g.seeding && g.turboOff(gp) && g.baseKHz > 0 && p.reqMaxKHz >= g.baseKHz {
			gp.userMaxKHz = p.maxKHz
		}
		g.applyTurbo(gp)
		if got := p.effMaxKHz(); nodeItoa(got) != eff {
			return EventClamped, nodeItoa(got)
		}
		return kind, eff
	}
	return handle, func() string { return nodeItoa(p.effMaxKHz()) + "\n" }
}

// policyGovernorHandler wraps the scaling_governor handler with what a
// change of governor does to the policy's other files: userspace starts at
// the current frequency and forgets it on stop (cpufreq_userspace.c:88,
// 101), and a setpolicy driver shows EPP 0 under performance, intel_pstate
// putting back the preference it saved on the way out.
func (n *Node) policyGovernorHandler(gp *nodeGovPolicy, inner func(string) (string, string)) func(string) (string, string) {
	p := gp.p
	return func(v string) (string, string) {
		old := p.governor
		kind, eff := inner(v)
		if p.governor == old {
			return kind, eff
		}
		switch {
		case p.governor == "userspace":
			gp.setspeedKHz = p.resolveKHz(p.clampKHz(int64(math.Round(p.curKHz))), false)
		case old == "userspace":
			gp.setspeedKHz = 0
		}
		g := &n.cpuGov
		if gp.hasEPP && g.active {
			switch {
			case p.governor == "performance":
				gp.eppSaved, p.epp = p.epp, "performance"
			case old == "performance" && g.intel && p.epp == "performance" && gp.eppSaved != "":
				p.epp = gp.eppSaved
			}
		}
		return kind, eff
	}
}

// setspeedHandler is scaling_setspeed with the userspace governor: under
// it a frequency parsed as sscanf("%u") becomes the target, shown clamped
// to the policy's limits and resolved to the lowest table entry at or above
// it (CPUFREQ_RELATION_L); under any other governor the write fails and
// the file shows <unsupported>, as nodeSetspeedHandler models it.
func (gp *nodeGovPolicy) setspeedHandler() (func(string) (string, string), func() string) {
	p := gp.p
	shown := func() int64 {
		return p.resolveKHz(min(max(gp.setspeedKHz, p.effMinKHz()), p.effMaxKHz()), false)
	}
	outside, outsideCur := nodeSetspeedHandler()
	handle := func(v string) (string, string) {
		if p.governor != "userspace" {
			return outside(v)
		}
		u, ok := nodeSscanfU(v)
		if !ok {
			return EventParseError, nodeItoa(shown())
		}
		gp.setspeedKHz = int64(u)
		if got := shown(); got != int64(u) {
			return EventClamped, nodeItoa(got)
		}
		return EventAccepted, v
	}
	current := func() string {
		if p.governor != "userspace" {
			return outsideCur()
		}
		return nodeItoa(shown()) + "\n"
	}
	return handle, current
}

// policyEPPHandler is energy_performance_preference with the governor
// facts. The first word must be one of epp.available, or on intel_pstate a
// raw EPP from 0 to 255, which reads back as its word when it has one
// (intel_pstate.c:826-908); "default" sets the boot preference. Under the
// performance policy of a setpolicy driver a non-zero EPP is refused with
// EBUSY and the file goes back to performance.
func (n *Node) policyEPPHandler(gp *nodeGovPolicy, rel string) (func(string) (string, string), func() string) {
	p, g := gp.p, &n.cpuGov
	handle := func(v string) (string, string) {
		words := strings.Fields(v)
		if len(words) == 0 {
			return EventParseError, p.epp
		}
		w, epp := words[0], -1
		switch {
		case w == "default" && slices.Contains(g.eppWords, w):
			w = g.eppDefault
			epp = cpuGovEPPValues[w]
		case slices.Contains(g.eppWords, w):
			epp = cpuGovEPPValues[w]
		case g.intel:
			if u, err := strconv.ParseUint(w, 10, 32); err == nil && u <= 255 {
				epp = int(u)
				w = nodeEPPWord(epp)
			}
		}
		if epp < 0 {
			return EventRevertedOutOfRange, p.epp
		}
		if g.active && p.governor == "performance" && epp > 0 {
			return n.rejectWrite(rel, ErrnoEBUSY), p.epp
		}
		p.epp = w
		return EventAccepted, w
	}
	return handle, func() string { return p.epp + "\n" }
}

// nodeEPPWord is how intel_pstate shows a raw EPP: the word whose value it
// is, else the number (intel_pstate.c:894-908).
func nodeEPPWord(epp int) string {
	for w, v := range cpuGovEPPValues {
		if v == epp {
			return w
		}
	}
	return strconv.Itoa(epp)
}

// policyBoostHandler is a policy's boost, parsed with kstrtobool and
// refused with EINVAL while the global boost is off (store_local_boost,
// cpufreq.c:621-648): on amd-pstate turning it off caps the policy's
// maximum at the top non-turbo frequency; on acpi-cpufreq it takes away
// the boost above the table (nodePolicy.ceilingKHz).
func (n *Node) policyBoostHandler(gp *nodeGovPolicy, rel string) (func(string) (string, string), func() string) {
	g := &n.cpuGov
	handle := func(v string) (string, string) {
		b, ok := nodeKstrtobool(v)
		if !ok {
			return EventParseError, strconv.Itoa(gp.boost)
		}
		if g.setBoost && g.boost == 0 && !g.seeding {
			return n.rejectWrite(rel, ErrnoEINVAL), strconv.Itoa(gp.boost)
		}
		gp.boost = b
		g.applyTurbo(gp)
		return EventAccepted, strconv.Itoa(b)
	}
	return handle, func() string { return strconv.Itoa(gp.boost) + "\n" }
}

// globalBoostHandler is cpufreq/boost where the driver has set_boost,
// parsed with kstrtobool (cpufreq.c:595-614). A new state goes to every
// policy; the state it already has changes nothing, so a policy switched
// on its own keeps its boost (cpufreq_boost_trigger_state,
// cpufreq.c:2781-2800). Seeding, each policy's own file carries its state.
func (g *nodeCPUGovernor) globalBoostHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		b, ok := nodeKstrtobool(v)
		if !ok {
			return EventParseError, strconv.Itoa(g.boost)
		}
		if b != g.boost && !g.seeding {
			for _, gp := range g.pols {
				gp.boost = b
				g.applyTurbo(gp)
			}
		}
		g.boost = b
		return EventAccepted, strconv.Itoa(b)
	}
	return handle, func() string { return strconv.Itoa(g.boost) + "\n" }
}

// noTurboHandler is intel_pstate/no_turbo: sscanf("%u"), clamped to 0 or 1
// (intel_pstate.c:1511-1527), applied to every policy.
func (g *nodeCPUGovernor) noTurboHandler(n *Node) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeSscanfU(v)
		if !ok {
			return EventParseError, strconv.Itoa(g.noTurbo)
		}
		was := g.noTurbo
		g.noTurbo = int(min(u, 1))
		if g.noTurbo == 1 && was == 0 && g.baseKHz > 0 && g.turboKHz > 0 {
			g.minPerfPct = min(g.minPerfPct, g.baseKHz*100/g.turboKHz)
		}
		for _, gp := range g.pols {
			g.applyTurbo(gp)
		}
		if u > 1 {
			return EventClamped, strconv.Itoa(g.noTurbo)
		}
		return EventAccepted, strconv.Itoa(g.noTurbo)
	}
	return handle, func() string { return strconv.Itoa(g.noTurbo) + "\n" }
}

// perfPctHandler is intel_pstate/max_perf_pct (high) or min_perf_pct:
// sscanf("%u"), clamped to [min_perf_pct, 100] and [the lowest P-state,
// max_perf_pct] (intel_pstate.c:1604-1665). With no_turbo set,
// min_perf_pct stays at or below the non-turbo share of the turbo maximum
// (intel_pstate.c:1548-1554).
func (g *nodeCPUGovernor) perfPctHandler(dst *int64, high bool) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeSscanfU(v)
		if !ok {
			return EventParseError, nodeItoa(*dst)
		}
		x := int64(min(u, math.MaxInt32))
		if high {
			*dst = min(max(x, g.minPerfPct), 100)
		} else {
			*dst = min(max(x, g.minPerfPctMin), g.maxPerfPct)
		}
		if *dst != x {
			return EventClamped, nodeItoa(*dst)
		}
		return EventAccepted, nodeItoa(*dst)
	}
	return handle, func() string { return nodeItoa(*dst) + "\n" }
}

// tunableHandler is one governor tunable, parsed as sscanf("%u") (kstrtouint
// for schedutil) and checked by cpuGovTunableCheck; a value the kernel
// refuses is reverted.
func (g *nodeCPUGovernor) tunableHandler(gov, name string) (func(string) (string, string), func() string) {
	key := cpuGovTunableKey(gov, name)
	parse := nodeSscanfU
	if gov == "schedutil" {
		parse = func(v string) (uint64, bool) { return nodeKstrtou(v, 10, 32) }
	}
	handle := func(v string) (string, string) {
		u, ok := parse(v)
		if !ok || u > math.MaxUint32 {
			return EventParseError, nodeItoa(g.tunables[key])
		}
		up, down := g.tunables[cpuGovTunableKey("conservative", "up_threshold")], g.tunables[cpuGovTunableKey("conservative", "down_threshold")]
		x, ok := cpuGovTunableCheck(gov, name, int64(u), up, down)
		if !ok {
			return EventRevertedOutOfRange, nodeItoa(g.tunables[key])
		}
		g.tunables[key] = x
		if x != int64(u) {
			return EventClamped, nodeItoa(x)
		}
		return EventAccepted, nodeItoa(x)
	}
	return handle, func() string { return nodeItoa(g.tunables[key]) + "\n" }
}

// set holds khz within the domain's initial range (assumed: the hardware
// clamp) and makes it the maximum (high) or the minimum.
func (d *nodeUncore) set(high bool, khz int64) {
	dst := &d.minKHz
	if high {
		dst = &d.maxKHz
	}
	*dst = min(max(khz, d.initMinKHz), d.initMaxKHz)
}

// nodeUncoreRatio parses an uncore limit write as the kernel does:
// kstrtouint, truncated to a whole ratio of 100 MHz and refused at ratio 0
// or above the 7-bit field (uncore-frequency.c:66-74,
// uncore-frequency-tpmi.c:290-292).
func nodeUncoreRatio(v string) (u uint64, ratio int64, kind string) {
	u, ok := nodeKstrtou(v, 10, 32)
	if !ok {
		return 0, 0, EventParseError
	}
	ratio = int64(u) / cpuGovUncoreUnitKHz
	if ratio == 0 || ratio > cpuGovUncoreMaxRatio {
		return u, 0, EventRevertedOutOfRange
	}
	return u, ratio, ""
}

// limitHandler is an uncore domain's max_freq_khz (high) or min_freq_khz,
// rel relative to the node root: a whole ratio (nodeUncoreRatio) held
// within the domain's initial range. With a TPMI root domain whose limit
// is set, a maximum above it or a minimum below it is refused with EINVAL
// (uncore-frequency-tpmi.c:319-325).
func (d *nodeUncore) limitHandler(n *Node, rel string, high bool) (func(string) (string, string), func() string) {
	dst := &d.minKHz
	if high {
		dst = &d.maxKHz
	}
	handle := func(v string) (string, string) {
		u, ratio, bad := nodeUncoreRatio(v)
		if bad != "" {
			return bad, nodeItoa(*dst)
		}
		if r := d.root; r != nil && !n.cpuGov.seeding {
			if high && r.maxRatio != 0 && r.maxRatio < ratio || !high && r.minRatio != 0 && r.minRatio > ratio {
				return n.rejectWrite(rel, ErrnoEINVAL), nodeItoa(*dst)
			}
		}
		khz := ratio * cpuGovUncoreUnitKHz
		d.set(high, khz)
		switch {
		case *dst != khz:
			return EventClamped, nodeItoa(*dst)
		case khz != int64(u):
			return EventQuantized, nodeItoa(*dst)
		}
		return EventAccepted, nodeItoa(*dst)
	}
	return handle, func() string { return nodeItoa(*dst) + "\n" }
}

// rootLimitHandler is max_freq_khz (high) or min_freq_khz of a TPMI root
// domain. It reads the highest maximum or the lowest minimum of the
// package's clusters (uncore-frequency-tpmi.c:150-181); a write, parsed as
// a cluster's, sets every cluster, each within its own initial range, and
// becomes the root's limit (uncore-frequency-tpmi.c:300-317). Seeding
// changes nothing: the clusters' files carry their limits, and the root's
// own limit, which no file shows, starts unset as at boot (assumed).
func (g *nodeCPUGovernor) rootLimitHandler(r *nodeUncoreRoot, high bool) (func(string) (string, string), func() string) {
	shown := func() int64 {
		var out int64
		for i, c := range r.clusters {
			switch {
			case i == 0:
				out = c.minKHz
				if high {
					out = c.maxKHz
				}
			case high:
				out = max(out, c.maxKHz)
			default:
				out = min(out, c.minKHz)
			}
		}
		return out
	}
	handle := func(v string) (string, string) {
		u, ratio, bad := nodeUncoreRatio(v)
		if bad != "" {
			return bad, nodeItoa(shown())
		}
		if g.seeding {
			return EventAccepted, nodeItoa(shown())
		}
		khz := ratio * cpuGovUncoreUnitKHz
		for _, c := range r.clusters {
			c.set(high, khz)
		}
		if high {
			r.maxRatio = ratio
		} else {
			r.minRatio = ratio
		}
		switch got := shown(); {
		case got != khz:
			return EventClamped, nodeItoa(got)
		case khz != int64(u):
			return EventQuantized, nodeItoa(got)
		}
		return EventAccepted, nodeItoa(khz)
	}
	return handle, func() string { return nodeItoa(shown()) + "\n" }
}

// nodeELCHandler is an efficiency latency control attribute of a TPMI
// uncore domain (uncore-frequency-common.c:60-80,
// uncore-frequency-tpmi.c:199-221): the enable is a kstrtobool, the
// thresholds are percentages up to 100, and the floor a frequency within
// the ratio field, kept in whole ratios.
func nodeELCHandler(name string, dst *int64) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		if name == "elc_high_threshold_enable" {
			b, ok := nodeKstrtobool(v)
			if !ok {
				return EventParseError, nodeItoa(*dst)
			}
			*dst = int64(b)
			return EventAccepted, nodeItoa(*dst)
		}
		u, ok := nodeKstrtou(v, 10, 32)
		if !ok {
			return EventParseError, nodeItoa(*dst)
		}
		x := int64(u)
		if name != "elc_floor_freq_khz" {
			if x > 100 {
				return EventRevertedOutOfRange, nodeItoa(*dst)
			}
			*dst = x
			return EventAccepted, nodeItoa(x)
		}
		if x/cpuGovUncoreUnitKHz > cpuGovUncoreMaxRatio {
			return EventRevertedOutOfRange, nodeItoa(*dst)
		}
		*dst = x / cpuGovUncoreUnitKHz * cpuGovUncoreUnitKHz
		if *dst != x {
			return EventQuantized, nodeItoa(*dst)
		}
		return EventAccepted, nodeItoa(x)
	}
	return handle, func() string { return nodeItoa(*dst) + "\n" }
}

// deriveCPUGovernor recomputes what follows from the governor state and the
// load: each package's govNative, the governor its policies run in the
// driver's own words, followed by " epp=" and the energy-performance
// preference where the policies have one (powersave epp=balance_power), as
// pkg/governor spells Plan.Native, and empty when its policies disagree.
// Under the performance policy of a setpolicy driver the kernel holds EPP
// at 0, so the preference is left out there, as a performance plan writes
// none.
func (n *Node) deriveCPUGovernor() {
	g := &n.cpuGov
	if !g.on {
		return
	}
	for _, pk := range n.pkgs {
		native, mixed := "", false
		for _, gp := range g.pols {
			if gp.p.socket != pk.socket {
				continue
			}
			s := gp.p.governor
			if gp.hasEPP && !(g.active && s == "performance") {
				s += " epp=" + gp.p.epp
			}
			if native != "" && s != native {
				mixed = true
			}
			native = s
		}
		if mixed {
			native = ""
		}
		pk.govNative = native
	}
}

// substepCPUGovernor advances the governor state by one substep of h
// seconds. Nothing does: the CPU governor state changes by writes alone.
func (n *Node) substepCPUGovernor(h float64) {}

// writeCPUGovernor writes the governor files the emulator owns: the
// preferences amd-pstate-epp offers, performance alone under the
// performance policy (amd-pstate.c:1136-1152), and each uncore domain's
// current_freq_khz, which runs from the domain's minimum to its maximum
// with the load of its package in whole ratios (assumed: no uncore physics).
func (n *Node) writeCPUGovernor() error {
	g := &n.cpuGov
	if !g.on {
		return nil
	}
	var errs []error
	for _, gp := range g.pols {
		if gp.eppAvail == nil {
			continue
		}
		prefs := cpuGovEPPList(g.eppWords)
		if gp.p.governor == "performance" {
			prefs = "performance\n"
		}
		if err := n.writeS(gp.eppAvail, prefs); err != nil {
			errs = append(errs, err)
		}
	}
	for _, d := range g.uncore {
		if d.cur == nil {
			continue
		}
		util := 0.0
		if d.socket < len(n.pkgs) {
			util = n.pkgs[d.socket].util
		}
		lo, hi := d.minKHz, max(d.minKHz, d.maxKHz)
		khz := lo + int64(math.Floor(float64(hi-lo)*math.Max(0, math.Min(1, util))/cpuGovUncoreUnitKHz))*cpuGovUncoreUnitKHz
		if err := n.writeS(d.cur, nodeItoa(khz)+"\n"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
