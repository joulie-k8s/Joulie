package otlpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

const powerTSec = uint64(1e9)

// powerTRes is one resource of a test document: its attributes and metrics.
type powerTRes struct {
	attrs   []KeyValue
	metrics []Metric
}

// powerTDoc builds a document and sends it through a JSON round trip, so the
// reader sees what it would decode from a producer's body.
func powerTDoc(t *testing.T, rs ...powerTRes) MetricsData {
	t.Helper()
	md := MetricsData{}
	for _, r := range rs {
		md.ResourceMetrics = append(md.ResourceMetrics, ResourceMetrics{
			Resource:     Resource{Attributes: r.attrs},
			ScopeMetrics: []ScopeMetrics{{Scope: Scope{Name: "test"}, Metrics: r.metrics}},
		})
	}
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	var out MetricsData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// powerTRaw builds a document of one node without a JSON round trip, which
// would reject non-finite values.
func powerTRaw(node string, metrics ...Metric) MetricsData {
	return MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource:     Resource{Attributes: powerTNode(node)},
		ScopeMetrics: []ScopeMetrics{{Metrics: metrics}},
	}}}
}

func powerTNode(node string) []KeyValue {
	return []KeyValue{String(semconv.AttrK8sNodeName, node), String(semconv.AttrServiceInstanceID, node)}
}

func powerTComp(hwType, id string) []KeyValue {
	return []KeyValue{String(semconv.AttrHwID, id), String(semconv.AttrHwType, hwType)}
}

func powerTGauge(name string, points ...NumberDataPoint) Metric {
	return Metric{Name: name, Unit: "W", Gauge: &Gauge{DataPoints: points}}
}

func powerTCounter(name string, temporality int, points ...NumberDataPoint) Metric {
	return Metric{Name: name, Unit: "J", Sum: &Sum{DataPoints: points, AggregationTemporality: temporality, IsMonotonic: true}}
}

func powerTPoint(attrs []KeyValue, start, at uint64, v float64) NumberDataPoint {
	return NumberDataPoint{Attributes: attrs, StartTimeUnixNano: Uint64String(start), TimeUnixNano: Uint64String(at), AsDouble: &v}
}

// powerTEnergy is one cumulative hw.energy point of a cpu or gpu component.
func powerTEnergy(hwType, id string, atSec uint64, joules float64) Metric {
	return powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityCumulative, powerTPoint(powerTComp(hwType, id), powerTSec, atSec*powerTSec, joules))
}

func powerTWant(t *testing.T, step string, gotW float64, gotOK bool, wantW float64, wantOK bool) {
	t.Helper()
	if gotOK != wantOK || (wantOK && math.Abs(gotW-wantW) > 1e-9) {
		t.Errorf("%s: Watts = (%v, %v), want (%v, %v)", step, gotW, gotOK, wantW, wantOK)
	}
}

// The hw.type list picks the components; memory and unlisted types never
// count, and an empty list selects nothing.
func TestPowerReaderSumsSelectedTypes(t *testing.T) {
	doc := powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
		powerTGauge(semconv.HwPower.Name,
			powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, 5*powerTSec, 100),
			powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu1"), 0, 5*powerTSec, 120),
			powerTPoint(powerTComp(semconv.HwTypeGPU, "gpu0"), 0, 5*powerTSec, 300),
			powerTPoint(append(powerTComp(semconv.HwTypeMemory, "memory0"), String(semconv.AttrHwParent, "cpu0")), 0, 5*powerTSec, 20),
		),
	}})
	var r PowerReader
	w, ok := r.Watts(doc, Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}})
	powerTWant(t, "cpu", w, ok, 220, true)
	w, ok = r.Watts(doc, Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU, semconv.HwTypeGPU}})
	powerTWant(t, "cpu and gpu", w, ok, 520, true)
	w, ok = r.Watts(doc, Selector{Node: "node-a"})
	powerTWant(t, "no types", w, ok, 0, false)
}

// A cumulative energy series gives its rate between the producer's
// timestamps: not ok on the first point, the last rate again when the time
// did not advance, and an older document changes nothing.
func TestPowerReaderEnergyRateFromProducerTime(t *testing.T) {
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}})
	}
	var r PowerReader
	w, ok := r.Watts(doc(100, 1000), sel)
	powerTWant(t, "first point", w, ok, 0, false)
	w, ok = r.Watts(doc(110, 2500), sel)
	powerTWant(t, "second point", w, ok, 150, true)
	w, ok = r.Watts(doc(110, 2500), sel)
	powerTWant(t, "same point again", w, ok, 150, true)
	w, ok = r.Watts(doc(105, 1700), sel)
	powerTWant(t, "older point", w, ok, 150, true)
	w, ok = r.Watts(doc(115, 3500), sel)
	powerTWant(t, "after the older point", w, ok, 200, true)
}

