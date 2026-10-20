package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// The agent and the controller manager write these objects as unstructured maps, so the
// JSON names of the Go types are the contract with them. The round trip
// through the unstructured converter checks that every field survives, and
// the path checks pin the names the writers in cmd/ emit today.

func ptr(v float64) *float64 { return &v }

func TestNodeTwinRoundTrip(t *testing.T) {
	in := &NodeTwin{
		TypeMeta:   metav1.TypeMeta{APIVersion: "joulie.io/v1alpha1", Kind: "NodeTwin"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: NodeTwinSpec{
			NodeName: "worker-1",
			Profile:  "eco",
			CPU: &NodeTwinCPU{
				PackagePowerCapWatts:    ptr(150),
				PackagePowerCapPctOfMax: ptr(60),
			},
			GPU: &NodeTwinGPU{PowerCap: &GPUPowerCap{
				Scope:          "perGpu",
				CapWattsPerGPU: ptr(300),
				CapPctOfMax:    ptr(75),
			}},
			Policy:     &NodeTwinPolicy{Name: "queue_aware_v1"},
			Scheduling: &NodeTwinScheduling{Draining: true},
		},
		Status: NodeTwinStatus{
			SchedulableClass: "eco",
			PowerMeasurement: &PowerMeasurement{
				Source:             "prometheus",
				MeasuredNodePowerW: 412.5,
				CPUCappedPowerW:    180,
				GPUCappedPowerW:    600,
				NodeCappedPowerW:   780,
				CPUTdpW:            300,
				GPUTdpW:            800,
				NodeTdpW:           1100,
				PowerTrendWPerMin:  -3.2,
			},
			PredictedPowerHeadroomScore: 47.1,
			PredictedCoolingStressScore: 20.5,
			PredictedPsuStressScore:     10,
			EffectiveCapState:           &CapState{CPUPct: 60, GPUPct: 75},
			EstimatedPUE:                1.12,
			ControlStatus: &ControlStatus{
				CPU: &ControlResult{Backend: "rapl", Result: "applied", Message: "ok", UpdatedAt: "2026-01-01T00:00:00Z"},
				GPU: &ControlResult{Backend: "nvml", Result: "failed", Message: "no device", UpdatedAt: "2026-01-01T00:00:01Z"},
			},
			LastUpdated: "2026-01-01T00:00:02Z",
			Conditions: []metav1.Condition{{
				Type: "AgentReporting", Status: metav1.ConditionFalse, Reason: "NoNodeHardware", Message: "no agent",
				// metav1.Time decodes in the local zone.
				LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 3, 0, time.UTC).Local()),
			}},
		},
	}
	u := roundTrip(t, in, &NodeTwin{})
	for _, p := range []string{
		"spec.nodeName", "spec.profile",
		"spec.cpu.packagePowerCapWatts", "spec.cpu.packagePowerCapPctOfMax",
		"spec.gpu.powerCap.scope", "spec.gpu.powerCap.capWattsPerGpu", "spec.gpu.powerCap.capPctOfMax",
		"spec.policy.name", "spec.scheduling.draining",
		"status.schedulableClass", "status.powerMeasurement.source", "status.powerMeasurement.powerTrendWPerMin",
		"status.predictedPowerHeadroomScore", "status.predictedCoolingStressScore", "status.predictedPsuStressScore",
		"status.effectiveCapState.cpuPct", "status.effectiveCapState.gpuPct",
		"status.estimatedPUE",
		"status.controlStatus.cpu.backend", "status.controlStatus.gpu.updatedAt", "status.lastUpdated",
		"status.conditions",
	} {
		mustHavePath(t, u, p)
	}
}

