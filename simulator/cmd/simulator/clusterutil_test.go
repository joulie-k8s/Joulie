package main

import (
	"math"
	"testing"
)

// The cluster's utilization weights each node by its capacity: GPU
// utilization counts only nodes with GPUs, by their GPU count, and CPU
// utilization weights each node by its cores. The simulator used to average
// every node equally, so in experiment 02 the eight CPU-only nodes pulled the
// GPU figure about 20 % below what the GPUs ran at.
func TestClusterUtilizationWeightsByCapacity(t *testing.T) {
	var states []*nodeState
	for i := 0; i < 33; i++ {
		states = append(states, &nodeState{CPUCapacityCores: 64, CPUUtil: 0.25, GPUUtil: 0.6, GPUDevices: make([]gpuDeviceState, 4)})
	}
	for i := 0; i < 8; i++ {
		states = append(states, &nodeState{CPUCapacityCores: 384, CPUUtil: 0.75})
	}
	cpu, gpu := clusterUtilization(states)
	if math.Abs(gpu-0.6) > 1e-12 {
		t.Errorf("GPU utilization %v, want 0.6: CPU-only nodes do not count", gpu)
	}
	want := (33*64*0.25 + 8*384*0.75) / (33*64 + 8*384)
	if math.Abs(cpu-want) > 1e-12 {
		t.Errorf("CPU utilization %v, want %v weighted by cores", cpu, want)
	}
	if cpu, gpu := clusterUtilization(nil); cpu != 0 || gpu != 0 {
		t.Errorf("empty cluster: %v, %v", cpu, gpu)
	}
}
