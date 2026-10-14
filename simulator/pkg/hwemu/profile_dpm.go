package hwemu

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// AMD GPU DPM emulation, profile side: the checks of gpus.dpm and the
// amdgpu files it renders (power_dpm_force_performance_level, pp_dpm_*,
// pp_od_clk_voltage, pp_power_profile_mode, pm_policy/). Validate and Render
// call these hooks.

// dpmLevelWords are the words power_dpm_force_performance_level parses, in
// the order the kernel compares their prefixes
// (amdgpu_pm.c v6.17:365-386); any other write fails with EINVAL.
var dpmLevelWords = []string{
	dpmAuto, dpmLow, dpmHigh, dpmManual, dpmProfileExit, dpmProfileStandard,
	dpmProfileMinSclk, dpmProfileMinMclk, dpmProfilePeak, dpmPerfDeterminism,
}

// The amdgpu performance levels (amdgpu_pm.c v6.17:343-351).
const (
	dpmAuto            = "auto"
	dpmLow             = "low"
	dpmHigh            = "high"
	dpmManual          = "manual"
	dpmProfileExit     = "profile_exit"
	dpmProfileStandard = "profile_standard"
	dpmProfileMinSclk  = "profile_min_sclk"
	dpmProfileMinMclk  = "profile_min_mclk"
	dpmProfilePeak     = "profile_peak"
	dpmPerfDeterminism = "perf_determinism"
)

// The overdrive bounds gpus.dpm.od.writable may list: the sclk minimum and
// maximum ("s 0", "s 1") and the uclk minimum and maximum ("m 0", "m 1").
const (
	dpmODSclkMin = "sclkMin"
	dpmODSclkMax = "sclkMax"
	dpmODUclkMin = "uclkMin"
	dpmODUclkMax = "uclkMax"
)

// The DPM files of an amdgpu device besides the level, overdrive and power
// profile files layout names, and the pm_policy attributes.
const (
	dpmFileSclk        = "pp_dpm_sclk"
	dpmFileMclk        = "pp_dpm_mclk"
	dpmFileFclk        = "pp_dpm_fclk"
	dpmFileSocclk      = "pp_dpm_socclk"
	dpmFilePCIe        = "pp_dpm_pcie"
	dpmPolicySocPState = "soc_pstate"
	dpmPolicyXGMIPLPD  = "xgmi_plpd"
)

// What the fake rocm-smi and amd-smi learn from the kernel's answer to a
// write, which a file tree cannot give, lives under state/amdgpu/card<N>/
// next to gfx_version (package smi reads them): perf_levels lists each level
// word, followed by the errno of a word the firmware refuses; od_writable the
// overdrive bounds a write may set; unprivileged_set_noop is 1 when a level
// set through rocm-smi without root changes nothing and exits 0.
const (
	dpmStatePerfLevels = "perf_levels"
	dpmStateODWritable = "od_writable"
	dpmStateUnprivNoop = "unprivileged_set_noop"
)

// dpmProfileLevel reports whether level is one of the profile_* levels that
// pin clocks and turn idle gating off (amdgpu_smu.c v6.17:2282-2301).
func dpmProfileLevel(level string) bool {
	switch level {
	case dpmProfileStandard, dpmProfileMinSclk, dpmProfileMinMclk, dpmProfilePeak:
		return true
	}
	return false
}

