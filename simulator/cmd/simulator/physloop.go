package main

// The physics tick of the modelled (non-emulated) nodes in Kubernetes mode.
// In emulate mode the emulator's tick runs it (publishEmulateSnapshot); with
// no emulator, runPhysicsLoop does, every SIM_PHYSICS_TICK, and keeps the
// facility gauges current when no workload loop does. Standalone mode steps
// its nodes itself, on its virtual clock.

import (
	"context"
	"log"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The period of the physics tick without emulate mode, and its default.
const (
	envPhysicsTick     = "SIM_PHYSICS_TICK"
	defaultPhysicsTick = time.Second
)

// stepLegacyNodes advances every modelled node by dt, in substeps of at most
// physMaxSubstep, and adds the energy each drew to the energy counters. An
// emulated node is skipped: the emulator steps it, and its energy comes from
// its ledger (applyEmulateSnapshot).
func (s *simulator) stepLegacyNodes(dt time.Duration) {
	if dt <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for node, st := range s.state {
		if st == nil || s.emulated(node) {
			continue
		}
		cpuJ, gpuJ := stepNode(st, s.modelLocked(node), dt.Seconds(), physMaxSubstep)
		s.addEnergyLocked(node, cpuJ, gpuJ)
	}
	s.energyLastTs = time.Now().UTC()
}

// addEnergyLocked adds a modelled node's CPU and GPU energy to the counters.
// The caller holds s.mu.
func (s *simulator) addEnergyLocked(node string, cpuJ, gpuJ float64) {
	if s.energyJByHwType == nil {
		s.energyJByHwType = map[string]float64{}
	}
	s.energyJByNode[node] += cpuJ + gpuJ
	s.energyTotalJ += cpuJ + gpuJ
	s.energyJByHwType[semconv.HwTypeCPU] += cpuJ
	s.energyJByHwType[semconv.HwTypeGPU] += gpuJ
}

// runPhysicsLoop steps the modelled nodes every interval of wall time, by the
// time that passed since the last step, until ctx ends. An interval of 0 or
// less, which time.NewTicker rejects with a panic, takes the default.
//
// Without a legacy trace it also updates the facility gauges after each
// step, since no workload loop runs to do it; with one, the workload loop
// updates them once per workload tick, before its timeseries row.
func (s *simulator) runPhysicsLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Printf("warning: %s=%s is not positive, using %s", envPhysicsTick, interval, defaultPhysicsTick)
		interval = defaultPhysicsTick
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.stepLegacyNodes(now.Sub(last))
			last = now
			if s.workload == nil {
				s.updateFacilityMetrics(now.UTC())
			}
		}
	}
}

// modelLocked is node's model, the base model unless it has its own. The
// caller holds s.mu, or nothing else runs (standalone mode).
func (s *simulator) modelLocked(node string) simModel {
	if m, ok := s.nodeModels[node]; ok {
		return m
	}
	return s.model
}
