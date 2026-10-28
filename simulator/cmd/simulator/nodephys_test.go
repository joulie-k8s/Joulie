package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/prometheus/client_golang/prometheus"
)

// stepFor steps node by dt seconds under s.mu, in substeps of at most
// physMaxSubstep, as the physics tick does.
func stepFor(s *simulator, node string, dt float64) {
	model := s.modelForNode(node)
	st := s.getNode(node)
	s.mu.Lock()
	defer s.mu.Unlock()
	stepNode(st, model, dt, physMaxSubstep)
}

// stepOnce steps st by dt seconds in a single substep and returns the
// node's power as the telemetry reports it. Before stepNode a read stepped
// the node by the wall time since its last update in one step; the tests
// that pinned the outcome of such a step use this to keep it.
func stepOnce(st *nodeState, model simModel, dt float64) float64 {
	stepNode(st, model, dt, time.Duration(dt*float64(time.Second)))
	return observe(st).powerW
}

// lagTestModel is an analytic CPU-only node whose CPU power stays constant
// while it steps: no cap binds, the frequency target is the full scale, and
// no thermal throttling is set.
func lagTestModel() simModel {
	return simModel{
		BaseIdleW: 100, PMaxW: 700, AlphaUtil: 1, BetaFreq: 1,
		FMinMHz: 1200, FMaxMHz: 3200,
		RaplHeadW: 5, DefaultCapW: 5000, RaplCapMinW: 100, RaplCapMaxW: 5000,
		CPUCapApplyTauMS: 2000, CPUTelemetryWindowMS: 1000, DvfsRampMS: 500,
		CPUSockets: 2,
	}
}

// Every lag is a clamped Euler step per substep, so after n substeps of h a
// quantity sits at target + (start - target) * (1 - h/tau)^n. One second in
// 100 ms substeps moves the cap from 5000 W toward 1000 W (tau 2 s) to
// 1000 + 4000 * 0.95^10 = 3394.95 W, and the CPU average from 100 W toward
// the constant instantaneous power P (window 1 s) to P + (100 - P) * 0.9^10.
// The same second in one substep would give 3000 W and P: the substep is
// what keeps a long tick from snapping a lag to its target.
func TestStepNodeFollowsFirstOrderLag(t *testing.T) {
	model := lagTestModel()
	st := &nodeState{CPUUtil: 0.5, FreqScale: 1, CapWatts: 5000, TargetCapWatts: 1000, CPUAvgPowerWatts: 100}
	p, _, _ := cpuPower(st, model, 1)

	stepNode(st, model, 1, physMaxSubstep)

	wantCap := 1000 + 4000*math.Pow(0.95, 10)
	if math.Abs(st.CapWatts-wantCap) > 1e-9 {
		t.Errorf("cap after 1 s: %.6f W, want %.6f W (clamped Euler, 10 substeps of 100 ms, tau 2 s)", st.CapWatts, wantCap)
	}
	wantAvg := p + (100-p)*math.Pow(0.9, 10)
	if math.Abs(st.CPUAvgPowerWatts-wantAvg) > 1e-9 {
		t.Errorf("CPU average after 1 s: %.6f W, want %.6f W (instantaneous %.4f W, window 1 s)", st.CPUAvgPowerWatts, wantAvg, p)
	}
	if st.CPUInstantPowerWatts != p {
		t.Errorf("instantaneous CPU power %.6f W, want the constant %.6f W", st.CPUInstantPowerWatts, p)
	}
	if math.Abs(st.CPUSockets[0].CapWatts-wantCap/2) > 1e-9 {
		t.Errorf("socket cap %.6f W, want half the node cap %.6f W", st.CPUSockets[0].CapWatts, wantCap/2)
	}

	// Two half seconds are the same ten substeps.
	split := &nodeState{CPUUtil: 0.5, FreqScale: 1, CapWatts: 5000, TargetCapWatts: 1000, CPUAvgPowerWatts: 100}
	stepNode(split, model, 0.5, physMaxSubstep)
	stepNode(split, model, 0.5, physMaxSubstep)
	if math.Abs(split.CapWatts-st.CapWatts) > 1e-9 || math.Abs(split.CPUAvgPowerWatts-st.CPUAvgPowerWatts) > 1e-9 {
		t.Errorf("two 0.5 s steps: cap %.9f W, average %.9f W; one 1 s step: %.9f W, %.9f W", split.CapWatts, split.CPUAvgPowerWatts, st.CapWatts, st.CPUAvgPowerWatts)
	}
}

