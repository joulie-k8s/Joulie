package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/operator/migration"
	"github.com/matbun/joulie/pkg/operator/twin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	nodeTwinStateGVR   = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwinstates"}
	twinNodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}
	workloadProfileGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "workloadprofiles"}
)

// reconcileNodeTwinState computes and publishes NodeTwinState for one node.
func reconcileNodeTwinState(ctx context.Context, dynClient dynamic.Interface, nodeName, profile string, cpuCapPct, gpuCapPct float64, draining bool) error {
	// Fetch NodeHardware
	hw := fetchNodeHardware(ctx, dynClient, nodeName)

	// Fetch WorkloadProfiles for pods on this node
	workloads := fetchWorkloadProfilesForNode(ctx, dynClient, nodeName)

	// Compute twin state
	in := twin.Input{
		NodeName:  nodeName,
		Hardware:  hw,
		Profile:   profile,
		CPUCapPct: cpuCapPct,
		GPUCapPct: gpuCapPct,
		Draining:  draining,
		Workloads: workloads,
	}
	out := twin.Compute(in)

	// Compute migration recommendations
	twinState := joulie.NodeTwinState{
		NodeName:                    nodeName,
		SchedulableClass:            out.SchedulableClass,
		PredictedPowerHeadroomScore: out.PredictedPowerHeadroomScore,
		PredictedCoolingStressScore: out.PredictedCoolingStressScore,
		PredictedPsuStressScore:     out.PredictedPsuStressScore,
		EffectiveCapState:           out.EffectiveCapState,
		HardwareDensityScore:        out.HardwareDensityScore,
		LastUpdated:                 out.LastUpdated,
	}

	// Build migration recommendations
	var workloadsOnNode []migration.WorkloadOnNode
	// WorkloadProfiles are already fetched; build WorkloadOnNode list
	// (ref is approximated since we don't have full pod names here)
	for _, w := range workloads {
		workloadsOnNode = append(workloadsOnNode, migration.WorkloadOnNode{
			Ref:     joulie.WorkloadRef{Kind: "Pod", Namespace: "default", Name: "unknown"},
			Profile: w,
		})
	}
	recs := migration.EvaluateNode(twinState, workloadsOnNode, migration.DefaultPolicy())
	twinState.RescheduleRecommendations = append(out.RescheduleRecommendations, recs...)

	// Write NodeTwinState CR
	return upsertNodeTwinState(ctx, dynClient, nodeName, twinState)
}

// fetchNodeHardware reads NodeHardware for the node from the API.
func fetchNodeHardware(ctx context.Context, dynClient dynamic.Interface, nodeName string) joulie.NodeHardware {
	hw := joulie.NodeHardware{NodeName: nodeName}

	obj, err := dynClient.Resource(twinNodeHardwareGVR).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("fetchNodeHardware: %v", err)
		}
		return hw
	}

	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	if status == nil {
		return hw
	}

	if cpu, ok := status["cpu"].(map[string]interface{}); ok {
		if v, ok := cpu["vendor"].(string); ok {
			hw.CPU.Vendor = v
		}
		if v, ok := cpu["model"].(string); ok {
			hw.CPU.Model = v
		}
		if v, ok := cpu["sockets"].(int64); ok {
			hw.CPU.Sockets = int(v)
		}
		if v, ok := cpu["totalCores"].(int64); ok {
			hw.CPU.TotalCores = int(v)
		}
		if v, ok := cpu["driverFamily"].(string); ok {
			hw.CPU.DriverFamily = v
		}
		if cr, ok := cpu["capRange"].(map[string]interface{}); ok {
			if v, ok := cr["maxWattsPerSocket"].(float64); ok {
				hw.CPU.CapRange.MaxWattsPerSocket = v
			}
			if v, ok := cr["minWattsPerSocket"].(float64); ok {
				hw.CPU.CapRange.MinWattsPerSocket = v
			}
		}
	}

	if gpu, ok := status["gpu"].(map[string]interface{}); ok {
		if v, ok := gpu["present"].(bool); ok {
			hw.GPU.Present = v
		}
		if v, ok := gpu["vendor"].(string); ok {
			hw.GPU.Vendor = v
		}
		if v, ok := gpu["model"].(string); ok {
			hw.GPU.Model = v
		}
		if v, ok := gpu["count"].(int64); ok {
			hw.GPU.Count = int(v)
		}
		if cr, ok := gpu["capRangePerGpu"].(map[string]interface{}); ok {
			if v, ok := cr["maxWatts"].(float64); ok {
				hw.GPU.CapRange.MaxWatts = v
			}
		}
		if slicing, ok := gpu["slicing"].(map[string]interface{}); ok {
			if v, ok := slicing["supported"].(bool); ok {
				hw.GPU.Slicing.Supported = v
			}
		}
	}

	return hw
}

