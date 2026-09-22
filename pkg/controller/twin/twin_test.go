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

// Edge case tests

func TestComputeUnknownProfile(t *testing.T) {
	out := Compute(Input{NodeName: "n", Profile: "bogus"})
	if out.SchedulableClass != "unknown" {
		t.Errorf("expected unknown for invalid profile, got %s", out.SchedulableClass)
	}
}

func TestComputeEmptyProfile(t *testing.T) {
	out := Compute(Input{NodeName: "n", Profile: ""})
	if out.SchedulableClass != "unknown" {
		t.Errorf("expected unknown for empty profile, got %s", out.SchedulableClass)
	}
}

func TestComputeZeroCapsDefaultToFull(t *testing.T) {
	out := Compute(Input{
		NodeName:  "n",
		Profile:   "performance",
		CPUCapPct: 0,
		GPUCapPct: 0,
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
		},
	})
	if out.EffectiveCapState.CPUPct != 100 {
		t.Errorf("expected CPU cap default to 100, got %f", out.EffectiveCapState.CPUPct)
	}
	if out.EffectiveCapState.GPUPct != 100 {
		t.Errorf("expected GPU cap default to 100, got %f", out.EffectiveCapState.GPUPct)
	}
	// With nodeCappedPower > 0 and measuredPower = 0, headroom should be 100.
	if out.PredictedPowerHeadroomScore != 100 {
		t.Errorf("headroom should be 100 when measured power is 0, got %f", out.PredictedPowerHeadroomScore)
	}
}

func TestComputeNegativeCapsDefaultToFull(t *testing.T) {
	out := Compute(Input{
		NodeName:  "n",
		Profile:   "eco",
		CPUCapPct: -10,
		GPUCapPct: -5,
	})
	if out.EffectiveCapState.CPUPct != 100 {
		t.Errorf("expected CPU cap default to 100 for negative, got %f", out.EffectiveCapState.CPUPct)
	}
}

func TestComputeZeroHardwareNoPanic(t *testing.T) {
	// Ensure zero-value hardware doesn't cause division by zero or panic
	out := Compute(Input{
		NodeName:  "empty",
		Profile:   "eco",
		CPUCapPct: 60,
		GPUCapPct: 60,
	})
	if out.HardwareDensityScore != 0 {
		t.Errorf("expected 0 density for zero hardware, got %f", out.HardwareDensityScore)
	}
	// nodeCappedPower = 0 → headroom = 100 (neutral for unknown hardware)
	if out.PredictedPowerHeadroomScore != 100 {
		t.Errorf("expected headroom 100 for zero hardware, got %f", out.PredictedPowerHeadroomScore)
	}
	// nodeTDP = 0 → cooling stress = 0
	if out.PredictedCoolingStressScore != 0 {
		t.Errorf("expected 0 cooling stress for zero hardware, got %f", out.PredictedCoolingStressScore)
	}
}

func TestComputeExtremeClusterPower(t *testing.T) {
	out := Compute(Input{
		NodeName:           "n",
		Profile:            "performance",
		CPUCapPct:          100,
		GPUCapPct:          100,
		ClusterTotalPowerW: 1e9, // absurdly high
	})
	if out.PredictedPsuStressScore != 100 {
		t.Errorf("expected PSU stress capped at 100, got %f", out.PredictedPsuStressScore)
	}
}

// --- Power headroom tests ---

func TestPowerHeadroomBasic(t *testing.T) {
	// 2×350W CPU + 8×700W GPU = nodeTDP 6300W
	// cpuCapPct=60 → cpuCapped=420, gpuCapPct=40 → gpuCapped=2240
	// nodeCappedPower = 2660W, measured = 1500W
	// headroom = (2660-1500)/2660*100 = 43.6%
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
			GPU: joulie.NodeHardwareGPU{Present: true, Count: 8,
				CapRange: joulie.GPUCapRange{MaxWatts: 700}},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		GPUCapPct:          40,
		MeasuredNodePowerW: 1500,
	}
	out := Compute(in)
	// Expected: (2660 - 1500) / 2660 * 100 ≈ 43.6
	if out.PredictedPowerHeadroomScore < 43 || out.PredictedPowerHeadroomScore > 44 {
		t.Errorf("expected headroom ~43.6, got %f", out.PredictedPowerHeadroomScore)
	}
}

func TestPowerHeadroomNegative(t *testing.T) {
	// Measured power exceeds capped budget → headroom goes negative (not clamped to 0)
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  1,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 200},
			},
		},
		Profile:            "eco",
		CPUCapPct:          50,
		MeasuredNodePowerW: 150, // exceeds 100W capped budget
	}
	out := Compute(in)
	if out.PredictedPowerHeadroomScore >= 0 {
		t.Errorf("expected negative headroom when over budget, got %f", out.PredictedPowerHeadroomScore)
	}
}

func TestPowerHeadroomCappedAt100(t *testing.T) {
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
		},
		Profile:            "performance",
		CPUCapPct:          100,
		MeasuredNodePowerW: 0,
	}
	out := Compute(in)
	if out.PredictedPowerHeadroomScore != 100 {
		t.Errorf("expected headroom 100 when no power drawn, got %f", out.PredictedPowerHeadroomScore)
	}
}

// --- Cooling stress tests ---

func TestCoolingStressBasic(t *testing.T) {
	// nodeTDP = 2*350 = 700W, measured = 350W, temp = 20 (multiplier = 1.0)
	// stress = (350/700) * 1.0 * 100 = 50
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		MeasuredNodePowerW: 350,
		OutsideTempC:       20,
	}
	out := Compute(in)
	if out.PredictedCoolingStressScore < 49.5 || out.PredictedCoolingStressScore > 50.5 {
		t.Errorf("expected cooling stress ~50, got %f", out.PredictedCoolingStressScore)
	}
}

