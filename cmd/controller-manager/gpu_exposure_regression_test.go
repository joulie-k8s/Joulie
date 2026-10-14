package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// A node that exposes its GPUs as MIG devices of the mixed strategy
// (nvidia.com/mig-<profile>), as renamed time-sliced replicas
// (nvidia.com/gpu.shared) or through DRA (no node resource at all) is
// planned as a GPU node: the GPU cap of its profile and no CPU percent cap.
// The GPU feature discovery product suffixes (-MIG-<profile>, -SHARED) do
// not keep it out of the catalog or put it in a family of its own.

// exposureNode is the Node of the 8x H100 NVL profile of the emulator with
// its GPUs exposed as exposure: the resources the device plugin advertises
// and the labels GPU feature discovery writes (simulator/pkg/hwemu), managed
// by Joulie and currently in profile.
func exposureNode(t *testing.T, name, exposure, profile string) *corev1.Node {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["nvidia-h100-nvl-8gpu"]
	p.GPUs.Exposure.V = exposure
	switch exposure {
	case "mig-single", "mig-mixed":
		p.GPUs.MIGProfile, p.GPUs.MIGDevicesPerGPU = "1g.12gb", 7
	case "timeslice-rename", "timeslice-norename":
		p.GPUs.TimesliceReplicas = 4
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	n := p.NodeObject(name)
	n.Labels["joulie.io/managed"] = "true"
	n.Labels["joulie.io/power-profile"] = profile
	return n
}

// reconcileExposures runs one static_partition reconcile with the default
// GPU product label keys. Fraction 0 keeps one performance node per
// hardware family, fraction 1 every node.
func reconcileExposures(t *testing.T, frac float64, gpuWriteAbsolute bool, objs ...runtime.Object) func(string) v1alpha1.NodeTwinSpec {
	t.Helper()
	reader, client, dyn := fakeClients(t, objs...)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, "static_partition", frac, 0, 1, 5, 10,
		100, 60, false, 100, 60, gpuWriteAbsolute,
		map[string]GPUModelCaps{}, parseCSVList(defaultGPUProductLabelKeys)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return func(node string) v1alpha1.NodeTwinSpec { return twinSpec(t, dyn, node) }
}

// gpuCapOf describes the GPU cap and the CPU percent cap of a spec.
func gpuCapOf(s v1alpha1.NodeTwinSpec) (gpuPct, gpuW, cpuPct string) {
	gpuPct, gpuW, cpuPct = "none", "none", "none"
	if s.GPU != nil && s.GPU.PowerCap != nil {
		if v := s.GPU.PowerCap.CapPctOfMax; v != nil {
			gpuPct = fmt.Sprintf("%g", *v)
		}
		if v := s.GPU.PowerCap.CapWattsPerGPU; v != nil {
			gpuW = fmt.Sprintf("%g", *v)
		}
	}
	if s.CPU != nil && s.CPU.PackagePowerCapPctOfMax != nil {
		cpuPct = fmt.Sprintf("%g", *s.CPU.PackagePowerCapPctOfMax)
	}
	return gpuPct, gpuW, cpuPct
}

// Every way the device plugin or a DRA driver exposes the GPUs of a node
// plans it as a GPU node, from its labels and resources alone (no
// NodeHardware yet): the GPU percent cap of its profile and no CPU percent
// cap. The suffixed products of mig-single and timeslice-norename resolve
// to the catalog's H100 NVL, so all six nodes are one family and one of
// them stays in performance.
func TestD18EveryGPUExposurePlansAGPUNode(t *testing.T) {
	exposures := []string{"whole", "mig-single", "mig-mixed", "timeslice-rename", "timeslice-norename", "dra"}
	var objs []runtime.Object
	for _, e := range exposures {
		objs = append(objs, exposureNode(t, e, e, "eco"))
	}
	spec := reconcileExposures(t, 0, false, objs...)
	performance := 0
	for _, e := range exposures {
		s := spec(e)
		want := "60"
		if s.Profile == profilePerformance {
			want = "100"
			performance++
		}
		if gpuPct, _, cpuPct := gpuCapOf(s); gpuPct != want || cpuPct != "none" {
			t.Errorf("%s (profile %s): GPU cap %s %%, CPU cap %s %%; want GPU %s %% and no CPU percent cap on a GPU node",
				e, s.Profile, gpuPct, cpuPct, want)
		}
	}
	if performance != 1 {
		t.Errorf("%d of the %d H100 NVL nodes in performance, want 1: every exposure is the same hardware family", performance, len(exposures))
	}
}

// The family split and the caps read one GPU count. A MIG node whose agent
// lists its 8 GPUs, a whole GPU node whose agent lists none, and a DRA node
// with no GPU label that only its agent knows to have GPUs are all GPU nodes
// for the split, so each keeps its own performance slot next to a CPU-only
// node with the same CPU, and all get GPU caps.
func TestD18GPUFamilyAndCapsAgree(t *testing.T) {
	const cpu = "AMD EPYC 9654 96-Core Processor"
	cases := []struct {
		name             string
		exposure         string
		agentGPU         int
		dropNVIDIALabels bool
	}{
		{"MIG node, agent lists 8 GPUs", "mig-mixed", 8, false},
		{"whole GPU node, agent lists none", "whole", 0, false},
		{"DRA node without GPU labels, agent lists 8 GPUs", "dra", 8, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gpuRaw := ""
			if c.agentGPU > 0 {
				gpuRaw = "NVIDIA H100 NVL"
			}
			node := exposureNode(t, "g0", c.exposure, "performance")
			if c.dropNVIDIALabels {
				for k := range node.Labels {
					if strings.HasPrefix(k, "nvidia.com/") {
						delete(node.Labels, k)
					}
				}
			}
			spec := reconcileExposures(t, 0, false,
				managedNode("c0", "performance"), nodeHardwareFor(t, "c0", cpu, 192, "", 0),
				node, nodeHardwareFor(t, "g0", cpu, 192, gpuRaw, c.agentGPU))
			c0, g0 := spec("c0"), spec("g0")
			if c0.Profile != profilePerformance || g0.Profile != profilePerformance {
				t.Errorf("profiles c0=%s g0=%s, want both performance: the GPU node is a family of its own", c0.Profile, g0.Profile)
			}
			if gpuPct, _, cpuPct := gpuCapOf(g0); gpuPct != "100" || cpuPct != "none" {
				t.Errorf("g0: GPU cap %s %%, CPU cap %s %%; want GPU 100 %% and no CPU percent cap", gpuPct, cpuPct)
			}
		})
	}
}

