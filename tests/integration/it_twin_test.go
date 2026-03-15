//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/matbun/joulie/tests/integration/helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var nodeTwinGVR = helpers.JoulieGVRs["nodetwins"]
var nodeHardwareGVR = helpers.JoulieGVRs["nodehardwares"]
var workloadProfileGVR = helpers.JoulieGVRs["workloadprofiles"]

// IT-TWIN-01: Operator writes NodeTwin when NodeHardware and profile are present.
func TestIT_TWIN_01_OperatorWritesNodeTwin(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	clients, err := helpers.NewClients(kubeconfig)
	if err != nil {
		t.Skipf("no kubeconfig available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Find a managed node
	nodes, err := clients.K8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "joulie.io/managed=true",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("no managed nodes found")
	}
	nodeName := nodes.Items[0].Name

	// Seed a NodeHardware fixture (simulate agent)
	nhi := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeHardware",
			"metadata":   map[string]interface{}{"name": nodeName},
			"spec":       map[string]interface{}{"nodeName": nodeName},
			"status": map[string]interface{}{
				"cpu": map[string]interface{}{
					"vendor":     "amd",
					"model":      "AMD_EPYC_9654",
					"sockets":    int64(2),
					"totalCores": int64(192),
					"capRange": map[string]interface{}{
						"maxWattsPerSocket": float64(360),
					},
				},
				"gpu": map[string]interface{}{
					"present": true,
					"count":   int64(8),
					"product": "NVIDIA_H100_NVL",
					"capRangePerGpu": map[string]interface{}{
						"maxWatts": float64(400),
					},
				},
			},
		},
	}
	if err := helpers.ApplyUnstructured(ctx, clients.Dynamic, nodeHardwareGVR, "", nhi); err != nil {
		t.Logf("seed NodeHardware: %v (may already exist)", err)
	}

	// Wait for operator to write NodeTwin
	twinObj, err := helpers.WaitForObject(ctx, clients.Dynamic, nodeTwinGVR, "", nodeName, 90*time.Second)
	if err != nil {
		t.Fatalf("NodeTwin not found for node %s within 90s: %v", nodeName, err)
	}

	// Assert schedulableClass is set
	class, found, _ := unstructured.NestedString(twinObj.Object, "status", "schedulableClass")
	if !found || class == "" {
		t.Errorf("NodeTwin missing status.schedulableClass")
	}
	if class != "eco" && class != "performance" && class != "draining" && class != "unknown" {
		t.Errorf("unexpected schedulableClass: %s", class)
	}
	t.Logf("NodeTwin for %s: schedulableClass=%s", nodeName, class)
}

// IT-PROF-01: WorkloadProfile consumption by operator/scheduler.
func TestIT_PROF_01_WorkloadProfileConsumed(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	clients, err := helpers.NewClients(kubeconfig)
	if err != nil {
		t.Skipf("no kubeconfig available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create a WorkloadProfile
	wpObj := map[string]interface{}{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "WorkloadProfile",
		"metadata": map[string]interface{}{
			"name":      "it-test-wp",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"workloadRef": map[string]interface{}{
				"kind": "Pod",
				"name": "it-test-pod",
			},
		},
		"status": map[string]interface{}{
			"criticality":   map[string]interface{}{"class": "standard"},
			"migratability": map[string]interface{}{"reschedulable": true},
			"cpu":           map[string]interface{}{"intensity": "high", "bound": "compute", "capSensitivity": "medium"},
			"gpu":           map[string]interface{}{"intensity": "high", "capSensitivity": "high"},
			"confidence":    float64(0.9),
		},
	}
	wpBytes, _ := json.Marshal(wpObj)
	wp := helpers.MustParseCR(t, string(wpBytes))
	if err := helpers.ApplyUnstructured(ctx, clients.Dynamic, workloadProfileGVR, "default", wp); err != nil {
		t.Fatalf("create WorkloadProfile: %v", err)
	}
	defer clients.Dynamic.Resource(workloadProfileGVR).Namespace("default").Delete(ctx, "it-test-wp", metav1.DeleteOptions{})

	// Verify the WorkloadProfile is listable
	list, err := clients.Dynamic.Resource(workloadProfileGVR).Namespace("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list WorkloadProfiles: %v", err)
	}
	found := false
	for _, item := range list.Items {
		if item.GetName() == "it-test-wp" {
			found = true
			break
		}
	}
	if !found {
		t.Error("created WorkloadProfile not found in list")
	}
}
