package hwemu

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// AMD GPU DPM emulation, Node side. NewNode, derive, substep and
// writeOutputs call these hooks. What the physics reads goes into
// nodeGPU.clock (clockstate.go).
//
// Kernel references are Linux v6.17: amdgpu_pm.c (the sysfs stores),
// amdgpu_smu.c (profile levels and idle gating), smu_v13_0.c and
// smu_v13_0_0_ppt.c (SMU 13.0.0, Navi 31), smu_v13_0_6_ppt.c (SMU 13.0.6,
// MI300). A write the kernel fails with an errno lands in the file anyway,
// so the handler refuses it with rejectWrite and the readback puts the value
// in effect back (reject.go).

const (
	// dpmBusyUtil is the utilization from which a GPU counts as running its
	// own work: the compute clock leaves idle (assumed, the CPU model's idle
	// threshold).
	dpmBusyUtil = 0.05
	// dpmMemoryActive is the utilization times memory intensity from which
	// a GPU's own work raises the data path under a demand-driven level
	// (assumed).
	dpmMemoryActive = 0.1
)

// dpmOD is an overdrive range in MHz: the sclk and uclk bounds. In the
// pending edits a zero bound is one nobody set.
type dpmOD struct{ sclkMin, sclkMax, uclkMin, uclkMax int64 }

// dpmState is what an amdgpu device keeps between writes: the performance
// level, the level a profile_* level was entered from (profile_exit goes
// back to it), the committed overdrive range and the edits pending a commit,
// the power profile and the pm_policy indices. Render seeds a tree from a
// fresh one; a Node seeds it from the tree.
type dpmState struct {
	level, saved        string
	od, custom          dpmOD
	profile             int
	socPState, xgmiPLPD int
}

// newDPMState is the state at boot: the default level, the overdrive range
// at the table limits, the default power profile and pm_policy indices.
func newDPMState(d *DPMSpec) dpmState {
	s := dpmState{level: d.Default.V, od: dpmDefaultOD(d)}
	if pp := d.PowerProfiles; pp != nil {
		s.profile = max(0, slices.Index(pp.Modes.V, pp.Default.V))
	}
	if pm := d.PMPolicy; pm != nil {
		if pm.SocPState != nil {
			s.socPState = pm.SocPState.Default.V
		}
		if pm.XGMIPLPD != nil {
			s.xgmiPLPD = pm.XGMIPLPD.Default.V
		}
	}
	return s
}

// dpmDefaultOD is the overdrive range at the table limits, which auto and
// the restore command "r" go back to (smu_v13_0_6_ppt.c:1981-2003,
// 2194-2215).
func dpmDefaultOD(d *DPMSpec) dpmOD {
	t := d.Tables
	return dpmOD{sclkMin: t.Sclk.MinMHz.V, sclkMax: t.Sclk.MaxMHz.V, uclkMin: dpmFirst(t.Mclk.V), uclkMax: dpmLast(t.Mclk.V)}
}

func dpmFirst(t []int64) int64 {
	if len(t) == 0 {
		return 0
	}
	return t[0]
}

func dpmLast(t []int64) int64 {
	if len(t) == 0 {
		return 0
	}
	return t[len(t)-1]
}

// dpmAtMost is the highest entry of t at or below mhz, or its first entry
// when none is: the level the DPM picks under a soft maximum.
func dpmAtMost(t []int64, mhz int64) int64 {
	v := dpmFirst(t)
	for _, x := range t {
		if x <= mhz {
			v = x
		}
	}
	return v
}

// setLevel applies an accepted level. profile_exit returns to the level a
// profile_* level was entered from, and does nothing outside one (assumed:
// the kernel then stores a level it shows as unknown, amdgpu_smu.c:2302-2311).
// Leaving perf_determinism restores the sclk maximum, and auto restores the
// overdrive range and drops the pending edits (smu_v13_0_6_ppt.c:1970-2003).
func (s *dpmState) setLevel(d *DPMSpec, w string) {
	prev := s.level
	if w == dpmProfileExit {
		if !dpmProfileLevel(prev) {
			return
		}
		w = s.saved
	}
	if dpmProfileLevel(w) && !dpmProfileLevel(prev) {
		s.saved = prev
	}
	if prev == dpmPerfDeterminism && w != dpmPerfDeterminism {
		s.od.sclkMax = d.Tables.Sclk.MaxMHz.V
	}
	if w == dpmAuto {
		s.od, s.custom = dpmDefaultOD(d), dpmOD{}
	}
	s.level = w
}

