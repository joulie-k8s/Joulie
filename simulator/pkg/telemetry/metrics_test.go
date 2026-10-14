package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
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

// testGovernorSnapshot is testSnapshot with the governor stage on nvl-0:
// both packages under schedutil (balanced), cpu0 having left performance;
// gpu0 unlocked (auto) with two clock domains and peer bytes, its graphics
// maximum in effect the sustained one; gpu1 under a graphics lock
// (efficient), having left unlocked (auto), with no bytes. Clock values are
// neutral test numbers.
func testGovernorSnapshot() *emulate.Snapshot {
	s := testSnapshot()
	nvl := &s.Nodes[1]
	nvl.Components = append([]emulate.ComponentReading(nil), nvl.Components...)
	for i := range nvl.Components {
		c := &nvl.Components[i]
		switch c.ID {
		case "cpu0", "cpu1":
			c.GovernorNative, c.GovernorPreset = "schedutil", "balanced"
		case "gpu0":
			c.ClockHz = map[string]int64{semconv.ClockDomainGraphics: 1200e6, semconv.ClockDomainMemory: 1500e6}
			c.ClockMaxHz = map[string]int64{semconv.ClockDomainGraphics: 1500e6, semconv.ClockDomainMemory: 1500e6}
			c.ClockLimitHz = map[string]int64{semconv.ClockDomainGraphics: 1400e6, semconv.ClockDomainMemory: 1500e6}
			c.GovernorNative, c.GovernorPreset = "unlocked", "auto"
			c.PeerRxBytes, c.PeerTxBytes = 4096, 8192
		case "gpu1":
			c.ClockHz = map[string]int64{semconv.ClockDomainGraphics: 600e6, semconv.ClockDomainMemory: 1500e6}
			c.ClockMaxHz = map[string]int64{semconv.ClockDomainGraphics: 1500e6, semconv.ClockDomainMemory: 1500e6}
			c.ClockLimitHz = map[string]int64{semconv.ClockDomainGraphics: 900e6, semconv.ClockDomainMemory: 1500e6}
			c.GovernorNative, c.GovernorPreset = "lgc=300,900", "efficient"
		}
	}
	nvl.PreviousGovernors = map[string]emulate.GovernorState{
		"cpu0": {Native: "performance", Preset: "performance"},
		"gpu1": {Native: "unlocked", Preset: "auto"},
	}
	return s
}

