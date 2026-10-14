package hwemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// nvclockFacts are neutral clock facts for tests: graphics 300 to 1500 MHz,
// sustaining 1400 under load, idle at 300; memory 300 to 1200 MHz, idle at
// 300, at P8 and with a run-time memory lock when lockable, or one memory
// clock of 1200 MHz at P0 without a memory lock, as on HBM parts.
func nvclockFacts(lockable bool) *NVClocksSpec {
	src := "assumed:test fixture"
	f := func(v int64) Fact[int64] { return Fact[int64]{V: v, Src: src} }
	c := &NVClocksSpec{}
	c.SM.MinMHz, c.SM.MaxMHz, c.SM.IdleMHz, c.SM.SustainedMaxMHz = f(300), f(1500), f(300), f(1400)
	c.Supported.SMMHz = Fact[[]int64]{V: []int64{1500, 1200, 900, 600, 300}, Src: src}
	if lockable {
		c.Mem.MinMHz, c.Mem.MaxMHz, c.Mem.IdleMHz = f(300), f(1200), f(300)
		c.Mem.RuntimeLock = Fact[bool]{V: true, Src: src}
		c.Supported.MemMHz = Fact[[]int64]{V: []int64{1200, 600, 300}, Src: src}
		c.IdlePState = Fact[string]{V: "P8", Src: src}
	} else {
		c.Mem.MinMHz, c.Mem.MaxMHz, c.Mem.IdleMHz = f(1200), f(1200), f(1200)
		c.Mem.RuntimeLock = Fact[bool]{V: false, Src: src}
		c.Supported.MemMHz = Fact[[]int64]{V: []int64{1200}, Src: src}
		c.IdlePState = Fact[string]{V: "P0", Src: src}
	}
	return c
}

// nvclockProfile is builtin profile name, cut small, with nvclockFacts.
func nvclockProfile(t testing.TB, name string, lockable bool) *Profile {
	t.Helper()
	p := ensureBuiltin(t, name)
	p.GPUs.Clocks = nvclockFacts(lockable)
	return p
}

// nvclockSMI runs the fake nvidia-smi on tree as the agent does and returns
// its output; a non-zero exit is the error.
func nvclockSMI(tree *Tree, args ...string) (string, error) {
	out, err := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}.Run(context.Background(), "nvidia-smi", args...)
	return string(out), err
}

func nvclockMustSMI(t testing.TB, tree *Tree, args ...string) string {
	t.Helper()
	out, err := nvclockSMI(tree, args...)
	if err != nil {
		t.Fatalf("nvidia-smi %s: %v (%q)", strings.Join(args, " "), err, out)
	}
	return out
}

// nvclockQuery is the query line of GPU i for fields, without units.
func nvclockQuery(t testing.TB, tree *Tree, i int, fields string) string {
	t.Helper()
	return strings.TrimSpace(nvclockMustSMI(t, tree, "-i", strconv.Itoa(i), "--query-gpu="+fields, "--format=csv,noheader,nounits"))
}

// nvclockBuiltinNames are the builtin NVIDIA profiles.
func nvclockBuiltinNames() []string {
	var out []string
	for _, name := range renderBuiltinNames {
		if strings.HasPrefix(name, "nvidia-") {
			out = append(out, name)
		}
	}
	return out
}

