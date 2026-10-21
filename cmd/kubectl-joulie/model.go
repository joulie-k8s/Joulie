package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/fsm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	nodeTwinGVR     = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}
	nodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}
)

// errNotInstalled is returned when the cluster serves no NodeTwin, which
// means Joulie's CRDs are not installed.
var errNotInstalled = errors.New("the cluster has no NodeTwin resource: is Joulie installed? (https://joulie-k8s.github.io/Joulie/docs/getting-started/)")

// The schedulable classes of NodeTwin.status.schedulableClass.
const (
	classPerformance = "performance"
	classEco         = "eco"
	classDraining    = "draining"
	classUnknown     = "unknown"
)

// The pod classes the scheduler extender tells apart.
const (
	podClassPerformance = "performance"
	podClassStandard    = "standard"
)

// cluster is one read of everything the plugin shows.
type cluster struct {
	taken time.Time
	nodes []nodeView
	pods  []podView
	// podsErr is why the pods could not be listed: the pod columns and the
	// placement verdict are then unknown, not empty.
	podsErr error
}

// nodeView is one managed node: its NodeTwin, its NodeHardware, its Node
// and the pods bound to it.
type nodeView struct {
	name    string
	class   string
	profile string // spec.profile, what the controller manager asked for

	cpuModel string
	sockets  int
	cores    int
	gpuModel string
	gpus     int
	hasHW    bool

	powerSource string
	measuredW   float64
	cappedW     float64
	tdpW        float64
	trendWMin   float64

	headroom float64
	cooling  float64
	psu      float64
	pue      float64

	cpuCapPct     float64
	gpuCapPct     float64
	capKnown      bool // the twin's status has an effectiveCapState
	gpuCapPlanned bool
	cpuControl    *v1alpha1.ControlResult
	gpuControl    *v1alpha1.ControlResult
	cpuGovernor   *v1alpha1.ControlResult
	gpuGovernor   *v1alpha1.ControlResult

	agent agentState

	cpuAllocPct float64
	memAllocPct float64
	gpuAllocPct float64
	hasGPU      bool
	pods        int
	perfPods    int

	updated time.Time

	twin *v1alpha1.NodeTwin
	hw   *v1alpha1.NodeHardware
	node *corev1.Node
}

// agentState is the AgentReporting condition of the twin.
type agentState struct {
	known     bool
	reporting bool
	reason    string
}

// podView is one pod bound to a node.
type podView struct {
	namespace string
	name      string
	node      string
	nodeClass string
	class     string
	phase     corev1.PodPhase
	cpuMilli  int64
	gpus      float64
	started   time.Time
}

// misplaced is a performance pod on an eco node: what the scheduler
// extender exists to prevent. A draining node is a performance node waiting
// for its performance pods to finish, and the extender lets performance
// pods onto a node of unknown class, so neither counts.
func (p podView) misplaced() bool {
	return p.class == podClassPerformance && p.nodeClass == classEco
}

// readCluster lists the NodeTwins, NodeHardwares, Nodes and pods of the
// cluster, the four lists in parallel, and joins them. namespace limits the
// pods to one namespace, "" lists them all.
func readCluster(ctx context.Context, c clients, namespace string) (cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		wg       sync.WaitGroup
		twins    *unstructured.UnstructuredList
		hws      *unstructured.UnstructuredList
		nodes    *corev1.NodeList
		pods     *corev1.PodList
		twinErr  error
		nodesErr error
		podsErr  error
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		twins, twinErr = c.dyn.Resource(nodeTwinGVR).List(ctx, metav1.ListOptions{})
	}()
	go func() {
		defer wg.Done()
		// NodeHardware is optional: a node without an agent has none, and a
		// user may lack the permission to list them.
		hws, _ = c.dyn.Resource(nodeHardwareGVR).List(ctx, metav1.ListOptions{})
	}()
	go func() {
		defer wg.Done()
		nodes, nodesErr = c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	}()
	go func() {
		defer wg.Done()
		pods, podsErr = c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
		})
	}()
	wg.Wait()

	if twinErr != nil {
		if apierrors.IsNotFound(twinErr) {
			return cluster{}, errNotInstalled
		}
		return cluster{}, fmt.Errorf("listing NodeTwins: %w", twinErr)
	}
	if nodesErr != nil {
		return cluster{}, fmt.Errorf("listing Nodes: %w", nodesErr)
	}
	tw, err := decodeTwins(twins)
	if err != nil {
		return cluster{}, err
	}
	var hw []v1alpha1.NodeHardware
	if hws != nil {
		if hw, err = decodeHardware(hws); err != nil {
			return cluster{}, err
		}
	}
	var podItems []corev1.Pod
	if podsErr == nil {
		podItems = pods.Items
	}
	snap := buildCluster(tw, hw, nodes.Items, podItems, time.Now())
	if podsErr != nil {
		snap.podsErr = fmt.Errorf("listing pods: %w", podsErr)
	}
	return snap, nil
}

