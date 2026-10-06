package hwemu

import (
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// renderQty returns the quantity of name in list as a string, or "" when the
// list has no such resource.
func renderQty(list corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}

// H17: NodeObject reports capacity in logical CPUs and Ki, 110 pods,
// allocatable as capacity minus reserved, the profile annotation, the
// hostname label and the profile's taints, and no status conditions.
func TestH17NodeObjectCapacityAndAllocatable(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "intel-xeon-2s-rapl")
	p.Node.Taints = []corev1.Taint{{Key: "example.com/pool", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	n := p.NodeObject("node-a")
	if n.Name != "node-a" || n.Labels["kubernetes.io/hostname"] != "node-a" {
		t.Errorf("name %q hostname %q", n.Name, n.Labels["kubernetes.io/hostname"])
	}
	if n.Annotations["sim.joulie.io/profile"] != "intel-xeon-2s-rapl" {
		t.Errorf("annotations = %v", n.Annotations)
	}
	cases := []struct {
		list       corev1.ResourceList
		name       corev1.ResourceName
		want, what string
	}{
		{n.Status.Capacity, corev1.ResourceCPU, "96", "capacity cpu"},
		{n.Status.Capacity, corev1.ResourceMemory, "791917286Ki", "capacity memory"},
		{n.Status.Capacity, corev1.ResourcePods, "110", "capacity pods"},
		{n.Status.Allocatable, corev1.ResourceCPU, "95", "allocatable cpu (96 - 1)"},
		{n.Status.Allocatable, corev1.ResourceMemory, "787722982Ki", "allocatable memory (791917286Ki - 4Gi)"},
		{n.Status.Allocatable, corev1.ResourcePods, "110", "allocatable pods"},
	}
	for _, c := range cases {
		if got := renderQty(c.list, c.name); got != c.want {
			t.Errorf("%s = %q, want %s", c.what, got, c.want)
		}
	}
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "example.com/pool" {
		t.Errorf("taints = %v", n.Spec.Taints)
	}
	if len(n.Status.Conditions) != 0 {
		t.Errorf("conditions = %v, want none: KWOK Stages own them", n.Status.Conditions)
	}

	vm := renderBuiltin(t, "vm-no-powercap").NodeObject("vm")
	if got := renderQty(vm.Status.Allocatable, corev1.ResourceCPU); got != "7900m" {
		t.Errorf("vm allocatable cpu = %s, want 8 - 100m", got)
	}
}

// H17: each GPU exposure mode renders the resources the device plugin
// advertises and the labels GFD publishes. The agent gates GPU control on an
// allocatable key ending in /gpu (allocatableGPUCount in cmd/agent), so
// mig-mixed, timeslice-rename and dra must have none.
func TestH17ExposureModesRenderResourcesAndGFDLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		exposure  string
		migDev    int
		replicas  int
		resources map[string]string
		labels    map[string]string
		gpuKey    bool
	}{
		{"whole", 0, 0,
			map[string]string{"nvidia.com/gpu": "8"},
			map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/gpu.replicas": "1", "nvidia.com/gpu.product": "NVIDIA-H100-NVL"},
			true},
		{"mig-single", 7, 0,
			map[string]string{"nvidia.com/gpu": "56"},
			map[string]string{"nvidia.com/gpu.count": "56", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-MIG-1g.12gb", "nvidia.com/mig.strategy": "single"},
			true},
		{"mig-mixed", 7, 0,
			map[string]string{"nvidia.com/mig-1g.12gb": "56"},
			map[string]string{"nvidia.com/mig.strategy": "mixed", "nvidia.com/mig-1g.12gb.count": "56", "nvidia.com/gpu.product": "NVIDIA-H100-NVL"},
			false},
		{"timeslice-rename", 0, 4,
			map[string]string{"nvidia.com/gpu.shared": "32"},
			map[string]string{"nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing", "nvidia.com/gpu.product": "NVIDIA-H100-NVL", "nvidia.com/gpu.count": "8"},
			false},
		{"timeslice-norename", 0, 4,
			map[string]string{"nvidia.com/gpu": "32"},
			map[string]string{"nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-SHARED"},
			true},
		{"dra", 0, 0,
			map[string]string{},
			map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-NVL"},
			false},
	}
	for _, c := range cases {
		t.Run(c.exposure, func(t *testing.T) {
			p := renderBuiltin(t, "nvidia-h100-nvl-8gpu")
			p.GPUs.Exposure.V = c.exposure
			p.GPUs.MIGDevicesPerGPU = c.migDev
			if c.migDev > 0 {
				p.GPUs.MIGProfile = "1g.12gb"
			}
			p.GPUs.TimesliceReplicas = c.replicas
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			n := p.NodeObject("n")
			got := map[string]string{}
			gpuKey := false
			for name, q := range n.Status.Allocatable {
				if !strings.Contains(string(name), "/") {
					continue
				}
				got[string(name)] = q.String()
				if strings.HasSuffix(string(name), "/gpu") {
					gpuKey = true
				}
				if cq := n.Status.Capacity[name]; cq.Cmp(q) != 0 {
					t.Errorf("%s: capacity %s and allocatable %s differ", name, cq.String(), q.String())
				}
			}
			if renderMapString(got) != renderMapString(c.resources) {
				t.Errorf("GPU resources = %v, want %v", got, c.resources)
			}
			if gpuKey != c.gpuKey {
				t.Errorf("allocatable has a */gpu key = %v, want %v", gpuKey, c.gpuKey)
			}
			for k, v := range c.labels {
				if n.Labels[k] != v {
					t.Errorf("label %s = %q, want %q", k, n.Labels[k], v)
				}
			}
			// GFD publishes its configured strategies on every node: none
			// outside MIG and outside time-slicing.
			if c.exposure != "mig-single" && c.exposure != "mig-mixed" {
				if got := n.Labels["nvidia.com/mig.strategy"]; got != "none" {
					t.Errorf("mig.strategy = %q without MIG, want none", got)
				}
			}
			if !strings.HasPrefix(c.exposure, "timeslice") {
				if got := n.Labels["nvidia.com/gpu.sharing-strategy"]; got != "none" {
					t.Errorf("sharing-strategy = %q without time-slicing, want none", got)
				}
			}
		})
	}

	amd := renderBuiltin(t, "amd-instinct-mi300x-8gpu").NodeObject("n")
	if got := renderQty(amd.Status.Allocatable, "amd.com/gpu"); got != "8" {
		t.Errorf("mi300x amd.com/gpu = %q, want 8", got)
	}
	// The node labeller publishes each value under both prefixes, and a
	// per-value key whose value is the GPU count.
	for k, v := range map[string]string{
		"amd.com/gpu.product-name":                              "AMD_Instinct_MI300X_OAM",
		"beta.amd.com/gpu.product-name":                         "AMD_Instinct_MI300X_OAM",
		"beta.amd.com/gpu.product-name.AMD_Instinct_MI300X_OAM": "8",
		"amd.com/gpu.device-id":                                 "74a1",
		"amd.com/gpu.family":                                    "AI",
	} {
		if amd.Labels[k] != v {
			t.Errorf("mi300x: label %s = %q, want %q", k, amd.Labels[k], v)
		}
	}
	for k := range amd.Labels {
		if strings.HasPrefix(k, "nvidia.com/") {
			t.Errorf("mi300x has GFD label %s", k)
		}
	}
	if _, ok := amd.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Errorf("mi300x advertises nvidia.com/gpu")
	}
	// An exposure the vendor's plugin does not have is rejected.
	bad := renderBuiltin(t, "amd-instinct-mi300x-8gpu")
	bad.GPUs.Exposure.V = "mig-mixed"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "not supported for amd") {
		t.Errorf("err = %v, want mig-mixed rejected for AMD", err)
	}
}

func renderMapString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k+"="+m[k])
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
