package main

// Job speed: how a node's frequency and throttling slow the CPU work of the
// jobs on it. The progress rates themselves are workload.Rates
// (legacyJobRates).

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
