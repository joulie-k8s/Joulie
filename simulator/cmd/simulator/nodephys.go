package main

// The legacy node physics: the CPU and GPU power of a modelled (non-emulated)
// node, its cap and frequency lags, its temperatures and its power averages.
//
// Physics advances only in stepNode, in simulated time (nodeState.SimTime):
// the physics tick (stepLegacyNodes) steps every modelled node, and
// standalone mode steps them on its virtual clock. Readers (the HTTP
// handlers, the pod poll, the gauges) take observe, which changes nothing,
// so how often a node is read never changes what it does.

import (
	"math"
	"strings"
	"time"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// physMaxSubstep is the longest substep stepNode takes for the physics tick.
// Every lag is a clamped Euler step (phys.FirstOrderToward), which reaches
// its target in one step once the step is as long as the time constant, so
// the substep stays below the shortest default constant, the 150 ms GPU
// cap lag.
const physMaxSubstep = 100 * time.Millisecond

// stepNode advances st by dt seconds of simulated time, in equal substeps of
// at most maxSubstep (a single step when maxSubstep is 0 or dt fits in it),
// and returns the energy the CPU and the GPUs drew: the integral of their
// instantaneous power over the substeps. It reads no clock, and st.SimTime
// moves by dt. A dt of 0 moves nothing and only brings the instantaneous
// readings up to date with st's targets and load.
//
// Each substep runs, in order: the frequency ramp, the CPU cap lag, the GPU
// cap lag, the CPU power with the frequency solved down to the cap, the CPU
// temperature, the GPU power and temperature, the averages, the energy, and
// SimTime.
func stepNode(st *nodeState, model simModel, dt float64, maxSubstep time.Duration) (cpuJ, gpuJ float64) {
	if !(dt > 0) {
		dt = 0
	}
	n := 1
	if limit := maxSubstep.Seconds(); limit > 0 && dt > limit {
		n = int(math.Ceil(dt / limit))
	}
	h := dt / float64(n)
	// SimTime is set from its start, rounded to the nanosecond, rather than
	// moved by h each substep: a truncated h would lose up to 1 ns per
	// substep against the clock that steps the node.
	start := st.SimTime
	for i := 0; i < n; i++ {
		cpuW, gpuW := stepNodeOnce(st, model, h)
		cpuJ += cpuW * h
		gpuJ += gpuW * h
		st.SimTime = start.Add(time.Duration(math.Round(float64(i+1) * h * float64(time.Second))))
	}
	settled := st.SimTime.Format(time.RFC3339Nano)
	for i := range st.GPUDevices {
		st.GPUDevices[i].SettledAtTimestamp = settled
	}
	return cpuJ, gpuJ
}

// stepNodeOnce is one substep of h seconds of stepNode, without the clock.
// It returns the CPU and GPU instantaneous power at the end of the substep,
// which stepNode integrates.
func stepNodeOnce(st *nodeState, model simModel, h float64) (cpuW, gpuW float64) {
	// 1. Frequency ramp toward the DVFS target.
	targetScale := 1.0 - clamp01(float64(st.TargetThrottlePct)/100.0)
	rampSec := math.Max(0.05, float64(model.DvfsRampMS)/1000.0)
	if st.FreqScale == 0 {
		st.FreqScale = 1
	}
	st.FreqScale = phys.FirstOrderToward(st.FreqScale, targetScale, h, rampSec)
	st.FreqScale = math.Max(minFreqScale(model), clamp01(st.FreqScale))
	st.ThrottlePct = int(math.Round((1.0 - st.FreqScale) * 100.0))

	// 2. CPU cap lag, spread over the sockets.
	cpuCapTauMs := model.CPUCapApplyTauMS
	if cpuCapTauMs <= 0 {
		cpuCapTauMs = 250
	}
	if st.TargetCapWatts <= 0 {
		st.TargetCapWatts = model.DefaultCapW
	}
	if st.CapWatts <= 0 {
		st.CapWatts = st.TargetCapWatts
	}
	cpuCapTauSec := math.Max(0.01, float64(cpuCapTauMs)/1000.0)
	st.CapWatts = phys.FirstOrderToward(st.CapWatts, st.TargetCapWatts, h, cpuCapTauSec)

	if len(st.CPUSockets) == 0 {
		sockets := model.CPUSockets
		if sockets <= 0 {
			sockets = 2
		}
		st.CPUSockets = make([]cpuSocketState, 0, sockets)
		for i := 0; i < sockets; i++ {
			st.CPUSockets = append(st.CPUSockets, cpuSocketState{Index: i})
		}
	}
	perSocketUtil := st.CPUUtil / float64(maxIntInt(1, len(st.CPUSockets)))
	cpuClass := strings.TrimSpace(st.CPUWorkClass)
	if cpuClass == "" {
		cpuClass = "cpu.mixed"
	}
	for i := range st.CPUSockets {
		st.CPUSockets[i].Utilization = clamp01(perSocketUtil)
		st.CPUSockets[i].PerfMultiplier = cpuThroughputMultiplier(st.FreqScale, cpuClass, model, st.MemoryIntensity, st.IOIntensity, st.CPUThermalThrottle)
		st.CPUSockets[i].CapWatts = st.CapWatts / float64(len(st.CPUSockets))
		if model.CPUSocketCapMinW > 0 && st.CPUSockets[i].CapWatts < model.CPUSocketCapMinW {
			st.CPUSockets[i].CapWatts = model.CPUSocketCapMinW
		}
		if model.CPUSocketCapMaxW > 0 && st.CPUSockets[i].CapWatts > model.CPUSocketCapMaxW {
			st.CPUSockets[i].CapWatts = model.CPUSocketCapMaxW
		}
	}

	// 3. GPU cap lag.
	tauMs := model.GPU.CapApplyTauMS
	if tauMs <= 0 {
		tauMs = 150
		if strings.EqualFold(model.GPU.Product, "AMD_Instinct_MI300X") || strings.Contains(strings.ToLower(model.GPU.Product), "mi300x") {
			tauMs = 350
		}
	}
	if model.GPU.Count > 0 && len(st.GPUDevices) == 0 {
		st.GPUDevices = make([]gpuDeviceState, 0, model.GPU.Count)
		for i := 0; i < model.GPU.Count; i++ {
			c := model.GPU.MaxWattsPerGPU
			st.GPUDevices = append(st.GPUDevices, gpuDeviceState{Index: i, CapWatts: c, TargetCapWatts: c})
		}
	}
	if st.GPUTargetCapWattsPerGpu <= 0 {
		st.GPUTargetCapWattsPerGpu = st.GPUCapWattsPerGpu
	}
	tauSec := math.Max(0.01, float64(tauMs)/1000.0)
	for i := range st.GPUDevices {
		d := &st.GPUDevices[i]
		target := st.GPUTargetCapWattsPerGpu
		if target <= 0 {
			target = model.GPU.MaxWattsPerGPU
		}
		if model.GPU.MinCapWattsPerGPU > 0 && target < model.GPU.MinCapWattsPerGPU {
			target = model.GPU.MinCapWattsPerGPU
		}
		if model.GPU.MaxWattsPerGPU > 0 && target > model.GPU.MaxWattsPerGPU {
			target = model.GPU.MaxWattsPerGPU
		}
		d.TargetCapWatts = target
		if d.CapWatts <= 0 {
			d.CapWatts = target
		}
		d.CapWatts = phys.FirstOrderToward(d.CapWatts, target, h, tauSec)
		st.GPUCapWattsPerGpu = d.CapWatts
	}

	// 4. CPU power, with the frequency solved down to the cap.
	cpuW = cpuPowerWithModel(st, model)

	// 5. CPU temperature and thermal throttle, then the power at that
	// throttle.
	cpuTempTarget := thermalTarget(model.CPUAmbientTempC, cpuW, model.CPUWattsPerDeltaC)
	st.CPUTemperatureC = phys.FirstOrderToward(st.CPUTemperatureC, cpuTempTarget, h, float64(maxIntInt(1, model.CPUThermalTauMS))/1000.0)
	st.CPUThermalThrottle = thermalThrottleFraction(st.CPUTemperatureC, model.CPUThermalThrottleStartC, model.CPUThermalThrottleFullC)
	cpuW = cpuPowerWithModel(st, model)
	st.CPUInstantPowerWatts = cpuW

	// 6. GPU power and temperature, then the power at the new throttle.
	powers, _ := gpuDevicePowers(st, model)
	gpuTauSec := float64(maxIntInt(1, model.GPU.ThermalTauMS)) / 1000.0
	for i := range st.GPUDevices {
		d := &st.GPUDevices[i]
		targetTemp := thermalTarget(model.GPU.AmbientTempC, powers[i], model.GPU.WattsPerDeltaC)
		d.TemperatureC = phys.FirstOrderToward(d.TemperatureC, targetTemp, h, gpuTauSec)
		d.ThermalThrottle = thermalThrottleFraction(d.TemperatureC, model.GPU.ThermalThrottleStartC, model.GPU.ThermalThrottleFullC)
	}
	powers, perf := gpuDevicePowers(st, model)
	perfSum := 0.0
	for i := range st.GPUDevices {
		st.GPUDevices[i].PowerWatts = powers[i]
		st.GPUDevices[i].PerfMultiplier = perf[i]
		gpuW += powers[i]
		perfSum += perf[i]
	}
	st.GPUPowerWatts = gpuW
	if len(st.GPUDevices) > 0 {
		st.GPUPerfMultiplier = perfSum / float64(len(st.GPUDevices))
	} else {
		st.GPUPerfMultiplier = 1.0
	}

	// 7. Averages, as a power meter's window reports them.
	st.CPUAvgPowerWatts = phys.FirstOrderToward(st.CPUAvgPowerWatts, cpuW, h, float64(maxIntInt(1, model.CPUTelemetryWindowMS))/1000.0)
	gpuWindowSec := float64(maxIntInt(1, model.GPU.TelemetryWindowMS)) / 1000.0
	for i := range st.GPUDevices {
		d := &st.GPUDevices[i]
		d.AvgPowerWatts = phys.FirstOrderToward(d.AvgPowerWatts, d.PowerWatts, h, gpuWindowSec)
	}
	st.TotalAvgPowerWatts = st.CPUAvgPowerWatts + gpuAvgPower(st)
	return cpuW, gpuW
}

// nodeObservation is a modelled node as a reader sees it: the readings the
// last stepNode left, never advanced by the read.
type nodeObservation struct {
	// powerW is the legacy packagePowerWatts: the node's average power
	// rounded to 0.01 W, or its instantaneous power while the average is 0.
	powerW float64
	// average says whether powerW is the average.
	average bool
	// cpuW and gpuW are the parts of powerW, unrounded, gpuW in the order
	// of st.GPUDevices: the averages, or the instantaneous powers while the
	// node's average is 0. hostW is cpuW plus every gpuW.
	cpuW, hostW float64
	gpuW        []float64
}

// observe reads st's power as the telemetry routes, the control response and
// the gauges report it. It changes nothing.
func observe(st *nodeState) nodeObservation {
	o := nodeObservation{average: st.TotalAvgPowerWatts > 0}
	if o.average {
		o.powerW = math.Round(st.TotalAvgPowerWatts*100) / 100
		o.cpuW = st.CPUAvgPowerWatts
	} else {
		o.powerW = math.Round((st.CPUInstantPowerWatts+st.GPUPowerWatts)*100) / 100
		o.cpuW = st.CPUInstantPowerWatts
	}
	o.hostW = o.cpuW
	for _, d := range st.GPUDevices {
		w := d.PowerWatts
		if o.average {
			w = d.AvgPowerWatts
		}
		o.gpuW = append(o.gpuW, w)
		o.hostW += w
	}
	return o
}

// gpuDevicePowers returns the power and the throughput multiplier of each of
// st's GPU devices at their current caps, temperatures and thermal
// throttles. The node's average utilization is spread over the busy devices:
// as many as the average needs run at the utilization that gives it, the
// rest idle (util 0), so idle boards can drop to their idle power. A model
// without GPU power gives 0 W and multiplier 1. It changes nothing.
func gpuDevicePowers(st *nodeState, model simModel) (powerW, perf []float64) {
	powerW = make([]float64, len(st.GPUDevices))
	perf = make([]float64, len(st.GPUDevices))
	if model.GPU.Count <= 0 || model.GPU.MaxWattsPerGPU <= 0 {
		for i := range perf {
			perf[i] = 1
		}
		return powerW, perf
	}
	gpuModel := phys.CappedBoardGPUModel{
		IdleW:         model.GPU.IdleWattsPerGPU,
		MaxW:          model.GPU.MaxWattsPerGPU,
		ComputeGamma:  model.GPU.ComputeGamma,
		MemoryEpsilon: model.GPU.MemoryEpsilon,
		MemoryGamma:   model.GPU.MemoryGamma,
	}
	class := strings.TrimSpace(st.GPUWorkClass)
	if class == "" {
		class = "gpu.mixed"
	}
	nDevices := float64(len(st.GPUDevices))
	avgUtil := clamp01(st.GPUUtil)
	activeDevices := int(math.Ceil(avgUtil * nDevices))
	if activeDevices > len(st.GPUDevices) {
		activeDevices = len(st.GPUDevices)
	}
	activeUtil := 0.0
	if activeDevices > 0 {
		activeUtil = clamp01(avgUtil * nDevices / float64(activeDevices))
	}
	for i, d := range st.GPUDevices {
		deviceUtil := 0.0
		if i < activeDevices {
			deviceUtil = activeUtil
		}
		deviceState := phys.DeviceState{
			Utilization:      deviceUtil,
			CapWatts:         d.CapWatts,
			MaxCapWatts:      model.GPU.MaxWattsPerGPU,
			IdlePowerWatts:   model.GPU.IdleWattsPerGPU,
			MemoryIntensity:  st.MemoryIntensity,
			CPUFeedIntensity: st.CPUFeedIntensity,
			TemperatureC:     d.TemperatureC,
			ThermalThrottle:  d.ThermalThrottle,
			Class:            class,
		}
		powerW[i] = gpuModel.Power(deviceState)
		perf[i] = gpuModel.ThroughputMultiplier(deviceState, class)
	}
	return powerW, perf
}

// cpuPowerWithModel returns the node's CPU power and, above the cap, lowers
// st.FreqScale to the scale that meets it and sets st.CapSaturated
// (cpuPower).
func cpuPowerWithModel(st *nodeState, model simModel) float64 {
	p, scale, saturated := cpuPower(st, model, st.FreqScale)
	st.FreqScale, st.CapSaturated = scale, saturated
	return p
}

// cpuPower returns the node's CPU power at frequency scale scale and, above
// the cap, the scale that meets it, which is otherwise scale itself. The
// natural power is computed uncapped on the node's own model (cpuPowerModel),
// so that a measured curve, which clamps to any cap it is given, still shows
// the solver its excess (D13). saturated marks a cap that even the minimum
// scale exceeds; the power is then the power at that scale, not the cap. The
// throttle target plays no part. It changes nothing.
func cpuPower(st *nodeState, model simModel, scale float64) (powerW, solvedScale float64, saturated bool) {
	util := clamp01(st.CPUUtil)
	cpuClass := strings.TrimSpace(st.CPUWorkClass)
	if cpuClass == "" {
		cpuClass = "cpu.mixed"
	}
	capW := st.CapWatts
	if capW < model.RaplCapMinW {
		capW = model.RaplCapMinW
	}
	if model.RaplCapMaxW > 0 && capW > model.RaplCapMaxW {
		capW = model.RaplCapMaxW
	}
	minFreq := minFreqScale(model)
	cpuModel := cpuPowerModel(model)
	cpuState := phys.DeviceState{
		Utilization:     util,
		FreqScale:       clamp01(scale),
		MemoryIntensity: st.MemoryIntensity,
		IOIntensity:     st.IOIntensity,
		TemperatureC:    st.CPUTemperatureC,
		ThermalThrottle: st.CPUThermalThrottle,
		Class:           cpuClass,
	}

	p := math.Max(20, cpuModel.Power(cpuState))

	solvedScale = scale
	floorW := 0.0
	if p > capW {
		// A curve's floor is its own power at the minimum scale. The
		// analytic floor stays thermal-free, as before F2: with the thermal
		// factor, util 0.3 under a 100 W cap would no longer be saturated
		// (102.7 against 84.7 W; cpupower_regression_test.go).
		floor := cpuState
		floor.FreqScale = minFreq
		if len(model.CPUPowerCurve) == 0 {
			floor.ThermalThrottle = 0
		}
		floorW = cpuModel.Power(floor)
		saturated = floorW > capW
		targetFreq := phys.SolveFreqScaleForCap(cpuModel, cpuState, capW, minFreq)
		solvedScale = math.Max(minFreq, math.Min(scale, targetFreq))
		cpuState.FreqScale = clamp01(solvedScale)
		p = cpuModel.Power(cpuState)
	}
	// The power is clipped to cap plus headroom, but never below the floor:
	// a cap under the power at the minimum scale cannot pull the package
	// lower, so a saturated node reports what it draws there.
	return math.Min(p, math.Max(capW+model.RaplHeadW, floorW)), solvedScale, saturated
}

// cpuPowerModel is the node's CPU power model: its measured curve when it has
// one, otherwise the analytic model. Neither is given a cap.
func cpuPowerModel(model simModel) phys.PowerModel {
	if len(model.CPUPowerCurve) > 0 {
		return phys.MeasuredCurveCPUModel{
			Points: model.CPUPowerCurve,
			Knee:   cpuKneeFromModel(model),
		}
	}
	return phys.AnalyticCPUModel{
		IdleW:     model.BaseIdleW,
		MaxW:      model.PMaxW,
		AlphaUtil: model.AlphaUtil,
		BetaFreq:  model.BetaFreq,
		Knee:      cpuKneeFromModel(model),
	}
}

func thermalTarget(ambientC, powerW, wattsPerDeltaC float64) float64 {
	if wattsPerDeltaC <= 0 {
		return ambientC
	}
	return ambientC + powerW/wattsPerDeltaC
}

func thermalThrottleFraction(tempC, startC, fullC float64) float64 {
	if fullC <= 0 || fullC <= startC {
		return 0
	}
	if tempC <= startC {
		return 0
	}
	if tempC >= fullC {
		return 1
	}
	return clamp01((tempC - startC) / (fullC - startC))
}

func gpuAvgPower(st *nodeState) float64 {
	total := 0.0
	for _, d := range st.GPUDevices {
		total += d.AvgPowerWatts
	}
	return total
}

func cpuKneeFromModel(model simModel) float64 {
	if model.CPULowestNonlinearFreqMHz > 0 && model.FMaxMHz > 0 {
		return clamp01(model.CPULowestNonlinearFreqMHz / model.FMaxMHz)
	}
	return 0.7
}

func minFreqScale(model simModel) float64 {
	if model.FMaxMHz <= 0 || model.FMinMHz <= 0 {
		return 0.35
	}
	return clamp01(model.FMinMHz / model.FMaxMHz)
}
