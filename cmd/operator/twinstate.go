package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/operator/twin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	nodeTwinGVR         = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}
	twinNodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}
)

// nodeTopology holds the physical topology context for a node.
type nodeTopology struct {
	rack            string
	coolingZone     string
	rackTotalPowerW float64
	zoneAmbientC    float64 // per-zone ambient; 0 = use global
}

// reconcileNodeTwin computes and publishes NodeTwin status for one node.
func reconcileNodeTwin(ctx context.Context, dynClient dynamic.Interface, nodeName, profile string, cpuCapPct, gpuCapPct float64, draining bool, topo *nodeTopology) error {
	hw := fetchNodeHardware(ctx, dynClient, nodeName)

	outsideTempC := facilityAmbientTempC
	var rack, coolingZone string
	var rackPowerW float64
	if topo != nil {
		rack = topo.rack
		coolingZone = topo.coolingZone
		rackPowerW = topo.rackTotalPowerW
		if topo.zoneAmbientC > 0 {
			outsideTempC = topo.zoneAmbientC
		}
	}

	// Resolve measured node power. For now, use static estimation (tier 3).
	// Future: add Kepler (tier 1) and utilization-based (tier 2) sources.
	measuredPower, source := resolveNodePower(ctx, dynClient, nodeName, hw)

	// Compute power trend from rolling window.
	trend := nodePowerTrend(nodeName, measuredPower)

	in := twin.Input{
		NodeName:            nodeName,
		Hardware:            hw,
		Profile:             profile,
		CPUCapPct:           cpuCapPct,
		GPUCapPct:           gpuCapPct,
		Draining:            draining,
		ClusterTotalPowerW:  facilityClusterPowerW,
		OutsideTempC:        outsideTempC,
		Rack:                rack,
		CoolingZone:         coolingZone,
		RackTotalPowerW:     rackPowerW,
		MeasuredNodePowerW:  measuredPower,
		PowerTrendWPerMin:   trend,
	}
	out := twin.Compute(in)

	pm := &joulie.PowerMeasurement{
		Source:             source,
		MeasuredNodePowerW: measuredPower,
		CpuCappedPowerW:   out.PowerMeasurement.CpuCappedPowerW,
		GpuCappedPowerW:   out.PowerMeasurement.GpuCappedPowerW,
		NodeCappedPowerW:  out.PowerMeasurement.NodeCappedPowerW,
		CpuTdpW:           out.PowerMeasurement.CpuTdpW,
		GpuTdpW:           out.PowerMeasurement.GpuTdpW,
		NodeTdpW:          out.PowerMeasurement.NodeTdpW,
		PowerTrendWPerMin: trend,
	}

	twinStatus := joulie.NodeTwinStatus{
		SchedulableClass:            out.SchedulableClass,
		PredictedPowerHeadroomScore: out.PredictedPowerHeadroomScore,
		PredictedCoolingStressScore: out.PredictedCoolingStressScore,
		PredictedPsuStressScore:     out.PredictedPsuStressScore,
		EffectiveCapState:           out.EffectiveCapState,
		HardwareDensityScore:        out.HardwareDensityScore,
		EstimatedPUE:                out.EstimatedPUE,
		PowerMeasurement:            pm,
		LastUpdated:                 out.LastUpdated,
	}

	return upsertNodeTwinStatus(ctx, dynClient, nodeName, twinStatus)
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
		if v, ok := cpu["sockets"].(float64); ok {
			hw.CPU.Sockets = int(v)
		} else if v, ok := cpu["sockets"].(int64); ok {
			hw.CPU.Sockets = int(v)
		}
		if v, ok := cpu["totalCores"].(float64); ok {
			hw.CPU.TotalCores = int(v)
		} else if v, ok := cpu["totalCores"].(int64); ok {
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
		if v, ok := gpu["count"].(float64); ok {
			hw.GPU.Count = int(v)
		} else if v, ok := gpu["count"].(int64); ok {
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

// upsertNodeTwinStatus patches the status subresource of a NodeTwin CR.
func upsertNodeTwinStatus(ctx context.Context, dynClient dynamic.Interface, nodeName string, status joulie.NodeTwinStatus) error {
	statusMap := nodeTwinStatusToMap(status)

	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal NodeTwin status patch: %w", err)
	}

	// Ensure the object exists first (it may have been created by upsertNodeTwinSpec)
	_, err = dynClient.Resource(nodeTwinGVR).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get NodeTwin %s: %w", nodeName, err)
	}
	if apierrors.IsNotFound(err) {
		// Create a minimal object; spec will be filled by upsertNodeTwinSpec
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "joulie.io/v1alpha1",
				"kind":       "NodeTwin",
				"metadata": map[string]interface{}{
					"name": nodeName,
				},
				"spec": map[string]interface{}{
					"nodeName": nodeName,
					"profile":  "unknown",
				},
			},
		}
		if _, err := dynClient.Resource(nodeTwinGVR).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create NodeTwin %s: %w", nodeName, err)
		}
	}

	// Patch status subresource
	_, err = dynClient.Resource(nodeTwinGVR).Patch(
		ctx, nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	if err != nil {
		// Fallback: full patch if status subresource not available
		fullPatch := map[string]interface{}{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata":   map[string]interface{}{"name": nodeName},
			"status":     statusMap,
		}
		fp, err := json.Marshal(fullPatch)
		if err != nil {
			return fmt.Errorf("marshal NodeTwin %s status patch: %w", nodeName, err)
		}
		_, err = dynClient.Resource(nodeTwinGVR).Patch(ctx, nodeName, types.MergePatchType, fp, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("patch NodeTwin %s status: %w", nodeName, err)
		}
	}

	return nil
}

