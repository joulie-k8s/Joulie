// Package otlpjson is a hand-written codec for the OTLP JSON encoding of
// metrics and logs (opentelemetry-proto v1.11.1, docs/specification.md,
// "JSON Protobuf Encoding"). Joulie adds no OpenTelemetry SDK: it writes and
// reads these structs with encoding/json, POSTs them to an OTLP/HTTP
// receiver such as Prometheus /api/v1/otlp/v1/metrics, and writes them one
// per line to .jsonl files, as the OTel file exporter does.
//
// The encoding rules the types implement:
//   - keys are the lowerCamelCase proto field names;
//   - int64, uint64 and fixed64 values are decimal strings on output, and a
//     string or a number on input (Uint64String, Int64String);
//   - enums are integers (AggregationTemporalityCumulative is 2);
//   - fields at their default value are omitted, except where a reader needs
//     them (a point's value, a histogram's count).
//
// MetricsData and LogsData are the bodies of an Export request and the line
// format of the file exporter, which is why the request types are aliases.
//
// The decoders complete the package (decode.go), with these signatures:
//
//	func DecodeMetrics(b []byte) (MetricsData, error)       // lenient: unknown fields ignored
//	func DecodeMetricsStrict(b []byte) (MetricsData, error) // unknown or snake_case keys fail
//	func DecodeLogsStrict(b []byte) (LogsData, error)       // unknown or snake_case keys fail
//	func IsMetricsData(b []byte) bool                       // a top-level resourceMetrics key
package otlpjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Uint64String is a uint64 that marshals as a decimal string, as OTLP JSON
// writes fixed64 and uint64 fields such as timeUnixNano. It unmarshals from a
// string or a number; null leaves it unchanged.
type Uint64String uint64

func (v Uint64String) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(v), 10) + `"`), nil
}

func (v *Uint64String) UnmarshalJSON(b []byte) error {
	s, ok, err := otlpIntText(b)
	if err != nil || !ok {
		return err
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("otlpjson: %s is not a uint64", b)
	}
	*v = Uint64String(n)
	return nil
}

// Int64String is an int64 that marshals as a decimal string, as OTLP JSON
// writes int64 fields such as asInt. It unmarshals from a string or a
// number; null leaves it unchanged.
type Int64String int64

func (v Int64String) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(v), 10) + `"`), nil
}

func (v *Int64String) UnmarshalJSON(b []byte) error {
	s, ok, err := otlpIntText(b)
	if err != nil || !ok {
		return err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("otlpjson: %s is not an int64", b)
	}
	*v = Int64String(n)
	return nil
}

// otlpIntText returns the digits of a JSON string or number; ok is false for
// null. Fractions and exponents are rejected by the caller's parse.
func otlpIntText(b []byte) (s string, ok bool, err error) {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return "", false, nil
	}
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return "", false, err
		}
		return s, true, nil
	}
	return string(b), true, nil
}

// AggregationTemporality values of a Sum or Histogram. Joulie writes
// cumulative only: Prometheus rejects delta unless its delta-to-cumulative
// conversion is enabled.
const (
	AggregationTemporalityDelta      = 1
	AggregationTemporalityCumulative = 2
)

// MetricsData is a metrics export: the body of ExportMetricsServiceRequest
// and one line of a metrics .jsonl file.
type MetricsData struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics"`
}

// ExportMetricsServiceRequest is what POST /v1/metrics carries.
type ExportMetricsServiceRequest = MetricsData

// LogsData is a logs export: the body of ExportLogsServiceRequest and one
// line of a logs .jsonl file.
type LogsData struct {
	ResourceLogs []ResourceLogs `json:"resourceLogs"`
}

// ExportLogsServiceRequest is what POST /v1/logs carries.
type ExportLogsServiceRequest = LogsData

// ResourceMetrics is the metrics of one resource, such as one emulated node.
type ResourceMetrics struct {
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics"`
	SchemaURL    string         `json:"schemaUrl,omitempty"`
}

