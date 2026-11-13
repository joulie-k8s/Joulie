package main

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// decisionsOf reads the decision conditions the controller manager wrote to
// the NodeTwin of node, through the typed status as kubectl joulie reads
// them, as "Type=Status/Reason" keyed by type.
func decisionsOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient, node string) map[string]string {
	t.Helper()
	u, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), joulie.ObjectNameForNode(node), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin %s: %v", node, err)
	}
	var nt v1alpha1.NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nt); err != nil {
		t.Fatalf("decode NodeTwin %s: %v", node, err)
	}
	out := map[string]string{}
	for _, c := range nt.Status.Conditions {
		if !slices.Contains(joulie.DecisionConditionTypes, c.Type) {
			continue
		}
		if c.LastTransitionTime.IsZero() || c.Message == "" {
			t.Errorf("%s condition %s has no lastTransitionTime or message: %+v", node, c.Type, c)
		}
		out[c.Type] = string(c.Status) + "/" + c.Reason
	}
	return out
}

// reconcileDecisions runs one static_partition reconcile at fraction 0, so
// each hardware family keeps one performance node, with GPU eco cap gpuEcoPct.
func reconcileDecisions(t *testing.T, objs []runtime.Object, gpuEcoPct float64) *dynamicfake.FakeDynamicClient {
	t.Helper()
	reader, client, dyn := fakeClients(t, objs...)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, "static_partition", 0, 0, 1, 5, 10,
		100, 60, false, 100, gpuEcoPct, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return dyn
}

// gpuCapResultTwin is the NodeTwin of node holding the agent's last GPU cap
// result.
func gpuCapResultTwin(node, result string) *v1alpha1.NodeTwin {
	return &v1alpha1.NodeTwin{
		TypeMeta:   metav1.TypeMeta{APIVersion: "joulie.io/v1alpha1", Kind: "NodeTwin"},
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: node, Profile: profileEco},
		Status: v1alpha1.NodeTwinStatus{ControlStatus: &v1alpha1.ControlStatus{
			GPU: &v1alpha1.ControlResult{Backend: "nvidia-smi", Result: result},
		}},
	}
}

