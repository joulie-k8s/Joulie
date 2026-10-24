package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/twin"
	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	nodeTwinGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}

	// twinHardwareCatalog is used by fetchNodeHardware to fill in TDP/capRange
	// when the NodeHardware CRD lacks this data (e.g. KWOK fake nodes).
	twinHardwareCatalog *hwinv.Catalog
)

// nodeTopology holds the physical topology context for a node, and the
// facility readings that apply to it alone.
type nodeTopology struct {
	rack            string
	coolingZone     string
	rackTotalPowerW float64
	// zoneAmbientC is the ambient temperature of the node's cooling zone,
	// a reading when zoneAmbientKnown, which 0 or below may be.
	zoneAmbientC     float64
	zoneAmbientKnown bool
	// inletC is the node's own inlet temperature, a reading when inletKnown.
	inletC     float64
	inletKnown bool
}

// nodeTopologyFor returns the topology of nodeName from its rack and
// cooling-zone labels and the facility readings, or nil when there is none.
// The rack's power is the facility's measured reading, or the sum of the
// estimates of its nodes (rackPowerEstimates) when the facility has none.
func nodeTopologyFor(nodeName, rack, zone string, rackPowerEstimates map[string]float64, facility facilitySnapshot) *nodeTopology {
	topo := nodeTopology{rack: rack, coolingZone: zone}
	if rack != "" {
		topo.rackTotalPowerW = rackPowerEstimates[rack]
		if w, ok := facility.rackPowerW[rack]; ok {
			topo.rackTotalPowerW = w
		}
	}
	if zone != "" {
		topo.zoneAmbientC, topo.zoneAmbientKnown = facility.zoneAmbientC[zone]
	}
	topo.inletC, topo.inletKnown = facility.nodeInletC[nodeName]
	if rack == "" && zone == "" && !topo.inletKnown {
		return nil
	}
	return &topo
}

// reconcileNodeTwin computes and publishes NodeTwin status for one node,
// and returns the node's AgentReporting condition, nil when its
// NodeHardware could not be read. Reads go through the cache; the status
// write goes through dynClient.
//
// The ambient temperature of the twin is the most local one known: the
// node's inlet, then its cooling zone's, then the facility's outdoor
// ambient. Each may be 0 or below once known.
//
// The GPU budget counts the planned GPU cap only where it applies
// (twin.GPUCapApplies), from NodeHardware and the agent's last GPU cap
// result, which the controller manager reads and never writes.
func reconcileNodeTwin(ctx context.Context, reader kube.Reader, dynClient dynamic.Interface, nodeName, profile string, cpuCapPct, gpuCapPct float64, draining bool, topo *nodeTopology) (*metav1.Condition, error) {
	hwObj, hwErr := readNodeHardware(ctx, reader, nodeName)
	hw := twinHardware(nodeName, hwObj)
	gpuCapNotApplied := !twin.GPUCapApplies(hw.GPU, fetchGPUControlResult(ctx, reader, nodeName))

	facility := currentTwinFacility()
	outsideTempC, outsideKnown := facility.ambientC, facility.ambientKnown
	var rack, coolingZone string
	var rackPowerW float64
	if topo != nil {
		rack = topo.rack
		coolingZone = topo.coolingZone
		rackPowerW = topo.rackTotalPowerW
		switch {
		case topo.inletKnown:
			outsideTempC, outsideKnown = topo.inletC, true
		case topo.zoneAmbientKnown:
			outsideTempC, outsideKnown = topo.zoneAmbientC, true
		}
	}

	// Resolve measured node power from NODE_POWER_SOURCE (nodepower.go).
	measuredPower, source := resolveNodePower(ctx, dynClient, nodeName, hw)

	// Compute power trend from rolling window.
	trend := nodePowerTrend(nodeName, measuredPower)

	in := twin.Input{
		NodeName:           nodeName,
		Hardware:           hw,
		Profile:            profile,
		CPUCapPct:          cpuCapPct,
		GPUCapPct:          gpuCapPct,
		GPUCapNotApplied:   gpuCapNotApplied,
		Draining:           draining,
		ClusterTotalPowerW: facility.itPowerW,
		OutsideTempC:       outsideTempC,
		OutsideTempKnown:   outsideKnown,
		Rack:               rack,
		CoolingZone:        coolingZone,
		RackTotalPowerW:    rackPowerW,
		MeasuredNodePowerW: measuredPower,
		PowerTrendWPerMin:  trend,
	}
	out := twin.Compute(in)
	warnImplausibleNodePower(nodeName, source, measuredPower, out.PowerMeasurement.NodeTdpW)

	pm := &joulie.PowerMeasurement{
		Source:             source,
		MeasuredNodePowerW: measuredPower,
		CpuCappedPowerW:    out.PowerMeasurement.CpuCappedPowerW,
		GpuCappedPowerW:    out.PowerMeasurement.GpuCappedPowerW,
		NodeCappedPowerW:   out.PowerMeasurement.NodeCappedPowerW,
		CpuTdpW:            out.PowerMeasurement.CpuTdpW,
		GpuTdpW:            out.PowerMeasurement.GpuTdpW,
		NodeTdpW:           out.PowerMeasurement.NodeTdpW,
		PowerTrendWPerMin:  trend,
	}

	twinStatus := joulie.NodeTwinStatus{
		SchedulableClass:            out.SchedulableClass,
		PredictedPowerHeadroomScore: out.PredictedPowerHeadroomScore,
		PredictedCoolingStressScore: out.PredictedCoolingStressScore,
		PredictedPsuStressScore:     out.PredictedPsuStressScore,
		EffectiveCapState:           out.EffectiveCapState,
		EstimatedPUE:                out.EstimatedPUE,
		PowerMeasurement:            pm,
		LastUpdated:                 out.LastUpdated,
	}
	// A NodeHardware that could not be read says nothing about the agent,
	// so the stored condition stays.
	var agent *metav1.Condition
	if hwErr == nil {
		c := agentReportingCondition(hwObj, out.LastUpdated)
		agent = &c
		twinStatus.Conditions = []metav1.Condition{c}
	}

	return agent, upsertNodeTwinStatus(ctx, dynClient, nodeName, twinStatus)
}

