package clusterspec

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// testBaseSpec is a valid spec that sets a value in every block a key can
// go in, so each unknown-key case below has a place to put its key.
const testBaseSpec = `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: t}
spec:
  defaults: {kwok: true, labels: {example.com/a: b}, annotations: {example.com/n: v}, baseLoad: {cpuUtil: 0.5}}
  facility: {ambientC: 20, coolingZones: [{name: z, offsetC: 1}]}
  pools:
  - {name: p, profile: vm-no-powercap, count: 1, coolingZone: z, taints: [{key: example.com/k, value: v, effect: NoSchedule}], overrides: {nfd: absent}, baseLoad: {gpuUtil: 0.1}}
  harness: {pools: {p: {toolDelivery: none}}, notASpecKey: [1, {k: text}]}
`

// testParse parses doc or fails the test.
func testParse(t *testing.T, doc string) *Spec {
	t.Helper()
	s, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, doc)
	}
	return s
}

// testReplace replaces old, which must occur in doc exactly once.
func testReplace(t *testing.T, doc, old, new string) string {
	t.Helper()
	if n := strings.Count(doc, old); n != 1 {
		t.Fatalf("%q occurs %d times in the spec, want once", old, n)
	}
	return strings.Replace(doc, old, new, 1)
}

// testWantErr fails unless err is non-nil and contains every one of want.
func testWantErr(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want one containing %q", what, want)
		return
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s: error %q does not contain %q", what, err, w)
		}
	}
}

// CS01: an unknown key anywhere in the spec fails and the error names its
// path; under spec.harness any key is accepted. Keys match case-sensitively,
// as on the API server, so a key that differs from a known one only in case
// is unknown too, and a file holds one spec: a second document fails (an
// empty one, as after a trailing ---, is fine).
func TestCS01UnknownKeyFailsOutsideHarness(t *testing.T) {
	t.Parallel()
	s := testParse(t, testBaseSpec)
	if err := s.Validate(); err != nil {
		t.Fatalf("base spec: %v", err)
	}
	cases := []struct{ path, old, new string }{
		{"bogusTop", "kind: ClusterSpec\n", "kind: ClusterSpec\nbogusTop: 1\n"},
		{"metadata.bogusMeta", "{name: t}", "{name: t, bogusMeta: 1}"},
		{"spec.bogusSpec", "spec:\n", "spec:\n  bogusSpec: 1\n"},
		{"spec.defaults.bogusDefaults", "defaults: {", "defaults: {bogusDefaults: 1, "},
		{"spec.defaults.baseLoad.bogusLoad", "{cpuUtil: 0.5}", "{cpuUtil: 0.5, bogusLoad: 1}"},
		{"spec.facility.bogusFacility", "facility: {", "facility: {bogusFacility: 1, "},
		{"spec.facility.coolingZones[0].bogusZone", "{name: z, offsetC: 1}", "{name: z, offsetC: 1, bogusZone: 1}"},
		{"spec.pools[0].bogusPool", "- {name: p,", "- {bogusPool: 1, name: p,"},
		{"spec.pools[0].taints[0].bogusTaint", "{key: example.com/k,", "{key: example.com/k, bogusTaint: 1,"},
		{"spec.pools[0].overrides.bogusOverride", "{nfd: absent}", "{nfd: absent, bogusOverride: 1}"},
		{"spec.pools[0].overrides.gpus.bogusGPU", "{nfd: absent}", "{nfd: absent, gpus: {exposure: whole, bogusGPU: 1}}"},
		{"spec.pools[0].baseLoad.bogusPoolLoad", "{gpuUtil: 0.1}", "{gpuUtil: 0.1, bogusPoolLoad: 1}"},
		// Case variants: next to the real key, one of the two values would
		// be dropped; in its place, the field would be set.
		{"Kind", "kind: ClusterSpec\n", "kind: ClusterSpec\nKind: ClusterSpec\n"},
		{"spec.pools[0].Count", "count: 1,", "count: 1, Count: 3,"},
		{"spec.pools[0].Profile", "profile: vm-no-powercap", "Profile: vm-no-powercap"},
		{"spec.pools[0].coolingzone", "coolingZone: z", "coolingzone: z"},
		{"spec.defaults.BaseLoad", "baseLoad: {cpuUtil: 0.5}", "BaseLoad: {cpuUtil: 0.5}"},
		{"spec.pools[0].taints[0].Effect", "effect: NoSchedule", "Effect: NoSchedule"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(testReplace(t, testBaseSpec, c.old, c.new)))
		testWantErr(t, c.path, err, `unknown field "`+c.path+`"`)
	}

	// Every unknown key is named, not only the first.
	_, err := Parse([]byte(testReplace(t, testBaseSpec, "count: 1,", "COUNT: 1, bogusPool: 1,")))
	testWantErr(t, "two unknown keys", err, `unknown field "spec.pools[0].COUNT"`, `unknown field "spec.pools[0].bogusPool"`)

	// Case variants are kept as written under spec.harness.
	s = testParse(t, testReplace(t, testBaseSpec, "notASpecKey:", "Pools: 1, notASpecKey:"))
	if !bytes.Contains(s.Spec.Harness, []byte(`"Pools":1`)) || !bytes.Contains(s.Spec.Harness, []byte(`"pools":{`)) {
		t.Errorf("harness with Pools and pools = %s", s.Spec.Harness)
	}

	_, err = Parse([]byte(testBaseSpec + "---\n" + testBaseSpec))
	testWantErr(t, "two documents", err, "document 2")
	_, err = Parse([]byte(testBaseSpec + "---\n# nothing\n---\n" + testBaseSpec))
	testWantErr(t, "a document after an empty one", err, "document 3")
	for _, c := range []struct{ what, doc string }{
		{"leading ---", "---\n" + testBaseSpec},
		{"trailing ---", testBaseSpec + "---\n"},
		{"trailing ...", testBaseSpec + "...\n"},
	} {
		if _, err := Parse([]byte(c.doc)); err != nil {
			t.Errorf("one document with a %s: %v", c.what, err)
		}
	}
}

