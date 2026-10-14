package main

// Regression tests of the CPU cap solver on a measured curve. A curve
// clamps its power to any cap it is given, so cpuPowerWithModel evaluates it
// uncapped: evaluated with the cap, p > capW never holds, and a curve node
// under a cap keeps full frequency and full throughput and is never marked
// cap-saturated. The analytic model's output is pinned by a golden
// (cpupowerGoldenPath). The curve and clearSimEnv come from thermal_test.go.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// cpupowerGoldenPath holds cpuPowerWithModel's output over the grid of
// cpupowerGoldenCases for the analytic model; -update regenerates it
// (golden_flag_test.go). The cases with memory or IO intensity under a cap
// they reach land on the cap, times the thermal factor (TestFC04 in
// loop_and_model_regression_test.go).
var cpupowerGoldenPath = filepath.Join("testdata", "physics", "analytic-cpu-power.golden.json")

// 500 W on the SPEC result 01203 curve (spec01203Curve) is load 70.25 %. A
// compute-bound workload's throughput above the knee is 0.877 x scale + 0.123,
// which reaches 0.7025 at scale 0.661. The old solver kept the scale at 1 and
// reported the clamped 500 W.
func TestSolveFreqScaleForCapCurveThrottles(t *testing.T) {
	clearSimEnv(t)
	model := defaultBaseModelFromEnv()
	model.CPUPowerCurve = spec01203Curve
	const capW = 500.0
	st := &nodeState{CPUUtil: 1, FreqScale: 1, CapWatts: capW, CPUWorkClass: "cpu.compute_bound"}

	power := cpuPowerWithModel(st, model)
	throughput := cpuThroughputMultiplier(st.FreqScale, st.CPUWorkClass, model, st.MemoryIntensity, st.IOIntensity, st.CPUThermalThrottle)
	if st.FreqScale >= 0.99 || math.Abs(st.FreqScale-0.661) > 0.005 || power < 485 || power > capW+model.RaplHeadW || throughput >= 1 {
		t.Fatalf("D13: 2x EPYC 9654 curve at util 1 under a %.0f W cap: scale=%.4f power=%.2f W throughput=%.4f; want scale near 0.661 (below 0.99), power in [485, %.0f] W and throughput below 1. A curve that clamps to the cap hides the excess from the solver.",
			capW, st.FreqScale, power, throughput, capW+model.RaplHeadW)
	}
}

// The solver must see the node's whole state, not only its utilization and
// class. A curve solver that bisects on utilization and class alone settles,
// with memory or IO intensity, at a scale where the node draws more than the
// cap (505 W, cap plus headroom, at memory intensity 0.8), and with a thermal
// throttle at one where it draws far less (about 430 W at 0.2). Each variant
// must land on the cap.
func TestSolveFreqScaleForCapCurveSeesTheFullState(t *testing.T) {
	clearSimEnv(t)
	model := defaultBaseModelFromEnv()
	model.CPUPowerCurve = spec01203Curve
	const capW = 500.0
	solve := func(mem, io, thermal float64) (power, scale float64) {
		st := &nodeState{CPUUtil: 1, FreqScale: 1, CapWatts: capW, CPUWorkClass: "cpu.compute_bound",
			MemoryIntensity: mem, IOIntensity: io, CPUThermalThrottle: thermal}
		power = cpuPowerWithModel(st, model)
		return power, st.FreqScale
	}
	_, bareScale := solve(0, 0, 0)

	cases := []struct {
		name             string
		mem, io, thermal float64
	}{
		{"memory intensity 0.8", 0.8, 0, 0},
		{"IO intensity 0.5", 0, 0.5, 0},
		{"thermal throttle 0.2", 0, 0, 0.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			power, scale := solve(tc.mem, tc.io, tc.thermal)
			if power > capW+1e-6 || power < capW-0.5 || scale >= 1 {
				t.Fatalf("D13: util 1 under a %.0f W cap with mem %.1f, io %.1f, thermal %.1f: power=%.2f W scale=%.4f; want power in [%.1f, %.0f] W and a scale below 1. A solver that drops part of the node state misses the cap.",
					capW, tc.mem, tc.io, tc.thermal, power, scale, capW-0.5, capW)
			}
			// Memory-bound work loses less throughput per step of frequency,
			// so the same cap needs a lower scale.
			if tc.mem > 0 && scale >= bareScale {
				t.Fatalf("D13: memory intensity %.1f gives scale %.4f, want below the %.4f of memory intensity 0", tc.mem, scale, bareScale)
			}
		})
	}
}

