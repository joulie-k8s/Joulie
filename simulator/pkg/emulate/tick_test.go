package emulate

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

func emuFloat(v float64) *float64 { return &v }

func emuUtilNear(t *testing.T, what string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}

// EM02: one Tick steps every member to the same time and grows Seq by 1. A
// member whose step fails gets the error in its Err and keeps the readings
// of its last good step, while the others advance; the error is counted for
// that node, and the next good step catches the node up.
func TestEM02TickStepsEveryMemberToTheSameTime(t *testing.T) {
	f := newEmuFakes()
	reg := prometheus.NewRegistry()
	c := emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0), emuMember("b", "", "", 1, 2), emuMember("c", "", "", 2, 0))
	c.Registerer, c.Workers = reg, 3
	e := emuNew(t, c)

	first := emuTick(t, e, time.Second, nil, true, nil)
	if first.Seq != 1 || first.Cluster != "test" || !first.Time.Equal(emuStart.Add(time.Second)) {
		t.Fatalf("first snapshot: Seq %d, cluster %q, time %v", first.Seq, first.Cluster, first.Time)
	}
	for _, n := range []string{"a", "b", "c"} {
		if adv := f.runner(t, n).advanced(); len(adv) != 1 || !adv[0].Equal(emuStart.Add(time.Second)) {
			t.Errorf("node %s advanced to %v, want once to +1s", n, adv)
		}
	}

	// The failed step also draws more power, which the snapshot must not
	// show until a step succeeds.
	rb := f.runner(t, "b")
	rb.mu.Lock()
	rb.failNext, rb.cpuW = errors.New("disk full"), 300
	rb.mu.Unlock()
	second, err := e.Tick(emuStart.Add(2*time.Second), nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "node b") || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Tick error = %v, want one naming node b and its error", err)
	}
	if second == nil || second.Seq != 2 || e.Latest() != second {
		t.Fatalf("a tick with a failed node did not publish Seq 2: %v", second)
	}
	b1, b2 := emuNode(t, first, "b"), emuNode(t, second, "b")
	if !strings.Contains(b2.Err, "disk full") {
		t.Errorf("node b Err = %q, want the step error", b2.Err)
	}
	// The inlet follows the facility, not the node's step.
	h1, h2 := b1.Host, b2.Host
	h1.InletC, h2.InletC = 0, 0
	if h2 != h1 || !reflect.DeepEqual(b2.Components, b1.Components) {
		t.Errorf("node b readings changed on a failed step:\n%+v\nwant\n%+v", b2, b1)
	}
	for _, n := range []string{"a", "c"} {
		before, after := emuNode(t, first, n), emuNode(t, second, n)
		if after.Err != "" || after.Host.EnergyJ <= before.Host.EnergyJ {
			t.Errorf("node %s did not advance next to the failed node: Err %q, energy %v then %v", n, after.Err, before.Host.EnergyJ, after.Host.EnergyJ)
		}
		if adv := f.runner(t, n).advanced(); !adv[len(adv)-1].Equal(emuStart.Add(2 * time.Second)) {
			t.Errorf("node %s last advanced to %v, want +2s", n, adv[len(adv)-1])
		}
	}
	if got := emuMetric(t, reg, "joulie_sim_emulate_node_step_errors_total", map[string]string{"node": "b"}); got != 1 {
		t.Errorf("step errors of b = %v, want 1", got)
	}
	if got := emuMetric(t, reg, "joulie_sim_emulate_node_step_errors_total", map[string]string{"node": "a"}); got != 0 {
		t.Errorf("step errors of a = %v, want 0", got)
	}

	third := emuTick(t, e, 3*time.Second, nil, true, nil)
	b3 := emuNode(t, third, "b")
	hostW := 300 + 10 + 2*200 + 50.0
	if b3.Err != "" {
		t.Errorf("node b Err after a good step = %q", b3.Err)
	}
	emuNear(t, "node b host power after a good step", b3.Host.PowerW, hostW)
	emuNear(t, "node b host energy after catching up", b3.Host.EnergyJ, hostW*3)
}