// Every branch that decides a node's profile and caps reaches its NodeTwin
// as a condition with its reason, through a whole reconcile and the typed
// status a reader decodes: c0 and g0 hold their family's performance slot,
// c1 and g1 go to eco, and the cases vary what decides the rest.
func TestDecisionConditionsRecordWhyEachNodeGotItsProfileAndCaps(t *testing.T) {
	gpuNodes := func(control bool) []runtime.Object {
		return []runtime.Object{
			governorNode("g0", "performance", 4), governorNode("g1", "performance", 4),
			gpuNodeHardware(t, "g0", control), gpuNodeHardware(t, "g1", control),
		}
	}
	cpuNodes := []runtime.Object{governorNode("c0", "performance", 0), governorNode("c1", "performance", 0)}
	off := "False/" + joulie.ReasonGovernorControlOff
	for _, tc := range []struct {
		name      string
		objs      []runtime.Object
		gpuEcoPct float64
		uncapCPU  string // CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES
		want      map[string]map[string]string
	}{
		{
			name: "cpu nodes", objs: cpuNodes, gpuEcoPct: 60,
			want: map[string]map[string]string{
				"c0": {
					joulie.ConditionEcoProfile:        "False/" + joulie.ReasonPerformanceSlot,
					joulie.ConditionCPUEcoCap:         "False/" + joulie.ReasonPerformanceProfile,
					joulie.ConditionGPUEcoCap:         "False/" + joulie.ReasonNoGPU,
					joulie.ConditionCPUGovernorPreset: off, joulie.ConditionGPUGovernorPreset: off,
				},
				"c1": {
					joulie.ConditionEcoProfile:        "True/" + joulie.ReasonNoPerformanceSlot,
					joulie.ConditionCPUEcoCap:         "True/" + joulie.ReasonEcoProfile,
					joulie.ConditionGPUEcoCap:         "False/" + joulie.ReasonNoGPU,
					joulie.ConditionCPUGovernorPreset: off, joulie.ConditionGPUGovernorPreset: off,
				},
			},
		},
		{
			name: "cpu node draining", objs: append(append([]runtime.Object{}, cpuNodes...), performancePodOn("c1")), gpuEcoPct: 60,
			want: map[string]map[string]string{"c1": {
				joulie.ConditionEcoProfile: "False/" + joulie.ReasonDrainingPerformancePods,
				joulie.ConditionCPUEcoCap:  "False/" + joulie.ReasonDrainingPerformancePods,
				joulie.ConditionGPUEcoCap:  "False/" + joulie.ReasonNoGPU,
			}},
		},
		{
			name: "gpu nodes", objs: gpuNodes(true), gpuEcoPct: 60,
			want: map[string]map[string]string{
				"g0": {
					joulie.ConditionEcoProfile: "False/" + joulie.ReasonPerformanceSlot,
					joulie.ConditionCPUEcoCap:  "False/" + joulie.ReasonPerformanceProfile,
					joulie.ConditionGPUEcoCap:  "False/" + joulie.ReasonPerformanceProfile,
				},
				"g1": {
					joulie.ConditionEcoProfile: "True/" + joulie.ReasonNoPerformanceSlot,
					joulie.ConditionCPUEcoCap:  "False/" + joulie.ReasonGPUNodeCPUUncapped,
					joulie.ConditionGPUEcoCap:  "True/" + joulie.ReasonEcoProfile,
				},
			},
		},
		{
			name: "gpu node draining", objs: append(gpuNodes(true), performancePodOn("g1")), gpuEcoPct: 60,
			want: map[string]map[string]string{"g1": {
				joulie.ConditionEcoProfile: "False/" + joulie.ReasonDrainingPerformancePods,
				joulie.ConditionCPUEcoCap:  "False/" + joulie.ReasonDrainingPerformancePods,
				joulie.ConditionGPUEcoCap:  "False/" + joulie.ReasonDrainingPerformancePods,
			}},
		},
		{
			name: "gpu eco cap not configured", objs: gpuNodes(true), gpuEcoPct: 0,
			want: map[string]map[string]string{"g1": {joulie.ConditionGPUEcoCap: "False/" + joulie.ReasonGPUCapNotConfigured}},
		},
		{
			name: "gpus without control", objs: gpuNodes(false), gpuEcoPct: 60,
			want: map[string]map[string]string{"g1": {
				joulie.ConditionCPUEcoCap: "False/" + joulie.ReasonGPUNodeCPUUncapped,
				joulie.ConditionGPUEcoCap: "False/" + joulie.ReasonGPUCapNotApplicable,
			}},
		},
		{
			name: "gpus without control, CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES", objs: gpuNodes(false), gpuEcoPct: 60, uncapCPU: "true",
			want: map[string]map[string]string{"g1": {
				joulie.ConditionCPUEcoCap: "True/" + joulie.ReasonCPUEcoCapOnUncappableGPUNode,
				joulie.ConditionGPUEcoCap: "False/" + joulie.ReasonGPUCapNotApplicable,
			}},
		},
		{
			name: "gpu cap blocked", objs: append(gpuNodes(true), gpuCapResultTwin("g1", joulie.CapResultBlocked)), gpuEcoPct: 60,
			want: map[string]map[string]string{"g1": {joulie.ConditionGPUEcoCap: "False/" + joulie.ReasonGPUCapBlocked}},
		},
		{
			name: "gpu cap failed", objs: append(gpuNodes(true), gpuCapResultTwin("g1", joulie.CapResultError)), gpuEcoPct: 60,
			want: map[string]map[string]string{"g1": {joulie.ConditionGPUEcoCap: "False/" + joulie.ReasonGPUCapFailed}},
		},
		{
			name: "gpus without control, but the cap applied", objs: append(gpuNodes(false), gpuCapResultTwin("g1", joulie.CapResultApplied)), gpuEcoPct: 60,
			want: map[string]map[string]string{"g1": {joulie.ConditionGPUEcoCap: "True/" + joulie.ReasonEcoProfile}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES", tc.uncapCPU)
			loadCapOptions().install()
			t.Cleanup(func() { capOptions{}.install() })
			dyn := reconcileDecisions(t, tc.objs, tc.gpuEcoPct)
			for node, want := range tc.want {
				got := decisionsOf(t, dyn, node)
				for condition, w := range want {
					if got[condition] != w {
						t.Errorf("%s %s = %q, want %q (all: %v)", node, condition, got[condition], w, got)
					}
				}
			}
		})
	}
}