// CapSaturated means that even the minimum frequency draws more than the cap.
// For a curve node that is the curve's own power at the minimum scale, in the
// node's full state, not the generic analytic model's (170.5 W with the
// default 80 and 420 W), which would call a 300 W cap reachable. At util 1,
// memory, IO and thermal 0, the minimum scale 0.375 is below the knee 0.5625:
// the multiplier is 0.8625 x 0.375 + 0.11 x 0.7975 + 0.0275 x 0.96875 =
// 0.4378 (weights and scales of phys.MeasuredCurveCPUModel), which the curve
// puts at 390.1 W. Memory intensity 0.8 raises the floor to about 439 W and a
// thermal throttle of 0.2 lowers it to about 359 W, so the last two caps are
// on the other side of a floor that ignores either.
func TestCapSaturatedUsesTheNodeModel(t *testing.T) {
	clearSimEnv(t)
	model := defaultBaseModelFromEnv()
	model.CPUPowerCurve = spec01203Curve
	const class = "cpu.compute_bound"
	minScale := minFreqScale(model)
	curve := phys.MeasuredCurveCPUModel{Points: model.CPUPowerCurve, Knee: cpuKneeFromModel(model)}
	bareFloorW := curve.Power(phys.DeviceState{Utilization: 1, FreqScale: minScale, Class: class})
	if math.Abs(bareFloorW-390.1) > 0.5 {
		t.Fatalf("test setup: curve power at util 1 and the minimum scale %.3f is %.2f W, want 390.1 W by hand", minScale, bareFloorW)
	}

	cases := []struct {
		name         string
		capW         float64
		mem, thermal float64
		saturated    bool
	}{
		{"cap below the curve at minimum scale", 300, 0, 0, true},
		{"cap above the curve at minimum scale", 400, 0, 0, false},
		{"memory intensity raises the floor above the cap", 420, 0.8, 0, true},
		{"thermal throttle lowers the floor below the cap", 375, 0, 0.2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			floorW := curve.Power(phys.DeviceState{Utilization: 1, FreqScale: minScale, Class: class, MemoryIntensity: tc.mem, ThermalThrottle: tc.thermal})
			if (floorW > tc.capW) != tc.saturated {
				t.Fatalf("test setup: curve power at the minimum scale %.3f is %.1f W, which does not put the %.0f W cap on the intended side", minScale, floorW, tc.capW)
			}
			if (tc.mem != 0 || tc.thermal != 0) && (bareFloorW > tc.capW) == tc.saturated {
				t.Fatalf("test setup: the floor without memory and thermal, %.1f W, is on the same side of the %.0f W cap, so the case cannot tell them apart", bareFloorW, tc.capW)
			}
			st := &nodeState{CPUUtil: 1, FreqScale: 1, CapWatts: tc.capW, CPUWorkClass: class, MemoryIntensity: tc.mem, CPUThermalThrottle: tc.thermal}
			power := cpuPowerWithModel(st, model)
			if st.CapSaturated != tc.saturated || st.FreqScale >= 1 || (tc.saturated && st.FreqScale != minScale) {
				t.Fatalf("D13: util 1, mem %.1f, thermal %.1f under a %.0f W cap, curve power %.1f W at the minimum scale %.3f: capSaturated=%v scale=%.4f power=%.2f W; want capSaturated=%v and a scale below 1 (the minimum when saturated)",
					tc.mem, tc.thermal, tc.capW, floorW, minScale, st.CapSaturated, st.FreqScale, power, tc.saturated)
			}
		})
	}
}

// cpupowerGoldenModel is the analytic model of the golden: the code defaults
// of defaultBaseModelFromEnv, written out so that later default changes do not
// move the golden.
func cpupowerGoldenModel() simModel {
	return simModel{
		BaseIdleW:   80,
		PMaxW:       420,
		AlphaUtil:   1.15,
		BetaFreq:    1.35,
		FMinMHz:     1200,
		FMaxMHz:     3200,
		RaplCapMinW: 80,
		RaplCapMaxW: 5000,
		RaplHeadW:   5,
	}
}