// agentReportingCondition is the AgentReporting condition of a node whose
// NodeHardware is obj, nil when it has none, at now. The policy plans a
// managed node whether or not an agent runs there; this condition is what
// says that nothing enforces the plan.
func agentReportingCondition(obj *v1alpha1.NodeHardware, now time.Time) metav1.Condition {
	const check = "check that the agent DaemonSet runs on the node: its tolerations (agent.tolerations), its node selector and the platforms of its image"
	c := metav1.Condition{Type: joulie.ConditionAgentReporting}
	if obj == nil {
		c.Status, c.Reason = metav1.ConditionFalse, joulie.ReasonNoNodeHardware
		c.Message = "no agent has published NodeHardware for this node, so the caps in spec are planned and not enforced; " + check
		return c
	}
	if at, err := time.Parse(time.RFC3339, obj.Status.UpdatedAt); err == nil && now.Sub(at) > joulie.AgentReportingStaleAfter {
		c.Status, c.Reason = metav1.ConditionFalse, joulie.ReasonNodeHardwareStale
		c.Message = fmt.Sprintf("the agent last wrote NodeHardware at %s, more than %s ago, so the caps in spec are planned and not enforced; %s",
			obj.Status.UpdatedAt, joulie.AgentReportingStaleAfter, check)
		return c
	}
	c.Status, c.Reason = metav1.ConditionTrue, joulie.ReasonNodeHardwareCurrent
	c.Message = "an agent publishes NodeHardware for this node"
	return c
}

// mergeConditions sets each of desired on existing as meta.SetStatusCondition
// does: lastTransitionTime moves only when a condition's status changes, and
// a condition not in desired stays. It returns the result and the desired
// conditions whose status changed, a new condition included.
func mergeConditions(existing, desired []metav1.Condition, now time.Time) (out, changed []metav1.Condition) {
	out = append([]metav1.Condition(nil), existing...)
	for _, d := range desired {
		prev := meta.FindStatusCondition(out, d.Type)
		if prev == nil || prev.Status != d.Status {
			changed = append(changed, d)
		}
		d.LastTransitionTime = metav1.NewTime(now.UTC().Truncate(time.Second))
		meta.SetStatusCondition(&out, d)
	}
	return out, changed
}