// A decrease and a new start time are resets: the interval across them is
// dropped, the series warms up again, and the next interval counts. A new
// start time is a reset even when the producer restarted with its clock
// behind the kept point, so the old lifetime's rate is not repeated.
func TestPowerReaderEnergyResetIsDropped(t *testing.T) {
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	comp := powerTComp(semconv.HwTypeCPU, "cpu0")
	doc := func(startSec, atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
			powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityCumulative, powerTPoint(comp, startSec*powerTSec, atSec*powerTSec, j)),
		}})
	}
	var r PowerReader
	steps := []struct {
		name          string
		start, at     uint64
		joules, wantW float64
		wantOK        bool
	}{
		{"first point", 1, 100, 1000, 0, false},
		{"rate", 1, 110, 2500, 150, true},
		{"decrease", 1, 120, 300, 0, false},
		{"after the decrease", 1, 130, 1300, 100, true},
		{"new start time, higher value", 125, 140, 5000, 0, false},
		{"after the new start", 125, 150, 5500, 50, true},
		{"new start time, clock behind", 95, 145, 10, 0, false},
		{"after the restart behind", 95, 148, 40, 10, true},
	}
	for _, s := range steps {
		w, ok := r.Watts(doc(s.start, s.at, s.joules), sel)
		powerTWant(t, s.name, w, ok, s.wantW, s.wantOK)
	}
}

// A delta energy point carries its own interval, so it needs no history.
func TestPowerReaderDeltaEnergyPoint(t *testing.T) {
	comp := powerTComp(semconv.HwTypeCPU, "cpu0")
	doc := powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
		powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityDelta, powerTPoint(comp, 100*powerTSec, 110*powerTSec, 1500)),
	}})
	var r PowerReader
	w, ok := r.Watts(doc, Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}})
	powerTWant(t, "delta", w, ok, 150, true)
}

// A component with energy and power reads the gauge while its energy series
// warms up, as rate(energy) or power does in PromQL, and the energy rate
// from then on.
func TestPowerReaderGaugeCoversEnergyWarmup(t *testing.T) {
	comp := powerTComp(semconv.HwTypeCPU, "cpu0")
	doc := func(atSec uint64, j, gaugeW float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
			powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j),
			powerTGauge(semconv.HwPower.Name, powerTPoint(comp, 0, atSec*powerTSec, gaugeW)),
		}})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	w, ok := r.Watts(doc(100, 1000, 140), sel)
	powerTWant(t, "warm-up", w, ok, 140, true)
	w, ok = r.Watts(doc(110, 2500, 170), sel)
	powerTWant(t, "energy", w, ok, 150, true)
}

// One cold energy component makes the whole reading not ok, even when the
// other components have readings.
func TestPowerReaderPartialWarmupNotOK(t *testing.T) {
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
			powerTGauge(semconv.HwPower.Name, powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, atSec*powerTSec, 100)),
			powerTEnergy(semconv.HwTypeCPU, "cpu1", atSec, j),
		}})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	w, ok := r.Watts(doc(100, 500), sel)
	powerTWant(t, "cold cpu1", w, ok, 0, false)
	w, ok = r.Watts(doc(110, 1500), sel)
	powerTWant(t, "both", w, ok, 200, true)
}

// Duplicates of a component count once with their maximum, and hw.type is
// part of the identity: a cpu and a gpu with the same hw.id both count.
func TestPowerReaderMaxOverDuplicates(t *testing.T) {
	attrs := func(instance string) []KeyValue {
		return []KeyValue{String(semconv.AttrK8sNodeName, "node-b"), String(semconv.AttrServiceInstanceID, instance)}
	}
	doc := powerTDoc(t,
		powerTRes{attrs: attrs("exporter-1"), metrics: []Metric{powerTGauge(semconv.HwPower.Name,
			powerTPoint(powerTComp(semconv.HwTypeCPU, "0"), 0, powerTSec, 100),
			powerTPoint(powerTComp(semconv.HwTypeGPU, "0"), 0, powerTSec, 250),
		)}},
		powerTRes{attrs: attrs("exporter-2"), metrics: []Metric{powerTGauge(semconv.HwPower.Name,
			powerTPoint(powerTComp(semconv.HwTypeCPU, "0"), 0, powerTSec, 90),
		)}},
	)
	var r PowerReader
	w, ok := r.Watts(doc, Selector{Node: "node-b", HwTypes: []string{semconv.HwTypeCPU, semconv.HwTypeGPU}})
	powerTWant(t, "duplicates", w, ok, 350, true)
}

