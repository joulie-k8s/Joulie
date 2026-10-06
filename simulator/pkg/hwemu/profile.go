package hwemu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Profiles are parsed in two passes. The first works on a generic tree:
// each file's provenance is resolved on its own, so a mapping-level src never
// reaches a value written in another file, and then extends is resolved by a
// deep merge in which maps merge and lists and values are replaced. The second
// pass walks that tree along the Profile type: it rejects every key the schema
// does not have, requires a source on every fact, and hands the result to
// encoding/json with unknown fields disallowed.

// renderLeaf is one resolved value of the first pass: a scalar or a list of
// scalars, and the source that covers it ("" when none does).
type renderLeaf struct {
	V   any
	Src string
}

// renderHeaderKeys are the top-level keys that are never facts and are never
// inherited through extends.
var renderHeaderKeys = map[string]bool{"schemaVersion": true, "name": true, "description": true, "extends": true}

var renderNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// LoadProfiles reads every dir/*.yaml of fsys, resolves extends and validates
// each profile. A profile's name must equal its file name without .yaml,
// because both appear in host paths and object names.
func LoadProfiles(fsys fs.FS, dir string) (map[string]*Profile, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no profiles (*.yaml) in %s", dir)
	}
	trees := map[string]map[string]any{}
	var errs []error
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tree, err := renderParseFile(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		want := strings.TrimSuffix(path.Base(f), ".yaml")
		if name, _ := tree["name"].(string); name != want {
			errs = append(errs, fmt.Errorf("%s: name is %q, want %q, the file name without .yaml", f, name, want))
			continue
		}
		trees[want] = tree
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	merged := map[string]map[string]any{}
	var resolve func(name string, chain []string) (map[string]any, error)
	resolve = func(name string, chain []string) (map[string]any, error) {
		if m, ok := merged[name]; ok {
			return m, nil
		}
		for _, c := range chain {
			if c == name {
				return nil, fmt.Errorf("extends cycle: %s -> %s", strings.Join(chain, " -> "), name)
			}
		}
		tree, ok := trees[name]
		if !ok {
			return nil, fmt.Errorf("%s extends %q, which is not a profile in %s", chain[len(chain)-1], name, dir)
		}
		out := tree
		if base, _ := tree["extends"].(string); base != "" {
			parent, err := resolve(base, append(chain, name))
			if err != nil {
				return nil, err
			}
			out = renderMergeProfiles(parent, tree)
		}
		merged[name] = out
		return out, nil
	}

	names := make([]string, 0, len(trees))
	for name := range trees {
		names = append(names, name)
	}
	sort.Strings(names)
	profiles := map[string]*Profile{}
	for _, name := range names {
		tree, err := resolve(name, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		p, err := renderDecode(tree)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if err := p.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		profiles[name] = p
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return profiles, nil
}

// renderParseFile is the first pass on one file: YAML to a generic tree, with
// duplicate keys rejected, then provenance resolved on the file's own values.
func renderParseFile(b []byte) (map[string]any, error) {
	j, err := yaml.YAMLToJSONStrict(b)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("a profile is a mapping")
	}
	if v, ok := root["schemaVersion"].(json.Number); !ok || v.String() != "1" {
		return nil, fmt.Errorf("schemaVersion is %v, want 1", root["schemaVersion"])
	}
	out := map[string]any{}
	for k, v := range root {
		if renderHeaderKeys[k] || k == "sources" {
			out[k] = v
			continue
		}
		r, err := renderResolve(v, "", k)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

// renderResolve turns every value below x into a renderLeaf. A mapping-level
// src covers every value below it; {v: X, src: Y} sets the source of X, and
// when X holds mappings, Y covers the values inside them. Label groups are
// kept as written: their src is a field of the group, not a provenance to
// spread over the label values.
func renderResolve(x any, src, at string) (any, error) {
	if at == "node.labels" {
		return x, nil
	}
	switch t := x.(type) {
	case map[string]any:
		s := src
		if raw, ok := t["src"]; ok {
			str, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s.src is %v, want a string", at, raw)
			}
			s = str
		}
		if renderIsLeafForm(t) {
			v := t["v"]
			if renderIsScalarValue(v) {
				return renderLeaf{V: v, Src: s}, nil
			}
			return renderResolve(v, s, at)
		}
		out := map[string]any{}
		for k, c := range t {
			if k == "src" {
				continue
			}
			r, err := renderResolve(c, s, renderJoinPath(at, k))
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		if renderIsScalarValue(t) {
			return renderLeaf{V: t, Src: src}, nil
		}
		out := make([]any, len(t))
		for i, e := range t {
			r, err := renderResolve(e, src, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return renderLeaf{V: t, Src: src}, nil
	}
}

// renderIsLeafForm reports whether m is written {v: X} or {v: X, src: Y}.
func renderIsLeafForm(m map[string]any) bool {
	if _, ok := m["v"]; !ok {
		return false
	}
	for k := range m {
		if k != "v" && k != "src" {
			return false
		}
	}
	return true
}

// renderIsScalarValue reports whether v is a scalar or a list of scalars,
// which a fact holds whole.
func renderIsScalarValue(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return false
	case []any:
		for _, e := range t {
			switch e.(type) {
			case map[string]any, []any:
				return false
			}
		}
		return true
	default:
		return true
	}
}

// renderMergeProfiles lays child over its resolved parent. Maps merge, lists
// and values are replaced. A label group and a source entry are replaced
// whole, so a child never re-attributes the parent's labels or mixes two
// sources under one id. name, description and extends are the child's own.
func renderMergeProfiles(parent, child map[string]any) map[string]any {
	out := renderMergeAt(parent, child, "").(map[string]any)
	for k := range renderHeaderKeys {
		if v, ok := child[k]; ok {
			out[k] = v
		} else {
			delete(out, k)
		}
	}
	return out
}

func renderMergeAt(base, child any, at string) any {
	bm, ok := base.(map[string]any)
	cm, ok2 := child.(map[string]any)
	if !ok || !ok2 || at == "node.labels.*" || at == "sources.*" {
		return child
	}
	out := make(map[string]any, len(bm)+len(cm))
	for k, v := range bm {
		out[k] = v
	}
	for k, cv := range cm {
		next := renderJoinPath(at, k)
		if at == "node.labels" || at == "sources" {
			next = at + ".*"
		}
		if bv, ok := out[k]; ok {
			out[k] = renderMergeAt(bv, cv, next)
		} else {
			out[k] = cv
		}
	}
	return out
}

// renderDecode is the second pass: it strips the resolved tree to plain JSON
// along the Profile type and decodes it with unknown fields disallowed.
func renderDecode(tree map[string]any) (*Profile, error) {
	plain, err := renderStrip(tree, reflect.TypeOf(Profile{}), "")
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// renderStrip converts x to what encoding/json decodes into t: a fact
// position becomes {"v", "src"} and must carry a source; every other value
// loses its provenance. A key t does not have is an error naming its path.
func renderStrip(x any, t reflect.Type, at string) (any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if renderIsFactType(t) {
		leaf, ok := x.(renderLeaf)
		if !ok {
			return nil, fmt.Errorf("%s: want a value or {v, src}, got %s", at, renderDescribe(x))
		}
		if leaf.Src == "" {
			return nil, fmt.Errorf("%s: no source; write {v: X, src: ...} or set src on an enclosing mapping", at)
		}
		field, _ := t.FieldByName("V")
		v, err := renderStrip(leaf.V, field.Type, at)
		if err != nil {
			return nil, err
		}
		return map[string]any{"v": v, "src": leaf.Src}, nil
	}
	if leaf, ok := x.(renderLeaf); ok {
		if leaf.V == nil {
			return nil, nil
		}
		if t.Kind() == reflect.Struct || t.Kind() == reflect.Map {
			return nil, fmt.Errorf("%s: want a mapping, got %v", at, leaf.V)
		}
		return renderStrip(leaf.V, t, at)
	}
	switch v := x.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := map[string]reflect.StructField{}
			for i := 0; i < t.NumField(); i++ {
				sf := t.Field(i)
				if sf.IsExported() {
					fields[renderJSONName(sf)] = sf
				}
			}
			out := map[string]any{}
			for _, k := range renderSortedKeys(v) {
				sf, ok := fields[k]
				if !ok {
					return nil, fmt.Errorf("%s: unknown key %q", renderJoinPath(at, k), k)
				}
				s, err := renderStrip(v[k], sf.Type, renderJoinPath(at, k))
				if err != nil {
					return nil, err
				}
				out[k] = s
			}
			return out, nil
		case reflect.Map:
			out := map[string]any{}
			for _, k := range renderSortedKeys(v) {
				s, err := renderStrip(v[k], t.Elem(), renderJoinPath(at, k))
				if err != nil {
					return nil, err
				}
				out[k] = s
			}
			return out, nil
		case reflect.Interface:
			return v, nil
		default:
			return nil, fmt.Errorf("%s: want a %s, got a mapping", at, t.Kind())
		}
	case []any:
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return nil, fmt.Errorf("%s: want a %s, got a list", at, t.Kind())
		}
		out := make([]any, len(v))
		for i, e := range v {
			s, err := renderStrip(e, t.Elem(), fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	default:
		return renderCheckScalar(v, t, at)
	}
}

// renderCheckScalar rejects a scalar of the wrong type with its path, which
// encoding/json would report without one.
func renderCheckScalar(v any, t reflect.Type, at string) (any, error) {
	ok := true
	switch t.Kind() {
	case reflect.String:
		_, ok = v.(string)
	case reflect.Bool:
		_, ok = v.(bool)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		_, ok = v.(json.Number)
	case reflect.Slice, reflect.Array, reflect.Map:
		ok = v == nil
	}
	if !ok {
		return nil, fmt.Errorf("%s: want a %s, got %v (%T)", at, t.Kind(), v, v)
	}
	return v, nil
}

func renderDescribe(x any) string {
	if m, ok := x.(map[string]any); ok {
		return fmt.Sprintf("a mapping with keys %v", renderSortedKeys(m))
	}
	if _, ok := x.([]any); ok {
		return "a list"
	}
	return fmt.Sprintf("%v", x)
}

func renderSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Values the schema accepts, by field.
var (
	renderNFDModes      = map[string]bool{"": true, "present": true, "absent": true}
	renderNumberings    = map[string]bool{"": true, "socket-major-smt-last": true}
	renderControlTypes  = map[string]bool{"intel-rapl": true, "intel-rapl-mmio": true}
	renderRepeats       = map[string]bool{"per-package": true, "per-die": true, "once": true}
	renderRejectWrites  = map[string]bool{"": true, "locked": true, "disabled": true}
	renderGroupings     = map[string]bool{"": true, "per-cpu": true, "per-core": true}
	renderHSMPLayouts   = map[string]bool{"platform": true}
	renderGPUVendors    = map[string]bool{"nvidia": true, "amd": true}
	renderLabellers     = map[string]bool{"gfd": true, "listed": true}
	renderPowerSensors  = map[string]bool{"": true, "input": true, "average": true, "both": true}
	renderPPT1Modes     = map[string]bool{"": true, "absent": true, "present": true}
	renderCPUModels     = map[string]bool{"": true, "analytic": true, "catalog-curve": true}
	renderSettleTaus    = map[string]bool{"": true, "timeWindow": true}
	renderSourceKinds   = map[string]bool{renderKindPublished: true, renderKindCorpus: true, renderKindMeasured: true}
	renderSourceIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	renderPCIClassRegex = regexp.MustCompile(`^0x[0-9a-fA-F]{6}$`)
	renderPCIIDRegex    = regexp.MustCompile(`^0x[0-9a-fA-F]{4}$`)
	renderTaintEffects  = map[corev1.TaintEffect]bool{corev1.TaintEffectNoSchedule: true, corev1.TaintEffectPreferNoSchedule: true, corev1.TaintEffectNoExecute: true}
)

// renderExposures lists the GPU exposure modes per vendor. AMD has only
// whole devices and DRA so far.
var renderExposures = map[string]map[string]bool{
	"nvidia": {"whole": true, "mig-single": true, "mig-mixed": true, "timeslice-rename": true, "timeslice-norename": true, "dra": true},
	"amd":    {"whole": true, "dra": true},
}

// renderToolVariants lists the pinned tool variants (package smi) and the vendor
// each tool reports on.
var renderToolVariants = map[string]struct {
	vendor   string
	variants map[string]bool
}{
	"nvidia-smi": {"nvidia", map[string]bool{"default": true}},
	"rocm-smi":   {"amd", map[string]bool{"current": true, "legacy-agent-contract": true}},
	"amd-smi":    {"amd", map[string]bool{"7.1": true, "7.2": true}},
}

// Validate checks a resolved profile: provenance on every fact by the
// grammar of fact.go, values the schema accepts, and the arithmetic that ties
// fields together. It returns every problem it finds, joined.
func (p *Profile) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{p.Name}, args...)...))
	}
	if p.SchemaVersion != 1 {
		fail("schemaVersion is %d, want 1", p.SchemaVersion)
	}
	if !renderNamePattern.MatchString(p.Name) {
		fail("name %q does not match [a-z0-9-]+", p.Name)
	}
	if strings.TrimSpace(p.Description) == "" {
		fail("description is empty")
	}

	ids := make([]string, 0, len(p.Sources))
	for id := range p.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := p.Sources[id]
		if !renderSourceIDRegex.MatchString(id) {
			fail("source id %q is not [A-Za-z0-9_-]+", id)
		}
		if !renderSourceKinds[s.Kind] {
			fail("source %q has kind %q, want published, corpus or measured", id, s.Kind)
		}
		if strings.TrimSpace(s.Ref) == "" {
			fail("source %q has no ref", id)
		}
		if s.Kind == renderKindPublished && strings.Contains(s.Ref, renderCatalogPath) {
			fail("source %q lists the hardware catalog as published; the catalog may appear only inside an assumed: reason", id)
		}
	}

	renderWalkFacts(reflect.ValueOf(p).Elem(), "", func(at string, f renderFact) {
		if src := f.renderSource(); src != "" {
			if err := renderCheckSrc(src, p.Sources); err != nil {
				fail("%s: %v", at, err)
			}
		} else if f.renderValueSet() {
			fail("%s: has a value but no source", at)
		}
	})

	errs = append(errs, renderValidateNode(p)...)
	errs = append(errs, renderValidatePCI(p)...)
	errs = append(errs, renderValidateCPU(p)...)
	errs = append(errs, renderValidatePowercap(p)...)
	errs = append(errs, renderValidateCPUFreq(p)...)
	errs = append(errs, renderValidateHSMP(p)...)
	errs = append(errs, renderValidateGPUs(p)...)
	if !renderCPUModels[p.Physics.CPU.Model] {
		fail("physics.cpu.model %q is not analytic or catalog-curve", p.Physics.CPU.Model)
	}
	if !renderSettleTaus[p.Physics.RAPL.SettleTau] {
		fail("physics.rapl.settleTau %q is not timeWindow", p.Physics.RAPL.SettleTau)
	}
	return errors.Join(errs...)
}

