package hwemu

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// NVIDIA clock emulation, profile side: the checks of gpus.clocks and the
// clock files of the NVML state (clocks, maximum and supported clocks, the
// hidden locks). Validate and Render call these hooks.

// The clock files of a GPU's NVMLGPUDir, besides the locks of package
// layout. clocks_sm_mhz, clocks_mem_mhz, pstate and clocks_event_reasons are
// emulator-owned and follow the load and the locks; clocks_max_sm_mhz,
// clocks_max_mem_mhz and supported_clocks are rendered once. The fake
// nvidia-smi answers clocks.sm, clocks.mem, pstate,
// clocks_event_reasons.active, clocks.max.sm and clocks.max.mem from them,
// and -q -d SUPPORTED_CLOCKS from supported_clocks.
const (
	nvclockSMFile        = "clocks_sm_mhz"
	nvclockMemFile       = "clocks_mem_mhz"
	nvclockPStateFile    = "pstate"
	nvclockReasonsFile   = "clocks_event_reasons"
	nvclockMaxSMFile     = "clocks_max_sm_mhz"
	nvclockMaxMemFile    = "clocks_max_mem_mhz"
	nvclockSupportedFile = "supported_clocks"
)

// Contents of a lock file: nvclockUnlocked when no lock is set, otherwise
// "<min>,<max>" in MHz as nvidia-smi was told them. A memory lock file that
// holds nvclockNotSupported is a GPU whose memory clocks cannot be locked
// at run time, such as Hopper (the nvidia-smi documentation: -lmc "does not
// work on GPUs based on NVIDIA Hopper architectures"); it is then
// render-owned, so a write to it is restored.
const (
	nvclockUnlocked     = "none"
	nvclockNotSupported = "[N/A]"
)

// Clock event reasons, the bits of clocks_event_reasons.active
// (nvmlClocksEventReason* in nvml.h). The file holds the mask as nvidia-smi
// prints it, 0x and 16 hex digits.
const (
	nvclockReasonGPUIdle             uint64 = 0x1
	nvclockReasonApplicationsSetting uint64 = 0x2
	nvclockReasonSWPowerCap          uint64 = 0x4
)

var renderPStateRegex = regexp.MustCompile(`^P([0-9]|1[0-5])$`)

// nvclockReasonsText is a mask as nvidia-smi prints clocks_event_reasons.active,
// such as 0x0000000000000001 for an idle GPU.
func nvclockReasonsText(mask uint64) string {
	return fmt.Sprintf("0x%016X", mask)
}

// nvclockLockFloorAtIdle is gpus.clocks.lockFloorAtIdle, true when unset.
func nvclockLockFloorAtIdle(c *NVClocksSpec) bool {
	return c.LockFloorAtIdle == nil || c.LockFloorAtIdle.V
}

// nvclockLocksResetOnDeinit is gpus.clocks.locksResetOnDeinit, false when
// unset.
func nvclockLocksResetOnDeinit(c *NVClocksSpec) bool {
	return c.LocksResetOnDeinit != nil && c.LocksResetOnDeinit.V
}

// renderValidateNVClock returns every problem with p.GPUs.Clocks: clocks on
// a GPU that is not NVIDIA, a range that is not 0 < min <= idle <= max, a
// sustained maximum outside [min, max], an idle performance state that is
// not P0 to P15, and a supported list that is empty, repeats a clock, or
// does not span exactly [min, max] of its domain, which -q -d
// SUPPORTED_CLOCKS would then contradict.
func renderValidateNVClock(p *Profile) []error {
	if p.GPUs == nil || p.GPUs.Clocks == nil {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: gpus.clocks."+format, append([]any{p.Name}, args...)...))
	}
	if p.GPUs.Vendor != "nvidia" {
		return []error{fmt.Errorf("%s: gpus.clocks is NVIDIA only; vendor is %q", p.Name, p.GPUs.Vendor)}
	}
	c := p.GPUs.Clocks
	domain := func(name string, minMHz, idleMHz, maxMHz int64, supported []int64) {
		if minMHz <= 0 || minMHz > idleMHz || idleMHz > maxMHz {
			fail("%s needs 0 < minMHz <= idleMHz <= maxMHz, got %d, %d, %d", name, minMHz, idleMHz, maxMHz)
		}
		list := "supported." + name + "MHz"
		if len(supported) == 0 {
			fail("%s is empty", list)
			return
		}
		sorted := slices.Clone(supported)
		slices.Sort(sorted)
		if len(slices.Compact(sorted)) != len(supported) {
			fail("%s lists a clock twice", list)
		}
		if sorted[0] != minMHz || sorted[len(sorted)-1] != maxMHz {
			fail("%s spans %d to %d MHz, want %s.minMHz %d to maxMHz %d", list, sorted[0], sorted[len(sorted)-1], name, minMHz, maxMHz)
		}
	}
	domain("sm", c.SM.MinMHz.V, c.SM.IdleMHz.V, c.SM.MaxMHz.V, c.Supported.SMMHz.V)
	domain("mem", c.Mem.MinMHz.V, c.Mem.IdleMHz.V, c.Mem.MaxMHz.V, c.Supported.MemMHz.V)
	if s := c.SM.SustainedMaxMHz.V; s < c.SM.MinMHz.V || s > c.SM.MaxMHz.V {
		fail("sm.sustainedMaxMHz %d is outside [minMHz, maxMHz]", s)
	}
	if !renderPStateRegex.MatchString(c.IdlePState.V) {
		fail("idlePState %q is not P0 to P15", c.IdlePState.V)
	}
	return errs
}

