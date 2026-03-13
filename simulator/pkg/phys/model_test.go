package phys

import (
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hw"
)

func TestMeasuredCurveMonotone(t *testing.T) {
	m := MeasuredCurveCPUModel{
		Points: []hw.PowerPoint{
			{LoadPct: 0, PowerW: 100},
			{LoadPct: 50, PowerW: 300},
			{LoadPct: 100, PowerW: 700},
		},
	}
	prev := 0.0
	for i := 0; i <= 100; i += 5 {
		p := m.Power(DeviceState{Utilization: float64(i) / 100.0, FreqScale: 1, Class: "cpu.compute_bound"})
		if p < prev {
			t.Fatalf("non-monotone at load=%d power=%f prev=%f", i, p, prev)
		}
		prev = p
	}
}

func TestProxyCurveScaling(t *testing.T) {
	base := MeasuredCurveCPUModel{
		Points: []hw.PowerPoint{
			{LoadPct: 0, PowerW: 120},
			{LoadPct: 100, PowerW: 820},
		},
	}
	p := ProxyCurveCPUModel{
		Base:         base,
		DynamicScale: 0.5,
	}
	got := p.Power(DeviceState{Utilization: 1})
	if got <= 120 || got >= 820 {
		t.Fatalf("proxy scaling failed got=%f", got)
	}
}

func TestGPUThroughputClassSensitivity(t *testing.T) {
	g := CappedBoardGPUModel{
		IdleW:         40,
		MaxW:          400,
		ComputeGamma:  1.0,
		MemoryEpsilon: 0.2,
		MemoryGamma:   1.2,
	}
	compute := g.ThroughputMultiplier(DeviceState{
		Utilization: 1, CapWatts: 200, MaxCapWatts: 400, Class: "gpu.compute_bound",
	}, "gpu.compute_bound")
	memory := g.ThroughputMultiplier(DeviceState{
		Utilization: 1, CapWatts: 200, MaxCapWatts: 400, Class: "gpu.memory_bound",
	}, "gpu.memory_bound")
	if memory <= compute {
		t.Fatalf("expected memory-bound throughput to degrade less: memory=%f compute=%f", memory, compute)
	}
}

func TestCPUThroughputIOBoundDegradesLessThanCompute(t *testing.T) {
	m := MeasuredCurveCPUModel{
		Points: []hw.PowerPoint{
			{LoadPct: 0, PowerW: 100},
			{LoadPct: 50, PowerW: 300},
			{LoadPct: 100, PowerW: 700},
		},
		Knee: 0.7,
	}
	compute := m.ThroughputMultiplier(DeviceState{FreqScale: 0.5, Class: "cpu.compute_bound"}, "cpu.compute_bound")
	ioBound := m.ThroughputMultiplier(DeviceState{FreqScale: 0.5, Class: "cpu.io_bound"}, "cpu.io_bound")
	if ioBound <= compute {
		t.Fatalf("expected io-bound throughput to degrade less: io=%f compute=%f", ioBound, compute)
	}
}

func TestCPUThroughputThermalPenaltyReducesOutput(t *testing.T) {
	m := MeasuredCurveCPUModel{
		Points: []hw.PowerPoint{
			{LoadPct: 0, PowerW: 100},
			{LoadPct: 50, PowerW: 300},
			{LoadPct: 100, PowerW: 700},
		},
		Knee: 0.7,
	}
	cool := m.ThroughputMultiplier(DeviceState{FreqScale: 0.8, Class: "cpu.compute_bound"}, "cpu.compute_bound")
	hot := m.ThroughputMultiplier(DeviceState{FreqScale: 0.8, ThermalThrottle: 0.25, Class: "cpu.compute_bound"}, "cpu.compute_bound")
	if hot >= cool {
		t.Fatalf("expected thermal throttling to reduce throughput: hot=%f cool=%f", hot, cool)
	}
}

func TestGPUPowerMemoryBoundLowerThanComputeBound(t *testing.T) {
	g := CappedBoardGPUModel{
		IdleW:         40,
		MaxW:          400,
		ComputeGamma:  1.0,
		MemoryEpsilon: 0.2,
		MemoryGamma:   1.2,
	}
	compute := g.Power(DeviceState{Utilization: 0.9, MemoryIntensity: 0.1, Class: "gpu.compute_bound"})
	memory := g.Power(DeviceState{Utilization: 0.9, MemoryIntensity: 0.9, Class: "gpu.memory_bound"})
	if memory >= compute {
		t.Fatalf("expected memory-bound GPU power to be lower: memory=%f compute=%f", memory, compute)
	}
}
