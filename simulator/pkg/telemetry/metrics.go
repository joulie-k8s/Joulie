package telemetry

import (
	"math"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// MaxNodesPerRequest is how many node resources one metrics export carries
// at most; the facility resource rides with the first.
const MaxNodesPerRequest = 64

// MetricsRequest is the metrics export of snapshot s: one resource per
// emulated node that exports anything, in snapshot order, then the facility
// resource. Points carry the snapshot time; counters start at their
// ledger's start, so they never decrease across a simulator restart. The
// Pusher builds the same export with points at the time it collects it
// instead. The int is the number of values left out because they were NaN,
// infinite or beyond the int64 range of an integer metric.
func MetricsRequest(s *emulate.Snapshot, r Resource) (otlpjson.MetricsData, int) {
	var at time.Time
	if s != nil {
		at = s.Time
	}
	reqs, dropped := metricsRequests(s, r, 0, at)
	if len(reqs) == 0 {
		return otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{}}, dropped
	}
	return reqs[0], dropped
}

// NodeRequest is the metrics export of one node, as MetricsRequest writes
// it, with points at time at: normally the time of the snapshot n comes
// from. A node that exports nothing, such as a hypervisor guest, gives an
// export with no resource.
func NodeRequest(n emulate.NodeSnapshot, cluster string, at time.Time, r Resource) (otlpjson.MetricsData, int) {
	d := otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{}}
	ms, dropped := nodeMetrics(n, at)
	if len(ms) > 0 {
		d.ResourceMetrics = append(d.ResourceMetrics, metricsResource(r.Node(n.Name, cluster), r, ms))
	}
	return d, dropped
}

// metricsRequests splits the export of s, with every point of the nodes
// and the facility at time at, into requests of at most maxNodes node
// resources each (all of them in one when maxNodes is 0); the facility
// resource goes with the first. A nil snapshot gives no request.
func metricsRequests(s *emulate.Snapshot, r Resource, maxNodes int, at time.Time) ([]otlpjson.MetricsData, int) {
	if s == nil {
		return nil, 0
	}
	dropped := 0
	var nodes []otlpjson.ResourceMetrics
	for _, n := range s.Nodes {
		ms, d := nodeMetrics(n, at)
		dropped += d
		if len(ms) > 0 {
			nodes = append(nodes, metricsResource(r.Node(n.Name, s.Cluster), r, ms))
		}
	}
	fac, d := facilityMetrics(s.Facility, at)
	dropped += d
	if maxNodes <= 0 {
		maxNodes = len(nodes) + 1
	}
	var reqs []otlpjson.MetricsData
	for i := 0; i == 0 || i < len(nodes); i += maxNodes {
		chunk := nodes[i:min(i+maxNodes, len(nodes))]
		rms := append([]otlpjson.ResourceMetrics{}, chunk...)
		if i == 0 && len(fac) > 0 {
			rms = append(rms, metricsResource(r.Facility(s.Cluster), r, fac))
		}
		if len(rms) > 0 {
			reqs = append(reqs, otlpjson.MetricsData{ResourceMetrics: rms})
		}
	}
	return reqs, dropped
}

// metricsResource wraps the metrics of one resource in the telemetry scope,
// with the semantic conventions schema on both.
func metricsResource(res otlpjson.Resource, r Resource, ms []otlpjson.Metric) otlpjson.ResourceMetrics {
	return otlpjson.ResourceMetrics{
		Resource: res,
		ScopeMetrics: []otlpjson.ScopeMetrics{{
			Scope:     otlpjson.Scope{Name: ScopeName, Version: r.ServiceVersion},
			Metrics:   ms,
			SchemaURL: semconv.SchemaURL,
		}},
		SchemaURL: semconv.SchemaURL,
	}
}

