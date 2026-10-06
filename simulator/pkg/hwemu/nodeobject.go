package hwemu

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// renderProfileAnnotation names the profile a fake Node was built from. No
// Joulie component reads node annotations; the harness and the simulator do.
const renderProfileAnnotation = "sim.joulie.io/profile"

// renderPodsCapacity is the kubelet's default --max-pods.
const renderPodsCapacity = 110

// NodeObject is the Kubernetes Node a real node of this profile would
// register as name: the label groups plus the generated NFD and GFD labels,
// the profile's taints, and capacity in logical CPUs, Ki of memory, 110 pods
// and the GPU resources of gpus.exposure. Allocatable is capacity minus
// node.reserved. Status has no conditions, because KWOK Stages own them. The
// profile must be valid.
func (p *Profile) NodeObject(name string) *corev1.Node {
	capacity := renderCapacity(p)
	allocatable, err := renderAllocatable(p)
	if err != nil {
		// Validate rejects a reservation that does not parse or exceeds
		// capacity; an unvalidated profile gets no reservation.
		allocatable = capacity.DeepCopy()
	}
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      renderNodeLabels(p, name),
			Annotations: map[string]string{renderProfileAnnotation: p.Name},
		},
		Status: corev1.NodeStatus{Capacity: capacity, Allocatable: allocatable},
	}
	if len(p.Node.Taints) > 0 {
		node.Spec.Taints = append([]corev1.Taint(nil), p.Node.Taints...)
	}
	return node
}

// renderCapacity is the node's capacity.
func renderCapacity(p *Profile) corev1.ResourceList {
	out := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(p.Node.Capacity.CPU.V, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(p.Node.Capacity.MemoryKi.V*1024, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(renderPodsCapacity, resource.DecimalSI),
	}
	for name, v := range renderGPUResources(p.GPUs) {
		out[name] = *resource.NewQuantity(v, resource.DecimalSI)
	}
	for name, v := range p.Node.ExtendedResources {
		out[corev1.ResourceName(name)] = *resource.NewQuantity(v, resource.DecimalSI)
	}
	return out
}

// renderAllocatable is capacity minus node.reserved cpu and memory.
func renderAllocatable(p *Profile) (corev1.ResourceList, error) {
	out := renderCapacity(p)
	for name, raw := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    p.Node.Reserved.CPU,
		corev1.ResourceMemory: p.Node.Reserved.Memory,
	} {
		if raw == "" {
			continue
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", name, raw, err)
		}
		left := out[name]
		if left.Cmp(q) < 0 {
			return nil, fmt.Errorf("%s %q exceeds capacity %s", name, raw, left.String())
		}
		left.Sub(q)
		out[name] = left
	}
	return out, nil
}

// renderGPUResources is what the device plugin advertises for each exposure
// mode (NVIDIA/k8s-device-plugin README.md at d7265fdcf89f, lines 250-263,
// 402):
//
//   - whole: <vendor>/gpu = N;
//   - mig-single: nvidia.com/gpu = N*m;
//   - mig-mixed: nvidia.com/mig-<profile> = N*m and no nvidia.com/gpu;
//   - timeslice-rename: nvidia.com/gpu.shared = N*r;
//   - timeslice-norename: nvidia.com/gpu = N*r;
//   - dra: nothing, because ResourceSlices are not node resources.
func renderGPUResources(g *GPUSpec) map[corev1.ResourceName]int64 {
	if g == nil {
		return nil
	}
	n := int64(g.Count.V)
	domain := "nvidia.com"
	if g.Vendor == "amd" {
		domain = "amd.com"
	}
	switch g.Exposure.V {
	case "whole":
		return map[corev1.ResourceName]int64{corev1.ResourceName(domain + "/gpu"): n}
	case "mig-single":
		return map[corev1.ResourceName]int64{"nvidia.com/gpu": n * int64(g.MIGDevicesPerGPU)}
	case "mig-mixed":
		return map[corev1.ResourceName]int64{corev1.ResourceName("nvidia.com/mig-" + g.MIGProfile): n * int64(g.MIGDevicesPerGPU)}
	case "timeslice-rename":
		return map[corev1.ResourceName]int64{"nvidia.com/gpu.shared": n * int64(g.TimesliceReplicas)}
	case "timeslice-norename":
		return map[corev1.ResourceName]int64{"nvidia.com/gpu": n * int64(g.TimesliceReplicas)}
	}
	return nil
}
