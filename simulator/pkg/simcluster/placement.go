package simcluster

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// DaemonSetTolerations returns the tolerations the DaemonSet controller adds
// to every pod it creates, whatever the pod spec says
// (AddOrUpdateDaemonPodTolerations in k8s.io/kubernetes
// pkg/controller/daemon/util): not-ready and unreachable for NoExecute;
// disk-pressure, memory-pressure, pid-pressure and unschedulable for
// NoSchedule; and network-unavailable for NoSchedule only when the pod uses
// the host network. A cordoned Node therefore still runs DaemonSet pods.
func DaemonSetTolerations(hostNetwork bool) []corev1.Toleration {
	exists := func(key string, effect corev1.TaintEffect) corev1.Toleration {
		return corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: effect}
	}
	out := []corev1.Toleration{
		exists(corev1.TaintNodeNotReady, corev1.TaintEffectNoExecute),
		exists(corev1.TaintNodeUnreachable, corev1.TaintEffectNoExecute),
		exists(corev1.TaintNodeDiskPressure, corev1.TaintEffectNoSchedule),
		exists(corev1.TaintNodeMemoryPressure, corev1.TaintEffectNoSchedule),
		exists(corev1.TaintNodePIDPressure, corev1.TaintEffectNoSchedule),
		exists(corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule),
	}
	if hostNetwork {
		out = append(out, exists(corev1.TaintNodeNetworkUnavailable, corev1.TaintEffectNoSchedule))
	}
	return out
}

// PlacementFromRender reads the Placement of the agent DaemonSet from
// render, the output of
//
//	helm template ... --show-only templates/agent-daemonset.yaml
//
// with the values files of the install. render must hold exactly one
// DaemonSet; other documents are ignored. imageArch is the architecture the
// agent image is built for, such as amd64: a render does not say it. A pod
// with required node affinity is refused, since Placement does not model it
// and the result would be wrong.
func PlacementFromRender(render []byte, imageArch string) (Placement, error) {
	if imageArch == "" {
		return Placement{}, errors.New("placement: no image architecture given")
	}
	var found []appsv1.DaemonSet
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(render)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Placement{}, fmt.Errorf("placement: %w", err)
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			return Placement{}, fmt.Errorf("placement: %w", err)
		}
		if meta.Kind != "DaemonSet" {
			continue
		}
		var ds appsv1.DaemonSet
		if err := yaml.UnmarshalStrict(doc, &ds); err != nil {
			return Placement{}, fmt.Errorf("placement: DaemonSet: %w", err)
		}
		found = append(found, ds)
	}
	if len(found) != 1 {
		return Placement{}, fmt.Errorf("placement: the render holds %d DaemonSets, want 1 (helm template --show-only templates/agent-daemonset.yaml)", len(found))
	}
	pod := found[0].Spec.Template.Spec
	if a := pod.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		return Placement{}, fmt.Errorf("placement: DaemonSet %s has required node affinity, which placement does not model", found[0].Name)
	}
	return Placement{
		NodeSelector: pod.NodeSelector,
		Tolerations:  pod.Tolerations,
		HostNetwork:  pod.HostNetwork,
		ImageArch:    imageArch,
	}, nil
}

// EligibleNodes splits nodes into those the agent DaemonSet would run on and
// those it would not, as the DaemonSet controller decides it:
//
//   - the pod's node selector must match the Node's labels;
//   - every NoSchedule and NoExecute taint must be tolerated by the pod's
//     tolerations or by DaemonSetTolerations; a PreferNoSchedule taint never
//     keeps a DaemonSet pod off, and the KWOK taint (clusterspec.KWOKTaintKey)
//     is ignored, since it only keeps infrastructure pods off fake Nodes;
//   - the Node's architecture (kubernetes.io/arch, else
//     status.nodeInfo.architecture) must be the image's; an empty
//     Placement.ImageArch accepts any.
//
// A cordoned Node (spec.unschedulable) stays eligible. eligible holds Node
// names, sorted; each Exclusion gives every reason its Node is left out,
// joined by "; ", such as "taint example.com/dedicated=batch:NoSchedule".
func EligibleNodes(nodes []*corev1.Node, p Placement) (eligible []string, excluded []Exclusion) {
	tolerations := append(append([]corev1.Toleration(nil), p.Tolerations...), DaemonSetTolerations(p.HostNetwork)...)
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if reasons := placementReasons(n, p, tolerations); len(reasons) > 0 {
			excluded = append(excluded, Exclusion{Node: n.Name, Reason: strings.Join(reasons, "; ")})
			continue
		}
		eligible = append(eligible, n.Name)
	}
	sort.Strings(eligible)
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Node < excluded[j].Node })
	return eligible, excluded
}

// placementReasons returns why the DaemonSet would not run on n, nothing
// when it would.
func placementReasons(n *corev1.Node, p Placement, tolerations []corev1.Toleration) []string {
	var reasons []string
	keys := make([]string, 0, len(p.NodeSelector))
	for k := range p.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := p.NodeSelector[k]
		got, ok := n.Labels[k]
		switch {
		case !ok:
			reasons = append(reasons, fmt.Sprintf("node selector %s=%s: no label %s", k, want, k))
		case got != want:
			reasons = append(reasons, fmt.Sprintf("node selector %s=%s: label is %q", k, want, got))
		}
	}
	for i := range n.Spec.Taints {
		taint := &n.Spec.Taints[i]
		if taint.Key == clusterspec.KWOKTaintKey {
			continue
		}
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !placementTolerated(taint, tolerations) {
			reasons = append(reasons, "taint "+taint.ToString())
		}
	}
	if p.ImageArch != "" {
		arch := n.Labels[corev1.LabelArchStable]
		if arch == "" {
			arch = n.Status.NodeInfo.Architecture
		}
		if arch != p.ImageArch {
			if arch == "" {
				arch = "unknown"
			}
			reasons = append(reasons, fmt.Sprintf("architecture %s, the agent image is %s", arch, p.ImageArch))
		}
	}
	return reasons
}

func placementTolerated(taint *corev1.Taint, tolerations []corev1.Toleration) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}

// Agents returns the emulated agents of the members the agent DaemonSet
// would reach under p, and the members it would not, with their reasons.
// See EligibleNodes and EmulatedAgents.
func Agents(members []clusterspec.Member, p Placement, h Harness, root string) ([]EmulatedAgent, []Exclusion, error) {
	nodes := make([]*corev1.Node, 0, len(members))
	for _, m := range members {
		if m.Node == nil {
			return nil, nil, fmt.Errorf("member %s has no Node", m.Name)
		}
		nodes = append(nodes, m.Node)
	}
	names, excluded := EligibleNodes(nodes, p)
	in := make(map[string]bool, len(names))
	for _, n := range names {
		in[n] = true
	}
	eligible := make([]clusterspec.Member, 0, len(names))
	for _, m := range members {
		if in[m.Node.Name] {
			eligible = append(eligible, m)
		}
	}
	agents, err := EmulatedAgents(eligible, h, root)
	if err != nil {
		return nil, nil, err
	}
	return agents, excluded, nil
}