// EM08: a member's base load is added to its job load and the sum is
// clamped at 1 per element; the GPU base reaches every GPU, including those
// the job load leaves out. A member without a job load runs its base load.
func TestEM08BaseLoadAddsToJobLoadClamped(t *testing.T) {
	f := newEmuFakes()
	busy := emuMember("busy", "", "", 2, 4)
	idle := emuMember("idle", "", "", 2, 4)
	base := clusterspec.Load{CPUUtil: emuFloat(0.5), GPUUtil: emuFloat(0.3), MemoryIntensity: emuFloat(0.2), CPUClass: "cpu.base", GPUClass: "gpu.base"}
	busy.BaseLoad, idle.BaseLoad = base, base
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, busy, idle))

	job := hwemu.Load{CPUUtil: []float64{0.8, 0.2}, GPUUtil: []float64{0.9, 0}, CPUClass: "cpu.job"}
	emuTick(t, e, time.Second, map[string]hwemu.Load{"busy": job}, true, nil)

	got := f.runner(t, "busy").lastLoad(t)
	emuUtilNear(t, "busy CPUUtil", got.CPUUtil, []float64{1, 0.7})
	emuUtilNear(t, "busy GPUUtil", got.GPUUtil, []float64{1, 0.3, 0.3, 0.3})
	if got.CPUClass != "cpu.job" || got.GPUClass != "gpu.base" || got.MemoryIntensity != 0.2 {
		t.Errorf("busy classes %q, %q, memory intensity %v; want the job's CPU class and the base for the rest", got.CPUClass, got.GPUClass, got.MemoryIntensity)
	}
	got = f.runner(t, "idle").lastLoad(t)
	emuUtilNear(t, "idle CPUUtil", got.CPUUtil, []float64{0.5})
	emuUtilNear(t, "idle GPUUtil", got.GPUUtil, []float64{0.3})
	if got.CPUClass != "cpu.base" || got.GPUClass != "gpu.base" {
		t.Errorf("idle classes %q, %q; want the base's", got.CPUClass, got.GPUClass)
	}

	// One value for all, and one per logical CPU, keep their shape.
	all := hwemu.Load{CPUUtil: []float64{0.9}, GPUUtil: []float64{0.4}}
	emuTick(t, e, 2*time.Second, map[string]hwemu.Load{"busy": all}, true, nil)
	got = f.runner(t, "busy").lastLoad(t)
	emuUtilNear(t, "busy CPUUtil, one value", got.CPUUtil, []float64{1})
	emuUtilNear(t, "busy GPUUtil, one value", got.GPUUtil, []float64{0.7})
}

