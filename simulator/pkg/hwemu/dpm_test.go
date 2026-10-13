package hwemu

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// mi300xProfile is the Instinct MI300X profile, SMU 13.0.6.
const mi300xProfile = "amd-instinct-mi300x-8gpu"

// dpmRel is a file of AMD GPU i's PCI device, card i+1 behind the BMC's
// card0, as both AMD profiles lay them out.
func dpmRel(i int, name string) string {
	return path.Join(layout.SysDir, layout.PCIDevDir(0x10+i), name)
}

// dpmNode renders builtin profile name with a small CPU and starts a Node
// over it.
func dpmNode(t *testing.T, name string) (*Profile, *Tree, *Node) {
	t.Helper()
	p := ensureBuiltin(t, name)
	tr := renderTestTree(t, p)
	return p, tr, nodeNewNode(t, p, tr)
}

// dpmStar is the clock on the starred line of a pp_dpm_* file, as a probe
// reads it: the field after the index, without its unit.
func dpmStar(t *testing.T, tr *Tree, i int, name string) string {
	t.Helper()
	for _, line := range strings.Split(nodeRead(t, tr, dpmRel(i, name)), "\n") {
		if strings.Contains(line, "*") {
			f := strings.Fields(line)
			return strings.TrimSuffix(f[len(f)-2], "Mhz")
		}
	}
	t.Fatalf("%s of GPU %d has no starred line:\n%s", name, i, nodeRead(t, tr, dpmRel(i, name)))
	return ""
}

// dpmSetLevel writes level to GPU i and steps; it returns the level read
// back and the file's one event.
func dpmSetLevel(t *testing.T, tr *Tree, n *Node, i int, level string) (string, Event) {
	t.Helper()
	rel := dpmRel(i, layout.AMDGPUPerfLevelFile)
	nodeWrite(t, tr, rel, level)
	nodeStep(t, n, time.Second)
	return strings.TrimSpace(nodeRead(t, tr, rel)), nodeOneEvent(t, n, rel)
}

// GD01: the W7900 takes auto, low, high, manual and every profile_* level,
// profile_exit going back to the level a profile level was entered from;
// perf_determinism is refused with EINVAL, the file goes back to the level
// in effect and the event carries the errno (error.type einval).
func TestGD01W7900LevelsAndPerfDeterminismRejected(t *testing.T) {
	t.Parallel()
	_, tr, n := dpmNode(t, w7900Profile)
	for _, level := range []string{"low", "high", "manual", "profile_standard", "profile_min_sclk", "profile_min_mclk", "profile_peak", "auto"} {
		got, e := dpmSetLevel(t, tr, n, 1, level)
		if got != level || e.Kind != EventAccepted || e.Effective != level || e.Component != "gpu1" {
			t.Errorf("write %s: reads %q, event %+v; want it accepted", level, got, e)
		}
	}
	dpmSetLevel(t, tr, n, 1, "high")
	dpmSetLevel(t, tr, n, 1, "profile_peak")
	if got, e := dpmSetLevel(t, tr, n, 1, "profile_exit"); got != "high" || e.Kind != EventAccepted {
		t.Errorf("profile_exit after high then profile_peak: reads %q, event %+v; want back at high", got, e)
	}
	got, e := dpmSetLevel(t, tr, n, 1, "perf_determinism")
	if got != "high" || e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.Written != "perf_determinism" || e.Effective != "high" {
		t.Errorf("perf_determinism: reads %q, event %+v; want it reverted to high with %s", got, e, ErrnoEINVAL)
	}
	if got, e := dpmSetLevel(t, tr, n, 1, "turbo"); got != "high" || e.Kind != EventRejected || e.Errno != ErrnoEINVAL {
		t.Errorf("an unknown word: reads %q, event %+v; want %s", got, e, ErrnoEINVAL)
	}
}

