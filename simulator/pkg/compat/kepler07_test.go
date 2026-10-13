package compat

import (
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// k07Family is the full name of a Kepler v0.7.12 family.
func k07Family(family string) string {
	return k07Namespace + "_" + k07Context + "_" + family + k07Suffix
}

// CP01: kepler-0.7 serves the v0.7.12 families with the labels package,
// instance and mode and the source each upstream family carries; every
// socket has package, core, uncore and dram series, the zones the snapshot
// lacks at 0; GPU series exist for NVIDIA GPUs only; and the exposition for
// an Intel and an NVL host matches the golden.
func TestCP01Kepler07FamiliesLabelsAndGolden(t *testing.T) {
	set, _ := compatRun(t, []string{Kepler07}, Options{}, compatIntel(), compatNVL(), compatMI300X())
	samples := compatGather(t, set, Kepler07)

	want := map[string]string{}
	for _, f := range k07Families {
		want[k07Family(f)] = k07SourceRAPL
	}
	want[k07Family(k07GPU)] = k07SourceNVML
	seen := map[string]bool{}
	for _, s := range samples {
		source, ok := want[s.family]
		if !ok {
			t.Errorf("unexpected family %s", s.family)
			continue
		}
		seen[s.family] = true
		if got := strings.Join(compatLabelNames(s), ","); got != "instance,mode,package,source" {
			t.Errorf("%s labels %s, want instance,mode,package,source", s.family, got)
		}
		if s.labels[k07LabelSource] != source {
			t.Errorf("%s source %q, want %q", s.family, s.labels[k07LabelSource], source)
		}
		if m := s.labels[k07LabelMode]; m != k07ModeDynamic && m != k07ModeIdle {
			t.Errorf("%s mode %q", s.family, m)
		}
	}
	for f := range want {
		if !seen[f] {
			t.Errorf("family %s missing", f)
		}
	}

	for _, node := range []string{"intel-0", "nvl-0", "mi300x-0"} {
		for _, f := range []string{k07Package, k07Core, k07Uncore, k07DRAM} {
			for _, socket := range []string{"0", "1"} {
				v := compatOne(t, samples, k07Family(f), map[string]string{k07LabelInstance: node, k07LabelPackage: socket, k07LabelMode: k07ModeDynamic})
				if (f == k07Core || f == k07Uncore) && v != 0 {
					t.Errorf("%s %s socket %s = %g; the snapshot has no such zone, want 0", node, f, socket, v)
				}
			}
		}
		compatOne(t, samples, k07Family(k07Other), map[string]string{k07LabelInstance: node, k07LabelPackage: k07OtherID, k07LabelMode: k07ModeIdle})
	}
	for node, n := range map[string]int{"intel-0": 0, "nvl-0": 8, "mi300x-0": 0} {
		if got := compatFind(samples, k07Family(k07GPU), map[string]string{k07LabelInstance: node, k07LabelMode: k07ModeDynamic}); len(got) != n {
			t.Errorf("%s has %d GPU series, want %d", node, len(got), n)
		}
	}
	for i := 0; i < 8; i++ {
		compatOne(t, samples, k07Family(k07GPU), map[string]string{k07LabelInstance: "nvl-0", k07LabelPackage: fmt.Sprintf("%d", i), k07LabelMode: k07ModeIdle})
	}

	golden, _ := compatRun(t, []string{Kepler07}, Options{}, compatIntel(), compatNVL())
	code, body := compatScrape(t, golden, Path(Kepler07))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path(Kepler07), code)
	}
	compatGolden(t, "kepler-0.7.prom", body)
}

// CP02: per series, the dynamic and idle increments of an interval sum to
// the energy measured over it, PowerW x dt, and both counters only grow,
// over 100 ticks of varying load and tick length.
func TestCP02Kepler07IdleAndDynamicSumToTheEnergy(t *testing.T) {
	set, err := NewSet([]string{Kepler07}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatIntel(), compatNVL())
	rng := rand.New(rand.NewSource(7))
	set.Observe(c.snap(0, 0.5, 0.5))
	prev := compatGather(t, set, Kepler07)
	for tick := 0; tick < 100; tick++ {
		dt := float64(1 + rng.Intn(5))
		snap := c.snap(dt, rng.Float64(), rng.Float64())
		set.Observe(snap)
		cur := compatGather(t, set, Kepler07)
		for i := range snap.Nodes {
			k07CheckIncrements(t, fmt.Sprintf("tick %d", tick), prev, cur, &snap.Nodes[i], dt)
		}
		prev = cur
	}
}