// NV03: under load clocks.sm is the lock maximum bounded by
// sm.sustainedMaxMHz, so a lock above it is clamped, and an unlocked GPU
// runs at the sustained maximum with its memory at the maximum; at idle a
// locked GPU sits at its lock's lower bound while lockFloorAtIdle holds,
// and at its idle clock when the profile says otherwise. State reports the
// same clocks in Hz.
func TestNV03LoadedClockIsTheLockBoundedBySustainedMax(t *testing.T) {
	t.Parallel()
	for _, floor := range []bool{true, false} {
		t.Run(fmt.Sprintf("lockFloorAtIdle=%v", floor), func(t *testing.T) {
			t.Parallel()
			p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
			if !floor {
				p.GPUs.Clocks.LockFloorAtIdle = &Fact[bool]{V: false, Src: "assumed:test fixture"}
			}
			tree := renderTestTree(t, p)
			n := nodeNewNode(t, p, tree)
			nvclockMustSMI(t, tree, "-i", "1", "-lgc", "600,1800")
			nvclockMustSMI(t, tree, "-i", "2", "-lgc", "600,1000")
			n.SetLoad(Load{GPUUtil: []float64{0.5, 0.5, 0.5, 0}})
			nodeStep(t, n, time.Second)
			for i, want := range []string{"1400, 1200, P0", "1400, 1200, P0", "1000, 1200, P0", "300, 300, P8"} {
				if got := nvclockQuery(t, tree, i, "clocks.sm,clocks.mem,pstate"); got != want {
					t.Errorf("loaded: gpu%d clocks.sm, clocks.mem, pstate = %q, want %q", i, got, want)
				}
			}
			if hz := stateComponent(t, n.State(), "gpu2").ClockHz; hz[semconv.ClockDomainGraphics] != 1000e6 || hz[semconv.ClockDomainMemory] != 1200e6 {
				t.Errorf("gpu2 ClockHz %v, want graphics 1000 MHz and memory 1200 MHz", hz)
			}

			n.SetLoad(Load{})
			nodeStep(t, n, time.Second)
			lockedIdle := "300, 300, P8"
			if floor {
				lockedIdle = "600, 300, P0"
			}
			for i, want := range []string{"300, 300, P8", lockedIdle, lockedIdle, "300, 300, P8"} {
				if got := nvclockQuery(t, tree, i, "clocks.sm,clocks.mem,pstate"); got != want {
					t.Errorf("idle: gpu%d clocks.sm, clocks.mem, pstate = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// NV03, a lock that reaches below the idle clock: a lock bounds the clock
// and its lower bound is a floor, so an idle GPU never drops below its idle
// clock for a lock whose minimum lies under it (the efficient preset locks
// from the lowest supported clock), and never rises above the lock's
// maximum. Only a minimum above the idle clock holds the idle GPU up, and
// only while lockFloorAtIdle holds.
func TestNV03LockBelowTheIdleClockDoesNotLowerIt(t *testing.T) {
	t.Parallel()
	for _, floor := range []bool{true, false} {
		t.Run(fmt.Sprintf("lockFloorAtIdle=%v", floor), func(t *testing.T) {
			t.Parallel()
			p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
			p.GPUs.Clocks.SM.IdleMHz = Fact[int64]{V: 600, Src: "assumed:test fixture"}
			if !floor {
				p.GPUs.Clocks.LockFloorAtIdle = &Fact[bool]{V: false, Src: "assumed:test fixture"}
			}
			tree := renderTestTree(t, p)
			n := nodeNewNode(t, p, tree)
			nvclockMustSMI(t, tree, "-i", "1", "-lgc", "300,1200")
			nvclockMustSMI(t, tree, "-i", "2", "-lgc", "300,450")
			nvclockMustSMI(t, tree, "-i", "3", "-lgc", "900,1200")
			nodeStep(t, n, time.Second)
			held := "600, P8"
			if floor {
				held = "900, P0"
			}
			for i, want := range []string{"600, P8", "600, P8", "450, P8", held} {
				if got := nvclockQuery(t, tree, i, "clocks.sm,pstate"); got != want {
					t.Errorf("idle gpu%d clocks.sm, pstate = %q, want %q", i, got, want)
				}
			}
			if hz := stateComponent(t, n.State(), "gpu1").ClockHz[semconv.ClockDomainGraphics]; hz != 600e6 {
				t.Errorf("gpu1 graphics ClockHz %v, want the idle clock 600 MHz", hz)
			}
		})
	}
}

// NV05: a GPU without a run-time memory lock (Hopper) refuses -lmc with
// exit 3 and nothing changes: the lock file keeps its token, no event, the
// clocks stay; a write past the tool is restored. Where the lock exists,
// -lmc <max>,<max> holds clocks.mem at the maximum on an idle GPU.
func TestNV05MemoryLockHoldsTheMemoryClockOnlyWhereItExists(t *testing.T) {
	t.Parallel()
	t.Run("hopper", func(t *testing.T) {
		t.Parallel()
		p := nvclockProfile(t, "nvidia-h100-sxm-8gpu", false)
		tree := renderTestTree(t, p)
		n := nodeNewNode(t, p, tree)
		nodeStep(t, n, time.Second)
		before := nvclockQuery(t, tree, 0, "clocks.sm,clocks.mem,pstate")
		out, err := nvclockSMI(tree, "-i", "0", "-lmc", "1200,1200")
		var ee *smi.ExitError
		if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(out, "not supported") {
			t.Fatalf("-lmc on Hopper: %v, %q; want exit 3, not supported", err, out)
		}
		lock := nodeNVMLFile(0, layout.NVMLLockedMemClocksFile)
		nodeStep(t, n, time.Second)
		if ev := n.State().Events; len(ev) != 0 {
			t.Errorf("a refused -lmc made events: %+v", ev)
		}
		if got := nodeRead(t, tree, lock); got != "[N/A]\n" {
			t.Errorf("memory lock %q after a refused -lmc, want [N/A]", got)
		}
		if got := nvclockQuery(t, tree, 0, "clocks.sm,clocks.mem,pstate"); got != before {
			t.Errorf("clocks %q after a refused -lmc, want %q", got, before)
		}
		// A root writer past the tool: the file is read-only.
		if err := os.Chmod(filepath.Join(tree.Root, filepath.FromSlash(lock)), 0o644); err != nil {
			t.Fatal(err)
		}
		nodeWrite(t, tree, lock, "1200,1200")
		nodeStep(t, n, time.Second)
		if got := nodeRead(t, tree, lock); got != "[N/A]\n" {
			t.Errorf("a write past the tool stays: %q", got)
		}
		if ev := nodeEvents(n, lock); len(ev) != 1 || ev[0].Kind != EventRestored {
			t.Errorf("events on the memory lock %+v, want one restored", ev)
		}
		if g := stateComponent(t, n.State(), "gpu0"); g.GovernorPreset != api.GPUGovernorPresetAuto {
			t.Errorf("preset %q, want auto", g.GovernorPreset)
		}
	})
	t.Run("gddr", func(t *testing.T) {
		t.Parallel()
		p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
		tree := renderTestTree(t, p)
		n := nodeNewNode(t, p, tree)
		nvclockMustSMI(t, tree, "-i", "1", "-lmc", "1200,1200")
		for k := 0; k < 3; k++ {
			nodeStep(t, n, time.Second)
			if got := nvclockQuery(t, tree, 1, "clocks.sm,clocks.mem,pstate"); got != "300, 1200, P0" {
				t.Fatalf("step %d: idle gpu1 with -lmc 1200,1200: %q, want 300, 1200, P0", k, got)
			}
		}
		if got := nvclockQuery(t, tree, 0, "clocks.mem"); got != "300" {
			t.Errorf("gpu0 without a lock: clocks.mem %q at idle, want 300", got)
		}
		if g := stateComponent(t, n.State(), "gpu1"); g.GovernorPreset != api.GPUGovernorPresetSustained {
			t.Errorf("gpu1 preset %q, want sustained", g.GovernorPreset)
		}
	})
}

// NV09: -q -d SUPPORTED_CLOCKS on a rendered builtin NVIDIA node lists the
// profile's memory clocks, each with the profile's graphics clocks up to
// that memory clock's ceiling (supported.smMaxByMem), all of them under a
// memory clock without one, in descending order.
func TestNV09SupportedClocksListsTheProfileClocks(t *testing.T) {
	t.Parallel()
	for _, name := range nvclockBuiltinNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ensureBuiltin(t, name)
			c := p.GPUs.Clocks
			if c == nil {
				t.Fatal("no gpus.clocks")
			}
			tree := renderTestTree(t, p)
			out := nvclockMustSMI(t, tree, "-q", "-d", "SUPPORTED_CLOCKS", "-i", "1")
			var mem []int64
			graphics := map[int64][]int64{}
			for _, line := range strings.Split(out, "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				v, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " MHz"), 10, 64)
				switch strings.TrimSpace(key) {
				case "Memory":
					if err != nil {
						t.Fatalf("line %q", line)
					}
					mem = append(mem, v)
				case "Graphics":
					if err != nil || len(mem) == 0 {
						t.Fatalf("line %q", line)
					}
					graphics[mem[len(mem)-1]] = append(graphics[mem[len(mem)-1]], v)
				}
			}
			desc := func(v []int64) []int64 {
				out := slices.Clone(v)
				slices.Sort(out)
				slices.Reverse(out)
				return out
			}
			if !slices.Equal(mem, desc(c.Supported.MemMHz.V)) {
				t.Errorf("memory clocks %v, want %v", mem, desc(c.Supported.MemMHz.V))
			}
			ceil := map[int64]int64{}
			for _, x := range c.Supported.SMMaxByMem {
				ceil[x.MemMHz.V] = x.SMMaxMHz.V
			}
			for _, m := range mem {
				var want []int64
				for _, g := range desc(c.Supported.SMMHz.V) {
					if top, ok := ceil[m]; !ok || g <= top {
						want = append(want, g)
					}
				}
				if !slices.Equal(graphics[m], want) {
					t.Errorf("graphics clocks under %d MHz: %v, want %v", m, graphics[m], want)
				}
			}
			if name == "nvidia-l40s-4gpu" && (len(graphics[405]) != 30 || graphics[405][0] != 645 || len(graphics[9001]) != len(c.Supported.SMMHz.V)) {
				t.Errorf("L40S lists %d graphics clocks from %v at 405 MHz memory and %d at 9001 MHz, want 30 from 645 and every one", len(graphics[405]), graphics[405][:1], len(graphics[9001]))
			}
			if got := nvclockQuery(t, tree, 1, "clocks.max.sm,clocks.max.mem"); got != fmt.Sprintf("%d, %d", c.SM.MaxMHz.V, c.Mem.MaxMHz.V) {
				t.Errorf("clocks.max.sm, clocks.max.mem = %q, want the profile's %d, %d", got, c.SM.MaxMHz.V, c.Mem.MaxMHz.V)
			}
		})
	}
}

// NV10: clocks_event_reasons.active shows GPU idle on an idle GPU, the SW
// power cap bit while the cap holds a busy GPU below its natural power,
// nothing on a busy GPU the cap does not bind, and the applications clocks
// setting bit while a graphics lock holds the clock below the sustained
// maximum (assumed for the lock).
func TestNV10EventReasonsShowTheBindingLimit(t *testing.T) {
	t.Parallel()
	p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
	tree := renderTestTree(t, p)
	n := nodeNewNode(t, p, tree)
	nvclockMustSMI(t, tree, "-i", "0", "-pl", strconv.FormatFloat(p.GPUs.Limits.MinW.V, 'f', -1, 64))
	nvclockMustSMI(t, tree, "-i", "2", "-lgc", "300,900")
	n.SetLoad(Load{GPUUtil: []float64{1, 1, 1, 0}})
	for k := 0; k < 3; k++ {
		nodeStep(t, n, time.Second)
	}
	for i, want := range []string{"0x0000000000000004", "0x0000000000000000", "0x0000000000000002", "0x0000000000000001"} {
		if got := nvclockQuery(t, tree, i, "clocks_event_reasons.active"); got != want {
			t.Errorf("gpu%d clocks_event_reasons.active = %s, want %s", i, got, want)
		}
	}
	n.SetLoad(Load{})
	nodeStep(t, n, time.Second)
	if got := nvclockQuery(t, tree, 0, "clocks_event_reasons.active"); got != "0x0000000000000001" {
		t.Errorf("idle gpu0 under its cap: %s, want GPU idle alone", got)
	}
}

// NV11: with persistence mode Disabled and no client, the locks survive the
// driver's deinit unless gpus.clocks.locksResetOnDeinit says otherwise
// (H35's analogue for the power limit). When they reset, each lock file
// reads none within the step, with a reset-persistence-off event naming the
// GPU and the old lock, and the clocks go back to idle; a GPU under load, or
// with persistence Enabled, keeps its locks.
func TestNV11LocksSurviveDeinitUnlessTheProfileResetsThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		reset       bool
		persistence string
	}{
		{"default", false, PersistenceDisabled},
		{"reset on deinit", true, PersistenceDisabled},
		{"reset but persistence on", true, PersistenceEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
			p.GPUs.PersistenceMode = Fact[string]{V: tc.persistence, Src: "assumed:test fixture"}
			if tc.reset {
				p.GPUs.Clocks.LocksResetOnDeinit = &Fact[bool]{V: true, Src: "assumed:test fixture"}
			}
			tree := renderTestTree(t, p)
			n := nodeNewNode(t, p, tree)
			for _, i := range []string{"0", "1"} {
				nvclockMustSMI(t, tree, "-i", i, "-lgc", "600,900")
				nvclockMustSMI(t, tree, "-i", i, "-lmc", "1200,1200")
			}
			n.SetLoad(Load{GPUUtil: []float64{0, 0.5}})
			nodeStep(t, n, time.Second)
			gpuLock, memLock := nodeNVMLFile(0, layout.NVMLLockedGPUClocksFile), nodeNVMLFile(0, layout.NVMLLockedMemClocksFile)
			deinit := tc.reset && tc.persistence == PersistenceDisabled
			if deinit {
				for file, old := range map[string]string{gpuLock: "600,900", memLock: "1200,1200"} {
					ev := nodeEvents(n, file)
					if len(ev) != 2 || ev[0].Kind != EventAccepted || ev[1].Kind != EventResetPersistenceOff {
						t.Fatalf("events on %s: %+v, want accepted then reset-persistence-off", file, ev)
					}
					if r := ev[1]; r.Component != "gpu0" || r.Written != old || r.Effective != "none" {
						t.Errorf("reset event %+v, want gpu0 from %s to none", r, old)
					}
				}
				if got := nvclockQuery(t, tree, 0, "clocks.sm,clocks.mem,pstate"); got != "300, 300, P8" {
					t.Errorf("gpu0 after the reset: %q, want its idle clocks", got)
				}
				if g := stateComponent(t, n.State(), "gpu0"); g.GovernorNative != api.GovernorStateUnlocked {
					t.Errorf("gpu0 native %q after the reset, want unlocked", g.GovernorNative)
				}
			}
			for k := 0; k < 2; k++ {
				nodeStep(t, n, time.Second)
				if ev := n.State().Events; len(ev) != 0 {
					t.Errorf("step %d: events %+v, want none", k, ev)
				}
			}
			want := map[string]string{gpuLock: "600,900\n", memLock: "1200,1200\n"}
			if deinit {
				want = map[string]string{gpuLock: "none\n", memLock: "none\n"}
			}
			want[nodeNVMLFile(1, layout.NVMLLockedGPUClocksFile)] = "600,900\n"
			for file, w := range want {
				if got := nodeRead(t, tree, file); got != w {
					t.Errorf("%s = %q, want %q", file, got, w)
				}
			}
		})
	}
}

// NV12: the layout golden of every builtin NVIDIA profile lists, for every
// GPU, the clock files with their owners: the current clocks, pstate and
// event reasons emulator-owned, the maximum and supported clocks
// render-owned, the graphics lock agent-owned, and the memory lock
// agent-owned where the profile has a run-time memory lock and render-owned
// (holding [N/A]) where it has not.
func TestNV12NVIDIALayoutGoldensListTheClockFiles(t *testing.T) {
	t.Parallel()
	for _, name := range nvclockBuiltinNames() {
		p := renderBuiltin(t, name)
		golden, err := os.ReadFile(filepath.Join("testdata", "layout", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		lines := map[string]bool{}
		for _, l := range strings.Split(string(golden), "\n") {
			lines[l] = true
		}
		memLock := "A file 0644"
		if !p.GPUs.Clocks.Mem.RuntimeLock.V {
			memLock = "R file 0444"
		}
		for i := 0; i < p.GPUs.Count.V; i++ {
			for file, kind := range map[string]string{
				nvclockSMFile:                  "S file 0444",
				nvclockMemFile:                 "S file 0444",
				nvclockPStateFile:              "S file 0444",
				nvclockReasonsFile:             "S file 0444",
				nvclockMaxSMFile:               "R file 0444",
				nvclockMaxMemFile:              "R file 0444",
				nvclockSupportedFile:           "R file 0444",
				layout.NVMLLockedGPUClocksFile: "A file 0644",
				layout.NVMLLockedMemClocksFile: memLock,
			} {
				if l := path.Join(layout.StateDir, layout.NVMLGPUDir(i), file) + " " + kind; !lines[l] {
					t.Errorf("%s golden lacks %q", name, l)
				}
			}
		}
	}
}

// The lock state maps to the preset each NVIDIA mapping writes: none is
// auto, a memory lock at the maximum sustained, the minimum to a ceiling
// efficient, both domains at the minimum low and at the maximum peak, and
// anything else no single preset. Where no memory lock exists (Hopper),
// low and peak need the graphics lock alone. A write that is not a lock is
// reverted, and a Node started over the tree keeps the locks.
func TestNVClockStateReportsTheLocksAsPresets(t *testing.T) {
	t.Parallel()
	type step struct {
		args           [][]string
		native, preset string
	}
	for _, tc := range []struct {
		name     string
		profile  string
		lockable bool
		steps    []step
	}{
		{"gddr", "nvidia-l40s-4gpu", true, []step{
			{nil, api.GovernorStateUnlocked, api.GPUGovernorPresetAuto},
			{[][]string{{"-lmc", "1200,1200"}}, "lmc=1200,1200", api.GPUGovernorPresetSustained},
			{[][]string{{"-rmc"}, {"-lgc", "300,900"}}, "lgc=300,900", api.GPUGovernorPresetEfficient},
			{[][]string{{"-lgc", "300,300"}, {"-lmc", "300,300"}}, "lgc=300,300 lmc=300,300", api.GPUGovernorPresetLow},
			{[][]string{{"-lgc", "1500,1500"}, {"-lmc", "1200,1200"}}, "lgc=1500,1500 lmc=1200,1200", api.GPUGovernorPresetPeak},
			{[][]string{{"-lgc", "600,900"}, {"-rmc"}}, "lgc=600,900", ""},
		}},
		{"hbm", "nvidia-h100-sxm-4gpu", false, []step{
			{[][]string{{"-lgc", "1500,1500"}}, "lgc=1500,1500", api.GPUGovernorPresetPeak},
			{[][]string{{"-lgc", "300,300"}}, "lgc=300,300", api.GPUGovernorPresetLow},
			{[][]string{{"-lgc", "300,1200"}}, "lgc=300,1200", api.GPUGovernorPresetEfficient},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := nvclockProfile(t, tc.profile, tc.lockable)
			tree := renderTestTree(t, p)
			n := nodeNewNode(t, p, tree)
			var last step
			for _, s := range tc.steps {
				for _, a := range s.args {
					nvclockMustSMI(t, tree, append([]string{"-i", "1"}, a...)...)
				}
				nodeStep(t, n, time.Second)
				g := stateComponent(t, n.State(), "gpu1")
				if g.GovernorNative != s.native || g.GovernorPreset != s.preset {
					t.Errorf("after %v: native %q, preset %q; want %q, %q", s.args, g.GovernorNative, g.GovernorPreset, s.native, s.preset)
				}
				last = s
			}

			lock := nodeNVMLFile(1, layout.NVMLLockedGPUClocksFile)
			held := nodeRead(t, tree, lock)
			for value, kind := range map[string]string{"fast": EventParseError, "900,300": EventRevertedOutOfRange} {
				nodeWrite(t, tree, lock, value)
				nodeStep(t, n, time.Second)
				if ev := nodeEvents(n, lock); len(ev) != 1 || ev[0].Kind != kind {
					t.Errorf("write %q: events %+v, want one %s", value, ev, kind)
				}
				if got := nodeRead(t, tree, lock); got != held {
					t.Errorf("write %q: lock file %q, want %q back", value, got, held)
				}
			}

			m := nodeNewNode(t, p, tree)
			if g := stateComponent(t, m.State(), "gpu1"); g.GovernorNative != last.native || g.GovernorPreset != last.preset {
				t.Errorf("a Node over the live tree reports %q, %q; want %q, %q", g.GovernorNative, g.GovernorPreset, last.native, last.preset)
			}
		})
	}
}

// Profile validation checks gpus.clocks: NVIDIA only, ordered ranges, a
// sustained maximum within the range, a P-state name, and supported lists
// that are not empty, repeat no clock and span exactly the range.
func TestNVClockValidation(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "nvidia-l40s-4gpu")
	p.GPUs.Clocks = nvclockFacts(true)
	if err := p.Validate(); err != nil {
		t.Fatalf("neutral clock facts are rejected: %v", err)
	}
	f := func(v int64) Fact[int64] { return Fact[int64]{V: v, Src: "assumed:test"} }
	for _, tc := range []struct {
		name   string
		mutate func(*NVClocksSpec)
		want   string
	}{
		{"idle below min", func(c *NVClocksSpec) { c.SM.IdleMHz = f(200) }, "gpus.clocks.sm needs 0 < minMHz <= idleMHz <= maxMHz"},
		{"mem max below idle", func(c *NVClocksSpec) { c.Mem.MaxMHz = f(200) }, "gpus.clocks.mem needs"},
		{"sustained above max", func(c *NVClocksSpec) { c.SM.SustainedMaxMHz = f(1600) }, "gpus.clocks.sm.sustainedMaxMHz"},
		{"pstate", func(c *NVClocksSpec) { c.IdlePState = Fact[string]{V: "P16", Src: "assumed:test"} }, "gpus.clocks.idlePState"},
		{"list misses max", func(c *NVClocksSpec) { c.Supported.SMMHz.V = []int64{1200, 300} }, "gpus.clocks.supported.smMHz spans"},
		{"list repeats", func(c *NVClocksSpec) { c.Supported.MemMHz.V = []int64{1200, 300, 300} }, "gpus.clocks.supported.memMHz lists a clock twice"},
		{"list empty", func(c *NVClocksSpec) { c.Supported.MemMHz = Fact[[]int64]{} }, "gpus.clocks.supported.memMHz is empty"},
	} {
		p := renderBuiltin(t, "nvidia-l40s-4gpu")
		p.GPUs.Clocks = nvclockFacts(true)
		tc.mutate(p.GPUs.Clocks)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", tc.name, err, tc.want)
		}
	}
	a := renderBuiltin(t, "amd-radeon-pro-w7900-4gpu")
	a.GPUs.Clocks = nvclockFacts(true)
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "gpus.clocks is NVIDIA only") {
		t.Errorf("clocks on an AMD profile: err = %v, want NVIDIA only", err)
	}
}

