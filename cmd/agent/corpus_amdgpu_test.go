package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// An AMD capture, laid out as scripts/collect-hardware-fixture.sh writes it,
// passes the validator and publishes the same NodeHardware status through
// the flat corpus replay (TestHardwareFixtureCorpus) and through the emulated
// one (TestHwemuA01): the corpus holds no AMD machine, so this builds one.
// Its cap, 300 W, is below the 350 W default, itself below the 400 W
// maximum, so the status shows each attribute reaching its field: the cap
// as currentCapWatts and power1_cap_max as the published maximum, while the
// default reaches the emulated node as the limit its GPUs start at.
func TestCorpusAMDGPUCaptureReplaysTheSameBothWays(t *testing.T) {
	t.Setenv("HARDWARE_CATALOG_PATH", "")
	root := t.TempDir()
	const machine = "amd-test-capture"
	dir := filepath.Join(root, machine)
	corpusWrite(t, dir, "machine.yaml", `description: Two AMD GPUs on an Intel host, built by a test.
source: cmd/agent/corpus_amdgpu_test.go
capturedAt: "2026-01-31T09:00:00Z"
allocatable:
  cpu: "2"
  memory: "16384Ki"
  gpu:
    amd.com/gpu: "2"
writeRejectingZones: []
`)
	corpusWrite(t, dir, "cpuinfo", corpusGoodCPUInfo)
	corpusWrite(t, dir, "node-labels.json", `{"feature.node.kubernetes.io/pci-1200_1002.present": "true", "amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM"}`+"\n")
	for i := 0; i < 2; i++ {
		g := fmt.Sprintf("%s/gpu%d/", corpusAMDGPUDir, i)
		corpusWrite(t, dir, g+"power1_cap", "300000000\n")
		corpusWrite(t, dir, g+"power1_cap_min", "100000000\n")
		corpusWrite(t, dir, g+"power1_cap_max", "400000000\n")
		corpusWrite(t, dir, g+"power1_cap_default", "350000000\n")
	}
	if msgs := validateCorpusMachine(root, machine); len(msgs) != 0 {
		t.Fatalf("the AMD capture was rejected:\n%s", strings.Join(msgs, "\n"))
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
	capRange, _ := status.GPU["capRangePerGpu"].(map[string]any)
	if status.GPU["vendor"] != "amd" || status.GPU["count"] != float64(2) || status.GPU["currentCapWatts"] != float64(300) ||
		status.GPU["controlAvailable"] != true || capRange["minWatts"] != float64(100) || capRange["maxWatts"] != float64(400) ||
		status.GPU["model"] != "AMD_INSTINCT_MI300X" {
		t.Fatalf("the replayed AMD capture publishes gpu %v; want amd, 2 GPUs, cap 300 W, 100 to 400 W with control, model AMD_INSTINCT_MI300X", status.GPU)
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
		t.Fatalf("the emulated AMD capture publishes a status that differs from the flat replay.\nemulated:\n%s\nflat:\n%s", emulated, flat)
	}
}
