package emulate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// emuStart is the clock start of every test emulator.
var emuStart = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// emuFakeRunner is a node whose components draw constant power: one CPU
// package, its DRAM zone, gpus GPUs and the platform. Its counters integrate
// that power from the start the emulator gives it, so a new runner starts
// them at 0 as a new hwemu.Node does.
type emuFakeRunner struct {
	mu         sync.Mutex
	opts       hwemu.Options
	now        time.Time
	loads      []hwemu.Load
	advances   []time.Time
	failNext   error           // returned by the next AdvanceTo
	takes      []time.Duration // how long the first AdvanceTo calls take, in order
	clock      *emuClock       // passes takes; nil sleeps them
	backJ      float64         // subtracted from every counter
	hostExtraW float64         // in the host counter but not in its power: power that varied between samples
	nonFinite  bool            // every counter reads NaN
	stepEvents [][]hwemu.Event // the events of the first AdvanceTo calls, in order
	events     []hwemu.Event   // the events of the last AdvanceTo, which State returns as is
	driver     string
	gpus       int
	cpuW       float64
	memW       float64
	gpuW       float64
	otherW     float64
}

func (r *emuFakeRunner) SetLoad(l hwemu.Load) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads = append(r.loads, l)
}

func (r *emuFakeRunner) AdvanceTo(t time.Time) error {
	r.mu.Lock()
	var take time.Duration
	if len(r.takes) > 0 {
		take, r.takes = r.takes[0], r.takes[1:]
	}
	clock := r.clock
	r.mu.Unlock()
	if clock != nil {
		clock.advance(take)
	} else {
		time.Sleep(take)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.advances = append(r.advances, t)
	// Like hwemu, a failed step still advances the clock.
	r.now = t
	r.events = nil
	if len(r.stepEvents) > 0 {
		r.events, r.stepEvents = r.stepEvents[0], r.stepEvents[1:]
	}
	err := r.failNext
	r.failNext = nil
	return err
}

func (r *emuFakeRunner) State() hwemu.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	sec := r.now.Sub(r.opts.Start).Seconds()
	if r.now.IsZero() {
		sec = 0
	}
	counter := func(w float64) float64 {
		if r.nonFinite {
			return math.NaN()
		}
		return w*sec - r.backJ
	}
	comp := func(id, typ, parent string, idx int, w float64) hwemu.ComponentState {
		return hwemu.ComponentState{ID: id, Type: typ, Name: id, Parent: parent, Index: idx, Measured: true, PowerW: w, EnergyJ: counter(w)}
	}
	st := hwemu.State{Time: r.now, Events: r.events, DriverVersion: r.driver}
	st.Components = append(st.Components, comp("cpu0", "cpu", "", 0, r.cpuW), comp("memory0", "memory", "cpu0", 0, r.memW))
	for i := 0; i < r.gpus; i++ {
		st.Components = append(st.Components, comp(fmt.Sprintf("gpu%d", i), "gpu", "", i, r.gpuW))
	}
	hostW := r.cpuW + r.memW + float64(r.gpus)*r.gpuW + r.otherW
	st.Host = hwemu.HostState{Measured: true, PowerW: hostW, EnergyJ: counter(hostW + r.hostExtraW), OtherW: r.otherW}
	return st
}

func (r *emuFakeRunner) lastLoad(t *testing.T) hwemu.Load {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.loads) == 0 {
		t.Fatal("the runner never got a load")
	}
	return r.loads[len(r.loads)-1]
}

func (r *emuFakeRunner) advanced() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.advances...)
}

// emuFakes is the Ensure and NewRunner of a test emulator. Ensure renders a
// minimal tree into an absent or empty root and resumes a tree it rendered,
// putting sys/ back when it is missing. Every runner it starts is kept by
// node name, the last one per node.
type emuFakes struct {
	mu      sync.Mutex
	runners map[string]*emuFakeRunner
	ensured []string                       // roots, in call order
	render  map[string]hwemu.RenderOptions // by node name
	setup   func(node string, r *emuFakeRunner)
}

func newEmuFakes() *emuFakes {
	return &emuFakes{runners: map[string]*emuFakeRunner{}, render: map[string]hwemu.RenderOptions{}}
}

