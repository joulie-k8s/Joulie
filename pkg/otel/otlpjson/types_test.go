package otlpjson

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// otlpCheckKeys fails on every object key of a decoded JSON document that is
// not lowerCamelCase, the proto field name form OTLP JSON requires.
func otlpCheckKeys(t *testing.T, v any, at string) {
	t.Helper()
	lowerCamel := regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if !lowerCamel.MatchString(k) {
				t.Errorf("%s: key %q is not lowerCamelCase", at, k)
			}
			otlpCheckKeys(t, e, at+"."+k)
		}
	case []any:
		for _, e := range x {
			otlpCheckKeys(t, e, at+"[]")
		}
	}
}

func otlpF(v float64) *float64 { return &v }

// OJ01: a metrics export marshals with lowerCamelCase keys, every 64-bit
// integer as a decimal string (times, asInt, a histogram's count and
// bucketCounts), cumulative temporality as the integer 2, and a zero value
// still written; it reads back unchanged, from strings or from numbers.
func TestOJ01MetricsMarshalAsOTLPJSON(t *testing.T) {
	asInt := Int64String(math.MaxInt64)
	data := MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: []KeyValue{String("k8s.node.name", "node-a"), Int("n", -3), Double("d", 0), Bool("b", false)}},
		ScopeMetrics: []ScopeMetrics{{
			Scope: Scope{Name: "github.com/matbun/joulie/simulator/pkg/telemetry", Version: "test"},
			Metrics: []Metric{
				{Name: "energy", Unit: "J", Sum: &Sum{
					AggregationTemporality: AggregationTemporalityCumulative, IsMonotonic: true,
					DataPoints: []NumberDataPoint{{StartTimeUnixNano: 1544712660300000000, TimeUnixNano: math.MaxUint64, AsInt: &asInt}},
				}},
				{Name: "power", Unit: "W", Gauge: &Gauge{DataPoints: []NumberDataPoint{{TimeUnixNano: 1, AsDouble: otlpF(0)}}}},
				{Name: "duration", Unit: "s", Histogram: &Histogram{
					AggregationTemporality: AggregationTemporalityCumulative,
					DataPoints:             []HistogramDataPoint{{TimeUnixNano: 2, Count: 2, Sum: otlpF(2), BucketCounts: []Uint64String{1, 1}, ExplicitBounds: []float64{1}}},
				}},
			},
		}},
		SchemaURL: "https://opentelemetry.io/schemas/1.44.0",
	}}}

	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"resourceMetrics":[`, `"scopeMetrics":[`, `"schemaUrl":"https://opentelemetry.io/schemas/1.44.0"`,
		`"aggregationTemporality":2`, `"isMonotonic":true`,
		`"startTimeUnixNano":"1544712660300000000"`, `"timeUnixNano":"18446744073709551615"`,
		`"asInt":"9223372036854775807"`, `"asDouble":0`,
		`"count":"2"`, `"bucketCounts":["1","1"]`, `"explicitBounds":[1]`,
		`{"key":"n","value":{"intValue":"-3"}}`, `{"key":"d","value":{"doubleValue":0}}`, `{"key":"b","value":{"boolValue":false}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshal lacks %s\n%s", want, got)
		}
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	otlpCheckKeys(t, generic, "$")

	var back MetricsData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, data) {
		t.Errorf("round trip changed the export:\n got %+v\nwant %+v", back, data)
	}

	// Numbers are accepted where strings are written, null leaves the value.
	var p NumberDataPoint
	if err := json.Unmarshal([]byte(`{"timeUnixNano":1544712660300000000,"startTimeUnixNano":null,"asInt":-7}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.TimeUnixNano != 1544712660300000000 || p.StartTimeUnixNano != 0 || p.AsInt == nil || *p.AsInt != -7 {
		t.Errorf("numeric input decoded as %+v", p)
	}
	for _, bad := range []string{`{"timeUnixNano":"-1"}`, `{"timeUnixNano":1.5}`, `{"timeUnixNano":"x"}`, `{"asInt":"1e3"}`} {
		var q NumberDataPoint
		if err := json.Unmarshal([]byte(bad), &q); err == nil {
			t.Errorf("%s decoded without error as %+v", bad, q)
		}
	}
}

// OJ02: a log record marshals its eventName, the occurrence time and the
// observed time as strings, and an integer severity.
func TestOJ02LogRecordMarshalsEventName(t *testing.T) {
	body := "cap applied"
	data := LogsData{ResourceLogs: []ResourceLogs{{
		Resource: Resource{Attributes: []KeyValue{String("service.instance.id", "node-a")}},
		ScopeLogs: []ScopeLogs{{LogRecords: []LogRecord{{
			TimeUnixNano:         1000000000,
			ObservedTimeUnixNano: 2000000000,
			SeverityNumber:       9,
			SeverityText:         "INFO",
			Body:                 &AnyValue{String: &body},
			Attributes:           []KeyValue{String("hw.id", "cpu0")},
			EventName:            "joulie.sim.cap.applied",
		}}}},
	}}}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"resourceLogs":[`, `"scopeLogs":[`, `"logRecords":[`,
		`"eventName":"joulie.sim.cap.applied"`, `"timeUnixNano":"1000000000"`, `"observedTimeUnixNano":"2000000000"`,
		`"severityNumber":9`, `"severityText":"INFO"`, `"body":{"stringValue":"cap applied"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshal lacks %s\n%s", want, got)
		}
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	otlpCheckKeys(t, generic, "$")
	var back LogsData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, data) {
		t.Errorf("round trip changed the export:\n got %+v\nwant %+v", back, data)
	}
}

// A point's value reads as a float64 from either field; a point without one
// has none.
func TestNumberDataPointFloat64(t *testing.T) {
	i := Int64String(-7)
	d := 2.5
	for _, c := range []struct {
		p    NumberDataPoint
		want float64
		ok   bool
	}{
		{NumberDataPoint{AsInt: &i}, -7, true},
		{NumberDataPoint{AsDouble: &d}, 2.5, true},
		{NumberDataPoint{}, 0, false},
	} {
		if v, ok := c.p.Float64(); v != c.want || ok != c.ok {
			t.Errorf("%+v: Float64 = (%v, %v), want (%v, %v)", c.p, v, ok, c.want, c.ok)
		}
	}
}

// An attribute value reads as text and as a Go value from whichever field
// is set, arrays included; a value without a field has neither.
func TestAnyValueTextAndAny(t *testing.T) {
	arr := IntArray("k", 1, 2).Value
	for _, c := range []struct {
		v    AnyValue
		text string
		any  any
		ok   bool
	}{
		{String("k", "x").Value, "x", "x", true},
		{Bool("k", true).Value, "true", true, true},
		{Int("k", -3).Value, "-3", int64(-3), true},
		{Double("k", 0.5).Value, "0.5", 0.5, true},
		{arr, "[1,2]", []any{int64(1), int64(2)}, true},
		{AnyValue{}, "", nil, false},
	} {
		text, ok := c.v.Text()
		if text != c.text || ok != c.ok {
			t.Errorf("Text = (%q, %v), want (%q, %v)", text, ok, c.text, c.ok)
		}
		got, ok := c.v.Any()
		if !reflect.DeepEqual(got, c.any) || ok != c.ok {
			t.Errorf("Any = (%#v, %v), want (%#v, %v)", got, ok, c.any, c.ok)
		}
	}
}
