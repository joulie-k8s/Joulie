package hwemu

import (
	"bytes"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// renderBuiltin returns one embedded profile, parsed afresh, so a test may
// change it.
func renderBuiltin(t testing.TB, name string) *Profile {
	t.Helper()
	profiles, err := BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles[name]
	if !ok {
		t.Fatalf("no builtin profile %q", name)
	}
	return p
}

// renderBuiltinNames are the builtin generic profiles, sorted.
var renderBuiltinNames = []string{
	"amd-epyc-2s-energy-only",
	"amd-epyc-2s-hsmp",
	"amd-instinct-mi300x-8gpu",
	"intel-xeon-2s-rapl",
	"nvidia-h100-nvl-8gpu",
	"nvidia-h100-sxm-8gpu",
	"vm-no-powercap",
}

// renderMinimalProfile is a valid profile small enough to mutate in a test.
// Each fact has a source, the cpu block through its mapping-level src.
const renderMinimalProfile = `schemaVersion: 1
name: base
description: minimal test profile
sources:
  doc: {kind: published, ref: "https://example.invalid/doc"}
  cap: {kind: corpus, ref: cmd/agent/testdata/hardware/vm-no-rapl}
node:
  labels:
    cpu: {src: "assumed:test labels", values: {kubernetes.io/arch: amd64}}
  capacity:
    cpu: {v: 2, src: "derived:doc#1 x 2 x 1"}
    memoryKi: {v: 1024, src: "assumed:test"}
cpu:
  src: corpus:cap#cpuinfo
  vendorID: GenuineIntel
  modelName: test cpu
  family: 6
  sockets: 1
  coresPerSocket: 2
  threadsPerCore: 1
physics:
  cpu: {model: analytic, maxPkgW: {v: 100, src: published:doc}}
`

func renderMapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["p/"+name+".yaml"] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// H01: every embedded profile loads, resolves extends and validates, so every
// fact carries a provenance that resolves to one of its sources.
// Extends is checked on facts only a parent defines.
func TestH01BuiltinProfilesLoadWithProvenance(t *testing.T) {
	t.Parallel()
	profiles, err := BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range profiles {
		names = append(names, n)
	}
	if got, want := strings.Join(renderSortedStrings(names), ","), strings.Join(renderBuiltinNames, ","); got != want {
		t.Fatalf("builtin profiles = %s, want %s", got, want)
	}
	for name, p := range profiles {
		facts := 0
		renderWalkFacts(reflect.ValueOf(p).Elem(), "", func(at string, f renderFact) {
			if f.renderSource() != "" {
				facts++
			}
		})
		if facts < 10 {
			t.Errorf("%s: only %d facts carry a source", name, facts)
		}
	}

	// The NVIDIA profiles extend the Intel RAPL host, so their CPU block and
	// package limits are the Intel ones, with Intel's sources.
	for _, name := range []string{"nvidia-h100-nvl-8gpu", "nvidia-h100-sxm-8gpu"} {
		p := profiles[name]
		if p.CPU.VendorID.V != "GenuineIntel" || p.CPU.VendorID.Src != "corpus:corpus#cpuinfo" {
			t.Errorf("%s: cpu.vendorID = %+v, want GenuineIntel from the Intel parent", name, p.CPU.VendorID)
		}
		if p.Powercap == nil || p.Powercap.ControlTypes[0].Zones[0].Constraints[0].MaxPowerUW.V != 165000000 {
			t.Errorf("%s: want the Intel package constraint of 165 W", name)
		}
		if p.CPUFreq == nil || p.CPUFreq.Driver.V != "intel_pstate" {
			t.Errorf("%s: want the intel_pstate cpufreq block", name)
		}
	}
	sxm := profiles["nvidia-h100-sxm-8gpu"]
	if sxm.GPUs.Limits.MinW.V != 300 || sxm.GPUs.Limits.MaxW.V != 700 || sxm.GPUs.Count.V != 8 {
		t.Errorf("sxm limits %+v count %d, want 300 to 700 W on 8 GPUs", sxm.GPUs.Limits, sxm.GPUs.Count.V)
	}
	if sxm.GPUs.PCI.Vendor.V != "0x10de" || sxm.GPUs.PCI.Device.V != "" {
		t.Errorf("sxm pci %+v: want the NVL vendor kept and the device replaced", sxm.GPUs.PCI)
	}
	if !strings.Contains(sxm.Description, "SXM") {
		t.Errorf("sxm description %q is not its own", sxm.Description)
	}
	mi := profiles["amd-instinct-mi300x-8gpu"]
	if mi.CPU.VendorID.V != "AuthenticAMD" || mi.Powercap.ControlTypes[0].Zones[0].Enabled.V != 0 {
		t.Errorf("mi300x: want the energy-only EPYC host")
	}
	hsmp := profiles["amd-epyc-2s-hsmp"]
	if hsmp.CPU.Sockets.V != 2 || hsmp.CPU.Sockets.Src != "assumed:generic 2S node" {
		t.Errorf("hsmp sockets = %+v, want the parent's fact and source", hsmp.CPU.Sockets)
	}
	if len(hsmp.CPUFreq.AvailableKHz.V) != 0 || hsmp.CPUFreq.Boost.V != 1 {
		t.Errorf("hsmp cpufreq: want the table replaced by an empty list and boost inherited, got %+v", hsmp.CPUFreq)
	}
}

// H01: a fact without a provenance, or with one the grammar rejects, fails
// the load. Each case would pass a loader that only decodes values.
func TestH01RejectsBadProvenance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, from, to, want string
	}{
		{"value without src", `memoryKi: {v: 1024, src: "assumed:test"}`, `memoryKi: 1024`, "node.capacity.memoryKi: no source"},
		{"zero value without src", `maxPkgW: {v: 100, src: published:doc}`, `maxPkgW: 0`, "physics.cpu.maxPkgW: no source"},
		{"assumed without reason", `"assumed:test"}`, `"assumed:"}`, "needs a reason after assumed:"},
		{"bare assumed", `"assumed:test"}`, `"assumed"}`, "is not <kind>:<ref>"},
		{"unknown kind", `"assumed:test"}`, `"guessed:test"}`, "unknown kind"},
		{"unknown source id", `src: published:doc}`, `src: published:nodoc}`, `cites source "nodoc"`},
		{"published cites a corpus source", `src: published:doc}`, `src: published:cap}`, `of kind "corpus" as published`},
		{"catalog as published", `ref: "https://example.invalid/doc"`, `ref: "pkg/hwinv/assets/hardware.yaml"`, "hardware catalog"},
		{"reason in place of an id", `src: published:doc}`, `src: "derived:no such files"}`, "contains a space"},
		{"empty location", `src: published:doc}`, `src: "published:doc#"}`, "empty location"},
		{"label group without src", `src: "assumed:test labels", `, ``, "node.labels.cpu.src"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(renderMinimalProfile, c.from, c.to, 1)
			if body == renderMinimalProfile {
				t.Fatalf("mutation %q not found", c.from)
			}
			_, err := LoadProfiles(renderMapFS(map[string]string{"base": body}), "p")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
	if _, err := LoadProfiles(renderMapFS(map[string]string{"base": renderMinimalProfile}), "p"); err != nil {
		t.Fatalf("the unmutated profile must load: %v", err)
	}
}

// H01: measured: provenance is allowed in local profiles and rejected in the
// profiles shipped with the repository.
func TestH01MeasuredOnlyInLocalProfiles(t *testing.T) {
	t.Parallel()
	body := strings.Replace(renderMinimalProfile, `  cap: {kind: corpus`, `  bench: {kind: measured, ref: "local bench run"}
  cap: {kind: corpus`, 1)
	body = strings.Replace(body, `maxPkgW: {v: 100, src: published:doc}`, `maxPkgW: {v: 100, src: "measured:bench#run 3"}`, 1)
	fsys := renderMapFS(map[string]string{"base": body})
	if _, err := LoadProfiles(fsys, "p"); err != nil {
		t.Fatalf("a local profile may cite a measured source: %v", err)
	}
	_, err := renderLoadEmbedded(fsys, "p")
	if err == nil || !strings.Contains(err.Error(), "physics.cpu.maxPkgW: measured: provenance belongs only in local profiles") {
		t.Fatalf("err = %v, want measured: rejected in an embedded profile", err)
	}
}

// H01: the shipped profiles never mention a measurement or a non-ASCII dash,
// whatever the loader accepts.
func TestH01EmbeddedProfilesHaveNoMeasuredProvenance(t *testing.T) {
	t.Parallel()
	files, err := fs.Glob(renderProfilesFS, "profiles/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded profiles: %v", err)
	}
	for _, f := range files {
		b, err := fs.ReadFile(renderProfilesFS, f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range bytes.Split(b, []byte("\n")) {
			if bytes.Contains(line, []byte("measured:")) {
				t.Errorf("%s:%d cites a measurement; measured: belongs only in local profiles: %s", f, i+1, line)
			}
			if bytes.ContainsAny(line, "\u2013\u2014") {
				t.Errorf("%s:%d has an en or em dash", f, i+1)
			}
		}
	}
}

// H01: decoding is strict. A misspelled key anywhere, including inside a fact
// or a zone, is an error naming its path, never a silently dropped value.
func TestH01RejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, from, to, want string
	}{
		{"misspelled field", `coresPerSocket: 2`, `coresPerSockt: 2`, `cpu.coresPerSockt: unknown key`},
		{"key next to a fact value", `memoryKi: {v: 1024, src: "assumed:test"}`, `memoryKi: {v: 1024, src: "assumed:test", unit: Ki}`, `node.capacity.memoryKi: want a value or {v, src}`},
		{"misspelled v", `memoryKi: {v: 1024, src: "assumed:test"}`, `memoryKi: {value: 1024, src: "assumed:test"}`, `node.capacity.memoryKi: want a value or {v, src}`},
		{"top-level key", `description: minimal test profile`, "description: minimal test profile\nextend: other", `extend: unknown key`},
		{"source key", `ref: "https://example.invalid/doc"}`, `ref: "https://example.invalid/doc", url: x}`, `sources.doc.url: unknown key`},
		{"label group key", `values: {kubernetes.io/arch: amd64}}`, `values: {kubernetes.io/arch: amd64}, value: x}`, `node.labels.cpu.value: unknown key`},
		{"child zone name key", `physics:`, "powercap:\n  controlTypes:\n    - name: intel-rapl\n      zones:\n        - {repeat: per-package, nameFormat: package-%d, energyUnitNJ: {v: 61035, src: published:doc}, children: [{name: dram, energyUnitNJ: {v: 15300, src: published:doc}}]}\nphysics:", `children[0].name: unknown key`},
		{"duplicate key", `family: 6`, "family: 6\n  family: 7", `already set`},
		{"wrong type in a fact", `family: 6`, `family: six`, `cpu.family: want a int, got six`},
		{"wrong type in a label value", `values: {kubernetes.io/arch: amd64}}`, `values: {kubernetes.io/arch: 64}}`, `node.labels.cpu.values.kubernetes.io/arch: want a string`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(renderMinimalProfile, c.from, c.to, 1)
			if body == renderMinimalProfile {
				t.Fatalf("mutation %q not found", c.from)
			}
			_, err := LoadProfiles(renderMapFS(map[string]string{"base": body}), "p")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// H22: provenance and extends. A mapping-level src covers the values below
// it and {v, src} overrides it; a child's mapping-level src never reaches the
// parent's values; extends merges maps and replaces lists; a label group
// keeps its own src through a merge, and a redefined group replaces the
// parent's whole.
func TestH22ProvenanceAndExtends(t *testing.T) {
	t.Parallel()
	base := strings.Replace(renderMinimalProfile, "physics:", `pci:
  - {class: "0x030000", vendor: "0x102b", src: "assumed:base vga"}
  - {class: "0x020000", vendor: "0x8086", src: "assumed:base nic"}
cpufreq:
  driver: {v: acpi-cpufreq, src: published:doc}
  governor: {v: performance, src: published:doc}
  availableGovernors: {v: "performance powersave", src: published:doc}
  availableKHz: {v: [2000000, 1000000], src: published:doc}
  cpuinfoMinKHz: {v: 1000000, src: published:doc}
  cpuinfoMaxKHz: {v: 2000000, src: published:doc}
physics:`, 1)
	base = strings.Replace(base, `    cpu: {src: "assumed:test labels", values: {kubernetes.io/arch: amd64}}`,
		`    cpu: {src: "assumed:test labels", values: {kubernetes.io/arch: amd64}}
    extra: {src: "assumed:base extra", values: {example.com/base-only: "x", example.com/shared: "base"}}`, 1)
	child := `schemaVersion: 1
name: child
extends: base
description: child test profile
sources:
  childdoc: {kind: published, ref: "https://example.invalid/child"}
node:
  labels:
    gpu: {src: "assumed:child gpu labels", values: {example.com/gpu: "1"}}
    extra: {src: "assumed:child extra", values: {example.com/shared: "child"}}
pci:
  - {class: "0x120000", vendor: "0x1002", src: "assumed:child accelerator"}
cpu:
  src: "assumed:child cpu block"
  model: 2
  stepping: {v: 3, src: "published:childdoc#stepping"}
cpufreq:
  availableKHz: {v: [1500000], src: published:childdoc}
`
	profiles, err := LoadProfiles(renderMapFS(map[string]string{"base": base, "child": child}), "p")
	if err != nil {
		t.Fatal(err)
	}
	b, c := profiles["base"], profiles["child"]

	if b.CPU.Family != (Fact[int]{V: 6, Src: "corpus:cap#cpuinfo"}) {
		t.Errorf("base cpu.family = %+v, want the mapping-level src", b.CPU.Family)
	}
	if b.Node.Capacity.CPU.Src != "derived:doc#1 x 2 x 1" {
		t.Errorf("{v, src} must set its own source, got %+v", b.Node.Capacity.CPU)
	}
	// The child's mapping-level src covers only what the child wrote.
	if c.CPU.Model != (Fact[int]{V: 2, Src: "assumed:child cpu block"}) {
		t.Errorf("child cpu.model = %+v", c.CPU.Model)
	}
	if c.CPU.Family != (Fact[int]{V: 6, Src: "corpus:cap#cpuinfo"}) {
		t.Errorf("child cpu.family = %+v, want the parent's value and source untouched", c.CPU.Family)
	}
	if c.CPU.Stepping != (Fact[int]{V: 3, Src: "published:childdoc#stepping"}) {
		t.Errorf("child cpu.stepping = %+v, want the leaf's own src over the mapping's", c.CPU.Stepping)
	}
	// Maps merge: the cpufreq keys the child did not write are the parent's.
	if c.CPUFreq.Driver.V != "acpi-cpufreq" || c.CPUFreq.CPUInfoMaxKHz.V != 2000000 {
		t.Errorf("child cpufreq = %+v, want the parent's keys merged in", c.CPUFreq)
	}
	// Lists are replaced, never appended to.
	if got := c.CPUFreq.AvailableKHz; !reflect.DeepEqual(got, Fact[[]int64]{V: []int64{1500000}, Src: "published:childdoc"}) {
		t.Errorf("child availableKHz = %+v, want the child's list only", got)
	}
	if len(c.PCI) != 1 || c.PCI[0].Vendor.V != "0x1002" || c.PCI[0].Class.Src != "assumed:child accelerator" {
		t.Errorf("child pci = %+v, want the child's list only", c.PCI)
	}
	// Label groups.
	if g := c.Node.Labels["cpu"]; g.Src != "assumed:test labels" || g.Values["kubernetes.io/arch"] != "amd64" {
		t.Errorf("child keeps the parent's cpu group with its src, got %+v", g)
	}
	if g := c.Node.Labels["gpu"]; g.Src != "assumed:child gpu labels" {
		t.Errorf("child gpu group = %+v", g)
	}
	if g := c.Node.Labels["extra"]; g.Src != "assumed:child extra" || len(g.Values) != 1 || g.Values["example.com/shared"] != "child" {
		t.Errorf("a redefined group replaces the parent's whole, got %+v", g)
	}
	// Sources merge by id; name, description and extends are the child's.
	if _, ok := c.Sources["doc"]; !ok {
		t.Errorf("child sources lack the parent's doc")
	}
	if c.Name != "child" || c.Description != "child test profile" || c.Extends != "base" {
		t.Errorf("child header = %q %q %q", c.Name, c.Description, c.Extends)
	}
}

// H22: extends must name a profile of the same set and must not loop, and a
// file's name must match the profile's.
func TestH22ExtendsErrors(t *testing.T) {
	t.Parallel()
	loop := func(name, ext string) string {
		return strings.Replace(strings.Replace(renderMinimalProfile, "name: base", "name: "+name+"\nextends: "+ext, 1), "description: minimal", "description: "+name, 1)
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing parent", map[string]string{"base": loop("base", "nothere")}, `extends "nothere"`},
		{"cycle", map[string]string{"a": loop("a", "b"), "b": loop("b", "a")}, "extends cycle"},
		{"name and file differ", map[string]string{"other": renderMinimalProfile}, `name is "base", want "other"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadProfiles(renderMapFS(c.files), "p")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func renderSortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
