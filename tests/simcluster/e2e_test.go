//go:build simcluster

package simcluster_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// e2eInputHook runs IN6's input checks (E07, E08, E16). IN6's
// otel_inputs_test.go sets it to its e2eInputChecks in an init function;
// while it is unset, the run says so in the summary's warnings.
var e2eInputHook func(t *testing.T, env *e2eEnv)

// TestSimclusterE2E checks a simulated cluster end to end: the fake Nodes,
// the images, the emulated agents, the hardware inventory, the telemetry,
// the caps through every control path, the scheduler extender and the
// developer loop. scripts/simcluster/smoke.sh (make simcluster-smoke)
// creates the cluster, deploys it with tests/simcluster/smoke-values.yaml
// and runs this test with the build tag simcluster:
//
//	go test -tags simcluster -count=1 -timeout 20m ./tests/simcluster/ -run TestSimclusterE2E -v
//
// Each check is a subtest named by its ID and lands in the run's
// summary.json (simulator/pkg/scenario, X12). E07, E08 and E16, the input
// checks, are IN6's (otel_inputs_test.go); MD01 to MD04 run only on a spec
// that has their pools, such as medium.yaml.
func TestSimclusterE2E(t *testing.T) {
	env := e2eNewEnv(t)
	defer env.finish(t)
	env.load(t)

	env.check(t, "E01", "every spec Node Ready and as clusterspec.Expand renders it", e2eE01NodesMatchTheSpec)
	env.check(t, "E02", "no pod without joulie.io/workload-class on a fake Node", e2eE02NoInfrastructureOnFakeNodes)
	env.check(t, "E03", "every container on an image images.sh built and imported", e2eE03ImagesAreTheOnesBuilt)
	env.check(t, "E04", "one emulated agent per Node the agent DaemonSet would reach", e2eE04AgentsWhereTheDaemonSetWouldRun)
	env.check(t, "E05", "one NodeHardware per agent, equal to its profile's golden", e2eE05HardwareMatchesTheGoldens)
	env.check(t, "E06", "series for every Node with a measured CPU package", e2eE06EveryMeasuredNodeReports)
	if e2eInputHook != nil {
		e2eInputHook(t, env)
	} else {
		env.warn(t, "E07, E08 and E16 did not run: tests/simcluster/otel_inputs_test.go (IN6) does not set e2eInputHook")
	}
	env.check(t, "E11", "the control status of each family", e2eE11ControlStatusPerFamily)
	env.check(t, "E12", "a cap planned on the unreached Node (D15)", e2eE12UnreachedNodeIsPlannedNotEnforced)
	env.check(t, "MD01", "shared GPU Nodes planned as CPU-only (D18)", e2eMD01SharedGPUsPlannedAsCPUOnly)
	env.check(t, "MD02", "nfd absent caps through DVFS", e2eMD02NoNFDTakesDVFS)
	env.check(t, "MD03", "no GPU tools leaves the GPUs alone", e2eMD03NoToolsLeavesGPUsAlone)
	env.check(t, "MD04", "an unmanaged pool has series only", e2eMD04UnmanagedPoolHasSeriesOnly)
	env.check(t, "E13", "the extender filters and scores a standard pod", e2eE13ExtenderPlacesAPod)

	// E09 and E10: the caps at 80 %, one helm upgrade to 50 %, the caps
	// and the power at 50 %. The upgrade runs in the first 50 % check that
	// runs, so a -run filter that selects neither leaves the caps alone.
	env.check(t, "E09.80pct", "RAPL package limits at 80 % of the maximum", e2eE09RAPLAt80)
	env.check(t, "E10.80pct", "NVIDIA GPU limits at 80 % of the maximum, DVFS applied", e2eE10GPUAndDVFSAt80)
	lower := env.lowerCapsOnce(t)
	env.check(t, "E09.50pct", "RAPL package limits at 50 %, the NodeTwin follows", func(c *e2eCase) {
		before, lowered := lower()
		e2eE09RAPLAt50(c, before, lowered)
	})
	env.check(t, "E10.50pct", "NVIDIA GPU limits at 50 %, the NodeTwins follow", func(c *e2eCase) {
		before, lowered := lower()
		e2eE10GPUAndDVFSAt50(c, before, lowered)
	})

	env.check(t, "E14", "a forced extender reload rolls out, an unchanged agent reload does nothing", e2eE14ReloadRollsOrDoesNothing)
	env.check(t, "E15", "status.sh exits 0", e2eE15StatusIsClean)
}

// E01: every spec Node exists and is Ready, and its labels, taints,
// annotations, capacity, allocatable and architecture are those
// clusterspec.Expand renders. Labels others own (e2eSpecLabels) and
// annotations others add (such as the TTL controller's) are allowed; the
// spec's must all be there.
func e2eE01NodesMatchTheSpec(c *e2eCase) {
	e := c.env
	for _, m := range e.Members {
		ctx, cancel := e.ctx()
		var n corev1.Node
		err := e.Client.Get(ctx, client.ObjectKey{Name: m.Name}, &n)
		cancel()
		if err != nil {
			c.Errorf("node %s: %v", m.Name, err)
			continue
		}
		want := m.Node
		if !e2eReady(&n) {
			c.Errorf("node %s is not Ready: %s", m.Name, e2eConditions(&n))
		}
		if d := e2eMapDiff(e2eSpecLabels(n.Labels), want.Labels, false); d != "" {
			c.Errorf("node %s labels differ from the spec: %s", m.Name, d)
		}
		if d := e2eMapDiff(n.Annotations, want.Annotations, true); d != "" {
			c.Errorf("node %s annotations differ from the spec: %s", m.Name, d)
		}
		if got, w := e2eTaints(n.Spec.Taints), e2eTaints(want.Spec.Taints); got != w {
			c.Errorf("node %s taints %s, want %s", m.Name, got, w)
		}
		if d := e2eResourceDiff(n.Status.Capacity, want.Status.Capacity); d != "" {
			c.Errorf("node %s capacity: %s", m.Name, d)
		}
		if d := e2eResourceDiff(n.Status.Allocatable, want.Status.Allocatable); d != "" {
			c.Errorf("node %s allocatable: %s", m.Name, d)
		}
		if got, w := n.Status.NodeInfo.Architecture, want.Status.NodeInfo.Architecture; got != w {
			c.Errorf("node %s architecture %q, want %q", m.Name, got, w)
		}
	}
}

// e2eSpecLabels is a live Node's labels without the ones others own: the
// power profile the controller manager writes, and the deprecated
// beta.kubernetes.io/arch and os, which Kubernetes mirrors from
// kubernetes.io/arch and os when they agree.
func e2eSpecLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	delete(out, workload.LabelPowerProfile)
	for stable, beta := range map[string]string{corev1.LabelArchStable: "beta.kubernetes.io/arch", corev1.LabelOSStable: "beta.kubernetes.io/os"} {
		if v, ok := out[beta]; ok && v == out[stable] {
			delete(out, beta)
		}
	}
	return out
}

func e2eReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func e2eConditions(n *corev1.Node) string {
	var out []string
	for _, cond := range n.Status.Conditions {
		out = append(out, fmt.Sprintf("%s=%s (%s)", cond.Type, cond.Status, cond.Reason))
	}
	return strings.Join(out, ", ")
}