// stepNode reads no clock: two copies of a node, one last updated an hour
// ago, stepped the same 2 s at different wall times, end in the same state,
// with SimTime moved by exactly 2 s. Before stepNode a read stepped the node
// by the wall time since LastUpdate, so standalone mode, which reads its
// nodes many times a wall second while it simulates seconds per tick, barely
// moved its lags and averages at all.
func TestStepNodeIgnoresWallClock(t *testing.T) {
	model := otlpTestModel(90, 2)
	s := newSimulatorWithRegisterer(model, nil, nil, 0, prometheus.NewRegistry())
	st := s.getNode("node-a")
	st.CPUUtil, st.GPUUtil, st.TargetCapWatts, st.GPUTargetCapWattsPerGpu = 0.7, 0.5, 200, 250
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.SimTime = start

	a, b := st.clone(), st.clone()
	b.LastUpdate = time.Now().Add(-time.Hour)
	stepNode(&a, model, 2, physMaxSubstep)
	time.Sleep(20 * time.Millisecond)
	stepNode(&b, model, 2, physMaxSubstep)

	if !a.SimTime.Equal(start.Add(2 * time.Second)) {
		t.Errorf("SimTime %v, want %v", a.SimTime, start.Add(2*time.Second))
	}
	b.LastUpdate = a.LastUpdate
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the same step at another wall time and last update differs:\n%+v\n%+v", a, b)
	}
}

// SimTime moves by exactly dt, to the nanosecond, however the step is split:
// moving it by a truncated h per substep lost up to 1 ns each, so the
// physics tick and a standalone run with SIM_SA_SUBSTEP drifted from the
// clock that steps them (11 substeps of a 1.000123456 s tick moved 2 ns
// less; a 5 s tick in 300 ms substeps moved 4.999999999 s).
func TestStepNodeMovesSimTimeByExactlyDt(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		dt, sub time.Duration
	}{
		{1000123456 * time.Nanosecond, physMaxSubstep},
		{5 * time.Second, 300 * time.Millisecond},
		{5 * time.Second, 0},
	} {
		st := &nodeState{CPUUtil: 0.5, FreqScale: 1, CapWatts: 5000, TargetCapWatts: 5000, SimTime: start}
		for i := 1; i <= 3; i++ {
			stepNode(st, lagTestModel(), tc.dt.Seconds(), tc.sub)
			if got, want := st.SimTime.Sub(start), time.Duration(i)*tc.dt; got != want {
				t.Errorf("dt %v, substep %v, step %d: SimTime moved %v, want %v", tc.dt, tc.sub, i, got, want)
			}
		}
	}
}

// The energy stepNode returns is the integral of the instantaneous power, not
// of the lagging average a power meter reports: a node starting with a 0 W
// average at constant instantaneous power P draws P x dt over dt, whatever
// the substep. stepLegacyNodes books it per node and per hw.type.
func TestEnergyIsTheIntegralOfInstantPower(t *testing.T) {
	model := lagTestModel()
	model.GPU = hw.GPUProfile{Count: 2, IdleWattsPerGPU: 30, MaxWattsPerGPU: 300, MinCapWattsPerGPU: 100, TelemetryWindowMS: 10000}
	newNode := func() *nodeState {
		st := &nodeState{CPUUtil: 0.5, GPUUtil: 0.5, FreqScale: 1, CapWatts: 5000, TargetCapWatts: 5000}
		stepNode(st, model, 0, physMaxSubstep)
		st.CPUAvgPowerWatts, st.TotalAvgPowerWatts = 0, 0
		for i := range st.GPUDevices {
			st.GPUDevices[i].AvgPowerWatts = 0
		}
		return st
	}
	st := newNode()
	cpuP, gpuP := st.CPUInstantPowerWatts, st.GPUPowerWatts
	if cpuP <= 0 || gpuP <= 0 {
		t.Fatalf("instantaneous power cpu %.2f W, gpu %.2f W; the test needs both", cpuP, gpuP)
	}

	for _, sub := range []time.Duration{physMaxSubstep, 10 * time.Second, 0} {
		st := newNode()
		cpuJ, gpuJ := stepNode(st, model, 10, sub)
		if math.Abs(cpuJ-cpuP*10) > 1e-9*cpuP*10 || math.Abs(gpuJ-gpuP*10) > 1e-9*gpuP*10 {
			t.Errorf("substep %v: energy cpu %.6f J, gpu %.6f J; want %.6f J and %.6f J (instantaneous power x 10 s)", sub, cpuJ, gpuJ, cpuP*10, gpuP*10)
		}
		if sub == physMaxSubstep && st.CPUAvgPowerWatts >= cpuP {
			t.Errorf("the CPU average %.4f W has reached the instantaneous %.4f W; the test needs it lagging", st.CPUAvgPowerWatts, cpuP)
		}
	}

	s := newSimulatorWithRegisterer(model, nil, nil, 0, prometheus.NewRegistry())
	s.state["node-a"] = newNode()
	s.stepLegacyNodes(10 * time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, want := s.energyJByHwType[semconv.HwTypeCPU], cpuP*10; math.Abs(got-want) > 1e-6 {
		t.Errorf("energy by hw.type cpu %.6f J, want %.6f J", got, want)
	}
	if got, want := s.energyJByHwType[semconv.HwTypeGPU], gpuP*10; math.Abs(got-want) > 1e-6 {
		t.Errorf("energy by hw.type gpu %.6f J, want %.6f J", got, want)
	}
	if got, want := s.energyJByNode["node-a"], (cpuP+gpuP)*10; math.Abs(got-want) > 1e-6 || s.energyTotalJ != got {
		t.Errorf("node energy %.6f J, total %.6f J; want %.6f J for both", got, s.energyTotalJ, want)
	}
}

// A SIM_PHYSICS_TICK of 0 or less takes the default: time.NewTicker panics
// on it, which took the whole simulator down at start.
func TestPhysicsLoopNonPositiveTickTakesDefault(t *testing.T) {
	s := newSimulatorWithRegisterer(lagTestModel(), nil, nil, 0, prometheus.NewRegistry())
	for _, interval := range []time.Duration{0, -time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("runPhysicsLoop(%v) panicked: %v", interval, r)
				}
			}()
			s.runPhysicsLoop(ctx, interval)
		}()
		cancel()
	}
}

