package hwemu

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderCorpusRoot is the hardware corpus the agent's tests replay, or
// JOULIE_HARDWARE_CORPUS when set, as the corpus test honours it.
func renderCorpusRoot() string {
	if v := os.Getenv("JOULIE_HARDWARE_CORPUS"); v != "" {
		return v
	}
	return filepath.Join("..", "..", "..", "cmd", "agent", "testdata", "hardware")
}

// renderCorpusMachines lists the machines of the corpus, skipping
// directories that start with _.
func renderCorpusMachines(t testing.TB) []string {
	t.Helper()
	entries, err := os.ReadDir(renderCorpusRoot())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no machines in %s", renderCorpusRoot())
	}
	return out
}

// H21: FromCorpus maps every corpus machine, and the rendered tree replays
// the capture: cpuinfo is byte-identical; every captured powercap file reads
// the same through class/powercap, the captured energy_uj included, and no
// zone gains or loses a file; writeRejectingZones reject writes with EISDIR;
// the cpufreq driver reads back from policy0; dev/nvidiactl exists exactly
// when nvidia-smi.txt does; the NVML state holds the captured values; and
// the Node carries the captured labels and allocatable.
func TestH21FromCorpusReplaysEveryMachine(t *testing.T) {
	t.Parallel()
	for _, machine := range renderCorpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(renderCorpusRoot(), machine)
			p, err := FromCorpus(dir)
			if err != nil {
				t.Fatal(err)
			}
			tree := renderTestTree(t, p)
			var meta renderCorpusMachine
			b, err := os.ReadFile(filepath.Join(dir, "machine.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(b, &meta); err != nil {
				t.Fatal(err)
			}

			want, _ := os.ReadFile(filepath.Join(dir, "cpuinfo"))
			if got := renderReadFile(t, tree, layout.CPUInfoFile); got != string(want) {
				t.Errorf("cpuinfo differs from the capture")
			}

			renderCheckCorpusPowercap(t, dir, tree, meta.WriteRejectingZones)

			driver, err := os.ReadFile(filepath.Join(dir, "cpufreq-driver"))
			policy0 := filepath.Join(tree.SysRoot, layout.PolicyDir(0), "scaling_driver")
			if err == nil {
				if got, _ := os.ReadFile(policy0); string(got) != string(driver) {
					t.Errorf("policy0/scaling_driver = %q, want the captured %q", got, driver)
				}
				if m, _ := filepath.Glob(filepath.Join(tree.SysRoot, "devices/system/cpu/cpu*/cpufreq/scaling_max_freq")); len(m) != 0 {
					t.Errorf("a driver-only capture renders scaling_max_freq, which the agent would write")
				}
			} else if _, err := os.Stat(filepath.Join(tree.SysRoot, "devices/system/cpu/cpufreq")); err == nil {
				t.Errorf("no cpufreq-driver captured, but cpufreq is rendered")
			}

			nvidia, nvErr := os.ReadFile(filepath.Join(dir, "nvidia-smi.txt"))
			_, devErr := os.Stat(filepath.Join(tree.Root, layout.DevNvidiactl))
			if (nvErr == nil) != (devErr == nil) {
				t.Errorf("nvidia-smi.txt present = %v, dev/nvidiactl present = %v", nvErr == nil, devErr == nil)
			}
			if nvErr == nil {
				renderCheckCorpusNVML(t, tree, string(nvidia))
			}

			var labels map[string]string
			lb, _ := os.ReadFile(filepath.Join(dir, "node-labels.json"))
			if err := json.Unmarshal(lb, &labels); err != nil {
				t.Fatal(err)
			}
			node := p.NodeObject("corpus-" + machine)
			for k, v := range labels {
				if k != "kubernetes.io/hostname" && node.Labels[k] != v {
					t.Errorf("label %s = %q, want the captured %q", k, node.Labels[k], v)
				}
			}
			for name, raw := range map[corev1.ResourceName]string{corev1.ResourceCPU: meta.Allocatable.CPU, corev1.ResourceMemory: meta.Allocatable.Memory} {
				want := resource.MustParse(raw)
				if got := node.Status.Allocatable[name]; got.Cmp(want) != 0 {
					t.Errorf("allocatable %s = %s, want %s", name, got.String(), raw)
				}
			}
			for name, raw := range meta.Allocatable.GPU {
				want := resource.MustParse(raw)
				if got := node.Status.Allocatable[corev1.ResourceName(name)]; got.Cmp(want) != 0 {
					t.Errorf("allocatable %s = %s, want %s", name, got.String(), raw)
				}
			}
		})
	}

	if _, err := FromCorpus(filepath.Join(renderCorpusRoot(), "_template")); err == nil {
		t.Errorf("FromCorpus accepted _template, which is not a machine")
	}
}

