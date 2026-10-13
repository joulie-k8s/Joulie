package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// A GPU node gets no eco CPU cap: capping its CPU saves little and slows the
// GPU data feed by more (exp-03), so the CPU of a GPU node runs as on a
// performance node whatever the node's profile. The old code skipped the
// percent cap on GPU nodes but left CapWatts at ECO_CAP_WATTS, which the spec
// then wrote as spec.cpu.packagePowerCapWatts, and the agent applied it: an
// eco GPU node had its CPU capped at 120 W.
func TestEcoGPUNodeCPUIsNotCapped(t *testing.T) {
	const perfW, ecoW = 5000.0, 120.0
	gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
	// Both GPU nodes are in performance and the fraction is 0, so the family
	// keeps one: g0 by name, and g1 goes to eco with nothing running on it.
	g0, g1 := managedNode("g0", "performance"), managedNode("g1", "performance")
	g0.Status.Allocatable, g1.Status.Allocatable = gpus, gpus
	reader, client, dyn := fakeClients(t, g0, g1)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		perfW, ecoW, "static_partition", 0, 0, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	cpuSpec := func(name string) (profile string, cpu map[string]any) {
		t.Helper()
		twin, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get NodeTwin %s: %v", name, err)
		}
		profile, _, _ = unstructured.NestedString(twin.Object, "spec", "profile")
		cpu, _, _ = unstructured.NestedMap(twin.Object, "spec", "cpu")
		if draining, _, _ := unstructured.NestedBool(twin.Object, "spec", "scheduling", "draining"); draining {
			t.Fatalf("%s runs no performance pod and must not be draining: spec=%v", name, twin.Object["spec"])
		}
		return profile, cpu
	}
	perfProfile, perfCPU := cpuSpec("g0")
	ecoProfile, ecoCPU := cpuSpec("g1")
	if perfProfile != profilePerformance || ecoProfile != profileEco {
		t.Fatalf("profiles g0=%q g1=%q, want performance and eco", perfProfile, ecoProfile)
	}
	if !reflect.DeepEqual(ecoCPU, perfCPU) {
		t.Fatalf("eco GPU node spec.cpu = %v, want what a performance GPU node gets (%v): no eco CPU cap on a GPU node", ecoCPU, perfCPU)
	}
}
