package twin

import (
	"math"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// A node whose CPU model is in the hardware catalog gets a per-socket maximum
// but keeps whatever socket count the agent reported. When that count is
// missing, multiplying by it erases the budget and the node reports unlimited
// headroom while it is busy, which is the worst possible default: the
// scheduler then treats a saturated node as the emptiest one.
func TestComputeAssumesOneSocketWhenCountUnknown(t *testing.T) {
	in := Input{
		NodeName: "node-unknown-sockets",
		Profile:  "performance",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				TotalCores: 48,
				Sockets:    0, // the agent could not determine it
				CapRange:   joulie.CPUCapRange{MaxWattsPerSocket: 270},
			},
		},
		MeasuredNodePowerW: 200,
	}

	out := Compute(in)

	if out.PowerMeasurement.CpuTdpW != 270 {
		t.Fatalf("cpuTdpW=%v want=270 (one socket assumed when the count is unknown)", out.PowerMeasurement.CpuTdpW)
	}
	if out.PowerMeasurement.NodeCappedPowerW != 270 {
		t.Fatalf("nodeCappedPowerW=%v want=270", out.PowerMeasurement.NodeCappedPowerW)
	}
	wantHeadroom := (270.0 - 200.0) / 270.0 * 100.0
	if math.Abs(out.PredictedPowerHeadroomScore-wantHeadroom) > 0.01 {
		t.Fatalf("headroom=%v want=%.2f (a busy node must not report free capacity)", out.PredictedPowerHeadroomScore, wantHeadroom)
	}
	if out.PredictedCoolingStressScore <= 0 {
		t.Fatalf("coolingStress=%v want>0", out.PredictedCoolingStressScore)
	}
}

func TestComputeUsesReportedSocketCount(t *testing.T) {
	in := Input{
		NodeName: "node-four-sockets",
		Profile:  "performance",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{
				Sockets:  4,
				CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 165},
			},
		},
		MeasuredNodePowerW: 330,
	}

	if got := Compute(in).PowerMeasurement.CpuTdpW; got != 660 {
		t.Fatalf("cpuTdpW=%v want=660 (4 sockets x 165 W)", got)
	}
}

// With no per-socket maximum there is nothing to assume a socket count for,
// and the twin keeps reporting unknown hardware rather than inventing a budget.
func TestComputeKeepsZeroTDPWhenCapRangeUnknown(t *testing.T) {
	in := Input{
		NodeName:           "node-no-caprange",
		Profile:            "performance",
		Hardware:           joulie.NodeHardware{CPU: joulie.NodeHardwareCPU{TotalCores: 48}},
		MeasuredNodePowerW: 200,
	}

	out := Compute(in)

	if out.PowerMeasurement.CpuTdpW != 0 {
		t.Fatalf("cpuTdpW=%v want=0", out.PowerMeasurement.CpuTdpW)
	}
	if out.PredictedPowerHeadroomScore != 100 {
		t.Fatalf("headroom=%v want=100 (unknown hardware stays neutral)", out.PredictedPowerHeadroomScore)
	}
}
