package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// twinObj is a NodeTwin as the API server sends it: whole numbers as int64.
func twinObj(name, class string, status map[string]interface{}) *unstructured.Unstructured {
	st := map[string]interface{}{"schedulableClass": class}
	for k, v := range status {
		st[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"nodeName": name, "profile": class},
		"status":     st,
	}}
}

func hwObj(name string, cpu, gpu map[string]interface{}) *unstructured.Unstructured {
	st := map[string]interface{}{"cpu": cpu}
	if gpu != nil {
		st["gpu"] = gpu
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeHardware",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"nodeName": name},
		"status":     st,
	}}
}

// node is a Node advertising cpu CPUs and the given extra resources.
func node(name, cpu string, res map[string]string) *corev1.Node {
	alloc := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("512Gi")}
	for k, v := range res {
		alloc[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: alloc}}
}

// pod is a running pod on node requesting the given resources; class
// "performance" sets the workload-class annotation.
func pod(name, nodeName, class string, res map[string]string) *corev1.Pod {
	req := corev1.ResourceList{}
	for k, v := range res {
		req[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
			Name: "c", Resources: corev1.ResourceRequirements{Requests: req, Limits: req},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if class == podClassPerformance {
		p.Annotations = map[string]string{"joulie.io/workload-class": "performance"}
	}
	return p
}

func fakeClients(t *testing.T, kubeObjs []runtime.Object, crs ...runtime.Object) clients {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeTwinGVR:     "NodeTwinList",
		nodeHardwareGVR: "NodeHardwareList",
	}, crs...)
	return clients{dyn: dyn, kube: fake.NewSimpleClientset(kubeObjs...)}
}

// plain renders a styled string as the terminal shows it without colors.
func plain(s string) string { return stripANSI(s) }

