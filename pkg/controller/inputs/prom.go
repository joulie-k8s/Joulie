package inputs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes bounds one Prometheus answer a reader takes: a grouped
// instant query returns one series per node, a few hundred bytes each, and
// a range query over a scenario's run, which the simulator's scenario
// client reads through DecodeResponse, far more.
const MaxResponseBytes = 64 << 20

// errorExcerptBytes is how much of a body that is not JSON an Error keeps,
// such as the HTML page of a proxy in front of Prometheus.
const errorExcerptBytes = 200

// Query runs q at the current time (GET <Address>/api/v1/query) and returns
// a vector or scalar result with its warnings. A failed query is an *Error:
// a status other than 2xx, a body that is not the Prometheus JSON, or a
// response with status "error". A matrix or string result is an error too.
// Samples keep the value Prometheus returned, NaN and infinities included;
// ByLabel, Single and Sum reject them.
func (c Client) Query(ctx context.Context, q string) (Result, error) {
	if c.Address == "" {
		return Result{}, errors.New("inputs: no Prometheus address")
	}
	u := strings.TrimRight(c.Address, "/") + "/api/v1/query?" + url.Values{"query": {q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, fmt.Errorf("inputs: %w", err)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := ReadResponseBody(resp.Body)
	if err != nil {
		return Result{}, err
	}
	return ParseResponse(resp.StatusCode, body)
}

// ReadResponseBody reads a Prometheus answer, at most MaxResponseBytes.
func ReadResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("inputs: reading the Prometheus answer: %w", err)
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("inputs: Prometheus answer exceeds %d bytes", MaxResponseBytes)
	}
	return body, nil
}

// Response is a successful Prometheus HTTP API answer: its result type, its
// result as sent, and its warnings.
type Response struct {
	ResultType string
	Result     json.RawMessage
	Warnings   []string
}

