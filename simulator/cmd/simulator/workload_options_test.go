package main

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// dueJobs is an engine of n legacy jobs due now, as the main_test.go
// literals build them.
func dueJobs(n int, opts workloadOptions) *workloadEngine {
	e := &workloadEngine{startTime: time.Now().Add(-time.Minute), opts: opts, jobByID: map[string]*simJob{}}
	for i := 0; i < n; i++ {
		j := &simJob{JobID: fmt.Sprintf("j%d", i), Namespace: "default", PodName: fmt.Sprintf("sim-j%d", i), RequestedCPUCores: 1, CPUUnitsRemaining: 100}
		e.jobs = append(e.jobs, j)
		e.jobByID[j.JobID] = j
	}
	return e
}

func createdPods(t *testing.T, kube *k8sfake.Clientset) int {
	t.Helper()
	n := 0
	for _, a := range kube.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func deletedPods(kube *k8sfake.Clientset) int {
	n := 0
	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

// legacyBehaviour is what the legacy engine e does in one tick: the pods it
// creates out of 30 due jobs, the completed pods it deletes out of 15, and
// whether a job with no progress for 119 s and for 121 s is completed by
// force.
func legacyBehaviour(t *testing.T, opts workloadOptions) (creates, deletes int, forced119, forced121 bool) {
	t.Helper()
	s := newLoaderTestSimulator()
	now := time.Now()

	s.workload = dueJobs(30, opts)
	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, now)
	creates = createdPods(t, kube)

	s.workload = dueJobs(15, opts)
	var running []runningPodInfo
	for _, j := range s.workload.jobs {
		j.Submitted, j.Completed, j.SubmittedAt, j.LastProgressAt = true, true, now, now
		running = append(running, runningPodInfo{Namespace: j.Namespace, Name: j.PodName, Node: "n1", JobID: j.JobID})
	}
	podDetailFunc = func(context.Context, kubernetes.Interface) ([]runningPodInfo, error) { return running, nil }
	defer func() { podDetailFunc = listRunningPodsDetailed }()
	kube = k8sfake.NewSimpleClientset()
	s.advanceJobProgress(context.Background(), kube, 1, now)
	deletes = deletedPods(kube)

	stalled := func(idle time.Duration) bool {
		s.workload = dueJobs(1, opts)
		j := s.workload.jobs[0]
		j.Submitted, j.SubmittedAt, j.LastProgressAt = true, now.Add(-idle), now.Add(-idle)
		s.detectStalledJobs(k8sfake.NewSimpleClientset(), now)
		return j.Completed
	}
	return creates, deletes, stalled(119 * time.Second), stalled(121 * time.Second)
}

// TestLegacyDefaultsUnchanged: with nothing set, and for the zero value of
// the options that every workloadEngine literal in the tests carries, the
// legacy engine keeps the constants it had: 20 creates and 10 deletes per
// tick, force-complete after 120 s, a client at 30 QPS with a burst of 50.
func TestLegacyDefaultsUnchanged(t *testing.T) {
	loaded, warnings, err := loadWorkloadOptions(envMap(nil), false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("loadWorkloadOptions with nothing set: %v, warnings %v", err, warnings)
	}
	for name, o := range map[string]workloadOptions{"loaded": loaded, "zero value": {}, "zero-value engine": (&workloadEngine{}).opts} {
		t.Run(name, func(t *testing.T) {
			if o.model() != workload.ModelLegacy {
				t.Errorf("model = %s, want legacy", o.model())
			}
			timeout, enabled := o.stallForceComplete()
			if o.maxCreatesPerTick() != 20 || o.maxDeletesPerTick() != 10 || timeout != 120*time.Second || !enabled ||
				o.kubeQPS() != 30 || o.kubeBurst() != 50 {
				t.Errorf("creates %d, deletes %d, force-complete %s (%v), qps %g, burst %d; want 20, 10, 120s (true), 30, 50",
					o.maxCreatesPerTick(), o.maxDeletesPerTick(), timeout, enabled, o.kubeQPS(), o.kubeBurst())
			}
			creates, deletes, forced119, forced121 := legacyBehaviour(t, o)
			if creates != 20 || deletes != 10 || forced119 || !forced121 {
				t.Errorf("one tick: %d creates, %d deletes, forced at 119 s %v, at 121 s %v; want 20, 10, false, true", creates, deletes, forced119, forced121)
			}
		})
	}
}

// TestLegacyInjectCapConfigurable: the per-tick caps and the force-complete
// timeout are settings in legacy mode, with -1 removing each.
func TestLegacyInjectCapConfigurable(t *testing.T) {
	for _, tc := range []struct {
		env                  map[string]string
		creates, deletes     int
		forced119, forced121 bool
	}{
		{map[string]string{workload.EnvMaxCreatesPerTick: "5", workload.EnvMaxDeletesPerTick: "3", workload.EnvStallForceComplete: "60s"}, 5, 3, true, true},
		{map[string]string{workload.EnvMaxCreatesPerTick: "-1", workload.EnvMaxDeletesPerTick: " -1 ", workload.EnvStallForceComplete: "-1"}, 30, 15, false, false},
		{map[string]string{workload.EnvStallForceComplete: "3m"}, 20, 10, false, false},
	} {
		t.Run(fmt.Sprint(tc.env), func(t *testing.T) {
			o, warnings, err := loadWorkloadOptions(envMap(tc.env), false)
			if err != nil || len(warnings) != 0 {
				t.Fatalf("loadWorkloadOptions: %v, warnings %v", err, warnings)
			}
			creates, deletes, forced119, forced121 := legacyBehaviour(t, o)
			if creates != tc.creates || deletes != tc.deletes || forced119 != tc.forced119 || forced121 != tc.forced121 {
				t.Errorf("one tick: %d creates, %d deletes, forced at 119 s %v, at 121 s %v; want %d, %d, %v, %v",
					creates, deletes, forced119, forced121, tc.creates, tc.deletes, tc.forced119, tc.forced121)
			}
		})
	}

	t.Run("invalid values are startup errors", func(t *testing.T) {
		_, _, err := loadWorkloadOptions(envMap(map[string]string{
			workload.EnvMaxCreatesPerTick:  "0",
			workload.EnvMaxDeletesPerTick:  "ten",
			workload.EnvStallForceComplete: "-2m",
			workload.EnvKubeQPS:            "0",
			workload.EnvKubeBurst:          "1.5",
		}), false)
		if err == nil {
			t.Fatal("loadWorkloadOptions accepted invalid values")
		}
		for _, k := range []string{workload.EnvMaxCreatesPerTick, workload.EnvMaxDeletesPerTick, workload.EnvStallForceComplete, workload.EnvKubeQPS, workload.EnvKubeBurst} {
			if !strings.Contains(err.Error(), k) {
				t.Errorf("error %q does not name %s", err, k)
			}
		}
		for _, qps := range []string{"NaN", "+Inf", "1e39"} {
			if _, _, err := loadWorkloadOptions(envMap(map[string]string{workload.EnvKubeQPS: qps}), false); err == nil || !strings.Contains(err.Error(), workload.EnvKubeQPS) {
				t.Errorf("%s=%s: error = %v, want one naming %s", workload.EnvKubeQPS, qps, err, workload.EnvKubeQPS)
			}
		}
	})

	t.Run("v1-only variables warn", func(t *testing.T) {
		o, warnings, err := loadWorkloadOptions(envMap(map[string]string{workload.EnvOwner: "pod", workload.EnvKubeQPS: "12.5"}), false)
		if err != nil || o.kubeQPS() != 12.5 {
			t.Fatalf("loadWorkloadOptions: %v, qps %g", err, o.kubeQPS())
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], workload.EnvOwner) {
			t.Fatalf("warnings = %q, want one naming %s", warnings, workload.EnvOwner)
		}
	})
}

