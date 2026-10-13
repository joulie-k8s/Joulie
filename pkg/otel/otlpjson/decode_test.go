package otlpjson

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decodeTestRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upstream", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeTestAttr returns the value of attribute k, failing when it is absent.
func decodeTestAttr(t *testing.T, kvs []KeyValue, k string) AnyValue {
	t.Helper()
	for _, kv := range kvs {
		if kv.Key == k {
			return kv.Value
		}
	}
	t.Fatalf("no attribute %q in %+v", k, kvs)
	return AnyValue{}
}

// OJ03: the upstream examples of opentelemetry-proto v1.11.1 decode without
// error. Sum and gauge values, attributes of every kind, severities and the
// eventName of events.json are read; the exponential histogram, kvlist
// values and trace ids, which Joulie does not model, are ignored rather
// than failing the decode.
func TestOJ03UpstreamExamplesDecode(t *testing.T) {
	b := decodeTestRead(t, "metrics.json")
	if !IsMetricsData(b) {
		t.Error("IsMetricsData(metrics.json) = false")
	}
	md, err := DecodeMetrics(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.ResourceMetrics) != 1 || len(md.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("metrics.json decoded as %+v", md)
	}
	rm := md.ResourceMetrics[0]
	if v := decodeTestAttr(t, rm.Resource.Attributes, "service.name"); v.String == nil || *v.String != "my.service" {
		t.Errorf("service.name = %+v", v)
	}
	sm := rm.ScopeMetrics[0]
	if sm.Scope.Name != "my.library" || sm.Scope.Version != "1.0.0" {
		t.Errorf("scope = %+v", sm.Scope)
	}
	byName := map[string]Metric{}
	for _, m := range sm.Metrics {
		byName[m.Name] = m
	}
	if len(byName) != 4 {
		t.Fatalf("metrics = %d, want 4", len(byName))
	}
	c := byName["my.counter"]
	if c.Sum == nil || !c.Sum.IsMonotonic || c.Sum.AggregationTemporality != AggregationTemporalityDelta || c.Unit != "1" || c.Description != "I am a Counter" {
		t.Fatalf("my.counter = %+v", c)
	}
	p := c.Sum.DataPoints[0]
	if p.AsDouble == nil || *p.AsDouble != 5 || p.StartTimeUnixNano != 1544712660300000000 || p.TimeUnixNano != 1544712660300000000 {
		t.Errorf("my.counter point = %+v", p)
	}
	if v := decodeTestAttr(t, p.Attributes, "my.counter.attr"); v.String == nil || *v.String != "some value" {
		t.Errorf("my.counter.attr = %+v", v)
	}
	g := byName["my.gauge"]
	if g.Gauge == nil || g.Gauge.DataPoints[0].AsDouble == nil || *g.Gauge.DataPoints[0].AsDouble != 10 {
		t.Errorf("my.gauge = %+v", g)
	}
	if h := byName["my.histogram"]; h.Histogram == nil || h.Histogram.DataPoints[0].Count != 2 || len(h.Histogram.DataPoints[0].BucketCounts) != 2 {
		t.Errorf("my.histogram = %+v", h)
	}
	if e := byName["my.exponential.histogram"]; e.Sum != nil || e.Gauge != nil || e.Histogram != nil {
		t.Errorf("my.exponential.histogram should decode to an empty metric, got %+v", e)
	}

	b = decodeTestRead(t, "logs.json")
	if IsMetricsData(b) {
		t.Error("IsMetricsData(logs.json) = true")
	}
	ld, err := DecodeLogs(b)
	if err != nil {
		t.Fatal(err)
	}
	r := ld.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if r.SeverityNumber != 10 || r.SeverityText != "Information" || r.TimeUnixNano != 1544712660300000000 || r.ObservedTimeUnixNano != 1544712660300000000 {
		t.Errorf("log record = %+v", r)
	}
	if r.Body == nil || r.Body.String == nil || *r.Body.String != "Example log record" {
		t.Errorf("body = %+v", r.Body)
	}
	if v := decodeTestAttr(t, r.Attributes, "boolean.attribute"); v.Bool == nil || !*v.Bool {
		t.Errorf("boolean.attribute = %+v", v)
	}
	if v := decodeTestAttr(t, r.Attributes, "int.attribute"); v.Int == nil || *v.Int != 10 {
		t.Errorf("int.attribute = %+v", v)
	}
	if v := decodeTestAttr(t, r.Attributes, "double.attribute"); v.Double == nil || *v.Double != 637.704 {
		t.Errorf("double.attribute = %+v", v)
	}
	if v := decodeTestAttr(t, r.Attributes, "array.attribute"); v.Array == nil || len(v.Array.Values) != 2 || *v.Array.Values[1].String != "values" {
		t.Errorf("array.attribute = %+v", v)
	}
	if v := decodeTestAttr(t, r.Attributes, "map.attribute"); v != (AnyValue{}) {
		t.Errorf("map.attribute (a kvlist) should decode to an empty value, got %+v", v)
	}

	ed, err := DecodeLogs(decodeTestRead(t, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := ed.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if ev.EventName != "browser.page_view" || ev.SeverityNumber != 9 || ev.SeverityText != "test severity text" {
		t.Errorf("event = %+v", ev)
	}
	if v := decodeTestAttr(t, ev.Attributes, "event.attribute"); v.String == nil || *v.String != "some event attribute" {
		t.Errorf("event.attribute = %+v", v)
	}

	// The strict decoders refuse the examples: they carry fields Joulie
	// does not model, which is what strictness is for.
	if _, err := DecodeMetricsStrict(decodeTestRead(t, "metrics.json")); err == nil || !strings.Contains(err.Error(), "exponentialHistogram") {
		t.Errorf("DecodeMetricsStrict(metrics.json) err = %v, want one naming exponentialHistogram", err)
	}
	if _, err := DecodeLogsStrict(decodeTestRead(t, "logs.json")); err == nil || !strings.Contains(err.Error(), `"traceId"`) {
		t.Errorf("DecodeLogsStrict(logs.json) err = %v, want one naming traceId", err)
	}
}

// OJ06 (decoders): the strict decoders refuse snake_case keys, unknown keys,
// keys in another case, a known key in the wrong object and data after the
// object; the lenient decoders accept each of them, ignoring the key. Both
// read what Joulie writes.
func TestOJ06StrictDecodersRefuseWhatLenientAccept(t *testing.T) {
	for _, tc := range []struct{ name, metrics, logs string }{
		{"snake_case", `{"resource_metrics":[]}`, `{"resource_logs":[]}`},
		{"unknown key", `{"resourceMetrics":[],"extra":1}`, `{"resourceLogs":[],"extra":1}`},
		{"other case", `{"ResourceMetrics":[]}`, `{"ResourceLogs":[]}`},
		{"nested snake_case", `{"resourceMetrics":[{"resource":{},"scope_metrics":[]}]}`, `{"resourceLogs":[{"resource":{},"scopeLogs":[{"scope":{},"logRecords":[{"time_unix_nano":"1"}]}]}]}`},
		{"known key in the wrong object", `{"resourceMetrics":[{"resource":{},"scopeMetrics":[{"scope":{},"metrics":[{"name":"m","gauge":{"dataPoints":[{"count":"1"}]}}]}]}]}`, `{"resourceLogs":[{"resource":{},"scopeLogs":[{"scope":{},"logRecords":[{"metrics":[]}]}]}]}`},
		{"dropped attributes count", `{"resourceMetrics":[{"resource":{"droppedAttributesCount":1},"scopeMetrics":[]}]}`, `{"resourceLogs":[{"resource":{"droppedAttributesCount":1},"scopeLogs":[]}]}`},
	} {
		if _, err := DecodeMetricsStrict([]byte(tc.metrics)); err == nil {
			t.Errorf("%s: DecodeMetricsStrict(%s) accepted it", tc.name, tc.metrics)
		}
		if _, err := DecodeMetrics([]byte(tc.metrics)); err != nil {
			t.Errorf("%s: DecodeMetrics(%s) = %v", tc.name, tc.metrics, err)
		}
		if _, err := DecodeLogsStrict([]byte(tc.logs)); err == nil {
			t.Errorf("%s: DecodeLogsStrict(%s) accepted it", tc.name, tc.logs)
		}
		if _, err := DecodeLogs([]byte(tc.logs)); err != nil {
			t.Errorf("%s: DecodeLogs(%s) = %v", tc.name, tc.logs, err)
		}
	}
	if _, err := DecodeMetricsStrict([]byte(`{"resourceMetrics":[]} {}`)); err == nil {
		t.Error("DecodeMetricsStrict accepted data after the object")
	}
	if _, err := DecodeMetrics([]byte(`{"resourceMetrics":[]} {}`)); err == nil {
		t.Error("DecodeMetrics accepted data after the object")
	}

	asInt := Int64String(42)
	md := MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource:  Resource{Attributes: []KeyValue{String("k8s.node.name", "node-a"), Bool("b", true)}},
		SchemaURL: "https://opentelemetry.io/schemas/1.44.0",
		ScopeMetrics: []ScopeMetrics{{Scope: Scope{Name: "s", Version: "v"}, Metrics: []Metric{
			{Name: "e", Unit: "J", Description: "d", Sum: &Sum{AggregationTemporality: AggregationTemporalityCumulative, IsMonotonic: true, DataPoints: []NumberDataPoint{{StartTimeUnixNano: 1, TimeUnixNano: 2, AsInt: &asInt}}}},
			{Name: "h", Unit: "s", Histogram: &Histogram{AggregationTemporality: AggregationTemporalityCumulative, DataPoints: []HistogramDataPoint{{Count: 1, Sum: otlpF(1), BucketCounts: []Uint64String{0, 1}, ExplicitBounds: []float64{1}, Min: otlpF(1), Max: otlpF(1)}}}},
		}}},
	}}}
	line, err := fileMarshalLine(md)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMetricsStrict(line); err != nil {
		t.Errorf("DecodeMetricsStrict refused what Joulie writes: %v\n%s", err, line)
	}
	body := "b"
	ld := LogsData{ResourceLogs: []ResourceLogs{{ScopeLogs: []ScopeLogs{{LogRecords: []LogRecord{{
		TimeUnixNano: 1, ObservedTimeUnixNano: 2, SeverityNumber: 9, SeverityText: "INFO", EventName: "e",
		Body: &AnyValue{String: &body}, Attributes: []KeyValue{Int("n", 1), Double("d", 1.5), {Key: "a", Value: AnyValue{Array: &ArrayValue{Values: []AnyValue{{String: &body}}}}}},
	}}}}}}}
	if line, err = fileMarshalLine(ld); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLogsStrict(line); err != nil {
		t.Errorf("DecodeLogsStrict refused what Joulie writes: %v\n%s", err, line)
	}
	if IsMetricsData([]byte(`{"packagePowerWatts":120}`)) || IsMetricsData([]byte(`[]`)) || IsMetricsData([]byte(`{"ResourceMetrics":[]}`)) {
		t.Error("IsMetricsData accepted a body without a resourceMetrics key")
	}
}
