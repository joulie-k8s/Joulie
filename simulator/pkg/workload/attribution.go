package workload

import (
	"math"
	"time"
)

// NodeEnergy is one node's energy readings at one instant, taken from the
// emulator's snapshot ledgers. The boundary is CPU packages plus GPUs, the
// same as Joulie's default node power input; DRAM and the rest of the host
// are left out.
type NodeEnergy struct {
	Time time.Time
	// CPUEnergyJ is the sum of the node's CPU package ledgers.
	CPUEnergyJ float64
	// CPUIdleW is the packages' power at zero utilization at their present
	// frequency, summed (hwemu.Node.IdleCPUPowerW). Attribute uses the value
	// of the later reading for the whole interval.
	CPUIdleW float64
	// AllocatableCPU is the node's allocatable CPUs, the denominator of the
	// idle split.
	AllocatableCPU float64
	// GPUEnergyJ is each GPU's ledger, by GPU index.
	GPUEnergyJ []float64
	// GPUIdleW is one GPU's power at zero utilization, the profile's
	// physics.gpu.idleW. Attribute uses the value of the later reading.
	GPUIdleW float64
	// GPUUnitsPerDevice is the units each GPU advertises, the denominator
	// of a GPU's idle split (GPUSlots.UnitsPerDevice): its MIG slices or
	// time-slice replicas, 1 for whole GPUs. Below 1 counts as 1.
	GPUUnitsPerDevice int
}

// JobLoad is one job that ran on the node during the interval.
type JobLoad struct {
	ID         string
	Class      string
	CPURequest float64
	CPUUtil    float64
	GPUUtil    float64
	GPUs       []DeviceUse
}

// EnergySplit is energy in joules by hardware type, the hw.type cpu and gpu
// of joulie.sim.workload.energy.
type EnergySplit struct {
	CPUJ float64
	GPUJ float64
}

// Total is CPUJ + GPUJ.
func (e EnergySplit) Total() float64 { return e.CPUJ + e.GPUJ }

// Attribution is one interval's energy on one node by holder. Classes is
// keyed by workload class, with ClassIdle for the energy no job held; Jobs
// is keyed by JobLoad.ID. The classes add up to the node's ledger deltas.
type Attribution struct {
	Classes map[string]EnergySplit
	Jobs    map[string]EnergySplit
}

// Attribute splits the energy a node's ledgers grew by between prev and cur
// among the jobs that ran on it.
//
// CPU energy splits in two. The idle part is CPUIdleW over the interval, at
// most the delta: it is shared by requested CPUs over allocatable CPUs, and
// the unrequested share goes to ClassIdle, so a 1-CPU job on a 384-CPU node
// is charged 1/384 of the idle power, not all of it. The dynamic rest goes
// by cpu request times CPU util, or by request when no job has any util, or
// to ClassIdle when no job requests a CPU.
//
// Each GPU's energy splits the same way. The idle part, GPUIdleW over the
// interval and at most the delta, is shared by units held over
// GPUUnitsPerDevice, and the unheld units' share goes to ClassIdle, so a job
// holding 1 of 7 MIG slices is charged 1/7 of the GPU's idle power, not all
// of it. The dynamic rest goes to the jobs holding units on the GPU by units
// times GPU util, or by units when none has util. A GPU nobody holds goes
// to ClassIdle. A ledger that did not grow, and a GPU missing from either
// reading, adds nothing. A job with no class counts as ClassStandard.
func Attribute(prev, cur NodeEnergy, jobs []JobLoad) Attribution {
	a := Attribution{Classes: map[string]EnergySplit{}, Jobs: map[string]EnergySplit{}}
	add := func(id, class string, cpuJ, gpuJ float64) {
		if class == "" {
			class = ClassStandard
		}
		c := a.Classes[class]
		c.CPUJ += cpuJ
		c.GPUJ += gpuJ
		a.Classes[class] = c
		if id != "" {
			j := a.Jobs[id]
			j.CPUJ += cpuJ
			j.GPUJ += gpuJ
			a.Jobs[id] = j
		}
	}
	for _, j := range jobs {
		add(j.ID, j.Class, 0, 0)
	}

	delta := math.Max(0, cur.CPUEnergyJ-prev.CPUEnergyJ)
	dt := math.Max(0, cur.Time.Sub(prev.Time).Seconds())
	idle := math.Min(delta, math.Max(0, cur.CPUIdleW)*dt)
	dynamic := delta - idle

	reqSum := 0.0
	for _, j := range jobs {
		reqSum += math.Max(0, j.CPURequest)
	}
	if denom := math.Max(cur.AllocatableCPU, reqSum); denom > 0 {
		for _, j := range jobs {
			add(j.ID, j.Class, idle*math.Max(0, j.CPURequest)/denom, 0)
		}
		add("", ClassIdle, idle*(denom-reqSum)/denom, 0)
	} else {
		add("", ClassIdle, idle, 0)
	}

	weight := func(j JobLoad) float64 { return math.Max(0, j.CPURequest) * clamp01(j.CPUUtil) }
	total := 0.0
	for _, j := range jobs {
		total += weight(j)
	}
	if total <= 0 {
		weight = func(j JobLoad) float64 { return math.Max(0, j.CPURequest) }
		total = reqSum
	}
	if total > 0 {
		for _, j := range jobs {
			add(j.ID, j.Class, dynamic*weight(j)/total, 0)
		}
	} else {
		add("", ClassIdle, dynamic, 0)
	}

	n := len(cur.GPUEnergyJ)
	if len(prev.GPUEnergyJ) < n {
		n = len(prev.GPUEnergyJ)
	}
	perDev := math.Max(1, float64(cur.GPUUnitsPerDevice))
	for d := 0; d < n; d++ {
		e := math.Max(0, cur.GPUEnergyJ[d]-prev.GPUEnergyJ[d])
		gpuIdle := math.Min(e, math.Max(0, cur.GPUIdleW)*dt)
		units := map[int]float64{}
		weights := map[int]float64{}
		unitSum, weightSum := 0.0, 0.0
		for i, j := range jobs {
			for _, u := range j.GPUs {
				if u.Device != d || u.Units <= 0 {
					continue
				}
				units[i] += float64(u.Units)
				weights[i] += float64(u.Units) * clamp01(j.GPUUtil)
				unitSum += float64(u.Units)
				weightSum += float64(u.Units) * clamp01(j.GPUUtil)
			}
		}
		if unitSum <= 0 {
			add("", ClassIdle, 0, e)
			continue
		}
		denom := math.Max(perDev, unitSum)
		for i, j := range jobs {
			if u := units[i]; u > 0 {
				add(j.ID, j.Class, 0, gpuIdle*u/denom)
			}
		}
		add("", ClassIdle, 0, gpuIdle*(denom-unitSum)/denom)
		by, sum := weights, weightSum
		if sum <= 0 {
			by, sum = units, unitSum
		}
		for i, j := range jobs {
			if w := by[i]; w > 0 {
				add(j.ID, j.Class, 0, (e-gpuIdle)*w/sum)
			}
		}
	}
	return a
}
