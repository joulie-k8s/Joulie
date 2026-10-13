package clusterspec

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Expand validates the spec, applies each pool's overrides to its profile
// and returns one Member per Node, sorted by name. src is called once per
// pool, so one pool's overrides never reach another pool; the members of a
// pool share its profile. Every problem is returned, joined, each naming
// its pool: an unknown profile (the error lists the known ones), overrides
// the profile does not accept, a profile label or taint the spec owns, and
// a pool taint that repeats a profile taint's key and effect.
//
// A Node is the profile's NodeObject with, in this order of precedence, the
// profile's labels, then defaults.labels, then the pool's labels, rack and
// cooling zone, then LabelCluster and LabelPool. Its annotations are the
// profile annotation, then the defaults' and the pool's, then the KWOK
// annotation. Its taints are the profile's, then the pool's, then the KWOK
// taint. The KWOK annotation and taint are there when defaults.kwok is
// true, the default. status.nodeInfo.architecture is the kubernetes.io/arch
// label, which KWOK keeps.
func (s *Spec) Expand(src ProfileSource) ([]Member, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	offsets := map[string]float64{}
	for _, z := range s.Spec.Facility.CoolingZones {
		offsets[z.Name] = z.OffsetC
	}
	var members []Member
	var errs []error
	for _, pool := range s.Spec.Pools {
		profiles, err := src()
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", pool.Name, err)
		}
		p := profiles[pool.Profile]
		if p == nil {
			errs = append(errs, fmt.Errorf("pool %s: profile %q is not known; the known profiles are %s", pool.Name, pool.Profile, strings.Join(expandProfileNames(profiles), ", ")))
			continue
		}
		if err := expandOverrides(p, s.Metadata.Name, pool); err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", pool.Name, err))
			continue
		}
		if nodeErrs := expandCheckNode(p, pool); len(nodeErrs) > 0 {
			for _, err := range nodeErrs {
				errs = append(errs, fmt.Errorf("pool %s: %w", pool.Name, err))
			}
			continue
		}
		for i := 0; i < pool.Count; i++ {
			members = append(members, s.expandMember(p, pool, pool.nodeName(i), offsets[pool.CoolingZone]))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}

// expandOverrides sets a pool's overrides on its profile, each fact with the
// provenance "assumed:cluster spec <spec> pool <pool> override", and
// validates the result. It validates a pool without overrides too, since a
// ProfileSource need not.
func expandOverrides(p *hwemu.Profile, spec string, pool Pool) error {
	if o := pool.Overrides; o != nil {
		src := fmt.Sprintf("assumed:cluster spec %s pool %s override", spec, pool.Name)
		if o.NFD != "" {
			p.Node.NFD = o.NFD
		}
		if g := o.GPUs; g != nil {
			if p.GPUs == nil {
				return fmt.Errorf("overrides.gpus: profile %s has no GPUs", p.Name)
			}
			if g.PersistenceMode != "" {
				if err := expandCheckPersistence(p, g.PersistenceMode); err != nil {
					return err
				}
				p.GPUs.PersistenceMode = hwemu.Fact[string]{V: g.PersistenceMode, Src: src}
			}
			if g.Exposure != "" {
				p.GPUs.Exposure = hwemu.Fact[string]{V: g.Exposure, Src: src}
			}
			if g.MIGProfile != "" {
				p.GPUs.MIGProfile = g.MIGProfile
			}
			if g.MIGDevicesPerGPU != nil {
				p.GPUs.MIGDevicesPerGPU = *g.MIGDevicesPerGPU
			}
			if g.TimesliceReplicas != nil {
				p.GPUs.TimesliceReplicas = *g.TimesliceReplicas
			}
		}
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("profile %s with the pool's overrides: %w", p.Name, err)
	}
	return nil
}

// expandCheckPersistence checks a persistenceMode override: enabled or
// disabled, on NVIDIA GPUs only, since persistence mode is an NVIDIA driver
// setting. Its error names the override key, which a Profile.Validate
// error would not.
func expandCheckPersistence(p *hwemu.Profile, mode string) error {
	if mode != hwemu.PersistenceEnabled && mode != hwemu.PersistenceDisabled {
		return fmt.Errorf("overrides.gpus.persistenceMode %q is not %s or %s", mode, hwemu.PersistenceEnabled, hwemu.PersistenceDisabled)
	}
	if p.GPUs.Vendor != "nvidia" {
		return fmt.Errorf("overrides.gpus.persistenceMode: profile %s has %s GPUs, and persistence mode is an NVIDIA driver setting", p.Name, p.GPUs.Vendor)
	}
	return nil
}

