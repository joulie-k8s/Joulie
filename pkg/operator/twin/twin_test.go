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
