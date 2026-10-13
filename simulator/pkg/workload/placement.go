package workload

import (
	"bytes"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// profileEco is the power profile a performance pod must avoid; it equals
// the controller's fsm.ProfileEco.
const profileEco = "eco"

// Placement is where a trace job may run: the affinity, nodeSelector and
// tolerations of its podTemplate, as corev1 types. v1 keeps all three on the
// pod; legacy never copied them.
type Placement struct {
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
}

// ParsePlacement reads the placement of a trace record's podTemplate, the
// decoded JSON object. Its other keys, such as requests, are left to the
// loader. An unknown field inside the three, or a value of the wrong type,
// is an error, so a misspelled term never silently widens where a pod may
// run. An empty affinity object reads as no affinity.
func ParsePlacement(podTemplate map[string]any) (Placement, error) {
	var p Placement
	sub := map[string]any{}
	for _, k := range []string{"affinity", "nodeSelector", "tolerations"} {
		if v, ok := podTemplate[k]; ok && v != nil {
			sub[k] = v
		}
	}
	if len(sub) == 0 {
		return p, nil
	}
	b, err := json.Marshal(sub)
	if err != nil {
		return Placement{}, fmt.Errorf("podTemplate placement: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Placement{}, fmt.Errorf("podTemplate placement: %w", err)
	}
	if a := p.Affinity; a != nil && a.NodeAffinity == nil && a.PodAffinity == nil && a.PodAntiAffinity == nil {
		p.Affinity = nil
	}
	return p, nil
}

// MergeIntent returns the affinity a v1 pod of class gets under intent: a
// copy of the trace's aff and, for a performance pod under IntentAffinity or
// IntentBoth, a NotIn-eco requirement on LabelPowerProfile ANDed into every
// required node selector term. Required terms are ORed, so a requirement
// added as a term of its own would leave eco nodes reachable through every
// other term. Without required node affinity the requirement becomes the one
// term. A term that already excludes eco is kept as it is, and so is an
// empty term, which matches no node and must not start matching. aff is not
// modified.
func MergeIntent(aff *corev1.Affinity, class string, intent Intent) *corev1.Affinity {
	out := aff.DeepCopy()
	if class != ClassPerformance || (intent != IntentAffinity && intent != IntentBoth) {
		return out
	}
	if out == nil {
		out = &corev1.Affinity{}
	}
	if out.NodeAffinity == nil {
		out.NodeAffinity = &corev1.NodeAffinity{}
	}
	req := out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		req = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
		out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = req
		req.NodeSelectorTerms[0].MatchExpressions = []corev1.NodeSelectorRequirement{notInEco()}
		return out
	}
	for i := range req.NodeSelectorTerms {
		term := &req.NodeSelectorTerms[i]
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		if termExcludesEco(*term) {
			continue
		}
		term.MatchExpressions = append(term.MatchExpressions, notInEco())
	}
	return out
}

func notInEco() corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: LabelPowerProfile, Operator: corev1.NodeSelectorOpNotIn, Values: []string{profileEco}}
}

func termExcludesEco(term corev1.NodeSelectorTerm) bool {
	for _, e := range term.MatchExpressions {
		if e.Key != LabelPowerProfile || e.Operator != corev1.NodeSelectorOpNotIn {
			continue
		}
		for _, v := range e.Values {
			if v == profileEco {
				return true
			}
		}
	}
	return false
}

// DefaultNodeSelector is SIM_WORKLOAD_NODE_SELECTOR unset: the nodes of the
// cluster spec named cluster, so no workload pod lands on the infra node,
// which runs a real kubelet. It is nil for an empty name.
func DefaultNodeSelector(cluster string) map[string]string {
	if cluster == "" {
		return nil
	}
	return map[string]string{clusterspec.LabelCluster: cluster}
}

// KWOKToleration is SIM_WORKLOAD_TOLERATIONS unset: it tolerates the
// NoSchedule taint on every KWOK node. A real cluster's pods would not carry
// it; it is the one placement the simulator adds that a user would not.
func KWOKToleration() corev1.Toleration {
	return corev1.Toleration{
		Key:      clusterspec.KWOKTaintKey,
		Operator: corev1.TolerationOpEqual,
		Value:    clusterspec.KWOKValue,
		Effect:   corev1.TaintEffectNoSchedule,
	}
}

// mergePlacement is a v1 pod's placement: the trace's p, plus the options'
// node selector keys the trace does not set and the options' tolerations it
// does not already carry, with the intent merged into the affinity. Where the
// trace and the options set one selector key, the trace's value is kept, as
// the trace's placement is the user's.
func mergePlacement(p Placement, class string, o BuildOptions) Placement {
	out := Placement{Affinity: MergeIntent(p.Affinity, class, o.intent())}
	if len(p.NodeSelector)+len(o.NodeSelector) > 0 {
		out.NodeSelector = make(map[string]string, len(p.NodeSelector)+len(o.NodeSelector))
		for k, v := range o.NodeSelector {
			out.NodeSelector[k] = v
		}
		for k, v := range p.NodeSelector {
			out.NodeSelector[k] = v
		}
	}
	for _, t := range p.Tolerations {
		out.Tolerations = append(out.Tolerations, *t.DeepCopy())
	}
	for _, t := range o.Tolerations {
		if !hasToleration(out.Tolerations, t) {
			out.Tolerations = append(out.Tolerations, *t.DeepCopy())
		}
	}
	return out
}

func hasToleration(list []corev1.Toleration, t corev1.Toleration) bool {
	for i := range list {
		if list[i].MatchToleration(&t) && ptrEqual(list[i].TolerationSeconds, t.TolerationSeconds) {
			return true
		}
	}
	return false
}

func ptrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