type cpupowerGoldenModelJSON struct {
	BaseIdleW   float64 `json:"baseIdleW"`
	PMaxW       float64 `json:"pMaxW"`
	AlphaUtil   float64 `json:"alphaUtil"`
	BetaFreq    float64 `json:"betaFreq"`
	FMinMHz     float64 `json:"fMinMHz"`
	FMaxMHz     float64 `json:"fMaxMHz"`
	RaplCapMinW float64 `json:"raplCapMinW"`
	RaplCapMaxW float64 `json:"raplCapMaxW"`
	RaplHeadW   float64 `json:"raplHeadW"`
}

type cpupowerGoldenCase struct {
	Util              float64 `json:"util"`
	CapW              float64 `json:"capW"`
	MemoryIntensity   float64 `json:"memoryIntensity"`
	IOIntensity       float64 `json:"ioIntensity"`
	ThermalThrottle   float64 `json:"thermalThrottle"`
	StartScale        float64 `json:"startScale"`
	TargetThrottlePct int     `json:"targetThrottlePct"`
	PowerW            float64 `json:"powerW"`
	FreqScale         float64 `json:"freqScale"`
	CapSaturated      bool    `json:"capSaturated"`
}

type cpupowerGolden struct {
	Description string                  `json:"description"`
	Model       cpupowerGoldenModelJSON `json:"model"`
	Cases       []cpupowerGoldenCase    `json:"cases"`
}

// cpupowerGoldenCases evaluates cpuPowerWithModel over the grid. The lowest cap
// is 100 W, the only one where the power at the minimum scale exceeds cap plus
// headroom (util 0.7 and 1). Util 0.3 under it with thermal throttle 0.5,
// starting at scale 1 or 0.6, are the two points where the thermal-free
// minimum power (102.7 W) and a thermal-inclusive one (84.7 W) fall on either
// side of the cap; thermal throttle 1 adds more. Memory and IO intensity each
// lower the activity on their own. The last cases set TargetThrottlePct,
// which plays no part since the C-state reduction below util 0.2 was
// removed: util 0.1 draws 104 W, which a 100 W cap throttles to 100 W at
// scale 0.87, and with IO intensity 0.5 it draws 98 W, under the cap.
func cpupowerGoldenCases(model simModel) []cpupowerGoldenCase {
	var out []cpupowerGoldenCase
	eval := func(c cpupowerGoldenCase) {
		st := &nodeState{
			CPUUtil:            c.Util,
			FreqScale:          c.StartScale,
			CapWatts:           c.CapW,
			MemoryIntensity:    c.MemoryIntensity,
			IOIntensity:        c.IOIntensity,
			CPUThermalThrottle: c.ThermalThrottle,
			TargetThrottlePct:  c.TargetThrottlePct,
		}
		c.PowerW = cpuPowerWithModel(st, model)
		c.FreqScale = st.FreqScale
		c.CapSaturated = st.CapSaturated
		out = append(out, c)
	}
	for _, util := range []float64{0, 0.3, 0.7, 1} {
		for _, capW := range []float64{100, 200, 300, 400, 5000} {
			for _, act := range [][2]float64{{0, 0}, {0.5, 0}, {0, 0.5}} {
				for _, thermal := range []float64{0, 0.5, 1} {
					for _, start := range []float64{1, 0.6, 0.375} {
						eval(cpupowerGoldenCase{Util: util, CapW: capW, MemoryIntensity: act[0], IOIntensity: act[1], ThermalThrottle: thermal, StartScale: start})
					}
				}
			}
		}
	}
	for _, util := range []float64{0, 0.1} {
		for _, capW := range []float64{100, 5000} {
			for _, io := range []float64{0, 0.5} {
				eval(cpupowerGoldenCase{Util: util, CapW: capW, IOIntensity: io, StartScale: 1, TargetThrottlePct: 20})
			}
		}
	}
	return out
}

// cpupowerMinScalePower is the analytic power at the minimum scale with the
// thermal factor applied, which a thermal-inclusive CapSaturated would use.
func cpupowerMinScalePower(model simModel, c cpupowerGoldenCase) float64 {
	activity := math.Max(0.35, math.Min(1, 1-0.30*c.MemoryIntensity-0.45*c.IOIntensity))
	p := model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(clamp01(c.Util*activity), model.AlphaUtil)*math.Pow(minFreqScale(model), model.BetaFreq)
	return p * (1 - 0.35*c.ThermalThrottle)
}