// TestEmulateWithLegacyIsAnError: emulate mode runs the v1 engine, which
// needs emulate mode (X10). The model defaults to v1 with SIM_EMULATE and to
// legacy without it; the other pairing is an error, and checkModes lets a
// trace through in emulate mode unless the model is legacy.
func TestEmulateWithLegacyIsAnError(t *testing.T) {
	if _, _, err := loadWorkloadOptions(envMap(map[string]string{workload.EnvModel: "legacy"}), true); err == nil ||
		!strings.Contains(err.Error(), "SIM_WORKLOAD_MODEL=legacy") || !strings.Contains(err.Error(), envEmulate) {
		t.Errorf("legacy with emulate: error = %v, want one naming %s=legacy and %s", err, workload.EnvModel, envEmulate)
	}
	if _, _, err := loadWorkloadOptions(envMap(map[string]string{workload.EnvModel: "v1"}), false); err == nil ||
		!strings.Contains(err.Error(), envEmulate) {
		t.Errorf("v1 without emulate: error = %v, want one naming %s", err, envEmulate)
	}
	if _, _, err := loadWorkloadOptions(envMap(map[string]string{workload.EnvModel: "v2"}), true); err == nil ||
		!strings.Contains(err.Error(), workload.EnvModel) {
		t.Errorf("unknown model: error = %v, want one naming %s", err, workload.EnvModel)
	}
	if o, _, err := loadWorkloadOptions(envMap(nil), true); err != nil || o.model() != workload.ModelV1 {
		t.Errorf("emulate with the model unset: model %s, %v; want v1", o.model(), err)
	}

	spec := map[string]string{envEmulate: "true", envClusterSpec: "/s.yaml", envWorkloadTrace: "/t.jsonl"}
	if err := checkModes(envMap(spec)); err != nil {
		t.Errorf("checkModes with a v1 trace: %v", err)
	}
	spec[workload.EnvModel] = "legacy"
	if err := checkModes(envMap(spec)); err == nil || !strings.Contains(err.Error(), envWorkloadTrace) {
		t.Errorf("checkModes with a legacy trace: error = %v, want one naming %s", err, envWorkloadTrace)
	}
}

