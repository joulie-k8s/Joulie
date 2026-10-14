package otlpjson

// Reading power from a metrics export. The HTTP telemetry input of the agent
// (TELEMETRY_CPU_SOURCE=http) and of the controller manager
// (NODE_POWER_SOURCE=http) answers each request with one
// ExportMetricsServiceRequest, such as the simulator's
// /otlp/telemetry/{node}. PowerReader turns the successive documents of a
// node into watts with the rules the Prometheus input applies to the same
// metrics: energy first, as a rate, the power gauge as the fallback, one
// value per component, then the sum.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// MaxTelemetryBody bounds the answer of an HTTP telemetry endpoint that
// FetchTelemetry reads. One node's export is a few kilobytes.
const MaxTelemetryBody = 8 << 20

// LegacyRemovedIn is the release that removes what the OpenTelemetry inputs
// replaced: the legacy packagePowerWatts JSON of the HTTP telemetry
// endpoints, which ReadPower reads and the simulator serves until then, and
// the controller manager's input aliases (FACILITY_*_METRIC and
// FACILITY_PRESET=legacy). One minor release after v0.3.0 added their
// replacements.
const LegacyRemovedIn = "v0.4.0"

// FetchTelemetry GETs url and returns the body of a 2xx answer. A body
// longer than MaxTelemetryBody is an error.
func FetchTelemetry(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("otlpjson: GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxTelemetryBody+1))
	if err != nil {
		return nil, fmt.Errorf("otlpjson: GET %s: %w", url, err)
	}
	if len(body) > MaxTelemetryBody {
		return nil, fmt.Errorf("otlpjson: GET %s: the answer exceeds %d bytes", url, MaxTelemetryBody)
	}
	return body, nil
}

// ReadPower reads the power in watts from one answer of an HTTP telemetry
// endpoint. An OTLP JSON metrics export (IsMetricsData) is read through r
// with s, and ok is PowerReader.Watts' ok. Any other JSON object is the
// legacy format, until LegacyRemovedIn, and legacy is set: its power is the
// first positive number of packagePowerWatts and cpu.packagePowerWatts,
// whatever s selects, and ok is false when neither is one. What
// packagePowerWatts holds is the producer's choice; the simulator puts every
// CPU package and GPU there and the packages alone under cpu. A body that
// is no JSON object is an error.
func ReadPower(body []byte, r *PowerReader, s Selector) (watts float64, ok, legacy bool, err error) {
	if IsMetricsData(body) {
		md, err := DecodeMetrics(body)
		if err != nil {
			return 0, false, false, err
		}
		w, ok := r.Watts(md, s)
		return w, ok, false, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, false, false, fmt.Errorf("otlpjson: the telemetry answer is neither an OTLP metrics export nor the legacy JSON: %w", err)
	}
	if v, ok := doc["packagePowerWatts"].(float64); ok && v > 0 {
		return v, true, true, nil
	}
	if cpu, ok := doc["cpu"].(map[string]any); ok {
		if v, ok := cpu["packagePowerWatts"].(float64); ok && v > 0 {
			return v, true, true, nil
		}
	}
	return 0, false, true, nil
}

// Selector says which readings of a document make up the power a
// PowerReader returns.
type Selector struct {
	// Node is the node the power is read for. A resource that carries
	// k8s.node.name is read only when that name equals Node; a resource
	// without it is always read, since an endpoint that serves one node
	// need not name it.
	Node string
	// HwTypes lists the hw.type values whose hw.energy and hw.power are
	// summed: cpu for the agent, cpu and gpu for the controller manager's
	// components scope. An empty list selects nothing. Ignored when Host is
	// set.
	HwTypes []string
	// Host reads the whole host, hw.host.energy and hw.host.power, instead
	// of components: the controller manager's host scope. The node is then
	// one component, whatever the hw.id of its readings.
	Host bool
}

// DefaultPowerMaxInterval is the MaxInterval of a PowerReader that sets
// none: the rate window of the Prometheus input when PROMETHEUS_RATE_WINDOW
// is unset.
const DefaultPowerMaxInterval = 5 * time.Minute

// PowerReader computes the power of the components a Selector picks from
// successive metrics exports. It keeps the previous point of every
// cumulative energy series per Selector, so one reader must see a node's
// documents in the order the producer wrote them; a point older than the
// one it keeps is ignored. A series missing from a document is forgotten
// and warms up again when it returns. The zero value is ready to use and
// safe for concurrent use.
type PowerReader struct {
	// MaxInterval is the longest interval an energy rate is measured over,
	// the window of rate(energy[5m]) in PromQL. Zero or less means
	// DefaultPowerMaxInterval.
	MaxInterval time.Duration

	mu     sync.Mutex
	series map[string]map[string]powerSeries // by Selector, then by series identity
	now    func() time.Time                  // the reader's clock; nil is time.Now
}

// powerSeries is the last point kept of a cumulative energy series and the
// rate measured up to it.
type powerSeries struct {
	start, time uint64
	energyJ     float64
	watts       float64
	hasWatts    bool
	kept        time.Time // the reader's clock when the point was kept
}

// powerComponent identifies a component as the Prometheus input does,
// max by (node, hw_type, hw_id): without hw.type, a cpu and a gpu that share
// an hw.id would count once.
type powerComponent struct{ hwType, hwID string }

// powerReading collects what one document says about one component.
type powerReading struct {
	energyW    float64 // max over the energy series that have a rate
	energyOK   bool
	energyCold bool // an energy series of the component has no rate yet
	gaugeW     float64
	gaugeOK    bool
}

// Watts returns the power in watts of what s selects in md, and whether
// every selected component has a reading. Per component (hw.type, hw.id):
//   - a cumulative hw.energy series (hw.host.energy under Host) gives the
//     energy difference to its previous point over the difference of their
//     timeUnixNano, the producer's clock, so polling jitter does not enter
//     the rate. A point whose time did not advance repeats the last rate,
//     until the reader's clock has run MaxInterval past the kept point. A
//     decrease or a new startTimeUnixNano is a reset: that interval is
//     dropped and the series warms up again. So is an interval longer than
//     MaxInterval, which rate(energy[5m]) does not reach across;
//   - a delta energy point gives its value over its own interval;
//   - otherwise, and while its energy series warms up, hw.power
//     (hw.host.power) gives the power, as PromQL's rate(energy) or power
//     does;
//   - duplicates of one component, from several resources or series, count
//     once, with their maximum. Under Host the whole node is one component,
//     as max by (k8s_node_name) makes it in PromQL.
//
// The result is the sum over the components. It is not ok when nothing is
// selected, or while a selected component has only an energy series
// without a rate: its first point, its first point after a reset or after
// a longer interval than MaxInterval, or a point that stood still for
// longer than MaxInterval. Points with a non-finite or negative value are
// ignored, and so are cumulative points without a timeUnixNano.
func (r *PowerReader) Watts(md MetricsData, s Selector) (float64, bool) {
	energyName, powerName := semconv.HwEnergy.Name, semconv.HwPower.Name
	if s.Host {
		energyName, powerName = semconv.HwHostEnergy.Name, semconv.HwHostPower.Name
	}
	types := make(map[string]bool, len(s.HwTypes))
	for _, t := range s.HwTypes {
		types[t] = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	bound := r.MaxInterval
	if bound <= 0 {
		bound = DefaultPowerMaxInterval
	}
	sel := s.key()
	prev := r.series[sel]
	next := map[string]powerSeries{}
	comps := map[powerComponent]*powerReading{}

	for _, rm := range md.ResourceMetrics {
		if node, ok := powerAttr(rm.Resource.Attributes, semconv.AttrK8sNodeName); ok && node != s.Node {
			continue
		}
		// The series identity Prometheus gives a point: job
		// (service.namespace/service.name), instance (service.instance.id),
		// then the metric name and the point attributes.
		var resID strings.Builder
		for _, k := range []string{semconv.AttrServiceNamespace, semconv.AttrServiceName, semconv.AttrServiceInstanceID} {
			v, _ := powerAttr(rm.Resource.Attributes, k)
			resID.WriteString(v)
			resID.WriteByte(0)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != energyName && m.Name != powerName {
					continue
				}
				points, delta := powerPoints(m)
				for _, p := range points {
					c, ok := powerSelect(p.Attributes, s.Host, types)
					if !ok {
						continue
					}
					v, ok := powerValue(p)
					if !ok {
						continue
					}
					rd := comps[c]
					if rd == nil {
						rd = &powerReading{}
						comps[c] = rd
					}
					switch {
					case m.Name == powerName:
						rd.gaugeW, rd.gaugeOK = math.Max(rd.gaugeW, v), true
					case delta:
						if p.StartTimeUnixNano == 0 || p.TimeUnixNano <= p.StartTimeUnixNano {
							continue
						}
						w := v / powerSeconds(uint64(p.TimeUnixNano)-uint64(p.StartTimeUnixNano))
						rd.energyW, rd.energyOK = math.Max(rd.energyW, w), true
					default:
						if p.TimeUnixNano == 0 {
							continue
						}
						id := resID.String() + m.Name + "\x00" + powerAttrsID(p.Attributes)
						old, seen := next[id]
						if !seen {
							old, seen = prev[id]
						}
						cur := powerSeries{start: uint64(p.StartTimeUnixNano), time: uint64(p.TimeUnixNano), energyJ: v, kept: now}
						switch {
						case !seen:
						case old.start != 0 && cur.start != 0 && cur.start != old.start:
							// A new lifetime, even from a producer whose clock
							// restarted behind the kept point.
						case cur.time <= old.time:
							cur = old
							if now.Sub(old.kept) > bound {
								// The producer stopped writing: its last rate
								// has left the window.
								cur.watts, cur.hasWatts = 0, false
							}
						case cur.energyJ < old.energyJ:
						case cur.time-old.time > uint64(bound):
							// The kept point is outside the window: the
							// series warms up again.
						default:
							cur.watts = (cur.energyJ - old.energyJ) / powerSeconds(cur.time-old.time)
							cur.hasWatts = true
						}
						next[id] = cur
						if cur.hasWatts {
							rd.energyW, rd.energyOK = math.Max(rd.energyW, cur.watts), true
						} else {
							rd.energyCold = true
						}
					}
				}
			}
		}
	}

	if len(next) == 0 {
		delete(r.series, sel)
	} else {
		if r.series == nil {
			r.series = map[string]map[string]powerSeries{}
		}
		r.series[sel] = next
	}

	keys := make([]powerComponent, 0, len(comps))
	for c := range comps {
		keys = append(keys, c)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].hwType != keys[j].hwType {
			return keys[i].hwType < keys[j].hwType
		}
		return keys[i].hwID < keys[j].hwID
	})
	total, counted := 0.0, 0
	for _, c := range keys {
		rd := comps[c]
		switch {
		case rd.energyOK:
			total += rd.energyW
		case rd.gaugeOK:
			total += rd.gaugeW
		case rd.energyCold:
			return 0, false
		default:
			continue
		}
		counted++
	}
	return total, counted > 0
}

