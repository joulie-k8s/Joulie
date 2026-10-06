package smi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The agent's GPU commands, verbatim from cmd/agent/main.go: the vendor
// probes of detectGPUVendor, and the inventory queries of listNvidiaDevices
// and listAmdDevices.
var (
	agentNvidiaList    = []string{"nvidia-smi", "-L"}
	agentNvidiaQuery   = []string{"nvidia-smi", "--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name", "--format=csv,noheader,nounits"}
	agentRocmProduct   = []string{"rocm-smi", "--showproductname"}
	agentRocmInventory = []string{"rocm-smi", "--showpowercap", "--showproductname", "--json"}
)

// agentNvidiaSet is the call of applyNvidiaGPUCap; agentRocmSet is that of
// applyAmdGPUCap.
func agentNvidiaSet(i, w int) []string {
	return []string{"nvidia-smi", "-i", strconv.Itoa(i), "-pl", strconv.Itoa(w)}
}

func agentRocmSet(d, w int) []string {
	return []string{"rocm-smi", "-d", strconv.Itoa(d), "--setpoweroverdrive", strconv.Itoa(w)}
}

// testNode is a node root built by hand, laid out as Render lays it out:
// sys/ and state/ with tools.json.
type testNode struct {
	t    *testing.T
	root string
	env  Env
}