func renderValidateNode(p *Profile) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: node."+format, append([]any{p.Name}, args...)...))
	}
	n := p.Node
	if !renderNFDModes[n.NFD] {
		fail("nfd %q is not present or absent", n.NFD)
	}
	seen := map[string]string{}
	for _, g := range renderSortedGroups(n.Labels) {
		lg := n.Labels[g]
		if err := renderCheckSrc(lg.Src, p.Sources); err != nil {
			fail("labels.%s.src: %v", g, err)
		}
		for k := range lg.Values {
			if k == "" {
				fail("labels.%s has an empty key", g)
			}
			if other, ok := seen[k]; ok {
				fail("label %q is in both groups %s and %s", k, other, g)
			}
			seen[k] = g
		}
	}
	for i, t := range n.Taints {
		if t.Key == "" || !renderTaintEffects[t.Effect] {
			fail("taints[%d] needs a key and an effect of NoSchedule, PreferNoSchedule or NoExecute", i)
		}
	}
	if n.Capacity.CPU.V <= 0 || n.Capacity.MemoryKi.V <= 0 {
		fail("capacity needs cpu and memoryKi above 0")
	}
	if _, err := renderAllocatable(p); err != nil {
		fail("reserved: %v", err)
	}
	for name, v := range n.ExtendedResources {
		if strings.HasPrefix(name, "nvidia.com/") || strings.HasPrefix(name, "amd.com/") {
			fail("extendedResources.%s: GPU resources come from gpus.exposure", name)
		}
		if v < 0 {
			fail("extendedResources.%s is negative", name)
		}
	}
	return errs
}