// GD02: the MI300X takes auto, manual and perf_determinism and refuses high.
// The firmware returns EOPNOTSUPP (smu_v13_0_6_ppt.c v6.17:2006-2010), but
// the sysfs store fails every refusal with EINVAL (amdgpu_pm.c v6.17:394-398),
// so error.type is einval, not the eopnotsupp the design expected.
func TestGD02MI300XLevelsAndHighRejected(t *testing.T) {
	t.Parallel()
	_, tr, n := dpmNode(t, mi300xProfile)
	for _, level := range []string{"manual", "perf_determinism", "auto"} {
		if got, e := dpmSetLevel(t, tr, n, 3, level); got != level || e.Kind != EventAccepted {
			t.Errorf("write %s: reads %q, event %+v; want it accepted", level, got, e)
		}
	}
	for _, level := range []string{"high", "low", "profile_peak", "profile_standard"} {
		got, e := dpmSetLevel(t, tr, n, 3, level)
		if got != "auto" || e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.File != dpmRel(3, layout.AMDGPUPerfLevelFile) {
			t.Errorf("write %s: reads %q, event %+v; want it reverted to auto with %s", level, got, e, ErrnoEINVAL)
		}
	}
}

// GD03: under high the data-path domains star their maximum while the GPU
// idles, and the idle compute clock reads its minimum: idle gating stays.
func TestGD03HighHoldsTheDataPathAndKeepsIdleGating(t *testing.T) {
	t.Parallel()
	for _, name := range []string{w7900Profile, mi300xProfile} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, tr, n := dpmNode(t, name)
			nodeStep(t, n, time.Second)
			tb := p.GPUs.DPM.Tables
			if mclk := dpmStar(t, tr, 0, "pp_dpm_mclk"); mclk != itoa(tb.Mclk.V[0]) {
				t.Fatalf("auto idle mclk star %s, want the minimum %d", mclk, tb.Mclk.V[0])
			}
			if _, e := dpmSetLevel(t, tr, n, 0, "high"); e.Kind != EventAccepted {
				if name == mi300xProfile {
					return // MI300X refuses high (GD02); nothing more to check
				}
				t.Fatalf("high: event %+v", e)
			}
			for _, c := range []struct {
				file  string
				table []int64
			}{{"pp_dpm_mclk", tb.Mclk.V}, {"pp_dpm_fclk", tb.Fclk.V}, {"pp_dpm_socclk", tb.Socclk.V}} {
				if got, want := dpmStar(t, tr, 0, c.file), itoa(c.table[len(c.table)-1]); got != want {
					t.Errorf("high, idle: %s star %s, want the maximum %s", c.file, got, want)
				}
			}
			if got := dpmStar(t, tr, 0, "pp_dpm_sclk"); got != itoa(tb.Sclk.MinMHz.V) {
				t.Errorf("high, idle: sclk star %s, want the minimum %d (idle gating kept)", got, tb.Sclk.MinMHz.V)
			}
			c := n.gpus[0].clock
			if c.dataPath != phys.DataPathMax || c.dataPathAuto || c.gatingOff || c.preset != api.GPUGovernorPresetSustained || c.native != "high" {
				t.Errorf("high: clock state %+v, want data path max, forced, gating kept, preset sustained", c)
			}
			n.SetLoad(Load{GPUUtil: []float64{1}})
			nodeStep(t, n, time.Second)
			if got := dpmStar(t, tr, 0, "pp_dpm_sclk"); got != itoa(tb.Sclk.MaxMHz.V) {
				t.Errorf("high, busy: sclk star %s, want the maximum %d", got, tb.Sclk.MaxMHz.V)
			}
		})
	}
}

