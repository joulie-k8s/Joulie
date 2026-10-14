package main

// Model resolution: which hardware model a modelled node runs, from the
// environment, the node classes and the hardware catalogue.

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/simulator/pkg/hw"
)

func (s *simulator) modelForNode(node string) simModel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.nodeModels[node]; ok {
		return m
	}
	return s.model
}

func (s *simulator) resolveModelForNode(nodeLabels map[string]string) (simModel, string) {
	model := s.model
	className := "default"
	for _, c := range s.classes {
		if !matchLabels(nodeLabels, c.MatchLabels) {
			continue
		}
		className = c.Name
		model = applyModelOverrides(model, c.Model)
		break
	}
	// The node's GPU count label wins over the class and the environment,
	// whether or not the catalogue knows its GPU: a GPU the catalogue does
	// not know used to keep a count of 0, so the node drew no GPU power.
	if n := hwinv.ParseIntString(nodeLabels["joulie.io/hw.gpu-count"]); n > 0 {
		model.GPU.Count = n
	}
	model, catalogClass := s.applyCatalogModelDefaults(model, nodeLabels)
	if catalogClass != "" {
		className = catalogClass
	}
	return orderCapRanges(model, className), className
}

// orderCapRanges keeps each cap range of model ordered, its minimum at or
// below its maximum, with a warning when it lowers one. A minimum set by hand
// can sit above a maximum the catalogue filled in: SIM_CPU_SOCKET_CAP_MIN_W of
// 200 on a Xeon Platinum 8260, whose TDP is 165 W. The maximum is the
// hardware's limit, so the minimum comes down to it, as capMinAtMax does for
// a catalogue minimum. className names the model in the warning.
func orderCapRanges(model simModel, className string) simModel {
	order := func(what string, minW *float64, maxW float64) {
		if maxW <= 0 || *minW <= maxW {
			return
		}
		warnOnce(fmt.Sprintf("cap range %s %s %v %v", className, what, *minW, maxW),
			fmt.Sprintf("%s minimum %v W is above its maximum %v W for class %s; using %v W", what, *minW, maxW, className, maxW))
		*minW = maxW
	}
	order("CPU socket cap", &model.CPUSocketCapMinW, model.CPUSocketCapMaxW)
	order("GPU cap", &model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	return model
}

func (s *simulator) applyCatalogModelDefaults(model simModel, nodeLabels map[string]string) (simModel, string) {
	if s.catalog == nil {
		return model, ""
	}
	desc := hwinv.NodeDescriptor{
		CPUModelRaw: firstNonEmpty(
			nodeLabels["joulie.io/hw.cpu-model"],
			nodeLabels["feature.node.kubernetes.io/cpu-model.name"],
		),
		CPUSockets: hwinv.ParseIntString(nodeLabels["joulie.io/hw.cpu-sockets"]),
		// After the simulator's own label, the controller manager's
		// GPU_PRODUCT_LABEL_KEYS default, in its order: the AMD node
		// labeller names the board in gpu.product-name, under both of its
		// prefixes, and the family code, read last, names no board.
		GPUModelRaw: firstNonEmpty(
			nodeLabels["joulie.io/hw.gpu-model"],
			nodeLabels["joulie.io/gpu.product"],
			nodeLabels["nvidia.com/gpu.product"],
			nodeLabels["amd.com/gpu.product-name"],
			nodeLabels["beta.amd.com/gpu.product-name"],
			nodeLabels["amd.com/gpu.product"],
			nodeLabels["amd.com/gpu.family"],
		),
		GPUCount: hwinv.ParseIntString(nodeLabels["joulie.io/hw.gpu-count"]),
	}
	match := s.catalog.MatchNode(desc)
	if match.CPUSpec != nil {
		cpuSpec := *match.CPUSpec
		model.CPUModel = match.CPUKey
		model.CPUProvenance = cpuSpec.Provenance
		if desc.CPUSockets > 0 {
			model.CPUSockets = desc.CPUSockets
		}
		sockets := model.CPUSockets
		if sockets <= 0 {
			sockets = 2
		}
		model.CPUDriverFamily = cpuSpec.Official.DriverFamily
		if cpuSpec.MeasuredCurves != nil && cpuSpec.MeasuredCurves.Node2S != nil && len(model.CPUPowerCurve) == 0 {
			// node2S was measured on a two-socket server. Another socket
			// count scales the whole curve, idle included, by sockets/2
			// (assumed: no measured curve of such a node exists).
			scale := float64(sockets) / 2
			points := make([]hw.PowerPoint, 0, len(cpuSpec.MeasuredCurves.Node2S.Points))
			for _, p := range cpuSpec.MeasuredCurves.Node2S.Points {
				points = append(points, hw.PowerPoint{LoadPct: p.LoadPct, PowerW: p.PowerW * scale})
			}
			model.CPUPowerCurve = points
			model.CPUCurveSource = cpuSpec.MeasuredCurves.Node2S.Source
		}
		if cpuSpec.ProxyFrom != nil {
			model.CPUProxyFrom = cpuSpec.ProxyFrom.Family
		}
		if tdp := cpuSpec.Official.TDPW; tdp > 0 {
			if catalogFills(model.Defaulted.CPUSocketCapMaxW, model.CPUSocketCapMaxW) {
				model.CPUSocketCapMaxW = tdp
				model.Defaulted.CPUSocketCapMaxW = false
			}
			if catalogFills(model.Defaulted.CPUSocketCapMinW, model.CPUSocketCapMinW) {
				minW := 0.55 * tdp
				if r := cpuSpec.Official.CTdpRangeW; len(r) > 0 && r[0] > 0 {
					minW = r[0]
				}
				model.CPUSocketCapMinW = capMinAtMax(minW, model.CPUSocketCapMaxW)
				model.Defaulted.CPUSocketCapMinW = false
			}
			// The analytic model peaks at every socket's TDP (assumed: the
			// package power at full load, with no measured curve to say
			// otherwise). A curve sets its own peak.
			if len(model.CPUPowerCurve) == 0 && catalogFills(model.Defaulted.PMaxW, model.PMaxW) {
				model.PMaxW = float64(sockets) * tdp
				model.Defaulted.PMaxW = false
			}
		}
	}
	if match.GPUSpec != nil {
		gpuSpec := *match.GPUSpec
		model.GPU.Provenance = gpuSpec.Provenance
		model.GPU.Vendor = gpuSpec.Official.Vendor
		model.GPU.Product = match.GPUKey
		if catalogFills(model.Defaulted.GPUMaxWattsPerGPU, model.GPU.MaxWattsPerGPU) && gpuSpec.Official.MaxBoardPowerW > 0 {
			model.GPU.MaxWattsPerGPU = gpuSpec.Official.MaxBoardPowerW
			model.Defaulted.GPUMaxWattsPerGPU = false
		}
		if catalogFills(model.Defaulted.GPUMinCapWattsPerGPU, model.GPU.MinCapWattsPerGPU) {
			minW := gpuSpec.Official.MinBoardPowerW
			if minW <= 0 {
				minW = 0.5 * model.GPU.MaxWattsPerGPU
			}
			model.GPU.MinCapWattsPerGPU = capMinAtMax(minW, model.GPU.MaxWattsPerGPU)
			model.Defaulted.GPUMinCapWattsPerGPU = false
		}
	}
	nameParts := []string{"default"}
	if match.CPUKey != "" {
		nameParts = append(nameParts, strings.ToLower(strings.ReplaceAll(match.CPUKey, "_", "-")))
	}
	if match.GPUKey != "" {
		nameParts = append(nameParts, strings.ToLower(strings.ReplaceAll(match.GPUKey, "_", "-")))
	}
	return model, strings.Join(nameParts, "+")
}

// catalogFills reports whether the catalogue may set a field: one that holds
// its built-in constant (hw.Defaulted) or a value of 0 or less, which the
// model reads as unset.
func catalogFills(defaulted bool, v float64) bool {
	return defaulted || v <= 0
}

// capMinAtMax lowers a catalogue minimum to the maximum when the environment
// or a node class set a maximum below it, so a catalogue minimum never sits
// above the maximum.
func capMinAtMax(minW, maxW float64) float64 {
	if maxW > 0 && minW > maxW {
		return maxW
	}
	return minW
}

// loadNodeClasses reads SIM_NODE_CLASS_CONFIG and validates every class
// against the base model; an empty path means no classes. A removed model
// key is ignored with a warning.
func loadNodeClasses(path string) ([]simNodeClass, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, key := range hw.RemovedClassKeys(b) {
		warnOnce("class "+key, fmt.Sprintf("SIM_NODE_CLASS_CONFIG key %s was removed and is ignored", key))
	}
	return hw.ParseClasses(b, defaultBaseModelFromEnv())
}

// removedModelEnv are the variables of model knobs that were removed because
// nothing read them: the per-pod and per-throttle-percent watts of the old
// linear power model, and the GPU power model (alpha, beta).
var removedModelEnv = []string{"SIM_POD_W", "SIM_DVFS_DROP_W_PER_PCT", "SIM_GPU_ALPHA_UTIL", "SIM_GPU_BETA_CAP"}

var (
	removedKnobWarnMu   sync.Mutex
	removedKnobWarnSeen = map[string]struct{}{}
)

// warnOnce logs msg the first time key is seen in this process: the base
// model is built more than once at startup, and a node's model is resolved
// again on every poll.
func warnOnce(key, msg string) {
	removedKnobWarnMu.Lock()
	_, seen := removedKnobWarnSeen[key]
	removedKnobWarnSeen[key] = struct{}{}
	removedKnobWarnMu.Unlock()
	if !seen {
		log.Printf("warning: %s", msg)
	}
}

// Built-in values of the fields the hardware catalogue may set (hw.Defaulted).
const (
	defaultPMaxW                = 420
	defaultCPUSocketCapMinW     = 120
	defaultCPUSocketCapMaxW     = 400
	defaultGPUMaxWattsPerGPU    = 300
	defaultGPUMinCapWattsPerGPU = 80
)

// envOrConstant is floatEnv for a field the catalogue may set: it also
// reports whether the value is the built-in constant, which it is unless key
// holds a number.
func envOrConstant(key string, def float64) (float64, bool) {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, false
		}
	}
	return def, true
}

