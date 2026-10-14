package otlpjson

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrSignalDisabled is returned by an export of a signal that has no
// endpoint.
var ErrSignalDisabled = errors.New("otlpjson: the signal has no endpoint")

// Retry schedule of an export: at most exportMaxAttempts attempts, waiting
// exportBackoffInitial, then twice as long each time up to exportBackoffMax,
// unless the server sends Retry-After. The whole export, waits included,
// stays within the signal's timeout.
const (
	exportMaxAttempts    = 5
	exportBackoffInitial = time.Second
	exportBackoffMax     = 8 * time.Second
	exportBodyLimit      = 64 << 10
	exportUserAgent      = "joulie-otlpjson"
)

// Exporter POSTs OTLP JSON to an OTLP/HTTP receiver: a Collector, or
// Prometheus at /api/v1/otlp/v1/metrics. It is safe for concurrent use.
//
// It follows the OTLP/HTTP specification (opentelemetry-proto v1.11.1,
// docs/specification.md, "OTLP/HTTP Throttling" and "Failures"): 429, 502,
// 503 and 504 and a lost connection are retried, honouring Retry-After;
// every other status fails at once. That includes 400, which Prometheus
// answers for an out-of-order sample, and 500, which it answers for a
// delta temporality it was not configured to convert.
type Exporter struct {
	cfg    ExporterConfig
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

// NewExporter checks the endpoints of c and returns its Exporter.
func NewExporter(c ExporterConfig) (*Exporter, error) {
	var errs []error
	for _, s := range []Signal{SignalMetrics, SignalLogs} {
		sc := c.Signal(s)
		if sc.Endpoint != "" {
			if err := envCheckURL(string(s)+" endpoint", sc.Endpoint); err != nil {
				errs = append(errs, err)
			}
		}
		if sc.Compression != "" && sc.Compression != "gzip" {
			errs = append(errs, fmt.Errorf("otlpjson: %s compression %q, want gzip or none", s, sc.Compression))
		}
		if sc.Timeout < 0 {
			errs = append(errs, fmt.Errorf("otlpjson: %s timeout %s is negative", s, sc.Timeout))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Exporter{cfg: c, client: client, sleep: exportSleep}, nil
}

// Enabled reports whether signal s has an endpoint.
func (e *Exporter) Enabled(s Signal) bool {
	return e != nil && e.cfg.Signal(s).Endpoint != ""
}

// Endpoint is the URL signal s is POSTed to, empty when it is not pushed.
func (e *Exporter) Endpoint(s Signal) string {
	if e == nil {
		return ""
	}
	return e.cfg.Signal(s).Endpoint
}

// ExportMetrics POSTs d to the metrics endpoint.
func (e *Exporter) ExportMetrics(ctx context.Context, d MetricsData) error {
	return e.export(ctx, SignalMetrics, d)
}

// ExportLogs POSTs d to the logs endpoint.
func (e *Exporter) ExportLogs(ctx context.Context, d LogsData) error {
	return e.export(ctx, SignalLogs, d)
}

// HTTPError is a response outside 2xx. Body holds the start of the
// response body, which is where Prometheus explains a rejection.
type HTTPError struct {
	Signal     Signal
	URL        string
	StatusCode int
	Body       string
	Attempts   int
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("otlpjson: %s export to %s: HTTP %d after %d attempt(s)", e.Signal, e.URL, e.StatusCode, e.Attempts)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// PartialSuccessError is a 2xx response that rejected part of the export
// (ExportMetricsPartialSuccess, ExportLogsPartialSuccess). It is not
// retried; the rest of the export was accepted.
type PartialSuccessError struct {
	Signal   Signal
	Rejected int64
	Message  string
}

func (e *PartialSuccessError) Error() string {
	return fmt.Sprintf("otlpjson: %s export partially rejected: %d rejected: %s", e.Signal, e.Rejected, e.Message)
}

func (e *Exporter) export(ctx context.Context, s Signal, v any) error {
	if !e.Enabled(s) {
		return ErrSignalDisabled
	}
	sc := e.cfg.Signal(s)
	body, err := exportBody(v, sc.Compression == "gzip")
	if err != nil {
		return fmt.Errorf("otlpjson: encode %s: %w", s, err)
	}
	timeout := sc.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	backoff := exportBackoffInitial
	for attempt := 1; ; attempt++ {
		status, header, respBody, err := e.post(ctx, sc, body)
		var last error
		wait := backoff
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return fmt.Errorf("otlpjson: %s export to %s after %d attempt(s): %w", s, sc.Endpoint, attempt, err)
			}
			last = fmt.Errorf("otlpjson: %s export to %s after %d attempt(s): %w", s, sc.Endpoint, attempt, err)
		case status >= 200 && status < 300:
			return exportPartialSuccess(s, respBody)
		default:
			herr := &HTTPError{Signal: s, URL: sc.Endpoint, StatusCode: status, Body: exportTrimBody(respBody), Attempts: attempt}
			if !exportRetryable(status) {
				return herr
			}
			last = herr
			if d, ok := exportRetryAfter(header.Get("Retry-After"), time.Now()); ok {
				wait = d
			}
		}
		if attempt >= exportMaxAttempts {
			return last
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return last
		}
		if e.sleep(ctx, wait) != nil {
			return last
		}
		if backoff *= 2; backoff > exportBackoffMax {
			backoff = exportBackoffMax
		}
	}
}

// post sends one attempt and returns the status, headers and the start of
// the body; the rest of the body is drained so the connection is reused.
func (e *Exporter) post(ctx context.Context, sc SignalConfig, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sc.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range sc.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if sc.Compression == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", exportUserAgent)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, exportBodyLimit))
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header, b, nil
}

// exportBody marshals v as one JSON object, gzipped when asked.
func exportBody(v any, gz bool) ([]byte, error) {
	b, err := fileMarshalLine(v)
	if err != nil || !gz {
		return b, err
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// exportRetryable lists the statuses the OTLP/HTTP specification calls
// retryable.
func exportRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// exportRetryAfter reads a Retry-After header: delay seconds or an HTTP
// date (RFC 9110, section 10.2.3).
func exportRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// exportPartialSuccess reads the partialSuccess field of an Export
// response. An empty or non-JSON body, which Prometheus may send, is a full
// success.
func exportPartialSuccess(s Signal, body []byte) error {
	var resp struct {
		PartialSuccess *struct {
			RejectedDataPoints Int64String `json:"rejectedDataPoints"`
			RejectedLogRecords Int64String `json:"rejectedLogRecords"`
			ErrorMessage       string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &resp) != nil || resp.PartialSuccess == nil {
		return nil
	}
	p := resp.PartialSuccess
	rejected := int64(p.RejectedDataPoints) + int64(p.RejectedLogRecords)
	if rejected == 0 && p.ErrorMessage == "" {
		return nil
	}
	return &PartialSuccessError{Signal: s, Rejected: rejected, Message: p.ErrorMessage}
}

// exportTrimBody keeps the first 512 bytes of a response body, with its
// whitespace collapsed to single spaces so the error stays on one line.
func exportTrimBody(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}

func exportSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
