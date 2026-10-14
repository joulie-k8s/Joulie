package workload_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

func traceAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a", "b"}},
				}},
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpExists}},
					MatchFields:      []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}},
				},
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"n2"}}}},
			}},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 10, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "rack", Operator: corev1.NodeSelectorOpIn, Values: []string{"r1"}}}},
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}}},
	}
}

// Required node selector terms are ORed, so a performance pod excludes eco
// nodes only if every term does: each trace term keeps its own requirements,
// in order, and gains one NotIn-eco requirement, while preferences, pod
// (anti-)affinity and the caller's object stay as they were.
func TestMergeIntentKeepsTraceTerms(t *testing.T) {
	in := traceAffinity()
	orig := in.DeepCopy()
	for _, intent := range []workload.Intent{workload.IntentAffinity, workload.IntentBoth} {
		out := workload.MergeIntent(in, workload.ClassPerformance, intent)
		if !reflect.DeepEqual(in, orig) {
			t.Fatalf("%s: MergeIntent modified its argument", intent)
		}
		terms := out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		origTerms := orig.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != len(origTerms) {
			t.Fatalf("%s: %d required terms, want the trace's %d (an added term would be ORed)", intent, len(terms), len(origTerms))
		}
		for i, term := range terms {
			want := origTerms[i]
			if !reflect.DeepEqual(term.MatchFields, want.MatchFields) {
				t.Errorf("%s term %d: matchFields %v, want %v", intent, i, term.MatchFields, want.MatchFields)
			}
			n := len(want.MatchExpressions)
			if len(term.MatchExpressions) != n+1 || (n > 0 && !reflect.DeepEqual(term.MatchExpressions[:n], want.MatchExpressions)) {
				t.Errorf("%s term %d: expressions %v, want %v plus one", intent, i, term.MatchExpressions, want.MatchExpressions)
				continue
			}
			got := term.MatchExpressions[n]
			if got.Key != fsm.PowerProfileLabelKey || got.Operator != corev1.NodeSelectorOpNotIn || !reflect.DeepEqual(got.Values, []string{fsm.ProfileEco}) {
				t.Errorf("%s term %d: added %+v, want %s NotIn [%s]", intent, i, got, fsm.PowerProfileLabelKey, fsm.ProfileEco)
			}
		}
		if !reflect.DeepEqual(out.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, orig.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) ||
			!reflect.DeepEqual(out.PodAntiAffinity, orig.PodAntiAffinity) {
			t.Errorf("%s: preferences or pod anti-affinity changed", intent)
		}
		pod := &corev1.Pod{Spec: corev1.PodSpec{Affinity: out}}
		if !fsm.PodExcludesEco(pod) {
			t.Errorf("%s: the controller does not see the merged affinity as excluding eco", intent)
		}
	}
}

// Without required node affinity the requirement becomes the one term; a
// term that already excludes eco is not given a second requirement; an empty
// term, which matches no node, is not turned into one that matches; and a
// standard pod, or a performance pod under an intent without affinity, gets
// an unchanged copy.
func TestMergeIntentEdges(t *testing.T) {
	out := workload.MergeIntent(nil, workload.ClassPerformance, workload.IntentAffinity)
	terms := out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 || terms[0].MatchExpressions[0].Key != fsm.PowerProfileLabelKey {
		t.Errorf("nil affinity merges to %+v, want one NotIn-eco term", terms)
	}

	already := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: fsm.PowerProfileLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"eco"}}}},
			{},
		},
	}}}
	out = workload.MergeIntent(already, workload.ClassPerformance, workload.IntentBoth)
	if !reflect.DeepEqual(out, already) {
		t.Errorf("an affinity that already excludes eco became %+v", out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	}

	for _, c := range []struct {
		class  string
		intent workload.Intent
	}{
		{workload.ClassStandard, workload.IntentAffinity},
		{workload.ClassStandard, workload.IntentBoth},
		{workload.ClassPerformance, workload.IntentAnnotation},
		{workload.ClassPerformance, workload.IntentNone},
	} {
		in := traceAffinity()
		out := workload.MergeIntent(in, c.class, c.intent)
		if !reflect.DeepEqual(out, in) || out == in {
			t.Errorf("%s under %s: got %+v, want an unchanged copy", c.class, c.intent, out)
		}
		if got := workload.MergeIntent(nil, c.class, c.intent); got != nil {
			t.Errorf("%s under %s: nil affinity became %+v", c.class, c.intent, got)
		}
	}
}