// dpmView is what one GPU shows and the physics reads at one load: the
// clock of each domain in MHz as the pp_dpm_* stars show it, the data-path
// level and whether it follows demand, idle gating, and the compute and
// memory clock bounds of gpuClockState.
type dpmView struct {
	sclk, mclk, fclk, socclk  int64
	dataPath                  phys.DataPathLevel
	dataPathAuto, gatingOff   bool
	floorMHz, ceilMHz, maxMHz float64
	memFloorMHz, memCeilMHz   float64
}

// view is the state of a GPU under s at a load: busy when it runs work of
// its own, dataUp while its data path is raised by its own memory-active
// work or the hold after it. The levels follow smu_v13_0_set_performance_level
// (smu_v13_0.c:1611-1665) and, on SMU 13.0.6, the overdrive range under
// manual and perf_determinism. An idle compute clock with idle gating kept
// reads its minimum (the deep-sleep clock below it is not shown;
// TestGD03HighHoldsTheDataPathAndKeepsIdleGating); with gating off
// (profile_* levels, amdgpu_smu.c:2293-2301) it stays at the clock the level
// holds, its maximum when no clock is pinned (assumed). profile_min_sclk and
// profile_min_mclk pin one domain and leave the others to demand (assumed:
// the kernel leaves their earlier range).
func (s *dpmState) view(d *DPMSpec, busy, dataUp bool) dpmView {
	t := d.Tables
	sMin, sMax := t.Sclk.MinMHz.V, t.Sclk.MaxMHz.V
	v := dpmView{maxMHz: float64(sMax), gatingOff: dpmProfileLevel(s.level)}
	demand := func(uclkMax int64) {
		v.dataPathAuto = true
		if dataUp {
			v.dataPath = phys.DataPathMax
			v.mclk, v.fclk, v.socclk = dpmAtMost(t.Mclk.V, uclkMax), dpmLast(t.Fclk.V), dpmLast(t.Socclk.V)
		} else {
			v.dataPath = phys.DataPathMin
			v.mclk, v.fclk, v.socclk = dpmFirst(t.Mclk.V), dpmFirst(t.Fclk.V), dpmFirst(t.Socclk.V)
		}
	}
	idle := func(run int64) int64 {
		if busy || v.gatingOff {
			return run
		}
		return sMin
	}
	pin := func(ps DPMPStateSpec) {
		v.sclk = ps.SclkMHz.V
		v.mclk, v.fclk, v.socclk = dpmOr(ps.MclkMHz.V, t.Mclk.V), dpmOr(ps.FclkMHz.V, t.Fclk.V), dpmOr(ps.SocclkMHz.V, t.Socclk.V)
		v.floorMHz, v.ceilMHz = float64(v.sclk), float64(v.sclk)
		v.memFloorMHz, v.memCeilMHz = float64(v.mclk), float64(v.mclk)
		ref, table := v.fclk, t.Fclk.V
		if len(table) == 0 {
			ref, table = v.mclk, t.Mclk.V
		}
		switch {
		case ref <= dpmFirst(table):
			v.dataPath = phys.DataPathMin
		case ref >= dpmLast(table):
			v.dataPath = phys.DataPathMax
		default:
			v.dataPath = phys.DataPathMid
		}
	}
	switch s.level {
	case dpmHigh:
		v.dataPath = phys.DataPathMax
		v.mclk, v.fclk, v.socclk = dpmLast(t.Mclk.V), dpmLast(t.Fclk.V), dpmLast(t.Socclk.V)
		v.sclk = idle(sMax)
		v.floorMHz = float64(sMax)
		v.memFloorMHz = float64(v.mclk)
	case dpmLow:
		v.dataPath = phys.DataPathMin
		v.mclk, v.fclk, v.socclk = dpmFirst(t.Mclk.V), dpmFirst(t.Fclk.V), dpmFirst(t.Socclk.V)
		v.sclk = sMin
		v.floorMHz, v.ceilMHz = float64(sMin), float64(sMin)
		v.memCeilMHz = float64(v.mclk)
	case dpmProfileStandard:
		pin(d.PState.Standard)
	case dpmProfilePeak:
		pin(d.PState.Peak)
	case dpmProfileMinSclk:
		demand(dpmLast(t.Mclk.V))
		v.sclk = sMin
		v.floorMHz, v.ceilMHz = float64(sMin), float64(sMin)
	case dpmProfileMinMclk:
		demand(dpmLast(t.Mclk.V))
		v.dataPath, v.dataPathAuto = phys.DataPathMin, false
		v.mclk = dpmFirst(t.Mclk.V)
		v.memCeilMHz = float64(v.mclk)
		v.sclk = idle(sMax)
	case dpmManual, dpmPerfDeterminism:
		demand(min(s.od.uclkMax, dpmLast(t.Mclk.V)))
		ceil := min(s.od.sclkMax, sMax)
		v.sclk = idle(ceil)
		if s.od.sclkMin > sMin {
			v.floorMHz = float64(s.od.sclkMin)
		}
		if ceil < sMax {
			v.ceilMHz = float64(ceil)
		}
		if s.od.uclkMax > 0 && s.od.uclkMax < dpmLast(t.Mclk.V) {
			v.memCeilMHz = float64(s.od.uclkMax)
		}
	default: // auto
		demand(dpmLast(t.Mclk.V))
		v.sclk = idle(sMax)
	}
	return v
}

