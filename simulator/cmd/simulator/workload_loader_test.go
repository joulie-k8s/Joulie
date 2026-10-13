package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// legacyOrderTrace has 64 jobs in a scrambled id order, half of them due at
// 0 s, so the 20-per-tick cap splits them over two ticks and the unstable
// sort decides which go first.
func legacyOrderTrace() []string {
	offsets := []float64{0, 0, 2, 5}
	lines := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		ns := "default"
		if i%3 == 0 {
			ns = "team-a"
		}
		lines = append(lines, fmt.Sprintf(
			`{"type":"job","jobId":"job-%02d","submitTimeOffsetSec":%g,"namespace":%q,"podTemplate":{"requests":{"cpu":"1"}},"work":{"cpuUnits":100}}`,
			(i*37)%64, offsets[(i*7)%4], ns))
	}
	return lines
}

// injectionOrder runs the legacy injection for ticks one-second ticks and
// returns one line per created pod: the tick, the namespace and name, and
// the jobId annotation.
func injectionOrder(t *testing.T, s *simulator, ticks int) []string {
	t.Helper()
	kube := k8sfake.NewSimpleClientset()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.workload.startTime = start
	var out []string
	for k := 0; k < ticks; k++ {
		kube.ClearActions()
		s.injectTraceJobs(context.Background(), kube, start.Add(time.Duration(k)*time.Second))
		for _, a := range kube.Actions() {
			ca, ok := a.(k8stesting.CreateAction)
			if !ok || a.GetResource().Resource != "pods" {
				continue
			}
			pod := ca.GetObject().(*corev1.Pod)
			out = append(out, fmt.Sprintf("%d %s/%s %s", k, pod.Namespace, pod.Name, pod.Annotations["sim.joulie.io/jobId"]))
		}
	}
	return out
}