// GD04: W7900 profile_standard pins fclk at its minimum and mclk at its
// maximum (smu_v13_0_0_ppt.c v6.17:2351, 2355), so the data path is at its
// minimum.
func TestGD04W7900ProfileStandardPinsFclkLowMclkHigh(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, w7900Profile)
	if _, e := dpmSetLevel(t, tr, n, 2, "profile_standard"); e.Kind != EventAccepted {
		t.Fatalf("profile_standard: event %+v", e)
	}
	tb := p.GPUs.DPM.Tables
	if got := dpmStar(t, tr, 2, "pp_dpm_fclk"); got != itoa(tb.Fclk.V[0]) {
		t.Errorf("fclk star %s, want the minimum %d", got, tb.Fclk.V[0])
	}
	if got := dpmStar(t, tr, 2, "pp_dpm_mclk"); got != itoa(tb.Mclk.V[len(tb.Mclk.V)-1]) {
		t.Errorf("mclk star %s, want the maximum %d", got, tb.Mclk.V[len(tb.Mclk.V)-1])
	}
	if got := dpmStar(t, tr, 2, "pp_dpm_sclk"); got != itoa(p.GPUs.DPM.PState.Standard.SclkMHz.V) {
		t.Errorf("sclk star %s, want pstate.standard %d", got, p.GPUs.DPM.PState.Standard.SclkMHz.V)
	}
	if c := n.gpus[2].clock; c.dataPath != phys.DataPathMin || !c.gatingOff {
		t.Errorf("clock state %+v, want the data path at its minimum and gating off", c)
	}
}

// GD05: profile_peak sets every domain to its maximum and turns idle gating
// off, so an idle GPU's compute clock reads pstate.peak.
func TestGD05ProfilePeakPinsEveryDomainAndTurnsGatingOff(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, w7900Profile)
	if _, e := dpmSetLevel(t, tr, n, 0, "profile_peak"); e.Kind != EventAccepted {
		t.Fatalf("profile_peak: event %+v", e)
	}
	d := p.GPUs.DPM
	for file, want := range map[string]int64{
		"pp_dpm_sclk": d.PState.Peak.SclkMHz.V, "pp_dpm_mclk": d.PState.Peak.MclkMHz.V,
		"pp_dpm_fclk": d.PState.Peak.FclkMHz.V, "pp_dpm_socclk": d.PState.Peak.SocclkMHz.V,
	} {
		if got := dpmStar(t, tr, 0, file); got != itoa(want) {
			t.Errorf("idle under profile_peak: %s star %s, want %d", file, got, want)
		}
	}
	c := n.gpus[0].clock
	if !c.gatingOff || c.dataPath != phys.DataPathMax || c.preset != api.GPUGovernorPresetPeak {
		t.Errorf("clock state %+v, want gating off, data path max, preset peak", c)
	}
	if c.computeFloorMHz != float64(d.PState.Peak.SclkMHz.V) || c.computeCeilMHz != c.computeFloorMHz {
		t.Errorf("compute bounds [%v, %v], want both at pstate.peak %d", c.computeFloorMHz, c.computeCeilMHz, d.PState.Peak.SclkMHz.V)
	}
}

// GD07: under auto, the GPU's own memory-active work raises the data path;
// when it stops the fclk star holds autoHoldS and then falls, a lagging
// sample rather than the state of the moment.
func TestGD07AutoHoldsTheDataPathAfterOwnWorkThenFalls(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, w7900Profile)
	tb := p.GPUs.DPM.Tables
	lo, hi := itoa(tb.Fclk.V[0]), itoa(tb.Fclk.V[len(tb.Fclk.V)-1])
	n.SetLoad(Load{GPUUtil: []float64{0, 1}, MemoryIntensity: 0.5, GPUClass: "gpu.memory_bound"})
	nodeStep(t, n, time.Second)
	if got := dpmStar(t, tr, 1, "pp_dpm_fclk"); got != hi {
		t.Fatalf("memory-active work: fclk star %s, want %s", got, hi)
	}
	if got := dpmStar(t, tr, 0, "pp_dpm_fclk"); got != lo {
		t.Errorf("the idle neighbour: fclk star %s, want %s", got, lo)
	}
	n.SetLoad(Load{})
	hold := time.Duration(p.GPUs.DPM.AutoHoldS.V * float64(time.Second))
	nodeStep(t, n, hold-500*time.Millisecond)
	if got := dpmStar(t, tr, 1, "pp_dpm_fclk"); got != hi {
		t.Errorf("%v after the work stopped: fclk star %s, want it held at %s", hold-500*time.Millisecond, got, hi)
	}
	nodeStep(t, n, time.Second)
	if got := dpmStar(t, tr, 1, "pp_dpm_fclk"); got != lo {
		t.Errorf("past the hold: fclk star %s, want it back at %s", got, lo)
	}
	if c := n.gpus[1].clock; c.dataPath != phys.DataPathMin || !c.dataPathAuto || c.holdS != p.GPUs.DPM.AutoHoldS.V {
		t.Errorf("clock state %+v, want the data path at its minimum, following demand, hold %v", c, p.GPUs.DPM.AutoHoldS.V)
	}
}