// In Kubernetes mode without SIM_WORKLOAD_TRACE_PATH no workload loop runs,
// so the loop that always runs, the physics tick, keeps the facility gauges
// (joulie_sim_facility_*) and the fake Prometheus answers that read them
// current. They stayed at 0, an ambient of 0 C and no IT power, because
// only the workload loop, emulate mode and standalone mode set them.
func TestPhysicsLoopWithoutATraceUpdatesFacilityGauges(t *testing.T) {
	s := newSimulatorWithRegisterer(lagTestModel(), nil, nil, 0, prometheus.NewRegistry())
	s.facilityBaseAmbientTempC, s.facilityTempAmplitudeC, s.facilityTempPeriodH = 22, 5, 24
	s.getNode("n1")
	if s.workload != nil {
		t.Fatal("test setup: a simulator without a trace has a workload engine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runPhysicsLoop(ctx, 5*time.Millisecond)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for gaugeValue(s.facilityITPowerW) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gaugeValue(s.facilityAmbientTemp); math.Abs(got-22) > 0.1 {
		t.Errorf("ambient %v C after the physics loop ran for up to 5 s without a trace, want about 22", got)
	}
	if got := gaugeValue(s.facilityITPowerW); got <= 0 {
		t.Errorf("IT power %v W with one node, want above 0", got)
	}
	if got := gaugeValue(s.facilityPUE); got < 1 {
		t.Errorf("PUE %v, want at least 1", got)
	}
	if v, ok := s.facilityMetricValue("datacenter_ambient_temperature_celsius"); !ok || math.Abs(v-22) > 0.1 {
		t.Errorf("the fake Prometheus answers the ambient query with %v, %v; want about 22", v, ok)
	}
}

// The physics tick and every reader and writer of the node state run at
// once. Under -race this fails if any of them touches a node outside s.mu:
// /state and /debug/nodes encode a node after taking it, so they take a
// deep copy (nodeState.clone) or encode under the lock.
func TestPhysicsTickAndHandlersRace(t *testing.T) {
	s := newSimulatorWithRegisterer(otlpTestModel(90, 2), nil, nil, 50, prometheus.NewRegistry())
	nodes := map[string]map[string]string{"node-0": {}, "node-1": {}, "node-2": {}}
	s.refreshNodeStateFromKubeData(map[string]int{"node-0": 2}, nil, nodes)
	mux := s.newMux()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runPhysicsLoop(ctx, time.Millisecond)
	}()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				node := fmt.Sprintf("node-%d", (g+i)%3)
				body := fmt.Sprintf(`{"action":"rapl.set_power_cap_watts","capWatts":%d}`, 150+10*i)
				if i%2 == 1 {
					body = fmt.Sprintf(`{"action":"gpu.set_power_cap_watts","capWattsPerGpu":%d}`, 200+i)
				}
				for _, req := range []*http.Request{
					httptest.NewRequest(http.MethodPost, "/control/"+node, strings.NewReader(body)),
					httptest.NewRequest(http.MethodGet, "/telemetry/"+node, nil),
					httptest.NewRequest(http.MethodGet, "/telemetry/"+node+"?format=legacy", nil),
					httptest.NewRequest(http.MethodGet, "/otlp/telemetry/"+node, nil),
					httptest.NewRequest(http.MethodGet, "/state/"+node, nil),
					httptest.NewRequest(http.MethodGet, "/debug/nodes", nil),
				} {
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						t.Errorf("%s %s: status %d", req.Method, req.URL.Path, w.Code)
					}
				}
				if g == 0 {
					s.refreshNodeStateFromKubeData(map[string]int{node: i % 4}, nil, nodes)
				}
			}
		}(g)
	}
	wg.Wait()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.energyTotalJ <= 0 {
		t.Fatalf("the physics tick booked %v J; it never ran", s.energyTotalJ)
	}
}