// renderCheckCorpusPowercap compares the capture's flat powercap tree with
// the rendered class/powercap view of it.
func renderCheckCorpusPowercap(t *testing.T, dir string, tree *Tree, rejecting []string) {
	t.Helper()
	captured := filepath.Join(dir, "powercap")
	zones, err := os.ReadDir(captured)
	if os.IsNotExist(err) {
		if m, _ := filepath.Glob(filepath.Join(tree.SysRoot, "class/powercap/*")); len(m) != 0 {
			t.Errorf("no powercap captured, but %v is rendered", m)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	rejects := map[string]bool{}
	for _, z := range rejecting {
		rejects[z] = true
	}
	class := filepath.Join(tree.SysRoot, "class", "powercap")
	for _, z := range zones {
		files, err := os.ReadDir(filepath.Join(captured, z.Name()))
		if err != nil {
			t.Fatal(err)
		}
		capturedFiles := map[string]bool{}
		for _, f := range files {
			capturedFiles[f.Name()] = true
			got := filepath.Join(class, z.Name(), f.Name())
			if rejects[z.Name()] && f.Name() == "constraint_0_power_limit_uw" {
				if !renderIsEISDIR(got) {
					t.Errorf("%s/%s accepts writes; machine.yaml lists the zone as rejecting", z.Name(), f.Name())
				}
				continue
			}
			want, _ := os.ReadFile(filepath.Join(captured, z.Name(), f.Name()))
			if b, err := os.ReadFile(got); err != nil || string(b) != string(want) {
				t.Errorf("%s/%s = %q (%v), want the captured %q", z.Name(), f.Name(), b, err, want)
			}
		}
		rendered, err := os.ReadDir(filepath.Join(class, z.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range rendered {
			if strings.HasPrefix(f.Name(), z.Name()+":") {
				continue // a child zone directory
			}
			if !capturedFiles[f.Name()] {
				t.Errorf("%s/%s is rendered but not captured", z.Name(), f.Name())
			}
		}
	}
}

// renderCheckCorpusNVML compares the NVML state with the captured query
// output: index, min, max, limit, draw, name.
func renderCheckCorpusNVML(t *testing.T, tree *Tree, text string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.Split(line, ", ")
		mw := func(field int) string {
			v, err := strconv.ParseFloat(parts[field], 64)
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%d\n", int64(math.Round(v*1000)))
		}
		for file, want := range map[string]string{
			"power_min_limit_mw": mw(1),
			"power_max_limit_mw": mw(2),
			"power_limit_mw":     mw(3),
			"name":               strings.Join(parts[5:], ", ") + "\n",
		} {
			if got := renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLGPUDir(i), file)); got != want {
				t.Errorf("gpu%d %s = %q, want %q", i, file, got, want)
			}
		}
		if i == 0 {
			if got := renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLGPUDir(0), "power_draw_mw")); got != mw(4) {
				t.Errorf("gpu0 power_draw_mw = %q, want %q", got, mw(4))
			}
		}
	}
}