// GD08: pp_od_clk_voltage is absent on the W7900, whose overdrive is off,
// and present on the MI300X. There, under manual, "s 1 1500" then "c"
// lowers the sclk maximum, also when both come in one write, and the preset
// reads efficient; under auto the edit is refused with EINVAL; going back to
// auto restores the range.
func TestGD08OverdriveOnlyUnderManual(t *testing.T) {
	t.Parallel()
	_, wtr, _ := dpmNode(t, w7900Profile)
	if _, err := os.Lstat(filepath.Join(wtr.Root, dpmRel(0, layout.AMDGPUODFile))); !os.IsNotExist(err) {
		t.Errorf("W7900 pp_od_clk_voltage: %v, want it absent", err)
	}

	p, tr, n := dpmNode(t, mi300xProfile)
	od := dpmRel(1, layout.AMDGPUODFile)
	sMin, sMax := p.GPUs.DPM.Tables.Sclk.MinMHz.V, p.GPUs.DPM.Tables.Sclk.MaxMHz.V
	full := nodeRead(t, tr, od)
	if !strings.HasPrefix(full, "OD_SCLK:\n0: "+itoa(sMin)+"Mhz\n1: "+itoa(sMax)+"Mhz\n") {
		t.Fatalf("pp_od_clk_voltage at boot:\n%s", full)
	}

	nodeWrite(t, tr, od, "s 1 1500")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, od); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || nodeRead(t, tr, od) != full {
		t.Errorf("an edit under auto: event %+v, file\n%s\nwant it refused with %s and the file unchanged", e, nodeRead(t, tr, od), ErrnoEINVAL)
	}

	dpmSetLevel(t, tr, n, 1, "manual")
	nodeWrite(t, tr, od, "s 1 1500")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, od); e.Kind != EventAccepted || nodeRead(t, tr, od) != full {
		t.Errorf("an edit without a commit: event %+v, file\n%s\nwant it taken and the committed range unchanged", e, nodeRead(t, tr, od))
	}
	nodeWrite(t, tr, od, "c")
	nodeStep(t, n, time.Second)
	want := strings.Replace(full, "1: "+itoa(sMax)+"Mhz", "1: 1500Mhz", 1)
	if got := nodeRead(t, tr, od); got != want {
		t.Errorf("after c:\n%s\nwant\n%s", got, want)
	}
	if c := n.gpus[1].clock; c.computeCeilMHz != 1500 || c.preset != api.GPUGovernorPresetEfficient || c.native != "manual" {
		t.Errorf("clock state %+v, want ceiling 1500, preset efficient, native manual", c)
	}
	n.SetLoad(Load{GPUUtil: []float64{0, 1}})
	nodeStep(t, n, time.Second)
	if got := dpmStar(t, tr, 1, "pp_dpm_sclk"); got != "1500" {
		t.Errorf("busy under the 1500 MHz ceiling: sclk star %s, want 1500", got)
	}

	dpmSetLevel(t, tr, n, 1, "auto")
	if got := nodeRead(t, tr, od); got != full {
		t.Errorf("back at auto:\n%s\nwant the table range\n%s", got, full)
	}
	dpmSetLevel(t, tr, n, 1, "manual")
	nodeWrite(t, tr, od, "s 1 1200\nc\n")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, od); !strings.Contains(got, "1: 1200Mhz") {
		t.Errorf("edit and commit in one write:\n%s\nwant the maximum at 1200", got)
	}
	nodeWrite(t, tr, od, "s 1 99999\nc")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, od); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || !strings.Contains(nodeRead(t, tr, od), "1: 1200Mhz") {
		t.Errorf("a maximum above the table: event %+v, file\n%s\nwant %s and the range kept", e, nodeRead(t, tr, od), ErrnoEINVAL)
	}

	// SG07 through the emulator: amd-smi set -L sclk max 1500 on a GPU at
	// auto leaves it at manual with the overdrive maximum at 1500.
	if code, out, errOut := w7900Tool(t, tr, "amd-smi", "set", "-g", "2", "-L", "sclk", "max", "1500"); code != 0 || !strings.Contains(out, "Successfully changed max of sclk to 1500MHz") {
		t.Fatalf("amd-smi set -L sclk max 1500 = %d, stdout %q, stderr %q", code, out, errOut)
	}
	nodeStep(t, n, time.Second)
	if lvl, got := strings.TrimSpace(nodeRead(t, tr, dpmRel(2, layout.AMDGPUPerfLevelFile))), nodeRead(t, tr, dpmRel(2, layout.AMDGPUODFile)); lvl != "manual" || got != want {
		t.Errorf("after amd-smi set -L: level %q, overdrive\n%s\nwant manual and\n%s", lvl, got, want)
	}
}

