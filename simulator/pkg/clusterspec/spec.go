package clusterspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	sigsjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
	goyaml "sigs.k8s.io/yaml/goyaml.v2"
)

// specReservedPrefix is the prefix of the labels and annotations the spec
// sets itself (LabelCluster, LabelPool, AnnotationProfile).
const specReservedPrefix = "sim.joulie.io/"

// specHwLabelPrefix is the prefix of the hardware labels the standalone
// simulator invents. Joulie reads them before the NFD and GFD labels, so on
// an emulated Node they would hide what the profile renders.
const specHwLabelPrefix = "joulie.io/hw."

// specTaintEffects are the effects a taint may have.
var specTaintEffects = map[corev1.TaintEffect]bool{
	corev1.TaintEffectNoSchedule:       true,
	corev1.TaintEffectPreferNoSchedule: true,
	corev1.TaintEffectNoExecute:        true,
}

// Parse decodes one spec document strictly, the way the API server decodes
// an object: keys match case-sensitively, and an unknown or duplicate key is
// an error that names its path, except under spec.harness, which is kept as
// raw JSON. A value of the wrong type is an error too, so a string such as
// the label value "true" needs its quotes. b holds one document; a second
// one that is not empty is an error. Parse does not validate; Validate and
// Expand do.
//
// yaml.UnmarshalStrict is not enough here: it matches keys the way
// encoding/json does, ignoring case, so it would take Count for count, or
// keep one of the two values when both are given.
func Parse(b []byte) (*Spec, error) {
	js, err := yaml.YAMLToJSONStrict(b)
	if err != nil {
		return nil, err
	}
	if err := specOneDocument(b); err != nil {
		return nil, err
	}
	var s Spec
	strict, err := sigsjson.UnmarshalStrict(js, &s, sigsjson.DisallowDuplicateFields, sigsjson.DisallowUnknownFields)
	if err != nil {
		return nil, err
	}
	if len(strict) > 0 {
		return nil, errors.Join(strict...)
	}
	return &s, nil
}

// specOneDocument fails when b holds a second YAML document that is not
// empty. yaml.YAMLToJSONStrict reads the first document only, so a second
// spec in the file would otherwise be dropped without a word.
func specOneDocument(b []byte) error {
	dec := goyaml.NewDecoder(bytes.NewReader(b))
	for i := 1; ; i++ {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if i > 1 && doc != nil {
			return fmt.Errorf("document %d: a spec file holds one document", i)
		}
	}
}

