package control

// Regression tests for the OTLP JSON HTTP telemetry input. They use only the
// API of the reader before it read OTLP, and literal bodies, so they compile
// against the old code and fail there.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// otlpCPUAndGPUPowerBody is one node's export with a CPU package at 150 W and
// a GPU at 400 W, as hw.power gauges.
const otlpCPUAndGPUPowerBody = `{"resourceMetrics":[{
  "resource":{"attributes":[
    {"key":"k8s.node.name","value":{"stringValue":"node-a"}},
    {"key":"service.instance.id","value":{"stringValue":"node-a"}}]},
  "scopeMetrics":[{"scope":{"name":"test"},"metrics":[
    {"name":"hw.power","unit":"W","gauge":{"dataPoints":[
      {"attributes":[{"key":"hw.id","value":{"stringValue":"cpu0"}},{"key":"hw.type","value":{"stringValue":"cpu"}},{"key":"hw.name","value":{"stringValue":"package-0"}}],
       "timeUnixNano":"1760000000000000000","asDouble":150},
      {"attributes":[{"key":"hw.id","value":{"stringValue":"gpu0"}},{"key":"hw.type","value":{"stringValue":"gpu"}},{"key":"hw.name","value":{"stringValue":"GPU"}}],
       "timeUnixNano":"1760000000000000000","asDouble":400}]}}]}]}]}`

// The agent reads an OTLP export and counts the CPU package only: the GPU's
// 400 W must not reach the RAPL power reading. The old reader looked only for
// packagePowerWatts and returned (0, false) on this body.
func TestHTTPPowerReaderOTLPExcludesGPUPower(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(otlpCPUAndGPUPowerBody))
	}))
	defer server.Close()

	reader := &HTTPPowerReader{Endpoint: server.URL + "/otlp/telemetry/{node}", NodeName: "node-a", Client: server.Client()}
	watts, ok, err := reader.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || watts != 150 {
		t.Fatalf("ReadPowerWatts = (%v, %v), want (150, true): the CPU package only, without the GPU", watts, ok)
	}
}
