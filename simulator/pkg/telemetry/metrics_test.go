package telemetry

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

var (
	testNow       = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	testNodeStart = testNow.Add(-2 * time.Hour)
	testFacStart  = testNow.Add(-3 * time.Hour)
)

func testResource() Resource {
	return Resource{ServiceName: DefaultServiceName, ServiceVersion: "test", Instance: "joulie-simulator-0"}
}

// testSnapshot is a cluster of three nodes and its facility: an Intel host
// with package limits, cpufreq and DRAM; an EPYC energy-only host with two
// NVIDIA GPUs and an unmeasured DRAM zone; and a hypervisor guest.
func testSnapshot() *emulate.Snapshot {
	intelCPU := func(s int) emulate.ComponentReading {
		return emulate.ComponentReading{
			ID: "cpu" + string(rune('0'+s)), Type: semconv.HwTypeCPU, Name: "package-" + string(rune('0'+s)),
			Vendor: "Intel", Model: "Intel(R) Xeon(R) Platinum 8480+", Index: s, Measured: true,
			PowerW: 150.25, EnergyJ: 5000.5 + float64(s), LimitW: 165, RequestedLimitW: 120,
			SpeedHz: 2.0e9 + 0.4, SpeedMaxHz: 3.8e9, SpeedLimitHz: 3.0e9 + 0.6, Utilization: 0.6,
		}
	}
	dram := func(s int, measured bool) emulate.ComponentReading {
		return emulate.ComponentReading{
			ID: "memory" + string(rune('0'+s)), Type: semconv.HwTypeMemory, Name: "dram", Parent: "cpu" + string(rune('0'+s)),
			Index: s, Measured: measured, PowerW: 12, EnergyJ: 400,
		}
	}
	epycCPU := func(s int) emulate.ComponentReading {
		return emulate.ComponentReading{
			ID: "cpu" + string(rune('0'+s)), Type: semconv.HwTypeCPU, Name: "package-" + string(rune('0'+s)),
			Vendor: "AMD", Model: "AMD EPYC 9654 96-Core Processor", Index: s, Measured: true,
			PowerW: 200, EnergyJ: 9000, SpeedHz: 2.4e9, SpeedMaxHz: 3.7e9, SpeedLimitHz: 3.7e9, Utilization: 0.5,
		}
	}
	gpu := func(i int) emulate.ComponentReading {
		return emulate.ComponentReading{
			ID: "gpu" + string(rune('0'+i)), Type: semconv.HwTypeGPU, Name: "NVIDIA H100 NVL", Vendor: "NVIDIA", Model: "NVIDIA H100 NVL",
			UUID: "GPU-00000000-0000-0000-0000-00000000000" + string(rune('0'+i)), PCIBusID: "00000000:1" + string(rune('0'+i)) + ":00.0",
			Index: i, Measured: true, PowerW: 300, EnergyJ: 70000, DeviceEnergyJ: 70100, LimitW: 400, RequestedLimitW: 400, Utilization: 0.75,
		}
	}
	return &emulate.Snapshot{
		Seq: 7, Cluster: "smoke", Time: testNow,
		Nodes: []emulate.NodeSnapshot{
			{
				Name: "intel-0", Pool: "intel", Profile: "intel-xeon-2s-rapl", Rack: "rack-1", CoolingZone: "zone-a",
				StartTime: testNodeStart, Started: true, Fresh: true,
				Host:       emulate.HostReading{Measured: true, PowerW: 424.5, EnergyJ: 12345.9, OtherW: 100, InletC: 20},
				Components: []emulate.ComponentReading{intelCPU(0), intelCPU(1), dram(0, true), dram(1, true)},
			},
			{
				Name: "nvl-0", Pool: "nvl", Profile: "nvidia-h100-nvl-8gpu", Rack: "rack-2", CoolingZone: "zone-b",
				StartTime: testNodeStart.Add(time.Minute), Started: true, Added: []string{"sys/cpuinfo", "bin/nvidia-smi"},
				DriverVersion: "550.54.15",
				Host:          emulate.HostReading{Measured: true, PowerW: 1100, EnergyJ: 4e6, OtherW: 100, InletC: 23},
				Components:    []emulate.ComponentReading{epycCPU(0), epycCPU(1), dram(0, false), dram(1, false), gpu(0), gpu(1)},
			},
			{
				Name: "vm-0", Pool: "vm", Profile: "vm-no-powercap", StartTime: testNodeStart,
				Host: emulate.HostReading{Measured: false, PowerW: 90, EnergyJ: 1000, InletC: 20},
				Components: []emulate.ComponentReading{{
					ID: "cpu0", Type: semconv.HwTypeCPU, Name: "package-0", Measured: false, PowerW: 80, EnergyJ: 900,
					SpeedHz: 2e9, SpeedMaxHz: 3e9, SpeedLimitHz: 3e9, LimitW: 100, RequestedLimitW: 100,
				}},
			},
		},
		Facility: emulate.FacilitySnapshot{
			StartTime: testFacStart, AmbientC: 21.5, ITPowerW: 1614.5, ITEnergyJ: 5e6, PUE: 1.19, CoolingPowerW: 306.8, CoolingEnergyJ: 9.5e5,
			Zones: []emulate.ZoneReading{{Name: "zone-a", AmbientC: 21.5}, {Name: "zone-b", AmbientC: 24.5}},
			Racks: []emulate.RackReading{{Name: "rack-1", PowerW: 424.5, EnergyJ: 1e5}, {Name: "rack-2", PowerW: 1100, EnergyJ: 3e5}},
		},
	}
}