// Resource is the entity that produced the telemetry, by its attributes.
type Resource struct {
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// ScopeMetrics is the metrics of one instrumentation scope.
type ScopeMetrics struct {
	Scope     Scope    `json:"scope"`
	Metrics   []Metric `json:"metrics"`
	SchemaURL string   `json:"schemaUrl,omitempty"`
}

// Scope is an InstrumentationScope: the code that produced the telemetry.
type Scope struct {
	Name       string     `json:"name,omitempty"`
	Version    string     `json:"version,omitempty"`
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// Metric is one metric. Exactly one of Sum, Gauge and Histogram is set.
type Metric struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Sum         *Sum       `json:"sum,omitempty"`
	Gauge       *Gauge     `json:"gauge,omitempty"`
	Histogram   *Histogram `json:"histogram,omitempty"`
}

// Sum is a counter (IsMonotonic) or an up-down counter.
type Sum struct {
	DataPoints             []NumberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality,omitempty"`
	IsMonotonic            bool              `json:"isMonotonic,omitempty"`
}

// Gauge is a set of sampled values.
type Gauge struct {
	DataPoints []NumberDataPoint `json:"dataPoints"`
}

// Histogram is a set of explicit-bucket distributions.
type Histogram struct {
	DataPoints             []HistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality,omitempty"`
}

// NumberDataPoint is one point of a Sum or Gauge. Exactly one of AsDouble and
// AsInt is set; they are pointers so that a zero value is still written.
type NumberDataPoint struct {
	Attributes        []KeyValue   `json:"attributes,omitempty"`
	StartTimeUnixNano Uint64String `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      Uint64String `json:"timeUnixNano,omitempty"`
	AsDouble          *float64     `json:"asDouble,omitempty"`
	AsInt             *Int64String `json:"asInt,omitempty"`
}

// Float64 is the point's value as a float64, and false when it has none.
func (p NumberDataPoint) Float64() (float64, bool) {
	switch {
	case p.AsDouble != nil:
		return *p.AsDouble, true
	case p.AsInt != nil:
		return float64(*p.AsInt), true
	}
	return 0, false
}

// HistogramDataPoint is one distribution. BucketCounts has one more element
// than ExplicitBounds; Count is the sum of BucketCounts.
type HistogramDataPoint struct {
	Attributes        []KeyValue     `json:"attributes,omitempty"`
	StartTimeUnixNano Uint64String   `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      Uint64String   `json:"timeUnixNano,omitempty"`
	Count             Uint64String   `json:"count"`
	Sum               *float64       `json:"sum,omitempty"`
	BucketCounts      []Uint64String `json:"bucketCounts,omitempty"`
	ExplicitBounds    []float64      `json:"explicitBounds,omitempty"`
	Min               *float64       `json:"min,omitempty"`
	Max               *float64       `json:"max,omitempty"`
}

// ResourceLogs is the log records of one resource.
type ResourceLogs struct {
	Resource  Resource    `json:"resource"`
	ScopeLogs []ScopeLogs `json:"scopeLogs"`
	SchemaURL string      `json:"schemaUrl,omitempty"`
}

// ScopeLogs is the log records of one instrumentation scope.
type ScopeLogs struct {
	Scope      Scope       `json:"scope"`
	LogRecords []LogRecord `json:"logRecords"`
	SchemaURL  string      `json:"schemaUrl,omitempty"`
}

// LogRecord is one log record. An event is a record with EventName set;
// TimeUnixNano is when it occurred and ObservedTimeUnixNano when it was
// emitted. Body is a display string only; readers use the attributes.
type LogRecord struct {
	TimeUnixNano         Uint64String `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano Uint64String `json:"observedTimeUnixNano,omitempty"`
	SeverityNumber       int          `json:"severityNumber,omitempty"`
	SeverityText         string       `json:"severityText,omitempty"`
	Body                 *AnyValue    `json:"body,omitempty"`
	Attributes           []KeyValue   `json:"attributes,omitempty"`
	EventName            string       `json:"eventName,omitempty"`
}

// KeyValue is one attribute.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is an attribute value. Exactly one field is set; they are
// pointers so that "", false and 0 are still written. kvlistValue and
// bytesValue are not used by Joulie and are ignored on input.
type AnyValue struct {
	String *string      `json:"stringValue,omitempty"`
	Bool   *bool        `json:"boolValue,omitempty"`
	Int    *Int64String `json:"intValue,omitempty"`
	Double *float64     `json:"doubleValue,omitempty"`
	Array  *ArrayValue  `json:"arrayValue,omitempty"`
}

// Text is the value as text, and false when no field is set: a string as
// it is, a bool, integer or double as strconv writes it, and an array as
// its elements' text in brackets, separated by commas.
func (v AnyValue) Text() (string, bool) {
	switch {
	case v.String != nil:
		return *v.String, true
	case v.Bool != nil:
		return strconv.FormatBool(*v.Bool), true
	case v.Int != nil:
		return strconv.FormatInt(int64(*v.Int), 10), true
	case v.Double != nil:
		return strconv.FormatFloat(*v.Double, 'g', -1, 64), true
	case v.Array != nil:
		parts := make([]string, len(v.Array.Values))
		for i, e := range v.Array.Values {
			parts[i], _ = e.Text()
		}
		return "[" + strings.Join(parts, ",") + "]", true
	}
	return "", false
}

// Any is the value as a Go value, and false when no field is set: a
// string, bool, int64 or float64, or for an array a []any of the elements
// that have a value.
func (v AnyValue) Any() (any, bool) {
	switch {
	case v.String != nil:
		return *v.String, true
	case v.Bool != nil:
		return *v.Bool, true
	case v.Int != nil:
		return int64(*v.Int), true
	case v.Double != nil:
		return *v.Double, true
	case v.Array != nil:
		out := make([]any, 0, len(v.Array.Values))
		for _, e := range v.Array.Values {
			if x, ok := e.Any(); ok {
				out = append(out, x)
			}
		}
		return out, true
	}
	return nil, false
}

// ArrayValue is a list of values.
type ArrayValue struct {
	Values []AnyValue `json:"values"`
}

// String is a string attribute.
func String(k, v string) KeyValue {
	return KeyValue{Key: k, Value: AnyValue{String: &v}}
}

// Int is an integer attribute.
func Int(k string, v int64) KeyValue {
	i := Int64String(v)
	return KeyValue{Key: k, Value: AnyValue{Int: &i}}
}

// Double is a floating-point attribute.
func Double(k string, v float64) KeyValue {
	return KeyValue{Key: k, Value: AnyValue{Double: &v}}
}

// Bool is a boolean attribute.
func Bool(k string, v bool) KeyValue {
	return KeyValue{Key: k, Value: AnyValue{Bool: &v}}
}
