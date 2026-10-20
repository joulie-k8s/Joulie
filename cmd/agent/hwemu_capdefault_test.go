package main

import (
	"context"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// withNAField makes nvidia-smi print [N/A] for one query field.
func withNAField(field string) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) { p.GPUs.FieldSupport = map[string]string{field: "[N/A]"} }
}

// A33: capRangePerGpu.defaultWatts is the driver's default limit where the
// agent reads one it can trust, and the maximum otherwise. The H100 NVL GPUs
// default to 310 W below their 400 W maximum and run at 400 W: NodeHardware
// publishes defaultWatts 310 from power.default_limit, next to maxWatts 400
// and currentCapWatts 400. Where nvidia-smi prints [N/A] for the default,
// defaultWatts is the maximum. On the MI300X with power1_cap_default at
// 600 W below the 750 W maximum, defaultWatts is the maximum, because the
// driver rewrites power1_cap_default with the cap in force and the agent
// does not read it.
func TestHwemuA33GPUCapDefaultIsTheDriversWhereReliable(t *testing.T) {
	for _, tc := range []struct {
		name, profile        string
		mutate               func(*hwemu.Profile)
		wantDefault, wantMax float64
		wantCurrent          float64
	}{
		{"nvidia default limit", "nvidia-h100-nvl-8gpu", nil, 310, 400, 400},
		{"nvidia default limit [N/A]", "nvidia-h100-nvl-8gpu", withNAField("power.default_limit"), 400, 400, 400},
		{"amd power1_cap_default not read", "amd-instinct-mi300x-8gpu", withDefaultLimit(600), 750, 750, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tree, p := hwemutest.NewNode(t, tc.profile, tc.mutate)
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
			gpu, _ := status["gpu"].(map[string]any)
			capRange, _ := gpu["capRangePerGpu"].(map[string]any)
			if capRange["defaultWatts"] != tc.wantDefault || capRange["maxWatts"] != tc.wantMax || gpu["currentCapWatts"] != tc.wantCurrent {
				t.Fatalf("NodeHardware gpu capRangePerGpu %v, currentCapWatts %v; want defaultWatts %v, maxWatts %v, currentCapWatts %v",
					capRange, gpu["currentCapWatts"], tc.wantDefault, tc.wantMax, tc.wantCurrent)
			}
		})
	}
}

// The default limit is the seventh field of the inventory line, read only
// when it is a positive number; gpuCapDefaultWatts publishes it only within
// the cap range, and the maximum for any other.
func TestNvidiaDefaultLimitIsTheSeventhField(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]float64{
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL, 310.00":          310,
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL":                  0,
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL, [N/A]":           0,
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL, [Not Supported]": 0,
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL, NaN":             0,
		"0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL, 0.00":            0,
	} {
		if got := nvidiaDefaultLimit(splitCSVLine(line)); got != want {
			t.Errorf("nvidiaDefaultLimit(%q) = %v, want %v", line, got, want)
		}
	}
	for _, tc := range []struct{ def, want float64 }{{310, 310}, {0, 400}, {150, 400}, {450, 400}, {200, 200}, {400, 400}} {
		hw := HardwareInfo{GPUCapMinWatts: 200, GPUCapMaxWatts: 400, GPUCapDefaultW: tc.def}
		if got := gpuCapDefaultWatts(hw); got != tc.want {
			t.Errorf("default %v in 200 to 400 W publishes %v, want %v", tc.def, got, tc.want)
		}
	}
}