func TestNodeHardwareRoundTrip(t *testing.T) {
	in := &NodeHardware{
		TypeMeta:   metav1.TypeMeta{APIVersion: "joulie.io/v1alpha1", Kind: "NodeHardware"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       NodeHardwareSpec{NodeName: "worker-1"},
		Status: NodeHardwareStatus{
			CPU: &NodeHardwareCPU{
				RawModel:           "Intel(R) Xeon(R) Platinum 8480+",
				Model:              "xeon-8480plus",
				Vendor:             "intel",
				Sockets:            2,
				TotalCores:         112,
				CoresPerSocket:     56,
				DriverFamily:       "intel_rapl",
				CapRange:           &CPUCapRange{Type: "package", MinWattsPerSocket: 100, MaxWattsPerSocket: 350},
				Landmarks:          &CPULandmarks{MinFreqMHz: 800, NominalFreqMHz: 2000, MaxBoostMHz: 3800, LowestNonlinearFreqMHz: 1200},
				ControlAvailable:   true,
				TelemetryAvailable: true,
				Warnings:           []string{"cpu model not recognized"},
			},
			GPU: &NodeHardwareGPU{
				Present:            true,
				RawModel:           "NVIDIA H100 80GB HBM3",
				Model:              "h100-sxm",
				Vendor:             "nvidia",
				Count:              4,
				CapRangePerGPU:     &GPUCapRange{MinWatts: 200, MaxWatts: 700, DefaultWatts: 700},
				CurrentCapWatts:    700,
				Slicing:            &GPUSlicing{Supported: true, Modes: []string{"mig", "mps"}},
				ControlAvailable:   true,
				TelemetryAvailable: true,
				Warnings:           []string{"gpu model not recognized"},
			},
			Memory:  &NodeHardwareMemory{TotalBytes: 2 << 40},
			Network: &NodeHardwareNetwork{LinkClass: "100G"},
			InventoryResolution: &InventoryResolution{
				HardwareCatalogKey: &CatalogKey{CPU: "xeon-8480plus", GPU: "h100-sxm"},
				Exactness:          &CatalogExactness{CPU: "exact", GPU: "proxy"},
			},
			Capabilities: &NodeHardwareCapabilities{CPUControl: true, GPUControl: true, CPUTelemetry: true, GPUTelemetry: true},
			Quality:      &NodeHardwareQuality{Overall: "full", Warnings: []string{"none"}},
			UpdatedAt:    "2026-01-01T00:00:00Z",
		},
	}
	u := roundTrip(t, in, &NodeHardware{})
	for _, p := range []string{
		"spec.nodeName",
		"status.cpu.rawModel", "status.cpu.sockets", "status.cpu.coresPerSocket", "status.cpu.driverFamily",
		"status.cpu.capRange.type", "status.cpu.capRange.minWattsPerSocket", "status.cpu.capRange.maxWattsPerSocket",
		"status.cpu.landmarks.lowestNonlinearFreqMHz", "status.cpu.controlAvailable", "status.cpu.telemetryAvailable", "status.cpu.warnings",
		"status.gpu.present", "status.gpu.count", "status.gpu.capRangePerGpu.defaultWatts", "status.gpu.currentCapWatts",
		"status.gpu.slicing.modes", "status.gpu.warnings",
		"status.memory.totalBytes", "status.network.linkClass",
		"status.inventoryResolution.hardwareCatalogKey.cpu", "status.inventoryResolution.exactness.gpu",
		"status.capabilities.cpuControl", "status.capabilities.gpuTelemetry",
		"status.quality.overall", "status.quality.warnings", "status.updatedAt",
	} {
		mustHavePath(t, u, p)
	}
}

// GV01: the governor fields of the spec, of controlStatus and of
// NodeHardware survive the unstructured round trip under the JSON names the
// controller manager and the agent will write, and a ceiling the API server
// returns as a whole number (an int64 in the unstructured map, a bare 75 in
// JSON) reads back through the typed struct.
func TestGV01GovernorFieldsRoundTrip(t *testing.T) {
	twin := &NodeTwin{
		TypeMeta:   metav1.TypeMeta{APIVersion: "joulie.io/v1alpha1", Kind: "NodeTwin"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: NodeTwinSpec{
			NodeName: "worker-1",
			Profile:  "eco",
			CPU:      &NodeTwinCPU{Governor: &CPUGovernorIntent{Preset: "efficiency", Boost: "off"}},
			GPU:      &NodeTwinGPU{Governor: &GPUGovernorIntent{Preset: "efficient", ComputeClockCeilingPctOfMax: ptr(75.5)}},
		},
		Status: NodeTwinStatus{ControlStatus: &ControlStatus{
			CPUGovernor: &ControlResult{Backend: "cpufreq", Result: "equivalent", Message: "no EPP", Applied: "conservative", UpdatedAt: "2026-01-01T00:00:00Z"},
			GPUGovernor: &ControlResult{Backend: "amdgpu-sysfs", Result: "applied", Applied: "manual", UpdatedAt: "2026-01-01T00:00:01Z"},
		}},
	}
	u := roundTrip(t, twin, &NodeTwin{})
	for _, p := range []string{
		"spec.cpu.governor.preset", "spec.cpu.governor.boost",
		"spec.gpu.governor.preset", "spec.gpu.governor.computeClockCeilingPctOfMax",
		"status.controlStatus.cpuGovernor.backend", "status.controlStatus.cpuGovernor.result",
		"status.controlStatus.cpuGovernor.message", "status.controlStatus.cpuGovernor.applied",
		"status.controlStatus.cpuGovernor.updatedAt", "status.controlStatus.gpuGovernor.applied",
	} {
		mustHavePath(t, u, p)
	}

	hw := &NodeHardware{
		TypeMeta:   metav1.TypeMeta{APIVersion: "joulie.io/v1alpha1", Kind: "NodeHardware"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       NodeHardwareSpec{NodeName: "worker-1"},
		Status: NodeHardwareStatus{
			CPU: &NodeHardwareCPU{Governor: &CPUGovernorInventory{
				Driver:     "intel_pstate",
				DriverMode: "active",
				Current:    "powersave",
				Available:  []string{"performance", "powersave"},
				EPP:        &CPUEPPInventory{Current: "balance_performance", Available: []string{"default", "performance", "balance_performance", "balance_power", "power"}},
				Boost:      &CPUBoostInventory{State: "on", Scope: "no-turbo"},
				Presets:    []GovernorPresetSupport{{Preset: "performance", Result: "applied", Native: "performance"}, {Preset: "balanced", Result: "equivalent", Native: "powersave"}},
				Baseline: &CPUGovernorBaseline{
					Groups:     []CPUGovernorState{{Policies: "0-63", Governor: "powersave", EPP: "balance_performance", Boost: "on"}},
					RecordedAt: "2026-01-01T00:00:00Z",
				},
				Writable: true,
			}},
			GPU: &NodeHardwareGPU{Governor: &GPUGovernorInventory{
				Mechanism: "amdgpu-sysfs",
				Current:   "auto",
				Presets:   []GovernorPresetSupport{{Preset: "auto", Result: "equivalent", Native: "auto"}, {Preset: "sustained", Result: "applied", Native: "high"}},
				ClockRangesMHz: &GPUClockRanges{
					Graphics: &ClockRangeMHz{Min: 100, Max: 2000},
					Memory:   &ClockRangeMHz{Min: 100, Max: 1000},
					Fabric:   &ClockRangeMHz{Min: 400, Max: 1600},
				},
				Baseline: &GPUGovernorBaseline{
					Devices:    []GPUGovernorDeviceState{{BusID: "0000:c1:00.0", Level: "auto"}},
					RecordedAt: "2026-01-01T00:00:00Z",
				},
				Writable: true,
				Uniform:  true,
			}},
		},
	}
	u = roundTrip(t, hw, &NodeHardware{})
	for _, p := range []string{
		"status.cpu.governor.driver", "status.cpu.governor.driverMode", "status.cpu.governor.current",
		"status.cpu.governor.available", "status.cpu.governor.epp.current", "status.cpu.governor.epp.available",
		"status.cpu.governor.boost.state", "status.cpu.governor.boost.scope", "status.cpu.governor.presets",
		"status.cpu.governor.baseline.groups", "status.cpu.governor.baseline.recordedAt", "status.cpu.governor.writable",
		"status.gpu.governor.mechanism", "status.gpu.governor.current", "status.gpu.governor.presets",
		"status.gpu.governor.clockRangesMHz.graphics.min", "status.gpu.governor.clockRangesMHz.memory.max",
		"status.gpu.governor.clockRangesMHz.fabric.max", "status.gpu.governor.baseline.devices",
		"status.gpu.governor.baseline.recordedAt", "status.gpu.governor.writable", "status.gpu.governor.uniform",
	} {
		mustHavePath(t, u, p)
	}

	// writable and uniform are written even when false, so a merge patch
	// that turns them off replaces the stored true.
	off, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&NodeHardware{Status: NodeHardwareStatus{
		CPU: &NodeHardwareCPU{Governor: &CPUGovernorInventory{Driver: "acpi-cpufreq"}},
		GPU: &NodeHardwareGPU{Governor: &GPUGovernorInventory{Mechanism: "nvidia-smi"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"status.cpu.governor.writable", "status.gpu.governor.writable", "status.gpu.governor.uniform"} {
		mustHavePath(t, off, p)
	}

	// A whole-number ceiling arrives as int64 from the API server.
	whole := map[string]any{
		"apiVersion": "joulie.io/v1alpha1", "kind": "NodeTwin",
		"metadata": map[string]any{"name": "worker-1"},
		"spec": map[string]any{
			"nodeName": "worker-1", "profile": "eco",
			"gpu": map[string]any{"governor": map[string]any{"preset": "efficient", "computeClockCeilingPctOfMax": int64(75)}},
		},
	}
	var got NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(whole, &got); err != nil {
		t.Fatalf("FromUnstructured with an int64 ceiling: %v", err)
	}
	if g := got.Spec.GPU.Governor; g == nil || g.ComputeClockCeilingPctOfMax == nil || *g.ComputeClockCeilingPctOfMax != 75 {
		t.Fatalf("int64 ceiling read back as %+v, want 75", got.Spec.GPU.Governor)
	}
	var fromJSON NodeTwin
	if err := json.Unmarshal([]byte(`{"spec":{"nodeName":"w","profile":"eco","gpu":{"governor":{"preset":"efficient","computeClockCeilingPctOfMax":75}}}}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if g := fromJSON.Spec.GPU.Governor; g == nil || g.ComputeClockCeilingPctOfMax == nil || *g.ComputeClockCeilingPctOfMax != 75 {
		t.Fatalf("JSON ceiling 75 read back as %+v", fromJSON.Spec.GPU.Governor)
	}
}

// GV03: the generated schema bounds computeClockCeilingPctOfMax to (0, 100]:
// 0 and 101 are rejected, 75 and 100 accepted. The check reads the bounds
// controller-gen wrote into config/crd/bases, the copy kubectl applies.
func TestGV03ComputeClockCeilingBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "joulie.io_nodetwins.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	schema := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	field := schemaAt(schema, "spec", "gpu", "governor", "computeClockCeilingPctOfMax")
	if field == nil {
		t.Fatal("spec.gpu.governor.computeClockCeilingPctOfMax missing from the schema")
	}
	for v, want := range map[float64]bool{0: false, 101: false, 75: true, 100: true, 0.5: true, -1: false} {
		if got := schemaNumberAllowed(field, v); got != want {
			t.Errorf("computeClockCeilingPctOfMax=%v: allowed=%v, want %v (schema %v)", v, got, want, field)
		}
	}
}

// schemaAt walks the properties of an OpenAPI v3 schema.
func schemaAt(schema map[string]any, path ...string) map[string]any {
	cur := schema
	for _, p := range path {
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = props[p].(map[string]any); !ok {
			return nil
		}
	}
	return cur
}

// schemaNumberAllowed applies the numeric bounds of an OpenAPI v3 schema
// (minimum, maximum and their boolean exclusive forms, as structural CRD
// schemas write them) to v.
func schemaNumberAllowed(s map[string]any, v float64) bool {
	num := func(k string) (float64, bool) {
		switch x := s[k].(type) {
		case float64:
			return x, true
		case int64:
			return float64(x), true
		}
		return 0, false
	}
	if lo, ok := num("minimum"); ok && (v < lo || v == lo && s["exclusiveMinimum"] == true) {
		return false
	}
	if hi, ok := num("maximum"); ok && (v > hi || v == hi && s["exclusiveMaximum"] == true) {
		return false
	}
	return true
}

// roundTrip converts in to unstructured and back into out, fails the test
// when they differ, and returns the unstructured form for path checks.
func roundTrip(t *testing.T, in, out runtime.Object) map[string]any {
	t.Helper()
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(in)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u, out); err != nil {
		t.Fatalf("FromUnstructured: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the object\n in: %#v\nout: %#v", in, out)
	}
	return u
}

func mustHavePath(t *testing.T, u map[string]any, path string) {
	t.Helper()
	var cur any = u
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %s: %q is not an object", path, key)
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("path %s: key %q missing from unstructured form", path, key)
		}
	}
}