func TestCoolingStressHighTemp(t *testing.T) {
	// nodeTDP = 700W, measured = 350W, temp = 45 → multiplier = 1/max(0.5, 1-25*0.02) = 1/0.5 = 2.0
	// stress = (350/700) * 2.0 * 100 = 100
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		MeasuredNodePowerW: 350,
		OutsideTempC:       45,
	}
	out := Compute(in)
	if out.PredictedCoolingStressScore != 100 {
		t.Errorf("expected cooling stress 100 at extreme temp, got %f", out.PredictedCoolingStressScore)
	}
}

func TestCoolingStressUnknownTemp(t *testing.T) {
	// temp = 0 (unknown) → multiplier = 1.0
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		MeasuredNodePowerW: 350,
		OutsideTempC:       0,
	}
	out := Compute(in)
	// Same as 20C: (350/700)*1.0*100 = 50
	if out.PredictedCoolingStressScore < 49.5 || out.PredictedCoolingStressScore > 50.5 {
		t.Errorf("expected cooling stress ~50 for unknown temp, got %f", out.PredictedCoolingStressScore)
	}
}

// --- Power measurement output tests ---

func TestPowerMeasurementOutput(t *testing.T) {
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  2,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 350},
			},
			GPU: joulie.NodeHardwareGPU{Present: true, Count: 8,
				CapRange: joulie.GPUCapRange{MaxWatts: 700}},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		GPUCapPct:          40,
		MeasuredNodePowerW: 1500,
		PowerTrendWPerMin:  -120,
	}
	out := Compute(in)
	pm := out.PowerMeasurement

	if pm.CpuTdpW != 700 {
		t.Errorf("expected cpuTDP 700, got %f", pm.CpuTdpW)
	}
	if pm.GpuTdpW != 5600 {
		t.Errorf("expected gpuTDP 5600, got %f", pm.GpuTdpW)
	}
	if pm.NodeTdpW != 6300 {
		t.Errorf("expected nodeTDP 6300, got %f", pm.NodeTdpW)
	}
	if pm.CpuCappedPowerW != 420 {
		t.Errorf("expected cpuCapped 420, got %f", pm.CpuCappedPowerW)
	}
	if pm.GpuCappedPowerW != 2240 {
		t.Errorf("expected gpuCapped 2240, got %f", pm.GpuCappedPowerW)
	}
	if pm.NodeCappedPowerW != 2660 {
		t.Errorf("expected nodeCapped 2660, got %f", pm.NodeCappedPowerW)
	}
	if pm.MeasuredNodePowerW != 1500 {
		t.Errorf("expected measuredPower 1500, got %f", pm.MeasuredNodePowerW)
	}
	if pm.PowerTrendWPerMin != -120 {
		t.Errorf("expected trend -120, got %f", pm.PowerTrendWPerMin)
	}
}

// --- Topology-aware tests ---

func TestPSUStressUsesPerRackPower(t *testing.T) {
	inCluster := Input{
		NodeName:           "n",
		Profile:            "performance",
		CPUCapPct:          100,
		GPUCapPct:          100,
		ClusterTotalPowerW: 40000,
	}
	inRack := Input{
		NodeName:           "n",
		Profile:            "performance",
		CPUCapPct:          100,
		GPUCapPct:          100,
		ClusterTotalPowerW: 40000,
		Rack:               "rack-1",
		RackTotalPowerW:    10000,
	}

	outCluster := Compute(inCluster)
	outRack := Compute(inRack)

	if outRack.PredictedPsuStressScore >= outCluster.PredictedPsuStressScore {
		t.Errorf("per-rack PSU stress should be lower than cluster-wide: rack=%.1f cluster=%.1f",
			outRack.PredictedPsuStressScore, outCluster.PredictedPsuStressScore)
	}
}

func TestPSUStressFallsBackToClusterWide(t *testing.T) {
	in := Input{
		NodeName:           "n",
		Profile:            "performance",
		CPUCapPct:          100,
		GPUCapPct:          100,
		ClusterTotalPowerW: 30000,
		Rack:               "rack-1",
		RackTotalPowerW:    0,
	}
	out := Compute(in)
	if out.PredictedPsuStressScore < 50 || out.PredictedPsuStressScore > 70 {
		t.Errorf("expected PSU stress around 60 from cluster power, got %.1f", out.PredictedPsuStressScore)
	}
}

func TestCoolingStressUsesPerZoneAmbient(t *testing.T) {
	hw := joulie.NodeHardware{
		CPU: joulie.NodeHardwareCPU{
			Sockets:  2,
			CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 360},
		},
	}
	inCool := Input{
		NodeName:           "n",
		Hardware:           hw,
		Profile:            "eco",
		CPUCapPct:          60,
		GPUCapPct:          60,
		OutsideTempC:       18,
		CoolingZone:        "zone-cool",
		MeasuredNodePowerW: 300,
	}
	inHot := Input{
		NodeName:           "n",
		Hardware:           hw,
		Profile:            "eco",
		CPUCapPct:          60,
		GPUCapPct:          60,
		OutsideTempC:       35,
		CoolingZone:        "zone-hot",
		MeasuredNodePowerW: 300,
	}

	outCool := Compute(inCool)
	outHot := Compute(inHot)

	if outCool.PredictedCoolingStressScore >= outHot.PredictedCoolingStressScore {
		t.Errorf("hot zone should have higher cooling stress: cool=%.1f hot=%.1f",
			outCool.PredictedCoolingStressScore, outHot.PredictedCoolingStressScore)
	}
}
