package smi

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// rocmJSON decodes rocm-smi --json output: card<i> to key to string value.
// Decoding into string values fails on any non-string, which is the point:
// rocm-smi prints every value as a string.
func rocmJSON(t *testing.T, out string) map[string]map[string]string {
	t.Helper()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("output %q, want one JSON line", out)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return m
}

// No rocm-smi release has --showpowercap, so a call with it fails argparse
// with exit 2, a usage error on stderr and an empty stdout.
func TestS04ShowPowerCapFailsWithUnrecognizedArguments(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	code, stdout, stderr := n.main("", rocmShowPowerCap...)
	if code != 2 || stdout != "" {
		t.Fatalf("exit %d, stdout %q; want 2 and empty", code, stdout)
	}
	if !strings.HasPrefix(stderr, "usage: rocm-smi [-h]") || !strings.Contains(stderr, "\nrocm-smi: error: unrecognized arguments: --showpowercap\n") {
		t.Fatalf("stderr %q, want usage and the unrecognized --showpowercap", stderr)
	}
	out, err := n.run(rocmShowPowerCap...)
	if exitCode(t, err) != 2 || !strings.Contains(string(out), "error: unrecognized arguments: --showpowercap") {
		t.Fatalf("Runner: %v, %q; want exit status 2 and the argparse error", err, out)
	}
}

