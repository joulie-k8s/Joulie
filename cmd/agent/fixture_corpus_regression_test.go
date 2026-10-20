package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// TestCorpusReplayIgnoresTheHostSysfs: the corpus replay reads the capture
// and never the /sys of the machine the test runs on. Governor discovery
// reads the host's /sys through dvfs.HostSysRoot, which pointAgentAtFixture
// left alone while it redirected every other host path, so on a machine with
// a /host-sys tree (an agent image, a runner that mounts the host) every
// golden picked up that host's cpufreq governor and frequency range. A
// populated tree stands in for /host-sys here, and every machine with a
// golden must still publish exactly its golden.
func TestCorpusReplayIgnoresTheHostSysfs(t *testing.T) {
	t.Setenv("HARDWARE_CATALOG_PATH", "")

	host := t.TempDir()
	policy := filepath.Join(host, "devices", "system", "cpu", "cpufreq", "policy0")
	if err := os.MkdirAll(policy, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"scaling_driver":              "acpi-cpufreq\n",
		"scaling_governor":            "userspace\n",
		"scaling_available_governors": "userspace performance \n",
		"cpuinfo_min_freq":            "300000\n",
		"cpuinfo_max_freq":            "1500000\n",
	} {
		if err := os.WriteFile(filepath.Join(policy, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(host, "devices", "system", "cpu", "cpufreq", "boost"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHostSys := dvfs.HostSysRoot
	dvfs.HostSysRoot = host
	t.Cleanup(func() { dvfs.HostSysRoot = oldHostSys })

	for _, machine := range corpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(corpusRoot(), machine)
			want, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if errors.Is(err, os.ErrNotExist) {
				t.Skipf("%s has no expected.json", machine)
			}
			if err != nil {
				t.Fatal(err)
			}
			meta := readCorpusMachine(t, dir)
			hideHostGPUProbe(t, dir)
			pointAgentAtFixture(t, dir, copyCorpusPowercap(t, dir))
			node := corpusNode(t, machine, dir, meta)

			got := publishedNodeHardwareStatus(t, node.Name, discoverHardware(context.Background(), node))
			if string(got) != string(want) {
				t.Fatalf("with a /host-sys tree on the test machine, %s publishes a status that differs from its golden: "+
					"the replay read the host.\ngot:\n%s\nwant:\n%s", machine, got, want)
			}
		})
	}
}

// TestCorpusNvidiaTokenCaptureReplaysTheSameBothWays: a capture whose
// nvidia-smi.txt prints [N/A] for the minimum and maximum limits, as a GPU
// without a settable limit prints them, passes the validator and publishes
// the same NodeHardware status through the flat replay and through the
// emulated one: the T4 listed with its 70 W limit in force, no
// capRangePerGpu, no control and a warning naming the token. The corpus
// holds no such machine, so this builds one from vm-no-rapl.
func TestCorpusNvidiaTokenCaptureReplaysTheSameBothWays(t *testing.T) {
	t.Setenv("HARDWARE_CATALOG_PATH", "")
	// The T4 capture of the repository's corpus, whatever corpus is plugged in.
	src := filepath.Join(corpusDir, "vm-no-rapl")
	root := t.TempDir()
	const machine = "vm-token-limits"
	dir := filepath.Join(root, machine)
	for _, f := range []string{"cpuinfo", "machine.yaml", "node-labels.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		corpusWrite(t, dir, f, string(b))
	}
	corpusWrite(t, dir, "nvidia-smi.txt", "0, [N/A], [N/A], 70.00, 9.97, Tesla T4\n")
	if msgs := validateCorpusMachine(root, machine); len(msgs) != 0 {
		t.Fatalf("the capture was rejected:\n%s", strings.Join(msgs, "\n"))
	}

	hideHostGPUProbe(t, dir)
	pointAgentAtFixture(t, dir, copyCorpusPowercap(t, dir))
	node := corpusNode(t, machine, dir, readCorpusMachine(t, dir))
	flat := publishedNodeHardwareStatus(t, node.Name, discoverHardware(context.Background(), node))
	var status struct {
		GPU map[string]any `json:"gpu"`
	}
	if err := json.Unmarshal(flat, &status); err != nil {
		t.Fatal(err)
	}
	const note = "no settable power limit on gpu 0: nvidia-smi prints power.min_limit [N/A], power.max_limit [N/A]"
	if _, ok := status.GPU["capRangePerGpu"]; ok || status.GPU["count"] != float64(1) || status.GPU["currentCapWatts"] != float64(70) ||
		status.GPU["controlAvailable"] != false || !containsAny(status.GPU["warnings"], note) {
		t.Fatalf("the replayed capture publishes gpu %v; want 1 GPU, 70 W in force, no capRangePerGpu, no control and the warning %q", status.GPU, note)
	}

	p, err := hwemu.FromCorpus(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: node.Name})
	if err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, tree, nil)
	emulated := publishedNodeHardwareStatus(t, node.Name, discoverHardware(context.Background(), e.nodeObject(p)))
	if string(emulated) != string(flat) {
		t.Fatalf("the emulated capture publishes a status that differs from the flat replay.\nemulated:\n%s\nflat:\n%s", emulated, flat)
	}
}