func renderValidatePCI(p *Profile) []error {
	var errs []error
	check := func(at string, d PCIDevice, needClass bool) {
		if (needClass || d.Class.V != "") && !renderPCIClassRegex.MatchString(d.Class.V) {
			errs = append(errs, fmt.Errorf("%s: %s.class %q is not 0x plus 6 hex digits", p.Name, at, d.Class.V))
		}
		if !renderPCIIDRegex.MatchString(d.Vendor.V) {
			errs = append(errs, fmt.Errorf("%s: %s.vendor %q is not 0x plus 4 hex digits", p.Name, at, d.Vendor.V))
		}
		if d.Device.V != "" && !renderPCIIDRegex.MatchString(d.Device.V) {
			errs = append(errs, fmt.Errorf("%s: %s.device %q is not 0x plus 4 hex digits", p.Name, at, d.Device.V))
		}
	}
	for i, d := range p.PCI {
		check(fmt.Sprintf("pci[%d]", i), d, true)
	}
	// A GPU's class may be unknown, as in a capture: it then gets no NFD
	// label. An AMD GPU needs its vendor, which the fake tools filter on.
	if g := p.GPUs; g != nil && (g.PCI.Vendor.V != "" || g.Vendor == "amd") {
		check("gpus.pci", g.PCI, false)
	}
	return errs
}

