package emulate

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// Files and directories the emulator keeps under its root, next to one
// directory per node. LedgerFile is in each node's state/ directory.
const (
	LedgerFile         = "ledger.json"
	FacilityLedgerFile = "facility-ledger.json"
	LockFile           = ".emulator.lock"
	LibDir             = "lib"
	FakeSMIName        = "fakesmi"
)

// Defaults of Config.
const (
	defaultTick = time.Second
	// renderCheckTicks is how many ticks apart render-owned files are
	// checked when Config.RenderCheckInterval is 0.
	renderCheckTicks = 10
	// maxDefaultWorkers caps the workers of a Config with Workers 0. A step
	// is mostly system calls on the node's files, and past about 12 workers
	// they contend in the kernel and the Go runtime: BenchmarkEM01Tick
	// intel-200 on a 28-thread host took 205 ms with 8 or 12 workers, 240 ms
	// with 16 and 290 ms with 28, on disk and on tmpfs alike.
	maxDefaultWorkers = 12
)

// Facility defaults for a Tick without a FacilityFunc: the simulator's
// SIM_FACILITY_AMBIENT_TEMP_C, SIM_FACILITY_TEMP_AMPLITUDE_C and
// SIM_FACILITY_TEMP_PERIOD_H defaults.
const (
	defaultAmbientC   = 20.0
	defaultAmplitudeC = 5.0
	defaultPeriodH    = 24.0
)

// errClosed is the error of a Tick after Close.
var errClosed = errors.New("emulate: the emulator is closed")

// Runner steps one node. *hwemu.Node is the Runner of a real emulator; tests
// use fakes. The emulator calls a Runner only from its own goroutine and its
// workers, never two calls at once for one node.
type Runner interface {
	SetLoad(hwemu.Load)
	AdvanceTo(time.Time) error
	State() hwemu.State
}

// EnsureFunc makes a root hold a node's tree, as hwemu.Ensure does.
type EnsureFunc func(*hwemu.Profile, string, hwemu.RenderOptions) (*hwemu.Tree, hwemu.EnsureReport, error)

// Config configures an Emulator.
//
// Root holds one directory per member, named after it, plus LibDir,
// LockFile and FacilityLedgerFile. Cluster is the spec name. Facility gives
// the cooling zones; its temperatures are read only when a Tick has no
// FacilityFunc.
//
// Tick is the interval of Run, 1 s when 0. Substep is the physics
// substep of each node (hwemu's default when 0). RenderCheckInterval is how
// often a node checks its render-owned files, 10 x Tick when 0. Workers is
// how many nodes step at once; 0 or less means GOMAXPROCS, at most 12
// (maxDefaultWorkers). FakeSMISource is the fakesmi binary copied to
// <Root>/LibDir/FakeSMIName; empty skips the copy. Start is when the nodes'
// clocks start, time.Now() when zero.
//
// Ensure and NewRunner default to hwemu.Ensure and hwemu.NewNode. Registerer
// receives the emulator's metrics when not nil. Logf defaults to log.Printf.
type Config struct {
	Root, Cluster       string
	Members             []clusterspec.Member
	Facility            clusterspec.Facility
	Tick                time.Duration
	Substep             time.Duration
	RenderCheckInterval time.Duration
	Workers             int
	FakeSMISource       string
	Start               time.Time
	Ensure              EnsureFunc
	NewRunner           func(*hwemu.Profile, *hwemu.Tree, hwemu.Options) (Runner, error)
	Registerer          prometheus.Registerer
	Logf                func(string, ...any)
}

// FacilityFunc returns the facility at now with specITPowerW of IT power
// from the emulated hosts. ITPowerW of the result is the facility's whole IT
// power: the simulator's FacilityFunc adds its legacy nodes to it.
type FacilityFunc func(specITPowerW float64, now time.Time) facility.Values

// LoadFunc returns the job load of each node for the next tick, by node
// name, and whether each member's BaseLoad is added to it. applyBase is
// false while a workload run drives the nodes (X9).
type LoadFunc func() (loads map[string]hwemu.Load, applyBase bool)