// expandCheckNode returns what the profile would put on the pool's Nodes
// that the spec owns, and what the API server would refuse: a label
// specReservedLabel names, a taint on the KWOK key, which defaults.kwok
// adds, and a pool taint with the key and effect of a profile taint.
// Validate checks the defaults and the pools for the same. No builtin
// profile has such a label or taint, but a local one can: a corpus capture
// copies every label of the captured node.
func expandCheckNode(p *hwemu.Profile, pool Pool) []error {
	var errs []error
	groups := make([]string, 0, len(p.Node.Labels))
	for g := range p.Node.Labels {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		for _, k := range specSortedKeys(p.Node.Labels[g].Values) {
			if msg := specReservedLabel(k); msg != "" {
				errs = append(errs, fmt.Errorf("profile %s node.labels.%s: %s", p.Name, g, msg))
			}
		}
	}
	profile := map[string]bool{}
	for i, t := range p.Node.Taints {
		if t.Key == KWOKTaintKey {
			errs = append(errs, fmt.Errorf("profile %s node.taints[%d] %s is added by defaults.kwok", p.Name, i, t.Key))
		}
		profile[specTaintID(t)] = true
	}
	for i, t := range pool.Taints {
		if profile[specTaintID(t)] {
			errs = append(errs, fmt.Errorf("taints[%d] repeats the key and effect %s of a taint of profile %s", i, specTaintID(t), p.Name))
		}
	}
	return errs
}

// expandMember builds the Member of one Node of pool.
func (s *Spec) expandMember(p *hwemu.Profile, pool Pool, name string, offsetC float64) Member {
	node := p.NodeObject(name)
	d := s.Spec.Defaults

	labels := node.Labels
	for k, v := range d.Labels {
		labels[k] = v
	}
	for k, v := range pool.Labels {
		labels[k] = v
	}
	if pool.Rack != "" {
		labels[LabelRack] = pool.Rack
	}
	if pool.CoolingZone != "" {
		labels[LabelCoolingZone] = pool.CoolingZone
	}
	labels[LabelCluster] = s.Metadata.Name
	labels[LabelPool] = pool.Name

	annotations := node.Annotations
	for k, v := range d.Annotations {
		annotations[k] = v
	}
	for k, v := range pool.Annotations {
		annotations[k] = v
	}

	for _, t := range pool.Taints {
		node.Spec.Taints = append(node.Spec.Taints, *t.DeepCopy())
	}
	if s.kwok() {
		annotations[KWOKAnnotation] = KWOKValue
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: KWOKTaintKey, Value: KWOKValue, Effect: corev1.TaintEffectNoSchedule})
	}
	node.Status.NodeInfo.Architecture = labels[corev1.LabelArchStable]

	return Member{
		Name:        name,
		Pool:        pool.Name,
		Rack:        pool.Rack,
		CoolingZone: pool.CoolingZone,
		ZoneOffsetC: offsetC,
		Profile:     p,
		Node:        node,
		BaseLoad:    expandLoad(d.BaseLoad, pool.BaseLoad),
	}
}

// expandLoad merges a pool's baseLoad over the defaults' field by field.
// The result has pointers of its own, so no two members share a value.
func expandLoad(defaults, pool *Load) Load {
	var out Load
	for _, l := range []*Load{defaults, pool} {
		if l == nil {
			continue
		}
		if l.CPUUtil != nil {
			v := *l.CPUUtil
			out.CPUUtil = &v
		}
		if l.GPUUtil != nil {
			v := *l.GPUUtil
			out.GPUUtil = &v
		}
		if l.MemoryIntensity != nil {
			v := *l.MemoryIntensity
			out.MemoryIntensity = &v
		}
		if l.CPUClass != "" {
			out.CPUClass = l.CPUClass
		}
		if l.GPUClass != "" {
			out.GPUClass = l.GPUClass
		}
	}
	return out
}

func expandProfileNames(m map[string]*hwemu.Profile) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