func renderValidateCPU(p *Profile) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: cpu."+format, append([]any{p.Name}, args...)...))
	}
	c := p.CPU
	if c.VendorID.V == "" || c.ModelName.V == "" {
		fail("vendorID and modelName are required")
	}
	if c.Sockets.V < 1 || c.CoresPerSocket.V < 1 || c.ThreadsPerCore.V < 1 {
		fail("sockets, coresPerSocket and threadsPerCore must be at least 1")
	}
	if c.DiesPerPackage.V < 0 {
		fail("diesPerPackage is negative")
	}
	if !renderNumberings[c.Numbering] {
		fail("numbering %q is not socket-major-smt-last", c.Numbering)
	}
	if c.OneSocketPerVCPU && (c.CoresPerSocket.V != 1 || c.ThreadsPerCore.V != 1) {
		fail("oneSocketPerVCPU needs coresPerSocket 1 and threadsPerCore 1")
	}
	if c.CPUInfoVerbatim == "" {
		if n := int64(c.Sockets.V * c.CoresPerSocket.V * c.ThreadsPerCore.V); n != p.Node.Capacity.CPU.V {
			fail("sockets x coresPerSocket x threadsPerCore is %d, but node.capacity.cpu is %d", n, p.Node.Capacity.CPU.V)
		}
	}
	return errs
}