// H21: an AMD capture becomes amdgpu hwmon devices with the captured limits
// and the legacy-agent-contract rocm-smi, a zone listed as rejecting that the
// tree lacks is an error, and GPUs that disagree on their limits are an
// error rather than a silently averaged profile.
func TestH21FromCorpusAMDAndErrors(t *testing.T) {
	t.Parallel()
	machine := func(t *testing.T, files map[string]string) string {
		dir := filepath.Join(t.TempDir(), "amd-test-machine")
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cpuinfo := "processor\t: 0\nvendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 17\nmodel name\t: AMD EPYC 9654 96-Core Processor\nstepping\t: 1\nphysical id\t: 0\ncore id\t\t: 0\ncpu cores\t: 1\nsiblings\t: 1\n\n"
	card := `{"Max Graphics Package Power (W)": "750.0", "Min Graphics Package Power (W)": "0.0", "Current Power Cap (W)": "600.0", "Average Graphics Package Power (W)": "131.0", "Card Series": "AMD Instinct MI300X", "Card SKU": "X0000000"}`
	files := map[string]string{
		"machine.yaml":     "description: test AMD machine\nallocatable:\n  cpu: \"1\"\n  memory: 1024Ki\n  gpu:\n    amd.com/gpu: \"2\"\nwriteRejectingZones: []\n",
		"cpuinfo":          cpuinfo,
		"node-labels.json": `{"feature.node.kubernetes.io/pci-1200_1002.present": "true"}`,
		"rocm-smi.txt":     `{"card0": ` + card + `, "card1": ` + card + `}`,
	}
	p, err := FromCorpus(machine(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if p.GPUs.PCI.Class.V != "0x120000" {
		t.Errorf("gpu class = %q, want 0x120000 from the captured NFD label", p.GPUs.PCI.Class.V)
	}
	tree := renderTestTree(t, p)
	if got := renderReadFile(t, tree, "state/tools.json"); got != `{"schemaVersion":1,"tools":{"rocm-smi":{"variant":"legacy-agent-contract"}}}`+"\n" {
		t.Errorf("tools.json = %s", got)
	}
	for i := 0; i < 2; i++ {
		link, _ := layout.HwmonClassLink(i+2, layout.PCIDevDir(0x10+i))
		for file, want := range map[string]string{"power1_cap": "600000000\n", "power1_cap_max": "750000000\n", "power1_cap_min": "0\n", "power1_average": "131000000\n"} {
			if got := renderReadFile(t, tree, "sys/"+link+"/"+file); got != want {
				t.Errorf("card%d %s = %q, want %q", i, file, got, want)
			}
		}
	}
	if got := renderReadFile(t, tree, "sys/"+layout.PCIDevDir(0x10)+"/vbios_version"); !strings.Contains(got, "-X0000000-") {
		t.Errorf("vbios_version = %q, want the captured Card SKU as its middle segment", got)
	}
	if got := renderQty(p.NodeObject("n").Status.Allocatable, "amd.com/gpu"); got != "2" {
		t.Errorf("allocatable amd.com/gpu = %q, want 2", got)
	}

	bad := map[string]string{}
	for k, v := range files {
		bad[k] = v
	}
	bad["machine.yaml"] = strings.Replace(files["machine.yaml"], "writeRejectingZones: []", "writeRejectingZones: [\"intel-rapl:0:0\"]", 1)
	if _, err := FromCorpus(machine(t, bad)); err == nil {
		t.Errorf("a rejecting zone without a powercap tree was accepted")
	}
	bad["machine.yaml"] = files["machine.yaml"]
	bad["rocm-smi.txt"] = `{"card0": ` + card + `, "card1": ` + strings.Replace(card, `"750.0"`, `"500.0"`, 1) + `}`
	if _, err := FromCorpus(machine(t, bad)); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("err = %v, want GPUs with different limits rejected", err)
	}
}
