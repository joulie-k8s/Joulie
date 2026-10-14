package workload_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

func approxSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-12 {
			return false
		}
	}
	return true
}

// Every exposure hwemu renders maps to how its units share a GPU; DRA
// advertises no node resource, so it has no units.
func TestSharingOf(t *testing.T) {
	cases := map[string]workload.Sharing{
		"whole": workload.SharingWhole, "mig-single": workload.SharingMIG, "mig-mixed": workload.SharingMIG,
		"timeslice-rename": workload.SharingTimeslice, "timeslice-norename": workload.SharingTimeslice,
		"dra": workload.SharingNone, "": workload.SharingNone,
	}
	for exp, want := range cases {
		if got := workload.SharingOf(exp); got != want {
			t.Errorf("SharingOf(%q) = %v, want %v", exp, got, want)
		}
	}
	s := workload.NewGPUSlots(8, "dra", 1)
	if _, ok := s.Assign("j", 1, 1); ok || s.Units() != 0 {
		t.Errorf("a DRA node assigned a unit (units %d)", s.Units())
	}
}

// Whole GPUs: a job takes the lowest free devices, each runs at its job's
// util, and a request larger than what is free assigns nothing.
func TestGPUSlotsWhole(t *testing.T) {
	s := workload.NewGPUSlots(4, "whole", 7)
	if d, ok := s.Assign("a", 2, 0.9); !ok || !reflect.DeepEqual(d, []int{0, 1}) {
		t.Fatalf("Assign(a, 2) = %v, %v", d, ok)
	}
	if d, ok := s.Assign("b", 1, 0.5); !ok || !reflect.DeepEqual(d, []int{2}) {
		t.Fatalf("Assign(b, 1) = %v, %v", d, ok)
	}
	if _, ok := s.Assign("c", 2, 1); ok {
		t.Errorf("assigned 2 GPUs with 1 free")
	}
	if got := s.DeviceUtil(); !approxSlice(got, []float64{0.9, 0.9, 0.5, 0}) {
		t.Errorf("DeviceUtil = %v", got)
	}
	if s.Share("a") != 1 {
		t.Errorf("whole GPU share %v", s.Share("a"))
	}
	s.Release("a")
	if d, ok := s.Assign("c", 2, 1); !ok || !reflect.DeepEqual(d, []int{0, 1}) {
		t.Errorf("after release Assign(c, 2) = %v, %v", d, ok)
	}
}

// MIG slices are isolated: a GPU runs at the sum of its slices' util over
// the slices per GPU, a job keeps its full share, and unit k sits on GPU
// k/7, so a job can straddle two GPUs.
func TestGPUSlotsMIG(t *testing.T) {
	s := workload.NewGPUSlots(2, "mig-mixed", 7)
	if s.Units() != 14 || s.Devices() != 2 {
		t.Fatalf("units %d devices %d", s.Units(), s.Devices())
	}
	s.Assign("a", 3, 0.7)
	d, ok := s.Assign("b", 5, 1)
	if !ok || !reflect.DeepEqual(d, []int{0, 1}) {
		t.Fatalf("Assign(b, 5) = %v, %v", d, ok)
	}
	if got := s.Uses("b"); !reflect.DeepEqual(got, []workload.DeviceUse{{Device: 0, Units: 4}, {Device: 1, Units: 1}}) {
		t.Errorf("Uses(b) = %v", got)
	}
	if got := s.DeviceUtil(); !approxSlice(got, []float64{(3*0.7 + 4) / 7, 1.0 / 7}) {
		t.Errorf("DeviceUtil = %v", got)
	}
	if s.Share("b") != 1 {
		t.Errorf("MIG share %v", s.Share("b"))
	}
}

// Time-sliced replicas share the GPU's time: the GPU runs at the sum of its
// replicas' util, at most 1, and when that sum exceeds 1 each job gets
// 1/sum of the time; a job spanning two oversubscribed GPUs gets its smallest
// share, whether that GPU comes first or last.
func TestGPUSlotsTimeslice(t *testing.T) {
	s := workload.NewGPUSlots(2, "timeslice-rename", 4)
	s.Assign("a", 1, 0.8)
	s.Assign("b", 1, 0.8)
	if got := s.DeviceUtil(); !approxSlice(got, []float64{1, 0}) {
		t.Errorf("DeviceUtil = %v", got)
	}
	if got := s.Share("a"); math.Abs(got-1/1.6) > 1e-12 {
		t.Errorf("Share(a) = %v, want 1/1.6", got)
	}
	s.Assign("c", 3, 0.5)
	if got := s.Uses("c"); !reflect.DeepEqual(got, []workload.DeviceUse{{Device: 0, Units: 2}, {Device: 1, Units: 1}}) {
		t.Errorf("Uses(c) = %v", got)
	}
	s.Assign("d", 1, 0.8)
	if got := s.Uses("d"); !reflect.DeepEqual(got, []workload.DeviceUse{{Device: 1, Units: 1}}) {
		t.Errorf("Uses(d) = %v", got)
	}
	if got := s.Share("c"); math.Abs(got-1/2.6) > 1e-12 {
		t.Errorf("Share(c) = %v, want 1/2.6 of GPU 0, smaller than 1/1.3 of GPU 1", got)
	}
	if got := s.Share("d"); math.Abs(got-1/1.3) > 1e-12 {
		t.Errorf("Share(d) = %v, want 1/1.3", got)
	}
	s.Release("b")
	s.Release("c")
	s.Release("d")
	if got := s.Share("a"); got != 1 {
		t.Errorf("Share(a) alone = %v", got)
	}
	if got := s.DeviceUtil(); !approxSlice(got, []float64{0.8, 0}) {
		t.Errorf("DeviceUtil alone = %v", got)
	}

	// The smaller share on the later GPU: GPU 0 sums 3*0.3+0.5, GPU 1 sums
	// 0.5+3*0.8.
	s = workload.NewGPUSlots(2, "timeslice-rename", 4)
	s.Assign("x", 3, 0.3)
	s.Assign("c", 2, 0.5)
	s.Assign("y", 3, 0.8)
	if got := s.Uses("c"); !reflect.DeepEqual(got, []workload.DeviceUse{{Device: 0, Units: 1}, {Device: 1, Units: 1}}) {
		t.Errorf("Uses(c) = %v", got)
	}
	if got := s.Share("c"); math.Abs(got-1/2.9) > 1e-12 {
		t.Errorf("Share(c) = %v, want 1/2.9 of GPU 1, smaller than 1/1.4 of GPU 0", got)
	}
}