// cpupowerEncodeGolden writes one case per line, so that a diff of the golden
// names the cases that moved.
func cpupowerEncodeGolden(g cpupowerGolden) ([]byte, error) {
	desc, err := json.Marshal(g.Description)
	if err != nil {
		return nil, err
	}
	model, err := json.Marshal(g.Model)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"description\": %s,\n  \"model\": %s,\n  \"cases\": [\n", desc, model)
	for i, c := range g.Cases {
		line, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		sep := ","
		if i == len(g.Cases)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    %s%s\n", line, sep)
	}
	b.WriteString("  ]\n}\n")
	return []byte(b.String()), nil
}

// cpupowerClose compares at a relative 1e-9, so that a platform that fuses
// multiply-adds still matches a golden written on amd64. On amd64 the
// golden regenerates byte for byte.
func cpupowerClose(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

// The analytic branch keeps its closed-form solve and its thermal-free
// CapSaturated: its power, scale and saturation over the grid equal the
// golden (cpupowerGoldenPath).
func TestAnalyticCPUPowerMatchesGolden(t *testing.T) {
	model := cpupowerGoldenModel()
	got := cpupowerGolden{
		Description: "cpuPowerWithModel for the analytic CPU model (no curve), one call per case from a fresh node state, with TargetThrottlePct 0 except in the last cases. Regenerate with: go test ./simulator/cmd/simulator/ -run AnalyticCPUPower -update",
		Model: cpupowerGoldenModelJSON{
			BaseIdleW: model.BaseIdleW, PMaxW: model.PMaxW, AlphaUtil: model.AlphaUtil, BetaFreq: model.BetaFreq,
			FMinMHz: model.FMinMHz, FMaxMHz: model.FMaxMHz,
			RaplCapMinW: model.RaplCapMinW, RaplCapMaxW: model.RaplCapMaxW, RaplHeadW: model.RaplHeadW,
		},
		Cases: cpupowerGoldenCases(model),
	}
	if *updateGoldens {
		b, err := cpupowerEncodeGolden(got)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(cpupowerGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cpupowerGoldenPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d cases)", cpupowerGoldenPath, len(got.Cases))
	}

	raw, err := os.ReadFile(cpupowerGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with -update)", err)
	}
	var want cpupowerGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode %s: %v", cpupowerGoldenPath, err)
	}
	if want.Model != got.Model || len(want.Cases) != len(got.Cases) {
		t.Fatalf("golden model or grid differs from the test (model %+v, %d cases; test %+v, %d cases); regenerate with -update", want.Model, len(want.Cases), got.Model, len(got.Cases))
	}

	flips := 0
	var diffs []string
	for i, w := range want.Cases {
		g := got.Cases[i]
		if w.Util != g.Util || w.CapW != g.CapW || w.MemoryIntensity != g.MemoryIntensity || w.IOIntensity != g.IOIntensity || w.ThermalThrottle != g.ThermalThrottle || w.StartScale != g.StartScale || w.TargetThrottlePct != g.TargetThrottlePct {
			t.Fatalf("case %d: golden inputs %+v differ from the grid %+v; regenerate with -update", i, w, g)
		}
		if !cpupowerClose(g.PowerW, w.PowerW) || !cpupowerClose(g.FreqScale, w.FreqScale) || g.CapSaturated != w.CapSaturated {
			diffs = append(diffs, fmt.Sprintf("| %.1f | %.0f | %.1f | %.1f | %.1f | %.3f | %d | %.4f / %.4f | %.4f / %.4f | %v / %v |",
				w.Util, w.CapW, w.MemoryIntensity, w.IOIntensity, w.ThermalThrottle, w.StartScale, w.TargetThrottlePct, g.PowerW, w.PowerW, g.FreqScale, w.FreqScale, g.CapSaturated, w.CapSaturated))
		}
		if w.CapSaturated && cpupowerMinScalePower(model, w) <= w.CapW {
			flips++
		}
	}
	if len(diffs) > 0 {
		t.Errorf("analytic CPU power moved at %d of %d cases (got / golden):\n| util | cap W | mem | io | thermal | start | target pct | power W | scale | saturated |\n|---|---|---|---|---|---|---|---|---|---|\n%s",
			len(diffs), len(want.Cases), strings.Join(diffs, "\n"))
	}
	// The golden must hold the points where only the thermal-free minimum
	// power marks the cap saturated, or it cannot tell the two apart.
	if flips < 2 {
		t.Errorf("the grid has %d cases where a thermal-inclusive CapSaturated would flip, want at least 2", flips)
	}
}