// testAttrs flattens string, bool and int attributes into a map.
func testAttrs(kvs []otlpjson.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.Value; {
		case v.String != nil:
			m[kv.Key] = *v.String
		case v.Bool != nil:
			if *v.Bool {
				m[kv.Key] = "true"
			} else {
				m[kv.Key] = "false"
			}
		case v.Int != nil:
			m[kv.Key] = strconv.FormatInt(int64(*v.Int), 10)
		case v.Double != nil:
			m[kv.Key] = mustJSON(*v.Double)
		}
	}
	return m
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// testResourceByInstance indexes the resources of d by service.instance.id.
func testResourceByInstance(t *testing.T, d otlpjson.MetricsData) map[string]otlpjson.ResourceMetrics {
	t.Helper()
	out := map[string]otlpjson.ResourceMetrics{}
	for _, rm := range d.ResourceMetrics {
		id := testAttrs(rm.Resource.Attributes)[semconv.AttrServiceInstanceID]
		if _, dup := out[id]; dup {
			t.Fatalf("two resources with service.instance.id %q", id)
		}
		out[id] = rm
	}
	return out
}

// testMetrics indexes the metrics of one resource by name.
func testMetrics(t *testing.T, rm otlpjson.ResourceMetrics) map[string]otlpjson.Metric {
	t.Helper()
	if len(rm.ScopeMetrics) != 1 {
		t.Fatalf("%d scopes, want 1", len(rm.ScopeMetrics))
	}
	out := map[string]otlpjson.Metric{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if _, dup := out[m.Name]; dup {
			t.Fatalf("metric %s appears twice in one scope", m.Name)
		}
		out[m.Name] = m
	}
	return out
}

func testPoints(m otlpjson.Metric) []otlpjson.NumberDataPoint {
	if m.Sum != nil {
		return m.Sum.DataPoints
	}
	if m.Gauge != nil {
		return m.Gauge.DataPoints
	}
	return nil
}

// testRequest builds the export of testSnapshot and checks it survives a
// strict round trip, so every assertion reads what goes on the wire.
func testRequest(t *testing.T, s *emulate.Snapshot) (otlpjson.MetricsData, int) {
	t.Helper()
	d, dropped := MetricsRequest(s, testResource())
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	back, err := otlpjson.DecodeMetricsStrict(b)
	if err != nil {
		t.Fatalf("the export does not decode strictly: %v\n%s", err, b)
	}
	return back, dropped
}

