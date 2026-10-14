package workload

import (
	"fmt"
	"math/rand"
	"testing"
)

// perJobShare is the share of one job computed on its own, as the v1
// engine asked for it once per job: 1 for a job holding no CPUs, else the
// node's CPU count over the summed demand when that is larger.
func perJobShare(s *CPUSlots, job string, util map[string]float64) float64 {
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

// The share computed once per node, applied to the jobs that hold CPUs, is
// bit for bit the share computed per job, over random nodes, requests,
// releases and utilizations, out-of-range and missing ones included.
func TestCPUSlotsShareOncePerNodeEqualsPerJob(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for c := 0; c < 2000; c++ {
		s := NewCPUSlots(rng.Intn(65))
		util := map[string]float64{}
		var ids []string
		for i, n := 0, rng.Intn(40); i < n; i++ {
			id := fmt.Sprintf("j%d", i)
			ids = append(ids, id)
			s.Assign(id, rng.Float64()*10-0.5)
			if rng.Intn(10) > 0 {
				util[id] = rng.Float64()*1.4 - 0.2
			}
		}
		for _, id := range ids {
			if rng.Intn(5) == 0 {
				s.Release(id)
			}
		}
		share := s.Share(util)
		for _, id := range append(ids, "never-assigned") {
			got := 1.0
			if s.Holds(id) {
				got = share
			}
			if want := perJobShare(s, id, util); got != want {
				t.Fatalf("case %d, job %s: share %v once per node, %v per job", c, id, got, want)
			}
		}
	}
}

// benchCPUNode is a 384-CPU node with 100 jobs of 1 to 8 CPUs.
func benchCPUNode() (*CPUSlots, []string, map[string]float64) {
	s := NewCPUSlots(384)
	ids := make([]string, 100)
	util := map[string]float64{}
	for i := range ids {
		ids[i] = fmt.Sprintf("job-%03d", i)
		s.Assign(ids[i], float64(1+i%8))
		util[ids[i]] = 0.5 + float64(i%5)/10
	}
	return s, ids, util
}

// The CPU shares of every job of one node for one step: once per job, as
// the engine computed them, and once per node.
func BenchmarkCPUSharesOfANodePerJob(b *testing.B) {
	s, ids, util := benchCPUNode()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			_ = perJobShare(s, id, util)
		}
	}
}

func BenchmarkCPUSharesOfANodeOnce(b *testing.B) {
	s, ids, util := benchCPUNode()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		share := s.Share(util)
		for _, id := range ids {
			if s.Holds(id) {
				_ = share
			}
		}
	}
}