// e2eMapDiff lists the keys of want that got lacks or holds another value
// for and, unless subset, the keys got has beyond want.
func e2eMapDiff(got, want map[string]string, subset bool) string {
	var out []string
	for _, k := range e2eKeys(want) {
		g, ok := got[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("missing %s=%s", k, want[k]))
		case g != want[k]:
			out = append(out, fmt.Sprintf("%s=%s, want %s", k, g, want[k]))
		}
	}
	if !subset {
		for _, k := range e2eKeys(got) {
			if _, ok := want[k]; !ok {
				out = append(out, fmt.Sprintf("extra %s=%s", k, got[k]))
			}
		}
	}
	return strings.Join(out, ", ")
}

func e2eKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// e2eTaints is a taint list as a sorted, comparable string.
func e2eTaints(ts []corev1.Taint) string {
	out := make([]string, 0, len(ts))
	for i := range ts {
		out = append(out, ts[i].ToString())
	}
	sort.Strings(out)
	return "[" + strings.Join(out, " ") + "]"
}

// e2eResourceDiff compares two resource lists by quantity.
func e2eResourceDiff(got, want corev1.ResourceList) string {
	var out []string
	for name, w := range want {
		g, ok := got[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("missing %s=%s", name, w.String()))
		case g.Cmp(w) != 0:
			out = append(out, fmt.Sprintf("%s=%s, want %s", name, g.String(), w.String()))
		}
	}
	for name, g := range got {
		if _, ok := want[name]; !ok {
			out = append(out, fmt.Sprintf("extra %s=%s", name, g.String()))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// E02: no pod without joulie.io/workload-class is bound to a fake Node. The
// fake Nodes have no kubelet; infrastructure belongs on the infra Node,
// which the KWOK taint keeps it on.
func e2eE02NoInfrastructureOnFakeNodes(c *e2eCase) {
	e := c.env
	fake := map[string]bool{}
	for _, m := range e.Members {
		fake[m.Name] = true
	}
	pods, err := e.pods("")
	if err != nil {
		c.Fatalf("list pods: %v", err)
	}
	for _, p := range pods {
		if fake[p.Spec.NodeName] && p.Annotations[workload.AnnotationWorkloadClass] == "" {
			c.Errorf("pod %s/%s is bound to fake node %s without the %s annotation", p.Namespace, p.Name, p.Spec.NodeName,
				workload.AnnotationWorkloadClass)
		}
	}
}

// e2eLocalImage is the reference of an image images.sh built from the
// working tree.
var e2eLocalImage = regexp.MustCompile(`^joulie\.local/joulie-[a-z-]+:dev-[0-9a-f]{12}$`)

// E03: every container in joulie-system and joulie-sim runs an image
// images.sh built (joulie.local/...:dev-<12 hex>), with the image ID
// images.env recorded for it (DR-21), and pull policy IfNotPresent: the
// images are imported, never pulled.
func e2eE03ImagesAreTheOnesBuilt(c *e2eCase) {
	e := c.env
	ids := map[string][]string{}
	for _, k := range []string{"AGENT", "CONTROLLER_MANAGER", "SCHEDULER", "SIMULATOR"} {
		ref := e.Images["IMAGE_"+k]
		if ref == "" {
			c.Fatalf("images.env sets no IMAGE_%s", k)
		}
		ids[ref] = strings.Split(e.Images["IMAGE_IDS_"+k], ",")
	}
	containers := 0
	for _, ns := range []string{scenario.NamespaceJoulie, scenario.NamespaceSimulator} {
		pods, err := e.pods(ns)
		if err != nil {
			c.Fatalf("list pods in %s: %v", ns, err)
		}
		for i := range pods {
			p := &pods[i]
			if !e2eRunning(p) {
				continue
			}
			statuses := map[string]string{}
			for _, s := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
				statuses[s.Name] = s.ImageID
			}
			for _, ct := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
				containers++
				where := fmt.Sprintf("%s/%s %s", ns, p.Name, ct.Name)
				if !e2eLocalImage.MatchString(ct.Image) {
					c.Errorf("%s: image %s is not joulie.local/joulie-<component>:dev-<12 hex>", where, ct.Image)
					continue
				}
				want, ok := ids[ct.Image]
				if !ok {
					c.Errorf("%s: image %s is not one images.env records", where, ct.Image)
					continue
				}
				if !e2eContains(want, statuses[ct.Name]) {
					c.Errorf("%s: imageID %q, want one of %v", where, statuses[ct.Name], want)
				}
				if ct.ImagePullPolicy != corev1.PullIfNotPresent {
					c.Errorf("%s: pull policy %s, want IfNotPresent", where, ct.ImagePullPolicy)
				}
			}
		}
	}
	if containers == 0 {
		c.Errorf("no running container in %s and %s", scenario.NamespaceJoulie, scenario.NamespaceSimulator)
	}
}

func e2eContains(list []string, s string) bool {
	for _, v := range list {
		if v != "" && v == s {
			return true
		}
	}
	return false
}

// E04: the emulated agent Deployments stand in for exactly the Nodes the
// agent DaemonSet would reach (EligibleNodes over the live Nodes, with the
// placement of the DaemonSet render deploy.sh made from the install's
// values files, R8). Each has one Running pod, on the infra Node, without
// host network and unprivileged; every Node left out has a reason.
func e2eE04AgentsWhereTheDaemonSetWouldRun(c *e2eCase) {
	e := c.env
	render, err := os.ReadFile(filepath.Join(e.Dir, "render", "agent-daemonset.yaml"))
	if err != nil {
		c.Fatalf("the DaemonSet render deploy.sh made: %v", err)
	}
	placement, err := simcluster.PlacementFromRender(render, "amd64")
	if err != nil {
		c.Fatalf("placement: %v", err)
	}
	nodes := make([]*corev1.Node, 0, len(e.Members))
	for _, m := range e.Members {
		ctx, cancel := e.ctx()
		n := &corev1.Node{}
		err := e.Client.Get(ctx, client.ObjectKey{Name: m.Name}, n)
		cancel()
		if err != nil {
			c.Fatalf("node %s: %v", m.Name, err)
		}
		nodes = append(nodes, n)
	}
	eligible, excluded := simcluster.EligibleNodes(nodes, placement)
	deps, err := e.agentDeployments()
	if err != nil {
		c.Fatalf("agent Deployments: %v", err)
	}
	if got := e2eKeys(deps); !reflect.DeepEqual(got, append([]string{}, eligible...)) {
		c.Errorf("emulated agents stand in for %v, the agent DaemonSet would reach %v", got, eligible)
	}
	for _, x := range excluded {
		if strings.TrimSpace(x.Reason) == "" {
			c.Errorf("node %s is left out without a reason", x.Node)
		}
		c.Logf("no agent on %s: %s", x.Node, x.Reason)
	}

	infra := e2eInfraNode(c)
	pods, err := e.pods(scenario.NamespaceJoulie, client.MatchingLabels{e2eLabelComponent: e2eComponentEmulated})
	if err != nil {
		c.Fatalf("agent pods: %v", err)
	}
	running := map[string][]*corev1.Pod{}
	for i := range pods {
		if p := &pods[i]; e2eRunning(p) {
			running[p.Annotations[e2eAnnotationNode]] = append(running[p.Annotations[e2eAnnotationNode]], p)
		}
	}
	for _, node := range eligible {
		ps := running[node]
		if len(ps) != 1 {
			c.Errorf("node %s: %d Running agent pods, want 1", node, len(ps))
			continue
		}
		p := ps[0]
		if p.Spec.NodeName != infra {
			c.Errorf("node %s: agent pod %s runs on %q, want the infra Node %s", node, p.Name, p.Spec.NodeName, infra)
		}
		if p.Spec.HostNetwork {
			c.Errorf("node %s: agent pod %s uses the host network", node, p.Name)
		}
		for _, ct := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
			if sc := ct.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
				c.Errorf("node %s: agent pod %s container %s is privileged", node, p.Name, ct.Name)
			}
		}
	}
}

