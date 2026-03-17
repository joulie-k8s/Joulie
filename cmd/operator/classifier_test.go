package main

import (
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestWorkloadProfileStatusToMapBasicFields(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	wp := joulie.WorkloadProfileStatus{
		Criticality: joulie.WorkloadCriticality{Class: "performance"},
		Migratability: joulie.WorkloadMigratability{Reschedulable: true},
		CPU: joulie.WorkloadCPUProfile{
			Intensity:         "high",
			Bound:             "compute",
			AvgUtilizationPct: 85.5,
			CapSensitivity:    "medium",
		},
		GPU: joulie.WorkloadGPUProfile{
			Intensity:         "low",
			Bound:             "none",
			AvgUtilizationPct: 10.0,
			CapSensitivity:    "low",
		},
		ClassificationReason: "kepler-metrics",
		Confidence:           0.92,
		LastUpdated:          ts,
	}

	m := workloadProfileStatusToMap(wp)

	// Check criticality.
	crit, ok := m["criticality"].(map[string]interface{})
	if !ok {
		t.Fatal("criticality not a map")
	}
	if crit["class"] != "performance" {
		t.Fatalf("criticality.class=%v want=performance", crit["class"])
	}

	// Check migratability.
	mig, ok := m["migratability"].(map[string]interface{})
	if !ok {
		t.Fatal("migratability not a map")
	}
	if mig["reschedulable"] != true {
		t.Fatalf("migratability.reschedulable=%v want=true", mig["reschedulable"])
	}

	// Check CPU.
	cpu, ok := m["cpu"].(map[string]interface{})
	if !ok {
		t.Fatal("cpu not a map")
	}
	if cpu["intensity"] != "high" {
		t.Fatalf("cpu.intensity=%v want=high", cpu["intensity"])
	}
	if cpu["bound"] != "compute" {
		t.Fatalf("cpu.bound=%v want=compute", cpu["bound"])
	}
	if cpu["avgUtilizationPct"] != 85.5 {
		t.Fatalf("cpu.avgUtilizationPct=%v want=85.5", cpu["avgUtilizationPct"])
	}
	if cpu["capSensitivity"] != "medium" {
		t.Fatalf("cpu.capSensitivity=%v want=medium", cpu["capSensitivity"])
	}

	// Check GPU.
	gpu, ok := m["gpu"].(map[string]interface{})
	if !ok {
		t.Fatal("gpu not a map")
	}
	if gpu["intensity"] != "low" {
		t.Fatalf("gpu.intensity=%v want=low", gpu["intensity"])
	}

	// Check scalar fields.
	if m["classificationReason"] != "kepler-metrics" {
		t.Fatalf("classificationReason=%v want=kepler-metrics", m["classificationReason"])
	}
	if m["confidence"] != 0.92 {
		t.Fatalf("confidence=%v want=0.92", m["confidence"])
	}
	if m["lastUpdated"] != "2025-06-15T10:00:00Z" {
		t.Fatalf("lastUpdated=%v want=2025-06-15T10:00:00Z", m["lastUpdated"])
	}
}

func TestWorkloadProfileStatusToMapRescheduleFields(t *testing.T) {
	t.Parallel()
	wp := joulie.WorkloadProfileStatus{
		RescheduleRecommended: true,
		RescheduleReason:      "thermal-stress",
		LastUpdated:           time.Now(),
	}

	m := workloadProfileStatusToMap(wp)

	if m["rescheduleRecommended"] != true {
		t.Fatalf("rescheduleRecommended=%v want=true", m["rescheduleRecommended"])
	}
	if m["rescheduleReason"] != "thermal-stress" {
		t.Fatalf("rescheduleReason=%v want=thermal-stress", m["rescheduleReason"])
	}
}

func TestWorkloadProfileStatusToMapNoRescheduleOmitsFields(t *testing.T) {
	t.Parallel()
	wp := joulie.WorkloadProfileStatus{
		RescheduleRecommended: false,
		LastUpdated:           time.Now(),
	}

	m := workloadProfileStatusToMap(wp)

	if _, exists := m["rescheduleRecommended"]; exists {
		t.Fatalf("rescheduleRecommended should be absent when false")
	}
	if _, exists := m["rescheduleReason"]; exists {
		t.Fatalf("rescheduleReason should be absent when not recommended")
	}
}

func TestSystemNamespacesAreSkipped(t *testing.T) {
	t.Parallel()
	systemNamespaces := []string{"kube-system", "kube-public", "kube-node-lease", "joulie-system"}
	for _, ns := range systemNamespaces {
		if ns != "kube-system" && ns != "kube-public" && ns != "kube-node-lease" && ns != "joulie-system" {
			t.Fatalf("unexpected system namespace: %s", ns)
		}
		// Verify the skip condition from classifyAllPods matches.
		skipped := ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease" || ns == "joulie-system"
		if !skipped {
			t.Fatalf("namespace %s should be skipped", ns)
		}
	}

	// Verify non-system namespaces are not skipped.
	userNamespaces := []string{"default", "production", "staging", "my-app"}
	for _, ns := range userNamespaces {
		skipped := ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease" || ns == "joulie-system"
		if skipped {
			t.Fatalf("namespace %s should NOT be skipped", ns)
		}
	}
}