// CP02: a snapshot timed before the last one adds nothing, so over a window
// that holds one, the dynamic and idle increments still sum to PowerW times
// the time that passed and not to the energy since the late snapshot. The
// load is constant, so PowerW is the same in every snapshot.
func TestCP02Kepler07OutOfOrderSnapshotAddsNothing(t *testing.T) {
	set, err := NewSet([]string{Kepler07}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatIntel(), compatNVL())
	first := c.snap(0, 0.5, 0.5)
	late := c.snap(5, 0.5, 0.5)
	mid := c.snap(5, 0.5, 0.5)
	last := c.snap(10, 0.5, 0.5)

	set.Observe(first)
	start := compatGather(t, set, Kepler07)
	set.Observe(mid)
	before := compatGather(t, set, Kepler07)
	set.Observe(late)
	compatSame(t, "a snapshot 5 s before the last one", before, compatGather(t, set, Kepler07))
	set.Observe(last)
	end := compatGather(t, set, Kepler07)
	for i := range last.Nodes {
		k07CheckIncrements(t, "first to last", start, end, &last.Nodes[i], last.Time.Sub(first.Time).Seconds())
	}
}

// k07CheckIncrements checks node n between two gathers dt seconds apart:
// the dynamic and idle counters of its package, dram, gpu and platform
// series did not fall and grew by PowerW x dt together, and other did not
// fall. A series absent from prev counts from 0.
func k07CheckIncrements(t *testing.T, what string, prev, cur []compatSample, n *emulate.NodeSnapshot, dt float64) {
	t.Helper()
	series := map[string]float64{k07Family(k07Platform) + "/" + k07PlatformID: n.Host.PowerW * dt}
	for _, comp := range n.Components {
		switch comp.Type {
		case semconv.HwTypeCPU:
			series[k07Family(k07Package)+"/"+fmt.Sprint(comp.Index)] = comp.PowerW * dt
		case semconv.HwTypeMemory:
			series[k07Family(k07DRAM)+"/"+fmt.Sprint(comp.Index)] = comp.PowerW * dt
		case semconv.HwTypeGPU:
			series[k07Family(k07GPU)+"/"+fmt.Sprint(comp.Index)] = comp.PowerW * dt
		}
	}
	for key, wantJ := range series {
		family, id, _ := strings.Cut(key, "/")
		var sum float64
		for _, mode := range []string{k07ModeDynamic, k07ModeIdle} {
			match := map[string]string{k07LabelInstance: n.Name, k07LabelPackage: id, k07LabelMode: mode}
			now := compatOne(t, cur, family, match)
			was := 0.0
			if got := compatFind(prev, family, match); len(got) == 1 {
				was = got[0].value
			}
			if now < was {
				t.Fatalf("%s: %s %s %s fell from %g to %g", what, n.Name, key, mode, was, now)
			}
			sum += now - was
		}
		compatNear(t, fmt.Sprintf("%s: %s %s dynamic+idle increment", what, n.Name, key), sum, wantJ)
	}
	for _, mode := range []string{k07ModeDynamic, k07ModeIdle} {
		match := map[string]string{k07LabelInstance: n.Name, k07LabelMode: mode}
		if now, was := compatOne(t, cur, k07Family(k07Other), match), compatOne(t, prev, k07Family(k07Other), match); now < was {
			t.Fatalf("%s: %s other %s fell from %g to %g", what, n.Name, mode, was, now)
		}
	}
}

// CP02: the split keeps the power of the lowest-load interval as idle; a
// new baseline needs both a load and a power at or below the current ones;
// an interval below the baseline is all idle; a zero interval adds nothing.
func TestCP02Kepler07SplitFollowsTheLowestLoadInterval(t *testing.T) {
	var s k07Split
	for i, c := range []struct {
		deltaJ, dt, load  float64
		wantIdle, wantDyn float64
	}{
		{0, 10, 0.1, 0, 0},          // no energy yet: no baseline
		{2000, 10, 0.5, 2000, 0},    // first interval: all idle, baseline 200 W at 0.5
		{1500, 10, 0.2, 1500, 0},    // lower load and power: baseline 150 W
		{3000, 10, 0.9, 1500, 1500}, // busier: 150 W idle, the rest dynamic
		{1200, 10, 0.6, 1200, 0},    // less power but more load: baseline kept
		{3000, 10, 0.9, 1500, 1500}, // so 150 W idle again, not 120 W
		{4000, 10, 0.1, 1500, 2500}, // lower load but more power: baseline kept
		{600, 5, 0.6, 600, 0},       // below the baseline: all idle, capped
		{1200, 10, 0.05, 1200, 0},   // lower load and power: baseline 120 W
		{2400, 20, 0.7, 2400, 0},    // 120 W x 20 s
		{3000, 20, 0.7, 2400, 600},  // 150 W: 120 W idle, the rest dynamic
		{100, 0, 0.7, 0, 0},         // no time: nothing
		{-50, 10, 0.7, 0, 0},        // a negative delta: nothing
	} {
		idle, dyn := s.add(c.deltaJ, c.dt, c.load)
		compatNear(t, fmt.Sprintf("interval %d idle", i), idle, c.wantIdle)
		compatNear(t, fmt.Sprintf("interval %d dynamic", i), dyn, c.wantDyn)
	}
	compatNear(t, "idle total", s.idleJ, 2000+1500+1500+1200+1500+1500+600+1200+2400+2400)
	compatNear(t, "dynamic total", s.dynJ, 1500+1500+2500+600)
}

