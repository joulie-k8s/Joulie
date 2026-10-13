package hwemu

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// NVIDIA clock emulation, Node side. NewNode, derive, substep and
// writeOutputs call these hooks. What the physics reads goes into
// nodeGPU.clock (clockstate.go).
//
// The fake nvidia-smi writes a GPU's locks to locked_gpu_clocks (-lgc,
// -rgc) and locked_mem_clocks (-lmc, -rmc); no query prints them, as NVML
// has no getter for a lock. Each step the clocks follow the locks and the
// load (nvclockRun): a busy GPU runs its compute clock at the highest it
// sustains, sm.sustainedMaxMHz, and its memory at mem.maxMHz, each within
// its lock, so a lock above the sustained maximum is clamped to it; an idle
// GPU sits at its idle clocks within the lock's maximum, held up at the
// lower bound of a lock above them while gpus.clocks.lockFloorAtIdle holds
// (assumed, default true). A busy GPU is in P0; an idle one in idlePState,
// or in P0 while a lock floor holds a clock above its idle clock (assumed).

// nvclockLock is one lock as nvidia-smi was told it, in MHz; set is false
// while no lock holds.
type nvclockLock struct {
	set      bool
	min, max int64
}

// text is the lock file's content for l, without the newline.
func (l nvclockLock) text() string {
	if !l.set {
		return nvclockUnlocked
	}
	return fmt.Sprintf("%d,%d", l.min, l.max)
}

// nodeGPUNVClock is the clock state of one NVIDIA GPU that gpuClockState
// does not hold: the facts, the locks as written and the files behind them.
// spec is nil for a GPU without gpus.clocks, which then has no clock model.
type nodeGPUNVClock struct {
	spec             *NVClocksSpec
	gpu, mem         nvclockLock
	gpuFile, memFile *nodeAFile

	smFile, memClkFile, pstateFile, reasonsFile *nodeSFile

	// pstate and reasons are what pstate and clocks_event_reasons show
	// after the last derive.
	pstate  string
	reasons uint64
}

// buildNVClock registers the lock files of every NVIDIA GPU and seeds the
// locks from them, so a Node started over a live tree keeps the locks in
// it. A profile without gpus.clocks builds nothing.
func (n *Node) buildNVClock() error {
	gs := n.profile.GPUs
	if gs == nil || gs.Vendor != "nvidia" || gs.Clocks == nil {
		return nil
	}
	for _, g := range n.gpus {
		if !g.nvml {
			continue
		}
		v := &g.nvclock
		v.spec = gs.Clocks
		dir := filepath.Join(n.stateRoot, filepath.FromSlash(layout.NVMLGPUDir(g.index)))
		file := func(name string) string { return filepath.Join(dir, name) }
		h, cur := nvclockLockHandler(&v.gpu)
		v.gpuFile = n.addAFile(file(layout.NVMLLockedGPUClocksFile), g.id(), h, cur)
		if gs.Clocks.Mem.RuntimeLock.V {
			h, cur = nvclockLockHandler(&v.mem)
			v.memFile = n.addAFile(file(layout.NVMLLockedMemClocksFile), g.id(), h, cur)
		}
		v.smFile = n.sfile(file(nvclockSMFile))
		v.memClkFile = n.sfile(file(nvclockMemFile))
		v.pstateFile = n.sfile(file(nvclockPStateFile))
		v.reasonsFile = n.sfile(file(nvclockReasonsFile))
		g.clock.modelled = true
		g.clock.computeMaxMHz = float64(gs.Clocks.SM.MaxMHz.V)
		g.clock.mhz = map[string]float64{}
	}
	return nil
}

// nvclockLockHandler is a lock file: none clears the lock, "<min>,<max>"
// with 0 < min <= max sets it, other numbers are reverted and anything else
// is a parse error.
func nvclockLockHandler(l *nvclockLock) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		if v == nvclockUnlocked {
			*l = nvclockLock{}
			return EventAccepted, v
		}
		lo, hi, ok := strings.Cut(v, ",")
		a, aok := nodeParseInt(lo)
		b, bok := nodeParseInt(hi)
		if !ok || !aok || !bok {
			return EventParseError, l.text()
		}
		if a <= 0 || a > b {
			return EventRevertedOutOfRange, l.text()
		}
		*l = nvclockLock{set: true, min: a, max: b}
		return EventAccepted, l.text()
	}
	return handle, func() string { return l.text() + "\n" }
}

// deriveNVClock recomputes each NVIDIA GPU's clock state from its locks and
// load.
func (n *Node) deriveNVClock() {
	for _, g := range n.gpus {
		if g.nvclock.spec != nil {
			n.deriveGPUNVClock(g)
		}
	}
}