// The whole road of a reading: the API server sends whole numbers as
// int64, readCluster decodes them through the typed NodeTwin and
// NodeHardware, and they reach the status screen. An unstructured read with
// a float64 assertion would show these nodes at 0 W.
func TestStatusShowsWholeNumberReadingsFromTheAPIServer(t *testing.T) {
	c := fakeClients(t,
		[]runtime.Object{
			node("xeon-0", "128", nil),
			node("h100-0", "96", map[string]string{"nvidia.com/gpu": "8"}),
			pod("sim", "xeon-0", podClassPerformance, map[string]string{"cpu": "32"}),
			pod("train", "h100-0", podClassStandard, map[string]string{"cpu": "16", "nvidia.com/gpu": "4"}),
		},
		twinObj("xeon-0", "performance", map[string]interface{}{
			"powerMeasurement":            map[string]interface{}{"source": "prometheus", "measuredNodePowerW": int64(412), "nodeCappedPowerW": int64(540), "nodeTdpW": int64(540)},
			"effectiveCapState":           map[string]interface{}{"cpuPct": int64(100)},
			"predictedPowerHeadroomScore": int64(70),
			"conditions": []interface{}{map[string]interface{}{
				"type": "AgentReporting", "status": "True", "reason": "NodeHardwareCurrent", "lastTransitionTime": "2026-10-21T15:00:00Z", "message": "",
			}},
		}),
		twinObj("h100-0", "eco", map[string]interface{}{
			"powerMeasurement":  map[string]interface{}{"source": "prometheus", "measuredNodePowerW": int64(2800), "nodeCappedPowerW": int64(3690), "nodeTdpW": int64(5930)},
			"effectiveCapState": map[string]interface{}{"cpuPct": int64(100), "gpuPct": int64(60)},
		}),
		hwObj("xeon-0", map[string]interface{}{"model": "INTEL_XEON_GOLD_6530", "sockets": int64(2), "totalCores": int64(128)}, nil),
		hwObj("h100-0", map[string]interface{}{"model": "INTEL_XEON_PLATINUM_8260", "sockets": int64(2)},
			map[string]interface{}{"present": true, "model": "NVIDIA_H100_SXM", "count": int64(8)}),
	)
	snap, err := readCluster(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(snap.nodes))
	}
	h100, xeon := snap.nodes[0], snap.nodes[1]
	if xeon.measuredW != 412 || xeon.cappedW != 540 || xeon.headroom != 70 || !xeon.agent.reporting {
		t.Errorf("xeon-0 read as %+v", xeon)
	}
	if h100.gpuCapPct != 60 || h100.gpus != 8 || h100.gpuAllocPct != 50 || !h100.hasGPU {
		t.Errorf("h100-0 read as gpuCap %v gpus %d gpuAlloc %v", h100.gpuCapPct, h100.gpus, h100.gpuAllocPct)
	}
	if xeon.perfPods != 1 || xeon.cpuAllocPct != 25 {
		t.Errorf("xeon-0: perfPods %d cpuAlloc %v, want 1 and 25", xeon.perfPods, xeon.cpuAllocPct)
	}

	out := plain(statusView(snap, "ctx", 0, -1, nil, nil))
	for _, want := range []string{"412 W", "2.80 kW", "2× Xeon Gold 6530", "8× H100 SXM", "● eco", "● performance", "3.21 kW", "1 ▲1", "agents 1/1 reporting"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

// The GPU columns count the GPUs of every node that advertises any GPU
// resource pkg/api classifies, and the share of them pods requested, in
// physical GPUs. A DRA node advertises no GPU resource: its GPU cap shows
// the cap its twin's spec carries, and its GPU column has nothing to count.
func TestStatusGPUColumnsShowSharedAndPartitionedGPUs(t *testing.T) {
	nodes := []*corev1.Node{
		node("whole", "96", map[string]string{"nvidia.com/gpu": "8"}),
		node("mig", "96", map[string]string{"nvidia.com/mig-1g.12gb": "56"}),
		node("shared", "96", map[string]string{"nvidia.com/gpu.shared": "32"}),
		node("amd-cpx", "96", map[string]string{"amd.com/cpx_nps4": "64"}),
		node("cpu", "96", nil),
		node("dra", "96", nil),
	}
	pods := []*corev1.Pod{
		pod("a", "whole", "", map[string]string{"nvidia.com/gpu": "2"}),
		pod("b", "mig", "", map[string]string{"nvidia.com/mig-1g.12gb": "7"}),
		pod("c", "shared", "", map[string]string{"nvidia.com/gpu.shared": "8"}),
		pod("d", "amd-cpx", "", map[string]string{"amd.com/cpx_nps4": "32"}),
	}
	var crs []runtime.Object
	for _, n := range nodes {
		tw := twinObj(n.Name, "eco", map[string]interface{}{"effectiveCapState": map[string]interface{}{"cpuPct": int64(100), "gpuPct": int64(60)}})
		if n.Name == "dra" {
			tw.Object["spec"].(map[string]interface{})["gpu"] = map[string]interface{}{"powerCap": map[string]interface{}{"scope": "perGpu", "capPctOfMax": int64(60)}}
		}
		crs = append(crs, tw)
	}
	var objs []runtime.Object
	for _, n := range nodes {
		objs = append(objs, n)
	}
	for _, p := range pods {
		objs = append(objs, p)
	}
	snap, err := readCluster(context.Background(), fakeClients(t, objs, crs...), "")
	if err != nil {
		t.Fatal(err)
	}
	wantGPU := map[string]string{"whole": "25%", "mig": "12%", "shared": "25%", "amd-cpx": "50%", "dra": "-", "cpu": "-"}
	for _, n := range snap.nodes {
		gpu := plain(allocCell(n.gpuAllocPct, n.hasGPU, stGPU))
		capGPU := strings.Split(plain(capCell(n)), " · ")[1]
		wantCap := "60%"
		if n.name == "cpu" {
			wantCap = "-"
		}
		if gpu != wantGPU[n.name] || capGPU != wantCap {
			t.Errorf("node %s: GPU %s and GPU cap %s, want %s and %s", n.name, gpu, capGPU, wantGPU[n.name], wantCap)
		}
	}
}

// Without Joulie's CRDs the plugin says so instead of printing an API error.
func TestReadClusterSaysWhenJoulieIsNotInstalled(t *testing.T) {
	c := fakeClients(t, nil)
	c.dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "nodetwins", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "joulie.io", Resource: "nodetwins"}, "")
	})
	if _, err := readCluster(context.Background(), c, ""); !errors.Is(err, errNotInstalled) {
		t.Fatalf("got %v, want errNotInstalled", err)
	}
}