// decodeTwins converts the listed objects to the typed NodeTwin, which reads
// a whole number the API server sends as an int64 into its float64 field.
func decodeTwins(list *unstructured.UnstructuredList) ([]v1alpha1.NodeTwin, error) {
	out := make([]v1alpha1.NodeTwin, 0, len(list.Items))
	for _, u := range list.Items {
		var t v1alpha1.NodeTwin
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &t); err != nil {
			return nil, fmt.Errorf("decoding NodeTwin %s: %w", u.GetName(), err)
		}
		out = append(out, t)
	}
	return out, nil
}

// decodeHardware converts the listed objects to the typed NodeHardware.
func decodeHardware(list *unstructured.UnstructuredList) ([]v1alpha1.NodeHardware, error) {
	out := make([]v1alpha1.NodeHardware, 0, len(list.Items))
	for _, u := range list.Items {
		var h v1alpha1.NodeHardware
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &h); err != nil {
			return nil, fmt.Errorf("decoding NodeHardware %s: %w", u.GetName(), err)
		}
		out = append(out, h)
	}
	return out, nil
}

// buildCluster joins the objects by node name: one nodeView per NodeTwin,
// sorted by name, and one podView per pod bound to one of those nodes.
func buildCluster(twins []v1alpha1.NodeTwin, hws []v1alpha1.NodeHardware, nodes []corev1.Node, pods []corev1.Pod, now time.Time) cluster {
	hwByNode := map[string]*v1alpha1.NodeHardware{}
	for i := range hws {
		hwByNode[nodeNameOf(hws[i].Spec.NodeName, hws[i].Name)] = &hws[i]
	}
	nodeByName := map[string]*corev1.Node{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}

	c := cluster{taken: now}
	index := map[string]int{}
	for _, t := range twins {
		n := twinView(t)
		if h := hwByNode[n.name]; h != nil {
			hardwareInto(&n, h)
		}
		n.node = nodeByName[n.name]
		c.nodes = append(c.nodes, n)
	}
	sort.Slice(c.nodes, func(i, j int) bool { return c.nodes[i].name < c.nodes[j].name })
	for i := range c.nodes {
		index[c.nodes[i].name] = i
	}

	type usage struct{ cpu, mem int64 }
	requested := map[string]*usage{}
	gpuRequested := map[string]float64{}
	for i := range pods {
		p := &pods[i]
		at, ok := index[p.Spec.NodeName]
		if !ok {
			continue
		}
		n := &c.nodes[at]
		u := requested[n.name]
		if u == nil {
			u = &usage{}
			requested[n.name] = u
		}
		pv := podView{
			namespace: p.Namespace,
			name:      p.Name,
			node:      n.name,
			nodeClass: n.class,
			class:     podClassStandard,
			phase:     p.Status.Phase,
		}
		if fsm.IsPerformanceSensitivePod(p) {
			pv.class = podClassPerformance
			n.perfPods++
		}
		if p.Status.StartTime != nil {
			pv.started = p.Status.StartTime.Time
		}
		for _, ct := range p.Spec.Containers {
			if q, ok := ct.Resources.Requests[corev1.ResourceCPU]; ok {
				pv.cpuMilli += q.MilliValue()
			}
			if q, ok := ct.Resources.Requests[corev1.ResourceMemory]; ok {
				u.mem += q.Value()
			}
			pv.gpus += gpuUnits(ct.Resources.Requests)
		}
		u.cpu += pv.cpuMilli
		gpuRequested[n.name] += pv.gpus
		n.pods++
		c.pods = append(c.pods, pv)
	}
	sort.Slice(c.pods, func(i, j int) bool {
		a, b := c.pods[i], c.pods[j]
		if a.class != b.class {
			return a.class == podClassPerformance
		}
		if a.node != b.node {
			return a.node < b.node
		}
		return a.namespace+"/"+a.name < b.namespace+"/"+b.name
	})

	for i := range c.nodes {
		n := &c.nodes[i]
		node := nodeByName[n.name]
		if node == nil {
			continue
		}
		alloc := node.Status.Allocatable
		u := requested[n.name]
		if u == nil {
			u = &usage{}
		}
		n.cpuAllocPct = pct(float64(u.cpu), float64(alloc.Cpu().MilliValue()))
		n.memAllocPct = pct(float64(u.mem), float64(alloc.Memory().Value()))
		if g := gpuUnits(alloc); g > 0 {
			n.hasGPU = true
			n.gpuAllocPct = pct(gpuRequested[n.name], g)
		}
	}
	return c
}

// nodeNameOf is the node an object describes: spec.nodeName when set, else
// the object name, which pkg/api.ObjectNameForNode derives from it.
func nodeNameOf(specName, objectName string) string {
	if specName != "" {
		return specName
	}
	return objectName
}