// EM09: a rack's power is the sum of its hosts' and its energy grows by
// their ledger increases; a zone is the ambient plus its offset and its
// hosts' inlet is the zone's temperature, the ambient for a host without a
// zone. IT energy grows by the hosts' ledger increases plus the legacy
// nodes' power over the tick, and cooling energy integrates cooling power.
// Host b's counter grows faster than its sampled power, as a real host's
// does when its power varies between samples, so the hosts' share of IT
// energy is their energy, not IT power x tick.
func TestEM09FacilityRacksAndZones(t *testing.T) {
	const extraW = 40.0
	f := newEmuFakes()
	f.setup = func(node string, r *emuFakeRunner) {
		if node == "b" {
			r.hostExtraW = extraW
		}
	}
	a := emuMember("a", "rack-1", "zone-a", 1, 0)
	b := emuMember("b", "rack-1", "zone-b", 1, 2)
	c := emuMember("c", "", "", 1, 0)
	a.ZoneOffsetC, b.ZoneOffsetC = 2, 5
	cfg := emuConfig(t.TempDir(), f, &emuLog{}, a, b, c)
	cfg.Facility.CoolingZones = []clusterspec.CoolingZone{{Name: "zone-a", OffsetC: 2}, {Name: "zone-b", OffsetC: 5}, {Name: "zone-c", OffsetC: -1}}
	e := emuNew(t, cfg)

	const legacyW = 100.0
	var gotSpecW []float64
	fac := func(specW float64, now time.Time) facility.Values {
		gotSpecW = append(gotSpecW, specW)
		it := specW + legacyW
		return facility.Values{AmbientC: 21, ITPowerW: it, PUE: 1.2, CoolingPowerW: 0.2 * it}
	}
	hostA, hostB, hostC := 160.0, 160+2*200.0, 160.0
	itW := hostA + hostB + hostC + legacyW
	itJPerS := itW + extraW

	s1 := emuTick(t, e, time.Second, nil, true, fac)
	s2 := emuTick(t, e, 3*time.Second, nil, true, fac)
	emuNear(t, "IT power passed to the facility", gotSpecW[1], hostA+hostB+hostC)

	fs := s2.Facility
	emuNear(t, "IT power", fs.ITPowerW, itW)
	emuNear(t, "IT energy over 3 s", fs.ITEnergyJ, itJPerS*3)
	emuNear(t, "cooling energy over 3 s", fs.CoolingEnergyJ, 0.2*itW*3)
	emuNear(t, "IT energy over the first 1 s", s1.Facility.ITEnergyJ, itJPerS)
	if !fs.StartTime.Equal(emuStart) || fs.AmbientC != 21 || fs.PUE != 1.2 {
		t.Errorf("facility start %v, ambient %v, PUE %v", fs.StartTime, fs.AmbientC, fs.PUE)
	}
	if len(fs.Racks) != 1 || fs.Racks[0].Name != "rack-1" {
		t.Fatalf("racks = %+v, want rack-1 only", fs.Racks)
	}
	emuNear(t, "rack-1 power", fs.Racks[0].PowerW, hostA+hostB)
	emuNear(t, "rack-1 energy", fs.Racks[0].EnergyJ, emuNode(t, s2, "a").Host.EnergyJ+emuNode(t, s2, "b").Host.EnergyJ)
	emuNear(t, "b host energy over 3 s", emuNode(t, s2, "b").Host.EnergyJ, (hostB+extraW)*3)

	wantZones := []ZoneReading{{Name: "zone-a", AmbientC: 23}, {Name: "zone-b", AmbientC: 26}, {Name: "zone-c", AmbientC: 20}}
	if !reflect.DeepEqual(fs.Zones, wantZones) {
		t.Errorf("zones = %+v, want %+v", fs.Zones, wantZones)
	}
	for name, want := range map[string]float64{"a": 23, "b": 26, "c": 21} {
		if got := emuNode(t, s2, name).Host.InletC; got != want {
			t.Errorf("node %s inlet = %v, want %v", name, got, want)
		}
	}
}

// Without a FacilityFunc, the facility follows package facility over the
// spec's parameters, with only the emulated hosts as IT load; a parameter
// the spec leaves out is the simulator's default: 20 C ambient, 5 C
// amplitude, a 24 h period. The tick is 3 h in, where the default period
// gives the ambient neither its base nor its peak.
func TestTickWithoutFacilityFuncUsesTheSpecFacility(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ambientC, amplitude *float64
		want                facility.Params
	}{
		{"set", emuFloat(25), emuFloat(0), facility.Params{BaseAmbientC: 25, AmplitudeC: 0, PeriodH: 24}},
		{"defaults", nil, nil, facility.Params{BaseAmbientC: 20, AmplitudeC: 5, PeriodH: 24}},
	} {
		f := newEmuFakes()
		cfg := emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0))
		cfg.Facility.AmbientC, cfg.Facility.AmplitudeC = tc.ambientC, tc.amplitude
		e := emuNew(t, cfg)
		s := emuTick(t, e, 3*time.Hour, nil, true, nil)
		want, _ := facility.Compute(tc.want, 3*time.Hour, 160, 0)
		if s.Facility.AmbientC != want.AmbientC || s.Facility.ITPowerW != 160 || s.Facility.PUE != want.PUE {
			t.Errorf("%s: facility = %+v, want %+v", tc.name, s.Facility, want)
		}
	}
}