func renderValidatePowercap(p *Profile) []error {
	if p.Powercap == nil {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: powercap."+format, append([]any{p.Name}, args...)...))
	}
	seen := map[string]bool{}
	var zone func(at string, z ZoneSpec, child bool)
	zone = func(at string, z ZoneSpec, child bool) {
		verbs, err := renderCountVerbs(z.NameFormat)
		switch {
		case err != nil:
			fail("%s.nameFormat: %v", at, err)
		case z.NameFormat == "":
			fail("%s.nameFormat is empty", at)
		case child && z.Repeat != "":
			fail("%s.repeat: a child zone repeats with its parent, so it has no repeat", at)
		case !child && !renderRepeats[z.Repeat]:
			fail("%s.repeat %q is not per-package, per-die or once", at, z.Repeat)
		case verbs != map[string]int{"per-package": 1, "per-die": 2}[z.Repeat]:
			fail("%s.nameFormat %q has %d %%d verbs, which repeat %q does not fill", at, z.NameFormat, verbs, z.Repeat)
		}
		if z.Enabled.V != 0 && z.Enabled.V != 1 {
			fail("%s.enabled is %d, want 0 or 1", at, z.Enabled.V)
		}
		if z.EnergyUnitNJ.V <= 0 && z.MaxEnergyRangeUJ == nil {
			fail("%s needs energyUnitNJ or maxEnergyRangeUJ", at)
		}
		if z.PowerUnitUW.V < 0 {
			fail("%s.powerUnitUW is negative", at)
		}
		for i, c := range z.Constraints {
			if c.Name == "" {
				fail("%s.constraints[%d].name is empty", at, i)
			}
			if !renderRejectWrites[c.RejectWrites.V] {
				fail("%s.constraints[%d].rejectWrites %q is not locked or disabled", at, i, c.RejectWrites.V)
			}
		}
		for name := range z.ExtraFiles {
			if name == "" || strings.Contains(name, "/") {
				fail("%s.extraFiles: %q is not a file name", at, name)
			}
		}
		for i, c := range z.Children {
			zone(fmt.Sprintf("%s.children[%d]", at, i), c, true)
		}
	}
	for i, ct := range p.Powercap.ControlTypes {
		if !renderControlTypes[ct.Name] {
			fail("controlTypes[%d].name %q is not intel-rapl or intel-rapl-mmio", i, ct.Name)
		}
		if seen[ct.Name] {
			fail("controlTypes[%d].name %q is listed twice", i, ct.Name)
		}
		seen[ct.Name] = true
		for j, z := range ct.Zones {
			zone(fmt.Sprintf("controlTypes[%d].zones[%d]", i, j), z, false)
		}
	}
	return errs
}

