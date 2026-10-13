package main

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SE05: the facility gauges are computed without a workload trace. Before,
// updateFacilityMetrics took the start of its temperature cycle from the
// workload engine, which is nil without a trace, and panicked; emulate mode
// runs without a trace and computes the facility every tick. Only names
// that already existed at a94f345 are used, so the test fails there.
func TestSE05FacilityMetricsWithoutATrace(t *testing.T) {
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	s.facilityBaseAmbientTempC, s.facilityTempAmplitudeC, s.facilityTempPeriodH = 22, 5, 24
	if s.workload != nil {
		t.Fatal("a simulator without a trace has a workload engine")
	}
	s.getNode("n1")
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("updateFacilityMetrics without a trace panicked: %v", r)
			}
		}()
		s.updateFacilityMetrics(time.Now())
	}()
	// Right after the start, the sinusoid is at its base.
	if got := readGaugeValue(s.facilityAmbientTemp); math.Abs(got-22) > 0.1 {
		t.Errorf("ambient %v C right after the start, want about 22", got)
	}
	if got := readGaugeValue(s.facilityITPowerW); got <= 0 {
		t.Errorf("IT power %v W with one idle node, want above 0", got)
	}
	if got := readGaugeValue(s.facilityPUE); got < 1 {
		t.Errorf("PUE %v, want at least 1", got)
	}
}
