package otlpjson

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// exportTestServer answers each POST with the next status of statuses
// (the last one repeats) and records every request.
type exportTestServer struct {
	mu       sync.Mutex
	statuses []int
	header   http.Header
	body     string
	reqs     []exportTestRequest
}

type exportTestRequest struct {
	path    string
	header  http.Header
	payload []byte
}

func (s *exportTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rd = zr
	}
	b, _ := io.ReadAll(rd)
	s.mu.Lock()
	s.reqs = append(s.reqs, exportTestRequest{path: r.URL.Path, header: r.Header.Clone(), payload: b})
	status := s.statuses[min(len(s.reqs), len(s.statuses))-1]
	for k, v := range s.header {
		w.Header()[k] = v
	}
	body := s.body
	s.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *exportTestServer) requests() []exportTestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]exportTestRequest(nil), s.reqs...)
}

// exportTestExporter returns an exporter for srv whose waits are recorded
// instead of slept.
func exportTestExporter(t *testing.T, srv *httptest.Server, gz bool) (*Exporter, *[]time.Duration) {
	t.Helper()
	compression := ""
	if gz {
		compression = "gzip"
	}
	e, err := NewExporter(ExporterConfig{
		Metrics: SignalConfig{Endpoint: srv.URL + "/api/v1/otlp/v1/metrics", Compression: compression, Headers: map[string]string{"X-Tenant": "t1"}},
		Logs:    SignalConfig{Endpoint: srv.URL + "/v1/logs", Compression: compression},
	})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	e.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	return e, &waits
}

func exportTestMetrics() MetricsData {
	return MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: []KeyValue{String("k8s.node.name", "node-a")}},
		ScopeMetrics: []ScopeMetrics{{Scope: Scope{Name: "s"}, Metrics: []Metric{
			{Name: "p", Unit: "W", Gauge: &Gauge{DataPoints: []NumberDataPoint{{TimeUnixNano: 1, AsDouble: otlpF(150)}}}},
		}}},
	}}}
}

// OJ05: the request carries Content-Type application/json, the configured
// headers and, when asked, a gzip body; 503 with Retry-After: 1 is retried
// after one second and then succeeds, and any Retry-After replaces the
// backoff; 400 and 500 fail after one attempt; 429, 502 and 504 are
// retried.
func TestOJ05ExporterTransportAndRetries(t *testing.T) {
	for _, gz := range []bool{false, true} {
		h := &exportTestServer{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}, header: http.Header{"Retry-After": {"1"}}}
		srv := httptest.NewServer(h)
		e, waits := exportTestExporter(t, srv, gz)
		if err := e.ExportMetrics(context.Background(), exportTestMetrics()); err != nil {
			t.Fatalf("gzip=%v: ExportMetrics after a 503 = %v", gz, err)
		}
		reqs := h.requests()
		if len(reqs) != 2 || len(*waits) != 1 || (*waits)[0] != time.Second {
			t.Fatalf("gzip=%v: %d requests and waits %v, want 2 requests and one 1 s wait", gz, len(reqs), *waits)
		}
		for _, r := range reqs {
			if r.path != "/api/v1/otlp/v1/metrics" || r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Tenant") != "t1" {
				t.Errorf("gzip=%v: request %s with headers %v", gz, r.path, r.header)
			}
			if got := r.header.Get("Content-Encoding"); (got == "gzip") != gz {
				t.Errorf("gzip=%v: Content-Encoding %q", gz, got)
			}
			d, err := DecodeMetricsStrict(r.payload)
			if err != nil || len(d.ResourceMetrics) != 1 {
				t.Errorf("gzip=%v: payload %s decoded as %+v, %v", gz, r.payload, d, err)
			}
		}
		srv.Close()
	}

	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusNotFound} {
		h := &exportTestServer{statuses: []int{status}, body: "out of order sample"}
		srv := httptest.NewServer(h)
		e, waits := exportTestExporter(t, srv, false)
		err := e.ExportLogs(context.Background(), LogsData{})
		var herr *HTTPError
		if !errors.As(err, &herr) || herr.StatusCode != status || herr.Signal != SignalLogs || herr.Body != "out of order sample" || herr.Attempts != 1 {
			t.Errorf("status %d: err = %#v", status, err)
		}
		if n := len(h.requests()); n != 1 || len(*waits) != 0 {
			t.Errorf("status %d: %d requests, waits %v; want 1 request and no retry", status, n, *waits)
		}
		srv.Close()
	}

	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusGatewayTimeout} {
		h := &exportTestServer{statuses: []int{status, status, http.StatusOK}}
		srv := httptest.NewServer(h)
		e, waits := exportTestExporter(t, srv, false)
		if err := e.ExportMetrics(context.Background(), exportTestMetrics()); err != nil {
			t.Errorf("status %d: %v", status, err)
		}
		if n := len(h.requests()); n != 3 || len(*waits) != 2 || (*waits)[0] != time.Second || (*waits)[1] != 2*time.Second {
			t.Errorf("status %d: %d requests, waits %v; want 3 requests after 1 s and 2 s", status, n, *waits)
		}
		srv.Close()
	}

	// Retry-After replaces the backoff, whether it is longer or shorter:
	// the default waits would be 1 s and then 2 s.
	for _, tc := range []struct {
		retryAfter string
		wait       time.Duration
	}{{"3", 3 * time.Second}, {"0", 0}} {
		h := &exportTestServer{statuses: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK}, header: http.Header{"Retry-After": {tc.retryAfter}}}
		srv := httptest.NewServer(h)
		e, waits := exportTestExporter(t, srv, false)
		if err := e.ExportMetrics(context.Background(), exportTestMetrics()); err != nil {
			t.Errorf("Retry-After %s: %v", tc.retryAfter, err)
		}
		if n := len(h.requests()); n != 3 || len(*waits) != 2 || (*waits)[0] != tc.wait || (*waits)[1] != tc.wait {
			t.Errorf("Retry-After %s: %d requests, waits %v; want 3 requests after %v each", tc.retryAfter, n, *waits, tc.wait)
		}
		srv.Close()
	}

	// A server that keeps answering 503 is given up on after five attempts.
	h := &exportTestServer{statuses: []int{http.StatusServiceUnavailable}}
	srv := httptest.NewServer(h)
	e, _ := exportTestExporter(t, srv, false)
	var herr *HTTPError
	if err := e.ExportMetrics(context.Background(), exportTestMetrics()); !errors.As(err, &herr) || herr.Attempts != exportMaxAttempts {
		t.Errorf("persistent 503: err = %v, want an HTTPError after %d attempts", err, exportMaxAttempts)
	}
	srv.Close()

	// A Retry-After longer than the time left gives up at once.
	h = &exportTestServer{statuses: []int{http.StatusServiceUnavailable}, header: http.Header{"Retry-After": {"60"}}}
	srv = httptest.NewServer(h)
	e, waits := exportTestExporter(t, srv, false)
	e.cfg.Metrics.Timeout = 5 * time.Second
	if err := e.ExportMetrics(context.Background(), exportTestMetrics()); !errors.As(err, &herr) || len(*waits) != 0 || len(h.requests()) != 1 {
		t.Errorf("Retry-After past the timeout: err %v, waits %v, %d requests", err, *waits, len(h.requests()))
	}
	srv.Close()
}

