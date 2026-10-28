package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// capFloorModel is a two-socket node with the cTDP range of an EPYC 9655
// (320 to 400 W per socket), whose caps the simulator accepts down to
// RaplCapMinW, 80 W, with four GPUs that accept 200 to 400 W each.
func capFloorModel() simModel {
	m := simModel{
		BaseIdleW: 100, RaplHeadW: 5, PMaxW: 900, DefaultCapW: 5000,
		RaplCapMinW: 80, RaplCapMaxW: 5000, CPUCapApplyTauMS: 1,
		CPUSockets: 2, CPUSocketCapMaxW: 400,
		AlphaUtil: 1.15, BetaFreq: 1.35, FMinMHz: 1200, FMaxMHz: 3200,
	}
	m.GPU.Count, m.GPU.MinCapWattsPerGPU, m.GPU.MaxWattsPerGPU, m.GPU.IdleWattsPerGPU = 4, 200, 400, 30
	return m
}

func capFloorControl(t *testing.T, s *simulator, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleControl(w, httptest.NewRequest(http.MethodPost, "/control/node-a", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("control status %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A CPU or GPU cap below the lowest one the simulator accepts is refused
// with an error that names the floor, and the node keeps its target; a cap
// at or above it is applied, a CPU cap below the cTDP range included. The
// simulator used to raise such a cap to the floor in silence, so a command
// it changed looked applied.
func TestCapBelowThePhysicalFloorIsRefused(t *testing.T) {
	s := newSimulatorWithRegisterer(capFloorModel(), nil, nil, 200, prometheus.NewRegistry())

	got := capFloorControl(t, s, `{"action":"rapl.set_power_cap_watts","capWatts":50}`)
	if got["ok"] != false || got["result"] != "error" || !strings.Contains(got["message"].(string), "80 W") {
		t.Errorf("a 50 W cap on an 80 W floor: %v", got)
	}
	if target := s.getNode("node-a").TargetCapWatts; target == 50 || target == 80 {
		t.Errorf("the refused cap set the target to %v", target)
	}
	// Below the cTDP range (640 W for the node) is a cap the CPU accepts.
	if got := capFloorControl(t, s, `{"action":"rapl.set_power_cap_watts","capWatts":220}`); got["ok"] != true {
		t.Errorf("a 220 W cap: %v", got)
	}
	if target := s.getNode("node-a").TargetCapWatts; target != 220 {
		t.Errorf("target %v after a 220 W cap", target)
	}

	got = capFloorControl(t, s, `{"action":"gpu.set_power_cap_watts","capWattsPerGpu":150}`)
	if got["ok"] != false || got["result"] != "error" || !strings.Contains(got["message"].(string), "200 W") {
		t.Errorf("a 150 W GPU cap on a 200 W floor: %v", got)
	}
	if target := s.getNode("node-a").GPUTargetCapWattsPerGpu; target == 150 || target == 200 {
		t.Errorf("the refused GPU cap set the target to %v", target)
	}
	if got := capFloorControl(t, s, `{"action":"gpu.set_power_cap_watts","capWattsPerGpu":300}`); got["ok"] != true {
		t.Errorf("a 300 W GPU cap: %v", got)
	}
}

// Whatever path sets a CPU cap below the floor (standalone mode, a default),
// the power is computed under the floor, never under the lower cap.
func TestCPUPowerNeverAppliesACapBelowTheFloor(t *testing.T) {
	m := capFloorModel()
	below := &nodeState{CPUUtil: 1, CapWatts: 20}
	atFloor := &nodeState{CPUUtil: 1, CapWatts: 80}
	pb, _, _ := cpuPower(below, m, 1)
	pf, _, _ := cpuPower(atFloor, m, 1)
	if pb != pf {
		t.Errorf("power under a 20 W cap %v, under the 80 W floor %v: want equal", pb, pf)
	}
	if got := cpuCapFloorW(m); got != 80 {
		t.Errorf("floor %v, want RaplCapMinW 80, not the cTDP range", got)
	}
}