// DecodeResponse reads the envelope of a Prometheus HTTP API answer with
// HTTP status status. A failed query is an *Error: a status other than 2xx,
// a body that is not the Prometheus JSON, or a response with status
// "error".
func DecodeResponse(status int, body []byte) (Response, error) {
	var r struct {
		Status    string   `json:"status"`
		ErrorType string   `json:"errorType"`
		Error     string   `json:"error"`
		Warnings  []string `json:"warnings"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Status == "" {
		return Response{}, &Error{Status: status, Message: excerpt(body)}
	}
	if status < 200 || status > 299 || r.Status != "success" {
		msg := r.Error
		if msg == "" {
			msg = excerpt(body)
		}
		return Response{}, &Error{Status: status, Type: r.ErrorType, Message: msg}
	}
	return Response{ResultType: r.Data.ResultType, Result: r.Data.Result, Warnings: r.Warnings}, nil
}

// ParseResponse reads the answer to an instant query (DecodeResponse, then
// Response.Instant).
func ParseResponse(status int, body []byte) (Result, error) {
	r, err := DecodeResponse(status, body)
	if err != nil {
		return Result{}, err
	}
	return r.Instant()
}

// Instant reads the vector or scalar result of an instant query; any other
// result type is an error.
func (r Response) Instant() (Result, error) {
	res := Result{Type: r.ResultType, Warnings: r.Warnings}
	switch r.ResultType {
	case "vector":
		var vec []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Result, &vec); err != nil {
			return Result{}, fmt.Errorf("inputs: decoding a vector result: %w", err)
		}
		res.Samples = make([]Sample, 0, len(vec))
		for _, s := range vec {
			t, v, err := ParseSample(s.Value)
			if err != nil {
				return Result{}, err
			}
			res.Samples = append(res.Samples, Sample{Labels: s.Metric, Value: v, Time: t})
		}
	case "scalar":
		var pair []json.RawMessage
		if err := json.Unmarshal(r.Result, &pair); err != nil {
			return Result{}, fmt.Errorf("inputs: decoding a scalar result: %w", err)
		}
		t, v, err := ParseSample(pair)
		if err != nil {
			return Result{}, err
		}
		res.Samples = []Sample{{Value: v, Time: t}}
	default:
		return Result{}, fmt.Errorf("inputs: query returned a %q result, want a vector or a scalar", r.ResultType)
	}
	return res, nil
}

// ParseSample reads a [time, "value"] pair: the time in Unix seconds, to
// the millisecond, and the value, which Prometheus writes as a string, NaN, +Inf and -Inf
// included.
func ParseSample(pair []json.RawMessage) (time.Time, float64, error) {
	if len(pair) != 2 {
		return time.Time{}, 0, fmt.Errorf("inputs: sample value has %d elements, want [time, value]", len(pair))
	}
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return time.Time{}, 0, fmt.Errorf("inputs: sample time %s is not a number", pair[0])
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return time.Time{}, 0, fmt.Errorf("inputs: sample value %s is not a string", pair[1])
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("inputs: sample value %q is not a number", s)
	}
	// Prometheus times have millisecond resolution.
	return time.UnixMilli(int64(math.Round(ts * 1000))).UTC(), v, nil
}

// excerpt is the start of a body for an error message.
func excerpt(body []byte) string {
	if len(body) > errorExcerptBytes {
		body = body[:errorExcerptBytes]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(body), ""))
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// ByLabel returns the value of each sample keyed by its value of label, and
// counts what it skipped. Two samples with the same key are an error.
//
// A sample without the label, or with a value that is not finite, is
// skipped and counted. On a duplicate the map is nil and Issues counts
// every duplicate; a grouped query should never return one, so the cause is
// a query that does not aggregate by label, or two producers that Prometheus
// stores as one target.
func (r Result) ByLabel(label string) (map[string]float64, Issues, error) {
	iss := Issues{Series: len(r.Samples)}
	out := make(map[string]float64, len(r.Samples))
	var dup string
	for _, s := range r.Samples {
		key, ok := s.Labels[label]
		if !ok || key == "" {
			iss.MissingLabel++
			continue
		}
		if !finite(s.Value) {
			iss.NonFinite++
			continue
		}
		if _, seen := out[key]; seen {
			iss.Duplicates++
			if dup == "" {
				dup = key
			}
			continue
		}
		out[key] = s.Value
	}
	if iss.Duplicates > 0 {
		return nil, iss, fmt.Errorf("inputs: %d series repeat a value of label %q (first: %q); aggregate the query by %s", iss.Duplicates, label, dup, label)
	}
	return out, iss, nil
}

// Single returns the value of the only sample. No sample is ErrNoData;
// several samples, or a value that is not finite, are an error.
func (r Result) Single() (float64, error) {
	switch len(r.Samples) {
	case 0:
		return 0, ErrNoData
	case 1:
	default:
		return 0, fmt.Errorf("inputs: query returned %d series, want 1; aggregate it, for example with sum or avg", len(r.Samples))
	}
	v := r.Samples[0].Value
	if !finite(v) {
		return 0, fmt.Errorf("inputs: query returned %v, want a finite value", v)
	}
	return v, nil
}

// Sum returns the sum of the finite sample values, 0 when there are none.
func (r Result) Sum() float64 {
	var sum float64
	for _, s := range r.Samples {
		if finite(s.Value) {
			sum += s.Value
		}
	}
	return sum
}

// PromDuration writes d as a PromQL range: whole minutes as Nm, anything
// else as seconds, Ns. A window under MinWindow, or one that is not a whole
// number of seconds, is an error.
func PromDuration(d time.Duration) (string, error) {
	if d < MinWindow {
		return "", fmt.Errorf("inputs: rate window %s is shorter than %s", d, MinWindow)
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m", nil
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s", nil
	}
	return "", fmt.Errorf("inputs: rate window %s is not a whole number of seconds", d)
}