// The governor decisions reach the NodeTwin too: with GOVERNOR_CONTROL=all
// every node of governorCluster gets the preset of the profile it runs at,
// a draining node keeps the performance one, a GPU node's CPU governor is
// left alone, and a CPU-only node has no GPU governor.
func TestDecisionConditionsRecordTheGovernors(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "GPU_GOVERNOR_PERFORMANCE_PRESET": "peak"})
	reader, client, dyn := fakeClients(t, governorCluster(t, performancePodOn("g1"))...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	for node, want := range map[string]map[string]string{
		"c0": {
			joulie.ConditionCPUGovernorPreset: "True/" + joulie.ReasonPerformanceProfile,
			joulie.ConditionGPUGovernorPreset: "False/" + joulie.ReasonNoGPU,
		},
		"c1": {joulie.ConditionCPUGovernorPreset: "True/" + joulie.ReasonEcoProfile},
		"g0": {
			joulie.ConditionCPUGovernorPreset: "False/" + joulie.ReasonGPUNodeCPUGovernorUnset,
			joulie.ConditionGPUGovernorPreset: "True/" + joulie.ReasonPerformanceProfile,
		},
		"g1": {joulie.ConditionGPUGovernorPreset: "True/" + joulie.ReasonDrainingPerformancePods},
	} {
		got := decisionsOf(t, dyn, node)
		for condition, w := range want {
			if got[condition] != w {
				t.Errorf("%s %s = %q, want %q (all: %v)", node, condition, got[condition], w, got)
			}
		}
	}
}

// Each branch of governorDecisions gives its reason, and the reason agrees
// with the intent: a preset is written exactly when the reason says so.
func TestGovernorDecisionReasons(t *testing.T) {
	all := allPresetsHardware()
	none := NodeHardware{}
	gap := NodeHardware{GPUGovernorGap: true}
	multiGPU := func() bool { return true }
	for _, tc := range []struct {
		name               string
		env                map[string]string
		profile            string
		draining, hasGPU   bool
		nh                 NodeHardware
		product            string
		pod                func() bool
		wantCPU, wantGPU   string
		cpuWrite, gpuWrite bool
	}{
		{name: "control off", env: nil, profile: profileEco, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonGovernorControlOff, wantGPU: joulie.ReasonGovernorControlOff},
		{name: "cpu only control", env: map[string]string{"GOVERNOR_CONTROL": "cpu"}, profile: profileEco, nh: all,
			wantCPU: joulie.ReasonEcoProfile, wantGPU: joulie.ReasonGovernorControlOff, cpuWrite: true},
		{name: "performance", env: map[string]string{"GOVERNOR_CONTROL": "all", "GPU_GOVERNOR_PERFORMANCE_PRESET": "peak", "CPU_GOVERNOR_ON_GPU_NODES": "true"},
			profile: profilePerformance, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonPerformanceProfile, wantGPU: joulie.ReasonPerformanceProfile, cpuWrite: true, gpuWrite: true},
		{name: "draining keeps performance", env: map[string]string{"GOVERNOR_CONTROL": "all", "GPU_GOVERNOR_PERFORMANCE_PRESET": "peak", "CPU_GOVERNOR_ON_GPU_NODES": "true"},
			profile: profilePerformance, draining: true, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonDrainingPerformancePods, wantGPU: joulie.ReasonDrainingPerformancePods, cpuWrite: true, gpuWrite: true},
		{name: "gpu node cpu governor left alone", env: map[string]string{"GOVERNOR_CONTROL": "all"}, profile: profileEco, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonGPUNodeCPUGovernorUnset, wantGPU: joulie.ReasonEcoProfile, gpuWrite: true},
		{name: "no gpu", env: map[string]string{"GOVERNOR_CONTROL": "all"}, profile: profileEco, nh: all,
			wantCPU: joulie.ReasonEcoProfile, wantGPU: joulie.ReasonNoGPU, cpuWrite: true},
		{name: "no gpu preset configured", env: map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_PERFORMANCE_PRESET": ""},
			profile: profilePerformance, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonGPUNodeCPUGovernorUnset, wantGPU: joulie.ReasonNoPresetConfigured},
		{name: "preset not listed", env: map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_ON_GPU_NODES": "true"}, profile: profileEco, hasGPU: true, nh: none,
			wantCPU: joulie.ReasonPresetNotListed, wantGPU: joulie.ReasonPresetNotListed},
		{name: "discovery gap", env: map[string]string{"GOVERNOR_CONTROL": "gpu"}, profile: profileEco, hasGPU: true, nh: gap,
			wantCPU: joulie.ReasonGovernorControlOff, wantGPU: joulie.ReasonGovernorDiscoveryGap, gpuWrite: true},
		{name: "model preset", env: map[string]string{"GOVERNOR_CONTROL": "gpu", "GPU_GOVERNOR_MODEL_PRESETS_JSON": `{"MODEL-A": {"eco": "low"}}`},
			profile: profileEco, hasGPU: true, nh: all, product: "MODEL-A",
			wantCPU: joulie.ReasonGovernorControlOff, wantGPU: joulie.ReasonModelPreset, gpuWrite: true},
		{name: "sustained without a multi-GPU pod", env: map[string]string{"GOVERNOR_CONTROL": "gpu", "GPU_GOVERNOR_PERFORMANCE_PRESET": "sustained", "GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU": "true"},
			profile: profilePerformance, hasGPU: true, nh: all,
			wantCPU: joulie.ReasonGovernorControlOff, wantGPU: joulie.ReasonNoMultiGPUPod},
		{name: "sustained with a multi-GPU pod", env: map[string]string{"GOVERNOR_CONTROL": "gpu", "GPU_GOVERNOR_PERFORMANCE_PRESET": "sustained", "GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU": "true"},
			profile: profilePerformance, hasGPU: true, nh: all, pod: multiGPU,
			wantCPU: joulie.ReasonGovernorControlOff, wantGPU: joulie.ReasonMultiGPUPodRunning, gpuWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := useGovernorEnv(t, tc.env)
			d := cfg.governorDecisions("n", tc.profile, tc.draining, tc.hasGPU, tc.nh, tc.product, tc.pod)
			if d.cpuReason != tc.wantCPU || d.gpuReason != tc.wantGPU {
				t.Errorf("reasons cpu=%s gpu=%s, want cpu=%s gpu=%s", d.cpuReason, d.gpuReason, tc.wantCPU, tc.wantGPU)
			}
			if (d.cpu != nil) != tc.cpuWrite || (d.gpu != nil) != tc.gpuWrite {
				t.Errorf("intents cpu=%s gpu=%s, want written cpu=%t gpu=%t", intentString(d.cpu), intentString(d.gpu), tc.cpuWrite, tc.gpuWrite)
			}
			for condition, reason := range map[string]string{joulie.ConditionCPUGovernorPreset: d.cpuReason, joulie.ConditionGPUGovernorPreset: d.gpuReason} {
				if !slices.Contains(joulie.DecisionReasonValues[condition], reason) {
					t.Errorf("%s reason %q is not listed in pkg/api DecisionReasonValues", condition, reason)
				}
			}
		})
	}
}