// dpmOr is v, or the top of table when the profile leaves v unset.
func dpmOr(v int64, table []int64) int64 {
	if v > 0 {
		return v
	}
	return dpmLast(table)
}

// dpmFile is the content of one pp_dpm_* file.
type dpmFile struct{ name, content string }

// files are the pp_dpm_* files of v: sclk, mclk, and fclk, socclk and pcie
// when the family has the table.
func (v dpmView) files(d *DPMSpec) []dpmFile {
	t := d.Tables
	out := []dpmFile{{dpmFileSclk, dpmSclkText(t, v.sclk)}, {dpmFileMclk, dpmLevelsText(t.Mclk.V, v.mclk)}}
	if len(t.Fclk.V) > 0 {
		out = append(out, dpmFile{dpmFileFclk, dpmLevelsText(t.Fclk.V, v.fclk)})
	}
	if len(t.Socclk.V) > 0 {
		out = append(out, dpmFile{dpmFileSocclk, dpmLevelsText(t.Socclk.V, v.socclk)})
	}
	if len(t.PCIe.V) > 0 {
		out = append(out, dpmFile{dpmFilePCIe, dpmPCIeText(t.PCIe.V)})
	}
	return out
}

// dpmLevelsText is a discrete pp_dpm_* file: "%d: %uMhz %s\n" per level,
// the star on the level equal to the current clock and an empty string,
// with its space, on the others (smu_v13_0_0_ppt.c:1282-1285).
func dpmLevelsText(table []int64, cur int64) string {
	var b strings.Builder
	for i, v := range table {
		star := ""
		if v == cur {
			star = "*"
		}
		fmt.Fprintf(&b, "%d: %dMhz %s\n", i, v, star)
	}
	return b.String()
}

// dpmSclkText is pp_dpm_sclk. A fine-grained table prints its minimum and
// maximum, and a clock between them as a middle level
// (smu_v13_0_0_ppt.c:1253-1280); a discrete one is printed as its two
// levels.
func dpmSclkText(t DPMTablesSpec, cur int64) string {
	lo, hi := t.Sclk.MinMHz.V, t.Sclk.MaxMHz.V
	if t.Sclk.Fine.V && cur != lo && cur != hi {
		return fmt.Sprintf("0: %dMhz\n1: %dMhz *\n2: %dMhz\n", lo, cur, hi)
	}
	return dpmLevelsText([]int64{lo, hi}, cur)
}

