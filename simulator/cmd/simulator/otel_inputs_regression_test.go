package main

// Regression tests for the fake Prometheus: the controller manager's otel
// facility preset reads the zone ambient and node inlet temperatures, which
// the legacy model does not have, and the old substring rule ("ambient" and
// "temp") answered both with the facility's outdoor ambient temperature, so
// every zone and every node inlet read the outdoor air. The queries are
// written out as the default preset spells them (otel preset, default
// translation strategy, 5m window), so the tests compile against the old
// code; TestRegressionQueriesAreTheControllerQueries keeps them in step.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	regressionZoneAmbientQuery = `avg by (joulie_cooling_zone_name) (joulie_cooling_zone_ambient_temperature_celsius)`
	regressionNodeInletQuery   = `max by (k8s_node_name) (hw_host_ambient_temperature_celsius{k8s_node_name!=""} or hw_host_ambient_temperature_celsius{k8s_node_name=""} * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) hw_host_ambient_temperature_celsius{k8s_node_name=""}))`
)

// regressionFakeQuery runs query against a simulator whose facility ambient
// temperature is 31.5 C and returns the number of series in the answer.
func regressionFakeQuery(t *testing.T, query string) int {
	t.Helper()
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityAmbientTemp.Set(31.5)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+url.Values{"query": {query}}.Encode(), nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)
	return fakeQuerySeries(t, w)
}

// fakeQuerySeries is the number of series in the fake Prometheus answer w,
// which must be a successful instant vector.
func fakeQuerySeries(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "vector" {
		t.Fatalf("status=%q resultType=%q, want success and vector", resp.Status, resp.Data.ResultType)
	}
	return len(resp.Data.Result)
}

func TestFakePrometheusLeavesNodeInletUnanswered(t *testing.T) {
	if n := regressionFakeQuery(t, regressionNodeInletQuery); n != 0 {
		t.Fatalf("the node inlet query got %d series, want an empty vector: the legacy model has no inlet sensors, and the facility ambient is not a node's inlet", n)
	}
}

func TestFakePrometheusLeavesZoneAmbientUnanswered(t *testing.T) {
	if n := regressionFakeQuery(t, regressionZoneAmbientQuery); n != 0 {
		t.Fatalf("the zone ambient query got %d series, want an empty vector: the legacy model has no cooling zones, and the facility ambient is not a zone's", n)
	}
}