// upsertNodeTwinSpec creates or updates the spec portion of a NodeTwin CR.
func upsertNodeTwinSpec(ctx context.Context, dyn dynamic.Interface, a NodeAssignment) error {
	name := sanitizeName(a.NodeName)
	spec := map[string]any{
		"nodeName": a.NodeName,
		"profile":  a.Profile,
		"policy": map[string]any{
			"name": a.ManagedBy,
		},
		"scheduling": map[string]any{
			"draining": a.Draining,
		},
	}
	cpu := map[string]any{}
	if a.CPUCapPctOfMax != nil {
		cpu["packagePowerCapPctOfMax"] = *a.CPUCapPctOfMax
	} else if a.CapWatts > 0 {
		cpu["packagePowerCapWatts"] = a.CapWatts
	}
	// If both CPUCapPctOfMax is nil and CapWatts is 0, no CPU cap is written.
	// The agent will leave the current cap unchanged.
	spec["cpu"] = cpu
	if a.GPU != nil {
		powerCap := map[string]any{
			"scope": "perGpu",
		}
		if a.GPU.CapWattsPerGPU != nil {
			powerCap["capWattsPerGpu"] = *a.GPU.CapWattsPerGPU
		}
		if a.GPU.CapPctOfMax != nil {
			powerCap["capPctOfMax"] = *a.GPU.CapPctOfMax
		}
		spec["gpu"] = map[string]any{
			"powerCap": powerCap,
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": spec,
	}}

	res := dyn.Resource(nodeTwinGVR)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get NodeTwin %s: %w", name, err)
		}
		_, err := res.Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create NodeTwin %s: %w", name, err)
		}
		return nil
	}

	existing.Object["spec"] = obj.Object["spec"]
	if _, err := res.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update NodeTwin %s: %w", name, err)
	}
	return nil
}

func nodeTwinStatusToMap(status joulie.NodeTwinStatus) map[string]interface{} {
	m := map[string]interface{}{
		"schedulableClass":            status.SchedulableClass,
		"predictedPowerHeadroomScore": status.PredictedPowerHeadroomScore,
		"predictedCoolingStressScore": status.PredictedCoolingStressScore,
		"predictedPsuStressScore":     status.PredictedPsuStressScore,
		"hardwareDensityScore":        status.HardwareDensityScore,
		"estimatedPUE":                status.EstimatedPUE,
		"lastUpdated":                 status.LastUpdated.Format(time.RFC3339),
		"effectiveCapState": map[string]interface{}{
			"cpuPct": status.EffectiveCapState.CPUPct,
			"gpuPct": status.EffectiveCapState.GPUPct,
		},
	}
	if status.PowerMeasurement != nil {
		m["powerMeasurement"] = map[string]interface{}{
			"source":             status.PowerMeasurement.Source,
			"measuredNodePowerW": status.PowerMeasurement.MeasuredNodePowerW,
			"cpuCappedPowerW":    status.PowerMeasurement.CpuCappedPowerW,
			"gpuCappedPowerW":    status.PowerMeasurement.GpuCappedPowerW,
			"nodeCappedPowerW":   status.PowerMeasurement.NodeCappedPowerW,
			"cpuTdpW":            status.PowerMeasurement.CpuTdpW,
			"gpuTdpW":            status.PowerMeasurement.GpuTdpW,
			"nodeTdpW":           status.PowerMeasurement.NodeTdpW,
			"powerTrendWPerMin":  status.PowerMeasurement.PowerTrendWPerMin,
		}
	}
	return m
}

// --- Measured power resolution ---

// nodePowerSamples stores recent power measurements for trend computation.
var (
	nodePowerSamplesMu sync.Mutex
	nodePowerSamples   = map[string][]powerSample{}
)

type powerSample struct {
	watts float64
	at    time.Time
}

const powerTrendWindow = 5 * time.Minute

// resolveNodePower returns the best available measured power for a node.
// Currently implements tier 3 (static estimation from pod specs).
// Future: tier 1 (Kepler) and tier 2 (utilization-based) will be added.
func resolveNodePower(_ context.Context, _ dynamic.Interface, _ string, _ joulie.NodeHardware) (float64, string) {
	// Tier 3: static estimation. For now, return 0 (no pods known at this level).
	// The operator will populate this from pod listings in a future iteration.
	return 0, "static"
}

// nodePowerTrend computes the power trend (watts/min) for a node from a
// rolling window of power samples.
func nodePowerTrend(nodeName string, currentPower float64) float64 {
	now := time.Now()
	nodePowerSamplesMu.Lock()
	defer nodePowerSamplesMu.Unlock()

	samples := nodePowerSamples[nodeName]
	samples = append(samples, powerSample{watts: currentPower, at: now})

	// Trim old samples outside the window.
	cutoff := now.Add(-powerTrendWindow)
	firstValid := 0
	for i, s := range samples {
		if s.at.After(cutoff) {
			firstValid = i
			break
		}
	}
	samples = samples[firstValid:]
	nodePowerSamples[nodeName] = samples

	if len(samples) < 2 {
		return 0
	}

	oldest := samples[0]
	newest := samples[len(samples)-1]
	elapsed := newest.at.Sub(oldest.at).Minutes()
	if elapsed < 0.1 {
		return 0
	}
	return (newest.watts - oldest.watts) / elapsed
}
