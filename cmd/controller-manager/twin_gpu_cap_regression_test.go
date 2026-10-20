package main

import (
	"context"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/hwinv"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The twin's GPU budget counts the planned GPU cap only where it applies.
// The old code multiplied the GPU TDP by the planned percentage on every GPU
// node, so a node whose GPUs cannot be capped (NodeHardware
// status.gpu.controlAvailable false, as for NVIDIA boards whose power limits
// read [N/A]) or whose GPU cap write the agent reported blocked or error
// showed a budget, and so a headroom, for a saving that never happened. An
// applied write counts even without controlAvailable: the simulator's HTTP
// backend caps GPUs the agent cannot list. The result values are spelled as
// the agent writes them.
func TestTwinGPUBudgetCountsOnlyAGPUCapThatApplies(t *testing.T) {
	// Without control the agent publishes no capRangePerGpu and no present
	// (upsertNodeHardwareStatus), and the twin takes the TDP from the catalog.
	catalog, err := hwinv.LoadCatalog("")
	if err != nil {
		t.Fatalf("load the embedded catalog: %v", err)
	}
	saved := twinHardwareCatalog
	twinHardwareCatalog = catalog
	t.Cleanup(func() { twinHardwareCatalog = saved })

	const (
		withRange   = `{"present": true, "count": 4, "capRangePerGpu": {"minWatts": 100, "maxWatts": 300}, "controlAvailable": true}`
		rangeNoCtl  = `{"present": true, "count": 4, "capRangePerGpu": {"minWatts": 100, "maxWatts": 300}}`
		naBoard     = `{"rawModel": "NVIDIA L40S", "count": 4}` // four 350 W boards, limits [N/A]
		capped300   = 4 * 300 * 0.6                             // eco at 60 %
		uncapped300 = 4 * 300.0
		capped350   = 4 * 350 * 0.6
		uncapped350 = 4 * 350.0
	)
	for _, tc := range []struct {
		name   string
		gpu    string // NodeHardware status.gpu
		result string // NodeTwin status.controlStatus.gpu.result, "" for none yet
		want   float64
	}{
		{"GPUs with control, no result yet", withRange, "", capped300},
		{"GPUs with control, applied", withRange, "applied", capped300},
		{"GPUs with control, blocked", withRange, "blocked", uncapped300},
		{"GPUs with control, error", withRange, "error", uncapped300},
		{"a range but no control", rangeNoCtl, "", uncapped300},
		{"[N/A] board, no result yet", naBoard, "", uncapped350},
		{"[N/A] board, blocked", naBoard, "blocked", uncapped350},
		{"[N/A] board, applied through the HTTP backend", naBoard, "applied", capped350},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hw := nodeHardwareFromJSON(t, `{"apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
				"metadata": {"name": "g1"}, "spec": {"nodeName": "g1"}, "status": {"gpu": `+tc.gpu+`}}`)
			nt := &v1alpha1.NodeTwin{
				ObjectMeta: metav1.ObjectMeta{Name: "g1"},
				Spec:       v1alpha1.NodeTwinSpec{NodeName: "g1", Profile: profileEco},
			}
			if tc.result != "" {
				nt.Status.ControlStatus = &v1alpha1.ControlStatus{GPU: &v1alpha1.ControlResult{Backend: "none", Result: tc.result}}
			}
			reader, _, dyn := fakeClients(t, hw, nt)
			if err := reconcileNodeTwin(context.Background(), reader, dyn, "g1", profileEco, 60, 60, false, nil); err != nil {
				t.Fatalf("reconcileNodeTwin: %v", err)
			}
			u, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "g1", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get NodeTwin: %v", err)
			}
			var got v1alpha1.NodeTwin
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &got); err != nil {
				t.Fatalf("decode NodeTwin: %v", err)
			}
			pm := got.Status.PowerMeasurement
			if pm == nil {
				t.Fatalf("no powerMeasurement written: %+v", got.Status)
			}
			if pm.GPUCappedPowerW != tc.want || pm.NodeCappedPowerW != tc.want {
				t.Fatalf("gpuCappedPowerW = %g, nodeCappedPowerW = %g, want %g for both (gpuTdpW %g)",
					pm.GPUCappedPowerW, pm.NodeCappedPowerW, tc.want, pm.GPUTdpW)
			}
			if c := got.Status.ControlStatus; tc.result != "" && (c == nil || c.GPU == nil || c.GPU.Result != tc.result) {
				t.Fatalf("the agent's control status changed: %+v", c)
			}
		})
	}
}