// LoadFile reads and parses the spec file at path. (Load is the type of a
// baseLoad, so the file loader has the longer name.)
func LoadFile(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Validate checks every rule that needs no hardware profile and returns all
// violations, joined, each naming where it is: a pool, the defaults, the
// facility or the spec itself. Expand checks the rest against the profiles:
// that each profile exists, that the overrides apply, and the labels and
// taints a profile brings. A caller that skips Expand gets none of those
// checks, so a spec is valid only when Expand returns no error.
//
// Spec, pool and Node names are DNS-1123 labels of at most 63 characters,
// so a Node's object name is its name (pkg/api.ObjectNameForNode) and each
// name is a valid label value; pool and Node names are unique. Labels may
// not set kubernetes.io/hostname, a sim.joulie.io/ key, the rack and
// cooling-zone labels (the pool fields set those) or a joulie.io/hw. key;
// annotations may not set a sim.joulie.io/ key or the KWOK annotation;
// taints may not use the KWOK key (defaults.kwok adds it) or repeat a key
// and effect. A pool's cooling zone is declared under facility.coolingZones,
// and each baseLoad value is in 0..1. spec.harness is never looked at.
func (s *Spec) Validate() error {
	var errs []error
	fail := func(at, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", at, fmt.Sprintf(format, args...)))
	}
	if s.APIVersion != APIVersion || s.Kind != Kind {
		fail("spec", "apiVersion %q and kind %q, want %s and %s", s.APIVersion, s.Kind, APIVersion, Kind)
	}
	for _, msg := range specCheckName(s.Metadata.Name) {
		fail("metadata.name", "%s", msg)
	}

	d := s.Spec.Defaults
	for _, msg := range specCheckLabels(d.Labels) {
		fail("defaults", "%s", msg)
	}
	for _, msg := range specCheckAnnotations(d.Annotations) {
		fail("defaults", "%s", msg)
	}
	for _, msg := range specCheckLoad(d.BaseLoad) {
		fail("defaults", "%s", msg)
	}

	f := s.Spec.Facility
	if f.AmbientC != nil && !specFinite(*f.AmbientC) {
		fail("facility", "ambientC %g is not a number", *f.AmbientC)
	}
	if f.AmplitudeC != nil && !(*f.AmplitudeC >= 0 && specFinite(*f.AmplitudeC)) {
		fail("facility", "amplitudeC %g is below 0", *f.AmplitudeC)
	}
	if f.PeriodH != nil && !(*f.PeriodH > 0 && specFinite(*f.PeriodH)) {
		fail("facility", "periodH %g is not above 0", *f.PeriodH)
	}
	zones := map[string]bool{}
	for i, z := range f.CoolingZones {
		at := fmt.Sprintf("facility.coolingZones[%d]", i)
		if z.Name == "" {
			fail(at, "has no name")
			continue
		}
		for _, msg := range validation.IsValidLabelValue(z.Name) {
			fail(at, "name %q: %s", z.Name, msg)
		}
		if zones[z.Name] {
			fail(at, "zone %s is declared twice", z.Name)
		}
		zones[z.Name] = true
		if !specFinite(z.OffsetC) {
			fail(at, "offsetC %g is not a number", z.OffsetC)
		}
	}

	if len(s.Spec.Pools) == 0 {
		fail("spec", "no pools")
	}
	pools, nodes := map[string]bool{}, map[string]string{}
	for i, pool := range s.Spec.Pools {
		at := "pool " + pool.Name
		if pool.Name == "" {
			at = fmt.Sprintf("pools[%d]", i)
		}
		for _, msg := range specCheckName(pool.Name) {
			fail(at, "name: %s", msg)
		}
		if pool.Name != "" && pools[pool.Name] {
			fail(at, "a second pool has this name")
		}
		pools[pool.Name] = true
		if pool.Profile == "" {
			fail(at, "names no profile")
		}
		if pool.Count < 1 {
			fail(at, "count %d is below 1", pool.Count)
		}
		if pool.StartIndex < 0 {
			fail(at, "startIndex %d is below 0", pool.StartIndex)
		}
		if pool.Rack != "" {
			for _, msg := range validation.IsValidLabelValue(pool.Rack) {
				fail(at, "rack %q: %s", pool.Rack, msg)
			}
		}
		if pool.CoolingZone != "" && !zones[pool.CoolingZone] {
			fail(at, "cooling zone %q is not declared under facility.coolingZones", pool.CoolingZone)
		}
		for _, msg := range specCheckLabels(pool.Labels) {
			fail(at, "%s", msg)
		}
		for _, msg := range specCheckAnnotations(pool.Annotations) {
			fail(at, "%s", msg)
		}
		for _, msg := range specCheckTaints(pool.Taints) {
			fail(at, "%s", msg)
		}
		for _, msg := range specCheckLoad(pool.BaseLoad) {
			fail(at, "%s", msg)
		}
		for j := 0; j < pool.Count; j++ {
			name := pool.nodeName(j)
			if msgs := specCheckName(name); len(msgs) > 0 {
				fail(at, "Node name %q: %s", name, strings.Join(msgs, "; "))
				break
			}
			if other, ok := nodes[name]; ok {
				fail(at, "Node %s is also in pool %s", name, other)
				continue
			}
			nodes[name] = pool.Name
		}
	}
	return errors.Join(errs...)
}

// kwok reports whether the spec's Nodes are KWOK Nodes: defaults.kwok, true
// when unset.
func (s *Spec) kwok() bool {
	return s.Spec.Defaults.Kwok == nil || *s.Spec.Defaults.Kwok
}

// nodeName is the name of the pool's i-th Node, <prefix>-<startIndex+i>,
// the prefix defaulting to the pool name.
func (p Pool) nodeName(i int) string {
	prefix := p.Prefix
	if prefix == "" {
		prefix = p.Name
	}
	return fmt.Sprintf("%s-%d", prefix, p.StartIndex+i)
}

// specCheckName checks a spec, pool or Node name: a DNS-1123 label, which
// is at most 63 characters and has no dot.
func specCheckName(name string) []string {
	if name == "" {
		return []string{"is empty"}
	}
	return validation.IsDNS1123Label(name)
}

// specCheckLabels checks the labels of the defaults or a pool.
func specCheckLabels(labels map[string]string) []string {
	var out []string
	for _, k := range specSortedKeys(labels) {
		for _, msg := range validation.IsQualifiedName(k) {
			out = append(out, fmt.Sprintf("label key %q: %s", k, msg))
		}
		for _, msg := range validation.IsValidLabelValue(labels[k]) {
			out = append(out, fmt.Sprintf("label %s value %q: %s", k, labels[k], msg))
		}
		if k == corev1.LabelHostname {
			out = append(out, fmt.Sprintf("label %s is set by the spec itself", k))
		} else if msg := specReservedLabel(k); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

// specReservedLabel says why a Node label with key k may come neither from
// the spec's labels nor from a profile, or returns "" when it may. The spec
// sets the sim.joulie.io/ labels, and the rack and cooling-zone labels from
// a pool's fields; a joulie.io/hw. label would hide the hardware labels the
// profile renders. kubernetes.io/hostname is not here: a profile may carry
// it (a corpus capture does), because NodeObject sets it last.
func specReservedLabel(k string) string {
	switch {
	case strings.HasPrefix(k, specReservedPrefix):
		return fmt.Sprintf("label %s is set by the spec itself", k)
	case k == LabelRack:
		return fmt.Sprintf("label %s is set from the pool's rack", k)
	case k == LabelCoolingZone:
		return fmt.Sprintf("label %s is set from the pool's coolingZone", k)
	case strings.HasPrefix(k, specHwLabelPrefix):
		return fmt.Sprintf("label %s: Joulie reads %s* before the NFD and GFD labels, so the Node would not show the hardware its profile renders", k, specHwLabelPrefix)
	}
	return ""
}

// specCheckAnnotations checks the annotations of the defaults or a pool.
func specCheckAnnotations(annotations map[string]string) []string {
	var out []string
	for _, k := range specSortedKeys(annotations) {
		for _, msg := range validation.IsQualifiedName(k) {
			out = append(out, fmt.Sprintf("annotation key %q: %s", k, msg))
		}
		if strings.HasPrefix(k, specReservedPrefix) {
			out = append(out, fmt.Sprintf("annotation %s is set by the spec itself", k))
		}
		if k == KWOKAnnotation {
			out = append(out, fmt.Sprintf("annotation %s is set by defaults.kwok", k))
		}
	}
	return out
}

// specCheckTaints checks a pool's taints. The API server rejects two taints
// with the same key and effect, so a spec may not have them either.
func specCheckTaints(taints []corev1.Taint) []string {
	var out []string
	seen := map[string]bool{}
	for i, t := range taints {
		for _, msg := range validation.IsQualifiedName(t.Key) {
			out = append(out, fmt.Sprintf("taints[%d] key %q: %s", i, t.Key, msg))
		}
		for _, msg := range validation.IsValidLabelValue(t.Value) {
			out = append(out, fmt.Sprintf("taints[%d] value %q: %s", i, t.Value, msg))
		}
		if !specTaintEffects[t.Effect] {
			out = append(out, fmt.Sprintf("taints[%d] effect %q is not NoSchedule, PreferNoSchedule or NoExecute", i, t.Effect))
		}
		if t.Key == KWOKTaintKey {
			out = append(out, fmt.Sprintf("taints[%d] %s is added by defaults.kwok", i, t.Key))
		}
		id := specTaintID(t)
		if seen[id] {
			out = append(out, fmt.Sprintf("taints[%d] repeats the key and effect %s", i, id))
		}
		seen[id] = true
	}
	return out
}

// specTaintID is what makes a taint unique on a Node: its key and effect.
func specTaintID(t corev1.Taint) string {
	return t.Key + ":" + string(t.Effect)
}

// specCheckLoad checks that each set utilization and memory intensity is in
// 0..1.
func specCheckLoad(l *Load) []string {
	if l == nil {
		return nil
	}
	var out []string
	for _, f := range []struct {
		key string
		v   *float64
	}{{"cpuUtil", l.CPUUtil}, {"gpuUtil", l.GPUUtil}, {"memoryIntensity", l.MemoryIntensity}} {
		if f.v != nil && !(*f.v >= 0 && *f.v <= 1) {
			out = append(out, fmt.Sprintf("baseLoad.%s %g is not in 0..1", f.key, *f.v))
		}
	}
	return out
}

func specFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func specSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