func matchLabels(nodeLabels, required map[string]string) bool {
	for k, v := range required {
		if nodeLabels[k] != v {
			return false
		}
	}
	return true
}

func applyModelOverrides(base simModel, o simModelOverrides) simModel {
	return hw.ApplyOverrides(base, o)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// defaultBaseModelFromEnv builds the base hardware profile from SIM_* variables.
// The peak power, the socket cap range and the GPU board power range are
// marked hw.Defaulted while their variables are unset, so that a node the
// hardware catalogue matches takes them from the catalogue instead.
// Thermal throttling is opt-in: while both of a device's
// *_THERMAL_THROTTLE_{START,FULL}_C are unset they default to 0, which
// thermalThrottleFraction reads as off. Setting either one turns the pair on
// with the former default (85/97 C, 82/92 C) for the other, so an
// environment that sets one throttles as before instead of failing
// validation or ramping from 0 C. The thermal constants are not calibrated
// per platform, and with the old defaults a measured curve at full load
// heated to about 126 C, throttled to throughput 0 and cycled
// (thermal_test.go).
func defaultBaseModelFromEnv() simModel {
	for _, key := range removedModelEnv {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			warnOnce(key, fmt.Sprintf("%s was removed and is ignored", key))
		}
	}
	pMaxW, pMaxDefaulted := envOrConstant("SIM_P_MAX_W", defaultPMaxW)
	socketMinW, socketMinDefaulted := envOrConstant("SIM_CPU_SOCKET_CAP_MIN_W", defaultCPUSocketCapMinW)
	socketMaxW, socketMaxDefaulted := envOrConstant("SIM_CPU_SOCKET_CAP_MAX_W", defaultCPUSocketCapMaxW)
	gpuMaxW, gpuMaxDefaulted := envOrConstant("SIM_GPU_MAX_WATTS_PER_GPU", defaultGPUMaxWattsPerGPU)
	gpuMinW, gpuMinDefaulted := envOrConstant("SIM_GPU_MIN_CAP_WATTS_PER_GPU", defaultGPUMinCapWattsPerGPU)
	cpuThrottleStartC, cpuThrottleFullC := 0.0, 0.0
	if strings.TrimSpace(os.Getenv("SIM_CPU_THERMAL_THROTTLE_START_C")) != "" || strings.TrimSpace(os.Getenv("SIM_CPU_THERMAL_THROTTLE_FULL_C")) != "" {
		cpuThrottleStartC, cpuThrottleFullC = 85, 97
	}
	gpuThrottleStartC, gpuThrottleFullC := 0.0, 0.0
	if strings.TrimSpace(os.Getenv("SIM_GPU_THERMAL_THROTTLE_START_C")) != "" || strings.TrimSpace(os.Getenv("SIM_GPU_THERMAL_THROTTLE_FULL_C")) != "" {
		gpuThrottleStartC, gpuThrottleFullC = 82, 92
	}
	return simModel{
		BaseIdleW:                 floatEnv("SIM_BASE_IDLE_W", 80),
		RaplHeadW:                 floatEnv("SIM_RAPL_HEADROOM_W", 5),
		DefaultCapW:               floatEnv("SIM_DEFAULT_CAP_W", 5000),
		PMaxW:                     pMaxW,
		AlphaUtil:                 floatEnv("SIM_ALPHA_UTIL", 1.15),
		BetaFreq:                  floatEnv("SIM_BETA_FREQ", 1.35),
		FMinMHz:                   floatEnv("SIM_F_MIN_MHZ", 1200),
		FMaxMHz:                   floatEnv("SIM_F_MAX_MHZ", 3200),
		RaplCapMinW:               floatEnv("SIM_RAPL_CAP_MIN_W", 80),
		RaplCapMaxW:               floatEnv("SIM_RAPL_CAP_MAX_W", 5000),
		DvfsRampMS:                int(floatEnv("SIM_DVFS_RAMP_MS", 500)),
		CPUCapApplyTauMS:          int(floatEnv("SIM_CPU_CAP_APPLY_TAU_MS", 250)),
		CPUTelemetryWindowMS:      int(floatEnv("SIM_CPU_TELEMETRY_WINDOW_MS", 250)),
		CPUAmbientTempC:           floatEnv("SIM_CPU_AMBIENT_TEMP_C", 24),
		CPUThermalTauMS:           int(floatEnv("SIM_CPU_THERMAL_TAU_MS", 4000)),
		CPUWattsPerDeltaC:         floatEnv("SIM_CPU_WATTS_PER_DELTA_C", 8.0),
		CPUThermalThrottleStartC:  floatEnv("SIM_CPU_THERMAL_THROTTLE_START_C", cpuThrottleStartC),
		CPUThermalThrottleFullC:   floatEnv("SIM_CPU_THERMAL_THROTTLE_FULL_C", cpuThrottleFullC),
		CPUDriverFamily:           strings.TrimSpace(envOrDefault("SIM_CPU_DRIVER_FAMILY", "amd-pstate")),
		CPULowestNonlinearFreqMHz: floatEnv("SIM_CPU_LOWEST_NONLINEAR_FREQ_MHZ", 1800),
		CPUSockets:                int(floatEnv("SIM_CPU_SOCKETS", 2)),
		CPUSocketCapMinW:          socketMinW,
		CPUSocketCapMaxW:          socketMaxW,
		GPU: hw.GPUProfile{
			Vendor:                strings.TrimSpace(envOrDefault("SIM_GPU_VENDOR", "")),
			Product:               strings.TrimSpace(envOrDefault("SIM_GPU_PRODUCT", "")),
			Count:                 int(floatEnv("SIM_GPU_COUNT", 0)),
			IdleWattsPerGPU:       floatEnv("SIM_GPU_IDLE_WATTS_PER_GPU", 30),
			MaxWattsPerGPU:        gpuMaxW,
			MinCapWattsPerGPU:     gpuMinW,
			CapApplyTauMS:         int(floatEnv("SIM_GPU_CAP_APPLY_TAU_MS", 150)),
			TelemetryWindowMS:     int(floatEnv("SIM_GPU_TELEMETRY_WINDOW_MS", 1000)),
			AmbientTempC:          floatEnv("SIM_GPU_AMBIENT_TEMP_C", 24),
			ThermalTauMS:          int(floatEnv("SIM_GPU_THERMAL_TAU_MS", 2500)),
			WattsPerDeltaC:        floatEnv("SIM_GPU_WATTS_PER_DELTA_C", 12.0),
			ThermalThrottleStartC: floatEnv("SIM_GPU_THERMAL_THROTTLE_START_C", gpuThrottleStartC),
			ThermalThrottleFullC:  floatEnv("SIM_GPU_THERMAL_THROTTLE_FULL_C", gpuThrottleFullC),
			ComputeGamma:          floatEnv("SIM_GPU_COMPUTE_GAMMA", 1.0),
			MemoryEpsilon:         floatEnv("SIM_GPU_MEMORY_EPSILON", 0.2),
			MemoryGamma:           floatEnv("SIM_GPU_MEMORY_GAMMA", 1.2),
		},
		Defaulted: hw.Defaulted{
			PMaxW:                pMaxDefaulted,
			CPUSocketCapMinW:     socketMinDefaulted,
			CPUSocketCapMaxW:     socketMaxDefaulted,
			GPUMaxWattsPerGPU:    gpuMaxDefaulted,
			GPUMinCapWattsPerGPU: gpuMinDefaulted,
		},
	}
}
