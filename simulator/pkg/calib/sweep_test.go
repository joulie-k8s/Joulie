package calib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The collector runs here over an hwemu tree, never over the host: the
// emulated node stands in for the CPUs and the clock. Its load comes from
// Node.SetLoad rather than from spinning threads, and the /proc/stat the
// collector reads is written from that load.

// emulatedHost is an hwemu node behind the collector's LoadDriver and Clock:
// SetUtilization sets the node's load, and Sleep advances the node and the
// CPU times of its /proc/stat.
type emulatedHost struct {
	t       testing.TB
	profile *hwemu.Profile
	tree    *hwemu.Tree
	node    *hwemu.Node
	proc    string
	now     time.Time
	cpus    int
	util    float64
	busy    float64 // USER_HZ ticks of every CPU
	idle    float64
	stopped bool
	// cancel, when set, is called after sleeps sleeps.
	cancel func()
	sleeps int
}

// newEmulatedHost renders a builtin profile, changed by mutate when set,
// with render options o, starts its node and writes its proc root.
func newEmulatedHost(t testing.TB, profile string, mutate func(*hwemu.Profile), o hwemu.RenderOptions) *emulatedHost {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatalf("builtin profiles: %v", err)
	}
	p, ok := profiles[profile]
	if !ok {
		t.Fatalf("no builtin profile %q", profile)
	}
	if mutate != nil {
		mutate(p)
	}
	root := t.TempDir()
	o.NodeName = fixtureNode
	tree, err := hwemu.Render(p, filepath.Join(root, "node"), o)
	if err != nil {
		t.Fatalf("render %s: %v", p.Name, err)
	}
	node, err := hwemu.NewNode(p, tree, hwemu.Options{Start: fixtureStart, RenderCheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("start %s: %v", p.Name, err)
	}
	e := &emulatedHost{
		t: t, profile: p, tree: tree, node: node, proc: filepath.Join(root, "proc"), now: fixtureStart,
		cpus: p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * max(1, p.CPU.ThreadsPerCore.V),
	}
	if err := os.MkdirAll(e.proc, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.ReadFile(tree.CPUInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.proc, "cpuinfo"), info, 0o444); err != nil {
		t.Fatal(err)
	}
	e.writeStat()
	return e
}

func (e *emulatedHost) SetUtilization(u float64) error {
	e.util = u
	e.node.SetLoad(hwemu.Load{CPUUtil: []float64{u}})
	return nil
}

func (e *emulatedHost) Stop() {
	e.stopped = true
	e.node.SetLoad(hwemu.Load{})
}

func (e *emulatedHost) Now() time.Time { return e.now }

func (e *emulatedHost) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		e.now = e.now.Add(d)
		e.busy += e.util * d.Seconds() * 100
		e.idle += (1 - e.util) * d.Seconds() * 100
	}
	if err := e.node.AdvanceTo(e.now); err != nil {
		return err
	}
	e.writeStat()
	e.sleeps++
	if e.cancel != nil && e.sleeps == 3 {
		e.cancel()
	}
	return nil
}

