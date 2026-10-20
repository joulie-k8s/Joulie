package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/controller/twin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mi250xNodeHardware is the NodeHardware the agent publishes for its MI250X
// test profile, four OAMs of two dies each: the agent's golden
// cmd/agent/testdata/hwemu/amd-instinct-mi250x-4oam.json, eight counted
// GPUs, powerDomains 4 and the package's 560 W maximum. With
// dropPowerDomains the status is the one an older agent publishes, without
// powerDomains.
func mi250xNodeHardware(t *testing.T, dropPowerDomains bool) *v1alpha1.NodeHardware {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "hwemu", "amd-instinct-mi250x-4oam.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatal(err)
	}
	gpu, _ := status["gpu"].(map[string]any)
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	if gpu["count"] != float64(8) || gpu["powerDomains"] != float64(4) || capRange["maxWatts"] != float64(560) {
		t.Fatalf("the agent's MI250X status has gpu %v; want count 8, powerDomains 4 and maxWatts 560, or this test proves nothing", gpu)
	}
	if dropPowerDomains {
		delete(gpu, "powerDomains")
	}
	if b, err = json.Marshal(status); err != nil {
		t.Fatal(err)
	}
	obj := &v1alpha1.NodeHardware{ObjectMeta: metav1.ObjectMeta{Name: "mi250x"}, Spec: v1alpha1.NodeHardwareSpec{NodeName: "mi250x"}}
	if err := json.Unmarshal(b, &obj.Status); err != nil {
		t.Fatal(err)
	}
	return obj
}

// The twin's GPU TDP and capped budget count the per-GPU maximum once per
// GPU that holds a power limit of its own. On four MI250X the agent counts
// eight GPUs and publishes powerDomains 4: the GPU TDP is 4 x 560 W = 2240 W
// and the 60 % budget 1344 W, not twice that. Without powerDomains, as from
// an older agent, the count stands for it.
func TestGPUPowerDomainsSetTheTwinBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drop    bool
		wantTDP float64
	}{
		{"powerDomains published", false, 2240},
		{"no powerDomains, the count", true, 4480},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := mi250xNodeHardware(t, tc.drop)
			hw := fetchNodeHardware(context.Background(), fakeReader(t, obj), "mi250x")
			pm := twin.Compute(twin.Input{NodeName: "mi250x", Hardware: hw, Profile: "eco", GPUCapPct: 60}).PowerMeasurement
			if pm.GpuTdpW != tc.wantTDP || pm.GpuCappedPowerW != 0.6*tc.wantTDP {
				t.Fatalf("gpuTdpW %v, gpuCappedPowerW %v; want %v and %v", pm.GpuTdpW, pm.GpuCappedPowerW, tc.wantTDP, 0.6*tc.wantTDP)
			}
		})
	}
}

// The planned power of a node, which the rack PSU estimate adds up, counts
// the GPU cap once per GPU that holds a limit: a 60 % cap on four MI250X is
// 4 x 336 W = 1344 W, since the cap written to each primary limits both
// dies of its package. Without powerDomains the count stands for it.
func TestGPUPowerDomainsSetThePlannedNodePower(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drop  bool
		wantW float64
	}{
		{"powerDomains published", false, 1344},
		{"no powerDomains, the count", true, 2688},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nh := parseNodeHardware(mi250xNodeHardware(t, tc.drop))
			if got := estimateNodePowerW(nh, 100, 60); got != tc.wantW {
				t.Fatalf("planned power %v W at a 60 %% GPU cap, want %v W", got, tc.wantW)
			}
		})
	}
}