// dpmPCIeText is pp_dpm_pcie: each level as the profile spells it after the
// index, the star on the highest. The link was measured unchanged at its top
// level whatever the performance level (assumed for every family).
func dpmPCIeText(levels []string) string {
	var b strings.Builder
	for i, l := range levels {
		star := ""
		if i == len(levels)-1 {
			star = "*"
		}
		fmt.Fprintf(&b, "%d: %s %s\n", i, l, star)
	}
	return b.String()
}

// odText is pp_od_clk_voltage on SMU 13.0.6: the committed sclk range, then
// the uclk range when the family lets a write set a uclk bound
// (smu_v13_0_6_ppt.c:1375-1380, 1421-1429).
func (s *dpmState) odText(d *DPMSpec) string {
	out := fmt.Sprintf("OD_SCLK:\n0: %dMhz\n1: %dMhz\n", s.od.sclkMin, s.od.sclkMax)
	if w := d.OD.Writable.V; slices.Contains(w, dpmODUclkMin) || slices.Contains(w, dpmODUclkMax) {
		out += fmt.Sprintf("OD_MCLK:\n0: %dMhz\n1: %dMhz\n", s.od.uclkMin, s.od.uclkMax)
	}
	return out
}

// dpmProfileTitles is the header of pp_power_profile_mode on SMU 13.0.0
// (smu_v13_0_0_ppt.c:2489-2509).
var dpmProfileTitles = []any{"PROFILE_INDEX(NAME)", "CLOCK_TYPE(NAME)", "FPS", "MinActiveFreqType", "MinActiveFreq",
	"BoosterFreqType", "BoosterFreq", "PD_Data_limit_c", "PD_Data_error_coeff", "PD_Data_error_rate_coeff"}

// profileText is pp_power_profile_mode: the header, then each mode as
// "%2d %14s%s:" with the star on the user's choice, never on the compute
// profile KFD raises while queues run (smu_v13_0_0_ppt.c:2531-2532;
// amdgpu_amdkfd.c:700-720). The activity monitor coefficients under each
// mode are not modelled.
func (s *dpmState) profileText(d *DPMSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%16s %s %s %s %s %s %s %s %s %s\n", dpmProfileTitles...)
	for i, m := range d.PowerProfiles.Modes.V {
		star := " "
		if i == s.profile {
			star = "*"
		}
		fmt.Fprintf(&b, "%2d %14s%s:\n", i, m, star)
	}
	return b.String()
}

// dpmPolicyText is a pm_policy attribute: "%d : %s" per policy, the star
// on the one in effect (amdgpu_pm.c:2104-2121).
func dpmPolicyText(a *PMPolicyAttrSpec, cur int) string {
	var b strings.Builder
	for i, p := range a.Policies.V {
		star := ""
		if i == cur {
			star = "*"
		}
		fmt.Fprintf(&b, "%d : %s%s\n", i, p, star)
	}
	return b.String()
}

// nodeGPUDPM is the DPM state of one AMD GPU that gpuClockState does not
// hold: the device's state, the hold of its data path after its own
// memory-active work, and the pp_dpm_* files the emulator writes.
type nodeGPUDPM struct {
	on bool
	// facts are the pkg/governor facts the agent discovers for the device,
	// which name the preset its level and overdrive range map to.
	facts     governor.AMDFacts
	st        dpmState
	view      dpmView
	memActive bool
	holdLeft  float64
	files     map[string]*nodeSFile
}