// writeStat writes /proc/stat with every CPU at the same times, plus the
// aggregate line and a few others the collector must skip.
func (e *emulatedHost) writeStat() {
	e.t.Helper()
	busy, idle := uint64(math.Round(e.busy)), uint64(math.Round(e.idle))
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu  %d 0 0 %d 0 0 0 0 0 0\n", busy*uint64(e.cpus), idle*uint64(e.cpus))
	for c := 0; c < e.cpus; c++ {
		fmt.Fprintf(&sb, "cpu%d %d 0 0 %d 0 0 0 0 0 0\n", c, busy, idle)
	}
	sb.WriteString("intr 0\nctxt 0\nbtime 1760000000\nprocesses 1\nprocs_running 1\nprocs_blocked 0\n")
	if err := os.WriteFile(filepath.Join(e.proc, "stat"), []byte(sb.String()), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// options are the sweep options over e.
func (e *emulatedHost) options(out *bytes.Buffer, steps []SweepStep, writeLimits bool) SweepOptions {
	return SweepOptions{
		SysRoot: e.tree.SysRoot, ProcRoot: e.proc, Node: fixtureNode, Steps: steps,
		Hold: 22 * time.Second, Period: 2 * time.Second, WriteLimits: writeLimits,
		Load: e, Clock: e, Out: out,
	}
}

// snapshot reads files under the sys root matching each pattern.
func snapshot(t testing.TB, sys string, patterns ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range patterns {
		m, err := filepath.Glob(filepath.Join(sys, filepath.FromSlash(p)))
		if err != nil || len(m) == 0 {
			t.Fatalf("no %s: %v", p, err)
		}
		for _, f := range m {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out[f] = string(b)
		}
	}
	return out
}

// sweepLines decodes every line of out strictly.
func sweepLines(t testing.TB, out []byte) []otlpjson.MetricsData {
	t.Helper()
	var mds []otlpjson.MetricsData
	for i, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
		md, err := otlpjson.DecodeMetricsStrict(line)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		mds = append(mds, md)
	}
	return mds
}

// attr is the text of attribute k of a point, as Series holds it.
func attr(kvs []otlpjson.KeyValue, k string) (string, bool) {
	for _, kv := range kvs {
		if kv.Key == k {
			return attrText(kv.Value)
		}
	}
	return "", false
}

// pointValue is a point's value.
func pointValue(p otlpjson.NumberDataPoint) float64 {
	v, _ := otlpPointValue(p)
	return v
}

// CB11: hwcalib sweep over an hwemu tree of amd-epyc-2s-energy-only under
// the performance governor, with the sweep of CB01 (three scaling_max_freq
// ceilings, six loads, 22 s each, sampled every 2 s) and Node.SetLoad in
// place of the spinning threads, writes a sweep the fit reads back to the
// profile within CB01's tolerances. Every line is a MetricsData with the
// pinned schema, the utilization of every logical CPU carries
// cpu.logical_number, only what the tree shows is emitted (no hw.host.*),
// the load is stopped and scaling_max_freq is put back.
func TestCB11SweepCollectorOverAnHwemuTree(t *testing.T) {
	t.Parallel()
	e := newEmulatedHost(t, "amd-epyc-2s-energy-only", func(p *hwemu.Profile) { p.CPUFreq.Governor.V = "performance" }, hwemu.RenderOptions{Seed: 1})
	before := snapshot(t, e.tree.SysRoot, "devices/system/cpu/cpufreq/policy*/scaling_max_freq")
	steps := SweepGrid([]float64{0, 0.2, 0.4, 0.6, 0.8, 1}, []float64{2.4e9, 1.9e9, 1.5e9}, nil)
	var out bytes.Buffer
	n, err := Sweep(context.Background(), e.options(&out, steps, true))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 18*11 {
		t.Errorf("%d samples, want %d", n, 18*11)
	}
	if !e.stopped {
		t.Error("the load was not stopped")
	}
	if after := snapshot(t, e.tree.SysRoot, "devices/system/cpu/cpufreq/policy*/scaling_max_freq"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("scaling_max_freq was not put back")
	}

	mds := sweepLines(t, out.Bytes())
	if len(mds) != n {
		t.Fatalf("%d lines for %d samples", len(mds), n)
	}
	allowed := map[string]bool{
		semconv.HwEnergy.Name: true, semconv.HwCPUSpeed.Name: true, semconv.HwCPUSpeedLimit.Name: true,
		semconv.SystemCPUUtilization.Name: true, semconv.JoulieCalibrationSetpoint.Name: true,
	}
	for i, md := range mds {
		if len(md.ResourceMetrics) != 1 || len(md.ResourceMetrics[0].ScopeMetrics) != 1 {
			t.Fatalf("line %d: want one resource and one scope", i+1)
		}
		rm := md.ResourceMetrics[0]
		sm := rm.ScopeMetrics[0]
		if rm.SchemaURL != semconv.SchemaURL || sm.SchemaURL != semconv.SchemaURL || sm.Scope.Name != SweepScopeName {
			t.Fatalf("line %d: schema %q and %q, scope %q", i+1, rm.SchemaURL, sm.SchemaURL, sm.Scope.Name)
		}
		if v, _ := attr(rm.Resource.Attributes, semconv.AttrHostName); v != fixtureNode {
			t.Fatalf("line %d: host.name %q", i+1, v)
		}
		u := steps[i/11].CPUUtilization
		for _, m := range sm.Metrics {
			if !allowed[m.Name] {
				t.Errorf("line %d: %s is not read from this tree", i+1, m.Name)
			}
			if m.Name == semconv.HwEnergy.Name {
				for _, p := range m.Sum.DataPoints {
					if typ, _ := attr(p.Attributes, semconv.AttrHwType); typ != semconv.HwTypeCPU {
						t.Errorf("line %d: hw.energy of %s on a profile without DRAM zones", i+1, typ)
					}
				}
			}
			if m.Name != semconv.SystemCPUUtilization.Name {
				continue
			}
			seen := map[string]bool{}
			for _, p := range m.Gauge.DataPoints {
				c, ok := attr(p.Attributes, semconv.AttrCPULogicalNumber)
				if mode, _ := attr(p.Attributes, semconv.AttrCPUMode); !ok || mode != semconv.CPUModeIdle {
					t.Fatalf("line %d: a utilization point without cpu.logical_number or cpu.mode=idle", i+1)
				}
				seen[c] = true
				if v := pointValue(p); math.Abs(v-(1-u)) > 0.01 {
					t.Fatalf("line %d: cpu %s idle %v at utilization %v", i+1, c, v, u)
				}
			}
			if len(seen) != e.cpus {
				t.Fatalf("line %d: %d logical CPUs, want %d", i+1, len(seen), e.cpus)
			}
		}
	}

	d, err := ReadOTLP(bytes.NewReader(out.Bytes()), fixtureNode)
	if err != nil {
		t.Fatalf("read the sweep: %v", err)
	}
	r, err := Fit(d, e.profile, Options{})
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	within(t, r, pathCPUIdle, 40, 0.01)
	within(t, r, pathCPUMax, 360, 0.01)
	near(t, r, pathCPUAlpha, 1.15, 0.02)
	near(t, r, pathCPUBeta, 1.35, 0.03)
	if r.Segments != 18 || r.Steady != 18 {
		t.Errorf("%d segments, %d steady, want 18 and 18", r.Segments, r.Steady)
	}
	if p := param(t, r, pathPlatform); p.Fitted {
		t.Errorf("platform fitted to %v from a sweep without host power", p.Value)
	}
}

// A package limit goes to the long_term constraint of each package zone,
// the node settles at it, the sweep records it as requested and enforced,
// DRAM is read with its parent, psys is never read, and the limits are put
// back. intel-xeon-2s-psys has psys between the two package zones.
func TestSweepWritesRAPLLimitsAndPutsThemBack(t *testing.T) {
	t.Parallel()
	e := newEmulatedHost(t, "intel-xeon-2s-psys", nil, hwemu.RenderOptions{Seed: 1})
	limitFiles := "devices/virtual/powercap/intel-rapl/intel-rapl:*/constraint_0_power_limit_uw"
	before := snapshot(t, e.tree.SysRoot, limitFiles)
	var out bytes.Buffer
	n, err := Sweep(context.Background(), e.options(&out, SweepGrid([]float64{1}, nil, []float64{150, 100}), true))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 22 {
		t.Errorf("%d samples, want 22", n)
	}
	if after := snapshot(t, e.tree.SysRoot, limitFiles); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("limits not put back:\nbefore %v\nafter  %v", before, after)
	}

	d, err := ReadOTLP(bytes.NewReader(out.Bytes()), fixtureNode)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range d.Select(semconv.HwEnergy.Name, nil) {
		ids = append(ids, s.Attrs[semconv.AttrHwID]+"/"+s.Attrs[semconv.AttrHwParent])
	}
	if got := strings.Join(ids, " "); got != "cpu0/ cpu1/ memory0/cpu0 memory1/cpu1" {
		t.Errorf("hw.energy series %q, want the two packages and their DRAM only", got)
	}
	segs, err := Segments(d)
	if err != nil || len(segs) != 2 {
		t.Fatalf("%d segments: %v", len(segs), err)
	}
	for k, lim := range []float64{150, 100} {
		a, b, _ := segs[k].Steady(10 * time.Second)
		for _, id := range []string{"cpu0", "cpu1"} {
			for _, state := range []string{semconv.LimitStateRequested, semconv.LimitStateEnforced} {
				s := d.Select(semconv.JoulieHwPowerLimit.Name, map[string]string{semconv.AttrHwID: id, semconv.AttrJoulieLimitState: state})
				if len(s) != 1 {
					t.Fatalf("%s: %d %s limit series", id, len(s), state)
				}
				if v, _ := s[0].mean(a, b); v != lim {
					t.Errorf("%s: %s limit %v, want %v", id, state, v, lim)
				}
			}
			w, n := d.Select(semconv.HwEnergy.Name, map[string]string{semconv.AttrHwID: id})[0].rate(a, b)
			if n == 0 || w > lim*1.02 || w < lim*0.9 {
				t.Errorf("%s draws %.1f W under a %v W limit", id, w, lim)
			}
		}
	}
}

