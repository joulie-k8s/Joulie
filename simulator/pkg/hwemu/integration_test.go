package hwemu_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// These tests wire the renderer, the physics and the fake tools together, the
// way an emulated agent pod sees them, without the agent itself.

// corpusRoot is the hardware corpus the agent's tests replay, or
// JOULIE_HARDWARE_CORPUS when set, as the corpus test honours it.
func corpusRoot() string {
	if v := os.Getenv("JOULIE_HARDWARE_CORPUS"); v != "" {
		return v
	}
	return filepath.Join("..", "..", "..", "cmd", "agent", "testdata", "hardware")
}

// The agent's GPU inventory query (listNvidiaDevices in cmd/agent).
var agentNvidiaQuery = []string{"--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name", "--format=csv,noheader,nounits"}

// X01: every captured NVIDIA machine, mapped by FromCorpus and rendered,
// answers the agent's inventory query through the fake nvidia-smi with
// exactly the bytes the real one printed. Renderer and tool agree on the NVML
// state, so a corpus golden built from the capture holds for the emulated node
// too. Like the corpus tests, it names no machine: it runs every directory
// with an nvidia-smi.txt. The repository's corpus has at least one, so finding
// none there fails; another corpus without one skips.
func TestX01CorpusNvidiaQueryIsByteIdentical(t *testing.T) {
	t.Parallel()
	root := corpusRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("cannot read the corpus at %s: %v", root, err)
	}
	ran := 0
	for _, e := range entries {
		// _template and other underscore directories are not machines.
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		machine := e.Name()
		dir := filepath.Join(root, machine)
		want, err := os.ReadFile(filepath.Join(dir, "nvidia-smi.txt"))
		if err != nil {
			continue
		}
		ran++
		t.Run(machine, func(t *testing.T) {
			p, err := hwemu.FromCorpus(dir)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: "x01-" + machine})
			if err != nil {
				t.Fatal(err)
			}
			runner := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}
			got, err := runner.Run(context.Background(), "nvidia-smi", agentNvidiaQuery...)
			if err != nil {
				t.Fatalf("nvidia-smi query on the emulated %s: %v (%q)", machine, err, got)
			}
			if string(got) != string(want) {
				t.Fatalf("nvidia-smi query on the emulated %s:\n%q\nwant the captured\n%q", machine, got, want)
			}
		})
	}
	if ran == 0 {
		if os.Getenv("JOULIE_HARDWARE_CORPUS") == "" {
			t.Fatalf("no machine in %s has nvidia-smi.txt; the repository's corpus has NVIDIA captures, so the listing is wrong", root)
		}
		t.Skipf("no machine in %s has nvidia-smi.txt", root)
	}
}

// X02: a cap rocm-smi writes is what the physics enforces. On the MI300X
// node with the legacy rocm-smi, at full load, rocm-smi -d 0
// --setpoweroverdrive 500 and three seconds of node time bring device 0's
// power1_input to at most 505 W, while device 1, which nobody capped, stays
// at its natural power.
func TestX02RocmSMICapReachesThePhysics(t *testing.T) {
	t.Parallel()
	n, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", func(p *hwemu.Profile) {
		for i := range p.GPUs.Tools {
			if p.GPUs.Tools[i].Name == "rocm-smi" {
				p.GPUs.Tools[i].Variant = "legacy-agent-contract"
			}
		}
	})
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	inputs := amdgpuPowerInputs(t, tree)
	if len(inputs) != p.GPUs.Count.V {
		t.Fatalf("%d amdgpu power1_input files, want %d", len(inputs), p.GPUs.Count.V)
	}
	if w := readWatts(t, inputs[0]); w <= 505 {
		t.Fatalf("device 0 draws %.1f W uncapped at full load; the test needs it above 505 W to mean anything", w)
	}

	runner := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}
	if out, err := runner.Run(context.Background(), "rocm-smi", "-d", "0", "--setpoweroverdrive", "500"); err != nil {
		t.Fatalf("rocm-smi -d 0 --setpoweroverdrive 500: %v (%q)", err, out)
	}
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if w := readWatts(t, inputs[0]); w > 505 {
		t.Fatalf("device 0 power1_input is %.3f W three seconds after a 500 W cap, want <= 505 W", w)
	}
	if w := readWatts(t, inputs[1]); w <= 505 {
		t.Fatalf("device 1 power1_input is %.3f W; only device 0 was capped, so it should still draw its natural power", w)
	}
}

// amdgpuPowerInputs lists power1_input of every amdgpu hwmon device, in the
// order the tools number the GPUs (DRM cards in numeric order, AMD only).
func amdgpuPowerInputs(t *testing.T, tree *hwemu.Tree) []string {
	t.Helper()
	dir := filepath.Join(tree.SysRoot, "class", "drm")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byCard := map[int]string{}
	maxCard := -1
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "card")
		c, err := strconv.Atoi(n)
		if !ok || err != nil {
			continue
		}
		dev := filepath.Join(dir, e.Name(), "device")
		if v, err := os.ReadFile(filepath.Join(dev, "vendor")); err != nil || strings.TrimSpace(string(v)) != "0x1002" {
			continue
		}
		hw, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*", "power1_input"))
		if len(hw) != 1 {
			t.Fatalf("card%d has %d power1_input files, want 1", c, len(hw))
		}
		byCard[c] = hw[0]
		maxCard = max(maxCard, c)
	}
	var out []string
	for c := 0; c <= maxCard; c++ {
		if f, ok := byCard[c]; ok {
			out = append(out, f)
		}
	}
	return out
}

// readWatts reads a hwmon power file, in microwatts, as watts.
func readWatts(t *testing.T, file string) float64 {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	uw, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return float64(uw) / 1e6
}
