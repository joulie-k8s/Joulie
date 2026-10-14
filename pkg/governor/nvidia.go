package governor

import (
	"fmt"
	"slices"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
)

// nvChoice is the native state one preset maps to on one NVIDIA GPU: the
// graphics and memory clock locks, [min, max] in MHz, nil for a domain left
// to the driver, and the result. The result of auto depends on the agent's
// record and is settled by PlanNvidia.
type nvChoice struct {
	gpu, mem *[2]int
	result   string
}

// nvKnownArch reports whether the architecture has a mapping table.
func nvKnownArch(arch string) bool {
	switch arch {
	case NVArchAmpere, NVArchAda, NVArchHopper, NVArchBlackwell:
		return true
	}
	return false
}

// nvMemLockable reports whether -lmc takes effect on f: at run time, and not
// on Hopper or Blackwell, which keep their memory at its rated clock.
func nvMemLockable(f NVFacts) bool {
	return f.MemLockRuntime && (f.Arch == NVArchAmpere || f.Arch == NVArchAda)
}

// distinctMHz returns the positive clocks of xs sorted ascending, without
// duplicates.
func distinctMHz(xs []int) []int {
	var out []int
	for _, x := range xs {
		if x > 0 {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// chooseNvidia maps a known preset to its native state on f.
func chooseNvidia(f NVFacts, preset string, ceilingPct float64) nvChoice {
	unsupported := nvChoice{result: joulie.GovernorResultUnsupported}
	if !nvKnownArch(f.Arch) {
		return unsupported
	}
	sm, mem := distinctMHz(f.SupportedSMMHz), distinctMHz(f.SupportedMemMHz)
	// A single memory clock leaves nothing to lock; none at all means
	// discovery could not read the table the lock needs.
	memLock := nvMemLockable(f) && len(mem) > 1
	memUnknown := nvMemLockable(f) && len(mem) == 0
	lock := func(lo, hi int) *[2]int { return &[2]int{lo, hi} }
	switch preset {
	case joulie.GPUGovernorPresetAuto:
		return nvChoice{result: joulie.GovernorResultApplied}
	case joulie.GPUGovernorPresetSustained:
		switch {
		case memLock:
			top := mem[len(mem)-1]
			return nvChoice{mem: lock(top, top), result: joulie.GovernorResultApplied}
		case memUnknown:
			return unsupported
		}
		return nvChoice{result: joulie.GovernorResultEquivalent}
	case joulie.GPUGovernorPresetEfficient:
		if len(sm) == 0 {
			return unsupported
		}
		target := ceilingPct * float64(sm[len(sm)-1]) / 100
		ceil := sm[0]
		for _, c := range sm {
			if float64(c) <= target {
				ceil = c
			}
		}
		return nvChoice{gpu: lock(sm[0], ceil), result: joulie.GovernorResultApplied}
	case joulie.GPUGovernorPresetLow, joulie.GPUGovernorPresetPeak:
		if len(sm) == 0 || memUnknown {
			return unsupported
		}
		pick := func(xs []int) int {
			if preset == joulie.GPUGovernorPresetLow {
				return xs[0]
			}
			return xs[len(xs)-1]
		}
		c := nvChoice{gpu: lock(pick(sm), pick(sm)), result: joulie.GovernorResultApplied}
		if memLock {
			c.mem = lock(pick(mem), pick(mem))
		}
		return c
	}
	return unsupported
}

// PlanNvidia plans the GPU preset for the NVIDIA GPU at busID, the PCI bus
// id nvidia-smi -i takes. ceilingPct is as for PlanAMD. An unknown preset,
// a ceiling outside (0, 100] for efficient, or an empty busID is an error.
//
// A domain the preset locks gets -lgc or -lmc; a domain it leaves to the
// driver gets -rgc or -rmc only when the agent's record holds a lock there,
// so an operator's own lock is never reset. auto with no lock in the record
// issues nothing and is equivalent.
func PlanNvidia(f NVFacts, busID, preset string, ceilingPct float64) (Plan, error) {
	if err := checkGPUPreset(preset, ceilingPct); err != nil {
		return Plan{}, err
	}
	if busID == "" {
		return Plan{}, fmt.Errorf("governor: NVIDIA GPU without a bus id")
	}
	c := chooseNvidia(f, preset, ceilingPct)
	if c.result == joulie.GovernorResultUnsupported {
		return Plan{Result: joulie.GovernorResultUnsupported}, nil
	}
	cmd := func(a ...string) []string { return append([]string{"-i", busID}, a...) }
	var args [][]string
	if c.gpu != nil {
		args = append(args, cmd("-lgc", lockArg(c.gpu)))
	} else if f.LockedGPU {
		args = append(args, cmd("-rgc"))
	}
	if c.mem != nil {
		args = append(args, cmd("-lmc", lockArg(c.mem)))
	} else if f.LockedMem && nvMemLockable(f) {
		args = append(args, cmd("-rmc"))
	}
	st := State{GPULockMHz: c.gpu, MemLockMHz: c.mem}
	p := Plan{Args: args, State: st, Native: nvidiaNative(st), Result: c.result}
	if p.Native == "" {
		p.Native = joulie.GovernorStateUnknown
		if len(args) > 0 {
			p.Native = joulie.GovernorStateUnlocked
		}
	}
	if preset == joulie.GPUGovernorPresetAuto && len(args) == 0 {
		p.Result = joulie.GovernorResultEquivalent
	}
	return p, nil
}

// lockArg is a lock as nvidia-smi is told it, "<min>,<max>".
func lockArg(l *[2]int) string {
	return fmt.Sprintf("%d,%d", l[0], l[1])
}

// nvidiaNative spells the locks of s as nvidia-smi is told them, such as
// "-lgc 300,1100 -lmc 405,405"; empty without a lock.
func nvidiaNative(s State) string {
	var locks []string
	if s.GPULockMHz != nil {
		locks = append(locks, "-lgc "+lockArg(s.GPULockMHz))
	}
	if s.MemLockMHz != nil {
		locks = append(locks, "-lmc "+lockArg(s.MemLockMHz))
	}
	return strings.Join(locks, " ")
}

// ReverseNvidia names the preset whose plan on f, at some ceiling, gives the
// graphics and memory locks gpu and mem (nil for a domain without a lock).
// ok is false when no preset does. A preset that sets the state as its own
// (applied) names it before one that falls back to it (equivalent): no lock
// is auto, also where sustained changes nothing. The efficient preset locks
// the graphics clock from its lowest supported clock to a supported clock at
// or below the ceiling, so it names such a lock that no other preset sets;
// the lowest clock alone is low's.
func ReverseNvidia(f NVFacts, gpu, mem *[2]int) (preset string, ok bool) {
	same := func(a, b *[2]int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	equivalent := ""
	for _, p := range joulie.GPUGovernorPresetValues {
		if p == joulie.GPUGovernorPresetEfficient {
			continue
		}
		c := chooseNvidia(f, p, DefaultCeilingPctOfMax)
		if c.result == joulie.GovernorResultUnsupported || !same(c.gpu, gpu) || !same(c.mem, mem) {
			continue
		}
		if c.result == joulie.GovernorResultApplied {
			return p, true
		}
		if equivalent == "" {
			equivalent = p
		}
	}
	if equivalent != "" {
		return equivalent, true
	}
	sm := distinctMHz(f.SupportedSMMHz)
	if nvKnownArch(f.Arch) && mem == nil && gpu != nil && len(sm) > 0 && gpu[0] == sm[0] && slices.Contains(sm, gpu[1]) {
		return joulie.GPUGovernorPresetEfficient, true
	}
	return "", false
}