// A step that sets a frequency ceiling or a limit needs write-limits: the
// sweep fails before it writes a file or a line, and still stops the load.
func TestSweepRefusesKnobsWithoutWriteLimits(t *testing.T) {
	t.Parallel()
	e := newEmulatedHost(t, "intel-xeon-2s-rapl", nil, hwemu.RenderOptions{Seed: 1})
	before := snapshot(t, e.tree.SysRoot, "devices/system/cpu/cpufreq/policy*/scaling_max_freq", "devices/virtual/powercap/intel-rapl/intel-rapl:*/constraint_0_power_limit_uw")
	for _, st := range []SweepStep{{CPUUtilization: 1, FrequencyMaxHz: 2e9}, {CPUUtilization: 1, PowerLimitW: 100}} {
		var out bytes.Buffer
		_, err := Sweep(context.Background(), e.options(&out, []SweepStep{st}, false))
		if err == nil || !strings.Contains(err.Error(), "write-limits") {
			t.Errorf("%+v: err %v, want one naming write-limits", st, err)
		}
		if out.Len() != 0 {
			t.Errorf("%+v: wrote %d bytes", st, out.Len())
		}
	}
	if after := snapshot(t, e.tree.SysRoot, "devices/system/cpu/cpufreq/policy*/scaling_max_freq", "devices/virtual/powercap/intel-rapl/intel-rapl:*/constraint_0_power_limit_uw"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("a file was written")
	}
	if !e.stopped {
		t.Error("the load was not stopped")
	}
}

