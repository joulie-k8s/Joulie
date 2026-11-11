package main

import (
	"context"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// capResults reads joulie_agent_cap_result of node's component, by result.
func capResults(node, component string) map[string]float64 {
	g, _ := capMetrics()
	out := map[string]float64{}
	for _, r := range joulie.CapResultValues {
		out[r] = testutil.ToFloat64(g.WithLabelValues(node, component, r))
	}
	return out
}

// statusWriteErrors reads joulie_agent_status_write_errors_total of node's
// object.
func statusWriteErrors(node, object string) float64 {
	_, c := capMetrics()
	return testutil.ToFloat64(c.WithLabelValues(node, object))
}

// The cap result the agent writes to status.controlStatus is exported as
// joulie_agent_cap_result, one series per result with the current one at 1,
// also when the status write is skipped because nothing changed: an alert on
// a cap that keeps failing must not depend on the write cadence. A node a
// pool agent stops controlling loses its series.
func TestCapResultGaugeFollowsTheControlStatus(t *testing.T) {
	const node = "cap-result-gauge"
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), nodeTwinObject(node, node))
	forgetWrites(node)
	t.Cleanup(func() { forgetNodeCapMetrics(node) })
	want := func(when, component, result string) {
		t.Helper()
		got := capResults(node, component)
		for _, r := range joulie.CapResultValues {
			v := 0.0
			if r == result {
				v = 1
			}
			if got[r] != v {
				t.Errorf("%s: joulie_agent_cap_result{component=%q,result=%q} = %v, want %v", when, component, r, got[r], v)
			}
		}
	}
	write := func(component, result string) {
		t.Helper()
		if err := updateNodeTwinControlStatus(context.Background(), dyn, node, component, "nvidia", result, "msg"); err != nil {
			t.Fatal(err)
		}
	}
	write("gpu", joulie.CapResultError)
	want("first write", "gpu", joulie.CapResultError)
	write("gpu", joulie.CapResultError)
	want("unchanged write", "gpu", joulie.CapResultError)
	write("gpu", joulie.CapResultApplied)
	want("applied", "gpu", joulie.CapResultApplied)
	write("cpu", joulie.CapResultBlocked)
	want("cpu blocked", "cpu", joulie.CapResultBlocked)
	want("gpu untouched by the cpu write", "gpu", joulie.CapResultApplied)

	forgetNodeCapMetrics(node)
	g, _ := capMetrics()
	if n := g.DeletePartialMatch(prometheus.Labels{"node": node}); n != 0 {
		t.Errorf("%d series of a forgotten node left", n)
	}
}

// A status write the API server refuses (RBAC, an admission policy) is
// counted in joulie_agent_status_write_errors_total for its object; a
// NodeTwin the controller manager has not created yet is not a failure.
func TestStatusWriteErrorsCountRefusedWrites(t *testing.T) {
	const node = "status-write-errors"
	forbidden := func(resource string) clienttesting.ReactionFunc {
		return func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "joulie.io", Resource: resource}, node, nil)
		}
	}
	t.Cleanup(func() { forgetNodeCapMetrics(node) })

	forgetWrites(node)
	missing := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if err := updateNodeTwinControlStatus(context.Background(), missing, node, "cpu", "rapl", joulie.CapResultApplied, "ok"); err != nil {
		t.Fatalf("NodeTwin not created yet: %v, want nil", err)
	}
	if got := statusWriteErrors(node, statusObjectNodeTwin); got != 0 {
		t.Errorf("NodeTwin not created yet: %v errors, want 0", got)
	}

	forgetWrites(node)
	refused := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeHardwareGVR: "NodeHardwareList"}, nodeTwinObject(node, node))
	refused.PrependReactor("patch", "nodetwins", forbidden("nodetwins"))
	refused.PrependReactor("*", "nodehardwares", forbidden("nodehardwares"))
	if err := updateNodeTwinControlStatus(context.Background(), refused, node, "cpu", "rapl", joulie.CapResultApplied, "ok"); err == nil {
		t.Fatal("refused NodeTwin patch returned no error")
	}
	if got := statusWriteErrors(node, statusObjectNodeTwin); got != 1 {
		t.Errorf("refused NodeTwin patch: %v errors, want 1", got)
	}
	writeNodeHardwareStatus(context.Background(), refused, node, HardwareInfo{})
	if got := statusWriteErrors(node, statusObjectNodeHardware); got != 1 {
		t.Errorf("refused NodeHardware write: %v errors, want 1", got)
	}
}

// A refused write of the status subresource is counted even when the
// fallback write of the object itself is accepted: the CRDs have a status
// subresource, so that write leaves the status as it was. This is the agent
// that lost its verbs on nodetwins/status or nodehardwares/status only.
func TestStatusWriteErrorsCountARefusedStatusSubresource(t *testing.T) {
	const node = "status-subresource-refused"
	refuseStatus := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		gr := schema.GroupResource{Group: "joulie.io", Resource: action.GetResource().Resource + "/status"}
		return true, nil, apierrors.NewForbidden(gr, node, nil)
	}
	t.Cleanup(func() { forgetNodeCapMetrics(node) })
	forgetWrites(node)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeHardwareGVR: "NodeHardwareList"}, nodeTwinObject(node, node))
	dyn.PrependReactor("patch", "nodetwins", refuseStatus)
	dyn.PrependReactor("update", "nodehardwares", refuseStatus)

	if err := updateNodeTwinControlStatus(context.Background(), dyn, node, "cpu", "rapl", joulie.CapResultApplied, "ok"); err == nil {
		t.Error("refused controlStatus patch returned no error")
	}
	if got := statusWriteErrors(node, statusObjectNodeTwin); got != 1 {
		t.Errorf("refused controlStatus patch: %v errors, want 1", got)
	}
	if err := upsertNodeHardwareStatus(context.Background(), dyn, node, HardwareInfo{}); err == nil {
		t.Error("refused NodeHardware status update returned no error")
	}
	writeNodeHardwareStatus(context.Background(), dyn, node, HardwareInfo{CPUModel: "changed"})
	if got := statusWriteErrors(node, statusObjectNodeHardware); got != 1 {
		t.Errorf("refused NodeHardware status update: %v errors, want 1", got)
	}
}