// --setpoweroverdrive with an empty stdin: a value in range is written; a
// value above max is refused with exit 0; a value above max(current,
// default) prompts and fails on the empty stdin; a missing device exits 0;
// 0 restores the default.
func TestS06SetPowerOverDriveOutcomes(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	if out, err := n.run(rocmSetPowerOverDrive(0, 500)...); err != nil {
		t.Fatalf("-d 0 500: %v (%q)", err, out)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "500000000" {
		t.Fatalf("after 500: power1_cap = %s, want 500000000", got)
	}
	if got := n.read(hwmonRel(1, "power1_cap")); got != "750000000" {
		t.Fatalf("-d 0 changed device 1: power1_cap = %s", got)
	}

	code, _, stderr := n.main("", rocmSetPowerOverDrive(0, 800)...)
	if code != 0 || !strings.Contains(stderr, "ERROR: GPU[0]\t: Unable to set Power OverDrive") ||
		!strings.Contains(stderr, "Value cannot be greater than: 750W") {
		t.Fatalf("800 above max: exit %d, stderr %q; want 0 and the refusal", code, stderr)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "500000000" {
		t.Fatalf("after refused 800: power1_cap = %s, want 500000000", got)
	}

	code, stdout, stderr := n.main("", rocmSetPowerOverDrive(9, 500)...)
	if code != 0 || stdout != "" || stderr != "WARNING: No such device card9\n" {
		t.Fatalf("-d 9: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	if code, _, stderr := n.main("", "rocm-smi", "-d", "0", "--setpoweroverdrive", "0"); code != 0 || stderr != "" {
		t.Fatalf("0: exit %d, stderr %q", code, stderr)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "750000000" {
		t.Fatalf("after 0: power1_cap = %s, want the default 750000000", got)
	}

	// Default 600 below max 750, current 600: 700 is above max(current,
	// default), so rocm-smi asks, and stdin is empty.
	p := newMI300XNode(t, rocmVariantCurrent, "")
	p.write(hwmonRel(0, "power1_cap_default"), "600000000", 0o444)
	p.write(hwmonRel(0, "power1_cap"), "600000000", 0o644)
	out, err := p.run(rocmSetPowerOverDrive(0, 700)...)
	if exitCode(t, err) != 1 || !strings.Contains(string(out), "Do you accept these terms? [y/N] ") ||
		!strings.HasSuffix(string(out), "EOFError: EOF when reading a line\n") {
		t.Fatalf("700 with EOF stdin: %v, %q; want exit status 1 after the prompt", err, out)
	}
	if got := p.read(hwmonRel(0, "power1_cap")); got != "600000000" {
		t.Fatalf("after EOF: power1_cap = %s, want 600000000", got)
	}
	if code, _, stderr := p.main("n\n", rocmSetPowerOverDrive(0, 700)...); code != 1 || stderr != "Confirmation not given. Exiting without setting value\n" {
		t.Fatalf("answer n: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := p.main("", append(rocmSetPowerOverDrive(0, 700), "--autorespond", "y")...); code != 0 || stderr != "" {
		t.Fatalf("--autorespond y: exit %d, stderr %q", code, stderr)
	}
	if got := p.read(hwmonRel(0, "power1_cap")); got != "700000000" {
		t.Fatalf("after --autorespond y: power1_cap = %s, want 700000000", got)
	}
}

// --showproductname --json prints showProduct's keys in its order, and Card
// SKU is a VBIOS segment, not the product. -M is the current cap floored, not
// the maximum.
func TestS07ProductNameKeysOrderAndSKU(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	n.write(path.Join(layout.SysDir, layout.PCIDevDir(0x11), "vbios_version"), "113-D65209-0105-X", 0o444)
	code, stdout, stderr := n.main("", "rocm-smi", "--showproductname", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// The JSON is one line, card0 first; its keys in order are the quoted
	// strings followed by ": " up to the first closing brace.
	card0, _, _ := strings.Cut(strings.TrimPrefix(stdout, `{"card0": {`), "}")
	var keys []string
	for _, kv := range regexp.MustCompile(`"([^"]*)": "`).FindAllStringSubmatch(card0, -1) {
		keys = append(keys, kv[1])
	}
	want := []string{"Card Series", "Card Model", "Card Vendor", "Card SKU", "Subsystem ID", "Device Rev", "Node ID", "GUID", "GFX Version"}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("card0 keys %q, want %q", keys, want)
	}
	m := rocmJSON(t, stdout)
	for k, v := range map[string]string{
		"Card Series": "AMD Instinct MI300X",
		"Card Model":  "0x74a1",
		"Card Vendor": "Advanced Micro Devices, Inc. [AMD/ATI]",
		"Card SKU":    "X0000000",
	} {
		if m["card0"][k] != v {
			t.Errorf("card0 %q = %q, want %q", k, m["card0"][k], v)
		}
	}
	// -M is the current cap floored to whole watts, not the top of the range.
	n.write(hwmonRel(0, "power1_cap"), "300500000", 0o644)
	if _, stdout, _ := n.main("", "rocm-smi", "-d", "0", "-M", "--json"); rocmJSON(t, stdout)["card0"]["Max Graphics Package Power (W)"] != "300.0" {
		t.Errorf("-M with power1_cap 300500000: %q, want 300.0", stdout)
	}
	if got := m["card1"]["Card SKU"]; got != "N/A" {
		t.Errorf("card1 Card SKU with three dashes in vbios_version = %q, want N/A", got)
	}
	if len(m) != 8 {
		t.Errorf("%d cards, want the 8 AMD GPUs without the BMC VGA card0", len(m))
	}
}

// argparse accepts a unique prefix of a long option, and rejects one that
// several options share, as real rocm-smi does.
func TestS11PrefixAbbreviation(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	_, full, _ := n.main("", "rocm-smi", "--showproductname", "--json")
	code, short, stderr := n.main("", "rocm-smi", "--showprod", "--json")
	if code != 0 || stderr != "" || short != full {
		t.Fatalf("--showprod: exit %d, stderr %q, stdout %q; want the --showproductname output", code, stderr, short)
	}
	code, stdout, stderr := n.main("", "rocm-smi", "--showp", "--json")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "error: ambiguous option: --showp could match --showproductname, ") {
		t.Fatalf("--showp: exit %d, stdout %q, stderr %q; want an ambiguity error", code, stdout, stderr)
	}
}

// With device 1 the secondary die of device 0: a set on it is skipped with
// exit 0 while the primary still takes one, and -P shows it as N/A (Secondary
// die) (rocm_smi.py:1687-1689, 2707-2709 at 323ab1105dce). The primary's
// reading is the package power, which the emulator writes there; the tools
// print each device's own reading and never add dies (2690-2711), so a
// secondary that has a reading of its own is not counted twice.
func TestS13SecondaryDie(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, amdsmiVariant72)
	n.write("state/amdgpu/card1/energy_count", "123456", 0o444)
	n.write("state/amdgpu/card2/energy_count", "0", 0o444)
	n.write(hwmonRel(0, "power1_input"), "210000000", 0o444)
	n.remove(hwmonRel(1, "power1_input"))

	code, _, stderr := n.main("", rocmSetPowerOverDrive(1, 500)...)
	if code != 0 || stderr != "" {
		t.Fatalf("set on the secondary: exit %d, stderr %q; want 0, silent", code, stderr)
	}
	for i := 0; i < 2; i++ {
		if got := n.read(hwmonRel(i, "power1_cap")); got != "750000000" {
			t.Fatalf("device %d power1_cap = %s after a set on the secondary, want 750000000", i, got)
		}
	}
	if code, _, stderr := n.main("", rocmSetPowerOverDrive(0, 500)...); code != 0 || stderr != "" {
		t.Fatalf("set on the primary: exit %d, stderr %q", code, stderr)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "500000000" {
		t.Fatalf("primary power1_cap = %s, want 500000000: a non-zero energy counter is a primary", got)
	}

	power := func() map[string]map[string]string {
		t.Helper()
		code, stdout, stderr := n.main("", "rocm-smi", "-P", "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("-P: exit %d, stderr %q", code, stderr)
		}
		return rocmJSON(t, stdout)
	}
	m := power()
	if got := m["card1"]["Average Graphics Package Power (W)"]; got != "N/A (Secondary die)" {
		t.Errorf("card1 power %q, want N/A (Secondary die)", got)
	}
	if got := m["card0"]["Current Socket Graphics Package Power (W)"]; got != "210.0" {
		t.Errorf("card0 power %q, want its own reading 210.0, the package power", got)
	}
	if got := m["card2"]["Current Socket Graphics Package Power (W)"]; got != "120.0" {
		t.Errorf("card2 power %q, want its own 120.0", got)
	}

	n.write(hwmonRel(1, "power1_input"), "15000000", 0o444)
	m = power()
	if got := m["card1"]["Current Socket Graphics Package Power (W)"]; got != "15.0" {
		t.Errorf("card1 with a reading prints %q, want its own 15.0 (the reading, 2701-2706, comes before the secondary check, 2707)", got)
	}
	if got := m["card0"]["Current Socket Graphics Package Power (W)"]; got != "210.0" {
		t.Errorf("card0 power %q, want 210.0: the secondary's reading must not be added again", got)
	}
	code, stdout, stderr := n.main("", "amd-smi", "metric", "-g", "0", "--power", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("amd-smi metric: exit %d, stderr %q", code, stderr)
	}
	var doc struct {
		GPUData []struct {
			Power struct {
				SocketPower struct {
					Value int64 `json:"value"`
				} `json:"socket_power"`
			} `json:"power"`
		} `json:"gpu_data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.GPUData) != 1 {
		t.Fatalf("amd-smi metric output %q: %v", stdout, err)
	}
	if got := doc.GPUData[0].Power.SocketPower.Value; got != 210 {
		t.Errorf("amd-smi socket_power of GPU 0 = %d W, want its own 210", got)
	}
}

// dpmLevelWords are the words power_dpm_force_performance_level parses.
var dpmLevelWords = []string{"auto", "low", "high", "manual", "profile_exit", "profile_standard",
	"profile_min_sclk", "profile_min_mclk", "profile_peak", "perf_determinism"}

// dpmCardFamily is the DPM rendering of one family, as package hwemu
// writes it, with neutral clocks: the levels it refuses with their errno,
// the overdrive bounds a write may set (none: no overdrive file), whether
// it has a power profile table, and the unprivileged rocm-smi no-op.
type dpmCardFamily struct {
	refused  map[string]string
	od       []string
	profiles bool
	noop     bool
}

// dpmLevelRel is power_dpm_force_performance_level of GPU i of newMI300XNode.
func dpmLevelRel(i int) string { return dpmDevRel(i, layout.AMDGPUPerfLevelFile) }

func dpmDevRel(i int, name string) string {
	return path.Join(layout.SysDir, layout.PCIDevDir(0x10+i), name)
}

// dpm adds the DPM files of family f to every GPU of newMI300XNode, at
// level auto, sclk 300 to 1500 MHz and mclk 400 to 800 MHz.
func (n *testNode) dpm(f dpmCardFamily) {
	for i := 0; i < 8; i++ {
		n.write(dpmLevelRel(i), "auto", 0o644)
		n.write(dpmDevRel(i, "pp_dpm_sclk"), "0: 300Mhz *\n1: 1500Mhz ", 0o644)
		n.write(dpmDevRel(i, "pp_dpm_mclk"), "0: 400Mhz *\n1: 800Mhz ", 0o644)
		state := path.Join(layout.StateDir, "amdgpu", "card"+strconv.Itoa(i+1))
		var levels []string
		for _, w := range dpmLevelWords {
			if e := f.refused[w]; e != "" {
				w += " " + e
			}
			levels = append(levels, w)
		}
		n.write(path.Join(state, amdgpuPerfLevelsFile), strings.Join(levels, "\n"), 0o444)
		noop := "0"
		if f.noop {
			noop = "1"
		}
		n.write(path.Join(state, amdgpuUnprivNoopFile), noop, 0o444)
		if len(f.od) > 0 {
			n.write(dpmDevRel(i, layout.AMDGPUODFile), "OD_SCLK:\n0: 300Mhz\n1: 1500Mhz\nOD_MCLK:\n0: 400Mhz\n1: 800Mhz", 0o644)
			n.write(path.Join(state, amdgpuODWritableFile), strings.Join(f.od, " "), 0o444)
		}
		if f.profiles {
			n.write(dpmDevRel(i, layout.AMDGPUPowerProfileFile), dpmProfileTable(0), 0o644)
		}
	}
}

// dpmProfileTable is pp_power_profile_mode of SMU 13.0.0 with mode star
// starred, without its trailing newline.
func dpmProfileTable(star int) string {
	modes := []string{"BOOTUP_DEFAULT", "3D_FULL_SCREEN", "POWER_SAVING", "VIDEO", "VR", "COMPUTE", "CUSTOM", "WINDOW_3D"}
	out := fmt.Sprintf("%16s %s %s %s %s %s %s %s %s %s", "PROFILE_INDEX(NAME)", "CLOCK_TYPE(NAME)", "FPS", "MinActiveFreqType",
		"MinActiveFreq", "BoosterFreqType", "BoosterFreq", "PD_Data_limit_c", "PD_Data_error_coeff", "PD_Data_error_rate_coeff")
	for i, m := range modes {
		s := " "
		if i == star {
			s = "*"
		}
		out += fmt.Sprintf("\n%2d %14s%s:", i, m, s)
	}
	return out
}

// dpmNavi is a family that takes every level but perf_determinism, has a
// power profile table and no overdrive, as the Radeon PRO W7900; dpmAqua
// takes auto, manual and perf_determinism only and has overdrive without an
// uclk minimum, as the MI300X.
var (
	dpmNavi = dpmCardFamily{refused: map[string]string{"perf_determinism": "EINVAL"}, profiles: true, noop: true}
	dpmAqua = dpmCardFamily{refused: map[string]string{"low": "EINVAL", "high": "EINVAL", "profile_exit": "EINVAL",
		"profile_standard": "EINVAL", "profile_min_sclk": "EINVAL", "profile_min_mclk": "EINVAL", "profile_peak": "EINVAL"},
		od: []string{"sclkMin", "sclkMax", "uclkMax"}, noop: true}
)

// SG01: -p prints each GPU's level as showPerformanceLevel does
// (rocm_smi.py:2612-2622), and under --json the key Performance Level.
func TestSG01ShowPerfLevel(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	n.dpm(dpmNavi)
	n.write(dpmLevelRel(2), "high", 0o644)
	code, stdout, stderr := n.main("", "rocm-smi", "--showperflevel")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for i := 0; i < 8; i++ {
		want := "auto"
		if i == 2 {
			want = "high"
		}
		if line := fmt.Sprintf("GPU[%d]\t\t: Performance Level: %s\n", i, want); !strings.Contains(stdout, line) {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	if !strings.Contains(stdout, "= Show Performance Level =") {
		t.Errorf("stdout lacks the section spacer:\n%s", stdout)
	}
	code, stdout, _ = n.main("", "rocm-smi", "-d", "1", "2", "--showperflevel", "--json")
	if m := rocmJSON(t, stdout); code != 0 || len(m) != 2 || m["card1"]["Performance Level"] != "auto" || m["card2"]["Performance Level"] != "high" {
		t.Errorf("--json: exit %d, %v; want card1 auto and card2 high", code, m)
	}
}

// SG02: --setperflevel high writes the level file and prints the success
// line; a word outside auto, low, high and manual, such as profile_peak, is
// refused by rocm-smi itself and writes nothing; a level the family refuses
// fails quietly (rsmi_ret_ok) and writes nothing either.
func TestSG02SetPerfLevel(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	n.dpm(dpmNavi)
	code, stdout, stderr := n.main("", "rocm-smi", "-d", "0", "--setperflevel", "high")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "GPU[0]\t\t: Performance level set to high\n") {
		t.Fatalf("high: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := n.read(dpmLevelRel(0)); got != "high" {
		t.Errorf("level after high: %q", got)
	}
	if got := n.read(dpmLevelRel(1)); got != "auto" {
		t.Errorf("-d 0 changed GPU 1: %q", got)
	}
	code, stdout, stderr = n.main("", "rocm-smi", "-d", "1", "--setperflevel", "profile_peak")
	if code != 0 || strings.Contains(stdout, "set to") || !strings.Contains(stderr, "ERROR: GPU[1]\t: Unable to set Performance Level\n") ||
		!strings.Contains(stderr, "ERROR: Invalid Performance level: profile_peak\n") {
		t.Errorf("profile_peak: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := n.read(dpmLevelRel(1)); got != "auto" {
		t.Errorf("profile_peak wrote the level: %q", got)
	}

	m := newMI300XNode(t, rocmVariantCurrent, "")
	m.dpm(dpmAqua)
	code, stdout, stderr = m.main("", "rocm-smi", "-d", "0", "--setperflevel", "high")
	if code != 0 || stderr != "" || strings.Contains(stdout, "set to") {
		t.Errorf("high on a family that refuses it: exit %d, stdout %q, stderr %q; want no success line", code, stdout, stderr)
	}
	if got := m.read(dpmLevelRel(0)); got != "auto" {
		t.Errorf("a refused high was written: %q", got)
	}
	if code, _, stderr := m.main("", "rocm-smi", "-d", "0", "--setperflevel", "high", "--loglevel", "debug"); code != 1 || !strings.Contains(stderr, "DEBUG: RSMI_STATUS_INVALID_ARGS GPU[0]: set_perf_level") {
		t.Errorf("refused high with --loglevel debug: exit %d, stderr %q; want exit 1 and the debug line", code, stderr)
	}
}

// SG03: without root, a family whose rocm-smi set is a silent no-op prints
// nothing, exits 0 and changes nothing; another family's set fails the
// write and prints no success line.
func TestSG03UnprivilegedSetPerfLevel(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	n.dpm(dpmNavi)
	n.env.Unprivileged = true
	if code, stdout, stderr := n.main("", "rocm-smi", "--setperflevel", "high"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("unprivileged no-op: exit %d, stdout %q, stderr %q; want 0 and nothing", code, stdout, stderr)
	}
	if code, stdout, stderr := n.main("", "rocm-smi", "-d", "0", "--setpoweroverdrive", "500"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("unprivileged no-op, power: exit %d, stdout %q, stderr %q; want 0 and nothing", code, stdout, stderr)
	}
	for i := 0; i < 8; i++ {
		if got := n.read(dpmLevelRel(i)); got != "auto" {
			t.Errorf("GPU %d level %q after the no-op", i, got)
		}
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "750000000" {
		t.Errorf("power1_cap %s after the no-op", got)
	}

	f := dpmNavi
	f.noop = false
	m := newMI300XNode(t, rocmVariantCurrent, "")
	m.dpm(f)
	m.env.Unprivileged = true
	code, stdout, _ := m.main("", "rocm-smi", "-d", "0", "--setperflevel", "high")
	if code != 0 || strings.Contains(stdout, "set to") || m.read(dpmLevelRel(0)) != "auto" {
		t.Errorf("unprivileged without the no-op: exit %d, stdout %q, level %q", code, stdout, m.read(dpmLevelRel(0)))
	}
	n.env.Unprivileged = false
	if code, stdout, _ := n.main("", "rocm-smi", "-d", "0", "--setperflevel", "high"); code != 0 || !strings.Contains(stdout, "set to high") {
		t.Errorf("as root: exit %d, stdout %q", code, stdout)
	}
}

// SG04: --setprofile N names the profile of bit N-1 (profileString,
// rocm_smi.py:4125-4133); rocm_smi_lib sets the level to manual first, then
// writes the mode's index (rocm_smi.cc:1807-1840). -l lists the profiles
// with the one in effect starred.
func TestSG04SetProfileSwitchesToManualFirst(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	n.dpm(dpmNavi)
	code, stdout, stderr := n.main("", "rocm-smi", "-d", "0", "--setprofile", "4")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "GPU[0]\t\t: Successfully set profile to: COMPUTE\n") {
		t.Fatalf("--setprofile 4: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if lvl, mode := n.read(dpmLevelRel(0)), n.read(dpmDevRel(0, layout.AMDGPUPowerProfileFile)); lvl != "manual" || mode != "5" {
		t.Errorf("after --setprofile 4: level %q, pp_power_profile_mode %q; want manual and 5, the index of COMPUTE", lvl, mode)
	}
	n.write(dpmDevRel(1, layout.AMDGPUPowerProfileFile), dpmProfileTable(5), 0o644)
	if _, stdout, _ := n.main("", "rocm-smi", "-d", "1", "--setprofile", "compute"); !strings.Contains(stdout, "GPU[1]\t\t: Profile was already set to: COMPUTE\n") {
		t.Errorf("--setprofile compute over COMPUTE: stdout %q", stdout)
	}
	if _, _, stderr := n.main("", "rocm-smi", "-d", "1", "--setprofile", "turbo"); !strings.Contains(stderr, "Unable to set profile to: turbo (UNKNOWN profile)") {
		t.Errorf("--setprofile turbo: stderr %q", stderr)
	}
	_, stdout, _ = n.main("", "rocm-smi", "-d", "1", "--showprofile")
	for _, line := range []string{
		"GPU[1]\t\t: 1. Available power profile (#1 of 7): CUSTOM\n",
		"GPU[1]\t\t: 4. Available power profile (#4 of 7): COMPUTE*\n",
		"GPU[1]\t\t: 7. Available power profile (#7 of 7): BOOTUP DEFAULT\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("--showprofile lacks %q:\n%s", line, stdout)
		}
	}
}

// SG08: the DPM options are modelled and work; every option still
// unmodelled exits 2.
func TestSG08ModelledFlagsWorkOthersStillExit2(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, amdsmiVariant72)
	n.dpm(dpmAqua)
	for _, argv := range [][]string{
		{"rocm-smi", "--showperflevel"},
		{"rocm-smi", "-d", "0", "--setperflevel", "manual"},
		{"rocm-smi", "--showprofile"},
		{"amd-smi", "set", "-g", "0", "--perf-level", "auto"},
		{"amd-smi", "set", "-g", "0", "--profile", "3"},
		{"amd-smi", "set", "-g", "0", "-L", "sclk", "max", "1200"},
		{"amd-smi", "metric", "-g", "0", "--perf-level", "--json"},
	} {
		if code, _, stderr := n.main("", argv...); code != 0 || strings.Contains(stderr, "not emulated") {
			t.Errorf("%v: exit %d, stderr %q; want it emulated", argv, code, stderr)
		}
	}
	for _, argv := range [][]string{
		{"rocm-smi", "--showclocks"},
		{"rocm-smi", "--setsclk", "1"},
		{"amd-smi", "set", "-g", "0", "--soc-pstate", "1"},
		{"amd-smi", "metric", "-g", "0", "--clock", "--json"},
		{"amd-smi", "metric", "-g", "0", "--perf-level"},
	} {
		if code, _, stderr := n.main("", argv...); code != 2 || !strings.Contains(stderr, "is not emulated") {
			t.Errorf("%v: exit %d, stderr %q; want 2 and not emulated", argv, code, stderr)
		}
	}
}