// testGolden prints every point of the named metrics of one resource as one
// line: name, unit, instrument, start (ledger for the node's ledger start),
// value and the sorted attributes. Lines are sorted.
func testGolden(t *testing.T, rm otlpjson.ResourceMetrics, ledgerStart time.Time, names ...string) []string {
	t.Helper()
	ms := testMetrics(t, rm)
	var out []string
	for _, name := range names {
		m, ok := ms[name]
		if !ok {
			continue
		}
		inst := "Gauge"
		if m.Sum != nil {
			if m.Sum.AggregationTemporality != otlpjson.AggregationTemporalityCumulative {
				t.Errorf("%s is not cumulative", name)
			}
			inst = "Sum(non-monotonic)"
			if m.Sum.IsMonotonic {
				inst = "Sum(monotonic)"
			}
		}
		for _, p := range testPoints(m) {
			start := strconv.FormatUint(uint64(p.StartTimeUnixNano), 10)
			if p.StartTimeUnixNano == otlpjson.Uint64String(ledgerStart.UnixNano()) {
				start = "ledger"
			}
			val := "double:" + mustJSON(p.AsDouble)
			if p.AsInt != nil {
				val = "int:" + strconv.FormatInt(int64(*p.AsInt), 10)
			}
			a := testAttrs(p.Attributes)
			keys := make([]string, 0, len(a))
			for k := range a {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			kv := make([]string, len(keys))
			for i, k := range keys {
				kv[i] = k + "=" + a[k]
			}
			out = append(out, strings.Join([]string{name, m.Unit, inst, start, val, strings.Join(kv, ",")}, " "))
		}
	}
	sort.Strings(out)
	return out
}

// GT01: the governor-stage series of a node, as a golden: hw.gpu.io a
// monotonic int counter in By from the ledger start, one point per
// direction on the peer path; joulie.hw.gpu.speed an int gauge in Hz per
// clock domain, without hw.name; joulie.hw.gpu.speed.limit an int gauge in
// Hz with a max and a throttled point per clock domain; joulie.hw.governor a
// non-monotonic int sum of unit 1, 1 for each component's state and 0 for
// the state it left.
func TestGT01GovernorStageSeriesGolden(t *testing.T) {
	s := testGovernorSnapshot()
	d, dropped := testRequest(t, s)
	if dropped != 0 {
		t.Errorf("dropped %d values", dropped)
	}
	got := testGolden(t, testResourceByInstance(t, d)["nvl-0"], s.Nodes[1].StartTime,
		"hw.gpu.io", "joulie.hw.gpu.speed", "joulie.hw.gpu.speed.limit", "joulie.hw.governor")
	const dev = "hw.id=%s,hw.model=NVIDIA H100 NVL,hw.name=NVIDIA H100 NVL,hw.vendor=NVIDIA"
	const clk = "hw.id=%s,hw.model=NVIDIA H100 NVL,hw.vendor=NVIDIA,joulie.hw.clock.domain=%s"
	const lim = "hw.id=%s,hw.limit_type=%s,hw.model=NVIDIA H100 NVL,hw.vendor=NVIDIA,joulie.hw.clock.domain=%s"
	want := []string{
		"hw.gpu.io By Sum(monotonic) ledger int:0 " + fmt.Sprintf(dev, "gpu1") + ",joulie.hw.gpu.io.path=peer,network.io.direction=receive",
		"hw.gpu.io By Sum(monotonic) ledger int:0 " + fmt.Sprintf(dev, "gpu1") + ",joulie.hw.gpu.io.path=peer,network.io.direction=transmit",
		"hw.gpu.io By Sum(monotonic) ledger int:4096 " + fmt.Sprintf(dev, "gpu0") + ",joulie.hw.gpu.io.path=peer,network.io.direction=receive",
		"hw.gpu.io By Sum(monotonic) ledger int:8192 " + fmt.Sprintf(dev, "gpu0") + ",joulie.hw.gpu.io.path=peer,network.io.direction=transmit",
		"joulie.hw.gpu.speed Hz Gauge 0 int:1200000000 " + fmt.Sprintf(clk, "gpu0", "graphics"),
		"joulie.hw.gpu.speed Hz Gauge 0 int:1500000000 " + fmt.Sprintf(clk, "gpu0", "memory"),
		"joulie.hw.gpu.speed Hz Gauge 0 int:1500000000 " + fmt.Sprintf(clk, "gpu1", "memory"),
		"joulie.hw.gpu.speed Hz Gauge 0 int:600000000 " + fmt.Sprintf(clk, "gpu1", "graphics"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1400000000 " + fmt.Sprintf(lim, "gpu0", "throttled", "graphics"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu0", "max", "graphics"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu0", "max", "memory"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu0", "throttled", "memory"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu1", "max", "graphics"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu1", "max", "memory"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:1500000000 " + fmt.Sprintf(lim, "gpu1", "throttled", "memory"),
		"joulie.hw.gpu.speed.limit Hz Gauge 0 int:900000000 " + fmt.Sprintf(lim, "gpu1", "throttled", "graphics"),
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:0 hw.id=cpu0,hw.type=cpu,joulie.hw.governor.native=performance,joulie.hw.governor.preset=performance",
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:0 hw.id=gpu1,hw.type=gpu,joulie.hw.governor.native=unlocked,joulie.hw.governor.preset=auto",
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:1 hw.id=cpu0,hw.type=cpu,joulie.hw.governor.native=schedutil,joulie.hw.governor.preset=balanced",
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:1 hw.id=cpu1,hw.type=cpu,joulie.hw.governor.native=schedutil,joulie.hw.governor.preset=balanced",
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:1 hw.id=gpu0,hw.type=gpu,joulie.hw.governor.native=unlocked,joulie.hw.governor.preset=auto",
		"joulie.hw.governor 1 Sum(non-monotonic) ledger int:1 hw.id=gpu1,hw.type=gpu,joulie.hw.governor.native=lgc=300,900,joulie.hw.governor.preset=efficient",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("governor-stage series:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A node without GPUs or governor state exports none of them.
	if g := testGolden(t, testResourceByInstance(t, d)["intel-0"], s.Nodes[0].StartTime,
		"hw.gpu.io", "joulie.hw.gpu.speed", "joulie.hw.gpu.speed.limit", "joulie.hw.governor"); len(g) != 0 {
		t.Errorf("intel-0 exports governor-stage series %v", g)
	}
}

// testEmuStart is the clock start of the emulators of the GT tests.
var testEmuStart = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// testEmulator starts an emulator over root with the real hwemu runner, one
// node per entry of nodes (node name to builtin profile name), and closes it
// at the end of the test.
func testEmulator(t *testing.T, root string, start time.Time, nodes map[string]string) *emulate.Emulator {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var members []clusterspec.Member
	for name, profile := range nodes {
		p, ok := profiles[profile]
		if !ok {
			t.Fatalf("no builtin profile %s", profile)
		}
		members = append(members, clusterspec.Member{Name: name, Pool: profile, Profile: p})
	}
	e, err := emulate.New(emulate.Config{Root: root, Cluster: "gt", Members: members, Start: start, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// testEmuTick ticks e at start+at and exports the snapshot.
func testEmuTick(t *testing.T, e *emulate.Emulator, at time.Time, loads map[string]hwemu.Load) (*emulate.Snapshot, otlpjson.MetricsData) {
	t.Helper()
	snap, err := e.Tick(at, loads, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, dropped := testRequest(t, snap)
	if dropped != 0 {
		t.Errorf("dropped %d values", dropped)
	}
	return snap, d
}

// testGPUIO is every hw.gpu.io point of node in d, keyed by
// <hw.id>/<direction>/<path>, and the start times the points carry.
func testGPUIO(t *testing.T, d otlpjson.MetricsData, node string) (map[string]int64, map[otlpjson.Uint64String]bool) {
	t.Helper()
	out, starts := map[string]int64{}, map[otlpjson.Uint64String]bool{}
	for _, p := range testPoints(testMetrics(t, testResourceByInstance(t, d)[node])[semconv.HwGPUIO.Name]) {
		a := testAttrs(p.Attributes)
		out[a[semconv.AttrHwID]+"/"+a[semconv.AttrNetworkIODirection]+"/"+a[semconv.AttrJoulieGPUIOPath]] = int64(*p.AsInt)
		starts[p.StartTimeUnixNano] = true
	}
	return out, starts
}

// GT02 (the EM03 analogue): across Close and a second New on the same root,
// whose nodes start their byte counters at 0 again, every hw.gpu.io point
// continues from its value before the restart and keeps the ledger start;
// the receiver of a peer transfer keeps counting after it.
func TestGT02GPUIONeverDecreasesAcrossARestart(t *testing.T) {
	root := t.TempDir()
	const node = "l40s-0"
	ring := map[string]hwemu.Load{node: {
		GPUUtil:       []float64{1, 1, 0, 0},
		PeerTransfers: []hwemu.PeerTransfer{{Src: 0, Dst: 1, BytesPerS: 4e9}, {Src: 1, Dst: 0, BytesPerS: 4e9}},
	}}
	e1 := testEmulator(t, root, testEmuStart, map[string]string{node: "nvidia-l40s-4gpu"})
	var d otlpjson.MetricsData
	for k := 1; k <= 3; k++ {
		_, d = testEmuTick(t, e1, testEmuStart.Add(time.Duration(k)*time.Second), ring)
	}
	before, _ := testGPUIO(t, d, node)
	if before["gpu1/receive/peer"] <= 0 || before["gpu0/transmit/peer"] <= 0 {
		t.Fatalf("hw.gpu.io before the restart %v: the transfer moved nothing", before)
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	restart := testEmuStart.Add(time.Minute)
	e2 := testEmulator(t, root, restart, map[string]string{node: "nvidia-l40s-4gpu"})
	_, d = testEmuTick(t, e2, restart.Add(time.Second), ring)
	after, starts := testGPUIO(t, d, node)
	if len(after) != len(before) || len(after) != 8 {
		t.Fatalf("hw.gpu.io points before %v, after %v; want 4 GPUs times 2 directions", before, after)
	}
	for k, v := range before {
		if after[k] < v {
			t.Errorf("hw.gpu.io %s went back across the restart: %d, then %d", k, v, after[k])
		}
	}
	if after["gpu1/receive/peer"] <= before["gpu1/receive/peer"] {
		t.Errorf("gpu1 received %d bytes before the restart and %d a second after it, want more", before["gpu1/receive/peer"], after["gpu1/receive/peer"])
	}
	if want := otlpjson.Uint64String(testEmuStart.UnixNano()); len(starts) != 1 || !starts[want] {
		t.Errorf("hw.gpu.io start times %v after the restart, want only the ledger start %d", starts, want)
	}
}

// testGovernor is every joulie.hw.governor point of component id of node in
// d, keyed by <preset>/<native>.
func testGovernor(t *testing.T, d otlpjson.MetricsData, node, id string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, p := range testPoints(testMetrics(t, testResourceByInstance(t, d)[node])[semconv.JoulieHwGovernor.Name]) {
		if a := testAttrs(p.Attributes); a[semconv.AttrHwID] == id {
			out[a[semconv.AttrJoulieGovernorPreset]+"/"+a[semconv.AttrJoulieGovernorNative]] = int64(*p.AsInt)
		}
	}
	return out
}

// GT03: a governor change emits 1 for the new state and 0 for the old one,
// in every export until the state changes again, and the component's other
// state is the only one at 0; GPUs that did not change report their state
// alone. Written through the emulated performance level file, as the agent
// writes it.
func TestGT03GovernorChangeEmitsOneForTheNewStateAndZeroForTheOld(t *testing.T) {
	const node = "w7900-0"
	e := testEmulator(t, t.TempDir(), testEmuStart, map[string]string{node: "amd-radeon-pro-w7900-4gpu"})
	snap, d := testEmuTick(t, e, testEmuStart.Add(time.Second), nil)
	if got, want := testGovernor(t, d, node, "gpu0"), map[string]int64{"auto/auto": 1}; mustJSON(got) != mustJSON(want) {
		t.Fatalf("gpu0 before any change: %v, want %v", got, want)
	}
	n := snap.Nodes[0]
	var level string
	for _, c := range n.Components {
		if c.ID == "gpu0" {
			level = filepath.Join(n.Root, layout.SysDir, c.SysfsPath, "power_dpm_force_performance_level")
		}
	}
	if level == "" {
		t.Fatal("gpu0 has no sysfs path")
	}
	write := func(v string) {
		t.Helper()
		if err := os.WriteFile(level, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("high")
	for k, at := range []time.Duration{2 * time.Second, 3 * time.Second} {
		_, d = testEmuTick(t, e, testEmuStart.Add(at), nil)
		if got, want := testGovernor(t, d, node, "gpu0"), map[string]int64{"sustained/high": 1, "auto/auto": 0}; mustJSON(got) != mustJSON(want) {
			t.Errorf("gpu0 export %d after high: %v, want %v", k+1, got, want)
		}
		if got, want := testGovernor(t, d, node, "gpu1"), map[string]int64{"auto/auto": 1}; mustJSON(got) != mustJSON(want) {
			t.Errorf("gpu1, unchanged, export %d: %v, want %v", k+1, got, want)
		}
	}

	write("auto")
	_, d = testEmuTick(t, e, testEmuStart.Add(4*time.Second), nil)
	if got, want := testGovernor(t, d, node, "gpu0"), map[string]int64{"auto/auto": 1, "sustained/high": 0}; mustJSON(got) != mustJSON(want) {
		t.Errorf("gpu0 back at auto: %v, want %v", got, want)
	}
}

// GT04: joulie.hw.gpu.speed has one point per clock domain the family has,
// with the clock the snapshot reports: graphics, memory, fabric and SoC on
// an AMD family with fclk and socclk tables, graphics and memory on NVIDIA.
func TestGT04GPUSpeedHasOnePointPerClockDomain(t *testing.T) {
	nodes := map[string]string{"w7900-0": "amd-radeon-pro-w7900-4gpu", "l40s-0": "nvidia-l40s-4gpu"}
	families := map[string][]string{
		"w7900-0": {semconv.ClockDomainFabric, semconv.ClockDomainGraphics, semconv.ClockDomainMemory, semconv.ClockDomainSoC},
		"l40s-0":  {semconv.ClockDomainGraphics, semconv.ClockDomainMemory},
	}
	e := testEmulator(t, t.TempDir(), testEmuStart, nodes)
	snap, d := testEmuTick(t, e, testEmuStart.Add(time.Second), nil)
	byID := testResourceByInstance(t, d)
	for _, n := range snap.Nodes {
		got := map[string]map[string]int64{}
		for _, p := range testPoints(testMetrics(t, byID[n.Name])[semconv.JoulieHwGPUSpeed.Name]) {
			a := testAttrs(p.Attributes)
			id, dom := a[semconv.AttrHwID], a[semconv.AttrJoulieClockDomain]
			if got[id] == nil {
				got[id] = map[string]int64{}
			}
			if _, dup := got[id][dom]; dup {
				t.Errorf("%s %s has two %s points", n.Name, id, dom)
			}
			got[id][dom] = int64(*p.AsInt)
		}
		gpus := 0
		for _, c := range n.Components {
			if c.Type != semconv.HwTypeGPU {
				continue
			}
			gpus++
			doms := make([]string, 0, len(got[c.ID]))
			for dom := range got[c.ID] {
				doms = append(doms, dom)
			}
			sort.Strings(doms)
			if mustJSON(doms) != mustJSON(families[n.Name]) {
				t.Errorf("%s %s clock domains %v, want %v", n.Name, c.ID, doms, families[n.Name])
			}
			if mustJSON(got[c.ID]) != mustJSON(c.ClockHz) {
				t.Errorf("%s %s joulie.hw.gpu.speed %v, want the snapshot's %v", n.Name, c.ID, got[c.ID], c.ClockHz)
			}
		}
		if gpus != 4 || len(got) != gpus {
			t.Errorf("%s: %d GPUs, joulie.hw.gpu.speed for %d", n.Name, gpus, len(got))
		}
	}
}

// joulie.hw.gpu.speed.limit has a max and a throttled point for every clock
// domain of every emulated GPU, the values of the snapshot, throttled never
// above max; an idle GPU at auto reports its maximum in effect, not its idle
// clock, and one W7900 GPU set to low reports a throttled graphics clock
// below its max.
func TestGPUSpeedLimitHasMaxAndThrottledPerClockDomain(t *testing.T) {
	nodes := map[string]string{"w7900-0": "amd-radeon-pro-w7900-4gpu", "l40s-0": "nvidia-l40s-4gpu"}
	root := t.TempDir()
	e := testEmulator(t, root, testEmuStart, nodes)
	levels, err := filepath.Glob(filepath.Join(root, "w7900-0", layout.SysDir, "bus", "pci", "devices", "*", layout.AMDGPUPerfLevelFile))
	if err != nil || len(levels) != 4 {
		t.Fatalf("w7900-0 performance level files %v (%v), want 4", levels, err)
	}
	sort.Strings(levels)
	if err := os.WriteFile(levels[1], []byte("low\n"), 0); err != nil {
		t.Fatal(err)
	}
	snap, d := testEmuTick(t, e, testEmuStart.Add(time.Second), nil)
	byID := testResourceByInstance(t, d)
	for _, n := range snap.Nodes {
		got := map[string]map[string]map[string]int64{} // hw.id, limit type, domain
		for _, p := range testPoints(testMetrics(t, byID[n.Name])[semconv.JoulieHwGPUSpeedLimit.Name]) {
			if p.AsInt == nil {
				t.Fatalf("%s joulie.hw.gpu.speed.limit point %+v is not asInt", n.Name, p)
			}
			a := testAttrs(p.Attributes)
			id, typ, dom := a[semconv.AttrHwID], a[semconv.AttrHwLimitType], a[semconv.AttrJoulieClockDomain]
			if got[id] == nil {
				got[id] = map[string]map[string]int64{}
			}
			if got[id][typ] == nil {
				got[id][typ] = map[string]int64{}
			}
			if _, dup := got[id][typ][dom]; dup {
				t.Errorf("%s %s has two %s %s points", n.Name, id, typ, dom)
			}
			got[id][typ][dom] = int64(*p.AsInt)
		}
		gpus, lowered := 0, 0
		for _, c := range n.Components {
			if c.Type != semconv.HwTypeGPU {
				continue
			}
			gpus++
			if len(c.ClockMaxHz) == 0 || len(c.ClockMaxHz) != len(c.ClockHz) || len(c.ClockLimitHz) != len(c.ClockHz) {
				t.Fatalf("%s %s snapshot clocks %v, maxima %v and %v: want one of each per domain", n.Name, c.ID, c.ClockHz, c.ClockMaxHz, c.ClockLimitHz)
			}
			want := map[string]map[string]int64{semconv.LimitTypeMax: c.ClockMaxHz, semconv.LimitTypeThrottled: c.ClockLimitHz}
			if mustJSON(got[c.ID]) != mustJSON(want) {
				t.Errorf("%s %s joulie.hw.gpu.speed.limit %v, want the snapshot's %v", n.Name, c.ID, got[c.ID], want)
			}
			for dom, hz := range c.ClockHz {
				if lim, top := c.ClockLimitHz[dom], c.ClockMaxHz[dom]; lim > top || hz > lim {
					t.Errorf("%s %s %s: speed %d, throttled %d, max %d; want speed <= throttled <= max", n.Name, c.ID, dom, hz, lim, top)
				}
			}
			if c.GovernorNative == "auto" && mustJSON(c.ClockLimitHz) != mustJSON(c.ClockMaxHz) {
				t.Errorf("%s %s at auto: throttled %v, want the maximum %v", n.Name, c.ID, c.ClockLimitHz, c.ClockMaxHz)
			}
			if c.GovernorNative == "low" {
				lowered++
				if g := semconv.ClockDomainGraphics; c.ClockLimitHz[g] >= c.ClockMaxHz[g] {
					t.Errorf("%s %s at low: throttled graphics %d, want below the maximum %d", n.Name, c.ID, c.ClockLimitHz[g], c.ClockMaxHz[g])
				}
			}
		}
		if want := map[string]int{"w7900-0": 1}[n.Name]; lowered != want {
			t.Errorf("%s: %d GPUs at low, want %d", n.Name, lowered, want)
		}
		if gpus != 4 || len(got) != gpus {
			t.Errorf("%s: %d GPUs, joulie.hw.gpu.speed.limit for %d", n.Name, gpus, len(got))
		}
	}
}

// testUnixNano is t as the timeUnixNano of a point; the test times are
// after the epoch.
func testUnixNano(t time.Time) otlpjson.Uint64String {
	return otlpjson.Uint64String(t.UnixNano())
}