// storedConditions reads status.conditions of a NodeTwin read through the
// dynamic client, through the typed status, so a field the API server sends
// in another shape fails here rather than reaching the merge.
func storedConditions(obj *unstructured.Unstructured) ([]metav1.Condition, error) {
	if obj == nil {
		return nil, nil
	}
	raw, ok, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	if err != nil || !ok {
		return nil, err
	}
	var status v1alpha1.NodeTwinStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(map[string]any{"conditions": raw}, &status); err != nil {
		return nil, err
	}
	return status.Conditions, nil
}

var (
	implausiblePowerWarnedMu sync.Mutex
	implausiblePowerWarned   = map[string]struct{}{}
)

// implausibleNodePower reports whether a measured power reading cannot be a
// real instantaneous reading for this node. The usual cause is a query that
// returns a cumulative energy counter (joules since boot) instead of watts.
func implausibleNodePower(measuredW, nodeTdpW float64) bool {
	if nodeTdpW <= 0 || measuredW <= 0 {
		return false
	}
	return measuredW > 2*nodeTdpW
}

func warnImplausibleNodePower(nodeName, source string, measuredW, nodeTdpW float64) {
	if !implausibleNodePower(measuredW, nodeTdpW) {
		return
	}
	implausiblePowerWarnedMu.Lock()
	_, seen := implausiblePowerWarned[nodeName]
	implausiblePowerWarned[nodeName] = struct{}{}
	implausiblePowerWarnedMu.Unlock()
	if seen {
		return
	}
	log.Printf("warning: node=%s measured power %.0fW exceeds twice its TDP %.0fW (source=%s); if the query returns a joules counter, wrap it in rate() to get watts",
		nodeName, measuredW, nodeTdpW, source)
}

// fetchNodeHardware reads the node's NodeHardware from the cache. A missing
// object, or a cluster without the CRD, yields an empty inventory that the
// catalog fills in as far as it can.
func fetchNodeHardware(ctx context.Context, reader kube.Reader, nodeName string) joulie.NodeHardware {
	obj, _ := readNodeHardware(ctx, reader, nodeName)
	return twinHardware(nodeName, obj)
}

// readNodeHardware returns the node's NodeHardware from the cache: nil with
// no error when the node has none or the cluster lacks the CRD, and an error,
// logged, for any other failure.
func readNodeHardware(ctx context.Context, reader kube.Reader, nodeName string) (*v1alpha1.NodeHardware, error) {
	var obj v1alpha1.NodeHardware
	if err := reader.Get(ctx, client.ObjectKey{Name: sanitizeName(nodeName)}, &obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		log.Printf("NodeHardware of node %s: %v", nodeName, err)
		return nil, err
	}
	return &obj, nil
}

// twinHardware is the twin's inventory of a node whose NodeHardware is obj:
// empty when it is nil, else obj's facts with the gaps the CRD leaves filled
// from the hardware catalog.
func twinHardware(nodeName string, obj *v1alpha1.NodeHardware) joulie.NodeHardware {
	if obj == nil {
		return joulie.NodeHardware{NodeName: nodeName}
	}
	hw := twinHardwareFromNodeHardware(nodeName, obj)
	enrichHardwareFromCatalog(&hw)
	return hw
}

// fetchGPUControlResult reads the agent's last GPU cap result,
// NodeTwin status.controlStatus.gpu, from the cache, or nil when the node has
// none yet.
func fetchGPUControlResult(ctx context.Context, reader kube.Reader, nodeName string) *joulie.ControlResult {
	var obj v1alpha1.NodeTwin
	if err := reader.Get(ctx, client.ObjectKey{Name: sanitizeName(nodeName)}, &obj); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			log.Printf("fetchGPUControlResult: %v", err)
		}
		return nil
	}
	cs := obj.Status.ControlStatus
	if cs == nil || cs.GPU == nil {
		return nil
	}
	return &joulie.ControlResult{Backend: cs.GPU.Backend, Result: cs.GPU.Result, Message: cs.GPU.Message}
}

