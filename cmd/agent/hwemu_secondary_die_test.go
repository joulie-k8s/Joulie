package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// mi250xProfile is the test profile testdata/hwemu-profiles/
// amd-instinct-mi250x-4oam.yaml: four MI250X OAMs, eight amdgpu devices,
// the odd ones secondary dies.
const mi250xProfile = "amd-instinct-mi250x-4oam"

// mi250xNode renders the MI250X test profile and points the agent at it.
func mi250xNode(t *testing.T) (*hwemu.Node, *hwemu.Tree, *hwemu.Profile, *emulatedNode) {
	t.Helper()
	profiles, local, err := hwemu.LoadProfileSet(filepath.Join("testdata", "hwemu-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0] != mi250xProfile {
		t.Fatalf("test profiles %v, want %s", local, mi250xProfile)
	}
	p := profiles[mi250xProfile]
	if got := p.GPUs.SecondaryDies; len(got) != 4 {
		t.Fatalf("%s has secondary dies %v, want 1, 3, 5 and 7", mi250xProfile, got)
	}
	n, tree := hwemutest.NewNodeOf(t, p)
	return n, tree, p, useEmulatedNode(t, tree, nil)
}

// A34: on four MI250X, eight GCDs each listed as an amdgpu device, the
// second die of each OAM reads power1_cap 0 and power1_cap_default 0 with
// the package's 0 to 560 W range, and refuses writes with EINVAL. The agent
// lists all eight, so gpu.count matches the eight allocatable amd.com/gpu,
// gives a secondary die no cap of its own, and publishes the range, the
// 560 W cap in force and control from the primaries, and powerDomains 4,
// the GPUs that hold a limit, which the controller manager and the
// scheduler multiply the per-GPU maximum by. The status is pinned in
// testdata/hwemu/amd-instinct-mi250x-4oam.json, which their tests read as
// the agent's output (-update regenerates it).
func TestHwemuA34SecondaryDiesAreListedWithoutACap(t *testing.T) {
	_, _, p, e := mi250xNode(t)
	hwmons := amdgpuHwmonDirs(t, e)
	if len(hwmons) != 8 {
		t.Fatalf("%d amdgpu devices, want 8", len(hwmons))
	}
	for i, dir := range hwmons {
		capUW, defUW := "560000000", "560000000"
		if i%2 == 1 {
			capUW, defUW = "0", "0"
		}
		for f, want := range map[string]string{"power1_cap": capUW, "power1_cap_default": defUW, "power1_cap_min": "0", "power1_cap_max": "560000000"} {
			if got := readTrimmed(t, filepath.Join(dir, f)); got != want {
				t.Fatalf("device %d %s = %s, want %s, or this test proves nothing about a secondary die", i, f, got, want)
			}
		}
	}

	devs, err := listAmdDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 8 {
		t.Fatalf("listed %d devices, want all 8 GCDs", len(devs))
	}
	for i, d := range devs {
		if own := amdgpuOwnCap(d); own != (i%2 == 0) || d.MaxCapWatts != 560 {
			t.Errorf("device %d (%s) cap %v W, range %v to %v W, cap of its own %v; want one only on the primaries, every die with the 560 W maximum",
				i, d.BusID, d.CurrentCapWatts, d.MinCapWatts, d.MaxCapWatts, own)
		}
	}

	node := e.nodeObject(p)
	if q := node.Status.Allocatable["amd.com/gpu"]; q.Value() != 8 {
		t.Fatalf("allocatable amd.com/gpu %s, want 8", q.String())
	}
	hw := discoverHardware(context.Background(), node)
	status := publishedStatusMap(t, node.Name, hw)
	gpu, _ := status["gpu"].(map[string]any)
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	if gpu["vendor"] != "amd" || gpu["count"] != float64(8) || gpu["controlAvailable"] != true || gpu["currentCapWatts"] != float64(560) ||
		capRange["minWatts"] != float64(0) || capRange["maxWatts"] != float64(560) || capRange["defaultWatts"] != float64(560) {
		t.Fatalf("NodeHardware gpu %v; want 8 amd GPUs, capRangePerGpu 0 to 560 W with default 560, currentCapWatts 560 and control", gpu)
	}
	if gpu["powerDomains"] != float64(4) {
		t.Fatalf("NodeHardware gpu powerDomains %v, want 4: the four primaries hold the limit of the eight counted GPUs", gpu["powerDomains"])
	}

	got := publishedNodeHardwareStatus(t, node.Name, hw)
	golden := filepath.Join(hwemuGoldenDir, mi250xProfile+".json")
	if *updateCorpusGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v. Generate it with: go test ./cmd/agent/ -run Hwemu -update", err)
	}
	if string(got) != string(want) {
		t.Fatalf("NodeHardware status for %s does not match %s.\ngot:\n%s\nwant:\n%s\n"+
			"If the change is intended, review it and run: go test ./cmd/agent/ -run Hwemu -update",
			mi250xProfile, golden, got, want)
	}
}

// A34b: a 400 W cap and a 50 % cap on the MI250X node are host-amd applied
// on the four primaries, which read them back, and no write ever reaches a
// secondary die's power1_cap, which stays 0. The writes go through the
// node's CheckWrite, so a write to a secondary die would fail with EINVAL.
func TestHwemuA34bSecondaryDiesAreNeverWritten(t *testing.T) {
	n, tree, p, e := mi250xNode(t)
	checkWritesThroughNode(t, n, tree)
	writes := watchWrites(t, tree.SysRoot)
	node := e.nodeObject(p)
	hwmons := amdgpuHwmonDirs(t, e)
	devs, err := listAmdDevices()
	if err != nil || len(devs) != len(hwmons) {
		t.Fatalf("listed %d devices (%v), want %d", len(devs), err, len(hwmons))
	}
	nc := e.controller(t)
	for _, tc := range []struct {
		capW, capPct *float64
		want         string
	}{
		{capW: float64Ptr(400), want: "400"},
		{capPct: float64Ptr(50), want: "280"},
	} {
		dyn, err := reconcileEmulated(t, nc, node, nodeTwinWithGPUCap(e.name, tc.capW, tc.capPct))
		if err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" || s["message"] != "applied gpu cap "+tc.want+".0W on 4 devices" {
			t.Fatalf("gpu control status %v, want a %s W cap applied on the 4 primaries", s, tc.want)
		}
		for i, dir := range hwmons {
			want := tc.want + "000000"
			if i%2 == 1 {
				want = "0"
			}
			if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != want {
				t.Errorf("device %d power1_cap = %s after a %s W cap, want %s", i, got, tc.want, want)
			}
		}
		w := writes.take()
		if len(w) != 4 {
			t.Errorf("wrote %v, want the 4 primaries", w)
		}
		for _, entry := range w {
			if !strings.Contains(entry, "/power1_cap="+tc.want+"000000") {
				t.Errorf("wrote %s, want only power1_cap writes of %s W", entry, tc.want)
			}
			for i := 1; i < len(devs); i += 2 {
				if strings.Contains(entry, devs[i].BusID) {
					t.Errorf("wrote %s, the power1_cap of secondary die %d (%s)", entry, i, devs[i].BusID)
				}
			}
		}
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Errorf("the agent ran %v", calls)
	}
}
