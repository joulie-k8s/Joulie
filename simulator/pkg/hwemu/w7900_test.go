package hwemu

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// w7900Profile is the Radeon PRO W7900 profile, whose power cap is pinned:
// on the measured node the driver and both tools report minimum, maximum,
// default and current at 241 W on every board (profile source w7900smi). Its
// tree has the MI300X's layout, a BMC VGA as card0 and board i as card i+1
// with hwmon i+2, so nodeMI300XCap names its files too.
const (
	w7900Profile = "amd-radeon-pro-w7900-4gpu"
	w7900CapUW   = "241000000"
)

// w7900Tool runs one fake tool over tree and returns its exit code, stdout
// and stderr.
func w7900Tool(t *testing.T, tree *Tree, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := smi.Main(args, smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// H43: every W7900 board's hwmon reports the cap range 241 to 241 W, with the
// cap and its default at 241 W. A write of any other value, below or above,
// is refused and reads back 241 W with a reverted-out-of-range event, and a
// write of 241 W is accepted. At full load no board draws more than 241 W.
func TestH43W7900CapIsPinnedAt241W(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, w7900Profile)
	tr := renderTestTree(t, p)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	nodeStep(t, n, 0)
	for i := 0; i < p.GPUs.Count.V; i++ {
		for _, name := range []string{"power1_cap", "power1_cap_min", "power1_cap_max", "power1_cap_default"} {
			if got := nodeRead(t, tr, nodeMI300XCap(i, name)); got != w7900CapUW+"\n" {
				t.Errorf("board %d %s = %q, want %s", i, name, got, w7900CapUW)
			}
		}
	}

	capf := nodeMI300XCap(2, "power1_cap")
	for _, v := range []string{"200000000", "240000000", "242000000", "295000000", "0"} {
		nodeWrite(t, tr, capf, v)
		nodeStep(t, n, time.Second)
		if got := nodeRead(t, tr, capf); got != w7900CapUW+"\n" {
			t.Errorf("a write of %s reads back %q, want %s", v, got, w7900CapUW)
		}
		if e := nodeOneEvent(t, n, capf); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.Written != v || e.Effective != w7900CapUW {
			t.Errorf("a write of %s: event %+v, want %s with %s keeping %s", v, e, EventRejected, ErrnoEINVAL, w7900CapUW)
		}
	}
	nodeWrite(t, tr, capf, w7900CapUW)
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, capf); e.Kind != EventAccepted || e.Effective != w7900CapUW {
		t.Errorf("a write of the pinned value: event %+v, want %s", e, EventAccepted)
	}

	nodeStep(t, n, 5*time.Second)
	for _, g := range n.State().GPUs {
		if g.LimitW != 241 || g.PowerW > 241 {
			t.Errorf("board %d at full load: limit %v W, power %.1f W; want the 241 W limit and at most 241 W", g.Index, g.LimitW, g.PowerW)
		}
	}
}

// H44: the W7900 boards are display controllers, so the node carries NFD's
// pci-0300_1002 next to the BMC's pci-0300_1a03, as measured, and no
// accelerator-class pci-1200_1002. The labeller's product name, 4 amd.com/gpu
// and the amd.com/gpu taint are the measured node's, and the identity facts
// are the published ones: device 0x7448 and 48 GB of memory.
func TestH44W7900IsADisplayClassNode(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, w7900Profile)
	node := p.NodeObject("n")
	labels := node.Labels
	for k, want := range map[string]string{
		"feature.node.kubernetes.io/pci-0300_1002.present": "true",
		"feature.node.kubernetes.io/pci-0300_1a03.present": "true",
		"feature.node.kubernetes.io/pci-1200_1002.present": "",
		"amd.com/gpu.product-name":                         "AMD_Radeon_PRO_W7900",
		"amd.com/gpu.device-id":                            "7448",
		"amd.com/gpu.vram":                                 "48G",
	} {
		if labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, labels[k], want)
		}
	}
	if q := node.Status.Capacity["amd.com/gpu"]; q.Value() != 4 {
		t.Errorf("capacity amd.com/gpu = %s, want 4", q.String())
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "amd.com/gpu" || node.Spec.Taints[0].Effect != "NoSchedule" {
		t.Errorf("taints %v, want amd.com/gpu NoSchedule alone", node.Spec.Taints)
	}
	if g := p.GPUs; g.PCI.Class.V != "0x030000" || g.PCI.Device.V != "0x7448" || g.Product.V != "AMD Radeon Pro W7900" || g.GFXVersion.V != "gfx1100" {
		t.Errorf("gpu pci %+v, product %q, gfx %q; want the display class, device 0x7448, the captured name and gfx1100", g.PCI, g.Product.V, g.GFXVersion.V)
	}
}

