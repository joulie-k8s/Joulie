package main

// The governor configuration and the governor policy: which CPU and GPU
// governor presets the controller manager writes into NodeTwin.spec.
// loadGovernorConfig reads every variable once at start. Governor control is
// off unless GOVERNOR_CONTROL turns it on, and a value it cannot use logs a
// warning and keeps the default, so a typo never turns writes on.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
)

// Values of GOVERNOR_CONTROL.
const (
	governorControlOff = "off"
	governorControlCPU = "cpu"
	governorControlGPU = "gpu"
	governorControlAll = "all"
)

// gpuGovernorModelPresets is one entry of GPU_GOVERNOR_MODEL_PRESETS_JSON. A
// field left out keeps the environment default; a preset set to "" writes
// none for that model.
type gpuGovernorModelPresets struct {
	Performance        *string  `json:"performance,omitempty"`
	Eco                *string  `json:"eco,omitempty"`
	EcoCeilingPctOfMax *float64 `json:"ecoCeilingPctOfMax,omitempty"`
}

// governorConfig is the governor configuration. An empty preset is not
// written.
type governorConfig struct {
	control              string // off, cpu, gpu or all
	cpuPerformancePreset string
	cpuEcoPreset         string
	cpuEcoBoost          string // on, off, or empty to leave boost alone
	cpuOnGPUNodes        bool
	gpuPerformancePreset string
	gpuEcoPreset         string
	// gpuCeilingPctOfMax is the compute clock ceiling written with the
	// efficient preset.
	gpuCeilingPctOfMax float64
	// gpuModelPresets is keyed like GPU_MODEL_CAPS_JSON: the GPU product
	// label value, also matched against the catalog key and the raw model
	// the agent reports.
	gpuModelPresets map[string]gpuGovernorModelPresets
}

// defaultGovernorConfig is the configuration with no variable set: control
// off, and the presets it would write once turned on.
func defaultGovernorConfig() governorConfig {
	return governorConfig{
		control:              governorControlOff,
		cpuPerformancePreset: joulie.CPUGovernorPresetPerformance,
		cpuEcoPreset:         joulie.CPUGovernorPresetEfficiency,
		gpuEcoPreset:         joulie.GPUGovernorPresetEfficient,
		// The agent's default (pkg/governor), provisional until measured: the
		// best energy per job of the published sweeps sits between 61 and 76 %
		// of the maximum clock, and the constant power of the rest of the node
		// moves the node optimum up.
		gpuCeilingPctOfMax: governor.DefaultCeilingPctOfMax,
		gpuModelPresets:    map[string]gpuGovernorModelPresets{},
	}
}

// loadGovernorConfig reads GOVERNOR_CONTROL, CPU_GOVERNOR_PERFORMANCE_PRESET,
// CPU_GOVERNOR_ECO_PRESET, CPU_GOVERNOR_ECO_BOOST, CPU_GOVERNOR_ON_GPU_NODES,
// GPU_GOVERNOR_PERFORMANCE_PRESET, GPU_GOVERNOR_ECO_PRESET,
// GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX and GPU_GOVERNOR_MODEL_PRESETS_JSON.
func loadGovernorConfig() governorConfig {
	cfg := defaultGovernorConfig()

	control := strings.ToLower(envOrDefault("GOVERNOR_CONTROL", cfg.control))
	switch control {
	case governorControlOff, governorControlCPU, governorControlGPU, governorControlAll:
		cfg.control = control
	default:
		log.Printf("warning: GOVERNOR_CONTROL=%q is not %s, %s, %s or %s; writing no governor (%s)",
			control, governorControlOff, governorControlCPU, governorControlGPU, governorControlAll, governorControlOff)
	}

	cfg.cpuPerformancePreset = checkGovernorPreset("CPU_GOVERNOR_PERFORMANCE_PRESET",
		envOrDefault("CPU_GOVERNOR_PERFORMANCE_PRESET", cfg.cpuPerformancePreset), cfg.cpuPerformancePreset, joulie.CPUGovernorPresetValues)
	cfg.cpuEcoPreset = checkGovernorPreset("CPU_GOVERNOR_ECO_PRESET",
		envOrDefault("CPU_GOVERNOR_ECO_PRESET", cfg.cpuEcoPreset), cfg.cpuEcoPreset, joulie.CPUGovernorPresetValues)
	if boost := strings.ToLower(envOrDefault("CPU_GOVERNOR_ECO_BOOST", "")); boost != "" {
		if slices.Contains(joulie.CPUGovernorBoostValues, boost) {
			cfg.cpuEcoBoost = boost
		} else {
			log.Printf("warning: CPU_GOVERNOR_ECO_BOOST=%q is not %s; leaving boost alone",
				boost, strings.Join(joulie.CPUGovernorBoostValues, " or "))
		}
	}
	cfg.cpuOnGPUNodes = governorBoolEnv("CPU_GOVERNOR_ON_GPU_NODES", cfg.cpuOnGPUNodes)

	// An empty GPU preset is a valid choice (write none), so a variable set
	// to "" is not the same as one left unset.
	if v, ok := os.LookupEnv("GPU_GOVERNOR_PERFORMANCE_PRESET"); ok {
		cfg.gpuPerformancePreset = checkGovernorPreset("GPU_GOVERNOR_PERFORMANCE_PRESET", v, cfg.gpuPerformancePreset, gpuGovernorPresetChoices())
	}
	if v, ok := os.LookupEnv("GPU_GOVERNOR_ECO_PRESET"); ok {
		cfg.gpuEcoPreset = checkGovernorPreset("GPU_GOVERNOR_ECO_PRESET", v, cfg.gpuEcoPreset, gpuGovernorPresetChoices())
	}
	if v := envOrDefault("GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX", ""); v != "" {
		pct, err := strconv.ParseFloat(v, 64)
		if err != nil || !validCeilingPct(pct) {
			log.Printf("warning: GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX=%q is not a number in (0, 100]; using %g",
				v, governor.DefaultCeilingPctOfMax)
		} else {
			cfg.gpuCeilingPctOfMax = pct
		}
	}
	cfg.gpuModelPresets = parseGPUGovernorModelPresets(envOrDefault("GPU_GOVERNOR_MODEL_PRESETS_JSON", "{}"))
	return cfg
}