// e2eInfraNode is the name of the one Node labelled joulie.io/infra=true,
// the k3s server's own kubelet.
func e2eInfraNode(c *e2eCase) string {
	c.Helper()
	ctx, cancel := c.env.ctx()
	defer cancel()
	var l corev1.NodeList
	if err := c.env.Client.List(ctx, &l, client.MatchingLabels{e2eLabelInfra: "true"}); err != nil {
		c.Fatalf("infra Node: %v", err)
	}
	if len(l.Items) != 1 {
		c.Fatalf("%d Nodes labelled %s=true, want 1", len(l.Items), e2eLabelInfra)
	}
	return l.Items[0].Name
}

// e2eGoldenComparable reports whether the in-process golden of a Node's
// profile describes it: the pool overrides nothing the inventory reads
// (persistence mode only changes when a GPU limit resets) and the GPU tools
// are delivered.
func (e *e2eEnv) goldenComparable(n e2eNode) bool {
	if n.ToolDelivery == simcluster.ToolDeliveryNone {
		return false
	}
	for _, p := range e.Spec.Spec.Pools {
		if p.Name != n.Pool || p.Overrides == nil {
			continue
		}
		o := p.Overrides
		if o.NFD != "" {
			return false
		}
		if g := o.GPUs; g != nil && (g.Exposure != "" || g.MIGProfile != "" || g.MIGDevicesPerGPU != nil || g.TimesliceReplicas != nil) {
			return false
		}
	}
	return true
}

