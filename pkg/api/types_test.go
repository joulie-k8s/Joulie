package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNodeHardwareMarshal(t *testing.T) {
	hw := NodeHardware{
		NodeName: "node1",
		CPU: NodeHardwareCPU{
			Vendor:       "amd",
			Model:        "AMD_EPYC_9654",
			Sockets:      2,
			TotalCores:   192,
			DriverFamily: "amd-pstate",
		},
		LastUpdated: time.Now(),
	}
	data, err := json.Marshal(hw)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out NodeHardware
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.NodeName != hw.NodeName {
		t.Errorf("nodeName mismatch: got %q want %q", out.NodeName, hw.NodeName)
	}
	if out.CPU.Vendor != hw.CPU.Vendor {
		t.Errorf("vendor mismatch: got %q want %q", out.CPU.Vendor, hw.CPU.Vendor)
	}
}

func TestNodeTwinMarshal(t *testing.T) {
	nt := NodeTwin{
		Spec: NodeTwinSpec{
			NodeName: "node1",
			Profile:  "eco",
			CPU:      &NodeTwinCPU{PackagePowerCapPctOfMax: func() *float64 { v := 60.0; return &v }()},
		},
		Status: NodeTwinStatus{
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			EffectiveCapState:           CapState{CPUPct: 60, GPUPct: 60},
		},
	}
	data, err := json.Marshal(nt)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out NodeTwin
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.Status.SchedulableClass != nt.Status.SchedulableClass {
		t.Errorf("schedulableClass mismatch: got %q want %q", out.Status.SchedulableClass, nt.Status.SchedulableClass)
	}
	if out.Spec.Profile != "eco" {
		t.Errorf("profile mismatch: got %q want %q", out.Spec.Profile, "eco")
	}
}

func TestWorkloadProfileStatus(t *testing.T) {
	wp := WorkloadProfileStatus{
		Criticality:   WorkloadCriticality{Class: "standard"},
		Migratability: WorkloadMigratability{Reschedulable: true},
		CPU:           WorkloadCPUProfile{Intensity: "high", Bound: "compute", CapSensitivity: "high"},
		GPU:           WorkloadGPUProfile{Intensity: "high", Bound: "compute", CapSensitivity: "high"},
		Confidence:    0.9,
	}
	data, err := json.Marshal(wp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out WorkloadProfileStatus
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.Criticality.Class != wp.Criticality.Class {
		t.Errorf("criticality mismatch")
	}
	if !out.Migratability.Reschedulable {
		t.Errorf("expected reschedulable=true")
	}
}
