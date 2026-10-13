package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/inputs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// resetNodePowerState forgets the form auto reads and the hints already
// logged, and restores nodePowerSpec after the test.
func resetNodePowerState(t *testing.T) {
	t.Helper()
	reset := func() {
		nodePowerAutoMu.Lock()
		nodePowerAutoForm = ""
		nodePowerAutoMu.Unlock()
		nodePowerHintsMu.Lock()
		nodePowerHintsSeen = map[string]struct{}{}
		nodePowerHintsMu.Unlock()
	}
	prev := nodePowerSpec
	reset()
	t.Cleanup(func() {
		nodePowerSpec = prev
		reset()
	})
}

// nodePowerQueries returns the default otel query in its labels and
// target_info forms.
func nodePowerQueries(t *testing.T, spec inputs.NodePowerSpec) (labelsForm, targetInfoForm string) {
	t.Helper()
	spec.ResourceAttributes = inputs.ResourceAttributesAuto
	primary, fallback, err := spec.Query()
	if err != nil {
		t.Fatal(err)
	}
	return primary.PromQL, fallback.PromQL
}

// resolveRound reads every node in one round, as one reconcile does.
func resolveRound(nodes ...string) map[string]float64 {
	ctx := withNodePowerRound(context.Background())
	out := map[string]float64{}
	for _, n := range nodes {
		w, source := resolveNodePower(ctx, nil, n, joulie.NodeHardware{})
		if source == joulie.PowerSourcePrometheus {
			out[n] = w
		}
	}
	return out
}

// The otel preset reads every node of a reconcile from one grouped query,
// and the twin of each node gets its own value.
func TestResolveNodePowerOTelPresetOneQueryPerReconcile(t *testing.T) {
	resetNodePowerState(t)
	labelsForm, _ := nodePowerQueries(t, inputs.NodePowerSpec{})
	prom := newRegProm(t, func(q string) (int, string) {
		if q != labelsForm {
			return 200, regVector()
		}
		return 200, regVector(
			regSeries(`{"k8s_node_name":"n0"}`, "350"),
			regSeries(`{"k8s_node_name":"n1"}`, "1050"),
			regSeries(`{"k8s_node_name":"n2"}`, "598.5"),
		)
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, "")

	reader, client, dyn := fakeClients(t, managedNode("n0", ""), managedNode("n1", ""), managedNode("n2", ""))
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := reconcile(
			context.Background(), reader, client, dyn, selector,
			"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
			5000, 120, "static_partition", 0.5, 0.5, 1, 5, 10,
			100, 60, false, 100, 60, false,
			map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
		); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if got := prom.seen(); len(got) != i {
			t.Fatalf("after reconcile %d Prometheus got %d queries, want %d (one per reconcile): %q", i, len(got), i, got)
		}
	}

	want := map[string]float64{"n0": 350, "n1": 1050, "n2": 598.5}
	for node, w := range want {
		got, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), node, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		status, _ := got.Object["status"].(map[string]interface{})
		pm, _ := status["powerMeasurement"].(map[string]interface{})
		if regNumber(pm["measuredNodePowerW"]) != w || pm["source"] != joulie.PowerSourcePrometheus {
			t.Errorf("NodeTwin %s powerMeasurement %v, want %v W from prometheus", node, pm, w)
		}
	}
}

// Under auto, data series without the node label (Prometheus does not
// promote k8s.node.name) send the reader to target_info, even when another
// series carries the label: two queries on the first reconcile, then only
// target_info while it works, and back to the labels form when target_info
// stops answering.
func TestResolveNodePowerAutoFallsBackToTargetInfo(t *testing.T) {
	resetNodePowerState(t)
	labelsForm, targetInfoForm := nodePowerQueries(t, inputs.NodePowerSpec{})
	var mu sync.Mutex
	promoted := false
	prom := newRegProm(t, func(q string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case q == labelsForm && promoted:
			return 200, regVector(regSeries(`{"k8s_node_name":"n0"}`, "310"), regSeries(`{"k8s_node_name":"n1"}`, "60"))
		case q == labelsForm:
			return 200, regVector(regSeries(`{"k8s_node_name":"n0"}`, "350"), regSeries(`{}`, "50"))
		case q == targetInfoForm && !promoted:
			return 200, regVector(regSeries(`{"k8s_node_name":"n0"}`, "300"), regSeries(`{"k8s_node_name":"n1"}`, "50"))
		}
		return 200, regVector()
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, "")
	buf := captureLog(t)

	if got := resolveRound("n0", "n1"); got["n0"] != 300 || got["n1"] != 50 {
		t.Fatalf("first reconcile read %v, want n0=300 n1=50 from target_info", got)
	}
	if q := prom.seen(); len(q) != 2 || q[0] != labelsForm || q[1] != targetInfoForm {
		t.Fatalf("first reconcile queries %q, want the labels form then the target_info form", q)
	}
	if got := resolveRound("n0", "n1"); got["n0"] != 300 {
		t.Fatalf("second reconcile read %v, want n0=300", got)
	}
	if q := prom.seen(); len(q) != 3 || q[2] != targetInfoForm {
		t.Fatalf("second reconcile queries %q, want the target_info form alone", q[2:])
	}
	if n := strings.Count(buf.String(), "reads the node from target_info"); n != 1 {
		t.Fatalf("the switch to target_info logged %d times, want once: %q", n, buf.String())
	}

	mu.Lock()
	promoted = true
	mu.Unlock()
	if got := resolveRound("n0", "n1"); got["n0"] != 310 || got["n1"] != 60 {
		t.Fatalf("after promotion read %v, want n0=310 n1=60 from the labels form", got)
	}
	if q := prom.seen(); len(q) != 5 || q[3] != targetInfoForm || q[4] != labelsForm {
		t.Fatalf("after promotion queries %q, want target_info then the labels form", q[3:])
	}
	if !strings.Contains(buf.String(), "reads the node label on the data series") {
		t.Fatalf("the switch back to the labels form was not logged: %q", buf.String())
	}
}