func (f *emuFakes) ensure(p *hwemu.Profile, root string, o hwemu.RenderOptions) (*hwemu.Tree, hwemu.EnsureReport, error) {
	f.mu.Lock()
	f.ensured = append(f.ensured, root)
	f.render[o.NodeName] = o
	f.mu.Unlock()
	meta := filepath.Join(root, layout.StateDir, "hwemu.json")
	tree := &hwemu.Tree{Root: root, NodeName: o.NodeName, SysRoot: filepath.Join(root, layout.SysDir), StateRoot: filepath.Join(root, layout.StateDir)}
	data, err := os.ReadFile(meta)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if entries, _ := os.ReadDir(root); len(entries) > 0 {
			return nil, hwemu.EnsureReport{}, hwemu.ErrTreeConflict
		}
		for _, d := range []string{layout.SysDir, layout.StateDir} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				return nil, hwemu.EnsureReport{}, err
			}
		}
		b, _ := json.Marshal(map[string]string{"profile": p.Name, "node": o.NodeName})
		if err := os.WriteFile(meta, b, 0o444); err != nil {
			return nil, hwemu.EnsureReport{}, err
		}
		return tree, hwemu.EnsureReport{Fresh: true}, nil
	case err != nil:
		return nil, hwemu.EnsureReport{}, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil || m["node"] != o.NodeName || m["profile"] != p.Name {
		return nil, hwemu.EnsureReport{}, fmt.Errorf("%w: %s", hwemu.ErrProfileChanged, data)
	}
	var rep hwemu.EnsureReport
	if _, err := os.Stat(tree.SysRoot); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(tree.SysRoot, 0o755); err != nil {
			return nil, hwemu.EnsureReport{}, err
		}
		rep.Added = []string{layout.SysDir}
	}
	return tree, rep, nil
}

func (f *emuFakes) newRunner(p *hwemu.Profile, t *hwemu.Tree, o hwemu.Options) (Runner, error) {
	r := &emuFakeRunner{opts: o, cpuW: 100, memW: 10, otherW: 50, gpuW: 200}
	if p.GPUs != nil {
		r.gpus = p.GPUs.Count.V
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setup != nil {
		f.setup(t.NodeName, r)
	}
	f.runners[t.NodeName] = r
	return r, nil
}

func (f *emuFakes) runner(t *testing.T, node string) *emuFakeRunner {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runners[node]
	if r == nil {
		t.Fatalf("no runner for node %s", node)
	}
	return r
}

// emuClock is the clock and the ticker of Run in a test. Time passes only
// through advance, which sends a beat for every multiple of the interval
// since the ticker started that it reaches, dropping a beat while one is
// pending, as a time.Ticker does for a slow receiver.
type emuClock struct {
	mu       sync.Mutex
	t, t0    time.Time
	interval time.Duration
	beats    chan time.Time
	reads    int
}

// now reads the clock. Run reads it twice a tick, when the tick starts and
// when it ends, publish included, and then waits for the next beat; so the
// second read of each pair also lets time pass until that beat, after the
// time it returns, outside the tick's duration.
func (c *emuClock) now() time.Time {
	c.mu.Lock()
	t := c.t
	c.reads++
	ends := c.reads%2 == 0
	c.mu.Unlock()
	if ends {
		c.waitBeat()
	}
	return t
}

func (c *emuClock) ticker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t0, c.interval, c.beats = c.t, d, make(chan time.Time, 1)
	return c.beats, func() {}
}

func (c *emuClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	end := c.t.Add(d)
	if c.beats != nil {
		for b := c.t0.Add((c.t.Sub(c.t0)/c.interval + 1) * c.interval); !b.After(end); b = b.Add(c.interval) {
			select {
			case c.beats <- b:
			default:
			}
		}
	}
	c.t = end
}

// waitBeat lets time pass until the next beat, unless one is pending: what
// Run waits for between two ticks.
func (c *emuClock) waitBeat() {
	c.mu.Lock()
	if len(c.beats) > 0 {
		c.mu.Unlock()
		return
	}
	next := c.t0.Add((c.t.Sub(c.t0)/c.interval + 1) * c.interval)
	d := next.Sub(c.t)
	c.mu.Unlock()
	c.advance(d)
}

