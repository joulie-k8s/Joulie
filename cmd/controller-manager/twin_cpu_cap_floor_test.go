package main

import (
	"context"
	"math"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/hwinv"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The twin's CPU budget under a percent cap is what the agent applies: pct
// of the node's CPU TDP, clamped up to the bottom of its cap range. A pool
// node publishes no capRange, so the twin takes the socket range from the
// catalog, the one the agent and the simulator use (hwinv's
// SocketCapRangeW, whose floor is 0.55 x TDP; the cTDP range is no floor):
// on a 2x EPYC 9654 at 60 % the agent sends 432 W. The twin used to fill no
// minimum for a CPU without a cTDP range. A range the agent read
// from RAPL files clamps the same way.
func TestTwinCPUBudgetClampsAtTheCapRangeMinimum(t *testing.T) {
	catalog, err := hwinv.LoadCatalog("")
	if err != nil {
		t.Fatalf("load the embedded catalog: %v", err)
	}
	saved := twinHardwareCatalog
	twinHardwareCatalog = catalog
	t.Cleanup(func() { twinHardwareCatalog = saved })

	for _, tc := range []struct {
		name string
		cpu  string // NodeHardware status.cpu
		pct  float64
		want float64
	}{
		{"2x EPYC 9654 at 60 %, catalog range", `{"rawModel": "AMD EPYC 9654 96-Core Processor", "sockets": 2}`, 60, 432},
		{"2x EPYC 9654 at 95 %, inside the range", `{"rawModel": "AMD EPYC 9654 96-Core Processor", "sockets": 2}`, 95, 684},
		{"Xeon 8260 at 50 %, no cTDP range", `{"rawModel": "Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz", "sockets": 1}`, 50, 0.55 * 165},
		{"RAPL file range at 50 %", `{"rawModel": "AMD EPYC 9654 96-Core Processor", "sockets": 4, "capRange": {"type": "package", "minWattsPerSocket": 90, "maxWattsPerSocket": 165}}`, 50, 360},
		{"performance", `{"rawModel": "AMD EPYC 9654 96-Core Processor", "sockets": 2}`, 100, 720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hw := nodeHardwareFromJSON(t, `{"apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
				"metadata": {"name": "c1"}, "spec": {"nodeName": "c1"}, "status": {"cpu": `+tc.cpu+`}}`)
			nt := &v1alpha1.NodeTwin{
				ObjectMeta: metav1.ObjectMeta{Name: "c1"},
				Spec:       v1alpha1.NodeTwinSpec{NodeName: "c1", Profile: profileEco},
			}
			reader, _, dyn := fakeClients(t, hw, nt)
			if _, err := reconcileNodeTwin(context.Background(), reader, dyn, "c1", profileEco, tc.pct, 100, false, nil); err != nil {
				t.Fatalf("reconcileNodeTwin: %v", err)
			}
			u, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "c1", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get NodeTwin: %v", err)
			}
			var got v1alpha1.NodeTwin
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &got); err != nil {
				t.Fatalf("decode NodeTwin: %v", err)
			}
			pm := got.Status.PowerMeasurement
			if pm == nil {
				t.Fatal("no status.powerMeasurement")
			}
			if math.Abs(pm.CPUCappedPowerW-tc.want) > 1e-6 {
				t.Fatalf("cpuCappedPowerW = %v, want %v, the cap the agent applies (cpuTdpW %v)", pm.CPUCappedPowerW, tc.want, pm.CPUTdpW)
			}
		})
	}
}