// TM01: one ResourceMetrics per exporting node, with service.instance.id,
// k8s.node.name and host.name equal to the node name, k8s.cluster.name, a
// host.id derived from the name, and the pinned schemaUrl on the resource
// and the scope; the facility is one more resource.
func TestTM01OneResourcePerNode(t *testing.T) {
	d, dropped := testRequest(t, testSnapshot())
	if dropped != 0 {
		t.Errorf("dropped %d values from a finite snapshot", dropped)
	}
	byID := testResourceByInstance(t, d)
	if len(byID) != 3 {
		t.Fatalf("resources %d, want intel-0, nvl-0 and the facility", len(byID))
	}
	for _, node := range []string{"intel-0", "nvl-0"} {
		rm, ok := byID[node]
		if !ok {
			t.Fatalf("no resource for %s", node)
		}
		a := testAttrs(rm.Resource.Attributes)
		want := map[string]string{
			semconv.AttrServiceName: DefaultServiceName, semconv.AttrServiceNamespace: ServiceNamespace, semconv.AttrServiceVersion: "test",
			semconv.AttrServiceInstanceID: node, semconv.AttrK8sNodeName: node, semconv.AttrHostName: node,
			semconv.AttrK8sClusterName: "smoke", semconv.AttrHostID: HostID(node),
		}
		for k, v := range want {
			if a[k] != v {
				t.Errorf("%s: %s = %q, want %q", node, k, a[k], v)
			}
		}
		if len(a) != len(want) {
			t.Errorf("%s: resource attributes %v, want exactly %v", node, a, want)
		}
		if rm.SchemaURL != semconv.SchemaURL || rm.ScopeMetrics[0].SchemaURL != semconv.SchemaURL {
			t.Errorf("%s: schemaUrl %q and %q, want %q", node, rm.SchemaURL, rm.ScopeMetrics[0].SchemaURL, semconv.SchemaURL)
		}
		if sc := rm.ScopeMetrics[0].Scope; sc.Name != ScopeName || sc.Version != "test" {
			t.Errorf("%s: scope %+v", node, sc)
		}
	}
	if id := HostID("intel-0"); !strings.HasPrefix(id, HostIDPrefix) || len(id) != len(HostIDPrefix)+32 || id == HostID("nvl-0") {
		t.Errorf("HostID = %q", id)
	}
	// Every name and point attribute is in the semconv table.
	table := map[string]semconv.Metric{}
	for _, m := range semconv.Metrics() {
		table[m.Name] = m
	}
	for id, rm := range byID {
		for name, m := range testMetrics(t, rm) {
			sm, ok := table[name]
			if !ok {
				t.Errorf("%s: metric %s is not in semconv", id, name)
				continue
			}
			if m.Unit != sm.Unit {
				t.Errorf("%s: %s unit %q, want %q", id, name, m.Unit, sm.Unit)
			}
			for _, p := range testPoints(m) {
				for _, kv := range p.Attributes {
					if !sm.HasAttribute(kv.Key) {
						t.Errorf("%s: %s point attribute %s is not listed in semconv", id, name, kv.Key)
					}
				}
			}
		}
	}
}