// gpuGovernorPresetChoices lists what a GPU preset variable accepts: every
// GPU preset, or "" for none.
func gpuGovernorPresetChoices() []string {
	return append([]string{""}, joulie.GPUGovernorPresetValues...)
}

// checkGovernorPreset checks the value of a preset variable against the
// presets it accepts, and returns def with a warning when it is not one.
func checkGovernorPreset(name, value, def string, accepted []string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if slices.Contains(accepted, v) {
		return v
	}
	log.Printf("warning: %s=%q is not one of %s; using %q", name, value, presetList(accepted), def)
	return def
}

// presetList spells the accepted presets for a warning; "" reads as empty.
func presetList(accepted []string) string {
	out := make([]string, 0, len(accepted))
	for _, p := range accepted {
		if p == "" {
			p = `"" (none)`
		}
		out = append(out, p)
	}
	return strings.Join(out, ", ")
}

// governorBoolEnv reads a boolean variable with the spellings boolEnv
// accepts, but a value that is neither true nor false logs a warning and
// keeps def instead of reading as false.
func governorBoolEnv(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(v) {
	case "":
		return def
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	log.Printf("warning: %s=%q is not true or false; using %t", key, v, def)
	return def
}

func validCeilingPct(pct float64) bool {
	return pct > 0 && pct <= 100
}

// parseGPUGovernorModelPresets reads GPU_GOVERNOR_MODEL_PRESETS_JSON. Invalid
// JSON gives no entry; an invalid preset or ceiling in an entry is dropped
// with a warning, so that model takes the environment default for it.
func parseGPUGovernorModelPresets(raw string) map[string]gpuGovernorModelPresets {
	raw = strings.TrimSpace(raw)
	out := map[string]gpuGovernorModelPresets{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		log.Printf("warning: invalid GPU_GOVERNOR_MODEL_PRESETS_JSON: %v", err)
		return map[string]gpuGovernorModelPresets{}
	}
	choices := gpuGovernorPresetChoices()
	for model, m := range out {
		for _, field := range []struct {
			key    string
			preset **string
		}{{"performance", &m.Performance}, {"eco", &m.Eco}} {
			if *field.preset == nil {
				continue
			}
			v := strings.ToLower(strings.TrimSpace(**field.preset))
			if !slices.Contains(choices, v) {
				log.Printf("warning: GPU_GOVERNOR_MODEL_PRESETS_JSON[%q].%s=%q is not one of %s; using the environment default",
					model, field.key, **field.preset, presetList(choices))
				*field.preset = nil
				continue
			}
			*field.preset = &v
		}
		if m.EcoCeilingPctOfMax != nil && !validCeilingPct(*m.EcoCeilingPctOfMax) {
			log.Printf("warning: GPU_GOVERNOR_MODEL_PRESETS_JSON[%q].ecoCeilingPctOfMax=%g is not in (0, 100]; using the environment default",
				model, *m.EcoCeilingPctOfMax)
			m.EcoCeilingPctOfMax = nil
		}
		out[model] = m
	}
	return out
}

func (c governorConfig) cpuEnabled() bool {
	return c.control == governorControlCPU || c.control == governorControlAll
}

func (c governorConfig) gpuEnabled() bool {
	return c.control == governorControlGPU || c.control == governorControlAll
}

var (
	governorCfgMu sync.RWMutex
	governorCfg   = defaultGovernorConfig()

	governorWarningMu   sync.Mutex
	governorWarningSeen = map[string]struct{}{}
)

// install makes c the configuration the reconcile reads.
func (c governorConfig) install() {
	governorCfgMu.Lock()
	governorCfg = c
	governorCfgMu.Unlock()
}

// currentGovernorConfig returns the installed configuration.
func currentGovernorConfig() governorConfig {
	governorCfgMu.RLock()
	defer governorCfgMu.RUnlock()
	return governorCfg
}

// logSummary logs the governor configuration in one line.
func (c governorConfig) logSummary() {
	if c.control == governorControlOff {
		log.Printf("governor control: off (GOVERNOR_CONTROL=off); no governor is written")
		return
	}
	models := make([]string, 0, len(c.gpuModelPresets))
	for m := range c.gpuModelPresets {
		models = append(models, m)
	}
	sort.Strings(models)
	log.Printf("governor control: %s cpu performance=%q eco=%q eco_boost=%q on_gpu_nodes=%t gpu performance=%q eco=%q ceiling_pct_of_max=%g model_presets=%v",
		c.control, c.cpuPerformancePreset, c.cpuEcoPreset, c.cpuEcoBoost, c.cpuOnGPUNodes,
		c.gpuPerformancePreset, c.gpuEcoPreset, c.gpuCeilingPctOfMax, models)
}

// gpuPresetFor returns the GPU preset and the ceiling of the efficient preset
// for a node at profile: the model map entry first, for the first of keys it
// holds, then the environment defaults.
func (c governorConfig) gpuPresetFor(profile string, keys ...string) (string, float64) {
	preset := c.gpuEcoPreset
	if profile == profilePerformance {
		preset = c.gpuPerformancePreset
	}
	ceiling := c.gpuCeilingPctOfMax
	for _, k := range keys {
		m, ok := c.gpuModelPresets[strings.TrimSpace(k)]
		if strings.TrimSpace(k) == "" || !ok {
			continue
		}
		override := m.Eco
		if profile == profilePerformance {
			override = m.Performance
		}
		if override != nil {
			preset = *override
		}
		if m.EcoCeilingPctOfMax != nil {
			ceiling = *m.EcoCeilingPctOfMax
		}
		break
	}
	return preset, ceiling
}

// governorIntents returns the governor presets of one node at profile, the
// profile it runs at (a draining node keeps performance). A preset the node
// does not list in NodeHardware status.{cpu,gpu}.governor.presets is not
// written, with one warning: falling back to another preset, auto included,
// would override a setting the node's operator chose.
//
// GPU nodes get no CPU governor unless CPU_GOVERNOR_ON_GPU_NODES is set, the
// rule the CPU cap follows; only GPU nodes get a GPU governor.
func (c governorConfig) governorIntents(nodeName, profile string, hasGPU bool, nh NodeHardware, gpuProduct string) (*joulie.CPUGovernorIntent, *joulie.GPUGovernorIntent) {
	var cpu *joulie.CPUGovernorIntent
	if c.cpuEnabled() && (!hasGPU || c.cpuOnGPUNodes) {
		preset, boost := c.cpuEcoPreset, c.cpuEcoBoost
		if profile == profilePerformance {
			preset, boost = c.cpuPerformancePreset, ""
		}
		switch {
		case preset == "":
		case !slices.Contains(nh.CPUGovernorPresets, preset):
			warnGovernorOnce(nodeName, "cpu", fmt.Sprintf(
				"CPU governor preset %q is not in NodeHardware status.cpu.governor.presets %v; writing no CPU governor", preset, nh.CPUGovernorPresets))
		default:
			cpu = &joulie.CPUGovernorIntent{Preset: preset}
			if boost != "" {
				if nh.CPUBoostAvailable {
					cpu.Boost = boost
				} else {
					warnGovernorOnce(nodeName, "cpu-boost", fmt.Sprintf(
						"NodeHardware reports no boost control (status.cpu.governor.boost); leaving boost alone instead of %q", boost))
				}
			}
		}
	}

	var gpu *joulie.GPUGovernorIntent
	if c.gpuEnabled() && hasGPU {
		preset, ceiling := c.gpuPresetFor(profile, gpuProduct, nh.GPUModel, nh.GPURawModel)
		switch {
		case preset == "":
		case !slices.Contains(nh.GPUGovernorPresets, preset):
			warnGovernorOnce(nodeName, "gpu", fmt.Sprintf(
				"GPU governor preset %q is not in NodeHardware status.gpu.governor.presets %v; writing no GPU governor", preset, nh.GPUGovernorPresets))
		default:
			gpu = &joulie.GPUGovernorIntent{Preset: preset}
			if preset == joulie.GPUGovernorPresetEfficient {
				gpu.ComputeClockCeilingPctOfMax = floatPtr(ceiling)
			}
		}
	}
	return cpu, gpu
}

// warnGovernorOnce logs a governor warning the first time it is seen for a
// node and component, not on every reconcile.
func warnGovernorOnce(nodeName, component, msg string) {
	key := nodeName + "|" + component + "|" + msg
	governorWarningMu.Lock()
	if _, ok := governorWarningSeen[key]; ok {
		governorWarningMu.Unlock()
		return
	}
	governorWarningSeen[key] = struct{}{}
	governorWarningMu.Unlock()
	log.Printf("warning: node=%s %s", nodeName, msg)
}
