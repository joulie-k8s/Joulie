package main

// The regression tests of the legacy loader: every job gets a pod of its
// own, a trace line longer than the scanner's default buffer loads, and a
// pod keeps the trace's affinity.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// newLoaderTestSimulator is a simulator with no nodes, for the loader and
// injection tests.
func newLoaderTestSimulator() *simulator {
	return newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
}

func writeTrace(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	return path
}

// TestLegacyLoaderSuffixesCollidingPodNames is the regression test of the
// first loader fix: distinct jobIds that sanitize to one pod name in one
// namespace each get a pod. Before, the second create failed with
// AlreadyExists, the job counted as submitted with no pod of its own, and it
// was completed by force after 120 s of no progress.
func TestLegacyLoaderSuffixesCollidingPodNames(t *testing.T) {
	long := strings.Repeat("x", 55)
	job := func(id, ns string) string {
		return fmt.Sprintf(`{"type":"job","jobId":%q,"submitTimeOffsetSec":0,"namespace":%q,"podTemplate":{"requests":{"cpu":"1"}}}`, id, ns)
	}
	s := newLoaderTestSimulator()
	path := writeTrace(t,
		job("a_b", "default"),
		job("a.b", "default"),
		job("a-b-2", "default"),
		job("A B", "default"),
		job("a:b", "team-a"),
		job(long+"-1", "default"),
		job(long+"-2", "default"),
	)
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	want := map[string]string{
		"a_b":       "default/sim-a-b",
		"a.b":       "default/sim-a-b-3",
		"a-b-2":     "default/sim-a-b-2",
		"A B":       "default/sim-a-b-4",
		"a:b":       "team-a/sim-a-b",
		long + "-1": "default/sim-" + long[:50],
		long + "-2": "default/sim-" + long[:50] + "-2",
	}
	seen := map[string]string{}
	for _, j := range s.workload.jobs {
		got := j.Namespace + "/" + j.PodName
		if other, ok := seen[got]; ok {
			t.Errorf("jobs %q and %q share pod %s", other, j.JobID, got)
		}
		seen[got] = j.JobID
		if got != want[j.JobID] {
			t.Errorf("job %q: pod %s, want %s", j.JobID, got, want[j.JobID])
		}
	}

	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, time.Now().Add(time.Second))
	for _, j := range s.workload.jobs {
		pod, err := kube.CoreV1().Pods(j.Namespace).Get(context.Background(), j.PodName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("job %q: no pod: %v", j.JobID, err)
			continue
		}
		if got := pod.Annotations["sim.joulie.io/jobId"]; got != j.JobID {
			t.Errorf("job %q counts as submitted, but pod %s/%s belongs to job %q", j.JobID, j.Namespace, j.PodName, got)
		}
	}
}

// TestLoadTraceLongLine is the regression test of the second loader fix: a
// job line over bufio's default 64 KiB loads. Before, the whole trace failed
// with "token too long" and the simulator ran without a workload. A line
// over the new 16 MiB limit is still refused, naming its line.
func TestLoadTraceLongLine(t *testing.T) {
	pad := strings.Repeat("p", 200*1024)
	path := writeTrace(t,
		`{"type":"job","jobId":"short","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1"}}}`,
		fmt.Sprintf(`{"type":"job","jobId":"long","submitTimeOffsetSec":1,"tags":[%q],"podTemplate":{"requests":{"cpu":"2"}}}`, pad),
	)
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions with a %d KiB line: %v", len(pad)/1024, err)
	}
	if len(s.workload.jobs) != 2 || s.workload.jobs[1].JobID != "long" || s.workload.jobs[1].RequestedCPUCores != 2 {
		t.Fatalf("jobs = %d, want short and long with cpu 2", len(s.workload.jobs))
	}

	t.Run("over the limit", func(t *testing.T) {
		huge := strings.Repeat("p", 17<<20)
		path := writeTrace(t,
			`{"type":"job","jobId":"short","podTemplate":{"requests":{"cpu":"1"}}}`,
			fmt.Sprintf(`{"type":"job","jobId":"huge","tags":[%q]}`, huge),
		)
		err := newLoaderTestSimulator().initWorkloadEngineWithOptions(path, workloadOptions{})
		if err == nil || !strings.Contains(err.Error(), "trace line 2") {
			t.Fatalf("error = %v, want one naming trace line 2", err)
		}
	})
}