// twinView reads what the plugin shows from one NodeTwin.
func twinView(t v1alpha1.NodeTwin) nodeView {
	st := t.Status
	n := nodeView{
		twin:     &t,
		name:     nodeNameOf(t.Spec.NodeName, t.Name),
		class:    st.SchedulableClass,
		profile:  t.Spec.Profile,
		headroom: clamp(st.PredictedPowerHeadroomScore, 0, 100),
		cooling:  clamp(st.PredictedCoolingStressScore, 0, 100),
		psu:      clamp(st.PredictedPsuStressScore, 0, 100),
		pue:      st.EstimatedPUE,
	}
	if n.class == "" {
		n.class = classUnknown
	}
	if t.Spec.GPU != nil && t.Spec.GPU.PowerCap != nil {
		n.gpuCapPlanned = true
	}
	if pm := st.PowerMeasurement; pm != nil {
		n.powerSource = pm.Source
		n.measuredW = pm.MeasuredNodePowerW
		n.cappedW = pm.NodeCappedPowerW
		n.tdpW = pm.NodeTdpW
		n.trendWMin = pm.PowerTrendWPerMin
	}
	if cs := st.EffectiveCapState; cs != nil {
		n.cpuCapPct = cs.CPUPct
		n.gpuCapPct = cs.GPUPct
		n.capKnown = true
	}
	if cs := st.ControlStatus; cs != nil {
		n.cpuControl, n.gpuControl = cs.CPU, cs.GPU
		n.cpuGovernor, n.gpuGovernor = cs.CPUGovernor, cs.GPUGovernor
	}
	for _, cond := range st.Conditions {
		if cond.Type == joulie.ConditionAgentReporting {
			n.agent = agentState{known: true, reporting: cond.Status == metav1.ConditionTrue, reason: cond.Reason}
		}
	}
	if ts, err := time.Parse(time.RFC3339, st.LastUpdated); err == nil {
		n.updated = ts
	}
	return n
}

// hardwareInto copies the hardware facts the agent published.
func hardwareInto(n *nodeView, h *v1alpha1.NodeHardware) {
	n.hasHW = true
	n.hw = h
	if cpu := h.Status.CPU; cpu != nil {
		n.cpuModel = firstNonEmpty(cpu.Model, cpu.RawModel)
		n.sockets = cpu.Sockets
		n.cores = cpu.TotalCores
	}
	if gpu := h.Status.GPU; gpu != nil && gpu.Present {
		n.gpuModel = firstNonEmpty(gpu.Model, gpu.RawModel)
		n.gpus = gpu.Count
	}
}

// gpuUnits is the GPUs in list: the units of every GPU resource pkg/api
// classifies (whole GPUs, MIG devices, partitions, shared replicas), each
// weighted by the share of a GPU it stands for (joulie.GPUResourceShare),
// so a node mixing MIG profiles adds up in GPUs. A replica weighs as the
// device it shares, so requested over allocatable is the share of replicas
// taken.
func gpuUnits(list corev1.ResourceList) float64 {
	var gpus float64
	for name, q := range list {
		gpus += float64(q.Value()) * joulie.GPUResourceShare(string(name))
	}
	return gpus
}

func pct(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return used / total * 100
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// totals is the cluster-wide summary of the header.
type totals struct {
	nodes, performance, eco, draining, unknown int
	pods, perfPods, misplaced                  int
	measuredW, cappedW, tdpW, trendWMin        float64
	measuredNodes                              int
	headroom, cooling, psu, pue                float64
	pueNodes                                   int
	peakCooling                                float64
	peakCoolingNode                            string
	agentsReporting, agentsMissing             int
	classW                                     map[string]float64 // measured power per class
}

func summarize(c cluster) totals {
	t := totals{classW: map[string]float64{}}
	t.nodes = len(c.nodes)
	for _, n := range c.nodes {
		t.classW[n.class] += n.measuredW
		switch n.class {
		case classPerformance:
			t.performance++
		case classEco:
			t.eco++
		case classDraining:
			t.draining++
		default:
			t.unknown++
		}
		if n.measuredW > 0 {
			t.measuredNodes++
		}
		t.measuredW += n.measuredW
		t.cappedW += n.cappedW
		t.tdpW += n.tdpW
		t.trendWMin += n.trendWMin
		t.headroom += n.headroom
		t.cooling += n.cooling
		t.psu += n.psu
		if n.pue > 0 {
			t.pue += n.pue
			t.pueNodes++
		}
		if n.cooling > t.peakCooling || t.peakCoolingNode == "" {
			t.peakCooling, t.peakCoolingNode = n.cooling, n.name
		}
		if n.agent.known {
			if n.agent.reporting {
				t.agentsReporting++
			} else {
				t.agentsMissing++
			}
		}
	}
	if t.nodes > 0 {
		k := float64(t.nodes)
		t.headroom /= k
		t.cooling /= k
		t.psu /= k
	}
	if t.pueNodes > 0 {
		t.pue /= float64(t.pueNodes)
	}
	for _, p := range c.pods {
		t.pods++
		if p.class == podClassPerformance {
			t.perfPods++
		}
		if p.misplaced() {
			t.misplaced++
		}
	}
	return t
}
