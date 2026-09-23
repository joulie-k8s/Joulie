//go:build envtest

package envtest_test

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
)

// newNodeTwin creates a minimal NodeTwin through the typed client and
// registers its deletion.
func newNodeTwin(t *testing.T, c client.Client, name string) *v1alpha1.NodeTwin {
	t.Helper()
	ctx := testContext(t)
	twin := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NodeTwinSpec{
			NodeName: name,
			Profile:  "eco",
		},
	}
	if err := c.Create(ctx, twin); err != nil {
		t.Fatalf("create NodeTwin %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(testContext(t), twin)
	})
	return twin
}

// TestNodeTwinStatusAcceptsEveryPowerSourceValue proves the CRD enum and
// pkg/api agree where it matters: at the API server. The unit test in
// tests/contracts compares two lists of strings; this one writes each value
// and lets the schema reject it. A drifted enum once made every status write
// fail in production while every unit test stayed green.
func TestNodeTwinStatusAcceptsEveryPowerSourceValue(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)

	if len(joulie.PowerSourceValues) == 0 {
		t.Fatal("pkg/api.PowerSourceValues is empty, nothing would be proven")
	}

	for _, source := range joulie.PowerSourceValues {
		t.Run(source, func(t *testing.T) {
			twin := newNodeTwin(t, c, objectName(t, ""))
			twin.Status = v1alpha1.NodeTwinStatus{
				SchedulableClass: "eco",
				PowerMeasurement: &v1alpha1.PowerMeasurement{
					Source:             source,
					MeasuredNodePowerW: 123.5,
				},
			}
			if err := c.Status().Update(ctx, twin); err != nil {
				t.Fatalf("API server rejected powerMeasurement.source %q, which pkg/api says a component may write: %v", source, err)
			}

			var got v1alpha1.NodeTwin
			if err := c.Get(ctx, types.NamespacedName{Name: twin.Name}, &got); err != nil {
				t.Fatalf("read back NodeTwin: %v", err)
			}
			if got.Status.PowerMeasurement == nil || got.Status.PowerMeasurement.Source != source {
				t.Fatalf("source did not survive the write: got %+v, want %q", got.Status.PowerMeasurement, source)
			}
		})
	}
}

// TestNodeTwinStatusRejectsUnknownPowerSource is the other half of the
// previous test: it shows the enum is enforced, so the acceptance above is
// evidence and not an unvalidated field silently swallowing anything.
func TestNodeTwinStatusRejectsUnknownPowerSource(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)

	twin := newNodeTwin(t, c, objectName(t, ""))
	twin.Status = v1alpha1.NodeTwinStatus{
		PowerMeasurement: &v1alpha1.PowerMeasurement{Source: "not-a-power-source"},
	}

	err := c.Status().Update(ctx, twin)
	if err == nil {
		t.Fatal("API server accepted an unknown powerMeasurement.source, so the enum is not enforced and the acceptance test above proves nothing")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error from schema validation, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "powerMeasurement.source") {
		t.Fatalf("rejection did not name the offending field: %v", err)
	}
}

// TestNodeTwinSpecRejectsUnknownProfile covers the second enum on the object,
// spec.profile, which the controller manager writes on every policy decision.
func TestNodeTwinSpecRejectsUnknownProfile(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)

	twin := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: objectName(t, "")},
		Spec: v1alpha1.NodeTwinSpec{
			NodeName: "worker-1",
			Profile:  "turbo",
		},
	}
	err := c.Create(ctx, twin)
	if err == nil {
		t.Cleanup(func() { _ = c.Delete(testContext(t), twin) })
		t.Fatal("API server accepted spec.profile=turbo, which is not in the enum")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got %T: %v", err, err)
	}
}