// CP03: kepler_node_platform_joules_total has two series per node, dynamic
// and idle, both with instance=<node>: the input of D12, where a query per
// node that kept only its first result read half the platform energy.
func TestCP03Kepler07PlatformHasTwoSeriesPerNode(t *testing.T) {
	set, _ := compatRun(t, []string{Kepler07}, Options{}, compatIntel(), compatNVL())
	samples := compatGather(t, set, Kepler07)
	for _, node := range []string{"intel-0", "nvl-0"} {
		got := compatFind(samples, k07Family(k07Platform), map[string]string{k07LabelInstance: node})
		if len(got) != 2 {
			t.Fatalf("%s: %d platform series, want 2: %v", node, len(got), got)
		}
		modes := map[string]bool{}
		for _, s := range got {
			modes[s.labels[k07LabelMode]] = true
			if s.labels[k07LabelPackage] != k07PlatformID || s.labels[k07LabelSource] != k07SourceRAPL {
				t.Errorf("%s platform labels %v, want package=%s and source=%s", node, s.labels, k07PlatformID, k07SourceRAPL)
			}
			if !(s.value > 0) {
				t.Errorf("%s platform %s = %g, want > 0", node, s.labels[k07LabelMode], s.value)
			}
		}
		if !modes[k07ModeDynamic] || !modes[k07ModeIdle] {
			t.Errorf("%s platform modes %v, want dynamic and idle", node, modes)
		}
	}
}

// CP02: each family's split compares the load upstream compares for it:
// the node CPU load for package, dram and platform, the GPU utilisation for
// gpu. In the second interval every component draws less than in the first
// while the reported CPU load rises and the GPU utilisation falls, so the
// CPU families keep their first baseline and the GPUs take a new one; the
// third interval, at full load, shows which baseline each family kept.
func TestCP02Kepler07EachFamilyComparesItsOwnLoad(t *testing.T) {
	set, err := NewSet([]string{Kepler07}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatNVL())
	// snap steps the node with powers set by the first two loads and
	// reports the last two as its utilisation.
	snap := func(dt, cpuPower, gpuPower, cpuUtil, gpuUtil float64) *emulate.Snapshot {
		s := c.snap(dt, cpuPower, gpuPower)
		for i := range s.Nodes[0].Components {
			switch comp := &s.Nodes[0].Components[i]; comp.Type {
			case semconv.HwTypeCPU:
				comp.Utilization = cpuUtil
			case semconv.HwTypeGPU:
				comp.Utilization = gpuUtil
			}
		}
		return s
	}
	set.Observe(snap(0, 0.5, 0.5, 0.5, 0.5))
	baseline := snap(10, 0.5, 0.5, 0.5, 0.5)
	set.Observe(baseline)
	lower := snap(10, 0, 0, 0.75, 0)
	set.Observe(lower)
	before := compatGather(t, set, Kepler07)
	busy := snap(10, 1, 1, 1, 1)
	set.Observe(busy)
	after := compatGather(t, set, Kepler07)

	hwType := map[string]string{k07Package: semconv.HwTypeCPU, k07DRAM: semconv.HwTypeMemory, k07GPU: semconv.HwTypeGPU}
	idleW := func(s *emulate.Snapshot, family, id string) float64 {
		n := &s.Nodes[0]
		if family == k07Platform {
			return n.Host.PowerW
		}
		for _, comp := range n.Components {
			if comp.Type == hwType[family] && fmt.Sprint(comp.Index) == id {
				return comp.PowerW
			}
		}
		t.Fatalf("no %s %s in the snapshot", family, id)
		return 0
	}
	for _, sr := range []struct{ family, id string }{
		{k07Package, "0"}, {k07Package, "1"}, {k07DRAM, "0"}, {k07DRAM, "1"}, {k07Platform, k07PlatformID}, {k07GPU, "0"}, {k07GPU, "7"},
	} {
		// The CPU families kept the first interval's power, the GPUs
		// took the second's.
		from := baseline
		if sr.family == k07GPU {
			from = lower
		}
		match := map[string]string{k07LabelInstance: "nvl-0", k07LabelPackage: sr.id, k07LabelMode: k07ModeIdle}
		compatNear(t, fmt.Sprintf("%s %s idle increment at full load", sr.family, sr.id),
			compatOne(t, after, k07Family(sr.family), match)-compatOne(t, before, k07Family(sr.family), match), idleW(from, sr.family, sr.id)*10)
	}
}