// TM02: hw.host.energy is a cumulative monotonic Sum of whole joules
// (asInt, rounded down) whose start is the node's ledger start, with
// hw.id=host and no hw.type; host power and inlet temperature are gauges
// with the same single attribute.
func TestTM02HostEnergyIsCumulativeInt(t *testing.T) {
	d, _ := testRequest(t, testSnapshot())
	nvl := testSnapshot().Nodes[1]
	ms := testMetrics(t, testResourceByInstance(t, d)["intel-0"])
	e := ms[semconv.HwHostEnergy.Name]
	if e.Sum == nil || !e.Sum.IsMonotonic || e.Sum.AggregationTemporality != otlpjson.AggregationTemporalityCumulative || e.Unit != "J" {
		t.Fatalf("hw.host.energy = %+v", e)
	}
	p := e.Sum.DataPoints[0]
	if len(e.Sum.DataPoints) != 1 || p.AsInt == nil || p.AsDouble != nil || *p.AsInt != 12345 {
		t.Errorf("hw.host.energy point = %+v, want asInt 12345", p)
	}
	if p.StartTimeUnixNano != otlpjson.Uint64String(testNodeStart.UnixNano()) || p.TimeUnixNano != otlpjson.Uint64String(testNow.UnixNano()) {
		t.Errorf("hw.host.energy start %d, time %d; want the ledger start and the snapshot time", p.StartTimeUnixNano, p.TimeUnixNano)
	}
	nms := testMetrics(t, testResourceByInstance(t, d)["nvl-0"])
	if got := nms[semconv.HwHostEnergy.Name].Sum.DataPoints[0].StartTimeUnixNano; got != otlpjson.Uint64String(nvl.StartTime.UnixNano()) {
		t.Errorf("nvl-0 start %d, want its own ledger start", got)
	}
	for _, name := range []string{semconv.HwHostEnergy.Name, semconv.HwHostPower.Name, semconv.HwHostAmbientTemperature.Name} {
		for _, p := range testPoints(ms[name]) {
			if a := testAttrs(p.Attributes); len(a) != 1 || a[semconv.AttrHwID] != semconv.HwIDHost {
				t.Errorf("%s attributes %v, want only hw.id=host", name, a)
			}
		}
	}
	if g := ms[semconv.HwHostPower.Name]; g.Gauge == nil || *g.Gauge.DataPoints[0].AsDouble != 424.5 || g.Gauge.DataPoints[0].StartTimeUnixNano != 0 {
		t.Errorf("hw.host.power = %+v", g)
	}
	if g := ms[semconv.HwHostAmbientTemperature.Name]; g.Gauge == nil || *g.Gauge.DataPoints[0].AsDouble != 20 || g.Unit != "Cel" {
		t.Errorf("hw.host.ambient_temperature = %+v", g)
	}
}