// buildGPUDPM registers the DPM files of every AMD GPU and seeds the state
// from them: the level, then the overdrive file, so a level set and an
// overdrive edit written together are handled in that order, as the real
// writes were made, then the power profile and pm_policy. The overdrive,
// power profile and pm_policy files are seeded from the tables they show,
// which no write could set, so a Node over a live tree keeps them.
func (n *Node) buildGPUDPM() error {
	gs := n.profile.GPUs
	if gs == nil || gs.Vendor != "amd" || gs.DPM == nil {
		return nil
	}
	d := gs.DPM
	for _, g := range n.gpus {
		if g.sysfsPath == "" {
			continue
		}
		dev := n.sysPath(g.sysfsPath)
		file := func(name string) string { return filepath.Join(dev, filepath.FromSlash(name)) }
		if !n.agentWritable(file(layout.AMDGPUPerfLevelFile)) {
			continue
		}
		dp := &g.dpm
		dp.on, dp.st, dp.files = true, newDPMState(d), map[string]*nodeSFile{}
		dp.facts = governor.AMDFacts{
			DeviceID: gs.PCI.Device.V,
			Family:   discover.AMDFamily(gs.PCI.Device.V),
			OD:       d.OD != nil && d.OD.Enabled.V,
			SclkMHz:  [2]int{int(d.Tables.Sclk.MinMHz.V), int(d.Tables.Sclk.MaxMHz.V)},
		}
		st := &dp.st
		if s, ok := n.readString(file(layout.AMDGPUODFile)); ok {
			dpmParseOD(s, &st.od)
		}
		if s, ok := n.readString(file(layout.AMDGPUPowerProfileFile)); ok && d.PowerProfiles != nil {
			if i, ok := dpmStarIndex(s, "*:"); ok && i < len(d.PowerProfiles.Modes.V) {
				st.profile = i
			}
		}
		var policies []dpmPolicyRef
		if pm := d.PMPolicy; pm != nil {
			policies = []dpmPolicyRef{{dpmPolicySocPState, pm.SocPState, &st.socPState}, {dpmPolicyXGMIPLPD, pm.XGMIPLPD, &st.xgmiPLPD}}
		}
		for _, p := range policies {
			if p.spec == nil {
				continue
			}
			if s, ok := n.readString(file("pm_policy/" + p.name)); ok {
				if i, ok := dpmStarIndex(s, "*"); ok && i < len(p.spec.Policies.V) {
					*p.cur = i
				}
			}
		}

		id := g.id()
		abs := file(layout.AMDGPUPerfLevelFile)
		h, cur := n.dpmLevelHandler(d, st, n.relPath(abs))
		n.addAFile(abs, id, h, cur)
		if d.OD != nil && d.OD.Enabled.V {
			abs = file(layout.AMDGPUODFile)
			h, cur = n.dpmODHandler(d, st, n.relPath(abs))
			n.addAFile(abs, id, h, cur)
		}
		if d.PowerProfiles != nil {
			abs = file(layout.AMDGPUPowerProfileFile)
			h, cur = n.dpmProfileHandler(d, st, n.relPath(abs))
			n.addAFile(abs, id, h, cur)
		}
		for _, p := range policies {
			if p.spec == nil {
				continue
			}
			abs = file("pm_policy/" + p.name)
			h, cur = n.dpmPolicyHandler(p.spec, p.cur, n.relPath(abs))
			n.addAFile(abs, id, h, cur)
		}
		for _, name := range []string{dpmFileSclk, dpmFileMclk, dpmFileFclk, dpmFileSocclk, dpmFilePCIe} {
			if f := n.sfile(file(name)); f != nil {
				dp.files[name] = f
			}
		}
		g.clock.modelled = true
	}
	return nil
}

// dpmPolicyRef is one pm_policy attribute of a GPU: its name, its spec and
// the index in effect.
type dpmPolicyRef struct {
	name string
	spec *PMPolicyAttrSpec
	cur  *int
}

// dpmParseOD reads the committed ranges back from pp_od_clk_voltage as
// odText prints it. A range it cannot read keeps od as it is.
func dpmParseOD(s string, od *dpmOD) {
	var section string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ":") {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		idx, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		mhz, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(val), "Mhz"), 10, 64)
		if err != nil {
			continue
		}
		switch section + " " + idx {
		case "OD_SCLK 0":
			od.sclkMin = mhz
		case "OD_SCLK 1":
			od.sclkMax = mhz
		case "OD_MCLK 0":
			od.uclkMin = mhz
		case "OD_MCLK 1":
			od.uclkMax = mhz
		}
	}
}

