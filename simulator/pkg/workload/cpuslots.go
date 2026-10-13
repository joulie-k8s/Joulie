package workload

import (
	"math"
	"sort"
)

// CPUSlots holds which logical CPUs of one node each running job occupies.
// A job holds ceil(cpu request) CPUs, at least one and at most the node's,
// from the moment it starts: the CPUs with the fewest holders, lowest index
// first. While a node has free CPUs this packs jobs onto the lowest free
// CPUs in start order; once every CPU is held, as fractional requests can
// make it, later jobs share the least shared CPUs.
//
// assumed: the kernel runs a job's threads on about as many CPUs as it has
// threads and leaves the other CPUs idle; no CPU pinning is modelled. The
// alternative, spreading a job's demand over every CPU, would put the whole
// node at a low utilization, where a demand-driven governor in hwemu picks a
// low frequency, and hwemu's CPUThroughput, measured against a
// governor-independent reference, slows an uncapped job down.
//
// CPUSlots is not safe for concurrent use.
type CPUSlots struct {
	holders [][]string
	held    map[string][]int
	request map[string]float64
}

// NewCPUSlots returns the slots of a node with cpus logical CPUs, all free.
func NewCPUSlots(cpus int) *CPUSlots {
	if cpus < 0 {
		cpus = 0
	}
	return &CPUSlots{holders: make([][]string, cpus), held: map[string][]int{}, request: map[string]float64{}}
}

// Len is the number of logical CPUs.
func (s *CPUSlots) Len() int { return len(s.holders) }

// Assign gives job its CPUs for request and returns them in ascending
// order. A job that already holds CPUs keeps them and the request it got
// them for. A node with no CPUs gives none.
func (s *CPUSlots) Assign(job string, request float64) []int {
	if cpus, ok := s.held[job]; ok {
		return append([]int(nil), cpus...)
	}
	n := len(s.holders)
	if n == 0 {
		return nil
	}
	want := 1
	if request > 1 {
		want = int(math.Min(float64(n), math.Ceil(request)))
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(s.holders[order[a]]) < len(s.holders[order[b]]) })
	cpus := append([]int(nil), order[:want]...)
	sort.Ints(cpus)
	for _, c := range cpus {
		s.holders[c] = append(s.holders[c], job)
	}
	s.held[job] = cpus
	if !(request > 0) {
		request = 0
	}
	s.request[job] = request
	return append([]int(nil), cpus...)
}

// Release frees the CPUs job holds; releasing a job that holds none does
// nothing.
func (s *CPUSlots) Release(job string) {
	for _, c := range s.held[job] {
		h := s.holders[c]
		for i := range h {
			if h[i] == job {
				s.holders[c] = append(h[:i:i], h[i+1:]...)
				break
			}
		}
	}
	delete(s.held, job)
	delete(s.request, job)
}

// CPUs returns the CPUs job holds, in ascending order, or nil.
func (s *CPUSlots) CPUs(job string) []int {
	return append([]int(nil), s.held[job]...)
}

// Util is the utilization of each CPU, the value hwemu.Load.CPUUtil takes.
// util is each job's utilization relative to its request, as the trace's
// cpuUtil is: a job asks for util x request CPUs, spread evenly over the
// CPUs it holds, so a 0.5-CPU job at util 0.9 asks 0.45 of its one CPU. A
// holder missing from util counts as 0. Where the holders of a CPU ask for
// more than all of it, the excess runs on the CPUs with time to spare, in
// proportion to the time each has spare, so the CPUs add up to what the
// jobs ask for, at most the node's CPU count.
//
// assumed: the kernel's load balancer moves threads off a contended CPU
// onto CPUs with idle time, so jobs whose requests fit the node are not
// slowed when their ceil(request) CPUs overlap.
func (s *CPUSlots) Util(util map[string]float64) []float64 {
	out := s.demand(util)
	over, spare := 0.0, 0.0
	for _, d := range out {
		if d > 1 {
			over += d - 1
		} else {
			spare += 1 - d
		}
	}
	if over == 0 {
		return out
	}
	fill := 1.0
	if over < spare {
		fill = over / spare
	}
	for c, d := range out {
		if d >= 1 {
			out[c] = 1
		} else {
			out[c] = d + (1-d)*fill
		}
	}
	return out
}

// Share is the share of the CPU time it asks for that job gets, the value
// RateInputs.CPUShare takes: the node's CPU count over what all its jobs
// ask for together (util x request, summed) when they ask for more than
// the node has, and 1 otherwise or for a job holding no CPUs. util is what
// Util takes.
//
// assumed: with the load balancing of Util, contention is the node's and
// not one CPU's, and the fair scheduler slows every job by the same
// factor, as the legacy model does with capacity over requested cores.
func (s *CPUSlots) Share(job string, util map[string]float64) float64 {
	if _, ok := s.held[job]; !ok {
		return 1
	}
	asked := 0.0
	for _, d := range s.demand(util) {
		asked += d
	}
	if n := float64(len(s.holders)); asked > n {
		return n / asked
	}
	return 1
}

// demand is what each CPU's holders ask of it: util x request of each
// holder, over the number of CPUs the holder holds, summed.
func (s *CPUSlots) demand(util map[string]float64) []float64 {
	out := make([]float64, len(s.holders))
	for c, holders := range s.holders {
		for _, job := range holders {
			out[c] += clamp01(util[job]) * s.request[job] / float64(len(s.held[job]))
		}
	}
	return out
}