// renderValidateGPUDPM returns every problem with p.GPUs.DPM: the block on a
// GPU that is not AMD; level words the kernel does not parse, a word both
// accepted and refused, an errno that is not one of Errnos, a default that is
// not accepted; clock tables that are not ascending; a profile level without
// the clocks it pins; overdrive facts on a family without overdrive; a power
// profile or pm_policy default outside its list.
func renderValidateGPUDPM(p *Profile) []error {
	if p.GPUs == nil || p.GPUs.DPM == nil {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: gpus.dpm."+format, append([]any{p.Name}, args...)...))
	}
	d := p.GPUs.DPM
	if p.GPUs.Vendor != "amd" {
		fail("is set on vendor %q; amdgpu DPM is AMD only", p.GPUs.Vendor)
		return errs
	}
	if len(d.Levels.Accept.V) == 0 {
		fail("levels.accept is empty")
	}
	for _, w := range d.Levels.Accept.V {
		if !slices.Contains(dpmLevelWords, w) {
			fail("levels.accept %q is not a power_dpm_force_performance_level word", w)
		}
	}
	rejected := make([]string, 0, len(d.Levels.Reject))
	for w := range d.Levels.Reject {
		rejected = append(rejected, w)
	}
	slices.Sort(rejected)
	for _, w := range rejected {
		e := d.Levels.Reject[w]
		switch {
		case !slices.Contains(dpmLevelWords, w):
			fail("levels.reject %q is not a power_dpm_force_performance_level word", w)
		case slices.Contains(d.Levels.Accept.V, w):
			fail("levels.reject %q is also in levels.accept", w)
		}
		if !slices.Contains(Errnos(), e.V) {
			fail("levels.reject %q has errno %q, want one of %s", w, e.V, strings.Join(Errnos(), ", "))
		}
	}
	if !slices.Contains(d.Levels.Accept.V, d.Default.V) || d.Default.V == dpmProfileExit {
		fail("default %q is not a level in levels.accept", d.Default.V)
	}

	t := d.Tables
	if t.Sclk.MinMHz.V <= 0 || t.Sclk.MaxMHz.V <= t.Sclk.MinMHz.V {
		fail("tables.sclk needs 0 < minMHz < maxMHz, got %d and %d", t.Sclk.MinMHz.V, t.Sclk.MaxMHz.V)
	}
	for _, tb := range []struct {
		name string
		v    []int64
	}{{"mclk", t.Mclk.V}, {"fclk", t.Fclk.V}, {"socclk", t.Socclk.V}} {
		for i, v := range tb.v {
			if v <= 0 || (i > 0 && v <= tb.v[i-1]) {
				fail("tables.%s %v is not ascending and positive", tb.name, tb.v)
				break
			}
		}
	}
	for i, s := range t.PCIe.V {
		if strings.TrimSpace(s) == "" {
			fail("tables.pcie entry %d is empty", i)
		}
	}
	if len(t.Mclk.V) == 0 {
		fail("tables.mclk is empty")
	}

	for _, ps := range []struct {
		level string
		name  string
		s     DPMPStateSpec
	}{{dpmProfileStandard, "standard", d.PState.Standard}, {dpmProfilePeak, "peak", d.PState.Peak}} {
		if !slices.Contains(d.Levels.Accept.V, ps.level) {
			continue
		}
		if v := ps.s.SclkMHz.V; v < t.Sclk.MinMHz.V || v > t.Sclk.MaxMHz.V {
			fail("pstate.%s.sclkMHz %d is outside tables.sclk", ps.name, v)
		}
		for _, c := range []struct {
			name  string
			v     int64
			table []int64
		}{{"mclkMHz", ps.s.MclkMHz.V, t.Mclk.V}, {"fclkMHz", ps.s.FclkMHz.V, t.Fclk.V}, {"socclkMHz", ps.s.SocclkMHz.V, t.Socclk.V}} {
			if len(c.table) > 0 && !slices.Contains(c.table, c.v) {
				fail("pstate.%s.%s %d is not an entry of its table %v", ps.name, c.name, c.v, c.table)
			}
		}
	}

	if od := d.OD; od != nil {
		switch {
		case !od.Enabled.V && len(od.Writable.V) > 0:
			fail("od.writable is set but od.enabled is false; overdrive facts belong to families that have it")
		case od.Enabled.V && len(od.Writable.V) == 0:
			fail("od.enabled needs od.writable")
		case od.Enabled.V && !slices.Contains(d.Levels.Accept.V, dpmManual) && !slices.Contains(d.Levels.Accept.V, dpmPerfDeterminism):
			fail("od.enabled needs manual or perf_determinism in levels.accept, the only levels overdrive edits are allowed in")
		}
		for _, w := range od.Writable.V {
			if !slices.Contains([]string{dpmODSclkMin, dpmODSclkMax, dpmODUclkMin, dpmODUclkMax}, w) {
				fail("od.writable %q is not sclkMin, sclkMax, uclkMin or uclkMax", w)
			}
		}
	}
	if pp := d.PowerProfiles; pp != nil {
		if len(pp.Modes.V) == 0 || len(pp.Modes.V) > 10 {
			fail("powerProfiles.modes needs 1 to 10 modes, got %d", len(pp.Modes.V))
		}
		if !slices.Contains(pp.Modes.V, pp.Default.V) {
			fail("powerProfiles.default %q is not in powerProfiles.modes", pp.Default.V)
		}
	}
	if pm := d.PMPolicy; pm != nil {
		for _, a := range []struct {
			name string
			s    *PMPolicyAttrSpec
		}{{dpmPolicySocPState, pm.SocPState}, {dpmPolicyXGMIPLPD, pm.XGMIPLPD}} {
			if a.s == nil {
				continue
			}
			if len(a.s.Policies.V) == 0 {
				fail("pmPolicy.%s.policies is empty", a.name)
			} else if a.s.Default.V < 0 || a.s.Default.V >= len(a.s.Policies.V) {
				fail("pmPolicy.%s.default %d is not an index of its policies", a.name, a.s.Default.V)
			}
		}
	}
	if d.AutoHoldS.V < 0 {
		fail("autoHoldS %v is negative", d.AutoHoldS.V)
	}
	return errs
}