// A cooling zone a member names but the facility does not declare is
// published at the member's offset; a declared zone keeps its own offset.
func TestUndeclaredZoneTakesTheMemberOffset(t *testing.T) {
	a := emuMember("a", "", "zone-x", 1, 0)
	b := emuMember("b", "", "zone-y", 1, 0)
	a.ZoneOffsetC, b.ZoneOffsetC = 3, 7
	cfg := emuConfig(t.TempDir(), newEmuFakes(), &emuLog{}, a, b)
	cfg.Facility.CoolingZones = []clusterspec.CoolingZone{{Name: "zone-y", OffsetC: 1}}
	e := emuNew(t, cfg)
	s := emuTick(t, e, time.Second, nil, true, func(specW float64, _ time.Time) facility.Values {
		return facility.Values{AmbientC: 21, ITPowerW: specW, PUE: 1.2}
	})
	wantZones := []ZoneReading{{Name: "zone-x", AmbientC: 24}, {Name: "zone-y", AmbientC: 22}}
	if !reflect.DeepEqual(s.Facility.Zones, wantZones) {
		t.Errorf("zones = %+v, want %+v", s.Facility.Zones, wantZones)
	}
	for name, want := range map[string]float64{"a": 24, "b": 22} {
		if got := emuNode(t, s, name).Host.InletC; got != want {
			t.Errorf("node %s inlet = %v, want %v", name, got, want)
		}
	}
}

