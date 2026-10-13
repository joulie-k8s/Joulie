package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"k8s.io/client-go/rest"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return PromResult{}, err
	}
	return ParsePromResponse(body, resp.StatusCode)
}

// ParsePromResponse decodes a Prometheus API response body: an error status
// or error body is an error naming Prometheus' errorType and error.
func ParsePromResponse(body []byte, status int) (PromResult, error) {
	var r struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return PromResult{}, fmt.Errorf("prometheus: HTTP %d: %w", status, err)
	}
	if r.Status != "success" {
		return PromResult{}, fmt.Errorf("prometheus: HTTP %d: %s: %s", status, r.ErrorType, r.Error)
	}
	out := PromResult{Type: r.Data.ResultType}
	switch out.Type {
	case "scalar":
		var pair []json.RawMessage
		if err := json.Unmarshal(r.Data.Result, &pair); err != nil {
			return out, err
		}
		s, err := promSample(pair)
		if err != nil {
			return out, err
		}
		out.Series = []PromSeries{{Labels: map[string]string{}, Samples: []PromSample{s}}}
	case "vector", "matrix":
		var series []struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(r.Data.Result, &series); err != nil {
			return out, err
		}
		for _, s := range series {
			ps := PromSeries{Labels: s.Metric}
			pairs := s.Values
			if s.Value != nil {
				pairs = [][]json.RawMessage{s.Value}
			}
			for _, pair := range pairs {
				sample, err := promSample(pair)
				if err != nil {
					return out, err
				}
				ps.Samples = append(ps.Samples, sample)
			}
			out.Series = append(out.Series, ps)
		}
	default:
		return out, fmt.Errorf("prometheus: result type %q", out.Type)
	}
	return out, nil
}

// promSample decodes [<unix seconds>, "<value>"].
func promSample(pair []json.RawMessage) (PromSample, error) {
	if len(pair) != 2 {
		return PromSample{}, fmt.Errorf("prometheus: sample %v is not [time, value]", pair)
	}
	var ts float64
	var text string
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return PromSample{}, err
	}
	if err := json.Unmarshal(pair[1], &text); err != nil {
		return PromSample{}, err
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return PromSample{}, err
	}
	sec, frac := math.Modf(ts)
	return PromSample{Time: time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC(), Value: v}, nil
}

// promNameLabel is the metric name label, which a capture's own name
// replaces.
const promNameLabel = "__name__"

// RangeToGauge re-encodes a query result as the OTel Gauge name: one point
// per sample, with the series' labels (except __name__) as attributes,
// sorted by key. A sample that is NaN or infinite has no JSON form and is
// left out. The labels keep their Prometheus spelling, since the query
// chose them.
func RangeToGauge(name, unit string, r PromResult) otlpjson.Metric {
	g := &otlpjson.Gauge{DataPoints: []otlpjson.NumberDataPoint{}}
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
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			v := p.Value
			g.DataPoints = append(g.DataPoints, otlpjson.NumberDataPoint{
				Attributes: attrs, TimeUnixNano: otlpjson.Uint64String(p.Time.UnixNano()), AsDouble: &v,
			})
		}
	}
	return otlpjson.Metric{Name: name, Unit: unit, Gauge: g}
}