// TM03: hw.energy and hw.power appear only for measured components, with
// attributes from {hw.id, hw.type, hw.name, hw.parent}, hw.parent only on
// memory, and never hw.id=host: a measured component that claims the host
// id, or has no id, is left out, since whole-host energy is only
// hw.host.energy.
func TestTM03ComponentEnergyOnlyWhenMeasured(t *testing.T) {
	s := testSnapshot()
	intel := &s.Nodes[0]
	intel.Components = append(append([]emulate.ComponentReading(nil), intel.Components...),
		emulate.ComponentReading{ID: semconv.HwIDHost, Type: semconv.HwTypeCPU, Name: "host", Measured: true, PowerW: 424.5, EnergyJ: 12345},
		emulate.ComponentReading{ID: "", Type: semconv.HwTypeMemory, Name: "dram", Parent: "cpu0", Measured: true, PowerW: 12, EnergyJ: 400},
	)
	d, _ := testRequest(t, s)
	byID := testResourceByInstance(t, d)
	allowed := map[string]bool{semconv.AttrHwID: true, semconv.AttrHwType: true, semconv.AttrHwName: true, semconv.AttrHwParent: true}
	want := map[string][]string{
		"intel-0": {"cpu0", "cpu1", "memory0", "memory1"},
		"nvl-0":   {"cpu0", "cpu1", "gpu0", "gpu1"},
	}
	for node, ids := range want {
		ms := testMetrics(t, byID[node])
		for _, name := range []string{semconv.HwEnergy.Name, semconv.HwPower.Name} {
			var got []string
			for _, p := range testPoints(ms[name]) {
				a := testAttrs(p.Attributes)
				got = append(got, a[semconv.AttrHwID])
				for k := range a {
					if !allowed[k] {
						t.Errorf("%s %s %s carries %s", node, name, a[semconv.AttrHwID], k)
					}
				}
				if a[semconv.AttrHwID] == semconv.HwIDHost {
					t.Errorf("%s %s has a point with hw.id=host", node, name)
				}
				if _, ok := a[semconv.AttrHwParent]; ok != (a[semconv.AttrHwType] == semconv.HwTypeMemory) {
					t.Errorf("%s %s %v: hw.parent must be set on memory and only there", node, name, a)
				}
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(ids, ",") {
				t.Errorf("%s %s components %v, want %v", node, name, got, ids)
			}
		}
		if m := ms[semconv.HwEnergy.Name]; m.Sum == nil || !m.Sum.IsMonotonic || testPoints(m)[0].AsDouble == nil {
			t.Errorf("%s hw.energy is not a monotonic double Sum: %+v", node, m)
		}
	}
	mem := testAttrs(testPoints(testMetrics(t, byID["intel-0"])[semconv.HwEnergy.Name])[2].Attributes)
	if mem[semconv.AttrHwParent] != "cpu0" || mem[semconv.AttrHwName] != "dram" {
		t.Errorf("memory0 attributes %v", mem)
	}
}

// TM04: a hypervisor guest exports no hw.* metric, and no resource at all,
// whatever its components report.
func TestTM04HypervisorExportsNoHardwareMetric(t *testing.T) {
	s := testSnapshot()
	d, _ := testRequest(t, s)
	if _, ok := testResourceByInstance(t, d)["vm-0"]; ok {
		t.Error("vm-0 has a resource")
	}
	nd, _ := NodeRequest(s.Nodes[2], s.Cluster, s.Time, testResource())
	if len(nd.ResourceMetrics) != 0 {
		t.Errorf("NodeRequest(vm-0) = %+v, want no resource", nd)
	}
	nd, _ = NodeRequest(s.Nodes[0], s.Cluster, s.Time, testResource())
	if len(nd.ResourceMetrics) != 1 || testAttrs(nd.ResourceMetrics[0].Resource.Attributes)[semconv.AttrK8sNodeName] != "intel-0" {
		t.Errorf("NodeRequest(intel-0) = %+v", nd)
	}
	if !testSameMetrics(nd.ResourceMetrics[0], testResourceByInstance(t, d)["intel-0"]) {
		t.Error("NodeRequest and MetricsRequest disagree on intel-0")
	}
}

func testSameMetrics(a, b otlpjson.ResourceMetrics) bool {
	return mustJSON(a) == mustJSON(b)
}

// TM05: hw.cpu.speed.limit has exactly a max and a throttled point per
// package, both asInt and rounded; hw.cpu.speed is asInt as well.
func TestTM05CPUSpeedLimitHasMaxAndThrottled(t *testing.T) {
	d, _ := testRequest(t, testSnapshot())
	ms := testMetrics(t, testResourceByInstance(t, d)["intel-0"])
	got := map[string]int64{}
	for _, p := range testPoints(ms[semconv.HwCPUSpeedLimit.Name]) {
		a := testAttrs(p.Attributes)
		if p.AsInt == nil || p.AsDouble != nil {
			t.Fatalf("hw.cpu.speed.limit point %+v is not asInt", p)
		}
		if a[semconv.AttrHwVendor] != "Intel" || a[semconv.AttrHwModel] == "" || a[semconv.AttrHwName] == "" {
			t.Errorf("hw.cpu.speed.limit attributes %v", a)
		}
		got[a[semconv.AttrHwID]+"/"+a[semconv.AttrHwLimitType]] = int64(*p.AsInt)
	}
	want := map[string]int64{"cpu0/max": 3800000000, "cpu0/throttled": 3000000001, "cpu1/max": 3800000000, "cpu1/throttled": 3000000001}
	if mustJSON(got) != mustJSON(want) {
		t.Errorf("hw.cpu.speed.limit = %v, want %v", got, want)
	}
	for _, p := range testPoints(ms[semconv.HwCPUSpeed.Name]) {
		if p.AsInt == nil || *p.AsInt != 2000000000 {
			t.Errorf("hw.cpu.speed point %+v, want asInt 2000000000", p)
		}
	}
	if g := ms[semconv.HwCPUSpeed.Name]; g.Gauge == nil || len(g.Gauge.DataPoints) != 2 {
		t.Errorf("hw.cpu.speed = %+v", g)
	}
}

// TM06: joulie.hw.power.limit has a requested and an enforced point for
// each component with a power limit, packages and GPUs alike, and none for
// an energy-only package or for a GPU whose LimitW and RequestedLimitW are
// both 0, which means no limit (emulate.ComponentReading), not a 0 W one.
func TestTM06PowerLimitPoints(t *testing.T) {
	s := testSnapshot()
	nvl := &s.Nodes[1]
	nvl.Components = append([]emulate.ComponentReading(nil), nvl.Components...)
	nvl.Components[5].RequestedLimitW, nvl.Components[5].LimitW = 250, 300
	nolimit := nvl.Components[5]
	nolimit.ID, nolimit.Index, nolimit.LimitW, nolimit.RequestedLimitW = "gpu2", 2, 0, 0
	nvl.Components = append(nvl.Components, nolimit)
	d, _ := testRequest(t, s)
	byID := testResourceByInstance(t, d)
	for node, want := range map[string]map[string]float64{
		"intel-0": {"cpu0/cpu/requested": 120, "cpu0/cpu/enforced": 165, "cpu1/cpu/requested": 120, "cpu1/cpu/enforced": 165},
		"nvl-0":   {"gpu0/gpu/requested": 400, "gpu0/gpu/enforced": 400, "gpu1/gpu/requested": 250, "gpu1/gpu/enforced": 300},
	} {
		got := map[string]float64{}
		for _, p := range testPoints(testMetrics(t, byID[node])[semconv.JoulieHwPowerLimit.Name]) {
			a := testAttrs(p.Attributes)
			got[a[semconv.AttrHwID]+"/"+a[semconv.AttrHwType]+"/"+a[semconv.AttrJoulieLimitState]] = *p.AsDouble
		}
		if mustJSON(got) != mustJSON(want) {
			t.Errorf("%s joulie.hw.power.limit = %v, want %v", node, got, want)
		}
	}
}

// hw.gpu.utilization, an input of hwcalib (section 1.5), has one asDouble
// point per GPU: the 0..1 ratio of unit 1, not a percentage, with
// hw.gpu.task=general and the device attributes. A node without a GPU has
// none.
func TestGPUUtilizationIsARatioPerGPU(t *testing.T) {
	s := testSnapshot()
	nvl := &s.Nodes[1]
	nvl.Components = append([]emulate.ComponentReading(nil), nvl.Components...)
	nvl.Components[5].Utilization = 0.25
	d, _ := testRequest(t, s)
	byID := testResourceByInstance(t, d)
	m, ok := testMetrics(t, byID["nvl-0"])[semconv.HwGPUUtilization.Name]
	if !ok || m.Gauge == nil || m.Unit != "1" {
		t.Fatalf("nvl-0 hw.gpu.utilization = %+v, want a gauge of unit 1", m)
	}
	got := map[string]float64{}
	for _, p := range m.Gauge.DataPoints {
		if p.AsDouble == nil || p.AsInt != nil {
			t.Fatalf("hw.gpu.utilization point %+v is not asDouble", p)
		}
		a := testAttrs(p.Attributes)
		want := map[string]string{
			semconv.AttrHwID: a[semconv.AttrHwID], semconv.AttrHwName: "NVIDIA H100 NVL",
			semconv.AttrHwVendor: "NVIDIA", semconv.AttrHwModel: "NVIDIA H100 NVL", semconv.AttrHwGPUTask: "general",
		}
		if mustJSON(a) != mustJSON(want) {
			t.Errorf("hw.gpu.utilization attributes %v, want %v", a, want)
		}
		got[a[semconv.AttrHwID]] = *p.AsDouble
	}
	if want := map[string]float64{"gpu0": 0.75, "gpu1": 0.25}; mustJSON(got) != mustJSON(want) {
		t.Errorf("hw.gpu.utilization = %v, want %v", got, want)
	}
	if _, ok := testMetrics(t, byID["intel-0"])[semconv.HwGPUUtilization.Name]; ok {
		t.Error("intel-0 has no GPU but exports hw.gpu.utilization")
	}
}

// TM07: the facility resource carries each facility metric once, zones and
// racks once per name, and energy counters from the facility's start.
func TestTM07FacilityResource(t *testing.T) {
	s := testSnapshot()
	d, _ := testRequest(t, s)
	rm, ok := testResourceByInstance(t, d)[FacilityInstance]
	if !ok {
		t.Fatal("no facility resource")
	}
	a := testAttrs(rm.Resource.Attributes)
	if a[semconv.AttrK8sClusterName] != "smoke" || a[semconv.AttrK8sNodeName] != "" || a[semconv.AttrHostName] != "" {
		t.Errorf("facility resource attributes %v", a)
	}
	ms := testMetrics(t, rm)
	for _, m := range []struct {
		metric semconv.Metric
		value  float64
	}{
		{semconv.JoulieFacilityAmbientTemperature, 21.5}, {semconv.JoulieFacilityITEnergy, 5e6}, {semconv.JoulieFacilityITPower, 1614.5},
		{semconv.JoulieFacilityCoolingEnergy, 9.5e5}, {semconv.JoulieFacilityCoolingPower, 306.8}, {semconv.JoulieFacilityPUE, 1.19},
	} {
		ps := testPoints(ms[m.metric.Name])
		if len(ps) != 1 || *ps[0].AsDouble != m.value || len(ps[0].Attributes) != 0 {
			t.Errorf("%s points %+v, want one %v without attributes", m.metric.Name, ps, m.value)
			continue
		}
		if m.metric.Kind == semconv.Counter && ps[0].StartTimeUnixNano != otlpjson.Uint64String(testFacStart.UnixNano()) {
			t.Errorf("%s starts at %d, want the facility start", m.metric.Name, ps[0].StartTimeUnixNano)
		}
	}
	for _, c := range []struct {
		metric semconv.Metric
		attr   string
		want   map[string]float64
	}{
		{semconv.JoulieCoolingZoneAmbientTemperature, semconv.AttrJoulieCoolingZoneName, map[string]float64{"zone-a": 21.5, "zone-b": 24.5}},
		{semconv.JoulieRackPower, semconv.AttrJoulieRackName, map[string]float64{"rack-1": 424.5, "rack-2": 1100}},
		{semconv.JoulieRackEnergy, semconv.AttrJoulieRackName, map[string]float64{"rack-1": 1e5, "rack-2": 3e5}},
	} {
		got := map[string]float64{}
		for _, p := range testPoints(ms[c.metric.Name]) {
			got[testAttrs(p.Attributes)[c.attr]] = *p.AsDouble
			if c.metric.Kind == semconv.Counter && p.StartTimeUnixNano != otlpjson.Uint64String(testFacStart.UnixNano()) {
				t.Errorf("%s starts at %d, want the facility start", c.metric.Name, p.StartTimeUnixNano)
			}
		}
		if mustJSON(got) != mustJSON(c.want) {
			t.Errorf("%s = %v, want %v", c.metric.Name, got, c.want)
		}
	}
	if len(ms) != 9 {
		t.Errorf("facility metrics %d, want 9", len(ms))
	}
}

// TM09: NaN, infinities and an integer value beyond int64 are left out of
// the export and counted; the rest of the export is intact and encodes.
func TestTM09NonFiniteValuesAreDroppedAndCounted(t *testing.T) {
	s := testSnapshot()
	s.Nodes[0].Host.PowerW = math.NaN()
	s.Nodes[0].Components[0].EnergyJ = math.Inf(1)
	s.Nodes[1].Host.EnergyJ = 1e30
	s.Facility.PUE = math.Inf(-1)
	d, dropped := testRequest(t, s)
	if dropped != 4 {
		t.Errorf("dropped %d, want 4", dropped)
	}
	byID := testResourceByInstance(t, d)
	intel := testMetrics(t, byID["intel-0"])
	if _, ok := intel[semconv.HwHostPower.Name]; ok {
		t.Error("hw.host.power with a NaN value is in the export")
	}
	if n := len(testPoints(intel[semconv.HwEnergy.Name])); n != 3 {
		t.Errorf("intel-0 hw.energy has %d points, want 3 once the infinite one is dropped", n)
	}
	if _, ok := testMetrics(t, byID["nvl-0"])[semconv.HwHostEnergy.Name]; ok {
		t.Error("an hw.host.energy beyond int64 is in the export")
	}
	if _, ok := testMetrics(t, byID[FacilityInstance])[semconv.JoulieFacilityPUE.Name]; ok {
		t.Error("joulie.facility.pue of -Inf is in the export")
	}
}