// GD09: the MI300X refuses an uclk minimum ("Setting min UCLK level is not
// supported", smu_v13_0_6_ppt.c v6.17:2174) and takes an uclk maximum.
func TestGD09MI300XRefusesTheUclkMinimum(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, mi300xProfile)
	od := dpmRel(0, layout.AMDGPUODFile)
	dpmSetLevel(t, tr, n, 0, "manual")
	before := nodeRead(t, tr, od)
	nodeWrite(t, tr, od, "m 0 1100")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, od); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || nodeRead(t, tr, od) != before {
		t.Errorf("m 0: event %+v, want it refused with %s", e, ErrnoEINVAL)
	}
	mclk := p.GPUs.DPM.Tables.Mclk.V
	nodeWrite(t, tr, od, "m 1 "+itoa(mclk[1])+"\nc")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, od); !strings.Contains(got, "OD_MCLK:\n0: "+itoa(mclk[0])+"Mhz\n1: "+itoa(mclk[1])+"Mhz\n") {
		t.Errorf("after m 1 and c:\n%s\nwant the uclk maximum at %d", got, mclk[1])
	}
	n.SetLoad(Load{GPUUtil: []float64{1}, MemoryIntensity: 1})
	nodeStep(t, n, time.Second)
	if got := dpmStar(t, tr, 0, "pp_dpm_mclk"); got != itoa(mclk[1]) {
		t.Errorf("memory-active under the uclk maximum: mclk star %s, want %d", got, mclk[1])
	}
}