// emuLog collects the emulator's log lines.
type emuLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *emuLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *emuLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// emuMember is a member with a profile of sockets CPU packages and gpus
// GPUs.
func emuMember(name, rack, zone string, sockets, gpus int) clusterspec.Member {
	p := &hwemu.Profile{Name: "fake-" + name}
	p.CPU.Sockets.V = sockets
	if gpus > 0 {
		p.GPUs = &hwemu.GPUSpec{Vendor: "nvidia"}
		p.GPUs.Count.V = gpus
	}
	return clusterspec.Member{Name: name, Pool: "pool-" + name, Rack: rack, CoolingZone: zone, Profile: p}
}

// emuConfig is a Config over fakes, starting at emuStart.
func emuConfig(root string, f *emuFakes, log *emuLog, members ...clusterspec.Member) Config {
	return Config{
		Root:      root,
		Cluster:   "test",
		Members:   members,
		Start:     emuStart,
		Ensure:    f.ensure,
		NewRunner: f.newRunner,
		Logf:      log.logf,
	}
}

func emuNew(t *testing.T, c Config) *Emulator {
	t.Helper()
	e, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func emuTick(t *testing.T, e *Emulator, at time.Duration, loads map[string]hwemu.Load, applyBase bool, fac FacilityFunc) *Snapshot {
	t.Helper()
	snap, err := e.Tick(emuStart.Add(at), loads, applyBase, fac)
	if err != nil {
		t.Fatalf("Tick at +%v: %v", at, err)
	}
	return snap
}

func emuNode(t *testing.T, s *Snapshot, name string) NodeSnapshot {
	t.Helper()
	for _, n := range s.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("snapshot %d has no node %s", s.Seq, name)
	return NodeSnapshot{}
}

// emuMetric reads one sample from reg: the counter, gauge or histogram
// sample count of family name with labels.
func emuMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if labels[lp.GetName()] != lp.GetValue() {
					continue metrics
				}
			}
			switch {
			case m.Counter != nil:
				return m.GetCounter().GetValue()
			case m.Gauge != nil:
				return m.GetGauge().GetValue()
			case m.Histogram != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
		t.Fatalf("%s has no sample with labels %v", name, labels)
	}
	t.Fatalf("%s is not registered", name)
	return 0
}

func emuNear(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %.12g, want %.12g", what, got, want)
	}
}

