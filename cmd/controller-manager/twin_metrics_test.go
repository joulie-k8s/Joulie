package main

import (
	"context"
	"errors"
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// reconcileManagedNodes runs one reconcile over the managed nodes named,
// with setup applied to the dynamic client before it.
func reconcileManagedNodes(t *testing.T, setup func(*dynamicfake.FakeDynamicClient), names ...string) {
	t.Helper()
	var objs []runtime.Object
	for _, n := range names {
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{"joulie.io/managed": "true"}}})
	}
	reader, client, dyn := fakeClients(t, objs...)
	if setup != nil {
		setup(dyn)
	}
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileWithCatalogAndFacility(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, "static_partition", 0.5, 0.5, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"}, loadHardwareCatalog(),
		&facilityMetrics{},
	); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// joulie_twin_nodes_power_source counts the managed nodes per measured power
// source of the reconcile, with every source of pkg/api set, 0 included: a
// prometheus source with no address gives every node prometheus-no-data, and
// a reconcile with no managed node sets every source to 0.
func TestPowerSourceGaugeCountsTheManagedNodes(t *testing.T) {
	regWithNodePowerVars(t, nodePowerSourcePrometheus, "", "", "")
	want := func(when string, noData float64) {
		t.Helper()
		for _, s := range joulie.PowerSourceValues {
			v := 0.0
			if s == joulie.PowerSourcePrometheusNoData {
				v = noData
			}
			if got := testutil.ToFloat64(twinNodesPowerSource.WithLabelValues(s)); got != v {
				t.Errorf("%s: joulie_twin_nodes_power_source{source=%q} = %v, want %v", when, s, got, v)
			}
		}
		if n := testutil.CollectAndCount(twinNodesPowerSource); n != len(joulie.PowerSourceValues) {
			t.Errorf("%s: %d series, want one per power source (%d)", when, n, len(joulie.PowerSourceValues))
		}
	}
	reconcileManagedNodes(t, nil, "ps-a", "ps-b")
	want("two nodes without a reading", 2)
	reconcileManagedNodes(t, nil)
	want("no managed node", 0)
}

// joulie_twin_status_updated_timestamp_seconds holds, per planned node, the
// lastUpdated of the NodeTwin status last written. A node whose status
// write the API server refuses keeps the time it was first planned, so it
// ages from there instead of having no series to alert on; a node that
// leaves the plan loses its series.
func TestTwinStatusTimestampAgesWhenTheWriteFails(t *testing.T) {
	t.Cleanup(func() { twinStatusFreshness.plan(nil, time.Now()) })
	twinStatusFreshness.plan(nil, time.Now())
	refuseStuck := func(dyn *dynamicfake.FakeDynamicClient) {
		dyn.PrependReactor("patch", "nodetwins", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.(clienttesting.PatchAction).GetName() == joulie.ObjectNameForNode("ts-stuck") {
				return true, nil, errors.New("admission webhook denied the request")
			}
			return false, nil, nil
		})
	}
	stamp := func(node string) float64 {
		return testutil.ToFloat64(twinStatusUpdated.WithLabelValues(node))
	}

	firstPlanned := time.Now().Add(-time.Hour)
	twinStatusFreshness.plan([]string{"ts-stuck"}, firstPlanned)
	before := time.Now().Unix()
	reconcileManagedNodes(t, refuseStuck, "ts-ok", "ts-stuck")
	if got := stamp("ts-ok"); got < float64(before) || got > float64(time.Now().Unix()) {
		t.Errorf("ts-ok: %v, want the lastUpdated just written (from %d)", got, before)
	}
	if got := stamp("ts-stuck"); got != float64(firstPlanned.Unix()) {
		t.Errorf("ts-stuck: %v, want the time it was first planned, %d", got, firstPlanned.Unix())
	}

	reconcileManagedNodes(t, nil, "ts-ok")
	if n := testutil.CollectAndCount(twinStatusUpdated); n != 1 {
		t.Errorf("%d series after ts-stuck left the plan, want only ts-ok", n)
	}
}
