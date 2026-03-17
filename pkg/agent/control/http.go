// Package control provides HTTP-based control and telemetry clients for the
// Joulie agent. These clients communicate with external telemetry/control
// endpoints when host-level sysfs access is not available (e.g. in VM-based
// or cloud environments).
package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPControlClient sends power cap control commands to an external endpoint.
type HTTPControlClient struct {
	Endpoint string
	NodeName string
	Client   *http.Client
}

// HTTPPowerReader reads power telemetry from an external HTTP endpoint.
type HTTPPowerReader struct {
	Endpoint string
	NodeName string
	Client   *http.Client
}

// ReadPowerWatts fetches the current CPU package power from the telemetry endpoint.
func (h *HTTPPowerReader) ReadPowerWatts() (float64, bool, error) {
	url := strings.ReplaceAll(h.Endpoint, "{node}", h.NodeName)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, fmt.Errorf("telemetry endpoint status=%d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, false, err
	}
	if v, ok := extractFloat(payload, "packagePowerWatts"); ok {
		return v, true, nil
	}
	if cpuRaw, ok := payload["cpu"].(map[string]any); ok {
		if v, ok := extractFloat(cpuRaw, "packagePowerWatts"); ok {
			return v, true, nil
		}
	}
	return 0, false, nil
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

func extractFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case json.Number:
		f, err := val.Float64()
		return f, err == nil
	}
	return 0, false
}