// A trace's podTemplate is decoded JSON; its placement reads into corev1
// types, its requests are left to the loader, and a misspelled or mistyped
// field is an error rather than a silently wider placement.
func TestParsePlacement(t *testing.T) {
	var tpl map[string]any
	raw := `{"requests":{"cpu":"2"},
		"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"zone","operator":"In","values":["a"]}]}]}}},
		"nodeSelector":{"pool":"gpu"},
		"tolerations":[{"key":"dedicated","operator":"Equal","value":"ml","effect":"NoSchedule"}]}`
	if err := json.Unmarshal([]byte(raw), &tpl); err != nil {
		t.Fatal(err)
	}
	p, err := workload.ParsePlacement(tpl)
	if err != nil {
		t.Fatalf("ParsePlacement: %v", err)
	}
	terms := p.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchExpressions[0].Key != "zone" || terms[0].MatchExpressions[0].Values[0] != "a" {
		t.Errorf("affinity %+v", terms)
	}
	if !reflect.DeepEqual(p.NodeSelector, map[string]string{"pool": "gpu"}) {
		t.Errorf("nodeSelector %v", p.NodeSelector)
	}
	if len(p.Tolerations) != 1 || p.Tolerations[0].Key != "dedicated" || p.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations %+v", p.Tolerations)
	}

	for _, c := range []struct{ name, raw string }{
		{"misspelled term", `{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerm":[]}}}}`},
		{"wrong type", `{"nodeSelector":["pool=gpu"]}`},
		{"unknown toleration field", `{"tolerations":[{"key":"k","efect":"NoSchedule"}]}`},
	} {
		var tpl map[string]any
		if err := json.Unmarshal([]byte(c.raw), &tpl); err != nil {
			t.Fatal(err)
		}
		if _, err := workload.ParsePlacement(tpl); err == nil || !strings.Contains(err.Error(), "podTemplate placement") {
			t.Errorf("%s: error %v, want a podTemplate placement error", c.name, err)
		}
	}

	for _, empty := range []map[string]any{nil, {}, {"affinity": map[string]any{}}, {"affinity": nil, "requests": map[string]any{"cpu": "1"}}} {
		p, err := workload.ParsePlacement(empty)
		if err != nil || p.Affinity != nil || p.NodeSelector != nil || p.Tolerations != nil {
			t.Errorf("ParsePlacement(%v) = %+v, %v; want an empty placement", empty, p, err)
		}
	}
}

// The v1 defaults keep workload pods on the spec's emulated nodes: the
// selector names the cluster label every spec node carries, and the
// toleration tolerates the taint every KWOK node carries.
func TestPlacementDefaults(t *testing.T) {
	if got := workload.DefaultNodeSelector("smoke"); !reflect.DeepEqual(got, map[string]string{clusterspec.LabelCluster: "smoke"}) {
		t.Errorf("DefaultNodeSelector = %v", got)
	}
	if got := workload.DefaultNodeSelector(""); got != nil {
		t.Errorf("DefaultNodeSelector(\"\") = %v, want nil", got)
	}
	tol := workload.KWOKToleration()
	taint := corev1.Taint{Key: clusterspec.KWOKTaintKey, Value: clusterspec.KWOKValue, Effect: corev1.TaintEffectNoSchedule}
	if !tol.ToleratesTaint(&taint) {
		t.Errorf("KWOKToleration %+v does not tolerate %+v", tol, taint)
	}
}
