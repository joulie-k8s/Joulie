package twin

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestComputeEcoProfile(t *testing.T) {
	in := Input{
		NodeName: "node1",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				TotalCores: 192,
				Sockets:    2,
				CapRange:   joulie.CPUCapRange{MaxWattsPerSocket: 360},
			},
			GPU: joulie.NodeHardwareGPU{Present: true, Count: 8,
				CapRange: joulie.GPUCapRange{MaxWatts: 400}},
		},
		Profile:   "eco",
		CPUCapPct: 60,
		GPUCapPct: 60,
	}
	out := Compute(in)

	if out.SchedulableClass != "eco" {
		t.Errorf("expected eco, got %s", out.SchedulableClass)
	}
	if out.EffectiveCapState.CPUPct != 60 {
		t.Errorf("expected CPUPct=60, got %f", out.EffectiveCapState.CPUPct)
	}
	if out.HardwareDensityScore <= 0 || out.HardwareDensityScore > 100 {
		t.Errorf("invalid density score: %f", out.HardwareDensityScore)
	}
}

func TestComputeDraining(t *testing.T) {
	in := Input{
		NodeName:  "node1",
		Profile:   "performance",
		Draining:  true,
		CPUCapPct: 100,
		GPUCapPct: 100,
	}
	out := Compute(in)
	if out.SchedulableClass != "draining" {
		t.Errorf("expected draining, got %s", out.SchedulableClass)
	}
}

func TestRescheduleRecommendations(t *testing.T) {
	in := Input{
		NodeName: "node1",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 360},
			},
			GPU: joulie.NodeHardwareGPU{
				Present:  true,
				Count:    8,
				CapRange: joulie.GPUCapRange{MaxWatts: 400},
			},
		},
		Profile:            "eco",
		CPUCapPct:          100, // max power = high cooling stress
		GPUCapPct:          100,
		ClusterTotalPowerW: 45000, // high PSU stress
		Workloads: []joulie.WorkloadProfileStatus{
			{
				Criticality:   joulie.WorkloadCriticality{Class: "best-effort"},
				Migratability: joulie.WorkloadMigratability{Reschedulable: true},
			},
		},
	}
	out := Compute(in)
	if len(out.RescheduleRecommendations) == 0 {
		t.Errorf("expected reschedule recommendations under high stress with reschedulable best-effort workloads")
	}
}

func TestGPUSlicingRecommendationNoGPU(t *testing.T) {
	in := Input{
		NodeName: "cpu-only",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{TotalCores: 64},
			GPU: joulie.NodeHardwareGPU{Present: false},
		},
		Profile:   "eco",
		CPUCapPct: 60,
		GPUCapPct: 100,
	}
	out := Compute(in)
	if out.GPUSlicingRecommendation != nil {
		t.Error("expected nil GPU slicing recommendation for CPU-only node")
	}
}

func TestGPUSlicingRecommendationSlicingNotSupported(t *testing.T) {
	in := Input{
		NodeName: "gpu-no-mig",
		Hardware: joulie.NodeHardware{
			GPU: joulie.NodeHardwareGPU{
				Present: true, Count: 4,
				Slicing: joulie.GPUSlicing{Supported: false},
			},
		},
		Profile:   "performance",
		CPUCapPct: 100,
		GPUCapPct: 100,
	}
	out := Compute(in)
	if out.GPUSlicingRecommendation != nil {
		t.Error("expected nil GPU slicing recommendation when slicing not supported")
	}
}

func TestGPUSlicingRecommendationNoWorkloads(t *testing.T) {
	in := Input{
		NodeName: "gpu-idle",
		Hardware: joulie.NodeHardware{
			GPU: joulie.NodeHardwareGPU{
				Present: true, Count: 4,
				Slicing: joulie.GPUSlicing{Supported: true, Modes: []string{"mig", "time-slicing"}},
			},
		},
		Profile:   "eco",
		CPUCapPct: 60,
		GPUCapPct: 60,
	}
	out := Compute(in)
	if out.GPUSlicingRecommendation == nil {
		t.Fatal("expected GPU slicing recommendation for idle GPU node with slicing support")
	}
	if out.GPUSlicingRecommendation.Mode != "time-slicing" {
		t.Errorf("expected time-slicing default, got %s", out.GPUSlicingRecommendation.Mode)
	}
}

func TestGPUSlicingRecommendationLowIntensity(t *testing.T) {
	in := Input{
		NodeName: "gpu-node",
		Hardware: joulie.NodeHardware{
			GPU: joulie.NodeHardwareGPU{
				Present: true, Count: 8,
				Slicing: joulie.GPUSlicing{Supported: true, Modes: []string{"mig"}},
			},
		},
		Profile:   "eco",
		CPUCapPct: 60,
		GPUCapPct: 60,
		Workloads: []joulie.WorkloadProfileStatus{
			{GPU: joulie.WorkloadGPUProfile{Intensity: "low"}},
			{GPU: joulie.WorkloadGPUProfile{Intensity: "low"}},
			{GPU: joulie.WorkloadGPUProfile{Intensity: "medium"}},
		},
	}
	out := Compute(in)
	rec := out.GPUSlicingRecommendation
	if rec == nil {
		t.Fatal("expected GPU slicing recommendation")
	}
	if rec.Mode != "mig" || rec.SliceType != "1g.10gb" {
		t.Errorf("expected mig/1g.10gb for low-intensity workloads, got %s/%s", rec.Mode, rec.SliceType)
	}
	if rec.TotalSlices != 7*8 {
		t.Errorf("expected 56 total slices (7×8 GPUs), got %d", rec.TotalSlices)
	}
}

func TestGPUSlicingRecommendationHighIntensity(t *testing.T) {
	in := Input{
		NodeName: "gpu-node",
		Hardware: joulie.NodeHardware{
			GPU: joulie.NodeHardwareGPU{
				Present: true, Count: 4,
				Slicing: joulie.GPUSlicing{Supported: true, Modes: []string{"mig"}},
			},
		},
		Profile:   "performance",
		CPUCapPct: 100,
		GPUCapPct: 100,
		Workloads: []joulie.WorkloadProfileStatus{
			{GPU: joulie.WorkloadGPUProfile{Intensity: "high"}},
			{GPU: joulie.WorkloadGPUProfile{Intensity: "high"}},
			{GPU: joulie.WorkloadGPUProfile{Intensity: "medium"}},
		},
	}
	out := Compute(in)
	rec := out.GPUSlicingRecommendation
	if rec == nil {
		t.Fatal("expected GPU slicing recommendation")
	}
	if rec.Mode != "none" {
		t.Errorf("expected whole-GPU (none) for high-intensity workloads, got %s", rec.Mode)
	}
	if rec.TotalSlices != 4 {
		t.Errorf("expected 4 total slices (1×4 GPUs), got %d", rec.TotalSlices)
	}
}

func TestComputeHardwareDensityScore(t *testing.T) {
	hw := joulie.NodeHardware{
		CPU: joulie.NodeHardwareCPU{TotalCores: 192},
		GPU: joulie.NodeHardwareGPU{Present: true, Count: 8},
	}
	score := ComputeHardwareDensityScore(hw)
	if score < 90 || score > 100 {
		t.Errorf("expected near-100 score for full node, got %f", score)
	}

	hw2 := joulie.NodeHardware{
		CPU: joulie.NodeHardwareCPU{TotalCores: 64},
		GPU: joulie.NodeHardwareGPU{Present: false},
	}
	score2 := ComputeHardwareDensityScore(hw2)
	if score2 >= score {
		t.Errorf("smaller node should have lower density score")
	}
}