// GD10: pp_power_profile_mode exists on the W7900 only. The star marks the
// user's choice; a write outside manual is refused (assumed).
func TestGD10PowerProfileOnW7900Only(t *testing.T) {
	t.Parallel()
	_, mtr, _ := dpmNode(t, mi300xProfile)
	if _, err := os.Lstat(filepath.Join(mtr.Root, dpmRel(0, layout.AMDGPUPowerProfileFile))); !os.IsNotExist(err) {
		t.Errorf("MI300X pp_power_profile_mode: %v, want it absent", err)
	}
	p, tr, n := dpmNode(t, w7900Profile)
	pf := dpmRel(0, layout.AMDGPUPowerProfileFile)
	boot := nodeRead(t, tr, pf)
	if !strings.Contains(boot, " 0 BOOTUP_DEFAULT*:\n") || !strings.Contains(boot, " 5        COMPUTE :\n") {
		t.Fatalf("pp_power_profile_mode at boot:\n%s", boot)
	}
	nodeWrite(t, tr, pf, "5")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, pf); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || nodeRead(t, tr, pf) != boot {
		t.Errorf("a write under auto: event %+v, want it refused", e)
	}
	dpmSetLevel(t, tr, n, 0, "manual")
	nodeWrite(t, tr, pf, "5")
	nodeStep(t, n, time.Second)
	got := nodeRead(t, tr, pf)
	if e := nodeOneEvent(t, n, pf); e.Kind != EventAccepted || e.Effective != "COMPUTE" || !strings.Contains(got, " 5        COMPUTE*:\n") || strings.Contains(got, "BOOTUP_DEFAULT*") {
		t.Errorf("COMPUTE under manual: event %+v, file\n%s", e, got)
	}
	nodeWrite(t, tr, pf, "9")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, pf); e.Kind != EventRejected || len(p.GPUs.DPM.PowerProfiles.Modes.V) > 9 {
		t.Errorf("mode 9 of %d: event %+v, want it refused", len(p.GPUs.DPM.PowerProfiles.Modes.V), e)
	}
}

// GD11: the MI300X pm_policy attributes take an index of their policies and
// refuse anything else.
func TestGD11MI300XPMPolicy(t *testing.T) {
	t.Parallel()
	_, tr, n := dpmNode(t, mi300xProfile)
	soc, plpd := dpmRel(2, "pm_policy/soc_pstate"), dpmRel(2, "pm_policy/xgmi_plpd")
	if got := nodeRead(t, tr, soc); got != "0 : soc_pstate_default*\n1 : soc_pstate_0\n2 : soc_pstate_1\n3 : soc_pstate_2\n" {
		t.Fatalf("soc_pstate at boot:\n%s", got)
	}
	nodeWrite(t, tr, soc, "2")
	nodeWrite(t, tr, plpd, "0")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, soc); !strings.Contains(got, "2 : soc_pstate_1*\n") || strings.Count(got, "*") != 1 {
		t.Errorf("soc_pstate after 2:\n%s", got)
	}
	if got := nodeRead(t, tr, plpd); got != "0 : plpd_disallow*\n1 : plpd_default\n2 : plpd_optimized\n" {
		t.Errorf("xgmi_plpd after 0:\n%s", got)
	}
	for _, v := range []string{"4", "-1", "1 2", "x"} {
		before := nodeRead(t, tr, soc)
		nodeWrite(t, tr, soc, v)
		nodeStep(t, n, time.Second)
		if e := nodeOneEvent(t, n, soc); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || nodeRead(t, tr, soc) != before {
			t.Errorf("soc_pstate %q: event %+v, want it refused and the file unchanged", v, e)
		}
	}
}