// EM01: New builds <root>/<node> for every member with the default Ensure
// and NewRunner, hwemu's, and the first snapshot of each node shows Started
// and Fresh; the next one shows neither. Every snapshot's host power is the
// sum of its components' plus OtherW.
func TestEM01NewBuildsATreePerMember(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var members []clusterspec.Member
	for _, n := range []struct{ name, profile string }{{"intel-0", "intel-xeon-2s-rapl"}, {"vm-0", "vm-no-powercap"}} {
		p := profiles[n.profile]
		if p == nil {
			t.Fatalf("builtin profile %s is missing", n.profile)
		}
		members = append(members, clusterspec.Member{Name: n.name, Pool: n.profile, Profile: p})
	}
	root := filepath.Join(t.TempDir(), "hwemu")
	log := &emuLog{}
	e := emuNew(t, Config{Root: root, Cluster: "test", Members: members, Start: emuStart, Logf: log.logf})

	for _, m := range members {
		if _, err := os.Stat(filepath.Join(root, m.Name, layout.StateDir, "hwemu.json")); err != nil {
			t.Errorf("node %s: no tree: %v", m.Name, err)
		}
		if !e.Has(m.Name) {
			t.Errorf("Has(%s) = false", m.Name)
		}
		if got, ok := e.Member(m.Name); !ok || got.Profile != m.Profile {
			t.Errorf("Member(%s) = %v, %v; want the member", m.Name, got.Name, ok)
		}
		if r, ok := e.RunnerFor(m.Name); !ok {
			t.Errorf("RunnerFor(%s): none", m.Name)
		} else if _, isNode := r.(*hwemu.Node); !isNode {
			t.Errorf("RunnerFor(%s) is %T, want *hwemu.Node", m.Name, r)
		}
	}
	if e.Has("absent") {
		t.Error(`Has("absent") = true`)
	}

	first := emuTick(t, e, time.Second, nil, true, nil)
	for _, m := range members {
		n := emuNode(t, first, m.Name)
		if !n.Started || !n.Fresh || n.Err != "" {
			t.Errorf("node %s first snapshot: Started %v, Fresh %v, Err %q; want Started and Fresh", m.Name, n.Started, n.Fresh, n.Err)
		}
		if n.Root != filepath.Join(root, m.Name) || n.Profile != m.Profile.Name || n.Pool != m.Pool || !n.StartTime.Equal(emuStart) {
			t.Errorf("node %s: root %s, profile %s, pool %s, start %v", m.Name, n.Root, n.Profile, n.Pool, n.StartTime)
		}
		if _, err := os.Stat(filepath.Join(root, m.Name, layout.StateDir, LedgerFile)); err != nil {
			t.Errorf("node %s: no ledger after a tick: %v", m.Name, err)
		}
	}
	second := emuTick(t, e, 2*time.Second, nil, true, nil)
	for _, m := range members {
		if n := emuNode(t, second, m.Name); n.Started || n.Fresh {
			t.Errorf("node %s second snapshot: Started %v, Fresh %v; want neither", m.Name, n.Started, n.Fresh)
		}
	}
	for _, s := range []*Snapshot{first, second} {
		for _, n := range s.Nodes {
			sum := n.Host.OtherW
			for _, c := range n.Components {
				sum += c.PowerW
			}
			emuNear(t, fmt.Sprintf("node %s host power at seq %d", n.Name, s.Seq), n.Host.PowerW, sum)
		}
	}
}

// EM05: Latest returns the snapshot the last Tick published, the same
// pointer however often it is called, and steps no node.
func TestEM05LatestOnlyReads(t *testing.T) {
	f := newEmuFakes()
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0)))
	if e.Latest() != nil {
		t.Fatal("Latest before the first tick is not nil")
	}
	snap := emuTick(t, e, time.Second, nil, true, nil)
	r := f.runner(t, "a")
	before := len(r.advanced())
	for i := 0; i < 100; i++ {
		if got := e.Latest(); got != snap {
			t.Fatalf("Latest call %d returned %p, want the published %p", i, got, snap)
		}
	}
	if after := len(r.advanced()); after != before {
		t.Errorf("Latest stepped the node: %d AdvanceTo calls, then %d", before, after)
	}
	next := emuTick(t, e, 2*time.Second, nil, true, nil)
	if e.Latest() != next || next == snap {
		t.Error("Latest does not return the snapshot of the next tick")
	}
}