// TestNodeHardwareAcceptsTheStatusTheAgentWrites builds the same map the
// agent builds in upsertNodeHardwareStatus (cmd/agent/main.go) and sends it
// as-is. The agent writes unstructured maps, so this is the payload the API
// server actually sees on a cluster: a field the agent emits but the schema
// does not know (or types differently) fails here.
func TestNodeHardwareAcceptsTheStatusTheAgentWrites(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)

	name := objectName(t, "")
	hw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeHardware",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"nodeName": name,
		},
	}}
	if err := c.Create(ctx, hw); err != nil {
		t.Fatalf("create NodeHardware: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(testContext(t), hw) })

	// Shape copied from upsertNodeHardwareStatus in cmd/agent/main.go,
	// including the int64 conversions and the optional capRange blocks.
	status := map[string]any{
		"cpu": map[string]any{
			"rawModel":           "Intel(R) Xeon(R) Gold 6248 CPU @ 2.50GHz",
			"model":              "intel-xeon-gold-6248",
			"vendor":             "intel",
			"sockets":            int64(2),
			"totalCores":         int64(40),
			"coresPerSocket":     int64(20),
			"driverFamily":       "intel_rapl",
			"controlAvailable":   true,
			"telemetryAvailable": true,
			"warnings":           []any{"cpu model not recognized"},
			"capRange": map[string]any{
				"type":              "package",
				"minWattsPerSocket": 50.0,
				"maxWattsPerSocket": 150.0,
			},
			"landmarks": map[string]any{
				"minFreqMHz":             1000.0,
				"nominalFreqMHz":         2500.0,
				"maxBoostMHz":            3900.0,
				"lowestNonlinearFreqMHz": 1200.0,
			},
		},
		"gpu": map[string]any{
			"present":            true,
			"rawModel":           "NVIDIA A100-SXM4-40GB",
			"model":              "nvidia-a100-40gb",
			"vendor":             "nvidia",
			"count":              int64(4),
			"currentCapWatts":    400.0,
			"controlAvailable":   true,
			"telemetryAvailable": true,
			"warnings":           []any{"gpu model not recognized"},
			"capRangePerGpu": map[string]any{
				"minWatts":     100.0,
				"maxWatts":     400.0,
				"defaultWatts": 400.0,
			},
			"slicing": map[string]any{
				"supported": true,
				"modes":     []any{"mig", "timeslicing"},
			},
		},
		"memory": map[string]any{
			"totalBytes": int64(541165879296),
		},
		"network": map[string]any{
			"linkClass": "100g",
		},
		"capabilities": map[string]any{
			"cpuControl":   true,
			"gpuControl":   true,
			"cpuTelemetry": true,
			"gpuTelemetry": true,
		},
		"quality": map[string]any{
			"overall":  "full",
			"warnings": []any{},
		},
		"inventoryResolution": map[string]any{
			"hardwareCatalogKey": map[string]any{
				"cpu": "intel-xeon-gold-6248",
				"gpu": "nvidia-a100-40gb",
			},
			"exactness": map[string]any{
				"cpu": "exact",
				"gpu": "exact",
			},
		},
		"updatedAt": "2026-09-23T10:00:00Z",
	}
	if err := unstructured.SetNestedField(hw.Object, status, "status"); err != nil {
		t.Fatalf("set NodeHardware status: %v", err)
	}
	if err := c.Status().Update(ctx, hw); err != nil {
		t.Fatalf("API server rejected the status the agent writes: %v", err)
	}

	// Read it back through the typed client: this catches a field the
	// schema accepted but pruned, which an unstructured read-back would
	// not notice.
	var got v1alpha1.NodeHardware
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("read back NodeHardware: %v", err)
	}
	if got.Status.CPU == nil || got.Status.CPU.Sockets != 2 || got.Status.CPU.CapRange == nil {
		t.Fatalf("cpu block did not survive: %+v", got.Status.CPU)
	}
	if got.Status.GPU == nil || got.Status.GPU.Count != 4 || got.Status.GPU.CapRangePerGPU == nil {
		t.Fatalf("gpu block did not survive: %+v", got.Status.GPU)
	}
	if got.Status.Capabilities == nil || !got.Status.Capabilities.CPUControl {
		t.Fatalf("capabilities block did not survive: %+v", got.Status.Capabilities)
	}
	if got.Status.InventoryResolution == nil || got.Status.InventoryResolution.Exactness == nil {
		t.Fatalf("inventoryResolution block did not survive: %+v", got.Status.InventoryResolution)
	}
	if got.Status.Memory == nil || got.Status.Memory.TotalBytes != 541165879296 {
		t.Fatalf("memory block did not survive: %+v", got.Status.Memory)
	}
}

// TestWholeNumberWattsSurviveTheTypedRoundTrip pins the reason the components
// read CRs through api/v1alpha1 and not through unstructured: the API server
// stores a whole-number float as an int64, so an `obj[...].(float64)`
// assertion returns 0 and drops the value without an error. The typed client
// decodes it correctly; the unstructured assertion below shows what it would
// have cost.
func TestWholeNumberWattsSurviveTheTypedRoundTrip(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)

	name := objectName(t, "")
	twin := newNodeTwin(t, c, name)
	twin.Status = v1alpha1.NodeTwinStatus{
		SchedulableClass: "performance",
		PowerMeasurement: &v1alpha1.PowerMeasurement{
			Source:             joulie.PowerSourcePrometheus,
			MeasuredNodePowerW: 250,
			CPUTdpW:            300,
			GPUTdpW:            1600,
			NodeTdpW:           1900,
		},
		PredictedPowerHeadroomScore: 42,
	}
	if err := c.Status().Update(ctx, twin); err != nil {
		t.Fatalf("write whole-number watts: %v", err)
	}

	var got v1alpha1.NodeTwin
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("read back NodeTwin: %v", err)
	}
	pm := got.Status.PowerMeasurement
	if pm == nil {
		t.Fatal("powerMeasurement is missing after the write")
	}
	for _, tc := range []struct {
		field string
		got   float64
		want  float64
	}{
		{"measuredNodePowerW", pm.MeasuredNodePowerW, 250},
		{"cpuTdpW", pm.CPUTdpW, 300},
		{"gpuTdpW", pm.GPUTdpW, 1600},
		{"nodeTdpW", pm.NodeTdpW, 1900},
		{"predictedPowerHeadroomScore", got.Status.PredictedPowerHeadroomScore, 42},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s read back as %v, want %v", tc.field, tc.got, tc.want)
		}
	}

	// The same object read as unstructured: the whole numbers come back as
	// int64, which is why a float64 type assertion on an unstructured read
	// silently loses them.
	raw := &unstructured.Unstructured{}
	raw.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("NodeTwin"))
	if err := c.Get(ctx, types.NamespacedName{Name: name}, raw); err != nil {
		t.Fatalf("read back NodeTwin as unstructured: %v", err)
	}
	stored, found, err := unstructured.NestedFieldNoCopy(raw.Object, "status", "powerMeasurement", "measuredNodePowerW")
	if err != nil || !found {
		t.Fatalf("measuredNodePowerW missing from the unstructured object: found=%v err=%v", found, err)
	}
	if _, isFloat := stored.(float64); isFloat {
		t.Fatalf("expected the API server to return the whole number as int64, got float64 %v; "+
			"if this ever changes, revisit the rule that CRs are read through api/v1alpha1", stored)
	}
	if asInt, ok := stored.(int64); !ok || asInt != 250 {
		t.Fatalf("measuredNodePowerW came back as %T(%v), want int64(250)", stored, stored)
	}
}