// GD12: the level, the overdrive range and the power profile survive a
// resume: Ensure over the live tree, then a new Node, starts where the last
// one stopped.
func TestGD12LevelsSurviveEnsureAndANewNode(t *testing.T) {
	t.Parallel()
	for _, name := range []string{w7900Profile, mi300xProfile} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ensureBuiltin(t, name)
			root := filepath.Join(t.TempDir(), "node")
			o := RenderOptions{NodeName: "hwemu-test-node"}
			tr, _, err := Ensure(p, root, o)
			if err != nil {
				t.Fatal(err)
			}
			n := nodeNewNode(t, p, tr)
			var wantLevel, wantOD, wantProfile string
			if name == w7900Profile {
				dpmSetLevel(t, tr, n, 1, "manual")
				nodeWrite(t, tr, dpmRel(1, layout.AMDGPUPowerProfileFile), "5")
				nodeStep(t, n, time.Second)
				dpmSetLevel(t, tr, n, 1, "high")
				wantLevel, wantProfile = "high", nodeRead(t, tr, dpmRel(1, layout.AMDGPUPowerProfileFile))
			} else {
				dpmSetLevel(t, tr, n, 1, "manual")
				nodeWrite(t, tr, dpmRel(1, layout.AMDGPUODFile), "s 1 1300\nc")
				nodeStep(t, n, time.Second)
				wantLevel, wantOD = "manual", nodeRead(t, tr, dpmRel(1, layout.AMDGPUODFile))
			}
			before := n.State()

			again, _, err := Ensure(p, root, o)
			if err != nil {
				t.Fatal(err)
			}
			m := nodeNewNode(t, p, again)
			nodeStep(t, m, time.Second)
			if got := strings.TrimSpace(nodeRead(t, again, dpmRel(1, layout.AMDGPUPerfLevelFile))); got != wantLevel {
				t.Errorf("level after the resume %q, want %q", got, wantLevel)
			}
			if wantOD != "" && nodeRead(t, again, dpmRel(1, layout.AMDGPUODFile)) != wantOD {
				t.Errorf("overdrive after the resume:\n%s\nwant\n%s", nodeRead(t, again, dpmRel(1, layout.AMDGPUODFile)), wantOD)
			}
			if wantProfile != "" && nodeRead(t, again, dpmRel(1, layout.AMDGPUPowerProfileFile)) != wantProfile {
				t.Errorf("power profile after the resume:\n%s", nodeRead(t, again, dpmRel(1, layout.AMDGPUPowerProfileFile)))
			}
			for _, e := range m.State().Events {
				t.Errorf("the resumed node rewrote a file: %+v", e)
			}
			after := m.State()
			for i, c := range after.Components {
				if c.Type == semconv.HwTypeGPU && (c.GovernorNative != before.Components[i].GovernorNative || c.GovernorPreset != before.Components[i].GovernorPreset) {
					t.Errorf("%s governor %q/%q after the resume, want %q/%q", c.ID, c.GovernorNative, c.GovernorPreset, before.Components[i].GovernorNative, before.Components[i].GovernorPreset)
				}
			}
		})
	}
}

// GD13: profile validation of gpus.dpm: the default level must be accepted,
// tables ascend, overdrive facts belong to a family with overdrive, and the
// block is AMD only.
func TestGD13DPMValidation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{w7900Profile, mi300xProfile} {
		if err := renderBuiltin(t, name).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"default not a level", func(p *Profile) { p.GPUs.DPM.Default.V = "turbo" }, "gpus.dpm.default"},
		{"default refused", func(p *Profile) { p.GPUs.DPM.Default.V = "perf_determinism" }, "gpus.dpm.default"},
		{"fclk descending", func(p *Profile) { p.GPUs.DPM.Tables.Fclk.V = []int64{1800, 1200} }, "gpus.dpm.tables.fclk"},
		{"sclk range empty", func(p *Profile) { p.GPUs.DPM.Tables.Sclk.MaxMHz.V = 100 }, "gpus.dpm.tables.sclk"},
		{"od writable without od", func(p *Profile) {
			p.GPUs.DPM.OD.Writable = Fact[[]string]{V: []string{"sclkMax"}, Src: "assumed:test"}
		}, "gpus.dpm.od.writable"},
		{"word both ways", func(p *Profile) { p.GPUs.DPM.Levels.Reject["high"] = Fact[string]{V: ErrnoEINVAL, Src: "assumed:test"} }, "gpus.dpm.levels.reject"},
		{"unknown errno", func(p *Profile) {
			p.GPUs.DPM.Levels.Reject["perf_determinism"] = Fact[string]{V: "ENOSPC", Src: "assumed:test"}
		}, "gpus.dpm.levels.reject"},
		{"pstate outside its table", func(p *Profile) { p.GPUs.DPM.PState.Peak.FclkMHz.V = 2000 }, "gpus.dpm.pstate.peak.fclkMHz"},
		{"power profile default", func(p *Profile) { p.GPUs.DPM.PowerProfiles.Default.V = "TURBO" }, "gpus.dpm.powerProfiles.default"},
		{"nvidia", func(p *Profile) { p.GPUs.Vendor = "nvidia" }, "gpus.dpm"},
	} {
		p := renderBuiltin(t, w7900Profile)
		tc.mutate(p)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", tc.name, err, tc.want)
		}
	}
	p := renderBuiltin(t, mi300xProfile)
	p.GPUs.DPM.PMPolicy.SocPState.Default.V = 4
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "gpus.dpm.pmPolicy.soc_pstate.default") {
		t.Errorf("soc_pstate default 4 of 4 policies: err = %v", err)
	}
}

