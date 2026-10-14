package main

// The regression tests of stepNode: legacy physics advances only in
// simulated time.

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Reads never advance the physics. Before stepNode every read stepped the
// node by the wall time since its last update, so how often a node was read
// changed what it did: 300 ms after the control below, a telemetry read
// snapped the 250 ms cap lag to its 200 W target, while the control's own
// response had shown the old cap. With nothing stepping the node in between,
// every read must report the cap the control response reported.
func TestReadsDoNotAdvancePhysics(t *testing.T) {
	s := newSimulatorWithRegisterer(simModel{
		BaseIdleW: 100, PMaxW: 400, AlphaUtil: 1, BetaFreq: 1, RaplHeadW: 5,
		DefaultCapW: 500, RaplCapMinW: 100, RaplCapMaxW: 500, CPUCapApplyTauMS: 250,
		CPUSockets: 2, CPUSocketCapMaxW: 250,
	}, nil, nil, 10, prometheus.NewRegistry())
	const node = "node-a"

	w := httptest.NewRecorder()
	s.handleControl(w, httptest.NewRequest(http.MethodPost, "/control/"+node, strings.NewReader(`{"action":"rapl.set_power_cap_watts","capWatts":200}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d", w.Code)
	}
	var control struct {
		State struct {
			CapWatts float64 `json:"capWatts"`
		} `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &control); err != nil {
		t.Fatalf("decode control: %v", err)
	}
	want := control.State.CapWatts
	if want <= 200 {
		t.Fatalf("control response cap %.3f W; a 250 ms lag cannot reach the 200 W target at once", want)
	}

	time.Sleep(300 * time.Millisecond)

	w = httptest.NewRecorder()
	s.handleTelemetry(w, httptest.NewRequest(http.MethodGet, "/telemetry/"+node+"?format=legacy", nil))
	var telemetry struct {
		CPU struct {
			CapWatts float64 `json:"capWatts"`
		} `json:"cpu"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &telemetry); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	if telemetry.CPU.CapWatts != want {
		t.Errorf("telemetry cap %.3f W 300 ms after the control, want the control's %.3f W: the read advanced the physics", telemetry.CPU.CapWatts, want)
	}

	w = httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest(http.MethodGet, "/state/"+node, nil))
	var state struct {
		State struct {
			CapWatts float64 `json:"capWatts"`
		} `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.State.CapWatts != want {
		t.Errorf("state cap %.3f W, want the control's %.3f W: the read advanced the physics", state.State.CapWatts, want)
	}
}

// Standalone mode simulates SIM_SA_DT seconds per tick at CPU speed. Before
// stepNode its energy was the node's lagging average power, stepped by the
// wall time between two reads (microseconds), times dt: the average barely
// left its 0 W start, so a node idling at P for 600 s booked almost nothing.
// The energy is the integral of the instantaneous power: P x 600 s.
func TestStandaloneEnergyIntegratesInstantPower(t *testing.T) {
	dir := t.TempDir()
	inventory := filepath.Join(dir, "inventory.yaml")
	if err := os.WriteFile(inventory, []byte("nodes:\n  - node_name_prefix: sa\n    replicas: 2\n    cpu: Generic CPU\n    cpu_sockets: 2\n    cpu_cores: 32\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace.jsonl")
	// One job that outlasts the run keeps one node busy; the other idles.
	if err := os.WriteFile(trace, []byte(`{"type":"job","jobId":"long","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"4"}},"work":{"cpuUnits":1000000000}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"SIM_STANDALONE_INVENTORY": inventory,
		"SIM_STANDALONE_BASELINE":  "A",
		"SIM_SA_DT":                "5",
		"SIM_SA_TIMEOUT":           "600",
		"SIM_SA_WORKERS":           "1",
		"SIM_SA_SAMPLE_EVERY":      "1",
		"SIM_BASE_SPEED_PER_CORE":  "1",
	} {
		t.Setenv(k, v)
	}
	model := simModel{
		BaseIdleW: 80, PMaxW: 420, AlphaUtil: 1.15, BetaFreq: 1.35, FMinMHz: 1200, FMaxMHz: 3200,
		RaplHeadW: 5, DefaultCapW: 5000, RaplCapMinW: 80, RaplCapMaxW: 5000,
		DvfsRampMS: 500, CPUCapApplyTauMS: 250, CPUTelemetryWindowMS: 250,
		CPUAmbientTempC: 24, CPUThermalTauMS: 4000, CPUWattsPerDeltaC: 8,
		CPULowestNonlinearFreqMHz: 1800, CPUSockets: 2,
	}
	s := newSimulatorWithRegisterer(model, nil, nil, 0, prometheus.NewRegistry())
	if err := s.initWorkloadEngineWithOptions(trace, workloadOptions{}); err != nil {
		t.Fatal(err)
	}
	runStandalone(s)

	idle, busy := "", ""
	for _, name := range []string{"sa-0", "sa-1"} {
		st := s.state[name]
		if st == nil {
			t.Fatalf("no state for %s", name)
		}
		if st.PodsRunning == 0 {
			idle = name
		} else {
			busy = name
		}
	}
	if idle == "" || busy == "" {
		t.Fatalf("idle %q, busy %q: want one node of each", idle, busy)
	}
	p := cpuPowerWithModel(&nodeState{FreqScale: 1, CapWatts: model.DefaultCapW, CPUWorkClass: "cpu.mixed"}, model)
	want := p * 600
	if got := s.energyJByNode[idle]; math.Abs(got-want) > 1e-9*want {
		t.Errorf("idle node %s booked %.3f J over 600 s, want %.3f J (%.2f W x 600 s)", idle, got, want, p)
	}
	if got := s.energyJByNode[busy]; got <= want {
		t.Errorf("busy node %s booked %.3f J, want more than the idle %.3f J", busy, got, want)
	}
}