// Emulator steps the emulated nodes of a cluster spec on one tick and
// publishes a Snapshot after each. Tick and Run may be called from one
// goroutine at a time; Latest, Has, Member and RunnerFor from any, but a
// Runner that RunnerFor returns only from the goroutine that ticks.
type Emulator struct {
	root      string
	cluster   string
	tick      time.Duration
	workers   int
	start     time.Time
	zones     []clusterspec.CoolingZone
	facParams facility.Params
	logf      func(string, ...any)
	unlock    func() error
	metrics   *emulatorMetrics

	// now and newTicker are the clock and the ticker of Run: time.Now and
	// time.NewTicker, which tests replace to time Run exactly.
	now       func() time.Time
	newTicker func(time.Duration) (beats <-chan time.Time, stop func())

	members []*member
	byName  map[string]*member

	latest atomic.Pointer[Snapshot]

	// Owned by Tick, under tickMu.
	tickMu       sync.Mutex
	closed       bool
	seq          uint64
	lastTick     time.Time
	peakITPowerW float64
	facLedger    facilityLedger // accumulated
	facPublished facilityLedger // as last persisted, what snapshots show
	facErr       string
}

// member is one emulated node and what the emulator keeps for it between
// ticks. Each tick, one worker owns it.
type member struct {
	spec       clusterspec.Member
	runner     Runner
	report     hwemu.EnsureReport
	sockets    int
	gpus       int
	ledgerPath string
	tmpDir     string

	ledger    nodeLedger         // accumulated
	hostBase  float64            // runner host counter at the last tick, 0 at New
	compBase  map[string]float64 // runner component counters at the last tick
	lastLoad  *hwemu.Load        // the load last passed to SetLoad
	published bool               // a snapshot of the node was published
	last      NodeSnapshot       // readings of the last good step
	lastErr   string

	// Results of the current tick.
	snap      NodeSnapshot
	hostDelta float64 // increase of the published host energy
	err       error
}

// New takes the lock on c.Root, installs fakesmi, makes each member's tree
// exist with c.Ensure and starts a Runner over it, and loads the ledgers.
// Members step in name order. Every member's failure is reported, joined;
// any failure releases the lock and returns no Emulator.
func New(c Config) (*Emulator, error) {
	if c.Root == "" {
		return nil, errors.New("emulate: Config.Root is empty")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return nil, fmt.Errorf("emulate: %w", err)
	}
	if c.Tick <= 0 {
		c.Tick = defaultTick
	}
	if c.RenderCheckInterval == 0 {
		c.RenderCheckInterval = renderCheckTicks * c.Tick
	}
	if c.Workers <= 0 {
		c.Workers = min(runtime.GOMAXPROCS(0), maxDefaultWorkers)
	}
	if c.Start.IsZero() {
		c.Start = time.Now()
	}
	if c.Ensure == nil {
		c.Ensure = hwemu.Ensure
	}
	if c.NewRunner == nil {
		c.NewRunner = newHwemuRunner
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	members, err := emulatorMembers(c.Members)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("emulate: %w", err)
	}
	unlock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}
	e := &Emulator{
		root:      root,
		cluster:   c.Cluster,
		tick:      c.Tick,
		workers:   c.Workers,
		start:     c.Start,
		logf:      c.Logf,
		unlock:    unlock,
		members:   members,
		byName:    make(map[string]*member, len(members)),
		lastTick:  c.Start,
		now:       time.Now,
		newTicker: emulatorTicker,
		facParams: facility.Params{
			BaseAmbientC: emulatorOr(c.Facility.AmbientC, defaultAmbientC),
			AmplitudeC:   emulatorOr(c.Facility.AmplitudeC, defaultAmplitudeC),
			PeriodH:      emulatorOr(c.Facility.PeriodH, defaultPeriodH),
		},
	}
	fail := func(err error) (*Emulator, error) {
		_ = unlock()
		return nil, err
	}
	if installed, err := installFakeSMI(root, c.FakeSMISource); err != nil {
		return fail(err)
	} else if installed {
		e.logf("emulate: installed %s from %s", filepath.Join(root, LibDir, FakeSMIName), c.FakeSMISource)
	}

	opts := hwemu.Options{Substep: c.Substep, Start: c.Start, RenderCheckInterval: c.RenderCheckInterval}
	errs := make([]error, len(members))
	e.parallel(len(members), func(i int) {
		errs[i] = e.startMember(members[i], c, opts)
	})
	var failed []error
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Errorf("emulate: node %s: %w", members[i].spec.Name, err))
		}
	}
	if len(failed) > 0 {
		return fail(errors.Join(failed...))
	}

	e.zones = emulatorZones(c.Facility.CoolingZones, members)
	if err := ledgerRemoveTemps(root, facilityLedgerTmpPrefix); err != nil {
		return fail(fmt.Errorf("emulate: %w", err))
	}
	fl, err := loadFacilityLedger(filepath.Join(root, FacilityLedgerFile), c.Cluster, c.Start, e.logf)
	if err != nil {
		return fail(fmt.Errorf("emulate: %w", err))
	}
	e.facLedger, e.facPublished = fl, fl.clone()

	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.spec.Name
		e.byName[m.spec.Name] = m
	}
	if e.metrics, err = newEmulatorMetrics(c.Registerer, names); err != nil {
		return fail(err)
	}
	return e, nil
}

