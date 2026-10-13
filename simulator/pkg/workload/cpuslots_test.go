package workload_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Jobs take ceil(request) CPUs, at least one, packed onto the lowest free
// CPUs in start order, and a released job's CPUs are reused first; each CPU
// runs at its holder's util x request over the CPUs it holds, so a 0.2-CPU
// job at util 0.3 keeps its CPU 6% busy, and the rest stay idle.
func TestCPUSlotsPackInStartOrder(t *testing.T) {
	s := workload.NewCPUSlots(8)
	steps := []struct {
		job     string
		request float64
		want    []int
	}{
		{"a", 2.5, []int{0, 1, 2}},
		{"b", 1, []int{3}},
		{"c", 0.2, []int{4}},
	}
	for _, st := range steps {
		if got := s.Assign(st.job, st.request); !reflect.DeepEqual(got, st.want) {
			t.Fatalf("Assign(%s, %v) = %v, want %v", st.job, st.request, got, st.want)
		}
	}
	if got := s.Assign("a", 6); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("a second Assign moved a running job: %v", got)
	}
	s.Release("a")
	if got := s.Assign("d", 1.5); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("Assign(d, 1.5) after releasing a = %v, want [0 1]", got)
	}
	want := []float64{0.675, 0.675, 0, 0.5, 0.06, 0, 0, 0}
	if got := s.Util(map[string]float64{"b": 0.5, "c": 0.3, "d": 0.9, "gone": 1}); !approxSlice(got, want) {
		t.Errorf("Util = %v, want %v", got, want)
	}
	if got := s.CPUs("a"); got != nil {
		t.Errorf("released job still holds %v", got)
	}
}

// Once every CPU is held, as fractional requests can make it, a new job
// shares the least shared CPUs. What a CPU's holders ask beyond all of it
// runs on the CPUs with time to spare, in proportion to that time, so the
// CPUs add up to what the jobs ask for. A request above the node's CPUs
// takes them all, and a node without CPUs gives none.
func TestCPUSlotsShareWhenFull(t *testing.T) {
	s := workload.NewCPUSlots(4)
	s.Assign("a", 4)
	if got := s.Assign("b", 2); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("Assign on a full node = %v, want [0 1]", got)
	}
	if got := s.Assign("c", 2); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Errorf("third job = %v, want the less shared [2 3]", got)
	}
	// Asked: 1.2 of CPUs 0 and 1, 0.7 of CPUs 2 and 3. The 0.4 over goes
	// to the 0.6 spare, two thirds of each spare CPU's 0.3.
	if got := s.Util(map[string]float64{"a": 0.6, "b": 0.6, "c": 0.1}); !approxSlice(got, []float64{1, 1, 0.9, 0.9}) {
		t.Errorf("Util = %v, want [1 1 0.9 0.9]", got)
	}
	if got := workload.NewCPUSlots(4).Assign("big", 64); len(got) != 4 {
		t.Errorf("request 64 on 4 CPUs holds %v", got)
	}
	if got := workload.NewCPUSlots(0).Assign("x", 1); got != nil {
		t.Errorf("node without CPUs gave %v", got)
	}
}

// A job asks for util x request CPUs, and jobs are slowed only when the
// node's jobs together ask for more CPUs than it has, each by the node's
// CPUs over what they ask for, as the legacy capacity over requested cores.
// Jobs whose requests fit are not slowed even when their ceil(request) CPUs
// overlap: eight 0.5-CPU jobs at util 0.9 hold two per CPU and ask 3.6 of 4
// CPUs, and a 0.5-CPU job sharing a CPU with a 1.5-CPU job leaves the node
// 0.5 CPU spare. Eight 1-CPU jobs at util 0.9 ask 7.2 CPUs and get 4/7.2
// until half of them end; one job asking 64 CPUs of 4 gets 1/16; a job
// holding no CPUs is not slowed.
func TestCPUSlotsShareSlowsContendedJobs(t *testing.T) {
	s := workload.NewCPUSlots(4)
	util := map[string]float64{}
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		s.Assign(id, 0.5)
		util[id] = 0.9
	}
	if got := s.Util(util); !approxSlice(got, []float64{0.9, 0.9, 0.9, 0.9}) {
		t.Errorf("Util of eight 0.5-CPU jobs at 0.9 = %v, want every CPU at 0.9", got)
	}
	for id := range util {
		if got := s.Share(id, util); got != 1 {
			t.Errorf("Share(%s) = %v with 3.6 of 4 CPUs asked, want 1", id, got)
		}
	}

	s = workload.NewCPUSlots(4)
	s.Assign("a", 1.5)
	s.Assign("b", 1.5)
	if got := s.Assign("c", 0.5); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("precondition: c holds %v, want [0] shared with a", got)
	}
	util = map[string]float64{"a": 1, "b": 1, "c": 1}
	if got := s.Util(util); !approxSlice(got, []float64{1, 0.75 + 0.25/3, 0.75 + 0.25/3, 0.75 + 0.25/3}) {
		t.Errorf("Util = %v, want CPU 0 at 1 and its 0.25 over spread on the others", got)
	}
	for id := range util {
		if got := s.Share(id, util); got != 1 {
			t.Errorf("Share(%s) = %v with 3.5 of 4 CPUs asked, want 1", id, got)
		}
	}

	s = workload.NewCPUSlots(4)
	util = map[string]float64{}
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		s.Assign(id, 1)
		util[id] = 0.9
	}
	if got := s.Util(util); !approxSlice(got, []float64{1, 1, 1, 1}) {
		t.Errorf("Util of eight 1-CPU jobs at 0.9 = %v, want every CPU at 1", got)
	}
	for id := range util {
		if got := s.Share(id, util); math.Abs(got-4/7.2) > 1e-12 {
			t.Errorf("Share(%s) = %v with 7.2 of 4 CPUs asked, want 4/7.2", id, got)
		}
	}
	if got := s.Share("idle", util); got != 1 {
		t.Errorf("Share of a job holding no CPUs = %v, want 1", got)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		s.Release(id)
	}
	if got := s.Share("e", util); got != 1 {
		t.Errorf("Share(e) = %v after half the jobs ended, want 1", got)
	}

	s = workload.NewCPUSlots(4)
	s.Assign("big", 64)
	if got := s.Share("big", map[string]float64{"big": 1}); math.Abs(got-1.0/16) > 1e-12 {
		t.Errorf("Share(big) = %v asking 64 of 4 CPUs, want 1/16", got)
	}
}
