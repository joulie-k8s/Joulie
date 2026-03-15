// kubectl-joulie is a kubectl plugin that displays Joulie cluster energy state.
//
// Install: go build -o kubectl-joulie ./cmd/kubectl-joulie && mv kubectl-joulie /usr/local/bin/
// Usage:   kubectl joulie status
//          kubectl joulie recommend
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

var (
	nodeTwinStateGVR   = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwinstates"}
	nodeHardwareGVR    = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}
	workloadProfileGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "workloadprofiles"}
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "status":
		explain := hasFlag(os.Args[2:], "--explain")
		runStatus()
		if explain {
			runExplain()
		}
	case "recommend":
		runRecommend()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func printUsage() {
	fmt.Println(`Usage: kubectl joulie <command> [flags]

Commands:
  status              Show cluster energy state overview
  status --explain    Show cluster state + workload classification reasons
  recommend           Show GPU slicing and rescheduling recommendations
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
	name           string
	class          string
	headroom       float64
	coolingStress  float64
	psuStress      float64
	cpuCapPct      float64
	gpuCapPct      float64
	density        float64
	gpuSlicing     *gpuSlicingRec
	rescheduleRecs int
	lastUpdated    string
}

type gpuSlicingRec struct {
	mode         string
	sliceType    string
	slicesPerGPU int
	totalSlices  int
	reason       string
	utilGain     float64
	confidence   float64
}

func fetchTwinStates(client dynamic.Interface) []nodeState {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	list, err := client.Resource(nodeTwinStateGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot list NodeTwinStates: %v\n", err)
		os.Exit(1)
	}

	var states []nodeState
	for _, item := range list.Items {
		states = append(states, parseNodeState(item))
	}
	sort.Slice(states, func(i, j int) bool { return states[i].name < states[j].name })
	return states
}

func parseNodeState(u unstructured.Unstructured) nodeState {
	status, _, _ := unstructured.NestedMap(u.Object, "status")
	if status == nil {
		return nodeState{name: u.GetName()}
	}

	ns := nodeState{name: u.GetName()}
	ns.class, _, _ = unstructured.NestedString(u.Object, "status", "schedulableClass")
	ns.headroom, _, _ = unstructured.NestedFloat64(u.Object, "status", "predictedPowerHeadroomScore")
	ns.coolingStress, _, _ = unstructured.NestedFloat64(u.Object, "status", "predictedCoolingStressScore")
	ns.psuStress, _, _ = unstructured.NestedFloat64(u.Object, "status", "predictedPsuStressScore")
	ns.density, _, _ = unstructured.NestedFloat64(u.Object, "status", "hardwareDensityScore")
	ns.lastUpdated, _, _ = unstructured.NestedString(u.Object, "status", "lastUpdated")

	if cap, ok := status["effectiveCapState"].(map[string]interface{}); ok {
		if v, ok := cap["cpuPct"].(float64); ok {
			ns.cpuCapPct = v
		}
		if v, ok := cap["gpuPct"].(float64); ok {
			ns.gpuCapPct = v
		}
	}

	if recs, ok := status["rescheduleRecommendations"].([]interface{}); ok {
		ns.rescheduleRecs = len(recs)
	}

	if gs, ok := status["gpuSlicingRecommendation"].(map[string]interface{}); ok {
		rec := &gpuSlicingRec{}
		rec.mode, _ = gs["mode"].(string)
		rec.sliceType, _ = gs["sliceType"].(string)
		if v, ok := gs["slicesPerGPU"].(int64); ok {
			rec.slicesPerGPU = int(v)
		} else if v, ok := gs["slicesPerGPU"].(float64); ok {
			rec.slicesPerGPU = int(v)
		}
		if v, ok := gs["totalSlices"].(int64); ok {
			rec.totalSlices = int(v)
		} else if v, ok := gs["totalSlices"].(float64); ok {
			rec.totalSlices = int(v)
		}
		rec.reason, _ = gs["reason"].(string)
		rec.utilGain, _ = gs["estimatedUtilizationGain"].(float64)
		rec.confidence, _ = gs["confidence"].(float64)
		ns.gpuSlicing = rec
	}

	return ns
}

func runStatus() {
	client := newClient()
	states := fetchTwinStates(client)

	if len(states) == 0 {
		fmt.Println("No NodeTwinState resources found. Is Joulie installed?")
		return
	}

	// Summary
	var eco, perf, draining, unknown int
	var totalCooling, totalPSU, totalHeadroom float64
	var peakCooling float64
	var peakCoolingNode string
	var gpuSlicingCount, reschedCount int

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
		if s.gpuSlicing != nil {
			gpuSlicingCount++
		}
		reschedCount += s.rescheduleRecs
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

	if gpuSlicingCount > 0 {
		fmt.Printf("  GPU slicing recommendations: %d nodes\n", gpuSlicingCount)
	}
	if reschedCount > 0 {
		fmt.Printf("  Reschedule recommendations: %d workloads\n", reschedCount)
	}

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

func runRecommend() {
	client := newClient()
	states := fetchTwinStates(client)

	if len(states) == 0 {
		fmt.Println("No NodeTwinState resources found. Is Joulie installed?")
		return
	}

	// GPU slicing recommendations
	hasGPU := false
	for _, s := range states {
		if s.gpuSlicing != nil {
			hasGPU = true
			break
		}
	}

	if hasGPU {
		fmt.Println("GPU SLICING RECOMMENDATIONS")
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tMODE\tSLICE TYPE\tSLICES/GPU\tTOTAL\tUTIL GAIN\tCONFIDENCE")
		for _, s := range states {
			if s.gpuSlicing == nil {
				continue
			}
			r := s.gpuSlicing
			sliceType := r.sliceType
			if sliceType == "" {
				sliceType = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t+%.0f%%\t%.0f%%\n",
				s.name, r.mode, sliceType, r.slicesPerGPU, r.totalSlices,
				r.utilGain, r.confidence*100)
		}
		w.Flush()

		// Print reasons
		fmt.Println()
		for _, s := range states {
			if s.gpuSlicing != nil && s.gpuSlicing.reason != "" {
				confLabel := "low"
				if s.gpuSlicing.confidence >= 0.7 {
					confLabel = "high"
				} else if s.gpuSlicing.confidence >= 0.4 {
					confLabel = "medium"
				}
				fmt.Printf("  %s: %s (confidence: %s)\n", s.name, s.gpuSlicing.reason, confLabel)
			}
		}
	} else {
		fmt.Println("GPU SLICING RECOMMENDATIONS")
		fmt.Println("  No GPU slicing recommendations (nodes may lack GPU slicing support or workload data)")
	}

	// Reschedule recommendations
	fmt.Println()
	hasResched := false
	for _, s := range states {
		if s.rescheduleRecs > 0 {
			hasResched = true
			break
		}
	}

	if hasResched {
		fmt.Println("RESCHEDULE RECOMMENDATIONS")
		for _, s := range states {
			if s.rescheduleRecs > 0 {
				fmt.Printf("  %s: %d workload(s) recommended for migration (class: %s, cooling: %.0f%%, PSU: %.0f%%)\n",
					s.name, s.rescheduleRecs, s.class, s.coolingStress, s.psuStress)
			}
		}
	} else {
		fmt.Println("RESCHEDULE RECOMMENDATIONS")
		fmt.Println("  No workloads recommended for rescheduling")
	}
}

func runExplain() {
	client := newClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	list, err := client.Resource(workloadProfileGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot list WorkloadProfiles: %v\n", err)
		return
	}

	if len(list.Items) == 0 {
		fmt.Println("\nWORKLOAD CLASSIFICATION")
		fmt.Println("  No WorkloadProfile resources found. Is the classifier running?")
		return
	}

	type wpRow struct {
		namespace string
		name      string
		class     string
		reason    string
		confPct   string
		cpuBound  string
		gpuBound  string
	}

	var rows []wpRow
	for _, item := range list.Items {
		ns := item.GetNamespace()
		name := item.GetName()

		class, _, _ := unstructured.NestedString(item.Object, "status", "criticality", "class")
		reason, _, _ := unstructured.NestedString(item.Object, "status", "classificationReason")
		confidence, _, _ := unstructured.NestedFloat64(item.Object, "status", "confidence")
		cpuBound, _, _ := unstructured.NestedString(item.Object, "status", "cpu", "bound")
		gpuBound, _, _ := unstructured.NestedString(item.Object, "status", "gpu", "bound")

		if class == "" {
			class = "-"
		}
		if reason == "" {
			reason = "-"
		}
		if cpuBound == "" {
			cpuBound = "-"
		}
		if gpuBound == "" {
			gpuBound = "-"
		}

		rows = append(rows, wpRow{
			namespace: ns,
			name:      name,
			class:     class,
			reason:    reason,
			confPct:   fmt.Sprintf("%.0f%%", confidence*100),
			cpuBound:  cpuBound,
			gpuBound:  gpuBound,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].namespace != rows[j].namespace {
			return rows[i].namespace < rows[j].namespace
		}
		return rows[i].name < rows[j].name
	})

	fmt.Println()
	fmt.Println("WORKLOAD CLASSIFICATION")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tCLASS\tCONFIDENCE\tCPU BOUND\tGPU BOUND\tREASON")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.namespace, r.name, r.class, r.confPct, r.cpuBound, r.gpuBound, r.reason)
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
