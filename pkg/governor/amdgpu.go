package governor

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
)

// amdgpu sysfs attributes of a device, under bus/pci/devices/<bus id>/.
const (
	amdLevelFile = "power_dpm_force_performance_level"
	amdODFile    = "pp_od_clk_voltage"
)

// amdgpu performance levels.
const (
	amdLevelAuto        = "auto"
	amdLevelHigh        = "high"
	amdLevelLow         = "low"
	amdLevelManual      = "manual"
	amdLevelProfilePeak = "profile_peak"
)

// amdLevels maps each family to the performance level of every preset a
// level word alone sets. A preset missing here is unsupported, except
// efficient on the families of amdODCeiling. MI300X accepts only auto,
// manual and perf_determinism; any other level is -EOPNOTSUPP.
var amdLevels = map[string]map[string]string{
	AMDFamilyNavi31: {
		joulie.GPUGovernorPresetAuto:      amdLevelAuto,
		joulie.GPUGovernorPresetSustained: amdLevelHigh,
		joulie.GPUGovernorPresetLow:       amdLevelLow,
		joulie.GPUGovernorPresetPeak:      amdLevelProfilePeak,
	},
	AMDFamilyAldebaran: {
		joulie.GPUGovernorPresetAuto:      amdLevelAuto,
		joulie.GPUGovernorPresetSustained: amdLevelHigh,
		joulie.GPUGovernorPresetLow:       amdLevelLow,
		joulie.GPUGovernorPresetPeak:      amdLevelProfilePeak,
	},
	AMDFamilyMI300: {
		joulie.GPUGovernorPresetAuto: amdLevelAuto,
	},
}

// amdODCeiling lists the families whose efficient preset is the manual level
// plus an overdrive sclk ceiling. The driver accepts the overdrive edit only
// under manual or perf_determinism. Navi 31 is left out: its overdrive file
// needs a non-default ppfeaturemask bit and its fine-grained sclk takes only
// two levels.
var amdODCeiling = map[string]bool{
	AMDFamilyAldebaran: true,
	AMDFamilyMI300:     true,
}

// amdChoice is the native state one preset maps to on one device: the
// performance level and, for an overdrive ceiling, the sclk maximum in MHz
// (zero for none). ok is false when the device cannot express the preset.
type amdChoice struct {
	level   string
	sclkMax int
	ok      bool
}

// chooseAMD maps a known preset to its native state on f.
func chooseAMD(f AMDFacts, preset string, ceilingPct float64) amdChoice {
	if preset != joulie.GPUGovernorPresetEfficient {
		level, ok := amdLevels[f.Family][preset]
		return amdChoice{level: level, ok: ok}
	}
	minMHz, maxMHz := f.SclkMHz[0], f.SclkMHz[1]
	if !amdODCeiling[f.Family] || !f.OD || maxMHz <= 0 {
		return amdChoice{}
	}
	mhz := max(int(math.Floor(ceilingPct*float64(maxMHz)/100)), minMHz)
	return amdChoice{level: amdLevelManual, sclkMax: mhz, ok: true}
}

// checkGPUPreset rejects an unknown GPU preset, and a ceiling outside
// (0, 100] for the efficient preset.
func checkGPUPreset(preset string, ceilingPct float64) error {
	if !slices.Contains(joulie.GPUGovernorPresetValues, preset) {
		return fmt.Errorf("governor: unknown GPU preset %q", preset)
	}
	if preset == joulie.GPUGovernorPresetEfficient && !(ceilingPct > 0 && ceilingPct <= 100) {
		return fmt.Errorf("governor: compute clock ceiling %v%% of max is outside (0, 100]", ceilingPct)
	}
	return nil
}

// PlanAMD plans the GPU preset (a pkg/api GPUGovernorPreset* value) for one
// amdgpu device. ceilingPct is the compute clock ceiling of the efficient
// preset, in percent of the maximum sclk; other presets ignore it. An
// unknown preset, a ceiling outside (0, 100] for efficient, or a device
// without a bus id is an error.
//
// Every preset but efficient is one write of the performance level. The
// efficient preset writes the manual level, then "s 1 <MHz>" and "c" to
// pp_od_clk_voltage: the ceiling rounded down to whole MHz, and never below
// the lowest sclk.
func PlanAMD(f AMDFacts, preset string, ceilingPct float64) (Plan, error) {
	if err := checkGPUPreset(preset, ceilingPct); err != nil {
		return Plan{}, err
	}
	if f.BusID == "" {
		return Plan{}, fmt.Errorf("governor: amdgpu device without a bus id")
	}
	c := chooseAMD(f, preset, ceilingPct)
	if !c.ok {
		return Plan{Result: joulie.GovernorResultUnsupported}, nil
	}
	dir := "bus/pci/devices/" + strings.ToLower(f.BusID) + "/"
	p := Plan{
		Writes: []Write{{RelPath: dir + amdLevelFile, Value: c.level}},
		Native: c.level,
		Result: joulie.GovernorResultApplied,
	}
	if c.sclkMax > 0 {
		mhz := strconv.Itoa(c.sclkMax)
		p.Writes = append(p.Writes,
			Write{RelPath: dir + amdODFile, Value: "s 1 " + mhz},
			Write{RelPath: dir + amdODFile, Value: "c"})
		p.Native += " sclk_max=" + mhz
	}
	return p, nil
}

// PresetsAMD lists the GPU presets the amdgpu device f can express, judging
// efficient at DefaultCeilingPctOfMax.
func PresetsAMD(f AMDFacts) []string {
	var out []string
	for _, p := range joulie.GPUGovernorPresetValues {
		if chooseAMD(f, p, DefaultCeilingPctOfMax).ok {
			out = append(out, p)
		}
	}
	return out
}
