package simcluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// The driver stub: one empty file under the hwemu root, which up.sh
// creates, mounted read-only at the first path of each requirement of the
// Node's GPU tools under ToolDeliveryMounted. The tools then find what they
// check for before they run, as on a node with the driver installed.
const (
	// DriverStubVolume is the name of the stub's volume in
	// EmulatedAgent.ExtraVolumes.
	DriverStubVolume = "driver-stub"
	// DriverStubPath is the stub's path under the hwemu root.
	DriverStubPath = "lib/driver-stub"
)

// agentVendorNVIDIA is the gpus.vendor of an NVIDIA profile, the vendor
// whose tree has dev/nvidiactl (hwemu renderBase).
const agentVendorNVIDIA = "nvidia"

// SpecHarness parses the spec's harness section (ParseHarness) and checks
// that every pool it lists is a pool of the spec.
func SpecHarness(s *clusterspec.Spec) (Harness, error) {
	h, err := ParseHarness(s.Spec.Harness)
	if err != nil {
		return Harness{}, err
	}
	pools := map[string]bool{}
	for _, p := range s.Spec.Pools {
		pools[p.Name] = true
	}
	var errs []error
	for _, name := range agentSortedKeys(h.Pools) {
		if !pools[name] {
			errs = append(errs, fmt.Errorf("spec.harness.pools.%s names no pool of spec %s", name, s.Metadata.Name))
		}
	}
	if len(errs) > 0 {
		return Harness{}, errors.Join(errs...)
	}
	return h, nil
}

// EmulatedAgents returns one EmulatedAgent per member, sorted by Node name:
// the members are the Nodes the agent DaemonSet reaches (EligibleNodes).
// DevNvidiactl is set for an NVIDIA profile, whose tree has dev/nvidiactl.
// Under the pool's tool delivery (Harness.ToolDelivery, install for a pool
// the spec leaves to the install) ToolDeliveryMounted, the agent mounts the
// driver stub (root/DriverStubPath) at the first path of each requirement
// of the profile's GPU tools, once per path; under ToolDeliveryNone it
// mounts nothing. root is the hwemu root on the infra Node, an absolute
// path.
func EmulatedAgents(members []clusterspec.Member, h Harness, install, root string) ([]EmulatedAgent, error) {
	if !path.IsAbs(root) {
		return nil, fmt.Errorf("emulated agents: the hwemu root %q is not an absolute path", root)
	}
	agents := make([]EmulatedAgent, 0, len(members))
	for _, m := range members {
		if m.Profile == nil {
			return nil, fmt.Errorf("emulated agents: member %s has no profile", m.Name)
		}
		a := EmulatedAgent{Name: m.Name, Profile: m.Profile.Name}
		if g := m.Profile.GPUs; g != nil {
			a.DevNvidiactl = g.Vendor == agentVendorNVIDIA
			tools, err := agentTools(g)
			if err != nil {
				return nil, fmt.Errorf("emulated agents: member %s: %w", m.Name, err)
			}
			a.ExtraVolumes, a.ExtraVolumeMounts = agentToolMounts(tools, h.ToolDelivery(m.Pool, install), root)
		}
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, nil
}

// agentTool is a GPU tool of a profile with its requirements: each one a
// list of paths of which one must exist for the tool to run (hwemu
// GPUSpec.Tools[].Requires).
type agentTool struct {
	Name     string             `json:"name"`
	Requires []agentRequirement `json:"requires"`
}

type agentRequirement struct {
	Paths []string `json:"paths"`
}

// agentTools reads the tools of g with their requirements. It goes through
// the JSON form of the profile's tools, so it reads the requirements of
// any profile type that has them and finds none in one that has not.
func agentTools(g *hwemu.GPUSpec) ([]agentTool, error) {
	b, err := json.Marshal(g.Tools)
	if err != nil {
		return nil, err
	}
	return agentToolsFromJSON(b)
}

func agentToolsFromJSON(b []byte) ([]agentTool, error) {
	var tools []agentTool
	if err := json.Unmarshal(b, &tools); err != nil {
		return nil, fmt.Errorf("GPU tools: %w", err)
	}
	for _, t := range tools {
		for i, r := range t.Requires {
			if len(r.Paths) == 0 || !path.IsAbs(r.Paths[0]) {
				return nil, fmt.Errorf("GPU tool %s: requirement %d has no absolute first path", t.Name, i)
			}
		}
	}
	return tools, nil
}

// agentToolMounts is the driver stub's volume and its mounts: one read-only
// mount at the first path of each requirement, in tool and requirement
// order, each path once. Nothing under ToolDeliveryNone or without a
// requirement.
func agentToolMounts(tools []agentTool, delivery, root string) ([]corev1.Volume, []corev1.VolumeMount) {
	if delivery == ToolDeliveryNone {
		return nil, nil
	}
	var mounts []corev1.VolumeMount
	seen := map[string]bool{}
	for _, t := range tools {
		for _, r := range t.Requires {
			p := r.Paths[0]
			if seen[p] {
				continue
			}
			seen[p] = true
			mounts = append(mounts, corev1.VolumeMount{Name: DriverStubVolume, MountPath: p, ReadOnly: true})
		}
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	file := corev1.HostPathFile
	vol := corev1.Volume{
		Name:         DriverStubVolume,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path.Join(root, DriverStubPath), Type: &file}},
	}
	return []corev1.Volume{vol}, mounts
}

// AgentsValues is the Joulie values file that sets agent.emulated.nodes to
// agents, the output of the simcluster command's agents subcommand. Each
// entry has exactly one profile key, which the scripts count.
func AgentsValues(spec string, agents []EmulatedAgent) ([]byte, error) {
	if agents == nil {
		agents = []EmulatedAgent{}
	}
	doc := map[string]any{"agent": map[string]any{"emulated": map[string]any{"nodes": agents}}}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("# Emulated Joulie agents of cluster spec %s, written by simcluster agents:\n# one per spec Node the agent DaemonSet would reach.\n", spec)
	return append([]byte(head), b...), nil
}

func agentSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