// No CPU preset variable can be set empty, but a configuration without one
// still writes none, with its reason.
func TestCPUGovernorWithoutAPresetSaysSo(t *testing.T) {
	d := governorConfig{control: governorControlCPU}.governorDecisions("n", profileEco, false, false, allPresetsHardware(), "", nil)
	if d.cpu != nil || d.cpuReason != joulie.ReasonNoPresetConfigured {
		t.Fatalf("CPU governor %s, reason %s; want none, reason %s", intentString(d.cpu), d.cpuReason, joulie.ReasonNoPresetConfigured)
	}
}

// Every reason pkg/api lists for a decision has its message here, a
// message formats nothing but the preset, so no number that changes between
// reconciles, and every reason is one the CRD's condition schema accepts.
func TestEveryDecisionReasonHasAMessage(t *testing.T) {
	reasonPattern := regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)
	for _, condition := range joulie.DecisionConditionTypes {
		reasons := joulie.DecisionReasonValues[condition]
		if len(reasons) == 0 {
			t.Errorf("%s lists no reasons", condition)
		}
		for _, reason := range reasons {
			msg, ok := decisionMessages[condition][reason]
			if !ok || msg == "" {
				t.Errorf("%s reason %s has no message", condition, reason)
			}
			if !reasonPattern.MatchString(reason) {
				t.Errorf("%s reason %q does not match the condition reason pattern", condition, reason)
			}
			if strings.Count(msg, "%") > strings.Count(msg, "%q") || strings.Count(msg, "%q") > 1 {
				t.Errorf("%s reason %s message %q formats more than the preset", condition, reason, msg)
			}
		}
		for reason := range decisionMessages[condition] {
			if !slices.Contains(reasons, reason) {
				t.Errorf("%s message for reason %s, which pkg/api does not list", condition, reason)
			}
		}
	}
	for condition := range decisionMessages {
		if !slices.Contains(joulie.DecisionConditionTypes, condition) {
			t.Errorf("messages for %s, which is not a decision condition", condition)
		}
	}
}

// A plan without a reason writes no condition for that decision: the CRD
// requires a reason, and an empty one would fail the whole status write.
func TestDecisionWithoutAReasonWritesNoCondition(t *testing.T) {
	t.Parallel()
	got := decisionConditions(NodeAssignment{NodeName: "n", Profile: profileEco, ProfileReason: joulie.ReasonNoPerformanceSlot}, "")
	if len(got) != 1 || got[0].Type != joulie.ConditionEcoProfile || got[0].Status != metav1.ConditionTrue {
		t.Fatalf("conditions %+v, want only EcoProfile True", got)
	}
}