// deriveGPUNVClock sets g.clock, the performance state and the event
// reasons of one GPU. The reasons are GPU idle on an idle GPU; on a busy
// one SW power cap while the natural power exceeds the cap in effect, and
// applications clocks setting while a graphics lock holds the clock below
// the sustained maximum (assumed: which reason a lock raises is not
// documented).
func (n *Node) deriveGPUNVClock(g *nodeGPU) {
	v := &g.nvclock
	c := v.spec
	busy := n.gpuUtil(g.index) > 0
	floor := nvclockLockFloorAtIdle(c)
	sm := nvclockRun(v.gpu, busy, floor, c.SM.MinMHz.V, c.SM.IdleMHz.V, c.SM.SustainedMaxMHz.V)
	mem := nvclockRun(v.mem, busy, floor, c.Mem.MinMHz.V, c.Mem.IdleMHz.V, c.Mem.MaxMHz.V)

	v.pstate = c.IdlePState.V
	if busy || sm > c.SM.IdleMHz.V || mem > c.Mem.IdleMHz.V {
		v.pstate = "P0"
	}
	v.reasons = 0
	switch {
	case !busy:
		v.reasons = nvclockReasonGPUIdle
	default:
		if v.gpu.set && v.gpu.max < c.SM.SustainedMaxMHz.V {
			v.reasons |= nvclockReasonApplicationsSetting
		}
		if n.nvclockCapBinds(g) {
			v.reasons |= nvclockReasonSWPowerCap
		}
	}

	k := &g.clock
	k.native = nvclockNative(v.gpu, v.mem)
	k.preset = nvclockPreset(c, v.gpu, v.mem)
	k.computeFloorMHz, k.computeCeilMHz = nvclockBounds(v.gpu)
	k.memFloorMHz, k.memCeilMHz = nvclockBounds(v.mem)
	k.dataPathAuto = !v.mem.set && c.Mem.MinMHz.V < c.Mem.MaxMHz.V
	switch {
	case mem >= c.Mem.MaxMHz.V:
		k.dataPath = phys.DataPathMax
	case mem <= c.Mem.MinMHz.V:
		k.dataPath = phys.DataPathMin
	default:
		k.dataPath = phys.DataPathMid
	}
	// The maximum in effect is the clock a busy GPU runs at within its
	// locks; the hardware maximum is NVML's maximum clock.
	smTop := nvclockRun(v.gpu, true, floor, c.SM.MinMHz.V, c.SM.IdleMHz.V, c.SM.SustainedMaxMHz.V)
	memTop := nvclockRun(v.mem, true, floor, c.Mem.MinMHz.V, c.Mem.IdleMHz.V, c.Mem.MaxMHz.V)
	k.setDomain(semconv.ClockDomainGraphics, float64(sm), float64(c.SM.MaxMHz.V), float64(smTop))
	k.setDomain(semconv.ClockDomainMemory, float64(mem), float64(c.Mem.MaxMHz.V), float64(memTop))
}

// nvclockRun is the clock of one domain: top, the highest it sustains,
// while busy, and idle while not, then within the lock: a busy clock at
// most the lock's maximum, an idle one at most the lock's maximum and, when
// floor holds, at least its minimum. A lock bounds the clock, so a minimum
// below idle never lowers an idle GPU. The result never leaves [lo, top],
// so a lock above top is clamped to it.
func nvclockRun(l nvclockLock, busy, floor bool, lo, idle, top int64) int64 {
	v := idle
	switch {
	case busy:
		v = top
		if l.set {
			v = min(v, l.max)
		}
	case l.set && floor:
		v = min(max(v, l.min), l.max)
	case l.set:
		v = min(v, l.max)
	}
	return max(lo, min(v, top))
}

// nvclockCapBinds reports whether g's cap holds its power below what the
// load would draw without one.
func (n *Node) nvclockCapBinds(g *nodeGPU) bool {
	if n.gpuModel == nil {
		return false
	}
	natural := n.gpuModel.Power(phys.DeviceState{
		Utilization:     n.gpuUtil(g.index),
		CapWatts:        math.Inf(1),
		MemoryIntensity: n.load.MemoryIntensity,
		Class:           n.load.GPUClass,
	})
	return natural > math.Max(g.settledW, nodeMinGPUCapW)+1e-9
}

// nvclockBounds is a lock as a clock range, zero on both sides without one.
func nvclockBounds(l nvclockLock) (lo, hi float64) {
	if !l.set {
		return 0, 0
	}
	return float64(l.min), float64(l.max)
}

