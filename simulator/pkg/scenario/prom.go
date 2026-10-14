package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"k8s.io/client-go/rest"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// PromClient queries the Prometheus HTTP API (GET /api/v1/query and
// /api/v1/query_range) under BaseURL, such as the harness Prometheus
// through the API server's service proxy.
type PromClient struct {
	HTTP    *http.Client
	BaseURL string
}

// NewProxyPromClient returns a client of the harness Prometheus,
// prometheus-operated in monitoring, through the API server's proxy.
func NewProxyPromClient(cfg *rest.Config) (*PromClient, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &PromClient{HTTP: hc, BaseURL: ServiceProxyURL(cfg, NamespaceMonitoring, PrometheusService)}, nil
}

// PromResult is a query result: the series of a vector or matrix, each
// with its samples (one for a vector), or the one sample of a scalar.
type PromResult struct {
	Type   string
	Series []PromSeries
}

// PromSeries is one series: its labels and samples.
type PromSeries struct {
	Labels  map[string]string
	Samples []PromSample
}

// PromSample is one value at one time.
type PromSample struct {
	Time  time.Time
	Value float64
}

// Query runs an instant query at t.
func (c *PromClient) Query(ctx context.Context, query string, t time.Time) (PromResult, error) {
	v := url.Values{"query": {query}, "time": {promTime(t)}}
	return c.get(ctx, "/api/v1/query", v)
}

// QueryRange runs a range query from start to end every step.
func (c *PromClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (PromResult, error) {
	v := url.Values{"query": {query}, "start": {promTime(start)}, "end": {promTime(end)},
		"step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	return c.get(ctx, "/api/v1/query_range", v)
}

func promTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

func (c *PromClient) get(ctx context.Context, path string, v url.Values) (PromResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+v.Encode(), nil)
	if err != nil {
		return PromResult{}, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return PromResult{}, err
	}
	defer resp.Body.Close()
	body, err := inputs.ReadResponseBody(resp.Body)
	if err != nil {
		return PromResult{}, err
	}
	return ParsePromResponse(body, resp.StatusCode)
}

// ParsePromResponse decodes a Prometheus API response body with the
// controller manager's decoder (package inputs): an error status or error
// body is an *inputs.Error naming Prometheus' errorType and error; a vector
// or scalar is read as inputs reads an instant query, and a matrix series by
// series, each sample with inputs.ParseSample.
func ParsePromResponse(body []byte, status int) (PromResult, error) {
	r, err := inputs.DecodeResponse(status, body)
	if err != nil {
		return PromResult{}, err
	}
	out := PromResult{Type: r.ResultType}
	if r.ResultType == "matrix" {
		var series []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(r.Result, &series); err != nil {
			return out, fmt.Errorf("prometheus: decoding a matrix result: %w", err)
		}
		for _, s := range series {
			ps := PromSeries{Labels: s.Metric}
			for _, pair := range s.Values {
				t, v, err := inputs.ParseSample(pair)
				if err != nil {
					return out, err
				}
				ps.Samples = append(ps.Samples, PromSample{Time: t, Value: v})
			}
			out.Series = append(out.Series, ps)
		}
		return out, nil
	}
	res, err := r.Instant()
	if err != nil {
		return out, err
	}
	for _, s := range res.Samples {
		labels := s.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out.Series = append(out.Series, PromSeries{Labels: labels, Samples: []PromSample{{Time: s.Time, Value: s.Value}}})
	}
	return out, nil
}

// promNameLabel is the metric name label, which a capture's own name
// replaces.
const promNameLabel = "__name__"

// RangeToGauge re-encodes a query result as the OTel Gauge name: one point
// per sample, with the series' labels (except __name__) as attributes,
// sorted by key. A sample that is NaN or infinite has no JSON form and is
// left out (otlpjson.GaugeMetric). The labels keep their Prometheus
// spelling, since the query chose them.
func RangeToGauge(name, unit string, r PromResult) otlpjson.Metric {
	var points []otlpjson.NumberDataPoint
	for _, s := range r.Series {
		keys := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			if k != promNameLabel {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		attrs := make([]otlpjson.KeyValue, len(keys))
		for i, k := range keys {
			attrs[i] = otlpjson.String(k, s.Labels[k])
		}
		for _, p := range s.Samples {
			points = append(points, otlpjson.DoublePoint(time.Time{}, p.Time, p.Value, attrs...))
		}
	}
	return otlpjson.GaugeMetric(semconv.Metric{Name: name, Unit: unit}, points...)
}