// A lower memory clock offers only the lower graphics clocks
// (supported.smMaxByMem): on the L40S under load, a memory lock at 405 MHz
// holds the graphics clock at 645 MHz and one at 810 MHz at 1290 MHz, as
// clocks.sm and the clock limit State reports show, and the board draws
// less; without the lock the GPU runs at its sustained maximum again.
func TestNV13MemoryLockHoldsTheGraphicsClockAtItsCeiling(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-l40s-4gpu")
	tree := renderTestTree(t, p)
	n := nodeNewNode(t, p, tree)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	nodeStep(t, n, time.Second)
	free := n.State().GPUs[1].PowerW
	for _, c := range []struct {
		lock string
		want int64
	}{{"405,405", 645}, {"810,810", 1290}, {"", p.GPUs.Clocks.SM.SustainedMaxMHz.V}} {
		if c.lock == "" {
			nvclockMustSMI(t, tree, "-i", "1", "-rmc")
		} else {
			nvclockMustSMI(t, tree, "-i", "1", "-lmc", c.lock)
		}
		nodeStep(t, n, 10*time.Second)
		if got := nvclockQuery(t, tree, 1, "clocks.sm"); got != strconv.FormatInt(c.want, 10) {
			t.Errorf("-lmc %q: clocks.sm %s, want %d", c.lock, got, c.want)
		}
		g := stateComponent(t, n.State(), "gpu1")
		if got := g.ClockLimitHz[semconv.ClockDomainGraphics]; got != c.want*1e6 {
			t.Errorf("-lmc %q: graphics clock limit %d Hz, want %d MHz", c.lock, got, c.want)
		}
		if w := n.State().GPUs[1].PowerW; c.lock == "405,405" && w >= free {
			t.Errorf("-lmc 405: %.1f W, want below the %.1f W of the unlocked GPU", w, free)
		}
	}
}