// Energy readings of one component count once with their maximum too, so the
// rates of two exporters that count the same package are not added. Each
// of service.namespace, service.name and service.instance.id tells two
// exporters' series apart, as job and instance do in Prometheus: folded
// into one series, the second exporter's point would read as a repeat of
// the first's. Delta points of one component also count once.
func TestPowerReaderMaxOverDuplicateEnergy(t *testing.T) {
	sel := Selector{Node: "node-b", HwTypes: []string{semconv.HwTypeCPU}}
	for _, key := range []string{semconv.AttrServiceInstanceID, semconv.AttrServiceName, semconv.AttrServiceNamespace} {
		exporter := func(v string, atSec uint64, j float64) powerTRes {
			return powerTRes{
				attrs:   []KeyValue{String(semconv.AttrK8sNodeName, "node-b"), String(key, v)},
				metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)},
			}
		}
		var r PowerReader
		w, ok := r.Watts(powerTDoc(t, exporter("exp-1", 100, 1000), exporter("exp-2", 100, 50000)), sel)
		powerTWant(t, key+": first points", w, ok, 0, false)
		w, ok = r.Watts(powerTDoc(t, exporter("exp-1", 110, 2500), exporter("exp-2", 110, 51700)), sel)
		powerTWant(t, key+": rates 150 and 170", w, ok, 170, true)
	}

	delta := func(instance string, j float64) powerTRes {
		return powerTRes{
			attrs: []KeyValue{String(semconv.AttrK8sNodeName, "node-b"), String(semconv.AttrServiceInstanceID, instance)},
			metrics: []Metric{powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityDelta,
				powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 100*powerTSec, 110*powerTSec, j))},
		}
	}
	var r PowerReader
	w, ok := r.Watts(powerTDoc(t, delta("exp-1", 1500), delta("exp-2", 1700)), sel)
	powerTWant(t, "delta points of 150 and 170", w, ok, 170, true)
}

// A document that carries two points of one series, as an export that
// batches two collections does, gives the rate between them.
func TestPowerReaderSeriesTwiceInOneDocument(t *testing.T) {
	var r PowerReader
	w, ok := r.Watts(powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
		powerTEnergy(semconv.HwTypeCPU, "cpu0", 100, 1000),
		powerTEnergy(semconv.HwTypeCPU, "cpu0", 110, 2500),
	}}), Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}})
	powerTWant(t, "two points", w, ok, 150, true)
}

// An energy reading sent as a gauge, without a start time, is read as a
// cumulative counter.
func TestPowerReaderEnergyGaugeIsCumulative(t *testing.T) {
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{{
			Name: semconv.HwEnergy.Name, Unit: "J",
			Gauge: &Gauge{DataPoints: []NumberDataPoint{powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, atSec*powerTSec, j)}},
		}}})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	w, ok := r.Watts(doc(100, 1000), sel)
	powerTWant(t, "first point", w, ok, 0, false)
	w, ok = r.Watts(doc(110, 2500), sel)
	powerTWant(t, "second point", w, ok, 150, true)
}