// enrichHardwareFromCatalog fills in missing TDP/capRange data from the
// hardware catalog. This is critical for KWOK fake nodes where the agent
// can't discover real hardware capabilities.
func enrichHardwareFromCatalog(hw *joulie.NodeHardware) {
	if twinHardwareCatalog == nil {
		return
	}

	// Try to match GPU model and fill in capRange if missing.
	if hw.GPU.Count > 0 && hw.GPU.CapRange.MaxWatts <= 0 {
		// Try model first, then rawModel.
		gpuQuery := hw.GPU.Model
		if gpuQuery == "" {
			gpuQuery = hw.GPU.RawModel
		}
		if _, spec, ok := twinHardwareCatalog.MatchGPU(gpuQuery); ok {
			hw.GPU.CapRange.MaxWatts = spec.Official.MaxBoardPowerW
			if spec.Official.MinBoardPowerW > 0 {
				hw.GPU.CapRange.MinWatts = spec.Official.MinBoardPowerW
			}
			hw.GPU.Present = true
		}
	}

	// Try to match CPU model and fill in capRange if missing.
	if hw.CPU.CapRange.MaxWattsPerSocket <= 0 {
		cpuQuery := hw.CPU.Model
		if cpuQuery == "" {
			cpuQuery = hw.CPU.RawModel
		}
		if cpuQuery != "" {
			// The socket range the agent and the simulator use for this
			// CPU (hwinv's SocketCapRangeW), so the twin's capped budget
			// clamps where the agent's cap does.
			if _, spec, ok := twinHardwareCatalog.MatchCPU(cpuQuery); ok {
				if minW, maxW, ok := spec.Official.SocketCapRangeW(); ok {
					hw.CPU.CapRange.MaxWattsPerSocket = maxW
					hw.CPU.CapRange.MinWattsPerSocket = minW
				}
			}
		}
		// Fallback: if CPU model unknown but we have core count, estimate
		// TDP from a conservative 10W/core heuristic (typical server range).
		if hw.CPU.CapRange.MaxWattsPerSocket <= 0 && hw.CPU.TotalCores > 0 {
			sockets := hw.CPU.Sockets
			if sockets <= 0 {
				sockets = 1
				hw.CPU.Sockets = 1
			}
			coresPerSocket := hw.CPU.TotalCores / sockets
			hw.CPU.CapRange.MaxWattsPerSocket = float64(coresPerSocket) * 5.0 // ~5W/core TDP
		}
	}
}