// emulatorMembers checks the members and returns them sorted by name. A
// member's name is its directory under the root, so it must be one path
// element that is neither hidden nor LibDir.
func emulatorMembers(in []clusterspec.Member) ([]*member, error) {
	var errs []error
	seen := map[string]bool{}
	out := make([]*member, 0, len(in))
	for _, m := range in {
		switch {
		case m.Name == "" || strings.ContainsAny(m.Name, `/\`) || strings.HasPrefix(m.Name, "."):
			errs = append(errs, fmt.Errorf("emulate: member name %q is not a directory name", m.Name))
		case m.Name == LibDir:
			errs = append(errs, fmt.Errorf("emulate: member name %q is the emulator's %s directory", m.Name, LibDir))
		case seen[m.Name]:
			errs = append(errs, fmt.Errorf("emulate: member %s is listed twice", m.Name))
		case m.Profile == nil:
			errs = append(errs, fmt.Errorf("emulate: member %s has no profile", m.Name))
		default:
			seen[m.Name] = true
			mb := &member{spec: m, sockets: m.Profile.CPU.Sockets.V}
			if m.Profile.GPUs != nil {
				mb.gpus = m.Profile.GPUs.Count.V
			}
			out = append(out, mb)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].spec.Name < out[j].spec.Name })
	return out, nil
}

// emulatorZones lists the declared cooling zones, plus any zone a member
// names that the facility does not declare, at the member's offset.
func emulatorZones(declared []clusterspec.CoolingZone, members []*member) []clusterspec.CoolingZone {
	seen := map[string]bool{}
	var zones []clusterspec.CoolingZone
	for _, z := range declared {
		if !seen[z.Name] {
			seen[z.Name] = true
			zones = append(zones, z)
		}
	}
	for _, m := range members {
		if z := m.spec.CoolingZone; z != "" && !seen[z] {
			seen[z] = true
			zones = append(zones, clusterspec.CoolingZone{Name: z, OffsetC: m.spec.ZoneOffsetC})
		}
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return zones
}

// startMember makes the member's tree exist, starts its Runner and loads its
// ledger.
func (e *Emulator) startMember(m *member, c Config, opts hwemu.Options) error {
	name := m.spec.Name
	tree, rep, err := c.Ensure(m.spec.Profile, filepath.Join(e.root, name), hwemu.RenderOptions{NodeName: name, Seed: emulatorSeed(name)})
	if err != nil {
		return err
	}
	if tree == nil {
		return errors.New("ensure returned no tree")
	}
	runner, err := c.NewRunner(m.spec.Profile, tree, opts)
	if err != nil {
		return err
	}
	stateRoot := tree.StateRoot
	if stateRoot == "" {
		stateRoot = filepath.Join(tree.Root, layout.StateDir)
	}
	m.runner, m.report = runner, rep
	m.report.Added = append([]string(nil), rep.Added...)
	m.ledgerPath = filepath.Join(stateRoot, LedgerFile)
	m.tmpDir = filepath.Join(stateRoot, layout.TmpDir)
	if err := os.MkdirAll(m.tmpDir, 0o755); err != nil {
		return err
	}
	if err := ledgerRemoveTemps(m.tmpDir, ledgerTmpPrefix); err != nil {
		return err
	}
	if m.ledger, err = loadNodeLedger(m.ledgerPath, name, c.Start); err != nil {
		return err
	}
	m.compBase = map[string]float64{}
	m.last = NodeSnapshot{
		Name:        name,
		Pool:        m.spec.Pool,
		Profile:     m.spec.Profile.Name,
		Rack:        m.spec.Rack,
		CoolingZone: m.spec.CoolingZone,
		Root:        tree.Root,
		StartTime:   m.ledger.StartTime,
		Host:        HostReading{EnergyJ: m.ledger.HostJ},
		Components:  []ComponentReading{},
	}
	return nil
}

// emulatorSeed seeds the energy counters a fresh tree starts from, from the
// node name: each node starts somewhere else in its counters' range, as
// machines that booted at different times do, and the same node always
// starts at the same place.
func emulatorSeed(node string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(node))
	if s := h.Sum64(); s != 0 {
		return s
	}
	return 1
}

func emulatorOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func emulatorTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

func newHwemuRunner(p *hwemu.Profile, t *hwemu.Tree, o hwemu.Options) (Runner, error) {
	n, err := hwemu.NewNode(p, t, o)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// parallel runs f(0) to f(n-1) on at most e.workers goroutines and waits.
func (e *Emulator) parallel(n int, f func(int)) {
	workers := e.workers
	if workers > n {
		workers = n
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := int(next.Add(1)) - 1; i < n; i = int(next.Add(1)) - 1 {
				f(i)
			}
		}()
	}
	wg.Wait()
}

// Close releases the root and unregisters the metrics. The trees and
// ledgers stay on disk for the next New. A Tick after Close fails; Latest
// still returns the last snapshot.
func (e *Emulator) Close() error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.metrics.unregister()
	return e.unlock()
}

// Has reports whether node is a member.
func (e *Emulator) Has(node string) bool {
	_, ok := e.byName[node]
	return ok
}

// Member returns the member named node.
func (e *Emulator) Member(node string) (clusterspec.Member, bool) {
	m, ok := e.byName[node]
	if !ok {
		return clusterspec.Member{}, false
	}
	return m.spec, true
}

// RunnerFor returns the Runner of node. It is not safe for concurrent use:
// call it only from the goroutine that ticks, such as inside the publish
// callback of Run.
func (e *Emulator) RunnerFor(node string) (Runner, bool) {
	m, ok := e.byName[node]
	if !ok {
		return nil, false
	}
	return m.runner, true
}

// Latest returns the last published snapshot, or nil before the first
// tick. It only reads: no node is stepped.
func (e *Emulator) Latest() *Snapshot {
	return e.latest.Load()
}

// Run ticks at once, then on every beat of a time.Ticker of Config.Tick,
// until ctx is done or the emulator is closed. Each tick advances to the
// wall time it starts at, takes the loads from loads (none, with base load,
// when nil) and passes the snapshot to publish, on the same goroutine, after
// the tick. A tick that takes longer than the interval is an overrun: it is
// counted, logged at most once a minute, and the next tick advances to the
// wall time again rather than catching up.
func (e *Emulator) Run(ctx context.Context, loads LoadFunc, fac FacilityFunc, publish func(*Snapshot)) {
	beats, stop := e.newTicker(e.tick)
	defer stop()
	var lastOverrunLog time.Time
	for ctx.Err() == nil {
		begin := e.now()
		var l map[string]hwemu.Load
		applyBase := true
		if loads != nil {
			l, applyBase = loads()
		}
		snap, err := e.Tick(begin, l, applyBase, fac)
		if errors.Is(err, errClosed) {
			return
		}
		if snap != nil && publish != nil {
			publish(snap)
		}
		if end := e.now(); end.Sub(begin) > e.tick {
			e.metrics.overruns.Inc()
			if lastOverrunLog.IsZero() || end.Sub(lastOverrunLog) >= time.Minute {
				lastOverrunLog = end
				e.logf("emulate: a tick took %v, longer than the %v interval; the next tick advances to the wall time (logged at most once a minute)", end.Sub(begin).Round(time.Millisecond), e.tick)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-beats:
		}
	}
}