// energy_uj that cannot be read for its mode is an error naming root and
// the kernel commit that made it 0400.
func TestSweepNeedsRootToReadEnergy(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of any mode")
	}
	e := newEmulatedHost(t, "intel-xeon-2s-rapl", nil, hwemu.RenderOptions{Seed: 1})
	f := filepath.Join(e.tree.SysRoot, filepath.FromSlash(layout.PowercapZoneDir("intel-rapl", 0)), "energy_uj")
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := Sweep(context.Background(), e.options(&out, SweepGrid([]float64{0}, nil, nil), false))
	if err == nil || !strings.Contains(err.Error(), "needs root") || !strings.Contains(err.Error(), "949dd0104c49") {
		t.Errorf("err %v, want one naming root and the commit", err)
	}
}

// A counter that wraps during the sweep keeps counting: the energy the
// sweep reports for package 0 is the node's own, and never falls.
func TestSweepEnergyCountsAcrossAWrap(t *testing.T) {
	t.Parallel()
	p, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	unit := p["intel-xeon-2s-rapl"].Powercap.ControlTypes[0].Zones[0].EnergyUnitNJ.V
	start := hwemu.MaxEnergyRangeUJ(unit) - 500_000_000
	e := newEmulatedHost(t, "intel-xeon-2s-rapl", nil, hwemu.RenderOptions{Seed: 1, EnergyStartUJ: map[string]int64{"intel-rapl:0": start}})
	var out bytes.Buffer
	if _, err := Sweep(context.Background(), e.options(&out, SweepGrid([]float64{1}, nil, nil), false)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(e.tree.SysRoot, filepath.FromSlash(layout.PowercapZoneDir("intel-rapl", 0)), "energy_uj"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); v >= start {
		t.Fatalf("energy_uj %d: the counter did not wrap", v)
	}
	d, err := ReadOTLP(bytes.NewReader(out.Bytes()), fixtureNode)
	if err != nil {
		t.Fatal(err)
	}
	s := d.Select(semconv.HwEnergy.Name, map[string]string{semconv.AttrHwID: "cpu0"})[0]
	for i := 1; i < len(s.Points); i++ {
		if s.Points[i].Value < s.Points[i-1].Value {
			t.Fatalf("hw.energy fell from %v to %v", s.Points[i-1].Value, s.Points[i].Value)
		}
	}
	var want float64
	for _, c := range e.node.State().Components {
		if c.ID == "cpu0" {
			want = c.EnergyJ
		}
	}
	if got := s.Points[len(s.Points)-1].Value; math.Abs(got-want) > 0.01 {
		t.Errorf("hw.energy %v J, the node counted %v J", got, want)
	}
}

// A sweep that ends early, here by its context, still puts back what it
// wrote and stops the load; the lines written so far stay whole.
func TestSweepPutsBackOnCancel(t *testing.T) {
	t.Parallel()
	e := newEmulatedHost(t, "intel-xeon-2s-rapl", nil, hwemu.RenderOptions{Seed: 1})
	files := []string{"devices/system/cpu/cpufreq/policy*/scaling_max_freq", "devices/virtual/powercap/intel-rapl/intel-rapl:*/constraint_0_power_limit_uw"}
	before := snapshot(t, e.tree.SysRoot, files...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.cancel = cancel
	var out bytes.Buffer
	n, err := Sweep(ctx, e.options(&out, SweepGrid([]float64{1}, []float64{2e9}, []float64{120}), true))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if n != 3 || len(sweepLines(t, out.Bytes())) != 3 {
		t.Errorf("%d samples before the cancel, want 3", n)
	}
	if after := snapshot(t, e.tree.SysRoot, files...); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("files not put back")
	}
	if !e.stopped {
		t.Error("the load was not stopped")
	}
}

// SweepGrid nests limits outside frequencies outside loads.
func TestSweepGridOrder(t *testing.T) {
	got := SweepGrid([]float64{0, 1}, []float64{1e9, 2e9}, []float64{100})
	want := []SweepStep{{0, 1e9, 100}, {1, 1e9, 100}, {0, 2e9, 100}, {1, 2e9, 100}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("SweepGrid = %v, want %v", got, want)
	}
	if got := SweepGrid([]float64{0.5}, nil, nil); len(got) != 1 || got[0] != (SweepStep{CPUUtilization: 0.5}) {
		t.Errorf("SweepGrid of loads only = %v", got)
	}
}

// The duty-cycle load takes a share in 0..1 and stops promptly. It runs on
// this machine for a few milliseconds on one CPU, so nothing else is
// checked.
func TestDutyCycleLoadStops(t *testing.T) {
	l := NewDutyCycleLoad([]int{0}, 5*time.Millisecond, nil)
	if err := l.SetUtilization(0.5); err != nil {
		t.Fatal(err)
	}
	for _, u := range []float64{-0.1, 1.5, math.NaN()} {
		if err := l.SetUtilization(u); err == nil {
			t.Errorf("SetUtilization(%v) accepted", u)
		}
	}
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { l.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	l.Stop()
}