// EM06: a root held by one emulator cannot be taken by a second, in the
// same process or another; Close releases it.
func TestEM06SecondEmulatorOnALockedRootFails(t *testing.T) {
	root := t.TempDir()
	f := newEmuFakes()
	c := emuConfig(root, f, &emuLog{}, emuMember("a", "", "", 1, 0))
	first, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(c)
	if want := "another simulator holds " + root; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("second New: %v, want an error containing %q", err, want)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(c)
	if err != nil {
		t.Fatalf("New after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if _, err := second.Tick(emuStart.Add(time.Second), nil, true, nil); err == nil {
		t.Error("Tick after Close succeeded")
	}
}

// EM07: fakesmi is copied to <root>/lib/fakesmi with mode 0755 through a
// rename, is left alone (inode and mtime) when its content is unchanged,
// and is replaced by a new file, never written over, when it changed. No
// temp file stays behind.
func TestEM07FakeSMIInstalledByRename(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "fakesmi")
	if err := os.WriteFile(src, []byte("fakesmi v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, LibDir, FakeSMIName)
	start := func() {
		t.Helper()
		c := emuConfig(root, newEmuFakes(), &emuLog{}, emuMember("a", "", "", 1, 0))
		c.FakeSMISource = src
		e, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	stat := func() os.FileInfo {
		t.Helper()
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	content := func() string {
		t.Helper()
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	start()
	first := stat()
	if first.Mode().Perm() != 0o755 || content() != "fakesmi v1\n" {
		t.Fatalf("installed fakesmi: mode %v, content %q", first.Mode().Perm(), content())
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatal(err)
	}

	start()
	same := stat()
	if !os.SameFile(first, same) || !same.ModTime().Equal(old) {
		t.Errorf("an unchanged fakesmi was rewritten: same inode %v, mtime %v (want %v)", os.SameFile(first, same), same.ModTime(), old)
	}

	if err := os.WriteFile(src, []byte("fakesmi v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start()
	changed := stat()
	if os.SameFile(same, changed) {
		t.Error("a changed fakesmi was written over the old file, not renamed into place")
	}
	if changed.Mode().Perm() != 0o755 || content() != "fakesmi v2\n" {
		t.Errorf("updated fakesmi: mode %v, content %q", changed.Mode().Perm(), content())
	}
	entries, err := os.ReadDir(filepath.Join(root, LibDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("lib/ holds %v, want only %s", names, FakeSMIName)
	}

	// An empty source skips the copy; a missing one fails New.
	c := emuConfig(t.TempDir(), newEmuFakes(), &emuLog{}, emuMember("a", "", "", 1, 0))
	e := emuNew(t, c)
	if _, err := os.Stat(filepath.Join(c.Root, LibDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an empty FakeSMISource created lib/: %v", err)
	}
	_ = e.Close()
	c.FakeSMISource = filepath.Join(t.TempDir(), "missing")
	if _, err := New(c); err == nil || !strings.Contains(err.Error(), "fakesmi") {
		t.Errorf("New with a missing fakesmi source: %v, want an error naming fakesmi", err)
	}
}

// EM10: a tick that takes longer than the interval counts as an overrun,
// publish included, logged at most once a minute, and the tick after it
// starts at once and advances to the wall time it starts at, not to the
// slow tick's time plus one interval. Run's clock and ticker are a fake, so
// the timing is exact however loaded the machine is; the wait for the next
// beat is outside each tick, so a fast tick takes no time.
func TestEM10SlowTickIsAnOverrun(t *testing.T) {
	const tick = time.Second
	clock := &emuClock{t: emuStart}
	f := newEmuFakes()
	// Ticks 2 and 3 are slow to step, 4 is slow to publish, 5 is slow to
	// step a minute after the first overrun.
	takes := []time.Duration{0, 3500 * time.Millisecond, 3500 * time.Millisecond, 0, 61 * time.Second, 0}
	f.setup = func(_ string, r *emuFakeRunner) { r.takes, r.clock = takes, clock }
	log := &emuLog{}
	reg := prometheus.NewRegistry()
	c := emuConfig(t.TempDir(), f, log, emuMember("a", "", "", 1, 0))
	c.Tick, c.Registerer = tick, reg
	e := emuNew(t, c)
	e.now, e.newTicker = clock.now, clock.ticker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var published []*Snapshot
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx, nil, nil, func(s *Snapshot) {
			published = append(published, s)
			switch len(published) {
			case 4:
				clock.advance(1500 * time.Millisecond)
			case len(takes):
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}

	if got := emuMetric(t, reg, "joulie_sim_emulate_tick_overruns_total", nil); got != 4 {
		t.Errorf("overruns = %v, want 4, the slow ticks, the one slow to publish included", got)
	}
	if got := log.count("longer than the"); got != 2 {
		t.Errorf("%d overrun log lines, want 2: one for ticks 2 to 4, one for tick 5 a minute later", got)
	}
	// Tick 3 starts when tick 2 ends, at +4.5s, not at +2s; tick 4 when
	// tick 3 ends; tick 5 when tick 4 has published, at +9.5s; tick 6 when
	// tick 5 ends.
	want := []time.Duration{0, time.Second, 4500 * time.Millisecond, 8 * time.Second, 9500 * time.Millisecond, 70500 * time.Millisecond}
	adv := f.runner(t, "a").advanced()
	if len(adv) != len(want) || len(published) != len(want) {
		t.Fatalf("%d advances, %d snapshots; want %d of each", len(adv), len(published), len(want))
	}
	for i, s := range published {
		if !adv[i].Equal(emuStart.Add(want[i])) || s.Seq != uint64(i+1) || !s.Time.Equal(adv[i]) {
			t.Errorf("tick %d: advanced to +%v, snapshot Seq %d at +%v; want both at +%v, Seq %d",
				i+1, adv[i].Sub(emuStart), s.Seq, s.Time.Sub(emuStart), want[i], i+1)
		}
	}
	if got := emuMetric(t, reg, "joulie_sim_emulate_tick_duration_seconds", nil); got != float64(len(want)) {
		t.Errorf("tick duration observations = %v, want %d", got, len(want))
	}
	if got := emuMetric(t, reg, "joulie_sim_emulate_nodes", nil); got != 1 {
		t.Errorf("joulie_sim_emulate_nodes = %v, want 1", got)
	}
}

// EM12: a RenderCheckInterval of 0 reaches the runner as 10 ticks, 20 s for
// a 2 s tick; a set interval, the substep and the start reach it as given.
func TestEM12RenderCheckIntervalDefaultsToTenTicks(t *testing.T) {
	for _, tc := range []struct{ set, want time.Duration }{{0, 20 * time.Second}, {5 * time.Second, 5 * time.Second}} {
		f := newEmuFakes()
		c := emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0))
		c.Tick, c.RenderCheckInterval, c.Substep = 2*time.Second, tc.set, 50*time.Millisecond
		e := emuNew(t, c)
		o := f.runner(t, "a").opts
		if o.RenderCheckInterval != tc.want || o.Substep != 50*time.Millisecond || !o.Start.Equal(emuStart) {
			t.Errorf("RenderCheckInterval %v: the runner got %+v, want a check interval of %v", tc.set, o, tc.want)
		}
		_ = e.Close()
	}
}

// EM14: Workers 0 or less steps min(GOMAXPROCS, 12) nodes at once, so a
// large host does not run more workers than the file system calls of a step
// gain from; a set Workers is kept, above the cap too.
func TestEM14WorkersDefaultToGOMAXPROCSCapped(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	for _, tc := range []struct{ procs, set, want int }{
		{2, 0, 2},
		{12, 0, 12},
		{28, 0, 12},
		{28, -1, 12},
		{4, 20, 20},
	} {
		runtime.GOMAXPROCS(tc.procs)
		f := newEmuFakes()
		c := emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0))
		c.Workers = tc.set
		e := emuNew(t, c)
		if e.workers != tc.want {
			t.Errorf("GOMAXPROCS %d, Workers %d: %d workers, want %d", tc.procs, tc.set, e.workers, tc.want)
		}
		_ = e.Close()
	}
}

// Ensure renders each node under its own name with a seed of its own,
// never 0, so nodes start their raw counters at different places, and the
// same node at the same place every time.
func TestEnsureGetsTheNodeNameAndItsSeed(t *testing.T) {
	f := newEmuFakes()
	emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0), emuMember("b", "", "", 1, 0)))
	a, b := f.render["a"], f.render["b"]
	if a.NodeName != "a" || b.NodeName != "b" {
		t.Errorf("node names %q, %q; want a, b", a.NodeName, b.NodeName)
	}
	if a.Seed == 0 || b.Seed == 0 || a.Seed == b.Seed || a.Seed != emulatorSeed("a") || b.Seed != emulatorSeed("b") {
		t.Errorf("seeds %d, %d; want two different, non-zero seeds by node name", a.Seed, b.Seed)
	}
}

// Members are checked before anything is written: a name that is not one
// directory, the lib directory's name, a duplicate and a missing profile
// each fail, all reported at once.
func TestNewRejectsMembersThatCannotHaveADirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	bad := []clusterspec.Member{emuMember("a", "", "", 1, 0), emuMember("a", "", "", 1, 0), emuMember(LibDir, "", "", 1, 0), emuMember("x/y", "", "", 1, 0), {Name: "noprofile"}}
	_, err := New(emuConfig(root, newEmuFakes(), &emuLog{}, bad...))
	if err == nil {
		t.Fatal("New accepted bad members")
	}
	for _, want := range []string{"listed twice", "lib directory", `"x/y"`, "noprofile has no profile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("New with bad members created the root: %v", err)
	}
}