// X03: on the W7900 node the fake tools print what the measured ones did and
// refuse every cap but 241 W the way the real ones do. rocm-smi
// --showmaxpower --json is byte for byte the captured output;
// --showproductname gives the captured series, model, vendor, SKU and GFX
// version; --showpower reads the average sensor; --showpowercap is an
// unrecognized argument; amd-smi static --limit reports PPT0 from 241
// to 241 W at 241 W and no PPT1. rocm-smi --setpoweroverdrive 200 and 300
// fail with the range bound and still exit 0, and amd-smi set -o ppt0 200
// names the 241 to 241 W range; no power1_cap changes.
func TestX03W7900ToolsReportThePinnedCap(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, w7900Profile)
	tr := renderTestTree(t, p)

	// The capture of rocm-smi --showmaxpower --json on the measured node.
	const maxPower = `{"card0": {"Max Graphics Package Power (W)": "241.0"}, "card1": {"Max Graphics Package Power (W)": "241.0"}, "card2": {"Max Graphics Package Power (W)": "241.0"}, "card3": {"Max Graphics Package Power (W)": "241.0"}}` + "\n"
	if code, out, _ := w7900Tool(t, tr, "rocm-smi", "--showmaxpower", "--json"); code != 0 || out != maxPower {
		t.Errorf("rocm-smi --showmaxpower --json = %d %q, want 0 and the capture %q", code, out, maxPower)
	}

	cards := func(args ...string) map[string]map[string]string {
		t.Helper()
		code, out, errOut := w7900Tool(t, tr, args...)
		var m map[string]map[string]string
		if err := json.Unmarshal([]byte(out), &m); code != 0 || err != nil || len(m) != p.GPUs.Count.V {
			t.Fatalf("%v = %d, %d cards (%v); stdout %q, stderr %q", args, code, len(m), err, out, errOut)
		}
		return m
	}
	for card, keys := range cards("rocm-smi", "--showproductname", "--json") {
		for k, want := range map[string]string{
			"Card Series": "AMD Radeon Pro W7900",
			"Card Model":  "0x7448",
			"Card Vendor": "Advanced Micro Devices, Inc. [AMD/ATI]",
			"Card SKU":    "D7070100",
			"GFX Version": "gfx1100",
		} {
			if keys[k] != want {
				t.Errorf("%s %s = %q, want the captured %q", card, k, keys[k], want)
			}
		}
	}
	for card, keys := range cards("rocm-smi", "--showpower", "--json") {
		if _, ok := keys["Average Graphics Package Power (W)"]; !ok || len(keys) != 1 {
			t.Errorf("%s power keys %v, want Average Graphics Package Power (W) alone, as captured", card, keys)
		}
	}
	if code, out, errOut := w7900Tool(t, tr, "rocm-smi", "--showpowercap", "--showproductname", "--json"); code != 2 || out != "" ||
		!strings.Contains(errOut, "rocm-smi: error: unrecognized arguments: --showpowercap") {
		t.Errorf("rocm-smi --showpowercap = %d, stdout %q, stderr %q; want 2 and the unrecognized --showpowercap, as captured", code, out, errOut)
	}

	code, out, _ := w7900Tool(t, tr, "amd-smi", "static", "--limit", "--json")
	var limits struct {
		GPUData []struct {
			GPU   int `json:"gpu"`
			Limit struct {
				PPT0 map[string]json.RawMessage `json:"ppt0"`
				PPT1 map[string]json.RawMessage `json:"ppt1"`
			} `json:"limit"`
		} `json:"gpu_data"`
	}
	if err := json.Unmarshal([]byte(out), &limits); code != 0 || err != nil || len(limits.GPUData) != p.GPUs.Count.V {
		t.Fatalf("amd-smi static --limit --json = %d (%v): %s", code, err, out)
	}
	for _, g := range limits.GPUData {
		for _, k := range []string{"max_power_limit", "min_power_limit", "socket_power_limit"} {
			if got := strings.Join(strings.Fields(string(g.Limit.PPT0[k])), ""); got != `{"value":241,"unit":"W"}` {
				t.Errorf("gpu %d ppt0 %s = %s, want 241 W", g.GPU, k, got)
			}
			if got := string(g.Limit.PPT1[k]); got != `"N/A"` {
				t.Errorf("gpu %d ppt1 %s = %s, want N/A", g.GPU, k, got)
			}
		}
	}

	// Every power1_cap keeps the rendered content after each call, so a
	// write that a later call puts back to 241 W still fails.
	untouched := func(call string) {
		t.Helper()
		for i := 0; i < p.GPUs.Count.V; i++ {
			if got := nodeRead(t, tr, nodeMI300XCap(i, "power1_cap")); got != w7900CapUW+"\n" {
				t.Errorf("after %s board %d power1_cap = %q, want it untouched, %q", call, i, got, w7900CapUW+"\n")
			}
		}
	}
	for _, c := range []struct{ watts, bound string }{{"200", "less than: 241W"}, {"300", "greater than: 241W"}} {
		code, _, errOut := w7900Tool(t, tr, "rocm-smi", "-d", "0", "--setpoweroverdrive", c.watts)
		if code != 0 || !strings.Contains(errOut, "Unable to set Power OverDrive") || !strings.Contains(errOut, "Value cannot be "+c.bound) {
			t.Errorf("rocm-smi --setpoweroverdrive %s = %d, stderr %q; want exit 0 and Value cannot be %s", c.watts, code, errOut, c.bound)
		}
		untouched("rocm-smi --setpoweroverdrive " + c.watts)
	}
	if code, out, _ := w7900Tool(t, tr, "amd-smi", "set", "-g", "0", "-o", "ppt0", "200"); code != 0 || !strings.Contains(out, "Power cap must be between 241W and 241W") {
		t.Errorf("amd-smi set -o ppt0 200 = %d, stdout %q; want 0 and the 241 to 241 W range", code, out)
	}
	untouched("amd-smi set -o ppt0 200")
	if code, out, _ := w7900Tool(t, tr, "amd-smi", "set", "-g", "0", "-o", "ppt0", "241"); code != 0 || !strings.Contains(out, "PPT0 power cap is already set to 241W") {
		t.Errorf("amd-smi set -o ppt0 241 = %d, stdout %q; want 0 and already set", code, out)
	}
	untouched("amd-smi set -o ppt0 241")
}
