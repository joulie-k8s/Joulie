package v1alpha1

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The agent and operator write these objects as unstructured maps, so the
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
			HardwareDensityScore:        88,
			EstimatedPUE:                1.12,
			ControlStatus: &ControlStatus{
				CPU: &ControlResult{Backend: "rapl", Result: "applied", Message: "ok", UpdatedAt: "2026-01-01T00:00:00Z"},
				GPU: &ControlResult{Backend: "nvml", Result: "failed", Message: "no device", UpdatedAt: "2026-01-01T00:00:01Z"},
			},
			LastUpdated: "2026-01-01T00:00:02Z",
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
		"status.hardwareDensityScore", "status.estimatedPUE",
		"status.controlStatus.cpu.backend", "status.controlStatus.gpu.updatedAt", "status.lastUpdated",
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