// renderGPUDPM writes the DPM files of every AMD GPU of p, in its PCI device
// directory: power_dpm_force_performance_level at the default level and
// pp_od_clk_voltage, pp_power_profile_mode and pm_policy/ where the family
// has them (A, 0644 as amdgpu creates them, amdgpu_pm.h v6.17:112-114), and
// pp_dpm_* as the emulator keeps them (S, also 0644; a level-mask write
// under manual is not emulated). Each GPU also gets the state the fake tools
// read (dpmStatePerfLevels and its neighbours).
func renderGPUDPM(b *renderBuilder, p *Profile, o RenderOptions) error {
	g := p.GPUs
	if g == nil || g.Vendor != "amd" || g.DPM == nil {
		return nil
	}
	d := g.DPM
	for i := 0; i < g.Count.V; i++ {
		bus, card := renderAMDGPUCard(p, i)
		dev := renderSys(layout.PCIDevDir(bus))
		st := newDPMState(d)
		v := st.view(d, false, false)
		entries := []renderEntry{{name: layout.AMDGPUPerfLevelFile, content: st.level + "\n", owner: layout.OwnerAgent, mode: 0o644}}
		for _, f := range v.files(d) {
			entries = append(entries, renderEntry{name: f.name, content: f.content, owner: layout.OwnerEmulator, mode: 0o644})
		}
		if d.OD != nil && d.OD.Enabled.V {
			entries = append(entries, renderEntry{name: layout.AMDGPUODFile, content: st.odText(d), owner: layout.OwnerAgent, mode: 0o644})
		}
		if d.PowerProfiles != nil {
			entries = append(entries, renderEntry{name: layout.AMDGPUPowerProfileFile, content: st.profileText(d), owner: layout.OwnerAgent, mode: 0o644})
		}
		if err := b.writeEntries(dev, entries); err != nil {
			return err
		}
		if pm := d.PMPolicy; pm != nil {
			var pe []renderEntry
			for _, a := range []struct {
				name string
				s    *PMPolicyAttrSpec
				cur  int
			}{{dpmPolicySocPState, pm.SocPState, st.socPState}, {dpmPolicyXGMIPLPD, pm.XGMIPLPD, st.xgmiPLPD}} {
				if a.s != nil {
					pe = append(pe, renderEntry{name: a.name, content: dpmPolicyText(a.s, a.cur), owner: layout.OwnerAgent, mode: 0o644})
				}
			}
			if err := b.writeEntries(renderSys(layout.AMDGPUPMPolicyDir(bus)), pe); err != nil {
				return err
			}
		}
		state := []renderEntry{{name: dpmStatePerfLevels, content: dpmPerfLevelsText(d), owner: layout.OwnerRender, mode: 0o444}}
		if d.OD != nil && d.OD.Enabled.V {
			state = append(state, renderEntry{name: dpmStateODWritable, content: strings.Join(d.OD.Writable.V, " ") + "\n", owner: layout.OwnerRender, mode: 0o444})
		}
		noop := "0\n"
		if d.UnprivilegedSetIsNoop.V {
			noop = "1\n"
		}
		state = append(state, renderEntry{name: dpmStateUnprivNoop, content: noop, owner: layout.OwnerRender, mode: 0o444})
		if err := b.writeEntries(path.Join(layout.StateDir, "amdgpu", fmt.Sprintf("card%d", card)), state); err != nil {
			return err
		}
	}
	return nil
}

// dpmPerfLevelsText is state perf_levels: every level word the kernel
// parses, one per line, followed by the errno when the family refuses it.
// A word the profile neither accepts nor refuses is refused with EINVAL, as
// the kernel's own fallback is (amdgpu_pm.c v6.17:394-398).
func dpmPerfLevelsText(d *DPMSpec) string {
	var b strings.Builder
	for _, w := range dpmLevelWords {
		b.WriteString(w)
		if e := dpmLevelErrno(d, w); e != "" {
			b.WriteString(" " + e)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// dpmLevelErrno is the errno a write of level gets, "" when the family
// accepts it.
func dpmLevelErrno(d *DPMSpec, level string) string {
	if slices.Contains(d.Levels.Accept.V, level) {
		return ""
	}
	if e, ok := d.Levels.Reject[level]; ok {
		return e.V
	}
	return ErrnoEINVAL
}