// nodeMetrics maps one node to the hardware metrics of the semantic
// conventions v1.44.0 (model/hardware) and Joulie's own: hw.host.* for the
// whole host, hw.energy and hw.power for each measured package, DRAM zone
// and GPU, hw.cpu.speed and hw.cpu.speed.limit for packages with cpufreq,
// hw.gpu.utilization and hw.gpu.io per GPU, joulie.hw.gpu.speed and
// joulie.hw.gpu.speed.limit per clock domain of a GPU with a clock model,
// joulie.hw.power.limit for each component with a power limit, and
// joulie.hw.governor for each component with a governor state. A host that
// is not measured, a hypervisor guest, exports nothing: no collector on
// such a machine reads any of it.
func nodeMetrics(n emulate.NodeSnapshot, at time.Time) ([]otlpjson.Metric, int) {
	if !n.Host.Measured {
		return nil, 0
	}
	b := newMetricsBuilder(at)
	start := metricsUnixNano(n.StartTime)

	// Whole-host readings carry hw.id=host and no hw.type, and the whole
	// host is never an hw.energy component (docs/hardware/host.md).
	host := otlpjson.String(semconv.AttrHwID, semconv.HwIDHost)
	b.add(semconv.HwHostEnergy, start, n.Host.EnergyJ, host)
	b.add(semconv.HwHostPower, 0, n.Host.PowerW, host)
	b.add(semconv.HwHostAmbientTemperature, 0, n.Host.InletC, host)

	comps := make([]emulate.ComponentReading, 0, len(n.Components))
	for _, c := range n.Components {
		if c.ID != "" && c.ID != semconv.HwIDHost {
			comps = append(comps, c)
		}
	}
	for _, c := range comps {
		if !c.Measured {
			continue
		}
		attrs := []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrHwID, c.ID),
			otlpjson.String(semconv.AttrHwType, c.Type),
		}
		if c.Name != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwName, c.Name))
		}
		if c.Type == semconv.HwTypeMemory && c.Parent != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwParent, c.Parent))
		}
		b.add(semconv.HwEnergy, start, c.EnergyJ, attrs...)
		b.add(semconv.HwPower, 0, c.PowerW, attrs...)
	}
	for _, c := range comps {
		if c.Type != semconv.HwTypeCPU || c.SpeedMaxHz <= 0 {
			continue
		}
		attrs := metricsDeviceAttrs(c)
		b.add(semconv.HwCPUSpeed, 0, c.SpeedHz, attrs...)
		b.add(semconv.HwCPUSpeedLimit, 0, c.SpeedMaxHz, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeMax))...)
		b.add(semconv.HwCPUSpeedLimit, 0, c.SpeedLimitHz, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeThrottled))...)
	}
	for _, c := range comps {
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		attrs := append(metricsDeviceAttrs(c), otlpjson.String(semconv.AttrHwGPUTask, semconv.GPUTaskGeneral))
		b.add(semconv.HwGPUUtilization, 0, c.Utilization, attrs...)
	}
	// hw.gpu.io counts only the peer link: the emulator moves no host or
	// NIC bytes. The counters are ledger values, so they start at the
	// node's ledger start.
	peer := otlpjson.String(semconv.AttrJoulieGPUIOPath, semconv.GPUIOPathPeer)
	for _, c := range comps {
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		attrs := metricsDeviceAttrs(c)
		b.add(semconv.HwGPUIO, start, float64(c.PeerRxBytes),
			append(attrs, otlpjson.String(semconv.AttrNetworkIODirection, semconv.NetworkIODirectionReceive), peer)...)
		b.add(semconv.HwGPUIO, start, float64(c.PeerTxBytes),
			append(attrs, otlpjson.String(semconv.AttrNetworkIODirection, semconv.NetworkIODirectionTransmit), peer)...)
	}
	for _, c := range comps {
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		for _, d := range semconv.ClockDomains() {
			if hz, ok := c.ClockHz[d]; ok {
				b.add(semconv.JoulieHwGPUSpeed, 0, float64(hz), metricsGPUClockAttrs(c, d)...)
			}
		}
	}
	// joulie.hw.gpu.speed.limit is to joulie.hw.gpu.speed what
	// hw.cpu.speed.limit is to hw.cpu.speed: the hardware maximum (max) and
	// the maximum in effect under the level, range or lock (throttled).
	for _, c := range comps {
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		for _, d := range semconv.ClockDomains() {
			for _, l := range []struct {
				typ string
				hz  map[string]int64
			}{{semconv.LimitTypeMax, c.ClockMaxHz}, {semconv.LimitTypeThrottled, c.ClockLimitHz}} {
				if hz, ok := l.hz[d]; ok {
					b.add(semconv.JoulieHwGPUSpeedLimit, 0, float64(hz), append(metricsGPUClockAttrs(c, d), otlpjson.String(semconv.AttrHwLimitType, l.typ))...)
				}
			}
		}
	}
	for _, c := range comps {
		if c.LimitW <= 0 && c.RequestedLimitW <= 0 {
			continue
		}
		for _, p := range []struct {
			state string
			w     float64
		}{{semconv.LimitStateRequested, c.RequestedLimitW}, {semconv.LimitStateEnforced, c.LimitW}} {
			b.add(semconv.JoulieHwPowerLimit, 0, p.w,
				otlpjson.String(semconv.AttrHwID, c.ID),
				otlpjson.String(semconv.AttrHwType, c.Type),
				otlpjson.String(semconv.AttrJoulieLimitState, p.state))
		}
	}
	// joulie.hw.governor reports a state as hw.status does: 1 for the state
	// a component is in, 0 for the one it left, until it changes again.
	for _, c := range comps {
		now := c.Governor()
		if now != (emulate.GovernorState{}) {
			b.add(semconv.JoulieHwGovernor, start, 1, metricsGovernorAttrs(c, now)...)
		}
		if left, ok := n.PreviousGovernors[c.ID]; ok && left != now {
			b.add(semconv.JoulieHwGovernor, start, 0, metricsGovernorAttrs(c, left)...)
		}
	}
	return b.metrics, b.dropped
}

