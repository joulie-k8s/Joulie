package simcluster

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// testSnapshot reads testdata/truth-fixture.json: an intel Node with an
// unmeasured DRAM zone, an NVIDIA and an AMD GPU Node, a Node without
// powercap and a VM, components out of index order.
func testSnapshot(t *testing.T) *emulate.Snapshot {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "truth-fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s emulate.Snapshot
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	return &s
}

// K01: each truth of each fixture Node, and the NodeTruth it comes from.
func TestK01TruthExpressionsOnTheFixture(t *testing.T) {
	truths := NodeTruths(testSnapshot(t))
	wantIntel := NodeTruth{PackagesW: []float64{150, 140}, PackageCapW: []float64{132, 165}, DRAMW: 10, PlatformOtherW: 50, HostW: 362}
	if !reflect.DeepEqual(truths["intel-0"], wantIntel) {
		t.Errorf("intel-0 truth %+v, want %+v", truths["intel-0"], wantIntel)
	}
	wantNVL := NodeTruth{PackagesW: []float64{100, 90}, PackageCapW: []float64{0, 0}, GPUsW: []float64{300, 310}, GPULimitW: []float64{320, 320}, GPUVendor: "NVIDIA", PlatformOtherW: 80, HostW: 900}
	if !reflect.DeepEqual(truths["nvl-0"], wantNVL) {
		t.Errorf("nvl-0 truth %+v, want %+v", truths["nvl-0"], wantNVL)
	}
	for node, want := range map[string]map[string]float64{
		"intel-0":      {TruthPackagesGPUs: 290, TruthPackagesNVIDIAGPUs: 290, TruthHost: 362, TruthKepler07PlatformSeries: 2, TruthKepler07Host: 362},
		"nvl-0":        {TruthPackagesGPUs: 800, TruthPackagesNVIDIAGPUs: 800, TruthHost: 900, TruthKepler07PlatformSeries: 2, TruthKepler07Host: 900},
		"mi300x-0":     {TruthPackagesGPUs: 1480, TruthPackagesNVIDIAGPUs: 230, TruthHost: 1600, TruthKepler07PlatformSeries: 2, TruthKepler07Host: 1600},
		"nopowercap-0": {TruthPackagesGPUs: 0, TruthPackagesNVIDIAGPUs: 0, TruthHost: 300, TruthKepler07PlatformSeries: 0, TruthKepler07Host: 0},
		"vm-0":         {TruthPackagesGPUs: 0, TruthPackagesNVIDIAGPUs: 0, TruthHost: 0, TruthKepler07PlatformSeries: 0, TruthKepler07Host: 0},
	} {
		nt, ok := truths[node]
		if !ok {
			t.Errorf("no truth for %s", node)
			continue
		}
		if len(want) != len(Truths()) {
			t.Fatalf("%s: the test covers %d of %d truths", node, len(want), len(Truths()))
		}
		for truth, w := range want {
			got, err := EvalTruth(truth, nt)
			if err != nil || got != w {
				t.Errorf("%s %s = %g, %v; want %g", node, truth, got, err, w)
			}
		}
	}
	// A GPU Node without a measured CPU package: Kepler reports nothing.
	gpuOnly := NodeTruth{GPUsW: []float64{300}, GPUVendor: "NVIDIA", HostW: 400}
	for truth, want := range map[string]float64{TruthPackagesGPUs: 300, TruthPackagesNVIDIAGPUs: 0, TruthHost: 400, TruthKepler07PlatformSeries: 0, TruthKepler07Host: 0} {
		if got, err := EvalTruth(truth, gpuOnly); err != nil || got != want {
			t.Errorf("GPU-only Node %s = %g, %v; want %g", truth, got, err, want)
		}
	}
	if _, err := EvalTruth("packages", NodeTruth{}); err == nil {
		t.Error("an unknown truth evaluates")
	}
}