// TestLegacyPodKeepsTheTraceAffinity is the regression test of the third
// loader fix: a legacy pod carries its trace record's affinity, with the
// NotIn-eco requirement of a performance pod ANDed into it. Before, the pod
// had only the requirement of its intent class, so a CPU-only job that the
// sweeps of experiments 02 and 03 restrict to joulie.io/hw.kind=cpu-only
// nodes could run on a GPU node.
func TestLegacyPodKeepsTheTraceAffinity(t *testing.T) {
	cpuOnly := `"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"joulie.io/hw.kind","operator":"In","values":["cpu-only"]}]}]}}}`
	s := newLoaderTestSimulator()
	path := writeTrace(t,
		`{"type":"job","jobId":"perf","submitTimeOffsetSec":0,"intentClass":"performance","podTemplate":{"requests":{"cpu":"1"},`+cpuOnly+`}}`,
		`{"type":"job","jobId":"std","submitTimeOffsetSec":0,"intentClass":"standard","podTemplate":{"requests":{"cpu":"1"},`+cpuOnly+`}}`,
		`{"type":"job","jobId":"bare","submitTimeOffsetSec":0,"intentClass":"standard","podTemplate":{"requests":{"cpu":"1"}}}`,
	)
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, time.Now().Add(time.Second))
	terms := func(id string) [][]string {
		for _, j := range s.workload.jobs {
			if j.JobID != id {
				continue
			}
			pod, err := kube.CoreV1().Pods(j.Namespace).Get(context.Background(), j.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("job %q: no pod: %v", id, err)
			}
			if pod.Spec.Affinity == nil {
				return nil
			}
			var out [][]string
			for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				var exprs []string
				for _, e := range term.MatchExpressions {
					exprs = append(exprs, fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values))
				}
				out = append(out, exprs)
			}
			return out
		}
		t.Fatalf("no job %q", id)
		return nil
	}
	if got, want := fmt.Sprint(terms("perf")), "[[joulie.io/hw.kind In [cpu-only] joulie.io/power-profile NotIn [eco]]]"; got != want {
		t.Errorf("performance pod terms %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(terms("std")), "[[joulie.io/hw.kind In [cpu-only]]]"; got != want {
		t.Errorf("standard pod terms %s, want %s", got, want)
	}
	if got := terms("bare"); got != nil {
		t.Errorf("a standard pod without trace affinity has terms %v, want none", got)
	}
}

// TestIdleGPUNodeDropsItsUtilization is the regression test of a GPU node
// whose last job ended: the pod poll sets its GPU utilization to 0, and its
// CPU to 0.12 per running pod outside the workload only. The
// workload tick only visits nodes with running pods, and the poll used to
// reset only the CPU, so the node kept its last job's GPU utilization, and
// that job's GPU power, until a new job arrived.
func TestIdleGPUNodeDropsItsUtilization(t *testing.T) {
	s := newLoaderTestSimulator()
	path := writeTrace(t, `{"type":"job","jobId":"j","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1","nvidia.com/gpu":"1"}},"work":{"cpuUnits":1,"gpuUnits":1}}`)
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	labels := map[string]map[string]string{"g1": {"joulie.io/hw.gpu-count": "8", "joulie.io/hw.cpu-cores": "64"}}
	s.refreshNodeStateFromKubeData(map[string]int{}, nil, labels)
	st := s.state["g1"]
	if st == nil {
		t.Fatalf("no state for g1")
	}
	st.GPUUtil = 0.9 // left by the node's last job
	// The pod of job j, which is not running on g1, adds no CPU load; a
	// pod outside the workload adds 0.12.
	workloadPod := runningPodInfo{Node: "g1", JobID: "j", Running: true}
	s.refreshNodeStateFromKubeData(map[string]int{"g1": 1}, []runningPodInfo{workloadPod}, labels)
	if st.GPUUtil != 0 || st.CPUUtil != 0 {
		t.Fatalf("node without jobs, one workload pod: GPU utilization %g, CPU %g, want 0 and 0", st.GPUUtil, st.CPUUtil)
	}
	otherPod := runningPodInfo{Node: "g1", Running: true}
	s.refreshNodeStateFromKubeData(map[string]int{"g1": 2}, []runningPodInfo{workloadPod, otherPod}, labels)
	if st.GPUUtil != 0 || st.CPUUtil != 0.12 {
		t.Fatalf("node without jobs, one pod outside the workload: GPU utilization %g, CPU %g, want 0 and 0.12", st.GPUUtil, st.CPUUtil)
	}
}