// The labels form whose series lack the node label names the Prometheus
// setting that adds it, once; auto names it too after both forms failed,
// and a target_info join that two nodes share names service.instance.id.
func TestResolveNodePowerHintsWhenNodeLabelMissing(t *testing.T) {
	resetNodePowerState(t)
	prom := newRegProm(t, func(q string) (int, string) {
		return 200, regVector(regSeries(`{}`, "597.69"))
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, "")
	buf := captureLog(t)

	nodePowerSpec = inputs.NodePowerSpec{ResourceAttributes: inputs.ResourceAttributesLabels}
	for i := 0; i < 2; i++ {
		if w, source := resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{}); source != joulie.PowerSourcePrometheusNoData {
			t.Fatalf("resolveNodePower = %v (%s), want prometheus-no-data", w, source)
		}
	}
	out := buf.String()
	for _, want := range []string{"lack the label k8s_node_name", "k8s.node.name", "otlp.promote_resource_attributes", "PROMETHEUS_RESOURCE_ATTRIBUTES"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hint %q missing from %q", want, out)
		}
	}
	if n := strings.Count(out, "[twin] hint:"); n != 1 {
		t.Fatalf("%d hints after two reads of the same failure, want 1: %q", n, out)
	}

	// auto: both forms lack the node.
	resetNodePowerState(t)
	buf.Reset()
	nodePowerSpec = inputs.NodePowerSpec{}
	resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{})
	out = buf.String()
	if !strings.Contains(out, "otlp.promote_resource_attributes") || !strings.Contains(out, "found no k8s_node_name on target_info") {
		t.Fatalf("auto hints %q, want the promotion hint and the target_info hint", out)
	}

	// target_info: Prometheus refuses a join two nodes share.
	buf.Reset()
	join := newRegProm(t, func(string) (int, string) {
		return 422, `{"status":"error","errorType":"execution","error":"found duplicate series for the match group {instance=\"10.0.0.7:9464\", job=\"joulie/joulie-simulator\"} on the right hand-side of the operation"}`
	})
	regWithNodePowerVars(t, "prometheus", "", join.URL, "")
	nodePowerSpec = inputs.NodePowerSpec{ResourceAttributes: inputs.ResourceAttributesTargetInfo}
	resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{})
	if out := buf.String(); !strings.Contains(out, "service.instance.id") {
		t.Fatalf("join failure hint %q, want it to name service.instance.id", out)
	}
}

// The Kepler presets name each other when theirs finds no series, and the
// node label variable when the series lack the node.
func TestResolveNodePowerHintsKeplerPreset(t *testing.T) {
	resetNodePowerState(t)
	var mu sync.Mutex
	answer := regVector()
	prom := newRegProm(t, func(string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return 200, answer
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, "")
	buf := captureLog(t)

	nodePowerSpec = inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKepler}
	resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{})
	if out := buf.String(); !strings.Contains(out, "NODE_POWER_PRESET=kepler-legacy") {
		t.Fatalf("kepler without data: hint %q, want it to point at kepler-legacy", out)
	}
	buf.Reset()
	nodePowerSpec = inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKeplerLegacy}
	resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{})
	if out := buf.String(); !strings.Contains(out, "is read with NODE_POWER_PRESET=kepler\n") {
		t.Fatalf("kepler-legacy without data: hint %q, want it to point at kepler", out)
	}

	// Scraped with honor_labels: true, Kepler 0.7 keeps instance and
	// Prometheus adds no exported_instance.
	buf.Reset()
	mu.Lock()
	answer = regVector(regSeries(`{}`, "450"))
	mu.Unlock()
	resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{})
	out := buf.String()
	if !strings.Contains(out, "lack the label exported_instance") || !strings.Contains(out, "NODE_POWER_NODE_LABEL") || !strings.Contains(out, "honor_labels") {
		t.Fatalf("kepler-legacy without the node label: hint %q, want exported_instance, NODE_POWER_NODE_LABEL and honor_labels", out)
	}
}

// A custom query without {node} is summed by the node label; series
// without it get a hint naming NODE_POWER_NODE_LABEL and {node}.
func TestResolveNodePowerHintsCustomGroupedQuery(t *testing.T) {
	resetNodePowerState(t)
	prom := newRegProm(t, func(string) (int, string) {
		return 200, regVector(regSeries(`{}`, "900"))
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, `rate(node_power_joules_total[5m])`)
	buf := captureLog(t)

	if _, source := resolveNodePower(context.Background(), nil, "n0", joulie.NodeHardware{}); source != joulie.PowerSourcePrometheusNoData {
		t.Fatalf("source %s, want prometheus-no-data", source)
	}
	if q := prom.seen(); len(q) != 1 || q[0] != `sum by (k8s_node_name) (rate(node_power_joules_total[5m]))` {
		t.Fatalf("queries %q, want the custom query summed by k8s_node_name", q)
	}
	out := buf.String()
	for _, want := range []string{"custom query is summed by the label k8s_node_name", "NODE_POWER_NODE_LABEL", "{node}"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hint %q missing from %q", want, out)
		}
	}
}