// key spells s as the key of its series state, independent of the order of
// HwTypes.
func (s Selector) key() string {
	types := append([]string(nil), s.HwTypes...)
	sort.Strings(types)
	return s.Node + "\x00" + strconv.FormatBool(s.Host) + "\x00" + strings.Join(types, "\x00")
}

// powerPoints returns the number points of m and whether they are deltas. An
// energy reading sent as a gauge is read like a cumulative sum.
func powerPoints(m Metric) ([]NumberDataPoint, bool) {
	switch {
	case m.Sum != nil:
		return m.Sum.DataPoints, m.Sum.AggregationTemporality == AggregationTemporalityDelta
	case m.Gauge != nil:
		return m.Gauge.DataPoints, false
	}
	return nil, false
}

// powerSelect returns the component of a point with attributes attrs, and
// whether the selector counts it: under host every point, all as one
// component, otherwise the points whose hw.type is in types.
func powerSelect(attrs []KeyValue, host bool, types map[string]bool) (powerComponent, bool) {
	if host {
		return powerComponent{}, true
	}
	id, _ := powerAttr(attrs, semconv.AttrHwID)
	t, _ := powerAttr(attrs, semconv.AttrHwType)
	return powerComponent{hwType: t, hwID: id}, types[t]
}

// powerValue returns the value of p, and false when it has none or the value
// is not finite or negative.
func powerValue(p NumberDataPoint) (float64, bool) {
	v, ok := p.Float64()
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

func powerSeconds(ns uint64) float64 { return float64(ns) / 1e9 }

// powerAttr returns the value of attribute key as text, and whether it is
// present.
func powerAttr(attrs []KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if kv.Key == key {
			text, _ := kv.Value.Text()
			return text, true
		}
	}
	return "", false
}

// powerAttrsID spells a set of point attributes independently of their order.
func powerAttrsID(attrs []KeyValue) string {
	parts := make([]string, len(attrs))
	for i, kv := range attrs {
		text, _ := kv.Value.Text()
		parts[i] = kv.Key + "=" + text
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}