// An energy point further than MaxInterval from the kept one gives no rate,
// as rate(energy[5m]) finds no two points in its window: the gauge covers
// the component, and without a gauge the reading is not ok. The next point
// within the bound gives a rate again. The default bound is five minutes.
func TestPowerReaderStaleEnergyFallsBackToGauge(t *testing.T) {
	comp := powerTComp(semconv.HwTypeCPU, "cpu0")
	// doc has a gauge point when gaugeW is given.
	doc := func(atSec uint64, j float64, gaugeW ...float64) MetricsData {
		ms := []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}
		for _, g := range gaugeW {
			ms = append(ms, powerTGauge(semconv.HwPower.Name, powerTPoint(comp, 0, atSec*powerTSec, g)))
		}
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: ms})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	j := 1000.0
	w, ok := r.Watts(doc(100, j, 100), sel)
	powerTWant(t, "warm-up", w, ok, 100, true)
	// An hour at 100 W, then 10 s at 400 W: the hour-long average is not
	// the current power.
	j += 100*3590 + 400*10
	w, ok = r.Watts(doc(3700, j, 400), sel)
	powerTWant(t, "after an hour", w, ok, 400, true)
	j += 400 * 10
	w, ok = r.Watts(doc(3710, j, 410), sel)
	powerTWant(t, "next point", w, ok, 400, true)
	j += 200 * 299
	w, ok = r.Watts(doc(4009, j, 999), sel)
	powerTWant(t, "after 299 s", w, ok, 200, true)
	j += 300 * 301
	w, ok = r.Watts(doc(4310, j, 250), sel)
	powerTWant(t, "after 301 s", w, ok, 250, true)

	r2 := PowerReader{MaxInterval: 30 * time.Second}
	w, ok = r2.Watts(doc(100, 1000), sel)
	powerTWant(t, "energy only, first point", w, ok, 0, false)
	w, ok = r2.Watts(doc(131, 4100), sel)
	powerTWant(t, "energy only, after 31 s", w, ok, 0, false)
	w, ok = r2.Watts(doc(161, 8600), sel)
	powerTWant(t, "energy only, after 30 s", w, ok, 150, true)
}

// A producer whose time stands still repeats its last rate for MaxInterval
// of the reader's clock, and no longer: a stopped exporter does not report
// its last power forever. When its time advances again, the rate is
// measured from the kept point.
func TestPowerReaderStuckEnergyExpires(t *testing.T) {
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	clock := time.Unix(1000, 0)
	r := PowerReader{now: func() time.Time { return clock }}
	r.Watts(doc(100, 1000), sel)
	w, ok := r.Watts(doc(110, 2500), sel)
	powerTWant(t, "rate", w, ok, 150, true)
	clock = clock.Add(DefaultPowerMaxInterval)
	w, ok = r.Watts(doc(110, 2500), sel)
	powerTWant(t, "same point after the bound", w, ok, 150, true)
	clock = clock.Add(time.Second)
	w, ok = r.Watts(doc(110, 2500), sel)
	powerTWant(t, "same point past the bound", w, ok, 0, false)
	w, ok = r.Watts(doc(120, 3500), sel)
	powerTWant(t, "time advances again", w, ok, 100, true)
}

// The order of HwTypes does not matter: a selector that lists the same
// types in another order continues the same series.
func TestPowerReaderSelectorIgnoresTypeOrder(t *testing.T) {
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}})
	}
	var r PowerReader
	w, ok := r.Watts(doc(100, 1000), Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU, semconv.HwTypeGPU}})
	powerTWant(t, "cpu, gpu", w, ok, 0, false)
	w, ok = r.Watts(doc(110, 2500), Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeGPU, semconv.HwTypeCPU}})
	powerTWant(t, "gpu, cpu", w, ok, 150, true)
}

// The host selector reads hw.host.* only, an Int energy counter included,
// and ignores the component metrics. The node is one component, as in
// max by (k8s_node_name): host readings from several resources count once,
// with their maximum, whatever their hw.id.
func TestPowerReaderHostScope(t *testing.T) {
	host := []KeyValue{String(semconv.AttrHwID, semconv.HwIDHost)}
	doc := func(atSec uint64, j int64) MetricsData {
		e := Int64String(j)
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
			{Name: semconv.HwHostEnergy.Name, Unit: "J", Sum: &Sum{
				AggregationTemporality: AggregationTemporalityCumulative, IsMonotonic: true,
				DataPoints: []NumberDataPoint{{Attributes: host, StartTimeUnixNano: Uint64String(powerTSec), TimeUnixNano: Uint64String(atSec * powerTSec), AsInt: &e}},
			}},
			powerTGauge(semconv.HwHostPower.Name, powerTPoint(host, 0, atSec*powerTSec, 690)),
			powerTGauge(semconv.HwPower.Name, powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, atSec*powerTSec, 150)),
		}})
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}, Host: true}
	var r PowerReader
	w, ok := r.Watts(doc(100, 10000), sel)
	powerTWant(t, "host gauge during warm-up", w, ok, 690, true)
	w, ok = r.Watts(doc(110, 17000), sel)
	powerTWant(t, "host energy", w, ok, 700, true)

	exporter := func(instance, hwID string, watts float64) powerTRes {
		return powerTRes{
			attrs:   []KeyValue{String(semconv.AttrK8sNodeName, "node-a"), String(semconv.AttrServiceInstanceID, instance)},
			metrics: []Metric{powerTGauge(semconv.HwHostPower.Name, powerTPoint([]KeyValue{String(semconv.AttrHwID, hwID)}, 0, powerTSec, watts))},
		}
	}
	w, ok = r.Watts(powerTDoc(t, exporter("exp-1", semconv.HwIDHost, 700), exporter("exp-2", "server-1", 690)), sel)
	powerTWant(t, "two host readings", w, ok, 700, true)
}