// A performance pod on an eco node is what the scheduler extender exists
// to prevent, so the summary and the placement screen flag it. A draining
// node is a performance node waiting for its performance pods to finish:
// those are not flagged.
func TestPerformancePodOnEcoNodeIsFlagged(t *testing.T) {
	c := fakeClients(t,
		[]runtime.Object{
			node("eco-0", "64", nil), node("drain-0", "64", nil),
			pod("lost", "eco-0", podClassPerformance, map[string]string{"cpu": "1"}),
			pod("finishing", "drain-0", podClassPerformance, map[string]string{"cpu": "1"}),
			pod("fine", "eco-0", podClassStandard, map[string]string{"cpu": "1"}),
		},
		twinObj("eco-0", "eco", nil), twinObj("drain-0", "draining", nil),
	)
	snap, err := readCluster(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := summarize(snap).misplaced; got != 1 {
		t.Fatalf("misplaced = %d, want 1", got)
	}
	out := plain(podsView(snap, "ctx", 0, -1))
	if !strings.Contains(out, "✗ 1 performance pods on eco nodes") || !strings.Contains(out, "✗ eco") {
		t.Errorf("placement screen does not flag the pod:\n%s", out)
	}
	if !strings.Contains(plain(summaryPanel(snap, 0, nil)), "1 performance pods on eco nodes") {
		t.Errorf("summary does not flag the pod")
	}
}

// The node table never overflows the terminal: it drops the optional
// columns, the least useful first, and keeps the essential ones.
func TestNodeTableFitsTheTerminal(t *testing.T) {
	snap := cluster{taken: time.Now()}
	for _, name := range []string{"a-very-long-node-name-0", "b"} {
		snap.nodes = append(snap.nodes, nodeView{name: name, class: classEco, capKnown: true, hasHW: true, cpuModel: "INTEL_XEON_GOLD_6530", sockets: 2, measuredW: 300, cappedW: 500})
	}
	full := nodeTable(snap.nodes, 0, -1, nil)
	for _, h := range []string{"HARDWARE", "TREND", "HEADROOM", "COOLING", "MEM"} {
		if !strings.Contains(full, h) {
			t.Errorf("unbounded table lacks %s", h)
		}
	}
	for _, width := range []int{140, 120, 100} {
		out := nodeTable(snap.nodes, width, -1, nil)
		if w := lipgloss.Width(out); w > width {
			t.Errorf("width %d: table is %d wide", width, w)
		}
		for _, h := range []string{"NODE", "CLASS", "POWER", "CPU", "GPU", "PODS", "AGENT"} {
			if !strings.Contains(out, h) {
				t.Errorf("width %d: table lacks %s", width, h)
			}
		}
	}
}

func TestShortModel(t *testing.T) {
	for in, want := range map[string]string{
		"INTEL_XEON_GOLD_6530":                         "Xeon Gold 6530",
		"Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz": "Xeon Platinum 8260",
		"AMD EPYC 9534 64-Core Processor":              "EPYC 9534",
		"NVIDIA_H100_SXM":                              "H100 SXM",
		"NVIDIA H100 80GB HBM3":                        "H100",
		"AMD_Instinct_MI300X_OAM":                      "Instinct MI300X",
		"AMD_INSTINCT_MI300X":                          "Instinct MI300X",
	} {
		if got := shortModel(in); got != want {
			t.Errorf("shortModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func dashboardWith(c cluster) *dashboard {
	d := newDashboard(func(context.Context) (cluster, error) { return c, nil }, "ctx", time.Second, screenNodes, "")
	d.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	d.Update(snapMsg{c: c})
	return d
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// The dashboard's keys: select a row, open the node in the sorted order,
// go back to it, cycle the sort, open the pods, quit.
func TestDashboardKeys(t *testing.T) {
	now := time.Now()
	c := cluster{taken: now, nodes: []nodeView{
		{name: "a", class: classEco, measuredW: 100},
		{name: "b", class: classPerformance, measuredW: 900},
		{name: "c", class: classEco, measuredW: 500},
	}, pods: []podView{{namespace: "ns", name: "p", node: "c", nodeClass: classEco, class: podClassStandard}}}
	d := dashboardWith(c)

	d.Update(key("s")) // sort by power: b, c, a
	if sortNames[d.sortBy] != "power" {
		t.Fatalf("sort = %s, want power", sortNames[d.sortBy])
	}
	d.Update(key("down"))
	d.Update(key("enter"))
	if d.screen != screenNode || d.node != "c" {
		t.Fatalf("enter on row 1 sorted by power opened %q (screen %d), want c", d.node, d.screen)
	}
	if v := plain(d.View().Content); !strings.Contains(v, "Hardware") || !strings.Contains(v, "Pods (1)") {
		t.Errorf("node screen:\n%s", v)
	}
	d.Update(key("esc"))
	if d.screen != screenNodes || d.cursor != 1 {
		t.Errorf("esc: screen %d cursor %d, want the node list on row 1", d.screen, d.cursor)
	}
	d.Update(key("p"))
	if d.screen != screenPods || !strings.Contains(plain(d.View().Content), "workload placement") {
		t.Errorf("p did not open the pods screen")
	}
	if _, cmd := d.Update(key("q")); cmd == nil || cmd() != tea.Quit() {
		t.Errorf("q does not quit")
	}
}

// A sparkline takes one point per twin update, not one per poll: a poll
// faster than the controller manager would otherwise draw flat steps.
func TestDashboardHistoryFollowsTwinUpdates(t *testing.T) {
	t0 := time.Now()
	d := newDashboard(nil, "", time.Second, screenNodes, "")
	read := func(w float64, at time.Time) cluster {
		return cluster{nodes: []nodeView{{name: "a", measuredW: w, updated: at}}}
	}
	d.record(read(100, t0))
	d.record(read(100, t0)) // same twin, polled again
	d.record(read(150, t0.Add(10*time.Second)))
	if got := d.history["a"]; len(got) != 2 || got[1] != 150 {
		t.Errorf("history = %v, want [100 150]", got)
	}
	if len(d.total) != 2 {
		t.Errorf("total history = %v, want 2 points", d.total)
	}
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"--context", "lab", "node", "gpu-0", "-o", "json"}, &bytes.Buffer{})
	if err != nil || o.command != "node" || len(o.args) != 1 || o.args[0] != "gpu-0" || o.output != "json" || o.overrides.CurrentContext != "lab" {
		t.Errorf("got %+v, %v", o, err)
	}
	if o, err := parseArgs(nil, &bytes.Buffer{}); err != nil || o.command != "status" {
		t.Errorf("no command: got %q, %v; want status", o.command, err)
	}
	if o, err := parseArgs([]string{"status", "-W"}, &bytes.Buffer{}); err != nil || !o.watch {
		t.Errorf("-W, the old watch flag, is not watch: %+v %v", o, err)
	}
	if o, err := parseArgs([]string{"pods", "-n", "team"}, &bytes.Buffer{}); err != nil || !o.namespaceSet || o.overrides.Context.Namespace != "team" {
		t.Errorf("-n: %+v %v", o, err)
	}
	if _, err := parseArgs([]string{"-o", "yaml"}, &bytes.Buffer{}); err == nil {
		t.Error("-o yaml accepted")
	}
}

func TestRunVersionAndUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 || out.String() != "kubectl-joulie dev\n" {
		t.Errorf("version: %d %q", code, out.String())
	}
	if code := run([]string{"nope"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), `unknown command "nope"`) {
		t.Errorf("unknown command: %d %q", code, errOut.String())
	}
}

// The -o json field names are a contract with scripts. GPU fields are left
// out for a node without GPUs rather than reported as 0.
func TestJSONOutput(t *testing.T) {
	c := cluster{taken: time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC), nodes: []nodeView{
		{name: "cpu-0", class: classEco, measuredW: 300, cpuCapPct: 60, capKnown: true, agent: agentState{known: true, reporting: true}},
		{name: "gpu-0", class: classPerformance, measuredW: 2000, hasGPU: true, capKnown: true, gpuCapPct: 100, gpuAllocPct: 50},
	}}
	b, err := json.Marshal(toJSON(c, "status", nil))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	sum := doc["summary"].(map[string]interface{})
	if sum["measuredPowerW"] != 2300.0 || sum["nodes"].(map[string]interface{})["eco"] != 1.0 {
		t.Errorf("summary = %v", sum)
	}
	nodes := doc["nodes"].([]interface{})
	cpu, gpu := nodes[0].(map[string]interface{}), nodes[1].(map[string]interface{})
	if _, ok := cpu["gpuCapPct"]; ok {
		t.Errorf("cpu-0 has gpuCapPct: %v", cpu)
	}
	if gpu["gpuRequestedPct"] != 50.0 || cpu["agentReporting"] != true || cpu["cpuCapPct"] != 60.0 {
		t.Errorf("nodes = %v", nodes)
	}
	if _, ok := doc["pods"]; ok {
		t.Errorf("status lists pods: %v", doc["pods"])
	}
	b, _ = json.Marshal(toJSON(cluster{}, "pods", nil))
	var empty map[string]json.RawMessage
	if err := json.Unmarshal(b, &empty); err != nil {
		t.Fatal(err)
	}
	if _, hasNodes := empty["nodes"]; string(empty["pods"]) != "[]" || hasNodes {
		t.Errorf("pods with no pods: %s, want \"pods\":[] and no nodes", b)
	}
}

// A pods list the API server refuses (RBAC, a timeout) is not an empty
// cluster: the screens say the pods are unreadable instead of declaring
// that no performance pod sits on an eco node.
func TestUnreadablePodsAreNotAnAllClear(t *testing.T) {
	c := fakeClients(t, []runtime.Object{node("eco-0", "64", nil)}, twinObj("eco-0", "eco", nil))
	c.kube.(*fake.Clientset).PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no"))
	})
	snap, err := readCluster(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"summary": plain(summaryPanel(snap, 0, nil)),
		"pods":    plain(podsView(snap, "ctx", 0, -1)),
	} {
		if strings.Contains(out, "✓") || !strings.Contains(out, "pods unreadable") {
			t.Errorf("%s screen with an unreadable pod list:\n%s", name, out)
		}
	}
	if doc := toJSON(snap, "pods", nil); doc.Summary.PodsError == "" {
		t.Error("-o json does not report the pod list error")
	}
}