// renderNVClock writes the clock files of every NVIDIA GPU of p that has
// gpus.clocks, in its NVML state directory: the current clocks at idle, the
// idle performance state and the GPU-idle event reason (S), the maximum and
// supported clocks (R), and the hidden locks, unlocked (A). A GPU whose
// memory clock has no run-time lock gets the not-supported token in
// locked_mem_clocks, render-owned. supported_clocks holds one line per
// domain, the clocks in descending order as -q -d SUPPORTED_CLOCKS prints
// them:
//
//	memory 1500 800 300
//	graphics 1500 1485 ... 300
func renderNVClock(b *renderBuilder, p *Profile, o RenderOptions) error {
	g := p.GPUs
	if g == nil || g.Vendor != "nvidia" || g.Clocks == nil {
		return nil
	}
	c := g.Clocks
	supported := "memory " + nvclockMHzList(c.Supported.MemMHz.V) + "\ngraphics " + nvclockMHzList(c.Supported.SMMHz.V) + "\n"
	memLock := renderEntry{name: layout.NVMLLockedMemClocksFile, content: nvclockUnlocked + "\n", owner: layout.OwnerAgent, mode: 0o644}
	if !c.Mem.RuntimeLock.V {
		memLock = renderEntry{name: layout.NVMLLockedMemClocksFile, content: nvclockNotSupported + "\n", owner: layout.OwnerRender, mode: 0o444}
	}
	for i := 0; i < g.Count.V; i++ {
		entries := []renderEntry{
			{name: nvclockSMFile, content: renderUint(c.SM.IdleMHz.V), owner: layout.OwnerEmulator, mode: 0o444},
			{name: nvclockMemFile, content: renderUint(c.Mem.IdleMHz.V), owner: layout.OwnerEmulator, mode: 0o444},
			{name: nvclockPStateFile, content: c.IdlePState.V + "\n", owner: layout.OwnerEmulator, mode: 0o444},
			{name: nvclockReasonsFile, content: nvclockReasonsText(nvclockReasonGPUIdle) + "\n", owner: layout.OwnerEmulator, mode: 0o444},
			{name: nvclockMaxSMFile, content: renderUint(c.SM.MaxMHz.V), owner: layout.OwnerRender, mode: 0o444},
			{name: nvclockMaxMemFile, content: renderUint(c.Mem.MaxMHz.V), owner: layout.OwnerRender, mode: 0o444},
			{name: nvclockSupportedFile, content: supported, owner: layout.OwnerRender, mode: 0o444},
			{name: layout.NVMLLockedGPUClocksFile, content: nvclockUnlocked + "\n", owner: layout.OwnerAgent, mode: 0o644},
			memLock,
		}
		if err := b.writeEntries(path.Join(layout.StateDir, layout.NVMLGPUDir(i)), entries); err != nil {
			return err
		}
	}
	return nil
}

// nvclockMHzList is clocks in descending order, separated by spaces.
func nvclockMHzList(mhz []int64) string {
	sorted := slices.Clone(mhz)
	slices.Sort(sorted)
	slices.Reverse(sorted)
	words := make([]string, len(sorted))
	for i, v := range sorted {
		words[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(words, " ")
}
