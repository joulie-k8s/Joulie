package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	policyGVR = schema.GroupVersionResource{
		Group:    "joulie.io",
		Version:  "v1alpha1",
		Resource: "powerpolicies",
	}
)

type HardwareInfo struct {
	CPUVendor  string
	GPUVendors []string
}

type PowerPolicy struct {
	Name       string
	Priority   int64
	Selector   map[string]string
	PowerWatts *float64
}

type PowerCapBackend interface {
	Name() string
	Supports(hw HardwareInfo) bool
	ApplyPackageCapWatts(watts float64) (int, error)
}

type RaplBackend struct {
	vendor string
}

func (r RaplBackend) Name() string {
	return fmt.Sprintf("%s-rapl", strings.ToLower(r.vendor))
}

func (r RaplBackend) Supports(hw HardwareInfo) bool {
	if hw.CPUVendor != r.vendor {
		return false
	}
	files, _ := raplCapFiles()
	return len(files) > 0
}

func (r RaplBackend) ApplyPackageCapWatts(watts float64) (int, error) {
	if watts <= 0 {
		return 0, fmt.Errorf("power cap watts must be > 0")
	}
	files, err := raplCapFiles()
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no RAPL cap files found under /host-sys/class/powercap")
	}

	uw := int64(watts * 1_000_000)
	payload := []byte(strconv.FormatInt(uw, 10))
	count := 0
	for _, f := range files {
		if err := os.WriteFile(f, payload, 0); err != nil {
			return count, fmt.Errorf("write %s: %w", f, err)
		}
		count++
	}
	return count, nil
}

func raplCapFiles() ([]string, error) {
	patterns := []string{
		"/host-sys/class/powercap/*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*:*/constraint_0_power_limit_uw",
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-agent] ")

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME env var is required")
	}

	reconcileEvery := 20 * time.Second
	if s := os.Getenv("RECONCILE_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			reconcileEvery = d
		}
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	backends := []PowerCapBackend{
		RaplBackend{vendor: "AuthenticAMD"},
		RaplBackend{vendor: "GenuineIntel"},
	}

	var lastApplied string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := reconcileOnce(ctx, kube, dyn, nodeName, backends, &lastApplied)
		cancel()
		if err != nil {
			log.Printf("reconcile failed: %v", err)
		}
		time.Sleep(reconcileEvery)
	}
}

func reconcileOnce(
	ctx context.Context,
	kube *kubernetes.Clientset,
	dyn dynamic.Interface,
	nodeName string,
	backends []PowerCapBackend,
	lastApplied *string,
) error {
	node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	hw := discoverHardware(node.Labels)
	policies, err := listPolicies(ctx, dyn)
	if err != nil {
		return fmt.Errorf("list powerpolicies: %w", err)
	}

	selected := selectPolicyForNode(policies, node.Labels)
	if selected == nil {
		if *lastApplied != "" {
			log.Printf("no matching policy for node %s; leaving current cap untouched", nodeName)
			*lastApplied = ""
		}
		return nil
	}
	if selected.PowerWatts == nil {
		log.Printf("policy %s has no cpu.packagePowerCapWatts; nothing to enforce", selected.Name)
		return nil
	}

	if len(hw.GPUVendors) > 0 {
		log.Printf("discovered GPUs %v on node %s; GPU caps are not implemented yet", hw.GPUVendors, nodeName)
	}

	for _, b := range backends {
		if !b.Supports(hw) {
			continue
		}
		key := fmt.Sprintf("%s|%s|%.2f", selected.Name, b.Name(), *selected.PowerWatts)
		if key == *lastApplied {
			return nil
		}
		count, err := b.ApplyPackageCapWatts(*selected.PowerWatts)
		if err != nil {
			return fmt.Errorf("backend %s apply failed: %w", b.Name(), err)
		}
		log.Printf("applied policy=%s backend=%s cap=%.2fW files=%d cpuVendor=%s", selected.Name, b.Name(), *selected.PowerWatts, count, hw.CPUVendor)
		*lastApplied = key
		return nil
	}

	log.Printf("no backend supports node %s cpuVendor=%s; expected NFD cpu label feature.node.kubernetes.io/cpu-vendor or feature.node.kubernetes.io/cpu-model.vendor_id", nodeName, hw.CPUVendor)
	return nil
}

func discoverHardware(nodeLabels map[string]string) HardwareInfo {
	hw := HardwareInfo{CPUVendor: discoverCPUVendor(nodeLabels)}

	if hasNFDGPUVendor(nodeLabels, "10de") {
		hw.GPUVendors = append(hw.GPUVendors, "nvidia")
	}
	if hasNFDGPUVendor(nodeLabels, "1002") {
		hw.GPUVendors = append(hw.GPUVendors, "amd")
	}
	if hasNFDGPUVendor(nodeLabels, "8086") {
		hw.GPUVendors = append(hw.GPUVendors, "intel")
	}

	return hw
}

func discoverCPUVendor(nodeLabels map[string]string) string {
	if v := normalizeCPUVendor(nodeLabels["feature.node.kubernetes.io/cpu-vendor"]); v != "" {
		return v
	}
	if v := normalizeCPUVendor(nodeLabels["feature.node.kubernetes.io/cpu-model.vendor_id"]); v != "" {
		return v
	}
	return ""
}

func normalizeCPUVendor(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	switch strings.ToUpper(v) {
	case "AUTHENTICAMD", "AMD":
		return "AuthenticAMD"
	case "GENUINEINTEL", "INTEL":
		return "GenuineIntel"
	default:
		return v
	}
}

func hasNFDGPUVendor(nodeLabels map[string]string, vendorHex string) bool {
	keys := []string{
		fmt.Sprintf("feature.node.kubernetes.io/pci-%s.present", vendorHex),
		fmt.Sprintf("feature.node.kubernetes.io/pci-0300_%s.present", vendorHex),
		fmt.Sprintf("feature.node.kubernetes.io/pci-0302_%s.present", vendorHex),
	}
	for _, k := range keys {
		if nodeLabels[k] == "true" {
			return true
		}
	}
	return false
}

func listPolicies(ctx context.Context, dyn dynamic.Interface) ([]PowerPolicy, error) {
	ul, err := dyn.Resource(policyGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]PowerPolicy, 0, len(ul.Items))
	for _, item := range ul.Items {
		out = append(out, parsePolicy(item))
	}
	return out, nil
}

func parsePolicy(u unstructured.Unstructured) PowerPolicy {
	p := PowerPolicy{Name: u.GetName(), Selector: map[string]string{}}

	if v, ok, _ := unstructured.NestedInt64(u.Object, "spec", "priority"); ok {
		p.Priority = v
	}
	if m, ok, _ := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels"); ok {
		p.Selector = m
	}
	if w, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "cpu", "packagePowerCapWatts"); ok {
		p.PowerWatts = &w
	}

	return p
}

func selectPolicyForNode(policies []PowerPolicy, nodeLabels map[string]string) *PowerPolicy {
	matches := make([]PowerPolicy, 0)
	for _, p := range policies {
		sel := labels.SelectorFromSet(labels.Set(p.Selector))
		if sel.Matches(labels.Set(nodeLabels)) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Priority == matches[j].Priority {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Priority > matches[j].Priority
	})
	return &matches[0]
}
