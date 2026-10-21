package simcluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

var (
	testRepo        = filepath.Join("..", "..", "..")
	testSmokeSpec   = filepath.Join(testRepo, "tests", "simcluster", "specs", "smoke.yaml")
	testMediumSpec  = filepath.Join(testRepo, "tests", "simcluster", "specs", "medium.yaml")
	testRenderFile  = filepath.Join("testdata", "daemonset-render.yaml")
	testManaged     = map[string]string{"joulie.io/managed": "true"}
	testPlacementOK = Placement{NodeSelector: testManaged, ImageArch: "amd64"}
)

// testNode is a managed amd64 Node with the taints.
func testNode(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"joulie.io/managed": "true", corev1.LabelArchStable: "amd64"}},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

func testTaint(key, value string, effect corev1.TaintEffect) corev1.Taint {
	return corev1.Taint{Key: key, Value: value, Effect: effect}
}

// testExpand loads and expands a spec with the builtin profiles.
func testExpand(t *testing.T, path string) []clusterspec.Member {
	t.Helper()
	s, err := clusterspec.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	return members
}

func testNodes(members []clusterspec.Member) []*corev1.Node {
	out := make([]*corev1.Node, len(members))
	for i, m := range members {
		out[i] = m.Node
	}
	return out
}

func testRenderPlacement(t *testing.T) Placement {
	t.Helper()
	b, err := os.ReadFile(testRenderFile)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlacementFromRender(b, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testPlacementCase runs EligibleNodes on nodes and checks the eligible
// names and each excluded Node's reason.
func testPlacementCase(t *testing.T, p Placement, nodes []*corev1.Node, wantEligible []string, wantExcluded map[string]string) {
	t.Helper()
	eligible, excluded := EligibleNodes(nodes, p)
	if !reflect.DeepEqual(eligible, wantEligible) {
		t.Errorf("eligible %v, want %v", eligible, wantEligible)
	}
	got := map[string]string{}
	for _, e := range excluded {
		got[e.Node] = e.Reason
	}
	if len(wantExcluded) == 0 {
		wantExcluded = map[string]string{}
	}
	if !reflect.DeepEqual(got, wantExcluded) {
		t.Errorf("excluded %v, want %v", got, wantExcluded)
	}
}

// PL01: the DaemonSet's node selector must match the Node's labels.
func TestPL01NodeSelectorMustMatch(t *testing.T) {
	unmanaged := testNode("unmanaged")
	unmanaged.Labels["joulie.io/managed"] = "false"
	unlabelled := testNode("unlabelled")
	delete(unlabelled.Labels, "joulie.io/managed")
	testPlacementCase(t, testPlacementOK, []*corev1.Node{testNode("managed"), unmanaged, unlabelled},
		[]string{"managed"},
		map[string]string{
			"unmanaged":  `node selector joulie.io/managed=true: label is "false"`,
			"unlabelled": "node selector joulie.io/managed=true: no label joulie.io/managed",
		})
}

// PL02: a NoSchedule taint excludes a Node unless the pod tolerates it.
func TestPL02NoScheduleTaintExcludesUntolerated(t *testing.T) {
	taint := testTaint("example.com/dedicated", "batch", corev1.TaintEffectNoSchedule)
	nodes := []*corev1.Node{testNode("tainted", taint), testNode("plain")}
	testPlacementCase(t, testPlacementOK, nodes, []string{"plain"},
		map[string]string{"tainted": "taint example.com/dedicated=batch:NoSchedule"})

	p := testPlacementOK
	p.Tolerations = []corev1.Toleration{{Key: "example.com/dedicated", Operator: corev1.TolerationOpExists}}
	testPlacementCase(t, p, nodes, []string{"plain", "tainted"}, nil)

	p.Tolerations = []corev1.Toleration{{Key: "example.com/dedicated", Operator: corev1.TolerationOpEqual, Value: "interactive", Effect: corev1.TaintEffectNoSchedule}}
	testPlacementCase(t, p, nodes, []string{"plain"},
		map[string]string{"tainted": "taint example.com/dedicated=batch:NoSchedule"})
}

// PL03: a NoExecute taint excludes a Node unless the pod tolerates that
// effect; a NoSchedule toleration is not enough.
func TestPL03NoExecuteTaintExcludesUntolerated(t *testing.T) {
	nodes := []*corev1.Node{testNode("evicting", testTaint("example.com/maintenance", "", corev1.TaintEffectNoExecute))}
	testPlacementCase(t, testPlacementOK, nodes, nil,
		map[string]string{"evicting": "taint example.com/maintenance:NoExecute"})

	p := testPlacementOK
	p.Tolerations = []corev1.Toleration{{Key: "example.com/maintenance", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	testPlacementCase(t, p, nodes, nil,
		map[string]string{"evicting": "taint example.com/maintenance:NoExecute"})

	p.Tolerations = []corev1.Toleration{{Key: "example.com/maintenance", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
	testPlacementCase(t, p, nodes, []string{"evicting"}, nil)
}

// PL04: a PreferNoSchedule taint never keeps a DaemonSet pod off a Node.
func TestPL04PreferNoScheduleDoesNotExclude(t *testing.T) {
	testPlacementCase(t, testPlacementOK,
		[]*corev1.Node{testNode("preferred", testTaint("example.com/spare", "", corev1.TaintEffectPreferNoSchedule))},
		[]string{"preferred"}, nil)
}

// PL05: the KWOK taint every fake Node carries is ignored, whatever its
// effect.
func TestPL05KWOKTaintIgnored(t *testing.T) {
	testPlacementCase(t, testPlacementOK,
		[]*corev1.Node{
			testNode("kwok-0", testTaint(clusterspec.KWOKTaintKey, clusterspec.KWOKValue, corev1.TaintEffectNoSchedule)),
			testNode("kwok-1", testTaint(clusterspec.KWOKTaintKey, clusterspec.KWOKValue, corev1.TaintEffectNoExecute)),
		},
		[]string{"kwok-0", "kwok-1"}, nil)
}

// PL06: the DaemonSet controller's own tolerations: a cordoned or
// not-ready Node stays eligible, and network-unavailable is tolerated only
// for a pod on the host network.
func TestPL06DaemonSetTolerations(t *testing.T) {
	cordoned := testNode("cordoned", testTaint(corev1.TaintNodeUnschedulable, "", corev1.TaintEffectNoSchedule))
	cordoned.Spec.Unschedulable = true
	notReady := testNode("not-ready", testTaint(corev1.TaintNodeNotReady, "", corev1.TaintEffectNoExecute))
	pressure := testNode("pressure",
		testTaint(corev1.TaintNodeDiskPressure, "", corev1.TaintEffectNoSchedule),
		testTaint(corev1.TaintNodeMemoryPressure, "", corev1.TaintEffectNoSchedule),
		testTaint(corev1.TaintNodePIDPressure, "", corev1.TaintEffectNoSchedule),
		testTaint(corev1.TaintNodeUnreachable, "", corev1.TaintEffectNoExecute))
	network := testNode("no-network", testTaint(corev1.TaintNodeNetworkUnavailable, "", corev1.TaintEffectNoSchedule))
	nodes := []*corev1.Node{cordoned, notReady, pressure, network}

	testPlacementCase(t, testPlacementOK, nodes, []string{"cordoned", "not-ready", "pressure"},
		map[string]string{"no-network": "taint node.kubernetes.io/network-unavailable:NoSchedule"})
	p := testPlacementOK
	p.HostNetwork = true
	testPlacementCase(t, p, nodes, []string{"cordoned", "no-network", "not-ready", "pressure"}, nil)

	if n := len(DaemonSetTolerations(true)); n != 7 {
		t.Errorf("%d tolerations with the host network, want 7", n)
	}
	if n := len(DaemonSetTolerations(false)); n != 6 {
		t.Errorf("%d tolerations without the host network, want 6", n)
	}
}

// PL07: a Node of another architecture than the image is excluded; the
// architecture comes from the kubernetes.io/arch label, else the status.
func TestPL07ArchitectureMustMatchTheImage(t *testing.T) {
	arm := testNode("arm")
	arm.Labels[corev1.LabelArchStable] = "arm64"
	statusOnly := testNode("status-only")
	delete(statusOnly.Labels, corev1.LabelArchStable)
	statusOnly.Status.NodeInfo.Architecture = "amd64"
	unknown := testNode("unknown")
	delete(unknown.Labels, corev1.LabelArchStable)
	testPlacementCase(t, testPlacementOK, []*corev1.Node{arm, statusOnly, unknown}, []string{"status-only"},
		map[string]string{
			"arm":     "architecture arm64, the agent image is amd64",
			"unknown": "architecture unknown, the agent image is amd64",
		})

	// Every reason of a Node is given.
	both := testNode("both", testTaint("example.com/x", "", corev1.TaintEffectNoSchedule))
	both.Labels[corev1.LabelArchStable] = "arm64"
	testPlacementCase(t, testPlacementOK, []*corev1.Node{both}, nil,
		map[string]string{"both": "taint example.com/x:NoSchedule; architecture arm64, the agent image is amd64"})
}

// PL08: with the chart's real DaemonSet render, which tolerates every taint,
// smoke's intel-tainted-0 and its GPU Nodes, tainted by their vendor, are
// eligible (D15), and intel-arm64-0 is the one Node left out, for its
// architecture alone.
func TestPL08SmokeLeavesOutOnlyTheOtherArchitecture(t *testing.T) {
	eligible, excluded := EligibleNodes(testNodes(testExpand(t, testSmokeSpec)), testRenderPlacement(t))
	for _, want := range []string{"intel-tainted-0", "mi300x-0", "nvl-0", "sxm-nopm-0"} {
		if !slices.Contains(eligible, want) {
			t.Errorf("%s is not eligible (D15); excluded: %v", want, excluded)
		}
	}
	want := []Exclusion{{Node: "intel-arm64-0", Reason: "architecture arm64, the agent image is amd64"}}
	if !reflect.DeepEqual(excluded, want) {
		t.Errorf("excluded %v, want %v", excluded, want)
	}
}

// PL09: the agent DaemonSet the chart renders today, with the values of the
// simulated cluster, reaches 48 of medium's 50 Nodes: all but the unmanaged
// pool. It renders the chart
// with helm, as the scripts do, and skips without helm. A medium.yaml
// that does not expand fails it: up.sh and simcluster validate refuse such
// a spec too, and no other test loads it through clusterspec.
func TestPL09MediumGives48EligibleNodes(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH")
	}
	s, err := clusterspec.LoadFile(testMediumSpec)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatalf("medium.yaml does not expand, so the harness cannot run it: %v", err)
	}
	cmd := exec.Command(helm, "template", "joulie", filepath.Join(testRepo, "charts", "joulie"), "-n", "joulie-system",
		"-f", filepath.Join(testRepo, "values", "simcluster.yaml"),
		"--set", "agent.mode=daemonset", "--show-only", "templates/agent-daemonset.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
	render, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	p, err := PlacementFromRender(render, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	eligible, excluded := EligibleNodes(testNodes(members), p)
	if len(members) != 50 || len(eligible) != 48 {
		var lines []string
		for _, e := range excluded {
			lines = append(lines, e.Node+": "+e.Reason)
		}
		t.Errorf("medium: %d of %d Nodes eligible, want 48 of 50; excluded:\n  %s", len(eligible), len(members), strings.Join(lines, "\n  "))
	}
}

// PlacementFromRender reads the pod's node selector, tolerations and host
// network from the render, and refuses a render it cannot model.
func TestPlacementFromRender(t *testing.T) {
	p := testRenderPlacement(t)
	want := Placement{NodeSelector: testManaged, Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, HostNetwork: true, ImageArch: "amd64"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("placement %+v, want %+v", p, want)
	}

	ds := func(extra string) string {
		return "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: a}\nspec:\n  template:\n    spec:\n      tolerations: [{key: k, operator: Exists}]\n" + extra
	}
	got, err := PlacementFromRender([]byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n---\n"+ds("")), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tolerations) != 1 || got.Tolerations[0].Key != "k" || got.ImageArch != "arm64" {
		t.Errorf("placement %+v, want the toleration k and arch arm64", got)
	}
	// The runtime class and the volumes beyond the chart's own: the GPU
	// tools reach the agent through either.
	got, err = PlacementFromRender([]byte(ds("      runtimeClassName: nvidia\n      volumes: [{name: host-sys, emptyDir: {}}, {name: hardware-catalog, emptyDir: {}}, {name: nvidia-driver, emptyDir: {}}]\n")), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeClassName != "nvidia" || !reflect.DeepEqual(got.ExtraVolumes, []string{"nvidia-driver"}) {
		t.Errorf("placement %+v, want runtime class nvidia and the extra volume nvidia-driver only", got)
	}
	for name, tc := range map[string]struct{ render, arch, want string }{
		"no arch":    {ds(""), "", "no image architecture"},
		"none":       {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n", "amd64", "holds 0 DaemonSets"},
		"two":        {ds("") + "---\n" + ds(""), "amd64", "holds 2 DaemonSets"},
		"affinity":   {ds("      affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: []}}}\n"), "amd64", "required node affinity"},
		"bad fields": {ds("      bogus: 1\n"), "amd64", "bogus"},
	} {
		if _, err := PlacementFromRender([]byte(tc.render), tc.arch); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
		}
	}
}

// PL10: the chart's agent DaemonSet, rendered with helm, delivers the GPU
// tools exactly when agent.runtimeClassName or agent.extraVolumes is set;
// its own volumes, the hardware catalog's included, deliver none.
func TestPL10ToolDeliveryFromTheChartRender(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH")
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"defaults":          {nil, ToolDeliveryNone},
		"hardware catalog":  {[]string{"--set", "hardwareCatalog.enabled=true"}, ToolDeliveryNone},
		"runtime class":     {[]string{"--set", "agent.runtimeClassName=nvidia"}, ToolDeliveryMounted},
		"extra volumes":     {[]string{"--set-json", `agent.extraVolumes=[{"name":"nvidia-driver","hostPath":{"path":"/usr/lib/nvidia"}}]`}, ToolDeliveryMounted},
		"simcluster values": {[]string{"-f", filepath.Join(testRepo, "values", "simcluster.yaml")}, ToolDeliveryNone},
	} {
		args := append([]string{"template", "joulie", filepath.Join(testRepo, "charts", "joulie"), "-n", "joulie-system",
			"--set", "agent.mode=daemonset", "--show-only", "templates/agent-daemonset.yaml"}, tc.args...)
		cmd := exec.Command(helm, args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
		render, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: helm template: %v", name, err)
		}
		p, err := PlacementFromRender(render, "amd64")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := p.ToolDelivery(); got != tc.want {
			t.Errorf("%s: tool delivery %s (runtime class %q, extra volumes %v), want %s", name, got, p.RuntimeClassName, p.ExtraVolumes, tc.want)
		}
	}
}
