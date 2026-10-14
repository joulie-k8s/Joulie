package calib

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Point is one reading of a series. For a cumulative counter Start is the
// start of its series, and a new Start marks a reset; for a delta point it is
// the start of the interval the point's increment covers. It is the zero time
// when the source gave none.
type Point struct {
	Time  time.Time
	Start time.Time
	Value float64
}

// Series is one stream of a metric: the metric's name and unit and the
// point's attributes, every value as text (a double in the shortest form that
// reads back the same, an int in decimal). Counter marks a monotonic sum,
// and Delta one with delta temporality, whose points are increments rather
// than totals. Points are sorted by time, one per time.
type Series struct {
	Metric  string
	Unit    string
	Attrs   map[string]string
	Counter bool
	Delta   bool
	Points  []Point
}

// Data is every series of one node of a sweep, sorted by metric name and
// attributes.
type Data struct {
	Node   string
	Series []*Series
}

// Select returns the series of metric whose attributes include every pair of
// match, in the order of d.Series.
func (d *Data) Select(metric string, match map[string]string) []*Series {
	var out []*Series
	for _, s := range d.Series {
		if s.Metric != metric {
			continue
		}
		ok := true
		for k, v := range match {
			if got, has := s.Attrs[k]; !has || got != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// mean is the mean value of the points of s in [a, b] and their number.
func (s *Series) mean(a, b time.Time) (float64, int) {
	sum, n := 0.0, 0
	for _, p := range s.Points {
		if p.Time.Before(a) || p.Time.After(b) {
			continue
		}
		sum += p.Value
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// rate is the mean rate of counter s over [a, b], in its unit per second,
// and the number of intervals behind it. A cumulative counter contributes
// the increment between each two consecutive points inside the window; an
// interval over which the value falls or the series' start changes is a
// reset and is dropped. A delta counter contributes each point inside the
// window whose interval starts inside it too; the increments are summed.
func (s *Series) rate(a, b time.Time) (float64, int) {
	sum, secs, n := 0.0, 0.0, 0
	for i, p := range s.Points {
		if p.Time.Before(a) || p.Time.After(b) || i == 0 && !s.Delta {
			continue
		}
		var from time.Time
		v := p.Value
		if s.Delta {
			from = p.Start
			if from.IsZero() {
				if i == 0 {
					continue
				}
				from = s.Points[i-1].Time
			}
		} else {
			prev := s.Points[i-1]
			if prev.Time.Before(a) {
				continue
			}
			if p.Value < prev.Value || !p.Start.IsZero() && !prev.Start.IsZero() && !p.Start.Equal(prev.Start) {
				continue
			}
			from, v = prev.Time, p.Value-prev.Value
		}
		if from.Before(a) || !p.Time.After(from) {
			continue
		}
		sum += v
		secs += p.Time.Sub(from).Seconds()
		n++
	}
	if n == 0 || secs <= 0 {
		return 0, 0
	}
	return sum / secs, n
}

// power is the mean of a power reading over [a, b]: the rate of the energy
// counter of the first series of energy when there is one with at least one
// interval, else the mean of the first gauge of power. ok is false when
// neither has a reading in the window.
func power(energy, power []*Series, a, b time.Time) (w float64, n int, ok bool) {
	for _, s := range energy {
		if w, n := s.rate(a, b); n > 0 {
			return w, n, true
		}
	}
	for _, s := range power {
		if w, n := s.mean(a, b); n > 0 {
			return w, n, true
		}
	}
	return 0, 0, false
}

// dataBuilder collects points into series as a reader decodes them.
type dataBuilder struct {
	index map[string]*Series
}

func newDataBuilder() *dataBuilder { return &dataBuilder{index: map[string]*Series{}} }

// seriesKey identifies a series: its metric and its sorted attributes.
func seriesKey(metric string, attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(attrs[k])
	}
	return b.String()
}

func (b *dataBuilder) add(metric, unit string, attrs map[string]string, counter, delta bool, p Point) {
	key := seriesKey(metric, attrs)
	s, ok := b.index[key]
	if !ok {
		s = &Series{Metric: metric, Unit: unit, Attrs: attrs, Counter: counter, Delta: delta}
		b.index[key] = s
	}
	s.Points = append(s.Points, p)
}

// data sorts the series by key and each series' points by time; of two
// points at the same time the one read last stays.
func (b *dataBuilder) data(node string) *Data {
	keys := make([]string, 0, len(b.index))
	for k := range b.index {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := &Data{Node: node}
	for _, k := range keys {
		s := b.index[k]
		sort.SliceStable(s.Points, func(i, j int) bool { return s.Points[i].Time.Before(s.Points[j].Time) })
		out := s.Points[:0]
		for _, p := range s.Points {
			if len(out) > 0 && out[len(out)-1].Time.Equal(p.Time) {
				out[len(out)-1] = p
				continue
			}
			out = append(out, p)
		}
		s.Points = out
		d.Series = append(d.Series, s)
	}
	return d
}

// maxLine bounds one line of a JSON Lines file: a per-CPU sweep of a large
// node writes a few hundred kB per line.
const maxLine = 64 << 20

// ReadOTLP reads a sweep in OTLP JSON Lines, one MetricsData object per
// line as the OpenTelemetry file exporter writes them, and returns the
// series of one node. A resource belongs to the node its k8s.node.name or
// host.name names; with node set, only the resources either attribute
// matches are read. With node empty the file must hold one node, or only
// resources that name none. Sums and gauges are read, with int64 values as
// strings or numbers; histograms are skipped, and so is a point without a
// time or a value.
func ReadOTLP(r io.Reader, node string) (*Data, error) {
	builders := map[string]*dataBuilder{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		md, err := otlpjson.DecodeMetrics(b)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		for _, rm := range md.ResourceMetrics {
			name, match := otlpResourceNode(rm.Resource.Attributes, node)
			if !match {
				continue
			}
			bd, ok := builders[name]
			if !ok {
				bd = newDataBuilder()
				builders[name] = bd
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					otlpAddMetric(bd, m)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("line %d: %w", line+1, err)
	}
	return pickNode(builders, node)
}

// otlpResourceNode returns the node a resource names, k8s.node.name before
// host.name, and whether it belongs to the node asked for: either attribute
// equals it, or no node was asked for.
func otlpResourceNode(attrs []otlpjson.KeyValue, node string) (string, bool) {
	var k8s, host string
	for _, kv := range attrs {
		switch kv.Key {
		case semconv.AttrK8sNodeName:
			k8s, _ = kv.Value.Text()
		case semconv.AttrHostName:
			host, _ = kv.Value.Text()
		}
	}
	name := k8s
	if name == "" {
		name = host
	}
	if node == "" {
		return name, true
	}
	return node, k8s == node || host == node
}

// pickNode returns the one node the builders hold. Resources that name no
// node count only when no resource names one.
func pickNode(builders map[string]*dataBuilder, node string) (*Data, error) {
	if node != "" {
		bd, ok := builders[node]
		if !ok {
			return nil, fmt.Errorf("no resource with k8s.node.name or host.name %q", node)
		}
		return bd.data(node), nil
	}
	var named []string
	for name := range builders {
		if name != "" {
			named = append(named, name)
		}
	}
	sort.Strings(named)
	switch {
	case len(named) == 1:
		return builders[named[0]].data(named[0]), nil
	case len(named) > 1:
		return nil, fmt.Errorf("the sweep holds %d nodes (%s); pick one", len(named), strings.Join(named, ", "))
	}
	if bd, ok := builders[""]; ok {
		return bd.data(""), nil
	}
	return nil, errors.New("the sweep holds no metrics")
}

// otlpAddMetric adds the points of one metric. A monotonic sum is a
// counter, cumulative unless its temporality says delta; a non-monotonic sum
// and a gauge are read as gauges.
func otlpAddMetric(bd *dataBuilder, m otlpjson.Metric) {
	var points []otlpjson.NumberDataPoint
	counter, delta := false, false
	switch {
	case m.Sum != nil:
		points = m.Sum.DataPoints
		counter = m.Sum.IsMonotonic
		delta = m.Sum.AggregationTemporality == otlpjson.AggregationTemporalityDelta
	case m.Gauge != nil:
		points = m.Gauge.DataPoints
	default:
		return
	}
	for _, p := range points {
		v, ok := p.Float64()
		if !ok || p.TimeUnixNano == 0 {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range p.Attributes {
			if s, ok := kv.Value.Text(); ok {
				attrs[kv.Key] = s
			}
		}
		pt := Point{Time: unixNano(uint64(p.TimeUnixNano)), Value: v}
		if p.StartTimeUnixNano != 0 {
			pt.Start = unixNano(uint64(p.StartTimeUnixNano))
		}
		bd.add(m.Name, m.Unit, attrs, counter, counter && delta, pt)
	}
}

// unixNano is ns nanoseconds since the epoch.
func unixNano(ns uint64) time.Time {
	return time.Unix(0, int64(ns))
}