// dpmStarIndex is the index at the start of the line that carries star, as
// pp_power_profile_mode and pm_policy mark the setting in effect.
func dpmStarIndex(s, star string) (int, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, star) {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			return 0, false
		}
		i, err := strconv.Atoi(f[0])
		return i, err == nil && i >= 0
	}
	return 0, false
}

// dpmLevelHandler is power_dpm_force_performance_level. The kernel takes
// the first word whose prefix matches, in dpmLevelWords order, and fails
// anything else with EINVAL; a level the firmware refuses fails with EINVAL
// too, whatever the firmware returned (amdgpu_pm.c:365-398), unless the
// profile names another errno.
func (n *Node) dpmLevelHandler(d *DPMSpec, st *dpmState, rel string) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		if v == "" {
			return EventParseError, st.level
		}
		word := ""
		for _, w := range dpmLevelWords {
			if strings.HasPrefix(v, w) {
				word = w
				break
			}
		}
		if word == "" {
			return n.rejectWrite(rel, ErrnoEINVAL), st.level
		}
		if e := dpmLevelErrno(d, word); e != "" {
			return n.rejectWrite(rel, e), st.level
		}
		st.setLevel(d, word)
		return EventAccepted, st.level
	}
	return handle, func() string { return st.level + "\n" }
}

// dpmODHandler is pp_od_clk_voltage. Each line of a write is one command,
// as if written on its own: a file holds only the last write, or the lines
// of writes appended to it, so two commands written within one step reach
// the emulator only this way. A rejected line does not stop the lines after
// it, as a failed write does not stop the next. Fidelity gap: the kernel
// takes one command per write and fails a single write of "s 1 <MHz>\nc"
// with EINVAL, the "c" failing kstrtol (amdgpu_pm.c:746-805), so only the
// fake tools, which cannot wait for a step, write both lines at once. A real
// writer writes each command on its own and, to work here as well, waits
// for the file to read back the table before it writes "c".
func (n *Node) dpmODHandler(d *DPMSpec, st *dpmState, rel string) (func(string) (string, string), func() string) {
	eff := func() string { return strings.Join(strings.Fields(st.odText(d)), " ") }
	handle := func(v string) (string, string) {
		if strings.TrimSpace(v) == "" {
			return EventParseError, eff()
		}
		kind := EventAccepted
		for _, line := range strings.Split(v, "\n") {
			if line == "" {
				continue
			}
			if e := st.odCommand(d, line); e != "" && kind == EventAccepted {
				kind = n.rejectWrite(rel, e)
			}
		}
		return kind, eff()
	}
	return handle, func() string { return st.odText(d) }
}