// CP02: other is platform minus package, dram and gpu per mode, and adds
// nothing when that is negative. Splitting the series apart makes either
// mode negative: in the second interval the packages take a lower baseline
// while the platform, with its GPUs busy, keeps its own, so the components'
// dynamic energy exceeds the platform's; in the third the host reads less
// than its components, as when a GPU's PowerW x dt overstates its interval,
// so the components' idle energy exceeds the platform's too.
func TestCP02Kepler07OtherNeverFalls(t *testing.T) {
	set, err := NewSet([]string{Kepler07}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatNVL())
	other := func() (dyn, idle float64) {
		t.Helper()
		samples := compatGather(t, set, Kepler07)
		match := map[string]string{k07LabelInstance: "nvl-0", k07LabelPackage: k07OtherID}
		match[k07LabelMode] = k07ModeDynamic
		dyn = compatOne(t, samples, k07Family(k07Other), match)
		match[k07LabelMode] = k07ModeIdle
		return dyn, compatOne(t, samples, k07Family(k07Other), match)
	}

	set.Observe(c.snap(0, 0.5, 0))
	first := c.snap(10, 0.5, 0)
	set.Observe(first)
	// The first interval is all idle: other is the host's 80 W.
	dyn, idle := other()
	compatNear(t, "other dynamic after the first interval", dyn, 0)
	compatNear(t, "other idle after the first interval", idle, 80*10)

	second := c.snap(10, 0.25, 1)
	set.Observe(second)
	// Idle: the platform at its first power against the packages and
	// DRAM at the second, and the GPUs at the first.
	var compIdleW float64
	for i, comp := range second.Nodes[0].Components {
		if comp.Type == semconv.HwTypeGPU {
			compIdleW += first.Nodes[0].Components[i].PowerW
		} else {
			compIdleW += comp.PowerW
		}
	}
	dyn, idle = other()
	compatNear(t, "other dynamic after the second interval", dyn, 0)
	compatNear(t, "other idle after the second interval", idle, 80*10+(first.Nodes[0].Host.PowerW-compIdleW)*10)

	third := c.snap(10, 0.25, 1)
	third.Nodes[0].Host.PowerW = 100
	third.Nodes[0].Host.EnergyJ = second.Nodes[0].Host.EnergyJ + 100*10
	set.Observe(third)
	gotDyn, gotIdle := other()
	compatNear(t, "other dynamic after a short host reading", gotDyn, dyn)
	compatNear(t, "other idle after a short host reading", gotIdle, idle)
}

// CP02: the first snapshot of a node sets the baselines: every series reads 0 and
// only dynamic series exist yet, as upstream's first collection. A snapshot
// that repeats the time changes nothing. A node that leaves the snapshot
// loses its series, and when it comes back it starts again from its first
// snapshot, as a restarted exporter does.
func TestCP02Kepler07FirstSnapshotRepeatsAndRemovedNodes(t *testing.T) {
	set, err := NewSet([]string{Kepler07}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := func(what, node string) {
		t.Helper()
		series := 0
		for _, s := range compatGather(t, set, Kepler07) {
			if s.labels[k07LabelInstance] != node {
				continue
			}
			series++
			if s.value != 0 {
				t.Errorf("%s: %s%v = %g, want 0", what, s.family, s.labels, s.value)
			}
			if s.labels[k07LabelMode] == k07ModeIdle && s.family != k07Family(k07Other) {
				t.Errorf("%s: idle series %s%v before any energy", what, s.family, s.labels)
			}
		}
		if series == 0 {
			t.Fatalf("%s: %s has no series", what, node)
		}
	}
	c := compatNewCluster(compatIntel(), compatNVL())
	set.Observe(c.snap(0, 0.5, 0.5))
	first("first snapshot", "intel-0")
	first("first snapshot", "nvl-0")
	second := c.snap(10, 0.5, 0.5)
	set.Observe(second)
	before := compatGather(t, set, Kepler07)
	set.Observe(second)
	compatSame(t, "a repeated snapshot", before, compatGather(t, set, Kepler07))

	third := c.snap(10, 0.5, 0.5)
	third.Nodes = third.Nodes[:1]
	set.Observe(third)
	if got := compatFind(compatGather(t, set, Kepler07), k07Family(k07Platform), map[string]string{k07LabelInstance: "nvl-0"}); len(got) != 0 {
		t.Errorf("nvl-0 left the snapshot and still has series %v", got)
	}
	set.Observe(c.snap(10, 0.5, 0.5))
	first("back in the snapshot", "nvl-0")
}