// The live dashboard's frame fits the terminal when the list is windowed,
// so the footer with the keys and the error status stays on screen.
func TestDashboardWindowKeepsTheFooter(t *testing.T) {
	c := cluster{taken: time.Now()}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("n-%02d", i)
		c.nodes = append(c.nodes, nodeView{name: name, class: classEco})
		c.pods = append(c.pods, podView{namespace: "ns", name: "p-" + name, node: name, nodeClass: classEco, class: podClassStandard})
	}
	for _, screen := range []string{"nodes", "pods"} {
		for _, h := range []int{20, 30, 45} {
			d := newDashboard(func(context.Context) (cluster, error) { return c, nil }, "ctx", time.Second, screenNodes, "")
			d.Update(tea.WindowSizeMsg{Width: 200, Height: h})
			d.Update(snapMsg{c: c})
			if screen == "pods" {
				d.Update(key("p"))
			}
			v := d.View().Content
			if got := lipgloss.Height(v); got > h || !strings.Contains(plain(v), "q quit") {
				t.Errorf("%s screen at height %d: %d lines, footer shown %v", screen, h, got, strings.Contains(plain(v), "q quit"))
			}
		}
	}
}

// withClients makes run talk to fake clients.
func withClients(t *testing.T, c clients) {
	t.Helper()
	old := connect
	connect = func(options) (clients, string, error) { return c, "ctx", nil }
	t.Cleanup(func() { connect = old })
}