// odCommand applies one overdrive command and returns the errno the kernel
// fails it with, "" when it takes it. Edits are allowed only under manual or
// perf_determinism; "s 0" and "s 1" set the sclk bounds within the table, "m"
// the uclk bounds the family lets a write set, "r" restores the table
// limits at once, "c" commits the edits, refusing a minimum at or above the
// maximum (smu_v13_0_6_ppt.c:2030-2056, 2097-2246; amdgpu_pm.c:746-805).
func (s *dpmState) odCommand(d *DPMSpec, line string) string {
	if s.level != dpmManual && s.level != dpmPerfDeterminism {
		return ErrnoEINVAL
	}
	var params []int64
	for _, f := range strings.Fields(line[1:]) {
		x, err := strconv.ParseInt(f, 0, 64)
		if err != nil {
			return ErrnoEINVAL
		}
		params = append(params, x)
	}
	t := d.Tables
	writable := func(bound string) bool { return slices.Contains(d.OD.Writable.V, bound) }
	switch line[0] {
	case 's':
		if len(params) != 2 {
			return ErrnoEINVAL
		}
		switch x := params[1]; {
		case params[0] == 0 && writable(dpmODSclkMin):
			if x < t.Sclk.MinMHz.V {
				s.custom.sclkMin = s.od.sclkMin
				return ErrnoEINVAL
			}
			s.custom.sclkMin = x
		case params[0] == 1 && writable(dpmODSclkMax):
			if x > t.Sclk.MaxMHz.V {
				s.custom.sclkMax = s.od.sclkMax
				return ErrnoEINVAL
			}
			s.custom.sclkMax = x
		default:
			return ErrnoEINVAL
		}
	case 'm':
		if len(params) != 2 {
			return ErrnoEINVAL
		}
		switch x := params[1]; {
		case params[0] == 0 && writable(dpmODUclkMin):
			if x < dpmFirst(t.Mclk.V) {
				return ErrnoEINVAL
			}
			s.custom.uclkMin = x
		case params[0] == 1 && writable(dpmODUclkMax):
			if x > dpmLast(t.Mclk.V) {
				s.custom.uclkMax = s.od.uclkMax
				return ErrnoEINVAL
			}
			s.custom.uclkMax = x
		default:
			// MI300X: "Setting min UCLK level is not supported" (2174).
			return ErrnoEINVAL
		}
	case 'r':
		if len(params) != 0 {
			return ErrnoEINVAL
		}
		s.od, s.custom = dpmDefaultOD(d), dpmOD{}
	case 'c':
		if len(params) != 0 {
			return ErrnoEINVAL
		}
		lo, hi := s.od.sclkMin, s.od.sclkMax
		if s.custom.sclkMin != 0 {
			lo = s.custom.sclkMin
		}
		if s.custom.sclkMax != 0 {
			hi = s.custom.sclkMax
		}
		if s.level == dpmPerfDeterminism {
			// Determinism keeps the table minimum and caps at the edit
			// (2069-2093).
			lo = t.Sclk.MinMHz.V
			if hi < t.Sclk.MinMHz.V || hi > t.Sclk.MaxMHz.V {
				return ErrnoEINVAL
			}
		} else if lo >= hi {
			return ErrnoEINVAL
		}
		ulo, uhi := s.od.uclkMin, s.od.uclkMax
		if s.custom.uclkMin != 0 {
			ulo = s.custom.uclkMin
		}
		if s.custom.uclkMax != 0 {
			uhi = s.custom.uclkMax
		}
		if (s.custom.uclkMin != 0 || s.custom.uclkMax != 0) && ulo >= uhi {
			return ErrnoEINVAL
		}
		s.od = dpmOD{sclkMin: lo, sclkMax: hi, uclkMin: ulo, uclkMax: uhi}
	default:
		return ErrnoEINVAL
	}
	return ""
}

// dpmProfileHandler is pp_power_profile_mode: the mode is the first
// character of the write, read as a number (amdgpu_pm.c:1381-1386); a mode
// the table lacks is refused. A write outside manual is refused as well
// (assumed: rocm_smi_lib switches to manual before it writes the mode,
// rocm_smi.cc:1807-1840, and the emulation holds every writer to that).
func (n *Node) dpmProfileHandler(d *DPMSpec, st *dpmState, rel string) (func(string) (string, string), func() string) {
	modes := d.PowerProfiles.Modes.V
	handle := func(v string) (string, string) {
		if v == "" {
			return EventParseError, modes[st.profile]
		}
		if st.level != dpmManual {
			return n.rejectWrite(rel, ErrnoEINVAL), modes[st.profile]
		}
		i, err := strconv.ParseInt(v[:1], 0, 64)
		if err != nil || i < 0 || int(i) >= len(modes) {
			return n.rejectWrite(rel, ErrnoEINVAL), modes[st.profile]
		}
		st.profile = int(i)
		return EventAccepted, modes[st.profile]
	}
	return handle, func() string { return st.profileText(d) }
}