// A resource named for another node is skipped; one without a node name is
// read.
func TestPowerReaderFiltersByNode(t *testing.T) {
	gauge := func(w float64) []Metric {
		return []Metric{powerTGauge(semconv.HwPower.Name, powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, powerTSec, w))}
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	w, ok := r.Watts(powerTDoc(t, powerTRes{attrs: powerTNode("node-b"), metrics: gauge(999)}, powerTRes{attrs: powerTNode("node-a"), metrics: gauge(150)}), sel)
	powerTWant(t, "two nodes", w, ok, 150, true)
	w, ok = r.Watts(powerTDoc(t, powerTRes{attrs: powerTNode("node-b"), metrics: gauge(999)}), sel)
	powerTWant(t, "other node only", w, ok, 0, false)
	w, ok = r.Watts(powerTDoc(t, powerTRes{metrics: gauge(120)}), sel)
	powerTWant(t, "no node name", w, ok, 120, true)
}

// One reader serves several nodes: series with the same identity but read
// for different nodes keep separate state.
func TestPowerReaderKeepsNodesApart(t *testing.T) {
	doc := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}})
	}
	a := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	b := Selector{Node: "node-b", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	r.Watts(doc(100, 1000), a)
	r.Watts(doc(100, 50000), b)
	w, ok := r.Watts(doc(110, 2000), a)
	powerTWant(t, "node-a", w, ok, 100, true)
	w, ok = r.Watts(doc(110, 53000), b)
	powerTWant(t, "node-b", w, ok, 300, true)
}

// A series missing from a document is forgotten and warms up again.
func TestPowerReaderForgetsMissingSeries(t *testing.T) {
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	energy := func(atSec uint64, j float64) MetricsData {
		return powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", atSec, j)}})
	}
	var r PowerReader
	r.Watts(energy(100, 1000), sel)
	r.Watts(powerTDoc(t, powerTRes{attrs: powerTNode("node-a")}), sel)
	w, ok := r.Watts(energy(110, 2500), sel)
	powerTWant(t, "after a gap", w, ok, 0, false)
	if n := len(r.series); n != 1 {
		t.Errorf("reader keeps %d selectors, want 1", n)
	}
}

// Non-finite and negative values, cumulative points without a time and delta
// points without an interval (no start time, or no time after it) are
// ignored: alone they give not ok, next to a valid component they leave its
// reading alone.
func TestPowerReaderIgnoresInvalidPoints(t *testing.T) {
	cpu := powerTComp(semconv.HwTypeCPU, "cpu0")
	invalid := []Metric{
		powerTGauge(semconv.HwPower.Name,
			powerTPoint(cpu, 0, powerTSec, math.NaN()),
			powerTPoint(cpu, 0, powerTSec, math.Inf(1)),
			powerTPoint(cpu, 0, powerTSec, -5),
		),
		powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityCumulative, powerTPoint(cpu, powerTSec, 0, 1000)),
		powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityDelta, powerTPoint(cpu, 10*powerTSec, 10*powerTSec, 1000)),
		powerTCounter(semconv.HwEnergy.Name, AggregationTemporalityDelta, powerTPoint(cpu, 0, 10*powerTSec, 1000)),
		powerTGauge(semconv.HwPower.Name, NumberDataPoint{Attributes: cpu, TimeUnixNano: Uint64String(powerTSec)}),
	}
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	var r PowerReader
	for i := 0; i < 2; i++ {
		w, ok := r.Watts(powerTRaw("node-a", invalid...), sel)
		powerTWant(t, "invalid points only", w, ok, 0, false)
	}

	// A time-less cumulative point kept as a series would stay cold and make
	// every reading not ok.
	valid := append([]Metric{powerTGauge(semconv.HwPower.Name, powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu1"), 0, powerTSec, 100))}, invalid...)
	var r2 PowerReader
	for i := 0; i < 2; i++ {
		w, ok := r2.Watts(powerTRaw("node-a", valid...), sel)
		powerTWant(t, "invalid points next to a valid gauge", w, ok, 100, true)
	}
}

// Concurrent reads of several nodes through one reader are safe (run with
// -race) and keep each node's series apart: every node reads its own rate.
func TestPowerReaderConcurrentUse(t *testing.T) {
	const nodes, points = 8, 20
	docs := make([][]MetricsData, nodes)
	for i := range docs {
		for k := 1; k <= points; k++ {
			j := float64(k) * 100 * float64(i+1)
			docs[i] = append(docs[i], powerTDoc(t, powerTRes{metrics: []Metric{powerTEnergy(semconv.HwTypeCPU, "cpu0", uint64(k), j)}}))
		}
	}
	var r PowerReader
	var wg sync.WaitGroup
	for i := range docs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sel := Selector{Node: fmt.Sprintf("node-%d", i), HwTypes: []string{semconv.HwTypeCPU}}
			for k, doc := range docs[i] {
				w, ok := r.Watts(doc, sel)
				if k == 0 {
					powerTWant(t, sel.Node+" first point", w, ok, 0, false)
				} else {
					powerTWant(t, fmt.Sprintf("%s point %d", sel.Node, k+1), w, ok, 100*float64(i+1), true)
				}
			}
		}(i)
	}
	wg.Wait()
}