// renderCountVerbs counts the %d verbs of a zone name format. %% is a
// literal percent; any other verb is an error.
func renderCountVerbs(format string) (int, error) {
	n := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if i+1 >= len(format) {
			return 0, fmt.Errorf("%q ends in a bare %%", format)
		}
		switch format[i+1] {
		case 'd':
			n++
		case '%':
		default:
			return 0, fmt.Errorf("%q has verb %%%c; only %%d is filled", format, format[i+1])
		}
		i++
	}
	return n, nil
}

func renderValidateCPUFreq(p *Profile) []error {
	f := p.CPUFreq
	if f == nil {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: cpufreq."+format, append([]any{p.Name}, args...)...))
	}
	if f.Driver.V == "" {
		fail("driver is empty")
	}
	if !renderGroupings[f.PolicyGrouping.V] {
		fail("policyGrouping %q is not per-cpu or per-core", f.PolicyGrouping.V)
	}
	if f.Minimal {
		return errs
	}
	if f.CPUInfoMinKHz.V <= 0 || f.CPUInfoMaxKHz.V < f.CPUInfoMinKHz.V {
		fail("needs 0 < cpuinfoMinKHz <= cpuinfoMaxKHz, got %d and %d", f.CPUInfoMinKHz.V, f.CPUInfoMaxKHz.V)
	}
	govs := strings.Fields(f.AvailableGovernors.V)
	if len(govs) == 0 {
		fail("availableGovernors is empty")
	} else if !renderContains(govs, f.Governor.V) {
		fail("governor %q is not in availableGovernors %q", f.Governor.V, f.AvailableGovernors.V)
	}
	for _, khz := range f.AvailableKHz.V {
		if khz < f.CPUInfoMinKHz.V || khz > f.CPUInfoMaxKHz.V {
			fail("availableKHz entry %d is outside [cpuinfoMinKHz, cpuinfoMaxKHz]", khz)
		}
	}
	if f.Boost.V != 0 && f.Boost.V != 1 {
		fail("boost is %d, want 0 or 1", f.Boost.V)
	}
	return errs
}