// upsertNodeTwinStatus patches the status subresource of a NodeTwin CR.
func upsertNodeTwinStatus(ctx context.Context, dynClient dynamic.Interface, nodeName string, status joulie.NodeTwinStatus) error {
	// Same object name as upsertNodeTwinSpec, otherwise a node whose name is
	// not a valid object name (for example one with dots) gets two NodeTwins:
	// one holding the spec and one holding the status.
	name := sanitizeName(nodeName)

	// Ensure the object exists first (it may have been created by upsertNodeTwinSpec)
	existing, err := dynClient.Resource(nodeTwinGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get NodeTwin %s: %w", nodeName, err)
	}
	notFound := apierrors.IsNotFound(err)
	if len(status.Conditions) > 0 {
		var stored []metav1.Condition
		if !notFound {
			if stored, err = storedConditions(existing); err != nil {
				return fmt.Errorf("NodeTwin %s status.conditions: %w", nodeName, err)
			}
		}
		var changed []metav1.Condition
		status.Conditions, changed = mergeConditions(stored, status.Conditions, time.Now())
		for _, c := range changed {
			if c.Type == joulie.ConditionAgentReporting && c.Status == metav1.ConditionFalse {
				log.Printf("warning: node=%s %s=False reason=%s: %s", nodeName, c.Type, c.Reason, c.Message)
			}
		}
	}
	statusMap := nodeTwinStatusToMap(status)

	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal NodeTwin status patch: %w", err)
	}

	if notFound {
		// Create a minimal object; spec will be filled by upsertNodeTwinSpec
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "joulie.io/v1alpha1",
				"kind":       "NodeTwin",
				"metadata": map[string]interface{}{
					"name": name,
				},
				"spec": map[string]interface{}{
					"nodeName": nodeName,
					"profile":  "unknown",
				},
			},
		}
		if _, err := dynClient.Resource(nodeTwinGVR).Create(ctx, obj, metav1.CreateOptions{FieldManager: joulie.FieldManagerControllerManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create NodeTwin %s: %w", nodeName, err)
		}
	}

	// Patch status subresource
	_, err = dynClient.Resource(nodeTwinGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{FieldManager: joulie.FieldManagerControllerManager}, "status",
	)
	if err != nil {
		// Fallback: full patch if status subresource not available
		fullPatch := map[string]interface{}{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata":   map[string]interface{}{"name": name},
			"status":     statusMap,
		}
		fp, err := json.Marshal(fullPatch)
		if err != nil {
			return fmt.Errorf("marshal NodeTwin %s status patch: %w", nodeName, err)
		}
		_, err = dynClient.Resource(nodeTwinGVR).Patch(ctx, name, types.MergePatchType, fp, metav1.PatchOptions{FieldManager: joulie.FieldManagerControllerManager})
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
	if g := a.CPUGovernor; g != nil {
		governor := map[string]any{"preset": g.Preset}
		if g.Boost != "" {
			governor["boost"] = g.Boost
		}
		cpu["governor"] = governor
	}
	spec["cpu"] = cpu
	gpu := map[string]any{}
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
		gpu["powerCap"] = powerCap
	}
	if g := a.GPUGovernor; g != nil {
		governor := map[string]any{"preset": g.Preset}
		if g.ComputeClockCeilingPctOfMax != nil {
			governor["computeClockCeilingPctOfMax"] = *g.ComputeClockCeilingPctOfMax
		}
		gpu["governor"] = governor
	}
	if len(gpu) > 0 {
		spec["gpu"] = gpu
	}
	// The whole spec is replaced below, so a governor the assignment no
	// longer carries (GOVERNOR_CONTROL turned off, a preset the node stopped
	// listing, but not a block in a discovery gap: governorIntents) leaves
	// the object in the same write.

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
		_, err := res.Create(ctx, obj, metav1.CreateOptions{FieldManager: joulie.FieldManagerControllerManager})
		if err != nil {
			return fmt.Errorf("create NodeTwin %s: %w", name, err)
		}
		return nil
	}

	// A write bumps resourceVersion and wakes every watcher of the object,
	// so only write when the desired state actually changed.
	if jsonEqual(existing.Object["spec"], obj.Object["spec"]) {
		return nil
	}
	existing.Object["spec"] = obj.Object["spec"]
	if _, err := res.Update(ctx, existing, metav1.UpdateOptions{FieldManager: joulie.FieldManagerControllerManager}); err != nil {
		return fmt.Errorf("update NodeTwin %s: %w", name, err)
	}
	return nil
}

// jsonEqual compares two values by their JSON encoding, which erases the
// int64/float64 difference between objects read from the API server and
// objects built in Go.
func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}

func nodeTwinStatusToMap(status joulie.NodeTwinStatus) map[string]interface{} {
	m := map[string]interface{}{
		"schedulableClass":            status.SchedulableClass,
		"predictedPowerHeadroomScore": status.PredictedPowerHeadroomScore,
		"predictedCoolingStressScore": status.PredictedCoolingStressScore,
		"predictedPsuStressScore":     status.PredictedPsuStressScore,
		// Removed from the CRD. In this merge patch null deletes the value a
		// previous release stored, including on clusters where Helm kept the
		// old CRD (it never upgrades crds/). Drop this line in the next minor
		// release.
		"hardwareDensityScore": nil,
		"estimatedPUE":         status.EstimatedPUE,
		"lastUpdated":          status.LastUpdated.Format(time.RFC3339),
		"effectiveCapState": map[string]interface{}{
			"cpuPct": status.EffectiveCapState.CPUPct,
			"gpuPct": status.EffectiveCapState.GPUPct,
		},
	}
	if len(status.Conditions) > 0 {
		conditions := make([]interface{}, 0, len(status.Conditions))
		for _, c := range status.Conditions {
			cm := map[string]interface{}{
				"type":               c.Type,
				"status":             string(c.Status),
				"reason":             c.Reason,
				"message":            c.Message,
				"lastTransitionTime": c.LastTransitionTime.UTC().Format(time.RFC3339),
			}
			if c.ObservedGeneration != 0 {
				cm["observedGeneration"] = c.ObservedGeneration
			}
			conditions = append(conditions, cm)
		}
		m["conditions"] = conditions
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

// --- Measured power trend ---

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