// metricsGPUClockAttrs are the attributes of a GPU clock point of domain d:
// hw.id, hw.vendor and hw.model when known, and joulie.hw.clock.domain.
func metricsGPUClockAttrs(c emulate.ComponentReading, d string) []otlpjson.KeyValue {
	attrs := []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, c.ID)}
	if c.Vendor != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwVendor, c.Vendor))
	}
	if c.Model != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwModel, c.Model))
	}
	return append(attrs, otlpjson.String(semconv.AttrJoulieClockDomain, d))
}

// metricsGovernorAttrs are the attributes of a joulie.hw.governor point of
// state g: hw.id, hw.type, the preset, empty when no single one maps to the
// native state, and the native state.
func metricsGovernorAttrs(c emulate.ComponentReading, g emulate.GovernorState) []otlpjson.KeyValue {
	return []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrHwID, c.ID),
		otlpjson.String(semconv.AttrHwType, c.Type),
		otlpjson.String(semconv.AttrJoulieGovernorPreset, g.Preset),
		otlpjson.String(semconv.AttrJoulieGovernorNative, g.Native),
	}
}

// metricsDeviceAttrs are the attributes of a device metric: hw.id, hw.name,
// and hw.vendor and hw.model when known.
func metricsDeviceAttrs(c emulate.ComponentReading) []otlpjson.KeyValue {
	attrs := []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, c.ID)}
	if c.Name != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwName, c.Name))
	}
	if c.Vendor != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwVendor, c.Vendor))
	}
	if c.Model != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwModel, c.Model))
	}
	return attrs
}