// E05: one NodeHardware per Node with an agent and none for the others.
// Each one's status, decoded into the typed status, equals the in-process
// golden of its profile (cmd/agent/testdata/hwemu/<profile>.json) with
// updatedAt and gpu.currentCapWatts cleared; currentCapWatts equals the
// GPU limit the hardware enforces.
func e2eE05HardwareMatchesTheGoldens(c *e2eCase) {
	e := c.env
	nodes, err := e.nodes()
	if err != nil {
		c.Fatalf("spec Nodes: %v", err)
	}
	var problems []string
	err = e2eEventually(e.agentSettle(), func() error {
		problems = problems[:0]
		hw, err := e.hardware()
		if err != nil {
			return err
		}
		snap, err := e.snapshot()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			h, ok := hw[n.Name]
			if !n.Agent {
				if ok {
					problems = append(problems, fmt.Sprintf("node %s has no agent but a NodeHardware %s", n.Name, h.Name))
				}
				continue
			}
			if !ok {
				problems = append(problems, fmt.Sprintf("node %s has an agent but no NodeHardware", n.Name))
				continue
			}
			if e.goldenComparable(n) {
				if d, err := e.goldenDiff(n.Profile.Name, h.Status); err != nil {
					return err
				} else if d != "" {
					problems = append(problems, fmt.Sprintf("node %s: NodeHardware status differs from %s.json: %s", n.Name, n.Profile.Name, d))
				}
			}
			if g := h.Status.GPU; g != nil && g.ControlAvailable {
				sn, ok := e2eSnapshotNode(snap, n.Name)
				if !ok {
					problems = append(problems, fmt.Sprintf("node %s is not in /debug/emulate", n.Name))
					continue
				}
				for _, gpu := range e2eComponents(sn, semconv.HwTypeGPU) {
					if !e2eNear(g.CurrentCapWatts, gpu.LimitW, e2eLimitTolW) {
						problems = append(problems, fmt.Sprintf("node %s: NodeHardware gpu.currentCapWatts %.1f, %s enforces %.1f W",
							n.Name, g.CurrentCapWatts, gpu.ID, gpu.LimitW))
						break
					}
				}
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
}

// goldenDiff compares a NodeHardware status with the golden of profile,
// both decoded into the typed status with updatedAt and currentCapWatts
// cleared, and returns the two as JSON when they differ.
func (e *e2eEnv) goldenDiff(profile string, got v1alpha1.NodeHardwareStatus) (string, error) {
	path := filepath.Join(e.Repo, "cmd", "agent", "testdata", "hwemu", profile+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the golden of %s: %w", profile, err)
	}
	var want v1alpha1.NodeHardwareStatus
	if err := json.Unmarshal(b, &want); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	got = *got.DeepCopy()
	for _, s := range []*v1alpha1.NodeHardwareStatus{&got, &want} {
		s.UpdatedAt = ""
		if s.GPU != nil {
			s.GPU.CurrentCapWatts = 0
		}
	}
	if reflect.DeepEqual(got, want) {
		return "", nil
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	return fmt.Sprintf("got %s, want %s", gb, wb), nil
}

// e2eSimJob is the job label of every series the simulator pushes.
const e2eSimJob = telemetry.ServiceNamespace + "/" + telemetry.DefaultServiceName

// E06: every spec Node with a measured CPU package is returned by the
// mode's wait expression; in the otel modes, target_info carries each one's
// k8s.node.name.
func e2eE06EveryMeasuredNodeReports(c *e2eCase) {
	e := c.env
	w := e.Parts.Wait
	if len(w.Nodes) == 0 {
		c.Skipf("no spec Node has a measured CPU package")
	}
	var missing []string
	err := e2eEventually(e.twinFollow(), func() error {
		got, err := e.promByLabel(w.Query, w.Label)
		if err != nil {
			return err
		}
		missing = missing[:0]
		for _, n := range w.Nodes {
			if _, ok := got[n]; !ok {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s returns no series for %v", w.Query, missing)
		}
		return nil
	})
	if err != nil {
		c.Errorf("%v", err)
	}
	if e.Mode == simcluster.ModeKepler || e.Mode == simcluster.ModeKeplerLegacy {
		return
	}
	label := e2eStrategy(e.Mode).LabelName(semconv.AttrK8sNodeName)
	query := fmt.Sprintf(`target_info{job=%q}`, e2eSimJob)
	r, err := e.prom(query)
	if err != nil {
		c.Fatalf("%v", err)
	}
	carried := map[string]bool{}
	for _, s := range r.Series {
		if s.Labels["instance"] != "" && s.Labels["instance"] == s.Labels[label] {
			carried[s.Labels[label]] = true
		}
	}
	for _, n := range w.Nodes {
		if !carried[n] {
			c.Errorf("%s has no series with instance=%s and %s=%s", query, n, label, n)
		}
	}
}

// e2eStatus is a control result as backend/result/message.
func e2eStatus(r *v1alpha1.ControlResult) string {
	if r == nil {
		return "no status"
	}
	return r.Backend + "/" + r.Result + "/" + r.Message
}

// e2eControlWant is the CPU or GPU control status a family must show:
// backend and result, and the message when it is fixed.
type e2eControlWant struct {
	gpu                      bool
	backend, result, message string
}

// e2eControlByFamily is E11's table.
var e2eControlByFamily = map[string]e2eControlWant{
	e2eFamilyRAPL:     {backend: "rapl", result: "applied"},
	e2eFamilyDVFS:     {backend: "dvfs", result: "applied"},
	e2eFamilyNoCPUCap: {backend: "none", result: "blocked", message: "pct intent unresolved and no dvfs backend available"},
	e2eFamilyNVIDIA:   {gpu: true, backend: "host-nvidia", result: "applied"},
}

// e2eD1Message is what the agent records while D1 lasts.
const e2eD1Message = "unrecognized arguments: --showpowercap"

// E11: the control status per family (cmd/agent/main.go): RAPL applied on
// the Intel host, DVFS applied on both EPYC hosts, host-nvidia applied on
// the NVIDIA GPUs, and on the VM nothing to apply. Known D1: the MI300X
// GPU control ends in none/error with the argparse message.
func e2eE11ControlStatusPerFamily(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyRAPL, e2eFamilyDVFS, e2eFamilyNoCPUCap, e2eFamilyNVIDIA, e2eFamilyAMDGPU)
	if len(nodes) == 0 {
		c.Skipf("no Node of a family E11 knows")
	}
	var problems []string
	d1 := map[string]string{}
	err := e2eEventually(e.agentSettle(), func() error {
		problems = problems[:0]
		twins, err := e.twins()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			tw, ok := twins[n.Name]
			if !ok {
				problems = append(problems, fmt.Sprintf("node %s has no NodeTwin", n.Name))
				continue
			}
			cs := tw.Status.ControlStatus
			if n.family() == e2eFamilyAMDGPU {
				var gpu *v1alpha1.ControlResult
				if cs != nil {
					gpu = cs.GPU
				}
				d1[n.Name] = e2eStatus(gpu)
				if gpu == nil || gpu.Result == "" {
					problems = append(problems, fmt.Sprintf("node %s: no GPU control status yet", n.Name))
				}
				continue
			}
			want := e2eControlByFamily[n.family()]
			var got *v1alpha1.ControlResult
			if cs != nil {
				got = cs.CPU
				if want.gpu {
					got = cs.GPU
				}
			}
			if got == nil || got.Backend != want.backend || got.Result != want.result || (want.message != "" && got.Message != want.message) {
				what := "CPU"
				if want.gpu {
					what = "GPU"
				}
				problems = append(problems, fmt.Sprintf("node %s (%s): %s control %s, want %s/%s%s", n.Name, n.family(), what,
					e2eStatus(got), want.backend, want.result, e2eSuffix("/", want.message)))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d nodes off", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
	for _, node := range e2eKeys(d1) {
		got := d1[node]
		reproduced := strings.HasPrefix(got, "none/error/") && strings.Contains(got, e2eD1Message)
		c.knownDefect("D1", reproduced, fmt.Sprintf("node %s GPU control %s", node, got))
	}
}

func e2eSuffix(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// e2eHasIntent reports whether a NodeTwin plans a CPU or GPU cap.
func e2eHasIntent(tw v1alpha1.NodeTwin) bool {
	cpu := tw.Spec.CPU != nil && (tw.Spec.CPU.PackagePowerCapPctOfMax != nil || tw.Spec.CPU.PackagePowerCapWatts != nil)
	gpu := tw.Spec.GPU != nil && tw.Spec.GPU.PowerCap != nil
	return cpu || gpu
}

// e2eIntent is a NodeTwin's planned caps, for messages.
func e2eIntent(tw v1alpha1.NodeTwin) string {
	var out []string
	if s := tw.Spec.CPU; s != nil && s.PackagePowerCapPctOfMax != nil {
		out = append(out, fmt.Sprintf("cpu %.0f %%", *s.PackagePowerCapPctOfMax))
	}
	if s := tw.Spec.CPU; s != nil && s.PackagePowerCapWatts != nil {
		out = append(out, fmt.Sprintf("cpu %.1f W", *s.PackagePowerCapWatts))
	}
	if s := tw.Spec.GPU; s != nil && s.PowerCap != nil && s.PowerCap.CapPctOfMax != nil {
		out = append(out, fmt.Sprintf("gpu %.0f %%", *s.PowerCap.CapPctOfMax))
	}
	if s := tw.Spec.GPU; s != nil && s.PowerCap != nil && s.PowerCap.CapWattsPerGPU != nil {
		out = append(out, fmt.Sprintf("gpu %.1f W", *s.PowerCap.CapWattsPerGPU))
	}
	if len(out) == 0 {
		return "no cap"
	}
	return strings.Join(out, ", ")
}

// e2eLimits is the enforced limits of a node's components of type typ.
func e2eLimits(n emulate.NodeSnapshot, typ string) []float64 {
	var out []float64
	for _, c := range e2eComponents(n, typ) {
		out = append(out, math.Round(c.LimitW*10)/10)
	}
	return out
}

// e2eAllNear reports whether every value is want within tol, and there is
// at least one.
func e2eAllNear(vs []float64, want, tol float64) bool {
	for _, v := range vs {
		if !e2eNear(v, want, tol) {
			return false
		}
	}
	return len(vs) > 0
}

// E12, known D15: a managed Node the agent DaemonSet does not reach (in the
// smoke spec intel-tainted-0, whose taint the agent does not tolerate) has
// a NodeTwin with a cap and no NodeHardware, as it must; while D15 lasts it
// also has no controlStatus and its hardware runs at the uncapped limit.
func e2eE12UnreachedNodeIsPlannedNotEnforced(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyUnreached)
	if len(nodes) == 0 {
		c.Skipf("every managed Node has an agent")
	}
	var problems []string
	err := e2eEventually(e.agentSettle(), func() error {
		problems = problems[:0]
		twins, err := e.twins()
		if err != nil {
			return err
		}
		hw, err := e.hardware()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if tw, ok := twins[n.Name]; !ok || !e2eHasIntent(tw) {
				problems = append(problems, fmt.Sprintf("node %s: no NodeTwin with a cap intent", n.Name))
			}
			if _, ok := hw[n.Name]; ok {
				problems = append(problems, fmt.Sprintf("node %s has no agent but a NodeHardware", n.Name))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d nodes off", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Fatalf("%v", err)
	}
	twins, err := e.twins()
	if err != nil {
		c.Fatalf("NodeTwins: %v", err)
	}
	snap, err := e.snapshot()
	if err != nil {
		c.Fatalf("%v", err)
	}
	for _, n := range nodes {
		tw := twins[n.Name]
		sn, ok := e2eSnapshotNode(snap, n.Name)
		if !ok {
			c.Errorf("node %s is not in /debug/emulate", n.Name)
			continue
		}
		cpuDefault, gpuDefault := e2eDefaultPackageLimitW(n.Profile), e2eDefaultGPULimitW(n.Profile)
		cpu, gpu := e2eLimits(sn, semconv.HwTypeCPU), e2eLimits(sn, semconv.HwTypeGPU)
		uncapped := (cpuDefault == 0 || e2eAllNear(cpu, cpuDefault, e2eLimitTolW)) &&
			(gpuDefault == 0 || e2eAllNear(gpu, gpuDefault, e2eLimitTolW))
		reproduced := tw.Status.ControlStatus == nil && uncapped
		c.knownDefect("D15", reproduced, fmt.Sprintf("node %s: NodeTwin %s, no NodeHardware, controlStatus %s, package limits %v W "+
			"(uncapped %.1f), GPU limits %v W (uncapped %.1f)", n.Name, e2eIntent(tw), e2eControlStatus(tw.Status.ControlStatus),
			cpu, cpuDefault, gpu, gpuDefault))
	}
}

func e2eControlStatus(cs *v1alpha1.ControlStatus) string {
	if cs == nil {
		return "none"
	}
	return fmt.Sprintf("cpu %s, gpu %s", e2eStatus(cs.CPU), e2eStatus(cs.GPU))
}

// e2ePackageMaxW is the CPU package cap range maximum a NodeHardware
// publishes, 0 when it has none.
func e2ePackageMaxW(h v1alpha1.NodeHardware) float64 {
	if h.Status.CPU.CapRange == nil {
		return 0
	}
	return h.Status.CPU.CapRange.MaxWattsPerSocket
}

// e2eGPUMaxW is the per-GPU cap range maximum a NodeHardware publishes, 0
// when it has none.
func e2eGPUMaxW(h v1alpha1.NodeHardware) float64 {
	if h.Status.GPU == nil || h.Status.GPU.CapRangePerGPU == nil {
		return 0
	}
	return h.Status.GPU.CapRangePerGPU.MaxWatts
}

// MD01, known D18: MIG-mixed and renamed time-slicing GPU Nodes get no GPU
// intent (none/none/"no GPU cap intent") and the CPU percent cap on both
// packages, 132 W at 80 % (R13).
func e2eMD01SharedGPUsPlannedAsCPUOnly(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyGPUShared)
	if len(nodes) == 0 {
		c.Skipf("the spec has no MIG-mixed or renamed time-slicing GPU Node")
	}
	observed := map[string]string{}
	reproduced := map[string]bool{}
	err := e2eEventually(e.agentSettle(), func() error {
		twins, err := e.twins()
		if err != nil {
			return err
		}
		hw, err := e.hardware()
		if err != nil {
			return err
		}
		snap, err := e.snapshot()
		if err != nil {
			return err
		}
		all := true
		for _, n := range nodes {
			tw, h := twins[n.Name], hw[n.Name]
			sn, _ := e2eSnapshotNode(snap, n.Name)
			var gpu *v1alpha1.ControlResult
			if tw.Status.ControlStatus != nil {
				gpu = tw.Status.ControlStatus.GPU
			}
			want := e2eCapPctBefore / 100 * e2ePackageMaxW(h)
			cpu := e2eLimits(sn, semconv.HwTypeCPU)
			observed[n.Name] = fmt.Sprintf("node %s: NodeTwin %s, GPU control %s, package limits %v W (80 %% is %.1f)",
				n.Name, e2eIntent(tw), e2eStatus(gpu), cpu, want)
			reproduced[n.Name] = gpu != nil && gpu.Backend == "none" && gpu.Result == "none" && gpu.Message == "no GPU cap intent" &&
				want > 0 && e2eAllNear(cpu, want, e2eLimitTolW)
			all = all && reproduced[n.Name]
		}
		if !all {
			return fmt.Errorf("not every shared GPU Node shows D18 yet")
		}
		return nil
	})
	if err != nil {
		c.Logf("%v", err)
	}
	for _, n := range nodes {
		c.knownDefect("D18", reproduced[n.Name], observed[n.Name])
	}
}

// MD02, nfd: absent: without NFD labels the agent reads no CPU vendor,
// still publishes the RAPL cap range (165 W), and caps through DVFS, so the
// RAPL long-term limit stays at the uncapped value (A11, D9).
func e2eMD02NoNFDTakesDVFS(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyNoNFD)
	if len(nodes) == 0 {
		c.Skipf("the spec has no pool with nfd: absent")
	}
	e.eventuallyEach(c, nodes, func(n e2eNode, tw v1alpha1.NodeTwin, h v1alpha1.NodeHardware, sn emulate.NodeSnapshot) []string {
		var out []string
		if h.Status.CPU.Vendor != "" {
			out = append(out, fmt.Sprintf("NodeHardware cpu.vendor %q, want empty", h.Status.CPU.Vendor))
		}
		if max, want := e2ePackageMaxW(h), e2eDefaultPackageLimitW(n.Profile); !e2eNear(max, want, e2eLimitTolW) {
			out = append(out, fmt.Sprintf("NodeHardware cpu.capRange.maxWattsPerSocket %.1f, want %.1f", max, want))
		}
		if cs := tw.Status.ControlStatus; cs == nil || cs.CPU == nil || cs.CPU.Backend != "dvfs" {
			out = append(out, fmt.Sprintf("CPU control %s, want backend dvfs", e2eControlStatus(cs)))
		}
		if cpu, want := e2eLimits(sn, semconv.HwTypeCPU), e2eDefaultPackageLimitW(n.Profile); !e2eAllNear(cpu, want, e2eLimitTolW) {
			out = append(out, fmt.Sprintf("package limits %v W, want them unchanged at %.1f", cpu, want))
		}
		return out
	})
}

// MD03, toolDelivery: none: without the GPU tools the agent finds no GPU to
// control: controlAvailable false, GPU control none/error starting with
// "gpu inventory failed", and no GPU limit changes.
func e2eMD03NoToolsLeavesGPUsAlone(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyNoTools)
	if len(nodes) == 0 {
		c.Skipf("the spec has no GPU pool with toolDelivery: none")
	}
	e.eventuallyEach(c, nodes, func(n e2eNode, tw v1alpha1.NodeTwin, h v1alpha1.NodeHardware, sn emulate.NodeSnapshot) []string {
		var out []string
		if h.Status.GPU != nil && h.Status.GPU.ControlAvailable {
			out = append(out, "NodeHardware gpu.controlAvailable true, want false")
		}
		var gpu *v1alpha1.ControlResult
		if tw.Status.ControlStatus != nil {
			gpu = tw.Status.ControlStatus.GPU
		}
		if gpu == nil || gpu.Backend != "none" || gpu.Result != "error" || !strings.HasPrefix(gpu.Message, "gpu inventory failed") {
			out = append(out, fmt.Sprintf("GPU control %s, want none/error/gpu inventory failed...", e2eStatus(gpu)))
		}
		if lim, want := e2eLimits(sn, semconv.HwTypeGPU), e2eDefaultGPULimitW(n.Profile); !e2eAllNear(lim, want, e2eLimitTolW) {
			out = append(out, fmt.Sprintf("GPU limits %v W, want them unchanged at %.1f", lim, want))
		}
		return out
	})
}

// eventuallyEach polls until check finds nothing wrong on any node, then
// reports what it found last. check gets the node's NodeTwin, NodeHardware
// and reading, zero when missing.
func (e *e2eEnv) eventuallyEach(c *e2eCase, nodes []e2eNode,
	check func(n e2eNode, tw v1alpha1.NodeTwin, h v1alpha1.NodeHardware, sn emulate.NodeSnapshot) []string) {
	c.Helper()
	var problems []string
	err := e2eEventually(e.agentSettle(), func() error {
		problems = problems[:0]
		twins, err := e.twins()
		if err != nil {
			return err
		}
		hw, err := e.hardware()
		if err != nil {
			return err
		}
		snap, err := e.snapshot()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			sn, _ := e2eSnapshotNode(snap, n.Name)
			for _, p := range check(n, twins[n.Name], hw[n.Name], sn) {
				problems = append(problems, fmt.Sprintf("node %s: %s", n.Name, p))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
}

// MD04: an unmanaged pool gets no agent and no NodeTwin, while the
// simulator still exports its series.
func e2eMD04UnmanagedPoolHasSeriesOnly(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyUnmanaged)
	if len(nodes) == 0 {
		c.Skipf("the spec has no unmanaged pool")
	}
	twins, err := e.twins()
	if err != nil {
		c.Fatalf("NodeTwins: %v", err)
	}
	series, err := e.promByLabel(e.Parts.Wait.Query, e.Parts.Wait.Label)
	if err != nil {
		c.Fatalf("%v", err)
	}
	waited := map[string]bool{}
	for _, n := range e.Parts.Wait.Nodes {
		waited[n] = true
	}
	for _, n := range nodes {
		if n.Agent {
			c.Errorf("unmanaged node %s has an emulated agent", n.Name)
		}
		if _, ok := twins[n.Name]; ok {
			c.Errorf("unmanaged node %s has a NodeTwin", n.Name)
		}
		if _, ok := series[n.Name]; waited[n.Name] && !ok {
			c.Errorf("unmanaged node %s has no series: %s", n.Name, e.Parts.Wait.Query)
		}
	}
}

// Extender metrics (cmd/scheduler), read through the proxy on port 9877.
const (
	e2eFilterRequests     = "joulie_scheduler_filter_requests_total"
	e2ePrioritizeRequests = "joulie_scheduler_prioritize_requests_total"
)

// e2eProbeImage is the image of E13's pod. A fake Node never pulls it.
const e2eProbeImage = "registry.k8s.io/pause:3.10"

// E13: a standard pod that tolerates the KWOK taint and selects managed
// Nodes is bound to a fake Node, and the extender's filter and prioritize
// counters grow. The pod is deleted again.
func e2eE13ExtenderPlacesAPod(c *e2eCase) {
	e := c.env
	counters := func() (filter, prioritize float64, err error) {
		b, err := e.proxyGet(scenario.NamespaceJoulie, e2eExtenderMetrics, "metrics", nil)
		if err != nil {
			return 0, 0, err
		}
		if filter, err = e2eMetricSum(b, e2eFilterRequests); err != nil {
			return 0, 0, err
		}
		prioritize, err = e2eMetricSum(b, e2ePrioritizeRequests)
		return filter, prioritize, err
	}
	f0, p0, err := counters()
	if err != nil {
		c.Fatalf("extender metrics: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   metav1.NamespaceDefault,
			Name:        fmt.Sprintf("joulie-e2e-e13-%d", time.Now().Unix()),
			Annotations: map[string]string{workload.AnnotationWorkloadClass: "standard"},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{e2eLabelManaged: "true"},
			Tolerations: []corev1.Toleration{{Key: clusterspec.KWOKTaintKey, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoSchedule}},
			Containers: []corev1.Container{{Name: "probe", Image: e2eProbeImage}},
		},
	}
	ctx, cancel := e.ctx()
	err = e.Client.Create(ctx, pod)
	cancel()
	if err != nil {
		c.Fatalf("create pod %s: %v", pod.Name, err)
	}
	defer func() {
		ctx, cancel := e.ctx()
		defer cancel()
		if err := e.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			c.Errorf("delete pod %s: %v", pod.Name, err)
			return
		}
		if err := e2eEventually(time.Minute, func() error {
			ctx, cancel := e.ctx()
			defer cancel()
			err := e.Client.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("pod %s is still there (%v)", pod.Name, err)
		}); err != nil {
			c.Errorf("%v", err)
		}
	}()

	managed := map[string]bool{}
	for _, m := range e.Members {
		if m.Node.Labels[e2eLabelManaged] == "true" {
			managed[m.Name] = true
		}
	}
	var bound string
	if err := e2eEventually(2*time.Minute, func() error {
		ctx, cancel := e.ctx()
		defer cancel()
		var p corev1.Pod
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(pod), &p); err != nil {
			return err
		}
		if p.Spec.NodeName == "" {
			return fmt.Errorf("pod %s is not bound: %v", pod.Name, p.Status.Conditions)
		}
		bound = p.Spec.NodeName
		return nil
	}); err != nil {
		c.Fatalf("%v", err)
	}
	if !managed[bound] {
		c.Errorf("pod %s is bound to %s, not a managed fake Node", pod.Name, bound)
	}
	c.Logf("pod %s bound to %s", pod.Name, bound)
	var f1, p1 float64
	if err := e2eEventually(time.Minute, func() error {
		var err error
		if f1, p1, err = counters(); err != nil {
			return err
		}
		if f1 <= f0 || p1 <= p0 {
			return fmt.Errorf("%s %v to %v, %s %v to %v", e2eFilterRequests, f0, f1, e2ePrioritizeRequests, p0, p1)
		}
		return nil
	}); err != nil {
		c.Errorf("the extender's counters did not grow: %v", err)
	}
}

// e2eCapState is the cluster before the caps are lowered: the time, the
// truth Joulie must read (when the mode gives Joulie data) and each
// NodeTwin's measured power.
type e2eCapState struct {
	at        time.Time
	haveTruth bool
	truth     map[string]float64
	twin      map[string]float64
	// unsteady says why the state is not a steady one to measure from;
	// empty when it is.
	unsteady string
}

// capsAt checks the enforced limits of the RAPL packages and NVIDIA
// GPUs at pct of the NodeHardware maximum, and that the measured power
// stays at most 1 % above the limit.
func (e *e2eEnv) capsAt(nodes []e2eNode, typ string, pct float64) []string {
	hw, err := e.hardware()
	if err != nil {
		return []string{err.Error()}
	}
	snap, err := e.snapshot()
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, n := range nodes {
		max := e2ePackageMaxW(hw[n.Name])
		if typ == semconv.HwTypeGPU {
			max = e2eGPUMaxW(hw[n.Name])
		}
		if max <= 0 {
			out = append(out, fmt.Sprintf("node %s: NodeHardware publishes no %s cap range", n.Name, typ))
			continue
		}
		want := pct / 100 * max
		sn, ok := e2eSnapshotNode(snap, n.Name)
		if !ok {
			out = append(out, fmt.Sprintf("node %s is not in /debug/emulate", n.Name))
			continue
		}
		comps := e2eComponents(sn, typ)
		if len(comps) == 0 {
			out = append(out, fmt.Sprintf("node %s has no %s component", n.Name, typ))
		}
		for _, cr := range comps {
			if !e2eNear(cr.LimitW, want, e2eLimitTolW) {
				out = append(out, fmt.Sprintf("node %s %s: limit %.2f W, want %.1f (%.0f %% of %.1f)", n.Name, cr.ID, cr.LimitW, want, pct, max))
			}
			if cr.PowerW > cr.LimitW*1.01 {
				out = append(out, fmt.Sprintf("node %s %s: power %.2f W, more than 1 %% above its limit %.2f", n.Name, cr.ID, cr.PowerW, cr.LimitW))
			}
		}
	}
	return out
}

// E09 at 80 %: on every CPU-only Node with RAPL caps, each package's limit
// is 80 % of the published maximum (132 W of 165) and its power at most
// 1 % above it (133.3 W).
func e2eE09RAPLAt80(c *e2eCase) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyRAPL)
	if len(nodes) == 0 {
		c.Skipf("no CPU-only Node caps through RAPL")
	}
	e.eventuallyNone(c, e.agentSettle(), func() []string { return e.capsAt(nodes, semconv.HwTypeCPU, e2eCapPctBefore) })
}

// E10 at 80 %: on every NVIDIA GPU Node each GPU's limit is 80 % of the
// published maximum (NVL 320 W of 400, SXM 560 of 700) and its power at
// most 1 % above it; the DVFS hosts report dvfs applied.
func e2eE10GPUAndDVFSAt80(c *e2eCase) {
	e := c.env
	gpus := e.nodesOf(c, e2eFamilyNVIDIA)
	dvfs := e.nodesOf(c, e2eFamilyDVFS)
	if len(gpus)+len(dvfs) == 0 {
		c.Skipf("no NVIDIA GPU Node and no DVFS host")
	}
	e.eventuallyNone(c, e.agentSettle(), func() []string {
		return append(e.capsAt(gpus, semconv.HwTypeGPU, e2eCapPctBefore), e.dvfsApplied(dvfs)...)
	})
}

// dvfsApplied lists the DVFS hosts whose CPU control is not dvfs applied.
func (e *e2eEnv) dvfsApplied(nodes []e2eNode) []string {
	if len(nodes) == 0 {
		return nil
	}
	twins, err := e.twins()
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, n := range nodes {
		var cpu *v1alpha1.ControlResult
		if cs := twins[n.Name].Status.ControlStatus; cs != nil {
			cpu = cs.CPU
		}
		if cpu == nil || cpu.Backend != "dvfs" || cpu.Result != "applied" {
			out = append(out, fmt.Sprintf("node %s: CPU control %s, want dvfs/applied", n.Name, e2eStatus(cpu)))
		}
	}
	return out
}

// eventuallyNone polls fn until it finds nothing wrong, for at most within,
// then reports what it found last.
func (e *e2eEnv) eventuallyNone(c *e2eCase, within time.Duration, fn func() []string) {
	c.Helper()
	var problems []string
	err := e2eEventually(within, func() error {
		problems = fn()
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
}

// lowerCaps waits until the series are older than a rate window
// (waitSeriesAge) and the NodeTwins the caps will move read the truth
// (within the check tolerance, 3 % with a 1 W floor), records that state,
// and lowers both performance caps to 50 % in one helm upgrade of the
// Joulie release (--reuse-values), then waits for the controller manager to
// roll out. A NodeTwin still reading a rate over a window that began before
// its series did, as right after a deploy, would make the drop look
// smaller or larger than it is. It returns the state before and whether
// the upgrade went through; a failed upgrade fails the test.
func (e *e2eEnv) lowerCaps(t *testing.T) (e2eCapState, bool) {
	t.Helper()
	var st e2eCapState
	all, err := e.nodes()
	if err != nil {
		t.Errorf("before lowering the caps: %v", err)
		return st, false
	}
	var moved []string
	for _, n := range all {
		switch n.family() {
		case e2eFamilyRAPL, e2eFamilyNVIDIA, e2eFamilyDVFS:
			moved = append(moved, n.Name)
		}
	}
	if err := e.waitSeriesAge(t); err != nil {
		t.Errorf("before lowering the caps: %v", err)
		return st, false
	}
	var problems []string
	err = e2eEventually(e.twinFollow(), func() error {
		problems = problems[:0]
		snap, err := e.snapshot()
		if err != nil {
			return err
		}
		truth, haveTruth, err := e.joulieTruth(snap)
		if err != nil {
			return err
		}
		twins, err := e.twins()
		if err != nil {
			return err
		}
		st = e2eCapState{at: time.Now(), haveTruth: haveTruth, truth: truth, twin: map[string]float64{}}
		for name, tw := range twins {
			if pm := tw.Status.PowerMeasurement; pm != nil {
				st.twin[name] = pm.MeasuredNodePowerW
			}
		}
		if !haveTruth {
			return nil
		}
		for _, name := range moved {
			want, got := truth[name], st.twin[name]
			if math.Abs(got-want) > math.Max(simcluster.CheckAbsFloorW, simcluster.CheckTolerance*want) {
				problems = append(problems, fmt.Sprintf("node %s: NodeTwin %.1f W, truth %.1f", name, got, want))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d NodeTwins off the truth", len(problems))
		}
		return nil
	})
	if err != nil {
		st.unsteady = fmt.Sprintf("the NodeTwins did not read the truth before the caps were lowered (%v): %s", err, strings.Join(problems, "; "))
		t.Log(st.unsteady)
	}
	pct := fmt.Sprintf("%.0f", e2eCapPctAfter)
	out, err := e.helm("upgrade", e2eReleaseJoulie, filepath.Join(e.Repo, "charts", "joulie"), "-n", scenario.NamespaceJoulie,
		"--reuse-values", "--skip-crds",
		"--set-string", "controllerManager.env.CPU_PERFORMANCE_CAP_PCT_OF_MAX="+pct,
		"--set-string", "controllerManager.env.GPU_PERFORMANCE_CAP_PCT_OF_MAX="+pct)
	if err != nil {
		t.Errorf("helm upgrade to %s %% caps: %v\n%s", pct, err, out)
		return st, false
	}
	if err := e.rollout(scenario.NamespaceJoulie, e2eDeployController, 5*time.Minute); err != nil {
		t.Errorf("the controller manager after the upgrade: %v", err)
		return st, false
	}
	t.Logf("caps lowered to %s %%", pct)
	return st, true
}

// lowerCapsOnce returns a function that runs lowerCaps on its first call
// and returns that call's result on every call. The 50 % checks call it
// inside their subtests, which a -run filter that leaves them out never
// starts; a failure of the upgrade still fails t, the whole test.
func (e *e2eEnv) lowerCapsOnce(t *testing.T) func() (e2eCapState, bool) {
	var (
		done    bool
		before  e2eCapState
		lowered bool
	)
	return func() (e2eCapState, bool) {
		if !done {
			done = true
			before, lowered = e.lowerCaps(t)
		}
		return before, lowered
	}
}

// follow polls until, on every node, the truth and the NodeTwin have
// both gone down from before: by at least 80 % of the truth's drop when
// proportional, else by anything, without the NodeTwin reading below the
// truth (more than the check tolerance): a rate over a window that spans
// the change lies between the old power and the new. The check observes the smallest twin
// drop over truth drop it saw on the proportional nodes.
func (e *e2eEnv) follow(c *e2eCase, before e2eCapState, proportional, lower []e2eNode) {
	c.Helper()
	if !before.haveTruth {
		c.Logf("mode %s gives Joulie no node power, so no NodeTwin follows the caps", e.Mode)
		return
	}
	if before.unsteady != "" {
		c.Errorf("%s", before.unsteady)
		return
	}
	var problems []string
	minRatio := math.Inf(1)
	err := e2eEventually(e.twinFollow(), func() error {
		problems = problems[:0]
		minRatio = math.Inf(1)
		snap, err := e.snapshot()
		if err != nil {
			return err
		}
		truth, _, err := e.joulieTruth(snap)
		if err != nil {
			return err
		}
		twins, err := e.twins()
		if err != nil {
			return err
		}
		twinNow := func(name string) float64 {
			if pm := twins[name].Status.PowerMeasurement; pm != nil {
				return pm.MeasuredNodePowerW
			}
			return 0
		}
		for _, n := range append(append([]e2eNode(nil), proportional...), lower...) {
			if got, want := twinNow(n.Name), truth[n.Name]; got < want-math.Max(simcluster.CheckAbsFloorW, simcluster.CheckTolerance*want) {
				problems = append(problems, fmt.Sprintf("node %s: the NodeTwin reads %.1f W, below the truth %.1f", n.Name, got, want))
			}
		}
		for _, n := range proportional {
			drop := before.truth[n.Name] - truth[n.Name]
			twinDrop := before.twin[n.Name] - twinNow(n.Name)
			if drop <= 0 {
				problems = append(problems, fmt.Sprintf("node %s: the truth did not go down (%.1f W to %.1f)", n.Name, before.truth[n.Name], truth[n.Name]))
				continue
			}
			ratio := twinDrop / drop
			minRatio = math.Min(minRatio, ratio)
			if ratio < 0.8 {
				problems = append(problems, fmt.Sprintf("node %s: the NodeTwin went down %.1f W (%.1f to %.1f), less than 80 %% of the truth's %.1f",
					n.Name, twinDrop, before.twin[n.Name], twinNow(n.Name), drop))
			}
		}
		for _, n := range lower {
			if truth[n.Name] >= before.truth[n.Name] {
				problems = append(problems, fmt.Sprintf("node %s: the truth did not go down (%.1f W to %.1f)", n.Name, before.truth[n.Name], truth[n.Name]))
			}
			if twinNow(n.Name) >= before.twin[n.Name] {
				problems = append(problems, fmt.Sprintf("node %s: the NodeTwin did not go down (%.1f W to %.1f)", n.Name, before.twin[n.Name], twinNow(n.Name)))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if len(proportional) > 0 && !math.IsInf(minRatio, 1) {
		c.observe(math.Round(minRatio*1000)/1000, ">= 0.8")
	}
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("within 3 rate windows plus 2 controller intervals of the new limits: %v", err)
	}
}

// E09 at 50 %: after the upgrade each RAPL package's limit is 50 % of the
// maximum (82.5 W of 165), and within 3 rate windows plus 2 controller
// intervals each NodeTwin's measured power falls by at least 80 % of the
// truth's drop.
func e2eE09RAPLAt50(c *e2eCase, before e2eCapState, lowered bool) {
	e := c.env
	nodes := e.nodesOf(c, e2eFamilyRAPL)
	if len(nodes) == 0 {
		c.Skipf("no CPU-only Node caps through RAPL")
	}
	if !lowered {
		c.Fatalf("the caps were not lowered (see the helm upgrade above)")
	}
	e.eventuallyNone(c, e.agentSettle(), func() []string { return e.capsAt(nodes, semconv.HwTypeCPU, e2eCapPctAfter) })
	e.follow(c, before, nodes, nil)
}

// E10 at 50 %: after the upgrade each NVIDIA GPU's limit is 50 % of the
// maximum (NVL 200 W, SXM 350), its power at most 1 % above it, and each
// NodeTwin follows as in E09; the DVFS hosts still report dvfs applied, and
// both their truth and their NodeTwin are lower than at 80 %.
func e2eE10GPUAndDVFSAt50(c *e2eCase, before e2eCapState, lowered bool) {
	e := c.env
	gpus := e.nodesOf(c, e2eFamilyNVIDIA)
	dvfs := e.nodesOf(c, e2eFamilyDVFS)
	if len(gpus)+len(dvfs) == 0 {
		c.Skipf("no NVIDIA GPU Node and no DVFS host")
	}
	if !lowered {
		c.Fatalf("the caps were not lowered (see the helm upgrade above)")
	}
	e.eventuallyNone(c, e.agentSettle(), func() []string {
		return append(e.capsAt(gpus, semconv.HwTypeGPU, e2eCapPctAfter), e.dvfsApplied(dvfs)...)
	})
	e.follow(c, before, gpus, dvfs)
}

// deploymentPods returns the UIDs of the Running pods matching the selector
// of Deployment name in joulie-system, and of all of them.
func (e *e2eEnv) deploymentPods(name string) (running map[string]bool, all map[string]bool, err error) {
	ctx, cancel := e.ctx()
	defer cancel()
	var d appsv1.Deployment
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: scenario.NamespaceJoulie, Name: name}, &d); err != nil {
		return nil, nil, err
	}
	pods, err := e.pods(scenario.NamespaceJoulie, client.MatchingLabels(d.Spec.Selector.MatchLabels))
	if err != nil {
		return nil, nil, err
	}
	running, all = map[string]bool{}, map[string]bool{}
	for i := range pods {
		all[string(pods[i].UID)] = true
		if e2eRunning(&pods[i]) {
			running[string(pods[i].UID)] = true
		}
	}
	return running, all, nil
}

// e2eTail is the end of a script's output, for a message.
func e2eTail(out string, lines int) string {
	ls := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n")
}

// E14, after E09 and E10: reload.sh scheduler --force ends with a new
// Running extender pod, which a hostNetwork extender reaches only with the
// Recreate strategy (D17); reload.sh agent without --force, with the agent
// binary unchanged, imports nothing and restarts no agent.
func e2eE14ReloadRollsOrDoesNothing(c *e2eCase) {
	e := c.env
	_, before, err := e.deploymentPods(e2eDeployExtender)
	if err != nil {
		c.Fatalf("extender pods: %v", err)
	}
	out, err := e.script("reload", map[string]string{"SIMCLUSTER_FORCE_RELOAD": ""}, "scheduler", "--force")
	if err != nil {
		c.Fatalf("reload.sh scheduler --force: %v\n%s", err, e2eTail(out, 30))
	}
	if err := e2eEventually(e.agentSettle(), func() error {
		running, _, err := e.deploymentPods(e2eDeployExtender)
		if err != nil {
			return err
		}
		for uid := range running {
			if !before[uid] {
				return nil
			}
		}
		return fmt.Errorf("no new Running extender pod (running %v, before %v)", e2eKeys(running), e2eKeys(before))
	}); err != nil {
		c.Errorf("after reload.sh scheduler --force: %v", err)
	}

	agentPods := func() (map[string]bool, map[string]int64, error) {
		pods, err := e.pods(scenario.NamespaceJoulie, client.MatchingLabels{e2eLabelComponent: e2eComponentEmulated})
		if err != nil {
			return nil, nil, err
		}
		uids := map[string]bool{}
		for i := range pods {
			uids[string(pods[i].UID)] = true
		}
		deps, err := e.agentDeployments()
		if err != nil {
			return nil, nil, err
		}
		gens := map[string]int64{}
		for node, d := range deps {
			gens[node] = d.Generation
		}
		return uids, gens, nil
	}
	uids0, gens0, err := agentPods()
	if err != nil {
		c.Fatalf("agent pods: %v", err)
	}
	images0, err := e2eEnvFile(filepath.Join(e.Dir, "images.env"))
	if err != nil {
		c.Fatalf("%v", err)
	}
	out, err = e.script("reload", map[string]string{"SIMCLUSTER_FORCE_RELOAD": ""}, "agent")
	if err != nil {
		c.Fatalf("reload.sh agent: %v\n%s", err, e2eTail(out, 30))
	}
	if strings.Contains(out, ": importing") {
		c.Errorf("reload.sh agent with an unchanged binary imported an image:\n%s", e2eTail(out, 30))
	}
	images1, err := e2eEnvFile(filepath.Join(e.Dir, "images.env"))
	if err != nil {
		c.Fatalf("%v", err)
	}
	if images0["IMAGE_AGENT"] != images1["IMAGE_AGENT"] {
		c.Errorf("reload.sh agent changed the agent image from %s to %s with an unchanged binary", images0["IMAGE_AGENT"], images1["IMAGE_AGENT"])
	}
	uids1, gens1, err := agentPods()
	if err != nil {
		c.Fatalf("agent pods: %v", err)
	}
	if !reflect.DeepEqual(uids0, uids1) {
		c.Errorf("reload.sh agent restarted agents: pods %v, now %v", e2eKeys(uids0), e2eKeys(uids1))
	}
	if !reflect.DeepEqual(gens0, gens1) {
		c.Errorf("reload.sh agent changed agent Deployments: generations %v, now %v", gens0, gens1)
	}
}

// E15: status.sh exits 0.
func e2eE15StatusIsClean(c *e2eCase) {
	e := c.env
	var out string
	err := e2eEventually(e.agentSettle(), func() error {
		var err error
		out, err = e.script("status", nil)
		return err
	})
	if err != nil {
		c.Errorf("status.sh: %v\n%s", err, e2eTail(out, 40))
	}
}