// lastTransitionTime moves when a condition's reason changes with its
// status unchanged, as when an eco node goes from NoPerformanceSlot to
// NoPerformanceDemand, and stays when only the message changes.
func TestConditionTransitionTimeMovesWithTheReason(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := func(reason, msg string) metav1.Condition {
		return metav1.Condition{Type: joulie.ConditionEcoProfile, Status: metav1.ConditionTrue, Reason: reason, Message: msg}
	}
	out, _ := mergeConditions(nil, []metav1.Condition{c(joulie.ReasonNoPerformanceSlot, "a")}, t0)
	t1 := t0.Add(time.Minute)
	out, changed := mergeConditions(out, []metav1.Condition{c(joulie.ReasonNoPerformanceSlot, "b")}, t1)
	if got := meta.FindStatusCondition(out, joulie.ConditionEcoProfile); len(changed) != 0 || !got.LastTransitionTime.Time.Equal(t0) || got.Message != "b" {
		t.Fatalf("message change: changed %v, condition %+v; want no change, lastTransitionTime %s, the new message", changed, got, t0)
	}
	t2 := t1.Add(time.Minute)
	out, changed = mergeConditions(out, []metav1.Condition{c(joulie.ReasonNoPerformanceDemand, "b")}, t2)
	if got := meta.FindStatusCondition(out, joulie.ConditionEcoProfile); len(changed) != 1 || !got.LastTransitionTime.Time.Equal(t2) || got.Reason != joulie.ReasonNoPerformanceDemand {
		t.Fatalf("reason change: changed %v, condition %+v; want a change at %s", changed, got, t2)
	}
}

// Two reconciles with the same decisions write the conditions once: the
// second status patch carries no conditions, so they keep their
// lastTransitionTime, and the spec is not rewritten. A changed decision
// is written in the next patch.
func TestSameDecisionsDoNotRewriteTheConditions(t *testing.T) {
	objs := []runtime.Object{governorNode("c0", "performance", 0), governorNode("c1", "performance", 0)}
	reader, client, dyn := fakeClients(t, objs...)
	var patches []map[string]any
	var specUpdates int
	dyn.PrependReactor("patch", "nodetwins", func(action clienttesting.Action) (bool, runtime.Object, error) {
		var p map[string]any
		if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &p); err != nil {
			t.Errorf("decode patch: %v", err)
		}
		patches = append(patches, p)
		return false, nil, nil
	})
	dyn.PrependReactor("update", "nodetwins", func(clienttesting.Action) (bool, runtime.Object, error) {
		specUpdates++
		return false, nil, nil
	})
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	run := func(perfPods ...runtime.Object) {
		t.Helper()
		if len(perfPods) > 0 {
			reader = fakeReader(t, append(append([]runtime.Object{}, objs...), perfPods...)...)
		}
		patches, specUpdates = nil, 0
		if err := reconcile(
			context.Background(), reader, client, dyn, selector,
			"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
			5000, 120, "static_partition", 0, 0, 1, 5, 10,
			100, 60, false, 100, 60, false,
			map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
		); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	conditionsIn := func(p map[string]any) []any {
		status, _ := p["status"].(map[string]any)
		c, _ := status["conditions"].([]any)
		return c
	}

	run()
	if len(patches) != 2 || conditionsIn(patches[0]) == nil || conditionsIn(patches[1]) == nil {
		t.Fatalf("first reconcile: %d status patches %v, want one per node, each with its conditions", len(patches), patches)
	}
	first, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "c1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored := first.Object["status"].(map[string]any)["conditions"]

	run()
	for _, p := range patches {
		if c := conditionsIn(p); c != nil {
			t.Errorf("second reconcile with the same decisions rewrote the conditions: %v", c)
		}
	}
	if specUpdates != 0 {
		t.Errorf("second reconcile updated the spec %d times, want none", specUpdates)
	}
	again, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "c1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(again.Object["status"].(map[string]any)["conditions"], stored) {
		t.Errorf("conditions changed without a changed decision:\n%v\nwant\n%v", again.Object["status"].(map[string]any)["conditions"], stored)
	}

	// A performance pod on c1 keeps it draining: the profile decision
	// changes, and the next patch carries it.
	run(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "default", Annotations: map[string]string{"joulie.io/workload-class": "performance"}},
		Spec:       corev1.PodSpec{NodeName: "c1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	if got := decisionsOf(t, dyn, "c1")[joulie.ConditionEcoProfile]; got != "False/"+joulie.ReasonDrainingPerformancePods {
		t.Errorf("c1 EcoProfile %q after a performance pod started, want False/%s", got, joulie.ReasonDrainingPerformancePods)
	}
}
