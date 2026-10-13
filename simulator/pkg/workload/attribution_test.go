package workload_test

import (
	"math"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// A 1-CPU job alone on an idle 384-CPU node is charged 1/384 of the node's
// idle package energy plus the dynamic energy it caused; the other 383/384
// of idle goes to the idle class. Splitting all package energy by request
// times util, as first specified, would charge the job all 800 J of idle.
func TestAttributionIdleSplit(t *testing.T) {
	prev := workload.NodeEnergy{Time: t0, CPUEnergyJ: 1000}
	cur := workload.NodeEnergy{Time: t0.Add(10 * time.Second), CPUEnergyJ: 1000 + 800 + 50, CPUIdleW: 80, AllocatableCPU: 384}
	a := workload.Attribute(prev, cur, []workload.JobLoad{{ID: "j", Class: workload.ClassPerformance, CPURequest: 1, CPUUtil: 1}})
	if got, want := a.Jobs["j"].CPUJ, 800.0/384+50; !near(got, want) {
		t.Errorf("job CPU energy %v J, want %v (1/384 of idle plus the dynamic 50 J)", got, want)
	}
	if got, want := a.Classes[workload.ClassIdle].CPUJ, 800.0*383/384; !near(got, want) {
		t.Errorf("idle class CPU energy %v J, want %v", got, want)
	}
	if got := a.Classes[workload.ClassPerformance].CPUJ; !near(got, a.Jobs["j"].CPUJ) {
		t.Errorf("performance class %v J, job %v J", got, a.Jobs["j"].CPUJ)
	}
}

// The classes add up to the ledgers' growth: dynamic CPU energy by request
// times util, each GPU's energy among its holders by units times util, and a
// GPU nobody holds to the idle class; the jobs add up to the job classes.
func TestAttributionConservesLedgerDeltas(t *testing.T) {
	prev := workload.NodeEnergy{Time: t0, CPUEnergyJ: 5000, GPUEnergyJ: []float64{100, 200, 300}}
	cur := workload.NodeEnergy{
		Time: t0.Add(5 * time.Second), CPUEnergyJ: 5000 + 1000, CPUIdleW: 100, AllocatableCPU: 16,
		GPUEnergyJ: []float64{100 + 3000, 200 + 2000, 300 + 400},
	}
	jobs := []workload.JobLoad{
		{ID: "p", Class: workload.ClassPerformance, CPURequest: 4, CPUUtil: 1, GPUUtil: 0.9, GPUs: []workload.DeviceUse{{Device: 0, Units: 1}}},
		{ID: "s1", Class: workload.ClassStandard, CPURequest: 4, CPUUtil: 0.5, GPUUtil: 0.3, GPUs: []workload.DeviceUse{{Device: 1, Units: 3}}},
		{ID: "s2", Class: workload.ClassStandard, CPURequest: 0, CPUUtil: 1, GPUUtil: 0.6, GPUs: []workload.DeviceUse{{Device: 1, Units: 1}}},
	}
	a := workload.Attribute(prev, cur, jobs)

	var cpu, gpu float64
	for _, c := range a.Classes {
		cpu += c.CPUJ
		gpu += c.GPUJ
	}
	if !near(cpu, 1000) || !near(gpu, 5400) {
		t.Errorf("classes sum to %v J CPU and %v J GPU, want 1000 and 5400", cpu, gpu)
	}
	// Idle 500 J: 4/16 to p, 4/16 to s1, 8/16 to idle. Dynamic 500 J by
	// 4x1 and 4x0.5: 2/3 to p, 1/3 to s1.
	if got, want := a.Jobs["p"].CPUJ, 125+500*2.0/3; !near(got, want) {
		t.Errorf("p CPU %v, want %v", got, want)
	}
	if got, want := a.Jobs["s1"].CPUJ, 125+500/3.0; !near(got, want) {
		t.Errorf("s1 CPU %v, want %v", got, want)
	}
	// GPU 1: 3x0.3 against 1x0.6, so 0.6 and 0.4 of 2000 J. GPU 2 is idle.
	if got := a.Jobs["p"].GPUJ; !near(got, 3000) {
		t.Errorf("p GPU %v, want 3000", got)
	}
	if got, want := a.Jobs["s1"].GPUJ, 1200.0; !near(got, want) {
		t.Errorf("s1 GPU %v, want %v", got, want)
	}
	if got, want := a.Jobs["s2"].GPUJ, 800.0; !near(got, want) {
		t.Errorf("s2 GPU %v, want %v", got, want)
	}
	if got := a.Classes[workload.ClassIdle]; !near(got.CPUJ, 250) || !near(got.GPUJ, 400) {
		t.Errorf("idle class %+v, want 250 J CPU and 400 J GPU", got)
	}
	std := a.Classes[workload.ClassStandard]
	if !near(std.Total(), a.Jobs["s1"].Total()+a.Jobs["s2"].Total()) {
		t.Errorf("standard class %v J, its jobs %v J", std.Total(), a.Jobs["s1"].Total()+a.Jobs["s2"].Total())
	}
}

// With no job everything is idle; a delta below the idle power is all idle;
// jobs without util share dynamic energy by request; a ledger that did not
// grow and a GPU missing from the earlier reading add nothing.
func TestAttributionEdges(t *testing.T) {
	prev := workload.NodeEnergy{Time: t0, CPUEnergyJ: 100, GPUEnergyJ: []float64{50}}
	cur := workload.NodeEnergy{Time: t0.Add(time.Second), CPUEnergyJ: 400, CPUIdleW: 100, AllocatableCPU: 8, GPUEnergyJ: []float64{150, 999}}
	a := workload.Attribute(prev, cur, nil)
	if got := a.Classes[workload.ClassIdle]; !near(got.CPUJ, 300) || !near(got.GPUJ, 100) || len(a.Jobs) != 0 {
		t.Errorf("no jobs: %+v", a)
	}

	cur.CPUIdleW = 1000
	a = workload.Attribute(prev, cur, []workload.JobLoad{{ID: "j", Class: workload.ClassStandard, CPURequest: 8, CPUUtil: 1}})
	if got := a.Jobs["j"].CPUJ; !near(got, 300) {
		t.Errorf("delta below idle: job %v J, want all 300 J as its idle share", got)
	}

	cur.CPUIdleW = 0
	a = workload.Attribute(prev, cur, []workload.JobLoad{{ID: "x", CPURequest: 1}, {ID: "y", CPURequest: 3}})
	if !near(a.Jobs["x"].CPUJ, 75) || !near(a.Jobs["y"].CPUJ, 225) || !near(a.Classes[workload.ClassStandard].CPUJ, 300) {
		t.Errorf("no util: jobs %+v, classes %+v", a.Jobs, a.Classes)
	}

	back := workload.NodeEnergy{Time: t0.Add(2 * time.Second), CPUEnergyJ: 50, GPUEnergyJ: []float64{10}}
	a = workload.Attribute(cur, back, nil)
	if got := a.Classes[workload.ClassIdle]; got.Total() != 0 {
		t.Errorf("shrinking ledgers attributed %+v", got)
	}
}

// A job holding 1 of 7 MIG slices, or 1 of 4 time-slice replicas, is
// charged its unit's share of the GPU's idle energy plus the dynamic energy
// above idle, which only its unit causes; the unheld units' idle share goes
// to the idle class, as do the CPUs nobody requested. Charging the holders
// each GPU's whole delta, as first written, gives the job all 1000 J of
// idle. A whole GPU's holder is charged all of it, and once every unit is
// held nothing is left for the idle class.
func TestAttributionGPUIdleSplitOverUnits(t *testing.T) {
	prev := workload.NodeEnergy{Time: t0, GPUEnergyJ: []float64{0, 0}}
	for _, c := range []struct {
		exposure string
		units    int
	}{{"mig-single", 7}, {"timeslice-rename", 4}, {"whole", 1}} {
		s := workload.NewGPUSlots(2, c.exposure, c.units)
		if _, ok := s.Assign("j", 1, 1); !ok {
			t.Fatalf("%s: Assign failed", c.exposure)
		}
		cur := workload.NodeEnergy{
			Time: t0.Add(10 * time.Second), GPUEnergyJ: []float64{1000 + 300, 1000},
			GPUIdleW: 100, GPUUnitsPerDevice: s.UnitsPerDevice(),
		}
		a := workload.Attribute(prev, cur, []workload.JobLoad{{ID: "j", Class: workload.ClassPerformance, GPUUtil: 1, GPUs: s.Uses("j")}})
		per := float64(c.units)
		if got, want := a.Jobs["j"].GPUJ, 1000/per+300; !near(got, want) {
			t.Errorf("%s: job GPU energy %v J, want %v (1/%v of idle plus the dynamic 300 J)", c.exposure, got, want, per)
		}
		if got, want := a.Classes[workload.ClassIdle].GPUJ, 1000*(per-1)/per+1000; !near(got, want) {
			t.Errorf("%s: idle class GPU energy %v J, want %v (the unheld units of GPU 0 and all of GPU 1)", c.exposure, got, want)
		}
		if got := a.Classes[workload.ClassIdle].GPUJ + a.Classes[workload.ClassPerformance].GPUJ; !near(got, 2300) {
			t.Errorf("%s: classes sum to %v J, want the 2300 J the ledgers grew by", c.exposure, got)
		}
	}

	s := workload.NewGPUSlots(1, "mig-single", 7)
	var jobs []workload.JobLoad
	for i := 0; i < 7; i++ {
		id := string(rune('a' + i))
		s.Assign(id, 1, 0)
		jobs = append(jobs, workload.JobLoad{ID: id, GPUs: s.Uses(id)})
	}
	cur := workload.NodeEnergy{Time: t0.Add(10 * time.Second), GPUEnergyJ: []float64{700}, GPUIdleW: 70, GPUUnitsPerDevice: 7}
	a := workload.Attribute(workload.NodeEnergy{Time: t0, GPUEnergyJ: []float64{0}}, cur, jobs)
	if got := a.Classes[workload.ClassIdle].GPUJ; got != 0 || !near(a.Jobs["c"].GPUJ, 100) {
		t.Errorf("every slice held: idle class %v J, job c %v J; want 0 and 100", got, a.Jobs["c"].GPUJ)
	}
}
