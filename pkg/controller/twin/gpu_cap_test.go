package twin

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// A GPU cap applies unless the agent's last GPU cap write was blocked or
// failed, or, short of an applied write, NodeHardware reports GPUs without
// control.
func TestGPUCapApplies(t *testing.T) {
	gpus := func(control bool) joulie.NodeHardwareGPU {
		return joulie.NodeHardwareGPU{Present: true, Count: 4, ControlAvailable: control}
	}
	result := func(r string) *joulie.ControlResult { return &joulie.ControlResult{Result: r} }
	for _, tc := range []struct {
		name string
		gpu  joulie.NodeHardwareGPU
		last *joulie.ControlResult
		want bool
	}{
		{"no GPU", joulie.NodeHardwareGPU{}, nil, true},
		{"GPUs with control, no result yet", gpus(true), nil, true},
		{"GPUs with control, applied", gpus(true), result(joulie.CapResultApplied), true},
		{"GPUs with control, no intent", gpus(true), result(joulie.CapResultNone), true},
		{"GPUs with control, blocked", gpus(true), result(joulie.CapResultBlocked), false},
		{"GPUs with control, error", gpus(true), result(joulie.CapResultError), false},
		{"GPUs without control, no result yet", gpus(false), nil, false},
		{"GPUs without control, no intent", gpus(false), result(joulie.CapResultNone), false},
		{"GPUs without control, blocked", gpus(false), result(joulie.CapResultBlocked), false},
		{"GPUs without control, applied through the HTTP backend", gpus(false), result(joulie.CapResultApplied), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GPUCapApplies(tc.gpu, tc.last); got != tc.want {
				t.Fatalf("GPUCapApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

// A GPU cap that does not apply leaves the GPU budget at the GPUs' uncapped
// power, and the node budget and headroom follow; the CPU budget and the
// planned percentages in effectiveCapState do not change.
func TestComputeGPUCapNotAppliedBudgetsUncappedGPUPower(t *testing.T) {
	in := Input{
		NodeName: "n",
		Hardware: joulie.NodeHardware{
			CPU: joulie.NodeHardwareCPU{Sockets: 2, CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 360}},
			GPU: joulie.NodeHardwareGPU{Present: true, Count: 4, CapRange: joulie.GPUCapRange{MaxWatts: 400}},
		},
		Profile:            "eco",
		CPUCapPct:          60,
		GPUCapPct:          60,
		MeasuredNodePowerW: 1000,
	}
	capped := Compute(in)
	in.GPUCapNotApplied = true
	uncapped := Compute(in)

	if got := capped.PowerMeasurement.GpuCappedPowerW; got != 960 {
		t.Fatalf("applied cap: gpuCappedPowerW = %g, want 960", got)
	}
	pm := uncapped.PowerMeasurement
	if pm.GpuCappedPowerW != 1600 || pm.GpuCappedPowerW != pm.GpuTdpW {
		t.Fatalf("cap not applied: gpuCappedPowerW = %g, want the GPU TDP 1600", pm.GpuCappedPowerW)
	}
	if pm.CpuCappedPowerW != 432 || pm.NodeCappedPowerW != 432+1600 {
		t.Fatalf("cap not applied: cpuCappedPowerW = %g, nodeCappedPowerW = %g, want 432 and 2032", pm.CpuCappedPowerW, pm.NodeCappedPowerW)
	}
	if want := (2032.0 - 1000) / 2032 * 100; uncapped.PredictedPowerHeadroomScore != want {
		t.Fatalf("headroom = %g, want %g", uncapped.PredictedPowerHeadroomScore, want)
	}
	if uncapped.EffectiveCapState != capped.EffectiveCapState {
		t.Fatalf("effectiveCapState = %+v, want the planned %+v", uncapped.EffectiveCapState, capped.EffectiveCapState)
	}
}