// floorTestModel is the analytic model with the code defaults of
// defaultBaseModelFromEnv (idle 80 W, maximum 420 W, minimum scale
// 1200/3200 = 0.375), written out so that later default changes do not move
// the tests that use it.
func floorTestModel() simModel {
	return simModel{
		BaseIdleW: 80, PMaxW: 420, AlphaUtil: 1.15, BetaFreq: 1.35,
		FMinMHz: 1200, FMaxMHz: 3200,
		RaplCapMinW: 80, RaplCapMaxW: 5000, RaplHeadW: 5,
	}
}

// A node's CPU power follows its utilization and the frequency it runs at,
// never the throttle target it was given. A C-state cut used to lower the
// power of any node with a target above 0 below util 0.2, by up to half: at
// util 0.05 and full frequency a node with target 40 drew 56.78 W and one
// with target 0 drew 90.85 W, a saving that no frequency or cap change
// caused and that only Joulie-managed nodes received.
func TestIdlePowerDoesNotDependOnThrottleTarget(t *testing.T) {
	model := floorTestModel()
	for _, util := range []float64{0, 0.05} {
		want := 80 + 340*math.Pow(util, 1.15)
		if util == 0.05 && math.Abs(want-90.85) > 0.01 {
			t.Fatalf("test setup: util 0.05 at full frequency is %.4f W, want 90.85 W by hand", want)
		}
		for _, pct := range []int{0, 40} {
			st := &nodeState{CPUUtil: util, FreqScale: 1, CapWatts: 5000, TargetThrottlePct: pct}
			if got := cpuPowerWithModel(st, model); math.Abs(got-want) > 1e-9 {
				t.Errorf("util %.2f at full frequency, throttle target %d %%: %.4f W, want %.4f W whatever the target", util, pct, got, want)
			}
		}
	}
}

// A cap below the power at the minimum frequency cannot hold: the package
// runs at its lowest frequency and draws what it draws there, which is the
// power the node must report and its energy must integrate. The power used
// to be clipped to cap plus headroom anyway, so at util 1 under a 100 W cap
// the analytic node reported 105 W while its power at the minimum scale
// 0.375 is 80 + 340 x 0.375^1.35 = 170.5 W. The curve branch floors the same
// way, at the curve's own power at the minimum scale.
func TestCapBelowFloorReportsFloorPower(t *testing.T) {
	analytic := floorTestModel()
	curve := floorTestModel()
	curve.CPUPowerCurve = spec01203Curve
	const class = "cpu.compute_bound"
	minScale := minFreqScale(analytic)
	analyticFloorW := 80 + 340*math.Pow(minScale, 1.35)
	if math.Abs(analyticFloorW-170.5) > 0.05 {
		t.Fatalf("test setup: analytic power at util 1 and the minimum scale %.3f is %.4f W, want 170.5 W by hand", minScale, analyticFloorW)
	}
	curveFloorW := cpuPowerModel(curve).Power(phys.DeviceState{Utilization: 1, FreqScale: minScale, Class: class})

	cases := []struct {
		name   string
		model  simModel
		capW   float64
		floorW float64
	}{
		{"analytic", analytic, 100, analyticFloorW},
		{"curve", curve, 300, curveFloorW},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.floorW <= tc.capW+tc.model.RaplHeadW {
				t.Fatalf("test setup: the floor %.1f W is not above the %.0f W cap plus %.0f W headroom", tc.floorW, tc.capW, tc.model.RaplHeadW)
			}
			st := &nodeState{CPUUtil: 1, FreqScale: 1, CapWatts: tc.capW, CPUWorkClass: class}
			got := cpuPowerWithModel(st, tc.model)
			if math.Abs(got-tc.floorW) > 1e-9 || !st.CapSaturated || st.FreqScale != minScale {
				t.Errorf("util 1 under a %.0f W cap: power %.4f W, capSaturated=%v, scale %.4f; want the floor %.4f W, capSaturated=true and the minimum scale %.3f, not the cap plus headroom (%.0f W)",
					tc.capW, got, st.CapSaturated, st.FreqScale, tc.floorW, minScale, tc.capW+tc.model.RaplHeadW)
			}
		})
	}
}