// CS02: a duplicate pool or Node name, a name over 63 characters or with a
// dot, an unknown profile (the error lists the known ones) and a cooling
// zone not declared under facility.coolingZones each fail, naming the pool.
func TestCS02NamesProfilesAndZonesAreChecked(t *testing.T) {
	t.Parallel()
	pool := "- {name: p, profile: vm-no-powercap, count: 1, coolingZone: z, taints: [{key: example.com/k, value: v, effect: NoSchedule}], overrides: {nfd: absent}, baseLoad: {gpuUtil: 0.1}}\n"
	long := strings.Repeat("a", 62)
	cases := []struct {
		what, pool string
		want       []string
	}{
		{"duplicate pool", pool + "  - {name: p, profile: vm-no-powercap, count: 1, prefix: other}\n", []string{"pool p: a second pool has this name"}},
		{"duplicate Node", pool + "  - {name: q, profile: vm-no-powercap, count: 2, prefix: p}\n", []string{"pool q: Node p-0 is also in pool p"}},
		{"long pool name", pool + "  - {name: " + long + "xx, profile: vm-no-powercap, count: 1, prefix: short}\n", []string{"pool " + long + "xx: name:", "63"}},
		{"long Node name", pool + "  - {name: q, profile: vm-no-powercap, count: 1, prefix: " + long + "}\n", []string{"pool q: Node name \"" + long + "-0\"", "63"}},
		{"dotted pool name", pool + "  - {name: q.r, profile: vm-no-powercap, count: 1, prefix: q}\n", []string{"pool q.r: name:"}},
		{"dotted prefix", pool + "  - {name: q, profile: vm-no-powercap, count: 1, prefix: q.r}\n", []string{"pool q: Node name \"q.r-0\""}},
		{"undeclared zone", pool + "  - {name: q, profile: vm-no-powercap, count: 1, coolingZone: nowhere}\n", []string{`pool q: cooling zone "nowhere" is not declared`}},
		{"zero count", pool + "  - {name: q, profile: vm-no-powercap, count: 0}\n", []string{"pool q: count 0 is below 1"}},
	}
	for _, c := range cases {
		s := testParse(t, testReplace(t, testBaseSpec, pool, c.pool))
		testWantErr(t, c.what, s.Validate(), c.want...)
		_, err := s.Expand(Profiles(""))
		testWantErr(t, c.what+" (Expand)", err, c.want...)
	}

	// A Node name of exactly 63 characters is fine.
	s := testParse(t, testReplace(t, testBaseSpec, pool, pool+"  - {name: q, profile: vm-no-powercap, count: 1, prefix: "+long[:61]+"}\n"))
	if err := s.Validate(); err != nil {
		t.Errorf("63-character Node name: %v", err)
	}

	s = testParse(t, testReplace(t, testBaseSpec, "profile: vm-no-powercap", "profile: no-such-profile"))
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate does not look at profiles, but: %v", err)
	}
	_, err := s.Expand(Profiles(""))
	testWantErr(t, "unknown profile", err, `pool p: profile "no-such-profile" is not known`, "intel-xeon-2s-rapl", "nvidia-h100-nvl-8gpu", "vm-no-powercap")

	s = testParse(t, testReplace(t, testBaseSpec, "metadata: {name: t}", "metadata: {name: T_1}"))
	testWantErr(t, "spec name", s.Validate(), "metadata.name")
}

