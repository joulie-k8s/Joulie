package main

import (
	"testing"

	"github.com/matbun/joulie/simulator/pkg/facility"
)

func TestGenerateWorkloadMix(t *testing.T) {
	jobs := generateWorkloadMix(100)
	if len(jobs) != 100 {
		t.Errorf("expected 100 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.CPUIntensity < 0 || j.CPUIntensity > 1 {
			t.Errorf("invalid CPU intensity %f for job %s", j.CPUIntensity, j.ID)
		}
		if j.GPUIntensity < 0 || j.GPUIntensity > 1 {
			t.Errorf("invalid GPU intensity %f for job %s", j.GPUIntensity, j.ID)
		}
	}
}

func TestBuildCluster(t *testing.T) {
	nodes := buildCluster()
	if len(nodes) == 0 {
		t.Fatal("expected non-empty cluster")
	}
	gpuNodes := 0
	for _, n := range nodes {
		if n.HasGPU {
			gpuNodes++
		}
	}
	if gpuNodes == 0 {
		t.Error("expected at least one GPU node")
	}
}

func TestRunScenarioBaseline(t *testing.T) {
	sc := ScenarioConfig{
		Name:             "A",
		Label:            "Baseline",
		CapsEnabled:      false,
		SchedulerEnabled: false,
				CPUCapPct:        100,
		GPUCapPct:        100,
	}
	jobs := generateWorkloadMix(20)
	nodes := buildCluster()
	fm := facility.NewModel(facility.DefaultConfig())
	result := runScenario(sc, jobs, nodes, fm)

	if result.TotalEnergyKWh <= 0 {
		t.Errorf("expected positive energy, got %f", result.TotalEnergyKWh)
	}
	if result.MakespanS <= 0 {
		t.Errorf("expected positive makespan")
	}
}

func TestRunScenarioComparisonEnergy(t *testing.T) {
	jobs := generateWorkloadMix(50)
	nodes := buildCluster()
	fm := facility.NewModel(facility.DefaultConfig())

	baseline := runScenario(ScenarioConfig{
		Name: "A", CapsEnabled: false, SchedulerEnabled: false,
		CPUCapPct: 100, GPUCapPct: 100,
	}, jobs, nodes, fm)

	withCaps := runScenario(ScenarioConfig{
		Name: "B", CapsEnabled: true, SchedulerEnabled: false,
		CPUCapPct: 65, GPUCapPct: 65,
	}, jobs, nodes, fm)

	// Capped scenario should use less energy
	if withCaps.TotalEnergyKWh >= baseline.TotalEnergyKWh {
		t.Errorf("expected energy savings with caps: baseline=%.2f capped=%.2f",
			baseline.TotalEnergyKWh, withCaps.TotalEnergyKWh)
	}
}