// nvclockNative is the lock state in nvidia-smi's words: "lgc=<min>,<max>"
// and "lmc=<min>,<max>" for the locks set, space separated, or unlocked
// when none is.
func nvclockNative(gpu, mem nvclockLock) string {
	var parts []string
	if gpu.set {
		parts = append(parts, "lgc="+gpu.text())
	}
	if mem.set {
		parts = append(parts, "lmc="+mem.text())
	}
	if len(parts) == 0 {
		return api.GovernorStateUnlocked
	}
	return strings.Join(parts, " ")
}

// nvclockPreset is the preset the locks map to, as the NVIDIA mapping
// writes each one: auto with no lock (also sustained on a GPU whose memory
// clock cannot be locked, where sustained changes nothing); sustained a
// memory lock at its maximum alone; efficient a graphics lock from the
// minimum to a ceiling below the maximum; low and peak both domains locked
// at their minimum or maximum, the memory lock only where it exists. Any
// other combination maps to no single preset.
func nvclockPreset(c *NVClocksSpec, gpu, mem nvclockLock) string {
	at := func(l nvclockLock, lo, hi int64) bool { return l.set && l.min == lo && l.max == hi }
	smMin, smMax := c.SM.MinMHz.V, c.SM.MaxMHz.V
	memMin, memMax := c.Mem.MinMHz.V, c.Mem.MaxMHz.V
	memAt := func(v int64) bool {
		if c.Mem.RuntimeLock.V && memMin < memMax {
			return at(mem, v, v)
		}
		return !mem.set
	}
	switch {
	case !gpu.set && !mem.set:
		return api.GPUGovernorPresetAuto
	case !gpu.set && at(mem, memMax, memMax):
		return api.GPUGovernorPresetSustained
	case at(gpu, smMin, smMin) && memAt(memMin):
		return api.GPUGovernorPresetLow
	case at(gpu, smMax, smMax) && memAt(memMax):
		return api.GPUGovernorPresetPeak
	case gpu.set && gpu.min == smMin && gpu.max < smMax && !mem.set:
		return api.GPUGovernorPresetEfficient
	}
	return ""
}

// substepNVClock advances the clock state by one substep of h seconds.
// Nothing moves within a step: the clocks follow the load and the locks at
// once (assumed), so there is no timer to advance.
func (n *Node) substepNVClock(h float64) {}

// writeNVClock writes the clock files the emulator owns: clocks_sm_mhz,
// clocks_mem_mhz, pstate and clocks_event_reasons. Before that, a GPU the
// driver deinitializes, persistence mode Disabled with no client holding
// it, loses its locks when gpus.clocks.locksResetOnDeinit says so (assumed
// false: the driver documentation lists the power limit among the settings
// a deinit resets, but not the locks); each lock file then reads none, with
// a reset-persistence-off event.
func (n *Node) writeNVClock() error {
	var errs []error
	for _, g := range n.gpus {
		v := &g.nvclock
		if v.spec == nil {
			continue
		}
		if g.persistence == persistTextDisabled && n.gpuUtil(g.index) == 0 && nvclockLocksResetOnDeinit(v.spec) && (v.gpu.set || v.mem.set) {
			errs = append(errs, n.nvclockReset(g, &v.gpu, v.gpuFile), n.nvclockReset(g, &v.mem, v.memFile))
			n.deriveGPUNVClock(g)
		}
		put := func(f *nodeSFile, content string) {
			if f == nil {
				return
			}
			if err := n.writeS(f, content); err != nil {
				errs = append(errs, fmt.Errorf("hwemu: write %s: %w", f.rel, err))
			}
		}
		put(v.smFile, renderUint(int64(math.Round(g.clock.mhz[semconv.ClockDomainGraphics]))))
		put(v.memClkFile, renderUint(int64(math.Round(g.clock.mhz[semconv.ClockDomainMemory]))))
		put(v.pstateFile, v.pstate+"\n")
		put(v.reasonsFile, nvclockReasonsText(v.reasons)+"\n")
	}
	return errors.Join(errs...)
}

// nvclockReset clears lock l of g and rewrites its file f in place, as
// persistReset does with the power limit, unless the agent wrote the file
// since the last readback, which the next readback then handles.
func (n *Node) nvclockReset(g *nodeGPU, l *nvclockLock, f *nodeAFile) error {
	if !l.set || f == nil {
		return nil
	}
	old := l.text()
	*l = nvclockLock{}
	var err error
	if c := f.current(); c != f.held && !f.emptySeen {
		var ok bool
		ok, err = nodeRewriteInPlace(f.path, []byte(f.held), []byte(c), time.Time{}, n.leases)
		if ok {
			f.held = c
		}
	}
	n.event(f.rel, g.id(), EventResetPersistenceOff, old, nvclockUnlocked)
	return err
}