// GD14: the layout goldens of both AMD profiles list the DPM files with
// their classes: the level, overdrive, power profile and pm_policy files
// agent-writable, the pp_dpm_* files the emulator's, the tools' state
// render-owned, and the bus/pci/devices entry of each GPU.
func TestGD14LayoutGoldensListTheDPMFiles(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "layout", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dev := func(name string) string { return dpmRel(0, name) }
	w, m := read(w7900Profile), read(mi300xProfile)
	for _, line := range []string{
		dev("power_dpm_force_performance_level") + " A file 0644",
		dev("pp_dpm_sclk") + " S file 0644",
		dev("pp_dpm_fclk") + " S file 0644",
		dev("pp_dpm_socclk") + " S file 0644",
		"state/amdgpu/card1/perf_levels R file 0444",
		"sys/bus/pci/devices/0000:10:00.0 R link 0777 -> ../../../devices/pci0000:10/0000:10:00.0",
	} {
		for golden, text := range map[string]string{w7900Profile: w, mi300xProfile: m} {
			if !strings.Contains(text, line+"\n") {
				t.Errorf("%s golden lacks %q", golden, line)
			}
		}
	}
	for _, c := range []struct {
		text, golden, line string
		want               bool
	}{
		{w, w7900Profile, dev("pp_power_profile_mode") + " A file 0644", true},
		{w, w7900Profile, dev("pp_dpm_pcie") + " S file 0644", true},
		{w, w7900Profile, dev("pp_od_clk_voltage"), false},
		{w, w7900Profile, dev("pm_policy"), false},
		{m, mi300xProfile, dev("pp_od_clk_voltage") + " A file 0644", true},
		{m, mi300xProfile, dev("pm_policy/soc_pstate") + " A file 0644", true},
		{m, mi300xProfile, dev("pm_policy/xgmi_plpd") + " A file 0644", true},
		{m, mi300xProfile, dev("pp_power_profile_mode"), false},
	} {
		if strings.Contains(c.text, c.line) != c.want {
			t.Errorf("%s golden lists %q: %v, want %v", c.golden, c.line, !c.want, c.want)
		}
	}
}

// State reports each AMD GPU's clocks per domain in Hz with its native level
// and preset, from the first step on.
func TestDPMStateReportsClocksAndGovernor(t *testing.T) {
	t.Parallel()
	p, _, n := dpmNode(t, w7900Profile)
	nodeStep(t, n, time.Second)
	tb := p.GPUs.DPM.Tables
	for _, c := range n.State().Components {
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		want := map[string]int64{
			semconv.ClockDomainGraphics: tb.Sclk.MinMHz.V * 1e6, semconv.ClockDomainMemory: tb.Mclk.V[0] * 1e6,
			semconv.ClockDomainFabric: tb.Fclk.V[0] * 1e6, semconv.ClockDomainSoC: tb.Socclk.V[0] * 1e6,
		}
		if len(c.ClockHz) != len(want) || c.GovernorNative != "auto" || c.GovernorPreset != api.GPUGovernorPresetAuto {
			t.Errorf("%s: clocks %v, governor %q/%q; want %v at auto", c.ID, c.ClockHz, c.GovernorNative, c.GovernorPreset, want)
		}
		for d, hz := range want {
			if c.ClockHz[d] != hz {
				t.Errorf("%s %s = %d Hz, want %d", c.ID, d, c.ClockHz[d], hz)
			}
		}
	}
}

func itoa(v int64) string { return nodeItoa(v) }