// Each snapshot carries the events of its own step, copied so the runner
// cannot change a published snapshot, and the driver version the runner
// reports. The first snapshot after a New that put part of the tree back
// carries what Ensure added, and the next one does not.
func TestSnapshotCarriesEventsAddedAndDriverVersion(t *testing.T) {
	root := t.TempDir()
	ev := func(kind string) hwemu.Event {
		return hwemu.Event{Time: emuStart, File: "power1_cap", Kind: kind, Written: "300000000", Effective: "300000000", Component: "gpu0"}
	}
	steps := func() [][]hwemu.Event { return [][]hwemu.Event{{ev("accepted"), ev("clamped")}, nil, {ev("restored")}} }
	want := steps()
	f := newEmuFakes()
	f.setup = func(_ string, r *emuFakeRunner) { r.stepEvents, r.driver = steps(), "550.54.15" }
	c := emuConfig(root, f, &emuLog{}, emuMember("a", "", "", 1, 1))
	e := emuNew(t, c)
	var snaps []*Snapshot
	for i := 1; i <= len(want); i++ {
		snaps = append(snaps, emuTick(t, e, time.Duration(i)*time.Second, nil, true, nil))
	}
	r := f.runner(t, "a")
	r.mu.Lock()
	r.events[0].Kind = "changed by the runner"
	r.mu.Unlock()
	for i, s := range snaps {
		n := emuNode(t, s, "a")
		if !reflect.DeepEqual(n.Events, want[i]) {
			t.Errorf("tick %d events = %+v, want %+v", i+1, n.Events, want[i])
		}
		if n.DriverVersion != "550.54.15" {
			t.Errorf("tick %d driver version = %q", i+1, n.DriverVersion)
		}
		if n.Added != nil {
			t.Errorf("tick %d of a fresh tree: Added %v", i+1, n.Added)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(root, "a", layout.SysDir)); err != nil {
		t.Fatal(err)
	}
	f2 := newEmuFakes()
	c.Ensure, c.NewRunner = f2.ensure, f2.newRunner
	e2 := emuNew(t, c)
	first := emuNode(t, emuTick(t, e2, time.Minute, nil, true, nil), "a")
	if !first.Started || first.Fresh || !reflect.DeepEqual(first.Added, []string{layout.SysDir}) {
		t.Errorf("first snapshot after the restart: Started %v, Fresh %v, Added %v; want Started and Added [%s]", first.Started, first.Fresh, first.Added, layout.SysDir)
	}
	if next := emuNode(t, emuTick(t, e2, time.Minute+time.Second, nil, true, nil), "a"); next.Added != nil || next.Started {
		t.Errorf("second snapshot after the restart: Started %v, Added %v; want neither", next.Started, next.Added)
	}
}

// EM13: with applyBase false, as during a workload run, the job load alone
// reaches the runner, and a member without one runs idle; the base load is
// back once applyBase is set again.
func TestEM13NoBaseLoadDuringARun(t *testing.T) {
	f := newEmuFakes()
	busy := emuMember("busy", "", "", 2, 4)
	idle := emuMember("idle", "", "", 2, 4)
	base := clusterspec.Load{CPUUtil: emuFloat(0.5), GPUUtil: emuFloat(0.3), MemoryIntensity: emuFloat(0.2), CPUClass: "cpu.base"}
	busy.BaseLoad, idle.BaseLoad = base, base
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, busy, idle))

	job := hwemu.Load{CPUUtil: []float64{0.8, 0.2}, GPUUtil: []float64{0.9, 0}, GPUClass: "gpu.job"}
	emuTick(t, e, time.Second, map[string]hwemu.Load{"busy": job}, false, nil)
	if got := f.runner(t, "busy").lastLoad(t); !reflect.DeepEqual(got, job) {
		t.Errorf("busy got %+v during a run, want the job load alone %+v", got, job)
	}
	if got := f.runner(t, "idle").lastLoad(t); !reflect.DeepEqual(got, hwemu.Load{}) {
		t.Errorf("idle got %+v during a run, want no load", got)
	}

	emuTick(t, e, 2*time.Second, nil, true, nil)
	got := f.runner(t, "idle").lastLoad(t)
	emuUtilNear(t, "idle CPUUtil after the run", got.CPUUtil, []float64{0.5})
	if got.CPUClass != "cpu.base" {
		t.Errorf("idle class after the run = %q", got.CPUClass)
	}
}

// A load that did not change since the last tick is not set again: hwemu
// restarts a constant power segment on every SetLoad.
func TestTickSetsOnlyAChangedLoad(t *testing.T) {
	f := newEmuFakes()
	m := emuMember("a", "", "", 1, 0)
	m.BaseLoad.CPUUtil = emuFloat(0.5)
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, m))
	for i := 1; i <= 3; i++ {
		emuTick(t, e, time.Duration(i)*time.Second, nil, true, nil)
	}
	emuTick(t, e, 4*time.Second, map[string]hwemu.Load{"a": {CPUUtil: []float64{0.1}}}, true, nil)
	r := f.runner(t, "a")
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.loads) != 2 {
		t.Errorf("%d SetLoad calls over 3 equal loads and a new one, want 2: %+v", len(r.loads), r.loads)
	}
}

// A tick earlier than the last one is refused, publishes nothing and steps
// no node.
func TestTickRefusesTimeGoingBack(t *testing.T) {
	f := newEmuFakes()
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0)))
	s := emuTick(t, e, 2*time.Second, nil, true, nil)
	if got, err := e.Tick(emuStart.Add(time.Second), nil, true, nil); err == nil || got != nil {
		t.Errorf("Tick back in time: %v, %v; want only an error", got, err)
	}
	if e.Latest() != s || len(f.runner(t, "a").advanced()) != 1 {
		t.Error("a refused tick changed the published snapshot or stepped the node")
	}
}
