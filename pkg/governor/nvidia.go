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
	var locks []string
	if c.gpu != nil {
		v := fmt.Sprintf("%d,%d", c.gpu[0], c.gpu[1])
		args = append(args, cmd("-lgc", v))
		locks = append(locks, "-lgc "+v)
	} else if f.LockedGPU {
		args = append(args, cmd("-rgc"))
	}
	if c.mem != nil {
		v := fmt.Sprintf("%d,%d", c.mem[0], c.mem[1])
		args = append(args, cmd("-lmc", v))
		locks = append(locks, "-lmc "+v)
	} else if f.LockedMem && nvMemLockable(f) {
		args = append(args, cmd("-rmc"))
	}
	p := Plan{Args: args, Native: strings.Join(locks, " "), Result: c.result}
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

// PresetsNvidia lists the GPU presets the NVIDIA GPU f can express, judging
// efficient at DefaultCeilingPctOfMax.
func PresetsNvidia(f NVFacts) []string {
	var out []string
	for _, p := range joulie.GPUGovernorPresetValues {
		if chooseNvidia(f, p, DefaultCeilingPctOfMax).result != joulie.GovernorResultUnsupported {
			out = append(out, p)
		}
	}
	return out
}