// TestV1OptionsDefaultsAndValues: v1 fills the defaults of the settings
// table, reads each variable, and refuses invalid values all together.
func TestV1OptionsDefaultsAndValues(t *testing.T) {
	o, warnings, err := loadWorkloadOptions(envMap(map[string]string{envHwemuRoot: "/emu"}), true)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("loadWorkloadOptions: %v, warnings %v", err, warnings)
	}
	want := workloadOptions{
		Model:             workload.ModelV1,
		MaxCreatesPerTick: workload.Unlimited,
		KubeQPS:           100,
		KubeBurst:         200,
		Owner:             workload.OwnerJob,
		Intent:            workload.IntentAnnotation,
		Completion:        workload.CompletionKWOK,
		Tolerations:       []corev1.Toleration{workload.KWOKToleration()},
		JobTTL:            10 * time.Minute,
		StallReport:       5 * time.Minute,
		RunsDir:           filepath.Join("/emu", "_runs"),
		OTLPFileInterval:  10 * time.Second,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("v1 defaults:\n got %+v\nwant %+v", o, want)
	}

	o, warnings, err = loadWorkloadOptions(envMap(map[string]string{
		workload.EnvOwner:              "pod",
		workload.EnvIntent:             "both",
		workload.EnvCompletion:         "delete",
		workload.EnvNamespace:          "sim-runs",
		workload.EnvNodeSelector:       "pool=gpu, zone=a",
		workload.EnvTolerations:        `[]`,
		workload.EnvJobTTL:             "0s",
		workload.EnvMaxCreatesPerTick:  "50",
		workload.EnvRunsDir:            "/runs",
		workload.EnvRunID:              "night-1",
		workload.EnvStallForceComplete: "60s",
	}), true)
	if err != nil {
		t.Fatalf("loadWorkloadOptions: %v", err)
	}
	if o.Owner != workload.OwnerPod || o.Intent != workload.IntentBoth || o.Completion != workload.CompletionDelete ||
		o.Namespace != "sim-runs" || !reflect.DeepEqual(o.NodeSelector, map[string]string{"pool": "gpu", "zone": "a"}) ||
		o.Tolerations == nil || len(o.Tolerations) != 0 || o.JobTTL != 0 || o.MaxCreatesPerTick != 50 ||
		o.RunsDir != "/runs" || o.RunID != "night-1" {
		t.Errorf("v1 values not read: %+v", o)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], workload.EnvStallForceComplete) {
		t.Errorf("warnings = %q, want one naming %s", warnings, workload.EnvStallForceComplete)
	}

	_, _, err = loadWorkloadOptions(envMap(map[string]string{
		workload.EnvOwner:            "cronjob",
		workload.EnvNamespace:        "Not_A_Namespace",
		workload.EnvNodeSelector:     "pool",
		workload.EnvTolerations:      `[{"keey":"x"}]`,
		workload.EnvStallReport:      "0s",
		workload.EnvOTLPFileInterval: "soon",
		workload.EnvRunID:            "../escape",
	}), true)
	if err == nil {
		t.Fatal("loadWorkloadOptions accepted invalid v1 values")
	}
	for _, k := range []string{workload.EnvOwner, workload.EnvNamespace, workload.EnvNodeSelector, workload.EnvTolerations,
		workload.EnvStallReport, workload.EnvOTLPFileInterval, workload.EnvRunID} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q does not name %s", err, k)
		}
	}
}