// With GPU_WRITE_ABSOLUTE_CAPS, a node known only from labels resolves its
// watts from the catalog entry of its product, the same for every
// exposure: the GPU feature discovery suffixes -MIG-<profile> and -SHARED
// do not hide the model.
func TestD18SuffixedGPUProductResolvesTheCatalogModel(t *testing.T) {
	exposures := []string{"whole", "mig-single", "timeslice-norename"}
	var objs []runtime.Object
	for _, e := range exposures {
		objs = append(objs, exposureNode(t, e, e, "performance"))
	}
	spec := reconcileExposures(t, 1, true, objs...)
	_, want, _ := gpuCapOf(spec("whole"))
	if want == "none" {
		t.Fatalf("whole: no capWattsPerGpu, so the catalog has no H100 NVL maximum to compare with")
	}
	for _, e := range exposures[1:] {
		if _, got, _ := gpuCapOf(spec(e)); got != want {
			t.Errorf("%s: capWattsPerGpu %s, want %s as on the whole exposure", e, got, want)
		}
	}
	// The raw model, which GPU_MODEL_CAPS_JSON and the governor model
	// presets are keyed by, is the product without its suffix.
	nh := nodeHardwareFromLabels(*exposureNode(t, "s", "mig-single", "eco"), loadHardwareCatalog(), parseCSVList(defaultGPUProductLabelKeys))
	if nh.GPURawModel != "NVIDIA-H100-NVL" {
		t.Errorf("mig-single raw GPU model %q, want NVIDIA-H100-NVL", nh.GPURawModel)
	}
}

// The GPU Operator runs GPU feature discovery with the single MIG strategy
// by default, so nvidia.com/mig.strategy=single is on nodes without any
// MIG-enabled GPU, where nvidia.com/gpu.count is the physical count. A DRA
// node known only from those labels is a GPU node, and a time-sliced node
// without renaming counts its 8 GPUs, not its 32 replicas.
func TestD18SingleMIGStrategyWithoutMIGDevicesKeepsThePhysicalCount(t *testing.T) {
	names := []string{"dra-a", "dra-b", "timeslice-norename"}
	var objs []runtime.Object
	nodes := map[string]*corev1.Node{}
	for _, name := range names {
		exposure := name
		if name != "timeslice-norename" {
			exposure = "dra"
		}
		n := exposureNode(t, name, exposure, "eco")
		n.Labels["nvidia.com/mig.strategy"] = "single"
		nodes[name] = n
		objs = append(objs, n)
	}
	spec := reconcileExposures(t, 0, false, objs...)
	for _, name := range names {
		s := spec(name)
		want := "60"
		if s.Profile == profilePerformance {
			want = "100"
		}
		if gpuPct, _, cpuPct := gpuCapOf(s); gpuPct != want || cpuPct != "none" {
			t.Errorf("%s (profile %s): GPU cap %s %%, CPU cap %s %%; want GPU %s %% and no CPU percent cap on a GPU node",
				name, s.Profile, gpuPct, cpuPct, want)
		}
	}
	nh := nodeHardwareFromLabels(*nodes["timeslice-norename"], loadHardwareCatalog(), parseCSVList(defaultGPUProductLabelKeys))
	if nh.GPUCount != 8 {
		t.Errorf("timeslice-norename with mig.strategy=single: GPU count %d, want the 8 physical GPUs of nvidia.com/gpu.count", nh.GPUCount)
	}
}