// CS10 (validation half): a baseLoad value outside 0..1, in the defaults or
// in a pool, fails and names the key. Expand merges the fields
// (TestCS10PoolBaseLoadOverridesDefaultsPerField).
func TestCS10BaseLoadOutsideZeroToOneFails(t *testing.T) {
	t.Parallel()
	cases := []struct{ what, old, new, want string }{
		{"defaults cpuUtil", "{cpuUtil: 0.5}", "{cpuUtil: 1.5}", "defaults: baseLoad.cpuUtil 1.5 is not in 0..1"},
		{"defaults memoryIntensity", "{cpuUtil: 0.5}", "{cpuUtil: 0.5, memoryIntensity: -0.1}", "defaults: baseLoad.memoryIntensity -0.1 is not in 0..1"},
		{"pool gpuUtil", "{gpuUtil: 0.1}", "{gpuUtil: 2}", "pool p: baseLoad.gpuUtil 2 is not in 0..1"},
	}
	for _, c := range cases {
		s := testParse(t, testReplace(t, testBaseSpec, c.old, c.new))
		testWantErr(t, c.what, s.Validate(), c.want)
	}
	// JSON has no NaN, so only a spec built in Go can carry one.
	s := testParse(t, testBaseSpec)
	nan := math.NaN()
	s.Spec.Pools[0].BaseLoad.GPUUtil = &nan
	testWantErr(t, "pool NaN", s.Validate(), "pool p: baseLoad.gpuUtil NaN is not in 0..1")
	for _, edge := range []string{"{gpuUtil: 0}", "{gpuUtil: 1}"} {
		s := testParse(t, testReplace(t, testBaseSpec, "{gpuUtil: 0.1}", edge))
		if err := s.Validate(); err != nil {
			t.Errorf("baseLoad %s: %v", edge, err)
		}
	}
}

// CS13 (validation half): within a pool, two taints with one key and
// effect fail, and so does a taint on the KWOK key, which defaults.kwok
// adds. Expand checks the pool's taints against the profile's
// (TestCS13TaintOrderIsProfilePoolKWOK).
func TestCS13PoolTaintsAreUniqueAndLeaveKWOKAlone(t *testing.T) {
	t.Parallel()
	taint := "taints: [{key: example.com/k, value: v, effect: NoSchedule}]"
	cases := []struct{ what, new, want string }{
		{"duplicate", "taints: [{key: example.com/k, value: v, effect: NoSchedule}, {key: example.com/k, value: w, effect: NoSchedule}]", "pool p: taints[1] repeats the key and effect example.com/k:NoSchedule"},
		{"KWOK key", "taints: [{key: kwok.x-k8s.io/node, value: fake, effect: NoSchedule}]", "pool p: taints[0] kwok.x-k8s.io/node is added by defaults.kwok"},
		{"effect", "taints: [{key: example.com/k, value: v, effect: Sometimes}]", `pool p: taints[0] effect "Sometimes"`},
	}
	for _, c := range cases {
		s := testParse(t, testReplace(t, testBaseSpec, taint, c.new))
		testWantErr(t, c.what, s.Validate(), c.want)
	}
	s := testParse(t, testReplace(t, testBaseSpec, taint, "taints: [{key: example.com/k, value: v, effect: NoSchedule}, {key: example.com/k, value: v, effect: NoExecute}]"))
	if err := s.Validate(); err != nil {
		t.Errorf("one key with two effects: %v", err)
	}
}

