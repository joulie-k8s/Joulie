// kubectl-joulie is a kubectl plugin that displays Joulie cluster energy state.
//
// Install: go build -o kubectl-joulie ./cmd/kubectl-joulie && mv kubectl-joulie /usr/local/bin/
// Usage:   kubectl joulie status
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var nodeTwinGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "status":
		// Check for -W / --watch flag
		watch := false
		for _, arg := range os.Args[2:] {
			if arg == "-W" || arg == "--watch" {
				watch = true
			}
		}
		if watch {
			runStatusWatch()
		} else {
			runStatus()
		}
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: kubectl joulie <command> [flags]

Commands:
  status              Show cluster energy state overview
  status -W           Watch mode — refresh every 2 seconds
  help                Show this help`)
}

type clients struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
}

func newClients() clients {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := config.ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to cluster: %v\n", err)
		os.Exit(1)
	}
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create dynamic client: %v\n", err)
		os.Exit(1)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create kube client: %v\n", err)
		os.Exit(1)
	}
	return clients{dyn: dynClient, kube: kubeClient}
}

type nodeState struct {
	name          string
	class         string
	headroom      float64
	coolingStress float64
	psuStress     float64
	cpuCapPct     float64
	gpuCapPct     float64
	density       float64
	lastUpdated   string
	// Resource allocation
	cpuAllocPct float64
	memAllocPct float64
	gpuAllocPct float64
	hasGPU      bool
	pods        int
}

func fetchTwinStates(client dynamic.Interface) []nodeState {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	list, err := client.Resource(nodeTwinGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot list NodeTwins: %v\n", err)
		os.Exit(1)
	}

	var states []nodeState
	for _, item := range list.Items {
		states = append(states, parseNodeState(item))
	}
	sort.Slice(states, func(i, j int) bool { return states[i].name < states[j].name })
	return states
}

// nodeResources holds allocatable and requested resources for a node.
type nodeResources struct {
	cpuAllocatable int64 // millicores
	memAllocatable int64 // bytes
	gpuAllocatable int64 // count
	cpuRequested   int64
	memRequested   int64
	gpuRequested   int64
	pods           int
}

func fetchNodeResources(kube kubernetes.Interface) map[string]*nodeResources {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := map[string]*nodeResources{}

	// Get node allocatable resources
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return res
	}
	for _, n := range nodes.Items {
		nr := &nodeResources{}
		if cpu, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			nr.cpuAllocatable = cpu.MilliValue()
		}
		if mem, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			nr.memAllocatable = mem.Value()
		}
		if gpu, ok := n.Status.Allocatable["nvidia.com/gpu"]; ok {
			nr.gpuAllocatable = gpu.Value()
		}
		if gpu, ok := n.Status.Allocatable["amd.com/gpu"]; ok {
			nr.gpuAllocatable += gpu.Value()
		}
		res[n.Name] = nr
	}

	// Sum pod requests per node
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
	})
	if err != nil {
		return res
	}
	for _, p := range pods.Items {
		nodeName := p.Spec.NodeName
		if nodeName == "" {
			continue
		}
		nr, ok := res[nodeName]
		if !ok {
			continue
		}
		nr.pods++
		for _, c := range p.Spec.Containers {
			if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				nr.cpuRequested += cpu.MilliValue()
			}
			if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				nr.memRequested += mem.Value()
			}
			if gpu, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
				nr.gpuRequested += gpu.Value()
			}
			if gpu, ok := c.Resources.Requests["amd.com/gpu"]; ok {
				nr.gpuRequested += gpu.Value()
			}
		}
	}

	return res
}

// nestedNumber extracts a numeric value from an unstructured object,
// handling both float64 and int64 representations (Kubernetes stores
// whole numbers as int64 in unstructured objects).
func nestedNumber(obj map[string]interface{}, fields ...string) float64 {
	val, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func parseNodeState(u unstructured.Unstructured) nodeState {
	status, _, _ := unstructured.NestedMap(u.Object, "status")
	if status == nil {
		return nodeState{name: u.GetName()}
	}

	ns := nodeState{name: u.GetName()}
	ns.class, _, _ = unstructured.NestedString(u.Object, "status", "schedulableClass")
	ns.headroom = math.Max(0, nestedNumber(u.Object, "status", "predictedPowerHeadroomScore"))
	ns.coolingStress = nestedNumber(u.Object, "status", "predictedCoolingStressScore")
	ns.psuStress = nestedNumber(u.Object, "status", "predictedPsuStressScore")
	ns.density = nestedNumber(u.Object, "status", "hardwareDensityScore")
	ns.lastUpdated, _, _ = unstructured.NestedString(u.Object, "status", "lastUpdated")

	if cap, ok := status["effectiveCapState"].(map[string]interface{}); ok {
		ns.cpuCapPct = nestedNumber(cap, "cpuPct")
		ns.gpuCapPct = nestedNumber(cap, "gpuPct")
	}

	return ns
}

func pct(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100.0
}

func enrichWithResources(states []nodeState, resources map[string]*nodeResources) {
	for i := range states {
		nr, ok := resources[states[i].name]
		if !ok {
			continue
		}
		states[i].cpuAllocPct = pct(nr.cpuRequested, nr.cpuAllocatable)
		states[i].memAllocPct = pct(nr.memRequested, nr.memAllocatable)
		states[i].gpuAllocPct = pct(nr.gpuRequested, nr.gpuAllocatable)
		states[i].hasGPU = nr.gpuAllocatable > 0
		states[i].pods = nr.pods
	}
}

func renderStatus(out *os.File, states []nodeState) {
	if len(states) == 0 {
		fmt.Fprintln(out, "No NodeTwin resources found. Is Joulie installed?")
		return
	}

	// Summary
	var eco, perf, draining, unknown int
	var totalCooling, totalPSU, totalHeadroom float64
	var peakCooling float64
	var peakCoolingNode string
	var totalPods int

	for _, s := range states {
		switch s.class {
		case "eco":
			eco++
		case "performance":
			perf++
		case "draining":
			draining++
		default:
			unknown++
		}
		totalCooling += s.coolingStress
		totalPSU += s.psuStress
		totalHeadroom += s.headroom
		if s.coolingStress > peakCooling {
			peakCooling = s.coolingStress
			peakCoolingNode = s.name
		}
		totalPods += s.pods
	}

	n := float64(len(states))
	fmt.Fprintln(out, "CLUSTER ENERGY STATE")
	fmt.Fprintf(out, "  Nodes: %d total", len(states))
	parts := []string{}
	if eco > 0 {
		parts = append(parts, fmt.Sprintf("%d eco", eco))
	}
	if perf > 0 {
		parts = append(parts, fmt.Sprintf("%d performance", perf))
	}
	if draining > 0 {
		parts = append(parts, fmt.Sprintf("%d draining", draining))
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", unknown))
	}
	if len(parts) > 0 {
		fmt.Fprintf(out, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintln(out)

	fmt.Fprintf(out, "  Avg cooling stress: %.0f%%", totalCooling/n)
	if peakCoolingNode != "" {
		fmt.Fprintf(out, "  |  Peak: %.0f%% (%s)", peakCooling, peakCoolingNode)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Avg PSU stress: %.0f%%\n", totalPSU/n)
	fmt.Fprintf(out, "  Avg power headroom: %.0f%%\n", totalHeadroom/n)
	fmt.Fprintf(out, "  Running pods: %d\n", totalPods)

	// Per-node table
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tCLASS\tHEADROOM\tCOOLING\tCPU%\tMEM%\tGPU%\tPODS\tCPU CAP\tGPU CAP")
	for _, s := range states {
		gpuAlloc := "-"
		gpuCap := "-"
		if s.hasGPU {
			gpuAlloc = fmt.Sprintf("%.0f%%", s.gpuAllocPct)
			gpuCap = fmt.Sprintf("%.0f%%", s.gpuCapPct)
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%s\t%d\t%.0f%%\t%s\n",
			s.name, s.class,
			s.headroom, s.coolingStress,
			s.cpuAllocPct, s.memAllocPct, gpuAlloc, s.pods,
			s.cpuCapPct, gpuCap)
	}
	w.Flush()
}

func runStatus() {
	c := newClients()
	states := fetchTwinStates(c.dyn)
	resources := fetchNodeResources(c.kube)
	enrichWithResources(states, resources)
	renderStatus(os.Stdout, states)
}

func runStatusWatch() {
	c := newClients()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// First render immediately
	fmt.Print("\033[2J\033[H") // clear screen + cursor home
	states := fetchTwinStates(c.dyn)
	resources := fetchNodeResources(c.kube)
	enrichWithResources(states, resources)
	renderStatus(os.Stdout, states)
	fmt.Printf("\n  (watching — refresh every 2s, Ctrl-C to stop)")

	for range ticker.C {
		fmt.Print("\033[2J\033[H")
		states = fetchTwinStates(c.dyn)
		resources = fetchNodeResources(c.kube)
		enrichWithResources(states, resources)
		renderStatus(os.Stdout, states)
		fmt.Printf("\n  (watching — refresh every 2s, Ctrl-C to stop)")
	}
}

// jsonPrint is a debug helper (unused in normal operation, kept for development).
func jsonPrint(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// Ensure imports are used.
var _ = json.Marshal
var _ resource.Quantity
