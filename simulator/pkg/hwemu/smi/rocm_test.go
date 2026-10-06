package smi

import (
	"encoding/json"
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

// No rocm-smi release has --showpowercap, so the agent's inventory call fails
// argparse with exit 2 and a usage error on stderr, and stdout is empty (D1).
func TestS04InventoryOnCurrentFailsWithUnrecognizedArguments(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	code, stdout, stderr := n.main("", agentRocmInventory...)
	if code != 2 || stdout != "" {
		t.Fatalf("exit %d, stdout %q; want 2 and empty", code, stdout)
	}
	if !strings.HasPrefix(stderr, "usage: rocm-smi [-h]") || !strings.Contains(stderr, "\nrocm-smi: error: unrecognized arguments: --showpowercap\n") {
		t.Fatalf("stderr %q, want usage and the unrecognized --showpowercap", stderr)
	}
	out, err := n.run(agentRocmInventory...)
	if exitCode(t, err) != 2 || !strings.Contains(string(out), "error: unrecognized arguments: --showpowercap") {
		t.Fatalf("Runner: %v, %q; want exit status 2 and the argparse error", err, out)
	}
}

// The legacy variant answers the agent's inventory call with the keys the
// agent reads (listAmdDevices in cmd/agent), all strings that parse as watts.
// Max is the top of the range, not the current cap, even when -M also ran.
func TestS05LegacyInventoryReturnsAgentKeysAsStrings(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantLegacy, "")
	n.write(hwmonRel(0, "power1_cap"), "600000000", 0o644)
	code, stdout, stderr := n.main("", agentRocmInventory...)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	m := rocmJSON(t, stdout)
	if len(m) != 8 {
		t.Fatalf("%d cards, want 8: %v", len(m), m)
	}
	for i := 0; i < 8; i++ {
		card := m["card"+strconv.Itoa(i)]
		for _, k := range []string{"Card SKU", "Max Graphics Package Power (W)", "Min Graphics Package Power (W)", "Current Power Cap (W)", "Average Graphics Package Power (W)"} {
			if _, ok := card[k]; !ok {
				t.Fatalf("card%d has no %q: %v", i, k, card)
			}
		}
	}
	card0 := m["card0"]
	want := map[string]string{
		"Card SKU":                           "X0000000",
		"Max Graphics Package Power (W)":     "750.0",
		"Min Graphics Package Power (W)":     "0.0",
		"Current Power Cap (W)":              "600.0",
		"Average Graphics Package Power (W)": "100.0",
	}
	for k, v := range want {
		if card0[k] != v {
			t.Errorf("card0 %q = %q, want %q", k, card0[k], v)
		}
		if k != "Card SKU" {
			if _, err := strconv.ParseFloat(card0[k], 64); err != nil {
				t.Errorf("card0 %q = %q does not parse as watts", k, card0[k])
			}
		}
	}

	_, stdout, _ = n.main("", "rocm-smi", "-M", "--showpowercap", "--json")
	if got := rocmJSON(t, stdout)["card0"]["Max Graphics Package Power (W)"]; got != "750.0" {
		t.Fatalf("with -M too, card0 Max = %q, want --showpowercap's 750.0, not -M's current cap", got)
	}
}

// --setpoweroverdrive as the agent calls it: a value in range is written; a
// value above max is refused with exit 0 (D2); a value above max(current,
// default) prompts and fails on the agent's empty stdin (D3); a missing
// device exits 0; 0 restores the default.
func TestS06SetPowerOverDriveOutcomes(t *testing.T) {
	t.Parallel()
	n := newMI300XNode(t, rocmVariantCurrent, "")
	if out, err := n.run(agentRocmSet(0, 500)...); err != nil {
		t.Fatalf("-d 0 500: %v (%q)", err, out)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "500000000" {
		t.Fatalf("after 500: power1_cap = %s, want 500000000", got)
	}
	if got := n.read(hwmonRel(1, "power1_cap")); got != "750000000" {
		t.Fatalf("-d 0 changed device 1: power1_cap = %s", got)
	}

	code, _, stderr := n.main("", agentRocmSet(0, 800)...)
	if code != 0 || !strings.Contains(stderr, "ERROR: GPU[0]\t: Unable to set Power OverDrive") ||
		!strings.Contains(stderr, "Value cannot be greater than: 750W") {
		t.Fatalf("800 above max: exit %d, stderr %q; want 0 and the refusal", code, stderr)
	}
	if got := n.read(hwmonRel(0, "power1_cap")); got != "500000000" {
		t.Fatalf("after refused 800: power1_cap = %s, want 500000000", got)
	}

	code, stdout, stderr := n.main("", agentRocmSet(9, 500)...)
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
	// default), so rocm-smi asks, and the agent's stdin is empty.
	p := newMI300XNode(t, rocmVariantCurrent, "")
	p.write(hwmonRel(0, "power1_cap_default"), "600000000", 0o444)
	p.write(hwmonRel(0, "power1_cap"), "600000000", 0o644)
	out, err := p.run(agentRocmSet(0, 700)...)
	if exitCode(t, err) != 1 || !strings.Contains(string(out), "Do you accept these terms? [y/N] ") ||
		!strings.HasSuffix(string(out), "EOFError: EOF when reading a line\n") {
		t.Fatalf("700 with EOF stdin: %v, %q; want exit status 1 after the prompt", err, out)
	}
	if got := p.read(hwmonRel(0, "power1_cap")); got != "600000000" {
		t.Fatalf("after EOF: power1_cap = %s, want 600000000", got)
	}
	if code, _, stderr := p.main("n\n", agentRocmSet(0, 700)...); code != 1 || stderr != "Confirmation not given. Exiting without setting value\n" {
		t.Fatalf("answer n: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := p.main("", append(agentRocmSet(0, 700), "--autorespond", "y")...); code != 0 || stderr != "" {
		t.Fatalf("--autorespond y: exit %d, stderr %q", code, stderr)
	}
	if got := p.read(hwmonRel(0, "power1_cap")); got != "700000000" {
		t.Fatalf("after --autorespond y: power1_cap = %s, want 700000000", got)
	}
}

// --showproductname --json prints showProduct's keys in its order, and Card
// SKU, which the agent takes for the product, is a VBIOS segment. -M, which
// the agent would take for the maximum, is the current cap floored (D4).
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

	code, _, stderr := n.main("", agentRocmSet(1, 500)...)
	if code != 0 || stderr != "" {
		t.Fatalf("set on the secondary: exit %d, stderr %q; want 0, silent", code, stderr)
	}
	for i := 0; i < 2; i++ {
		if got := n.read(hwmonRel(i, "power1_cap")); got != "750000000" {
			t.Fatalf("device %d power1_cap = %s after a set on the secondary, want 750000000", i, got)
		}
	}
	if code, _, stderr := n.main("", agentRocmSet(0, 500)...); code != 0 || stderr != "" {
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