// newTestNode writes tools.json listing tools (name to variant).
func newTestNode(t *testing.T, tools map[string]string) *testNode {
	t.Helper()
	root := t.TempDir()
	n := &testNode{t: t, root: root, env: Env{SysRoot: filepath.Join(root, layout.SysDir), StateRoot: filepath.Join(root, layout.StateDir)}}
	m := layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{}}
	for name, v := range tools {
		m.Tools[name] = layout.ToolEntry{Variant: v}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	n.write(path.Join(layout.StateDir, layout.ToolsFile), string(b), 0o444)
	if err := os.MkdirAll(n.env.SysRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return n
}

// write writes rel with a trailing newline, as sysfs and the renderer do.
// Read-only files are 0444, so a tool that writes one fails in a non-root
// test instead of passing unnoticed.
func (n *testNode) write(rel, content string, mode os.FileMode) {
	n.t.Helper()
	p := filepath.Join(n.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		n.t.Fatal(err)
	}
	os.Remove(p)
	if err := os.WriteFile(p, []byte(content+"\n"), mode); err != nil {
		n.t.Fatal(err)
	}
}

// remove deletes rel, as a renderer that never created it would leave it.
func (n *testNode) remove(rel string) {
	n.t.Helper()
	if err := os.Remove(filepath.Join(n.root, filepath.FromSlash(rel))); err != nil {
		n.t.Fatal(err)
	}
}

func (n *testNode) symlink(rel, target string) {
	n.t.Helper()
	p := filepath.Join(n.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		n.t.Fatal(err)
	}
}

// read returns rel without its trailing newline.
func (n *testNode) read(rel string) string {
	n.t.Helper()
	b, err := os.ReadFile(filepath.Join(n.root, filepath.FromSlash(rel)))
	if err != nil {
		n.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// nvmlGPU writes state/nvml/gpu<i>. power_limit_mw is agent-owned (0644);
// everything else is 0444.
func (n *testNode) nvmlGPU(i int, files map[string]string) {
	for f, v := range files {
		mode := os.FileMode(0o444)
		if f == "power_limit_mw" {
			mode = 0o644
		}
		n.write(path.Join(layout.StateDir, layout.NVMLGPUDir(i), f), v, mode)
	}
}

func nvmlRel(i int, file string) string {
	return path.Join(layout.StateDir, layout.NVMLGPUDir(i), file)
}

// newNVLNode is eight H100 NVL at 200 to 400 W, each capped at 400 W and
// drawing 50 W.
func newNVLNode(t *testing.T) *testNode {
	n := newTestNode(t, map[string]string{"nvidia-smi": "default"})
	n.write(path.Join(layout.StateDir, layout.NVMLDir, "driver_version"), "550.54.15", 0o444)
	for i := 0; i < 8; i++ {
		n.nvmlGPU(i, map[string]string{
			"name":                    "NVIDIA H100 NVL",
			"uuid":                    fmt.Sprintf("GPU-00000000-0000-0000-0000-00000000000%d", i),
			"pci_bus_id":              fmt.Sprintf("00000000:%02x:00.0", 0x10+i),
			"power_min_limit_mw":      "200000",
			"power_max_limit_mw":      "400000",
			"power_default_limit_mw":  "400000",
			"power_limit_mw":          "400000",
			"enforced_power_limit_mw": "400000",
			"power_draw_mw":           "50000",
			"total_energy_mj":         "1000",
			"utilization_gpu_pct":     "0",
		})
	}
	return n
}

// amdCard renders one AMD PCI function as Render lays out the MI300X node:
// the device at bus, drm/card<card> with its class link, and, when hwmon is
// not nil, hwmon<hw> with its class link. Attributes in attrs are read-only;
// power1_cap is agent-owned.
func (n *testNode) amdCard(bus, card, hw int, attrs, hwmon map[string]string) {
	dev := layout.PCIDevDir(bus)
	for f, v := range attrs {
		n.write(path.Join(layout.SysDir, dev, f), v, 0o444)
	}
	cardDir := layout.DRMCardDir(bus, card)
	n.symlink(path.Join(layout.SysDir, cardDir, "device"), "../../../"+path.Base(dev))
	n.symlink(path.Join(layout.SysDir, "class/drm", "card"+strconv.Itoa(card)), "../../"+cardDir)
	if hwmon == nil {
		return
	}
	link, target := layout.HwmonClassLink(hw, dev)
	n.symlink(path.Join(layout.SysDir, link), target)
	for f, v := range hwmon {
		mode := os.FileMode(0o444)
		if f == "power1_cap" || f == "power2_cap" {
			mode = 0o644
		}
		n.write(path.Join(layout.SysDir, dev, "hwmon", "hwmon"+strconv.Itoa(hw), f), v, mode)
	}
}

// mi300xHwmon is one MI300X hwmon: 0 to 750 W, default and cap 750 W, input
// power drawW.
func mi300xHwmon(drawW int) map[string]string {
	return map[string]string{
		"name":               "amdgpu",
		"power1_cap":         "750000000",
		"power1_cap_min":     "0",
		"power1_cap_max":     "750000000",
		"power1_cap_default": "750000000",
		"power1_input":       strconv.Itoa(drawW * 1000000),
	}
}

// newMI300XNode is the MI300X layout Render writes: the BMC VGA as card0 at
// bus 0x02, then GPU i at bus 0x10+i, card i+1, hwmon i+2, drawing 100+10i W.
func newMI300XNode(t *testing.T, rocm, amdsmi string) *testNode {
	tools := map[string]string{}
	if rocm != "" {
		tools["rocm-smi"] = rocm
	}
	if amdsmi != "" {
		tools["amd-smi"] = amdsmi
	}
	n := newTestNode(t, tools)
	n.amdCard(0x02, 0, 0, map[string]string{"vendor": "0x1a03", "device": "0x2000", "class": "0x030000"}, nil)
	for i := 0; i < 8; i++ {
		n.amdCard(0x10+i, i+1, i+2, map[string]string{
			"vendor":        "0x1002",
			"device":        "0x74a1",
			"class":         "0x120000",
			"product_name":  "AMD Instinct MI300X",
			"vbios_version": "113-X0000000-102",
		}, mi300xHwmon(100+10*i))
	}
	return n
}

// hwmonRel is an attribute of GPU i of newMI300XNode.
func hwmonRel(i int, file string) string {
	return path.Join(layout.SysDir, layout.PCIDevDir(0x10+i), "hwmon", "hwmon"+strconv.Itoa(i+2), file)
}

// main runs Main as the binary does, with stdout and stderr apart.
func (n *testNode) main(stdin string, argv ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = Main(argv, n.env, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

// run is the agent's call: Runner.Run, combined output.
func (n *testNode) run(argv ...string) ([]byte, error) {
	return Runner{Env: n.env}.Run(context.Background(), argv[0], argv[1:]...)
}

// exitCode is the exit code behind a Runner error, 0 for nil.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	ee, ok := err.(*ExitError)
	if !ok {
		t.Fatalf("error %v (%T), want *ExitError", err, err)
	}
	return ee.Code
}

// The agent parses stdout and stderr combined (OSCommandRunner.Run), so a
// success path that also wrote to stderr would corrupt what it parses, such
// as the rocm-smi JSON. Each of the six agent calls is run on a node where it
// succeeds.
func TestS15AgentSuccessPathsWriteNothingToStderr(t *testing.T) {
	t.Parallel()
	type tc struct {
		name string
		node func(*testing.T) *testNode
		argv []string
	}
	cases := []tc{
		{"nvidia-smi -L", newNVLNode, agentNvidiaList},
		{"nvidia-smi query", newNVLNode, agentNvidiaQuery},
		{"nvidia-smi -i 0 -pl 300", newNVLNode, agentNvidiaSet(0, 300)},
	}
	for _, v := range []string{rocmVariantCurrent, rocmVariantLegacy} {
		v := v
		node := func(t *testing.T) *testNode { return newMI300XNode(t, v, "") }
		cases = append(cases,
			tc{"rocm-smi --showproductname " + v, node, agentRocmProduct},
			tc{"rocm-smi -d 0 --setpoweroverdrive 500 " + v, node, agentRocmSet(0, 500)},
		)
	}
	cases = append(cases, tc{"rocm-smi inventory legacy", func(t *testing.T) *testNode { return newMI300XNode(t, rocmVariantLegacy, "") }, agentRocmInventory})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := c.node(t).main("", c.argv...)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q, want 0 and empty (stdout %q)", code, stderr, stdout)
			}
			if stdout == "" {
				t.Fatalf("stdout empty, want the tool's output")
			}
		})
	}
}