// dpmPolicyHandler is a pm_policy attribute: exactly one number, an index
// of the attribute's policies (amdgpu_pm.c:2149-2194; an index outside the
// list is refused, assumed EINVAL).
func (n *Node) dpmPolicyHandler(a *PMPolicyAttrSpec, cur *int, rel string) (func(string) (string, string), func() string) {
	names := a.Policies.V
	handle := func(v string) (string, string) {
		f := strings.Fields(v)
		if len(f) == 0 {
			return EventParseError, names[*cur]
		}
		i, err := strconv.ParseInt(f[0], 0, 64)
		if len(f) != 1 || err != nil || i < 0 || int(i) >= len(names) {
			return n.rejectWrite(rel, ErrnoEINVAL), names[*cur]
		}
		*cur = int(i)
		return EventAccepted, names[*cur]
	}
	return handle, func() string { return dpmPolicyText(a, *cur) }
}

// deriveGPUDPM recomputes each AMD GPU's clock state from its level and
// load: a GPU whose own work is memory-active raises its data path and
// restarts the hold, and gpuClockState gets the clocks, bounds and preset
// the level gives.
func (n *Node) deriveGPUDPM() {
	gs := n.profile.GPUs
	if gs == nil || gs.DPM == nil {
		return
	}
	d := gs.DPM
	for _, g := range n.gpus {
		dp := &g.dpm
		if !dp.on {
			continue
		}
		u := n.gpuUtil(g.index)
		dp.memActive = u*n.load.MemoryIntensity >= dpmMemoryActive
		if dp.memActive {
			dp.holdLeft = d.AutoHoldS.V
		}
		dp.view = dp.st.view(d, u >= dpmBusyUtil, dp.memActive || dp.holdLeft > 1e-9)
		v := dp.view
		c := &g.clock
		c.modelled = true
		c.native = dp.st.level
		c.preset, _ = governor.ReverseAMD(dp.facts, dp.st.level, [2]int{int(dp.st.od.sclkMin), int(dp.st.od.sclkMax)})
		c.computeFloorMHz, c.computeCeilMHz, c.computeMaxMHz = v.floorMHz, v.ceilMHz, v.maxMHz
		c.memFloorMHz, c.memCeilMHz = v.memFloorMHz, v.memCeilMHz
		c.dataPath, c.dataPathAuto, c.holdS, c.gatingOff = v.dataPath, v.dataPathAuto, d.AutoHoldS.V, v.gatingOff
		// The maximum in effect is what the level gives a busy GPU with its
		// data path raised; the hardware maximum is the top of each table.
		t, top := d.Tables, dp.st.view(d, true, true)
		c.setDomain(semconv.ClockDomainGraphics, float64(v.sclk), float64(t.Sclk.MaxMHz.V), float64(top.sclk))
		c.setDomain(semconv.ClockDomainMemory, float64(v.mclk), float64(dpmLast(t.Mclk.V)), float64(top.mclk))
		if len(t.Fclk.V) > 0 {
			c.setDomain(semconv.ClockDomainFabric, float64(v.fclk), float64(dpmLast(t.Fclk.V)), float64(top.fclk))
		}
		if len(t.Socclk.V) > 0 {
			c.setDomain(semconv.ClockDomainSoC, float64(v.socclk), float64(dpmLast(t.Socclk.V)), float64(top.socclk))
		}
	}
}

// substepGPUDPM runs down the hold of every GPU whose own memory-active
// work has stopped by one substep of h seconds.
func (n *Node) substepGPUDPM(h float64) {
	for _, g := range n.gpus {
		if dp := &g.dpm; dp.on && !dp.memActive {
			dp.holdLeft = math.Max(0, dp.holdLeft-h)
		}
	}
}

// writeGPUDPM writes the pp_dpm_* files whose stars moved.
func (n *Node) writeGPUDPM() error {
	gs := n.profile.GPUs
	if gs == nil || gs.DPM == nil {
		return nil
	}
	var errs []error
	for _, g := range n.gpus {
		dp := &g.dpm
		if !dp.on {
			continue
		}
		for _, f := range dp.view.files(gs.DPM) {
			if sf := dp.files[f.name]; sf != nil {
				if err := n.writeS(sf, f.content); err != nil {
					errs = append(errs, fmt.Errorf("hwemu: write %s: %w", sf.rel, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}
