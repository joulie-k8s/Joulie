package main

// Job speed: how a node's frequency, throttling and feed slow the jobs on it.

import (
	"github.com/matbun/joulie/simulator/pkg/phys"
)

func cpuThroughputMultiplier(freqScale float64, workloadClass string, model simModel, memoryIntensity, ioIntensity, thermalThrottle float64) float64 {
	m := phys.MeasuredCurveCPUModel{
		Points: model.CPUPowerCurve,
		Knee:   cpuKneeFromModel(model),
	}
	return m.ThroughputMultiplier(phys.DeviceState{
		FreqScale:       freqScale,
		MemoryIntensity: memoryIntensity,
		IOIntensity:     ioIntensity,
		ThermalThrottle: thermalThrottle,
		Class:           workloadClass,
	}, workloadClass)
}

func cpuThrottleImpactFactor(cpuMul float64, j *simJob) float64 {
	baseImpact := clamp01(0.60*clamp01(j.CPUUtilTarget) + 0.25*(1.0-clamp01(j.MemoryIntensity)) + 0.15*(1.0-clamp01(j.IOIntensity)))
	effective := 1.0 - baseImpact*(1.0-clamp01(cpuMul))
	effective = 1.0 - j.SensitivityCPU*(1.0-effective)
	return clamp01(effective)
}

func cpuFeedThrottleFactor(cpuMul float64, j *simJob) float64 {
	feed := clamp01(j.CPUFeedIntensity)
	if feed <= 0 {
		return 1
	}
	return clamp01(1.0 - (1.0-clamp01(cpuMul))*feed*j.SensitivityCPU)
}