// facilityMetrics maps the facility: each facility metric once, the
// cooling zones and racks once per name. Energy counters start at the
// facility ledger's start.
func facilityMetrics(f emulate.FacilitySnapshot, at time.Time) ([]otlpjson.Metric, int) {
	b := newMetricsBuilder(at)
	start := metricsUnixNano(f.StartTime)
	b.add(semconv.JoulieFacilityAmbientTemperature, 0, f.AmbientC)
	b.add(semconv.JoulieFacilityITEnergy, start, f.ITEnergyJ)
	b.add(semconv.JoulieFacilityITPower, 0, f.ITPowerW)
	b.add(semconv.JoulieFacilityCoolingEnergy, start, f.CoolingEnergyJ)
	b.add(semconv.JoulieFacilityCoolingPower, 0, f.CoolingPowerW)
	b.add(semconv.JoulieFacilityPUE, 0, f.PUE)
	for _, z := range f.Zones {
		b.add(semconv.JoulieCoolingZoneAmbientTemperature, 0, z.AmbientC, otlpjson.String(semconv.AttrJoulieCoolingZoneName, z.Name))
	}
	for _, rk := range f.Racks {
		name := otlpjson.String(semconv.AttrJoulieRackName, rk.Name)
		b.add(semconv.JoulieRackEnergy, start, rk.EnergyJ, name)
		b.add(semconv.JoulieRackPower, 0, rk.PowerW, name)
	}
	return b.metrics, b.dropped
}

// metricsBuilder collects the points of one resource, one Metric per name
// in the order names first appear.
type metricsBuilder struct {
	at      otlpjson.Uint64String
	metrics []otlpjson.Metric
	index   map[string]int
	dropped int
}

func newMetricsBuilder(at time.Time) *metricsBuilder {
	return &metricsBuilder{at: metricsUnixNano(at), index: map[string]int{}}
}

// add appends one point of m with value v. A counter is a cumulative,
// monotonic Sum starting at start; an up-down counter is a cumulative
// non-monotonic Sum; a gauge is a Gauge. An Int metric is written as asInt:
// counters round down, as a whole-joule reading of a finer counter does,
// and gauges round to the nearest. A value that cannot be encoded is
// dropped and counted. Histograms are not built here.
func (b *metricsBuilder) add(m semconv.Metric, start otlpjson.Uint64String, v float64, attrs ...otlpjson.KeyValue) {
	if m.Kind == semconv.Histogram {
		return
	}
	p := otlpjson.NumberDataPoint{TimeUnixNano: b.at}
	if len(attrs) > 0 {
		p.Attributes = append([]otlpjson.KeyValue(nil), attrs...)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		b.dropped++
		return
	}
	if m.Value == semconv.Int {
		if m.Kind == semconv.Counter {
			v = math.Floor(v)
		} else {
			v = math.Round(v)
		}
		// 2^63 is the first float64 beyond int64.
		if v >= math.MaxInt64 || v < math.MinInt64 {
			b.dropped++
			return
		}
		i := otlpjson.Int64String(int64(v))
		p.AsInt = &i
	} else {
		p.AsDouble = &v
	}
	i, ok := b.index[m.Name]
	if !ok {
		i = len(b.metrics)
		b.index[m.Name] = i
		om := otlpjson.Metric{Name: m.Name, Description: m.Brief, Unit: m.Unit}
		switch m.Kind {
		case semconv.Counter, semconv.UpDownCounter:
			om.Sum = &otlpjson.Sum{AggregationTemporality: otlpjson.AggregationTemporalityCumulative, IsMonotonic: m.Kind == semconv.Counter}
		default:
			om.Gauge = &otlpjson.Gauge{}
		}
		b.metrics = append(b.metrics, om)
	}
	if om := &b.metrics[i]; om.Sum != nil {
		p.StartTimeUnixNano = start
		om.Sum.DataPoints = append(om.Sum.DataPoints, p)
	} else {
		om.Gauge.DataPoints = append(om.Gauge.DataPoints, p)
	}
}

// metricsUnixNano is t in nanoseconds since the epoch, 0 for the zero time
// or a time before the epoch.
func metricsUnixNano(t time.Time) otlpjson.Uint64String {
	if t.IsZero() || t.UnixNano() < 0 {
		return 0
	}
	return otlpjson.Uint64String(t.UnixNano())
}