// TestLegacyInjectionOrderUnchanged pins the order and the per-tick split in
// which the legacy engine creates pods. The golden was captured from the code
// before the per-tick caps became settings, and legacy keeps sort.Slice, so
// the order of jobs due at the same offset must not move either.
func TestLegacyInjectionOrderUnchanged(t *testing.T) {
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineFromTrace(writeTrace(t, legacyOrderTrace()...)); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	got := []byte(strings.Join(injectionOrder(t, s, 7), "\n") + "\n")
	golden := filepath.Join("testdata", "legacy-injection-order.golden")
	if *updateGoldens {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy injection order moved:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestLoadTraceReadsV3Fields: a v3 record carries its podTemplate placement,
// its schemaVersion, whether it gave a sensitivity, and the shared and MIG
// GPU resources. A v2 record with the same keys reads as before.
func TestLoadTraceReadsV3Fields(t *testing.T) {
	podTemplate := `"podTemplate":{"requests":{"cpu":"1",%s},` +
		`"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"zone","operator":"In","values":["a"]}]}]}}},` +
		`"nodeSelector":{"pool":"gpu"},"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]}`
	rec := func(id, schema, gpu, extra string) string {
		return fmt.Sprintf(`{"type":"job","schemaVersion":%q,"jobId":%q,`+podTemplate+`%s}`, schema, id, gpu, extra)
	}
	path := writeTrace(t,
		rec("v3-mig", workload.TraceSchemaV3, `"nvidia.com/mig-3g.40gb":"2","nvidia.com/mig-1g.10gb":"1"`, `,"sensitivity":{"gpu":0.5}`),
		rec("v3-shared", workload.TraceSchemaV3, `"nvidia.com/gpu.shared":"1"`, ""),
		rec("v3-whole", workload.TraceSchemaV3, `"nvidia.com/gpu":"1","nvidia.com/mig-1g.10gb":"1"`, ""),
		rec("v2-mig", workload.TraceSchemaV2, `"nvidia.com/mig-1g.10gb":"1"`, `,"sensitivity":{"cpu":0.4,"gpu":0.5}`),
	)
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	byID := map[string]*simJob{}
	for _, j := range s.workload.jobs {
		byID[j.JobID] = j
	}
	for _, tc := range []struct {
		id, resource string
		gpus         float64
		sensSet      bool
	}{
		{"v3-mig", "nvidia.com/mig-1g.10gb", 1, true},
		{"v3-shared", "nvidia.com/gpu.shared", 1, false},
		{"v3-whole", "nvidia.com/gpu", 1, false},
		{"v2-mig", "", 0, true},
	} {
		j := byID[tc.id]
		if j.GPUResourceName != tc.resource || j.RequestedGPUs != tc.gpus || j.SensitivitySet != tc.sensSet {
			t.Errorf("%s: gpu %q x %g, sensitivity set %v; want %q x %g, %v", tc.id, j.GPUResourceName, j.RequestedGPUs, j.SensitivitySet, tc.resource, tc.gpus, tc.sensSet)
		}
	}
	v3 := byID["v3-mig"]
	if v3.SchemaVersion != workload.TraceSchemaV3 || v3.SensitivityGPU != 0.5 || v3.SensitivityCPU != 1 {
		t.Errorf("v3-mig: schema %q, sensitivity cpu %g gpu %g; want v3, 1, 0.5", v3.SchemaVersion, v3.SensitivityCPU, v3.SensitivityGPU)
	}
	if v3.Affinity == nil || v3.Affinity.NodeAffinity == nil || v3.NodeSelector["pool"] != "gpu" ||
		len(v3.Tolerations) != 1 || v3.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("v3-mig placement = %+v %v %v, want the trace's affinity, nodeSelector and toleration", v3.Affinity, v3.NodeSelector, v3.Tolerations)
	}
	if v2 := byID["v2-mig"]; v2.Affinity != nil || v2.NodeSelector != nil || v2.Tolerations != nil || v2.SchemaVersion != workload.TraceSchemaV2 {
		t.Errorf("v2-mig: placement %+v %v %v, schema %q; want none, v2", v2.Affinity, v2.NodeSelector, v2.Tolerations, v2.SchemaVersion)
	}

	t.Run("a misspelled placement field is an error", func(t *testing.T) {
		path := writeTrace(t, `{"type":"job","schemaVersion":"v3","jobId":"bad","podTemplate":{"requests":{"cpu":"1"},"nodeSelector":{"a":"b"},"tolerations":[{"keey":"x"}]}}`)
		err := newLoaderTestSimulator().initWorkloadEngineFromTrace(path)
		if err == nil || !strings.Contains(err.Error(), "trace line 1") || !strings.Contains(err.Error(), "keey") {
			t.Fatalf("error = %v, want one naming trace line 1 and the field", err)
		}
	})
}

// TestV1LoaderKeepsTraceOrderAndNames: v1 sorts stably, so jobs due at the
// same offset stay in trace order, and leaves pod names alone; its pods are
// named by their Job objects.
func TestV1LoaderKeepsTraceOrderAndNames(t *testing.T) {
	path := writeTrace(t, legacyOrderTrace()...)
	engine, parts, err := newWorkloadEngineFromTrace(path, workloadOptions{Model: workload.ModelV1})
	if err != nil || parts != 1 {
		t.Fatalf("newWorkloadEngineFromTrace: parts %d, %v", parts, err)
	}
	var want []string
	for _, off := range []float64{0, 2, 5} {
		for i := range legacyOrderTrace() {
			if []float64{0, 0, 2, 5}[(i*7)%4] == off {
				want = append(want, fmt.Sprintf("job-%02d", (i*37)%64))
			}
		}
	}
	for i, j := range engine.jobs {
		if j.JobID != want[i] {
			t.Fatalf("job %d is %s, want %s: v1 must keep trace order at equal offsets", i, j.JobID, want[i])
		}
	}
	if engine.opts.model() != workload.ModelV1 {
		t.Fatalf("engine model = %s, want v1", engine.opts.model())
	}
}