// A node name that matches no NodeTwin fails in every output, so a script
// reading -o json sees the typo.
func TestNodeCommandFailsForAnUnknownNode(t *testing.T) {
	withClients(t, fakeClients(t, []runtime.Object{node("gpu-0", "64", nil)}, twinObj("gpu-0", "eco", nil)))
	for _, args := range [][]string{{"node", "gpu-O"}, {"node", "gpu-O", "-o", "json"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), `no NodeTwin for node "gpu-O"`) {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

// -n lists the pods of that namespace alone, so a user allowed only there
// can use it, and the output holds only its pods.
func TestPodsNamespaceListsOnlyThatNamespace(t *testing.T) {
	team := pod("mine", "eco-0", podClassStandard, map[string]string{"cpu": "1"})
	team.Namespace = "team"
	c := fakeClients(t, []runtime.Object{node("eco-0", "64", nil), team, pod("other", "eco-0", podClassStandard, nil)}, twinObj("eco-0", "eco", nil))
	var listed []string
	c.kube.(*fake.Clientset).PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		listed = append(listed, a.GetNamespace())
		return false, nil, nil
	})
	withClients(t, c)
	var out, errOut bytes.Buffer
	if code := run([]string{"pods", "-n", "team", "-o", "json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if len(listed) != 1 || listed[0] != "team" {
		t.Errorf("listed pods in %q, want only team", listed)
	}
	var doc jsonDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || len(doc.Pods) != 1 || doc.Pods[0].Name != "mine" {
		t.Errorf("pods %+v, %v", doc.Pods, err)
	}
}

// node NAME -w opens the dashboard on that node, pods -w on the pods.
func TestWatchOpensTheDashboardOnTheCommandsScreen(t *testing.T) {
	for args, want := range map[string]struct {
		screen int
		node   string
	}{
		"status -w":     {screenNodes, ""},
		"top":           {screenNodes, ""},
		"pods -w":       {screenPods, ""},
		"node gpu-0 -w": {screenNode, "gpu-0"},
	} {
		o, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if screen, node := dashboardStart(o); screen != want.screen || node != want.node {
			t.Errorf("%s: screen %d node %q, want %d %q", args, screen, node, want.screen, want.node)
		}
	}
}

// A twin whose status has no effectiveCapState yet (just created) has no
// known cap: it is not shown as a 0% cap.
func TestUnknownCapIsNotShownAsZero(t *testing.T) {
	c := fakeClients(t, []runtime.Object{node("new-0", "64", map[string]string{"nvidia.com/gpu": "4"})}, twinObj("new-0", "eco", nil))
	snap, err := readCluster(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	n := snap.nodes[0]
	if got := plain(capCell(n)); got != "- · -" {
		t.Errorf("cap cell %q, want - · -", got)
	}
	if j := nodeJSON(n); j.CPUCapPct != nil || j.GPUCapPct != nil {
		t.Errorf("json caps %v %v, want none", j.CPUCapPct, j.GPUCapPct)
	}
}
