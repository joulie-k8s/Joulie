package workload

import (
	"math"
	"sort"
	"strings"
)

// Sharing is how the units a device plugin advertises for one GPU share it.
type Sharing int

const (
	// SharingNone: the exposure advertises no units, as with DRA, whose
	// ResourceSlices are not node resources.
	SharingNone Sharing = iota
	// SharingWhole: one unit per GPU (exposure whole).
	SharingWhole
	// SharingMIG: isolated MIG slices (exposures mig-single, mig-mixed).
	SharingMIG
	// SharingTimeslice: time-sliced replicas of one GPU (exposures
	// timeslice-rename, timeslice-norename).
	SharingTimeslice
)

// SharingOf maps a profile's gpus.exposure, as hwemu.Node.GPUExposure
// returns it, to its Sharing.
func SharingOf(exposure string) Sharing {
	switch {
	case exposure == "whole":
		return SharingWhole
	case strings.HasPrefix(exposure, "mig-"):
		return SharingMIG
	case strings.HasPrefix(exposure, "timeslice-"):
		return SharingTimeslice
	}
	return SharingNone
}

// DeviceUse is how many of a job's GPU units sit on one device.
type DeviceUse struct {
	Device int
	Units  int
}

// GPUSlots holds which GPU units of one node each running job occupies, the
// way a device plugin assigns them. A node with N GPUs and u units per GPU
// has N*u units; unit k is on device k/u. A job takes the lowest free units
// when it starts and frees them when it ends.
//
// Device utilization and a job's share of its devices (assumed: hwemu has no
// sharing model of its own):
//   - whole: the device runs at its one job's GPU util;
//   - MIG: the slices are isolated, so the device runs at the sum of its
//     units' util over u, and each job gets its full slice;
//   - time-slicing: the device runs at the sum of its units' util, at most
//     1, and when that sum exceeds 1 every job on it gets 1/sum of its time.
//     A job's share is the smallest over its devices, since its work waits
//     for the slowest one.
//
// GPUSlots is not safe for concurrent use.
type GPUSlots struct {
	sharing  Sharing
	perDev   int
	unitJob  []string
	jobUtil  map[string]float64
	jobUnits map[string][]int
}

// NewGPUSlots returns the slots of a node with devices GPUs under exposure,
// with unitsPerDevice units per GPU (hwemu.Node.GPUExposure). Whole GPUs
// have one unit each whatever unitsPerDevice says; an exposure with no units
// gives a node on which Assign always fails.
func NewGPUSlots(devices int, exposure string, unitsPerDevice int) *GPUSlots {
	s := &GPUSlots{sharing: SharingOf(exposure), jobUtil: map[string]float64{}, jobUnits: map[string][]int{}}
	switch s.sharing {
	case SharingWhole:
		s.perDev = 1
	case SharingMIG, SharingTimeslice:
		s.perDev = unitsPerDevice
	}
	if devices > 0 && s.perDev > 0 {
		s.unitJob = make([]string, devices*s.perDev)
	}
	return s
}

// Units is the number of units the node advertises.
func (s *GPUSlots) Units() int { return len(s.unitJob) }

// UnitsPerDevice is the number of units each GPU advertises: 1 for whole
// GPUs, its slices or replicas otherwise, 0 under an exposure with no units.
// NodeEnergy.GPUUnitsPerDevice takes it.
func (s *GPUSlots) UnitsPerDevice() int { return s.perDev }

// Devices is the number of GPUs the slots cover.
func (s *GPUSlots) Devices() int {
	if s.perDev == 0 {
		return 0
	}
	return len(s.unitJob) / s.perDev
}

// Assign gives job the lowest free units and returns the devices they sit
// on, in ascending order. util is the job's GPU util. It fails, assigning
// nothing, when fewer than units are free; the scheduler never binds more
// units than a node advertises, so a failure means the slots disagree with
// the node's allocatable. A job that already holds units keeps them.
func (s *GPUSlots) Assign(job string, units int, util float64) ([]int, bool) {
	if _, ok := s.jobUnits[job]; ok {
		return s.devicesOf(job), true
	}
	if units <= 0 {
		return nil, false
	}
	var free []int
	for k, owner := range s.unitJob {
		if owner == "" {
			free = append(free, k)
			if len(free) == units {
				break
			}
		}
	}
	if len(free) < units {
		return nil, false
	}
	for _, k := range free {
		s.unitJob[k] = job
	}
	s.jobUnits[job] = free
	s.jobUtil[job] = clamp01(util)
	return s.devicesOf(job), true
}

// Release frees the units job holds.
func (s *GPUSlots) Release(job string) {
	for _, k := range s.jobUnits[job] {
		s.unitJob[k] = ""
	}
	delete(s.jobUnits, job)
	delete(s.jobUtil, job)
}

// Uses lists, by ascending device, how many of job's units sit on each
// device; energy attribution weighs a job's claim on a device by it.
func (s *GPUSlots) Uses(job string) []DeviceUse {
	var out []DeviceUse
	for _, k := range s.jobUnits[job] {
		d := k / s.perDev
		if n := len(out); n > 0 && out[n-1].Device == d {
			out[n-1].Units++
			continue
		}
		out = append(out, DeviceUse{Device: d, Units: 1})
	}
	return out
}

func (s *GPUSlots) devicesOf(job string) []int {
	var out []int
	for _, u := range s.Uses(job) {
		out = append(out, u.Device)
	}
	sort.Ints(out)
	return out
}

// deviceSum is the sum of util over device d's held units.
func (s *GPUSlots) deviceSum(d int) float64 {
	sum := 0.0
	for k := d * s.perDev; k < (d+1)*s.perDev; k++ {
		if job := s.unitJob[k]; job != "" {
			sum += s.jobUtil[job]
		}
	}
	return sum
}

// DeviceUtil is each GPU's utilization, the value hwemu.Load.GPUUtil takes.
func (s *GPUSlots) DeviceUtil() []float64 {
	out := make([]float64, s.Devices())
	for d := range out {
		sum := s.deviceSum(d)
		switch s.sharing {
		case SharingMIG:
			out[d] = sum / float64(s.perDev)
		default:
			out[d] = math.Min(1, sum)
		}
	}
	return out
}

// Share is the share of its devices' time job gets: below 1 only on a
// time-sliced device whose units ask for more than all of it, and 1 for a
// job holding no units.
func (s *GPUSlots) Share(job string) float64 {
	if s.sharing != SharingTimeslice {
		return 1
	}
	share := 1.0
	for _, u := range s.Uses(job) {
		if sum := s.deviceSum(u.Device); sum > 1 {
			share = math.Min(share, 1/sum)
		}
	}
	return share
}
