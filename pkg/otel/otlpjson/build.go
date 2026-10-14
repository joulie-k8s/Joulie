package otlpjson

// Builders for the records Joulie writes: number and histogram points, the
// Sum, Gauge and Histogram metrics that hold them, event log records, and
// the one-resource, one-scope exports a .jsonl line or a push carries. A
// metric or event is described by its semconv table entry, so the name,
// unit, instrument, value type and severity are spelled once, in package
// semconv, and the builders turn them into OTLP.
//
// The builders keep what OTLP JSON cannot carry out of the output instead of
// failing the whole export: encoding/json refuses NaN and infinities, so a
// point whose value is not finite is left out, and so is an attribute whose
// value is, or holds in an array, a double that is not finite. A caller that
// counts such points compares the number of points it passed with the number
// in the result.

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// buildUnixNano is t as nanoseconds since the Unix epoch. The zero time, and
// any time before the epoch, gives 0, which OTLP reads as unset and which
// the time fields omit.
func buildUnixNano(t time.Time) Uint64String {
	if t.IsZero() || t.Before(time.Unix(0, 0)) {
		return 0
	}
	return Uint64String(t.UnixNano())
}

// buildAttrs copies attrs, so a caller may reuse its slice for the next
// point or record, and leaves out the attributes buildFinite rejects. It
// returns nil for none, which the encoding omits.
func buildAttrs(attrs []KeyValue) []KeyValue {
	out := make([]KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if buildFinite(kv.Value) {
			out = append(out, kv)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildFinite reports whether v holds no double that is NaN or infinite, at
// any depth of its arrays.
func buildFinite(v AnyValue) bool {
	if v.Double != nil && (math.IsNaN(*v.Double) || math.IsInf(*v.Double, 0)) {
		return false
	}
	if v.Array != nil {
		for _, e := range v.Array.Values {
			if !buildFinite(e) {
				return false
			}
		}
	}
	return true
}

// IntPoint is a point of a Sum or Gauge with an integer value. start is the
// start of the cumulative series (the zero time for a gauge) and at the time
// of the reading. attrs are copied.
func IntPoint(start, at time.Time, v int64, attrs ...KeyValue) NumberDataPoint {
	i := Int64String(v)
	return NumberDataPoint{
		Attributes:        buildAttrs(attrs),
		StartTimeUnixNano: buildUnixNano(start),
		TimeUnixNano:      buildUnixNano(at),
		AsInt:             &i,
	}
}

// DoublePoint is a point of a Sum or Gauge with a floating-point value, as
// IntPoint. A metric builder leaves it out when v is NaN or infinite.
func DoublePoint(start, at time.Time, v float64, attrs ...KeyValue) NumberDataPoint {
	return NumberDataPoint{
		Attributes:        buildAttrs(attrs),
		StartTimeUnixNano: buildUnixNano(start),
		TimeUnixNano:      buildUnixNano(at),
		AsDouble:          &v,
	}
}

// SumMetric is metric m as a Sum with cumulative temporality: monotonic for
// a semconv.Counter, not monotonic for a semconv.UpDownCounter. Joulie
// writes cumulative sums only, since Prometheus rejects delta ones.
//
// m.Value decides how each point is encoded: semconv.Int points are written
// as asInt, a double rounded down for a semconv.Counter, as a whole-joule
// reading of a finer counter does, and to the nearest integer otherwise,
// and semconv.Double points as asDouble. A point is left out when its value is not finite, when
// it has no value, or when a double does not fit an int64. A zero m.Kind or
// m.Value is not checked, for a metric that is not in the table, such as a
// scenario's PromQL capture: a zero Kind gives a sum that is not monotonic,
// and a zero Value keeps each point's own type.
//
// It panics when m.Kind is another instrument: that is a programming error,
// as semconv.Register treats an invalid entry.
func SumMetric(m semconv.Metric, points ...NumberDataPoint) Metric {
	if m.Kind != 0 && m.Kind != semconv.Counter && m.Kind != semconv.UpDownCounter {
		panic(fmt.Sprintf("otlpjson: SumMetric(%q): the metric is a %v, not a counter", m.Name, m.Kind))
	}
	return Metric{
		Name: m.Name, Description: m.Brief, Unit: m.Unit,
		Sum: &Sum{
			DataPoints:             buildNumberPoints(m, points),
			AggregationTemporality: AggregationTemporalityCumulative,
			IsMonotonic:            m.Kind == semconv.Counter,
		},
	}
}

// GaugeMetric is metric m as a Gauge. Points are encoded and left out as
// SumMetric says. It panics when m.Kind is neither semconv.Gauge nor zero.
func GaugeMetric(m semconv.Metric, points ...NumberDataPoint) Metric {
	if m.Kind != 0 && m.Kind != semconv.Gauge {
		panic(fmt.Sprintf("otlpjson: GaugeMetric(%q): the metric is a %v, not a gauge", m.Name, m.Kind))
	}
	return Metric{
		Name: m.Name, Description: m.Brief, Unit: m.Unit,
		Gauge: &Gauge{DataPoints: buildNumberPoints(m, points)},
	}
}

// buildNumberPoints encodes points with EncodeNumberPoint and drops those
// OTLP JSON cannot carry. The result is never nil, so an empty metric is
// written with "dataPoints":[].
func buildNumberPoints(m semconv.Metric, points []NumberDataPoint) []NumberDataPoint {
	out := make([]NumberDataPoint, 0, len(points))
	for _, p := range points {
		if p, ok := EncodeNumberPoint(m, p); ok {
			out = append(out, p)
		}
	}
	return out
}

// EncodeNumberPoint is p as SumMetric and GaugeMetric write it for m: as
// m's value type (zero: as given), rounding as SumMetric says. ok is false
// for a point they leave out. A builder that counts what it leaves out
// encodes each point with it before collecting it.
func EncodeNumberPoint(m semconv.Metric, p NumberDataPoint) (NumberDataPoint, bool) {
	vt := m.Value
	if p.AsDouble != nil && p.AsInt != nil {
		// value is a oneof: keep the declared type's value, and the
		// double when no type is declared.
		if vt == semconv.Int {
			p.AsDouble = nil
		} else {
			p.AsInt = nil
		}
	}
	switch {
	case p.AsDouble == nil && p.AsInt == nil:
		return p, false
	case p.AsDouble != nil && (math.IsNaN(*p.AsDouble) || math.IsInf(*p.AsDouble, 0)):
		return p, false
	case vt == semconv.Int && p.AsDouble != nil:
		r := math.Round(*p.AsDouble)
		if m.Kind == semconv.Counter {
			r = math.Floor(*p.AsDouble)
		}
		// 2^63 is exact as a float64; anything at or beyond it does not
		// fit an int64, nor does anything below -2^63.
		if r >= 1<<63 || r < -(1<<63) {
			return p, false
		}
		i := Int64String(int64(r))
		p.AsDouble, p.AsInt = nil, &i
	case vt == semconv.Double && p.AsInt != nil:
		d := float64(*p.AsInt)
		p.AsDouble, p.AsInt = &d, nil
	}
	return p, true
}

// ExplicitHistogram accumulates values into explicit buckets, cumulatively
// from its creation, and turns the result into a HistogramDataPoint. Bucket
// i counts the values v with bounds[i-1] < v <= bounds[i], the first bucket
// everything up to bounds[0] and the last everything above the last bound,
// as opentelemetry-proto defines explicit_bounds. It is not safe for
// concurrent use.
type ExplicitHistogram struct {
	bounds   []float64
	counts   []uint64
	count    uint64
	sum      float64
	min, max float64
	negative bool
}

// NewExplicitHistogram returns an empty histogram over bounds, which must be
// finite and strictly increasing; none means one bucket for every value. It
// panics on invalid bounds, which are a programming error: callers pass
// constants.
func NewExplicitHistogram(bounds ...float64) *ExplicitHistogram {
	if err := buildCheckBounds(bounds); err != nil {
		panic("otlpjson: NewExplicitHistogram: " + err.Error())
	}
	return &ExplicitHistogram{
		bounds: append([]float64(nil), bounds...),
		counts: make([]uint64, len(bounds)+1),
	}
}

// buildCheckBounds reports why bounds are not valid explicit bounds.
func buildCheckBounds(bounds []float64) error {
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			return fmt.Errorf("bound %d is %v", i, b)
		}
		if i > 0 && b <= bounds[i-1] {
			return fmt.Errorf("bound %d (%v) is not above bound %d (%v)", i, b, i-1, bounds[i-1])
		}
	}
	return nil
}

// Record adds v. It returns false, and records nothing, when v is NaN or
// infinite, which no OTLP sum can hold.
func (h *ExplicitHistogram) Record(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	// SearchFloat64s gives the first bound >= v: v <= bounds[i] puts it in
	// bucket i, and above every bound in the last one.
	h.counts[sort.SearchFloat64s(h.bounds, v)]++
	if h.count == 0 || v < h.min {
		h.min = v
	}
	if h.count == 0 || v > h.max {
		h.max = v
	}
	h.count++
	h.sum += v
	if v < 0 {
		h.negative = true
	}
	return true
}

// Point is the histogram so far as a cumulative point from start to at. The
// bounds and counts are copies. Sum is left out once a negative value was
// recorded, as opentelemetry-proto asks so that the sum stays monotonic for
// OpenMetrics readers; Min and Max are set only when there is a value.
func (h *ExplicitHistogram) Point(start, at time.Time, attrs ...KeyValue) HistogramDataPoint {
	p := HistogramDataPoint{
		Attributes:        buildAttrs(attrs),
		StartTimeUnixNano: buildUnixNano(start),
		TimeUnixNano:      buildUnixNano(at),
		Count:             Uint64String(h.count),
		BucketCounts:      make([]Uint64String, len(h.counts)),
		ExplicitBounds:    append([]float64(nil), h.bounds...),
	}
	for i, c := range h.counts {
		p.BucketCounts[i] = Uint64String(c)
	}
	if !h.negative {
		sum := h.sum
		p.Sum = &sum
	}
	if h.count > 0 {
		lo, hi := h.min, h.max
		p.Min, p.Max = &lo, &hi
	}
	return p
}

// HistogramMetric is metric m as a Histogram with cumulative temporality. A
// point is left out when it breaks the explicit-bucket rules: bounds that
// are not finite and strictly increasing, bucket counts that are not one
// more than the bounds (or both empty) or do not add up to the count, or a
// sum, min or max that is not finite. It panics when m.Kind is neither
// semconv.Histogram nor zero.
func HistogramMetric(m semconv.Metric, points ...HistogramDataPoint) Metric {
	if m.Kind != 0 && m.Kind != semconv.Histogram {
		panic(fmt.Sprintf("otlpjson: HistogramMetric(%q): the metric is a %v, not a histogram", m.Name, m.Kind))
	}
	out := make([]HistogramDataPoint, 0, len(points))
	for _, p := range points {
		if buildHistogramPointValid(p) {
			out = append(out, p)
		}
	}
	return Metric{
		Name: m.Name, Description: m.Brief, Unit: m.Unit,
		Histogram: &Histogram{DataPoints: out, AggregationTemporality: AggregationTemporalityCumulative},
	}
}

// buildHistogramPointValid reports whether p follows the explicit-bucket
// rules HistogramMetric lists.
func buildHistogramPointValid(p HistogramDataPoint) bool {
	for _, f := range []*float64{p.Sum, p.Min, p.Max} {
		if f != nil && (math.IsNaN(*f) || math.IsInf(*f, 0)) {
			return false
		}
	}
	if buildCheckBounds(p.ExplicitBounds) != nil {
		return false
	}
	if len(p.BucketCounts) == 0 {
		return len(p.ExplicitBounds) == 0
	}
	if len(p.BucketCounts) != len(p.ExplicitBounds)+1 {
		return false
	}
	var total uint64
	for _, c := range p.BucketCounts {
		total += uint64(c)
	}
	return total == uint64(p.Count)
}

// EventRecord is one occurrence of event e: a LogRecord with e's name as
// eventName and e's severity. at is when it occurred and observed when it
// was emitted; a zero observed means at. body is a display string for
// people, left out when empty; readers use the attributes, which are
// copied. A record that needs another severity than the table's, such as a
// failed assertion, sets SeverityNumber and SeverityText on the result.
func EventRecord(e semconv.Event, at, observed time.Time, body string, attrs ...KeyValue) LogRecord {
	if observed.IsZero() {
		observed = at
	}
	r := LogRecord{
		TimeUnixNano:         buildUnixNano(at),
		ObservedTimeUnixNano: buildUnixNano(observed),
		SeverityNumber:       e.SeverityNumber,
		SeverityText:         e.SeverityText,
		Attributes:           buildAttrs(attrs),
		EventName:            e.Name,
	}
	if body != "" {
		r.Body = &AnyValue{String: &body}
	}
	return r
}

// IntArray is an attribute whose value is a list of integers, such as the
// GPU indices a job holds. An empty list is written as "values":[].
func IntArray(k string, v ...int64) KeyValue {
	vals := make([]AnyValue, len(v))
	for i := range v {
		n := Int64String(v[i])
		vals[i] = AnyValue{Int: &n}
	}
	return KeyValue{Key: k, Value: AnyValue{Array: &ArrayValue{Values: vals}}}
}

// NewMetricsData is the export of metrics from one resource and one scope,
// with both declaring the pinned semconv.SchemaURL: one line of a metrics
// .jsonl file, or the body of a push. Their attributes are copied and left
// out as a point's are.
func NewMetricsData(resource Resource, scope Scope, metrics ...Metric) MetricsData {
	resource.Attributes, scope.Attributes = buildAttrs(resource.Attributes), buildAttrs(scope.Attributes)
	return MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource: resource,
		ScopeMetrics: []ScopeMetrics{{
			Scope:     scope,
			Metrics:   append(make([]Metric, 0, len(metrics)), metrics...),
			SchemaURL: semconv.SchemaURL,
		}},
		SchemaURL: semconv.SchemaURL,
	}}}
}

// NewLogsData is the export of log records from one resource and one scope,
// as NewMetricsData.
func NewLogsData(resource Resource, scope Scope, records ...LogRecord) LogsData {
	resource.Attributes, scope.Attributes = buildAttrs(resource.Attributes), buildAttrs(scope.Attributes)
	return LogsData{ResourceLogs: []ResourceLogs{{
		Resource: resource,
		ScopeLogs: []ScopeLogs{{
			Scope:      scope,
			LogRecords: append(make([]LogRecord, 0, len(records)), records...),
			SchemaURL:  semconv.SchemaURL,
		}},
		SchemaURL: semconv.SchemaURL,
	}}}
}