func renderValidateHSMP(p *Profile) []error {
	h := p.HSMP
	if h == nil {
		return nil
	}
	var errs []error
	if !renderHSMPLayouts[h.Layout.V] {
		errs = append(errs, fmt.Errorf("%s: hsmp.layout %q is not platform", p.Name, h.Layout.V))
	}
	if h.CapDefaultUW.V <= 0 || h.CapMaxUW.V < h.CapDefaultUW.V {
		errs = append(errs, fmt.Errorf("%s: hsmp needs 0 < capDefaultUW <= capMaxUW", p.Name))
	}
	return errs
}

func renderValidateGPUs(p *Profile) []error {
	g := p.GPUs
	if g == nil {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: gpus."+format, append([]any{p.Name}, args...)...))
	}
	if !renderGPUVendors[g.Vendor] {
		fail("vendor %q is not nvidia or amd", g.Vendor)
		return errs
	}
	if g.Count.V < 1 {
		fail("count must be at least 1")
	}
	if g.Product.V == "" {
		fail("product is empty")
	}
	l := g.Limits
	if l.MinW.V < 0 || l.MinW.V > l.DefaultW.V || l.DefaultW.V > l.MaxW.V || l.MaxW.V <= 0 {
		fail("limits need 0 <= minW <= defaultW <= maxW and maxW > 0")
	}
	if !renderExposures[g.Vendor][g.Exposure.V] {
		fail("exposure %q is not supported for %s", g.Exposure.V, g.Vendor)
	}
	switch g.Exposure.V {
	case "mig-single", "mig-mixed":
		if g.MIGProfile == "" || g.MIGDevicesPerGPU < 1 {
			fail("exposure %s needs migProfile and migDevicesPerGPU", g.Exposure.V)
		}
	case "timeslice-rename", "timeslice-norename":
		if g.TimesliceReplicas < 1 {
			fail("exposure %s needs timesliceReplicas", g.Exposure.V)
		}
	}
	if !renderLabellers[g.Labeller.V] {
		fail("labeller %q is not gfd or listed", g.Labeller.V)
	} else if g.Labeller.V == "gfd" && g.Vendor != "nvidia" {
		fail("labeller gfd labels NVIDIA GPUs only")
	}
	if !renderPowerSensors[g.PowerSensor.V] {
		fail("powerSensor %q is not input, average or both", g.PowerSensor.V)
	}
	if !renderPPT1Modes[g.PPT1.V] {
		fail("ppt1 %q is not absent or present", g.PPT1.V)
	}
	for field := range g.FieldSupport {
		if g.Vendor != "nvidia" {
			fail("fieldSupport applies to nvidia-smi only")
			break
		}
		if _, ok := renderNVMLFields[field]; !ok {
			fail("fieldSupport.%s is not a query field the emulated nvidia-smi has", field)
		}
	}
	for _, d := range g.SecondaryDies {
		if g.Vendor != "amd" || d < 1 || d >= g.Count.V {
			fail("secondaryDies entry %d must be an AMD device index in [1, count)", d)
		}
	}
	names := map[string]bool{}
	for i, t := range g.Tools {
		tv, ok := renderToolVariants[t.Name]
		switch {
		case !ok:
			fail("tools[%d].name %q is not nvidia-smi, rocm-smi or amd-smi", i, t.Name)
		case tv.vendor != g.Vendor:
			fail("tools[%d] %s reports on %s GPUs, not %s", i, t.Name, tv.vendor, g.Vendor)
		case !tv.variants[t.Variant]:
			fail("tools[%d] %s has no variant %q", i, t.Name, t.Variant)
		case names[t.Name]:
			fail("tools[%d] %s is listed twice", i, t.Name)
		}
		names[t.Name] = true
	}
	return errs
}

func renderSortedGroups(m map[string]LabelGroup) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func renderContains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
