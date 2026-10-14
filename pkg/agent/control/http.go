// Package control provides HTTP-based control and telemetry clients for the
// Joulie agent. These clients communicate with external telemetry/control
// endpoints when host-level sysfs access is not available (e.g. in VM-based
// or cloud environments).
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// HTTPControlClient sends power cap control commands to an external endpoint.
type HTTPControlClient struct {
	Endpoint string
	NodeName string
	Client   *http.Client
}

// HTTPPowerReader reads power telemetry from an external HTTP endpoint.
//
// The endpoint answers with one OTLP JSON ExportMetricsServiceRequest, such
// as the simulator's /otlp/telemetry/{node}, and the reader returns the power
// of the node's CPU packages (hw.type=cpu): the RAPL path it stands in for
// measures packages only, so GPU power never enters it. Any other JSON
// object is the legacy packagePowerWatts JSON, read until
// otlpjson.LegacyRemovedIn with one deprecation warning per endpoint and
// node (otlpjson.ReadPower).
//
// The agent builds a new reader on every reconcile, so what must outlive one
// read, the previous energy points and the warning, is kept per Endpoint and
// NodeName for the life of the process: readers with the same two fields
// share it.
type HTTPPowerReader struct {
	Endpoint string
	NodeName string
	Client   *http.Client
}

// httpPowerKey identifies the readers that share an httpPowerState.
type httpPowerKey struct{ endpoint, node string }

// httpPowerState is what the readers of one endpoint and node keep between
// reads.
type httpPowerState struct {
	otlp       otlpjson.PowerReader // keeps the previous energy points
	legacyWarn sync.Once
}

// httpPowerStates holds an *httpPowerState per httpPowerKey. An agent reads
// one node from the endpoint of its telemetry profile, so it holds one entry
// per endpoint the profile has named.
var httpPowerStates sync.Map

func (h *HTTPPowerReader) state() *httpPowerState {
	k := httpPowerKey{endpoint: h.Endpoint, node: h.NodeName}
	if st, ok := httpPowerStates.Load(k); ok {
		return st.(*httpPowerState)
	}
	st, _ := httpPowerStates.LoadOrStore(k, &httpPowerState{})
	return st.(*httpPowerState)
}

// ReadPowerWatts fetches the current CPU package power from the telemetry
// endpoint. With an OTLP body it is not ok while a CPU energy series without
// a power gauge warms up (otlpjson.PowerReader.Watts).
func (h *HTTPPowerReader) ReadPowerWatts() (float64, bool, error) {
	body, err := otlpjson.FetchTelemetry(context.Background(), h.Client, strings.ReplaceAll(h.Endpoint, "{node}", h.NodeName))
	if err != nil {
		return 0, false, err
	}
	st := h.state()
	w, ok, legacy, err := otlpjson.ReadPower(body, &st.otlp, otlpjson.Selector{Node: h.NodeName, HwTypes: []string{semconv.HwTypeCPU}})
	if legacy {
		st.legacyWarn.Do(func() {
			log.Printf("warning: telemetry endpoint %s answered with the legacy packagePowerWatts JSON, which is deprecated and read until %s; serve an OTLP JSON metrics export with %s or %s points of %s=%s instead",
				h.Endpoint, otlpjson.LegacyRemovedIn, semconv.HwEnergy.Name, semconv.HwPower.Name, semconv.AttrHwType, semconv.HwTypeCPU)
		})
	}
	return w, ok, err
}

// ApplyCPUControl sends a CPU control command to the control endpoint.
func (h *HTTPControlClient) ApplyCPUControl(action string, capWatts float64, throttlePct int) error {
	url := strings.ReplaceAll(h.Endpoint, "{node}", h.NodeName)
	reqBody := map[string]any{
		"node":        h.NodeName,
		"action":      action,
		"capWatts":    capWatts,
		"throttlePct": throttlePct,
		"ts":          time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("control endpoint status=%d", resp.StatusCode)
	}
	return nil
}

// ApplyGPUControl sends a GPU control command to the control endpoint.
func (h *HTTPControlClient) ApplyGPUControl(action string, capWattsPerGPU float64) error {
	url := strings.ReplaceAll(h.Endpoint, "{node}", h.NodeName)
	reqBody := map[string]any{
		"node":           h.NodeName,
		"action":         action,
		"capWattsPerGpu": capWattsPerGPU,
		"ts":             time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("control endpoint status=%d", resp.StatusCode)
	}
	return nil
}
