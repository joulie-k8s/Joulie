package scenario_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// A range query's matrix is re-encoded as one OTel Gauge named after the
// capture: a point per sample with the series' labels as attributes
// (__name__ dropped), so the capture reads back from the harness file like
// any other gauge. A NaN sample, which JSON cannot carry, is left out.
func TestPromRangeReencodedAsGauge(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query().Encode()
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"__name__":"hw_power_watts","k8s_node_name":"intel-0","hw_type":"cpu"},"values":[[1759608000,"150.5"],[1759608015.5,"151"]]},
{"metric":{"k8s_node_name":"nvl-0"},"values":[[1759608000,"NaN"],[1759608015,"600"]]}]}}`))
	}))
	defer srv.Close()
	c := &scenario.PromClient{HTTP: srv.Client(), BaseURL: srv.URL}
	start := time.Unix(1759608000, 0)
	r, err := c.QueryRange(context.Background(), `sum by (k8s_node_name) (hw_power_watts)`, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"step=15", "start=1759608000.000", "end=1759608060.000", "query=sum+by"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %s lacks %s", gotQuery, want)
		}
	}
	m := scenario.RangeToGauge("joulie.capture.node.component_power", "W", r)
	if m.Name != "joulie.capture.node.component_power" || m.Unit != "W" || m.Gauge == nil || m.Sum != nil || len(m.Gauge.DataPoints) != 3 {
		t.Fatalf("metric %+v", m)
	}
	b, err := json.Marshal(otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{{ScopeMetrics: []otlpjson.ScopeMetrics{{Metrics: []otlpjson.Metric{m}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var md otlpjson.MetricsData
	if err := json.Unmarshal(b, &md); err != nil {
		t.Fatal(err)
	}
	points := scenario.FlattenMetrics(md, "harness")
	p := points[1]
	if p.Kind != semconv.Gauge || p.Value != 151 || !p.Time.Equal(time.Unix(1759608015, 500000000)) || len(p.Attributes) != 2 {
		t.Errorf("point %+v", p)
	}
	if v, _ := p.Attributes.String("k8s_node_name"); v != "intel-0" {
		t.Errorf("attributes %v", p.Attributes)
	}
	if _, ok := p.Attributes["__name__"]; ok {
		t.Errorf("__name__ kept: %v", p.Attributes)
	}
	if !strings.Contains(string(b), `"timeUnixNano":"1759608015500000000"`) || strings.Contains(string(b), "NaN") {
		t.Errorf("encoded %s", b)
	}
}

// Instant queries decode vectors and scalars, and an error answer is an
// error naming Prometheus' error.
func TestPromQueryResults(t *testing.T) {
	for _, c := range []struct {
		body   string
		status int
		typ    string
		series int
		err    string
	}{
		{`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"a":"b"},"value":[1759608000,"3"]}]}}`, 200, "vector", 1, ""},
		{`{"status":"success","data":{"resultType":"vector","result":[]}}`, 200, "vector", 0, ""},
		{`{"status":"success","data":{"resultType":"scalar","result":[1759608000,"2.5"]}}`, 200, "scalar", 1, ""},
		{`{"status":"error","errorType":"bad_data","error":"parse error"}`, 400, "", 0, "HTTP 400 bad_data: parse error"},
		{`<html>`, 502, "", 0, "HTTP 502"},
	} {
		r, err := scenario.ParsePromResponse([]byte(c.body), c.status)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: error %v, want %s", c.body, err, c.err)
			}
			continue
		}
		if err != nil || r.Type != c.typ || len(r.Series) != c.series {
			t.Errorf("%s: %+v %v", c.body, r, err)
		}
	}
}
