// kubectl-joulie is a kubectl plugin that displays Joulie cluster energy state.
//
// Install: go build -o kubectl-joulie ./cmd/kubectl-joulie && mv kubectl-joulie /usr/local/bin/
// Usage:   kubectl joulie status
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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
		runStatus()
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
  help                Show this help`)
}

func newClient() dynamic.Interface {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := config.ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to cluster: %v\n", err)
		os.Exit(1)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create client: %v\n", err)
		os.Exit(1)
	}
	return client
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
	ns.headroom = nestedNumber(u.Object, "status", "predictedPowerHeadroomScore")
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

func runStatus() {
	client := newClient()
	states := fetchTwinStates(client)

	if len(states) == 0 {
		fmt.Println("No NodeTwin resources found. Is Joulie installed?")
		return
	}

	// Summary
	var eco, perf, draining, unknown int
	var totalCooling, totalPSU, totalHeadroom float64
	var peakCooling float64
	var peakCoolingNode string

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
	}

	n := float64(len(states))
	fmt.Println("CLUSTER ENERGY STATE")
	fmt.Printf("  Nodes: %d total", len(states))
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
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	fmt.Println()

	fmt.Printf("  Avg cooling stress: %.0f%%", totalCooling/n)
	if peakCoolingNode != "" {
		fmt.Printf("  |  Peak: %.0f%% (%s)", peakCooling, peakCoolingNode)
	}
	fmt.Println()
	fmt.Printf("  Avg PSU stress: %.0f%%\n", totalPSU/n)
	fmt.Printf("  Avg power headroom: %.0f%%\n", totalHeadroom/n)

	// Per-node table
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tCLASS\tHEADROOM\tCOOLING\tPSU\tCPU CAP\tGPU CAP\tDENSITY")
	for _, s := range states {
		fmt.Fprintf(w, "%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f\n",
			s.name, s.class,
			s.headroom, s.coolingStress, s.psuStress,
			s.cpuCapPct, s.gpuCapPct, s.density)
	}
	w.Flush()
}


// jsonPrint is a debug helper (unused in normal operation, kept for development).
func jsonPrint(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// Ensure json import is used.
var _ = json.Marshal