// fetchWorkloadProfilesForNode returns WorkloadProfile statuses for pods on this node.
// Currently fetches all WorkloadProfiles in all namespaces and returns them.
// A production implementation would filter by node affinity / pod scheduling.
func fetchWorkloadProfilesForNode(ctx context.Context, dynClient dynamic.Interface, nodeName string) []joulie.WorkloadProfileStatus {
	var profiles []joulie.WorkloadProfileStatus

	list, err := dynClient.Resource(workloadProfileGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("fetchWorkloadProfiles: %v", err)
		}
		return profiles
	}

	for _, item := range list.Items {
		status, _, _ := unstructured.NestedMap(item.Object, "status")
		if status == nil {
			continue
		}
		wp := parseWorkloadProfileStatus(status)
		profiles = append(profiles, wp)
	}
	return profiles
}

func parseWorkloadProfileStatus(status map[string]interface{}) joulie.WorkloadProfileStatus {
	wp := joulie.WorkloadProfileStatus{}

	if crit, ok := status["criticality"].(map[string]interface{}); ok {
		if v, ok := crit["class"].(string); ok {
			wp.Criticality.Class = v
		}
	}
	if mig, ok := status["migratability"].(map[string]interface{}); ok {
		if v, ok := mig["reschedulable"].(bool); ok {
			wp.Migratability.Reschedulable = v
		}
	}
	if cpu, ok := status["cpu"].(map[string]interface{}); ok {
		if v, ok := cpu["intensity"].(string); ok {
			wp.CPU.Intensity = v
		}
		if v, ok := cpu["bound"].(string); ok {
			wp.CPU.Bound = v
		}
		if v, ok := cpu["capSensitivity"].(string); ok {
			wp.CPU.CapSensitivity = v
		}
	}
	if gpu, ok := status["gpu"].(map[string]interface{}); ok {
		if v, ok := gpu["intensity"].(string); ok {
			wp.GPU.Intensity = v
		}
		if v, ok := gpu["capSensitivity"].(string); ok {
			wp.GPU.CapSensitivity = v
		}
	}
	return wp
}

// upsertNodeTwinState creates or updates a NodeTwinState CR.
func upsertNodeTwinState(ctx context.Context, dynClient dynamic.Interface, nodeName string, state joulie.NodeTwinState) error {
	statusMap := nodeTwinStateToMap(state)

	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal NodeTwinState patch: %w", err)
	}

	// Ensure the object exists first
	_, err = dynClient.Resource(nodeTwinStateGVR).Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "joulie.io/v1alpha1",
				"kind":       "NodeTwinState",
				"metadata": map[string]interface{}{
					"name": nodeName,
				},
				"spec": map[string]interface{}{
					"nodeName": nodeName,
				},
			},
		}
		if _, err := dynClient.Resource(nodeTwinStateGVR).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create NodeTwinState %s: %w", nodeName, err)
		}
	}

	// Patch status
	_, err = dynClient.Resource(nodeTwinStateGVR).Patch(
		ctx, nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	if err != nil {
		// If status subresource not available, try full patch
		fullPatch := map[string]interface{}{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwinState",
			"metadata":   map[string]interface{}{"name": nodeName},
			"spec":       map[string]interface{}{"nodeName": nodeName},
			"status":     statusMap,
		}
		fp, _ := json.Marshal(fullPatch)
		_, err = dynClient.Resource(nodeTwinStateGVR).Patch(ctx, nodeName, types.MergePatchType, fp, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("patch NodeTwinState %s: %w", nodeName, err)
		}
	}

	return nil
}

func nodeTwinStateToMap(state joulie.NodeTwinState) map[string]interface{} {
	m := map[string]interface{}{
		"nodeName":                    state.NodeName,
		"schedulableClass":            state.SchedulableClass,
		"predictedPowerHeadroomScore": state.PredictedPowerHeadroomScore,
		"predictedCoolingStressScore": state.PredictedCoolingStressScore,
		"predictedPsuStressScore":     state.PredictedPsuStressScore,
		"hardwareDensityScore":        state.HardwareDensityScore,
		"lastUpdated":                 state.LastUpdated.Format(time.RFC3339),
		"effectiveCapState": map[string]interface{}{
			"cpuPct": state.EffectiveCapState.CPUPct,
			"gpuPct": state.EffectiveCapState.GPUPct,
		},
	}

	if len(state.RescheduleRecommendations) > 0 {
		recs := make([]interface{}, len(state.RescheduleRecommendations))
		for i, r := range state.RescheduleRecommendations {
			recs[i] = map[string]interface{}{
				"workloadRef": map[string]interface{}{
					"kind":      r.WorkloadRef.Kind,
					"namespace": r.WorkloadRef.Namespace,
					"name":      r.WorkloadRef.Name,
				},
				"reason": r.Reason,
			}
		}
		m["rescheduleRecommendations"] = recs
	}
	return m
}