// A 200 that rejects points is a PartialSuccessError; an empty 200 body is
// a full success; a signal without an endpoint is refused.
func TestExporterPartialSuccessAndDisabledSignal(t *testing.T) {
	h := &exportTestServer{statuses: []int{http.StatusOK}, body: `{"partialSuccess":{"rejectedDataPoints":"3","errorMessage":"too old"}}`}
	srv := httptest.NewServer(h)
	defer srv.Close()
	e, _ := exportTestExporter(t, srv, false)
	var perr *PartialSuccessError
	if err := e.ExportMetrics(context.Background(), exportTestMetrics()); !errors.As(err, &perr) || perr.Rejected != 3 || perr.Message != "too old" {
		t.Errorf("err = %v, want a partial success rejecting 3", err)
	}
	h.body = `{}`
	if err := e.ExportMetrics(context.Background(), exportTestMetrics()); err != nil {
		t.Errorf("err = %v on an empty response", err)
	}

	only, err := NewExporter(ExporterConfig{Metrics: SignalConfig{Endpoint: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if only.Enabled(SignalLogs) || !only.Enabled(SignalMetrics) {
		t.Error("Enabled does not follow the endpoints")
	}
	if err := only.ExportLogs(context.Background(), LogsData{}); !errors.Is(err, ErrSignalDisabled) {
		t.Errorf("ExportLogs without an endpoint = %v", err)
	}
	if _, err := NewExporter(ExporterConfig{Logs: SignalConfig{Endpoint: "localhost:4318"}}); err == nil {
		t.Error("NewExporter accepted an endpoint without a scheme")
	}
}

func TestExportRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"1", time.Second, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	} {
		got, ok := exportRetryAfter(tc.v, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("exportRetryAfter(%q) = %v, %v; want %v, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

// exportSleep, which the other tests replace, waits for its duration and
// returns early with the context's error when the export is cancelled.
func TestExportSleep(t *testing.T) {
	start := time.Now()
	if err := exportSleep(context.Background(), 20*time.Millisecond); err != nil || time.Since(start) < 20*time.Millisecond {
		t.Errorf("exportSleep(20ms) = %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- exportSleep(ctx, time.Hour) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("exportSleep(1h) cancelled = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("exportSleep(1h) did not return when its context was cancelled")
	}
}