// ReadPower reads an OTLP export through the PowerReader with the selector,
// and any other JSON object as the legacy format: the first positive
// packagePowerWatts, top level before cpu, whatever the selector says. A
// body that is no JSON object is an error and is not legacy.
func TestReadPowerOTLPAndLegacy(t *testing.T) {
	sel := Selector{Node: "node-a", HwTypes: []string{semconv.HwTypeCPU}}
	otlp, err := json.Marshal(powerTDoc(t, powerTRes{attrs: powerTNode("node-a"), metrics: []Metric{
		powerTGauge(semconv.HwPower.Name,
			powerTPoint(powerTComp(semconv.HwTypeCPU, "cpu0"), 0, 5*powerTSec, 100),
			powerTPoint(powerTComp(semconv.HwTypeGPU, "gpu0"), 0, 5*powerTSec, 300),
		),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		body       string
		watts      float64
		ok, legacy bool
		wantErr    bool
	}{
		{name: "otlp", body: string(otlp), watts: 100, ok: true},
		{name: "otlp next to a legacy key", body: strings.TrimSuffix(string(otlp), "}") + `,"packagePowerWatts":999}`, watts: 100, ok: true},
		{name: "legacy top level", body: `{"packagePowerWatts": 125.5, "cpu": {"packagePowerWatts": 80}}`, watts: 125.5, ok: true, legacy: true},
		{name: "legacy under cpu", body: `{"cpu": {"packagePowerWatts": 200}}`, watts: 200, ok: true, legacy: true},
		{name: "legacy zero top level", body: `{"packagePowerWatts": 0, "cpu": {"packagePowerWatts": 90}}`, watts: 90, ok: true, legacy: true},
		{name: "legacy without power", body: `{"node": "node-a"}`, legacy: true},
		{name: "legacy with a string", body: `{"packagePowerWatts": "125"}`, legacy: true},
		{name: "not json", body: `<html>bad gateway</html>`, wantErr: true},
		{name: "json array", body: `[1, 2]`, wantErr: true},
	} {
		var r PowerReader
		w, ok, legacy, err := ReadPower([]byte(c.body), &r, sel)
		if (err != nil) != c.wantErr || ok != c.ok || legacy != c.legacy || w != c.watts {
			t.Errorf("%s: ReadPower = (%v, %v, %v, %v), want (%v, %v, %v, error %v)", c.name, w, ok, legacy, err, c.watts, c.ok, c.legacy, c.wantErr)
		}
	}
}

// FetchTelemetry returns the body of a 2xx answer and fails on another
// status and on a body over MaxTelemetryBody.
func TestFetchTelemetry(t *testing.T) {
	big := bytes.Repeat([]byte(" "), MaxTelemetryBody+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{"packagePowerWatts": 1}`)
		case "/big":
			_, _ = w.Write(big)
		default:
			http.Error(w, "no", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	if b, err := FetchTelemetry(context.Background(), srv.Client(), srv.URL+"/ok"); err != nil || string(b) != `{"packagePowerWatts": 1}` {
		t.Errorf("GET /ok = %q, %v", b, err)
	}
	if _, err := FetchTelemetry(context.Background(), srv.Client(), srv.URL+"/down"); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("GET /down: %v, want an HTTP 503 error", err)
	}
	if _, err := FetchTelemetry(context.Background(), srv.Client(), srv.URL+"/big"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("GET /big: %v, want a size error", err)
	}
}
