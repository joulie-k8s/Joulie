package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadPowerWattsTopLevel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"packagePowerWatts": 125.5})
	}))
	defer server.Close()

	reader := &HTTPPowerReader{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	watts, ok, err := reader.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for valid power reading")
	}
	if watts != 125.5 {
		t.Errorf("expected 125.5W, got %f", watts)
	}
}

func TestReadPowerWattsNestedCPU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"cpu": map[string]any{"packagePowerWatts": 200.0},
		})
	}))
	defer server.Close()

	reader := &HTTPPowerReader{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	watts, ok, err := reader.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if watts != 200.0 {
		t.Errorf("expected 200W, got %f", watts)
	}
}

func TestReadPowerWattsNoPowerField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"other": "data"})
	}))
	defer server.Close()

	reader := &HTTPPowerReader{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	_, ok, err := reader.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no power field present")
	}
}

func TestReadPowerWattsServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	reader := &HTTPPowerReader{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	_, _, err := reader.ReadPowerWatts()
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestReadPowerWattsNodeNameSubstitution(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"packagePowerWatts": 100.0})
	}))
	defer server.Close()

	reader := &HTTPPowerReader{
		Endpoint: server.URL + "/nodes/{node}/power",
		NodeName: "worker-1",
		Client:   server.Client(),
	}
	_, _, _ = reader.ReadPowerWatts()
	if receivedPath != "/nodes/worker-1/power" {
		t.Errorf("expected node substitution, got path %s", receivedPath)
	}
}

func TestApplyCPUControlSuccess(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &HTTPControlClient{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	err := client.ApplyCPUControl("rapl.set_power_cap_watts", 120.0, -1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received["action"] != "rapl.set_power_cap_watts" {
		t.Errorf("expected action rapl.set_power_cap_watts, got %v", received["action"])
	}
	if received["capWatts"] != 120.0 {
		t.Errorf("expected capWatts 120, got %v", received["capWatts"])
	}
}

func TestApplyCPUControlServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := &HTTPControlClient{
		Endpoint: server.URL,
		NodeName: "test-node",
		Client:   server.Client(),
	}
	err := client.ApplyCPUControl("rapl.set_power_cap_watts", 120.0, -1)
	if err == nil {
		t.Error("expected error for 400 response")
	}
}

func TestApplyGPUControlSuccess(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &HTTPControlClient{
		Endpoint: server.URL,
		NodeName: "gpu-node",
		Client:   server.Client(),
	}
	err := client.ApplyGPUControl("gpu.set_power_cap_watts", 250.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received["action"] != "gpu.set_power_cap_watts" {
		t.Errorf("expected action gpu.set_power_cap_watts, got %v", received["action"])
	}
	if received["capWattsPerGpu"] != 250.0 {
		t.Errorf("expected capWattsPerGpu 250, got %v", received["capWattsPerGpu"])
	}
}

func TestExtractFloat(t *testing.T) {
	tests := []struct {
		name   string
		val    any
		expect float64
		ok     bool
	}{
		{"float64", 42.5, 42.5, true},
		{"int", 42, 42.0, true},
		{"int64", int64(42), 42.0, true},
		{"json.Number", json.Number("42.5"), 42.5, true},
		{"string", "42", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{"key": tt.val}
			got, ok := extractFloat(m, "key")
			if ok != tt.ok {
				t.Errorf("extractFloat ok=%v, want %v", ok, tt.ok)
			}
			if ok && got != tt.expect {
				t.Errorf("extractFloat=%f, want %f", got, tt.expect)
			}
		})
	}
}

func TestExtractFloatMissingKey(t *testing.T) {
	m := map[string]any{"other": 42.0}
	_, ok := extractFloat(m, "missing")
	if ok {
		t.Error("expected ok=false for missing key")
	}
}