// Labels and annotations the spec sets itself, or that would hide the
// rendered hardware labels, are rejected in the defaults and in a pool.
func TestValidateRejectsLabelsTheSpecOwns(t *testing.T) {
	t.Parallel()
	cases := []struct{ what, old, new, want string }{
		{"hostname", "labels: {example.com/a: b}", "labels: {kubernetes.io/hostname: x}", "defaults: label kubernetes.io/hostname is set by the spec itself"},
		{"sim prefix", "labels: {example.com/a: b}", "labels: {sim.joulie.io/pool: x}", "defaults: label sim.joulie.io/pool is set by the spec itself"},
		{"rack", "count: 1, coolingZone: z,", "count: 1, coolingZone: z, labels: {joulie.io/rack: r},", "pool p: label joulie.io/rack is set from the pool's rack"},
		{"zone", "count: 1, coolingZone: z,", "count: 1, coolingZone: z, labels: {joulie.io/cooling-zone: z},", "pool p: label joulie.io/cooling-zone is set from the pool's coolingZone"},
		{"hw label", "count: 1, coolingZone: z,", "count: 1, coolingZone: z, labels: {joulie.io/hw.cpu-model: x},", "pool p: label joulie.io/hw.cpu-model"},
		{"bad value", "labels: {example.com/a: b}", "labels: {example.com/a: 'a b'}", `defaults: label example.com/a value "a b"`},
		{"profile annotation", "annotations: {example.com/n: v}", "annotations: {sim.joulie.io/profile: x}", "defaults: annotation sim.joulie.io/profile is set by the spec itself"},
		{"KWOK annotation", "annotations: {example.com/n: v}", "annotations: {kwok.x-k8s.io/node: fake}", "defaults: annotation kwok.x-k8s.io/node is set by defaults.kwok"},
	}
	for _, c := range cases {
		s := testParse(t, testReplace(t, testBaseSpec, c.old, c.new))
		testWantErr(t, c.what, s.Validate(), c.want)
	}
}

// CS14: spec.harness is never validated and survives a marshal and parse
// round trip byte for byte. Parsing turns the YAML into JSON with sorted
// keys and no spaces, so that is the form the first parse must give, and
// every later round trip must give the same bytes.
func TestCS14HarnessRoundTripsUnchecked(t *testing.T) {
	t.Parallel()
	s := testParse(t, testBaseSpec)
	want := []byte(`{"notASpecKey":[1,{"k":"text"}],"pools":{"p":{"toolDelivery":"none"}}}`)
	if !bytes.Equal(s.Spec.Harness, want) {
		t.Fatalf("harness = %s, want %s", s.Spec.Harness, want)
	}
	if _, err := s.Expand(Profiles("")); err != nil {
		t.Fatalf("Expand: %v", err)
	}
	again := s
	for i := 1; i <= 2; i++ {
		b, err := yaml.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		again = testParse(t, string(b))
		if !bytes.Equal(again.Spec.Harness, want) {
			t.Errorf("harness after round trip %d = %s, want %s", i, again.Spec.Harness, want)
		}
	}

	for _, h := range []string{"[1, 2]", `"text"`, "null", "{pools: {nowhere: {toolDelivery: bogus}}}"} {
		s := testParse(t, testReplace(t, testBaseSpec, "{pools: {p: {toolDelivery: none}}, notASpecKey: [1, {k: text}]}", h))
		if err := s.Validate(); err != nil {
			t.Errorf("harness %s: %v", h, err)
		}
	}
	s = testParse(t, testReplace(t, testBaseSpec, "  harness: {pools: {p: {toolDelivery: none}}, notASpecKey: [1, {k: text}]}\n", ""))
	if s.Spec.Harness != nil {
		t.Errorf("no harness: Harness = %s, want nil", s.Spec.Harness)
	}
}
