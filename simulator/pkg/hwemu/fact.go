package hwemu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// UnmarshalJSON decodes a fact in the resolved form {"v": ..., "src": ...}
// that LoadProfiles produces. Any other key is an error, and so is an unknown
// field inside V, so a misspelled key next to a value is never dropped.
func (f *Fact[T]) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("a fact is {v, src}: %w", err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "v" && k != "src" {
			return fmt.Errorf("unknown key %q in a fact, which has only v and src", k)
		}
	}
	v, ok := raw["v"]
	if !ok {
		return errors.New("a fact needs v")
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.DisallowUnknownFields()
	var val T
	if err := dec.Decode(&val); err != nil {
		return err
	}
	var src string
	if s, ok := raw["src"]; ok {
		if err := json.Unmarshal(s, &src); err != nil {
			return fmt.Errorf("src: %w", err)
		}
	}
	f.V, f.Src = val, src
	return nil
}

// renderFact is what every Fact[T] satisfies, whatever T is. The loader uses
// it to find fact positions in the schema, and Validate to visit every fact.
type renderFact interface {
	renderSource() string
	renderValueSet() bool
}

func (f Fact[T]) renderSource() string { return f.Src }

// renderValueSet reports whether V differs from its zero value. An empty
// list counts as unset.
func (f Fact[T]) renderValueSet() bool {
	v := reflect.ValueOf(f.V)
	if !v.IsValid() {
		return false
	}
	if v.Kind() == reflect.Slice {
		return v.Len() > 0
	}
	return !v.IsZero()
}

var renderFactType = reflect.TypeOf((*renderFact)(nil)).Elem()

// renderIsFactType reports whether t, or what it points to, is a Fact.
func renderIsFactType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Implements(renderFactType)
}

// Provenance kinds of the grammar published:<id>[#loc], corpus:<id>[#file],
// derived:<id>[#formula], measured:<id>[#loc] and assumed:<reason>.
const (
	renderKindPublished = "published"
	renderKindCorpus    = "corpus"
	renderKindDerived   = "derived"
	renderKindMeasured  = "measured"
	renderKindAssumed   = "assumed"
)

// renderCatalogPath is the hardware catalog. It may appear inside an assumed
// reason, for strings the agent must match, but never as a published source:
// its curves are not vendor data (D14 in cmd/agent/hwemu_test.go).
const renderCatalogPath = "pkg/hwinv/assets/hardware.yaml"

// renderSrc is one parsed provenance string.
type renderSrc struct {
	Kind   string
	ID     string
	Loc    string
	Reason string
}

// renderParseSrc parses a provenance string by the grammar. It checks the
// shape only; renderCheckSrc also resolves the id against the sources.
func renderParseSrc(src string) (renderSrc, error) {
	kind, rest, ok := strings.Cut(src, ":")
	if !ok {
		return renderSrc{}, fmt.Errorf("provenance %q is not <kind>:<ref>; kinds are published, corpus, derived, measured and assumed", src)
	}
	switch kind {
	case renderKindAssumed:
		if strings.TrimSpace(rest) == "" {
			return renderSrc{}, fmt.Errorf("provenance %q needs a reason after assumed:", src)
		}
		return renderSrc{Kind: kind, Reason: rest}, nil
	case renderKindPublished, renderKindCorpus, renderKindDerived, renderKindMeasured:
		id, loc, hasLoc := strings.Cut(rest, "#")
		if id == "" {
			return renderSrc{}, fmt.Errorf("provenance %q names no source id", src)
		}
		if strings.ContainsAny(id, " \t") {
			return renderSrc{}, fmt.Errorf("provenance %q: source id %q contains a space; a reason belongs after # or in an assumed: provenance", src, id)
		}
		if hasLoc && strings.TrimSpace(loc) == "" {
			return renderSrc{}, fmt.Errorf("provenance %q has an empty location after #", src)
		}
		return renderSrc{Kind: kind, ID: id, Loc: loc}, nil
	default:
		return renderSrc{}, fmt.Errorf("provenance %q has unknown kind %q; kinds are published, corpus, derived, measured and assumed", src, kind)
	}
}

// renderCheckSrc parses src and resolves its id: published and corpus cite a
// source of their own kind, measured a measured one, derived any source.
func renderCheckSrc(src string, sources map[string]Source) error {
	ps, err := renderParseSrc(src)
	if err != nil {
		return err
	}
	if ps.Kind == renderKindAssumed {
		return nil
	}
	s, ok := sources[ps.ID]
	if !ok {
		return fmt.Errorf("provenance %q cites source %q, which the profile's sources do not list", src, ps.ID)
	}
	switch ps.Kind {
	case renderKindPublished, renderKindCorpus, renderKindMeasured:
		if s.Kind != ps.Kind {
			return fmt.Errorf("provenance %q cites source %q of kind %q as %s", src, ps.ID, s.Kind, ps.Kind)
		}
	}
	if ps.Kind == renderKindPublished && strings.Contains(s.Ref, renderCatalogPath) {
		return fmt.Errorf("provenance %q cites the hardware catalog as published; the catalog may appear only inside an assumed: reason", src)
	}
	return nil
}

// renderWalkFacts calls fn for every Fact reachable from v, with its path in
// the profile's YAML keys.
func renderWalkFacts(v reflect.Value, path string, fn func(path string, f renderFact)) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Type().Implements(renderFactType) {
		fn(path, v.Interface().(renderFact))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			renderWalkFacts(v.Field(i), renderJoinPath(path, renderJSONName(sf)), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			renderWalkFacts(v.Index(i), fmt.Sprintf("%s[%d]", path, i), fn)
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			renderWalkFacts(v.MapIndex(k), renderJoinPath(path, fmt.Sprint(k)), fn)
		}
	}
}

// renderJSONName is the key of a struct field in the profile: its json tag
// name, or the field name when it has none.
func renderJSONName(sf reflect.StructField) string {
	tag := sf.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return sf.Name
	}
	return name
}

func renderJoinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