// NodeTruthsOver reads power from the energy the ledgers added, as rate()
// does, and leaves out a Node missing from either snapshot.
func TestNodeTruthsOver(t *testing.T) {
	from := testSnapshot(t)
	to := testSnapshot(t)
	to.Time = from.Time.Add(10 * time.Second)
	to.Nodes = to.Nodes[:3]
	for i := range to.Nodes {
		n := &to.Nodes[i]
		n.Host.EnergyJ += 4000
		for j := range n.Components {
			n.Components[j].EnergyJ += 100 * float64(j+1)
		}
	}
	truths, err := NodeTruthsOver(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(truths) != 3 {
		t.Errorf("%d truths, want the 3 Nodes of both snapshots", len(truths))
	}
	// intel-0's components in snapshot order: cpu1, cpu0, memory0.
	want := NodeTruth{PackagesW: []float64{20, 10}, PackageCapW: []float64{132, 165}, DRAMW: 30, PlatformOtherW: 50, HostW: 400}
	if !reflect.DeepEqual(truths["intel-0"], want) {
		t.Errorf("intel-0 truth over 10 s %+v, want %+v", truths["intel-0"], want)
	}
	if _, err := NodeTruthsOver(to, from); err == nil {
		t.Error("snapshots out of order are accepted")
	}
}

// K02: the tolerance is 3 % of the truth or 1 W, whichever is larger, so a
// zero truth is checked against the floor; a Node without a series passes
// only when its truth is zero.
func TestK02ToleranceAndZeroTruthFloor(t *testing.T) {
	c := Check{ID: "k02", Truth: TruthHost, Tolerance: CheckTolerance, AbsFloorW: CheckAbsFloorW}
	for _, tc := range []struct {
		observed, want float64
		pass           bool
	}{
		{1030, 1000, true}, {970, 1000, true}, {1030.5, 1000, false}, {969.5, 1000, false},
		{10.9, 10, true}, {11.1, 10, false},
		{0.9, 0, true}, {-0.9, 0, true}, {1.1, 0, false},
		{math.NaN(), 0, false}, {math.Inf(1), 1000, false},
	} {
		if got := c.Within(tc.observed, tc.want); got != tc.pass {
			t.Errorf("Within(%g, %g) = %v, want %v", tc.observed, tc.want, got, tc.pass)
		}
	}

	truth := map[string]NodeTruth{"a": {HostW: 1000}, "zero": {}, "silent": {HostW: 50}}
	results, err := Evaluate(c, map[string]float64{"a": 1010, "zero": 0.5, "stranger": 3}, truth)
	if err == nil || !strings.Contains(err.Error(), "fails on 2 of 4 Nodes") {
		t.Fatalf("error %v, want 2 failures of 4", err)
	}
	pass := map[string]bool{}
	for _, r := range results {
		pass[r.Node] = r.Pass
	}
	if want := map[string]bool{"a": true, "zero": true, "silent": false, "stranger": false}; !reflect.DeepEqual(pass, want) {
		t.Errorf("results %v, want %v", pass, want)
	}
	for _, msg := range []string{"silent: no series, want 50.000", "stranger: observed 3.000, but the truth has no such Node"} {
		if !strings.Contains(err.Error(), msg) {
			t.Errorf("error %q lacks %q", err, msg)
		}
	}
	if _, err := Evaluate(c, map[string]float64{"a": 1010}, map[string]NodeTruth{"a": {HostW: 1000}, "vm": {}}); err != nil {
		t.Errorf("a missing series with a zero truth fails: %v", err)
	}
	if _, err := Evaluate(c, nil, nil); err == nil {
		t.Error("a check with nothing to compare passes")
	}
	bad := c
	bad.Truth = "bogus"
	if _, err := Evaluate(bad, map[string]float64{"a": 1}, truth); err == nil {
		t.Error("an unknown truth evaluates")
	}
}

// K03: a check with a known defect is expected to fail; once it passes on
// every Node, that is an error saying the defect is gone.
func TestK03PassingKnownDefectIsAnError(t *testing.T) {
	c := Check{ID: "mi300x-gpu", Truth: TruthPackagesGPUs, Tolerance: CheckTolerance, AbsFloorW: CheckAbsFloorW, KnownDefect: "D1"}
	truth := map[string]NodeTruth{"mi300x-0": {PackagesW: []float64{100}, GPUsW: []float64{600}}}
	_, err := Evaluate(c, map[string]float64{"mi300x-0": 700}, truth)
	if err == nil || !strings.Contains(err.Error(), "known defect D1 no longer reproduces") || !strings.Contains(err.Error(), "known_defects_test.go") {
		t.Errorf("passing known defect: error %v, want one saying D1 no longer reproduces", err)
	}
	if _, err := Evaluate(c, map[string]float64{"mi300x-0": 100}, truth); err != nil {
		t.Errorf("a known defect that still fails is an error: %v", err)
	}
}
