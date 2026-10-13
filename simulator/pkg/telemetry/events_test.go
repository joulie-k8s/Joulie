package telemetry

import (
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// testEventSnapshot is testSnapshot with one event of every kind on its
// nodes.
func testEventSnapshot() *emulate.Snapshot {
	s := testSnapshot()
	at := testNow.Add(-300 * time.Millisecond)
	s.Nodes[0].Events = []hwemu.Event{
		{Time: at, File: "sys/devices/virtual/powercap/intel-rapl/intel-rapl:1/constraint_0_power_limit_uw", Kind: hwemu.EventAccepted, Written: "120000000", Effective: "120000000", Component: "cpu1"},
		{Time: at.Add(time.Millisecond), File: "sys/devices/system/cpu/cpufreq/policy0/scaling_governor", Kind: hwemu.EventAccepted, Written: "powersave", Effective: "powersave", Component: "cpu0"},
		{Time: at.Add(2 * time.Millisecond), File: "sys/devices/virtual/powercap/intel-rapl/intel-rapl:0/constraint_0_power_limit_uw", Kind: hwemu.EventRevertedOutOfRange, Written: "999999999999", Effective: "165000000", Component: "cpu0"},
		{Time: at.Add(3 * time.Millisecond), File: "cpuinfo", Kind: hwemu.EventRestored, Written: "", Effective: "processor : 0"},
	}
	s.Nodes[1].Events = []hwemu.Event{
		{Time: at, File: "state/nvml/gpu1/power_limit_mw", Kind: hwemu.EventResetPersistenceOff, Written: "250000", Effective: "400000", Component: "gpu1"},
		{Time: at, File: "state/nvml/gpu1/power_limit_mw", Kind: "unknown-kind", Component: "gpu1"},
	}
	return s
}

// EV02: every record takes its eventName and severity from semconv, and
// carries only attributes semconv lists for that event.
func TestEV02EventNamesAndSeveritiesComeFromSemconv(t *testing.T) {
	recs := append(HardwareEvents(testEventSnapshot()),
		NodeMismatch("intel-0", semconv.ErrorTypeProfileMismatch, "intel-xeon-2s-rapl", "amd-epyc-2s-hsmp", testNow),
		NodeMismatch("vm-0", semconv.ErrorTypeNodeMissing, "vm-no-powercap", "", testNow))
	if len(recs) != 9 {
		t.Fatalf("%d records, want 2 node.ready, 5 hardware and 2 mismatch", len(recs))
	}
	table := map[string]semconv.Event{}
	for _, e := range semconv.Events() {
		table[e.Name] = e
	}
	d := logsRequest(recs, "smoke", testNow, testResource())
	seen := map[string]int{}
	for _, rl := range d.ResourceLogs {
		for _, lr := range rl.ScopeLogs[0].LogRecords {
			e, ok := table[lr.EventName]
			if !ok {
				t.Errorf("eventName %q is not in semconv", lr.EventName)
				continue
			}
			seen[lr.EventName]++
			if lr.SeverityNumber != e.SeverityNumber || lr.SeverityText != e.SeverityText {
				t.Errorf("%s severity %d %s, want %d %s", lr.EventName, lr.SeverityNumber, lr.SeverityText, e.SeverityNumber, e.SeverityText)
			}
			for _, kv := range lr.Attributes {
				if !e.HasAttribute(kv.Key) {
					t.Errorf("%s carries %s, which semconv does not list", lr.EventName, kv.Key)
				}
			}
			if lr.Body == nil || lr.Body.String == nil || *lr.Body.String == "" {
				t.Errorf("%s has no display body", lr.EventName)
			}
		}
	}
	want := map[string]int{
		semconv.EventSimNodeReady.Name: 2, semconv.EventSimCapApplied.Name: 1, semconv.EventSimSettingApplied.Name: 1,
		semconv.EventSimWriteRejected.Name: 2, semconv.EventSimCapReset.Name: 1, semconv.EventSimNodeMismatch.Name: 2,
	}
	if mustJSON(seen) != mustJSON(want) {
		t.Errorf("events %v, want %v", seen, want)
	}
	reset := testAttrs(recs[len(recs)-3].Attributes)
	if recs[len(recs)-3].Event.Name != semconv.EventSimCapReset.Name || reset[semconv.AttrHwID] != "gpu1" || reset[semconv.AttrHwType] != semconv.HwTypeGPU ||
		reset[semconv.AttrSimValueWritten] != "250000" || reset[semconv.AttrSimValueEffective] != "400000" {
		t.Errorf("cap.reset attributes %v", reset)
	}
	rejected := testAttrs(recs[3].Attributes)
	if rejected[semconv.AttrErrorType] != semconv.ErrorTypeRevertedOutOfRange || rejected[semconv.AttrSimOutcome] != hwemu.EventRevertedOutOfRange || rejected[semconv.AttrHwType] != semconv.HwTypeCPU {
		t.Errorf("write.rejected attributes %v", rejected)
	}
	if restored := testAttrs(recs[4].Attributes); restored[semconv.AttrHwID] != "" || restored[semconv.AttrErrorType] != semconv.ErrorTypeRestored {
		t.Errorf("a restored node file has attributes %v; want no hw.id and error.type restored", restored)
	}
	mm := testAttrs(recs[len(recs)-2].Attributes)
	if mm[semconv.AttrErrorType] != semconv.ErrorTypeProfileMismatch || mm[semconv.AttrSimNodeProfile] != "amd-epyc-2s-hsmp" {
		t.Errorf("node.mismatch attributes %v", mm)
	}
	if missing := testAttrs(recs[len(recs)-1].Attributes); missing[semconv.AttrSimNodeProfile] != "" {
		t.Errorf("node_missing carries a Node profile: %v", missing)
	}
	// Records go on their node's resource.
	for _, rl := range d.ResourceLogs {
		a := testAttrs(rl.Resource.Attributes)
		if a[semconv.AttrK8sNodeName] == "" || a[semconv.AttrServiceInstanceID] != a[semconv.AttrK8sNodeName] || rl.SchemaURL != semconv.SchemaURL {
			t.Errorf("event resource %v", a)
		}
	}
	if len(d.ResourceLogs) != 3 {
		t.Errorf("%d resources, want intel-0, nvl-0, vm-0", len(d.ResourceLogs))
	}
	// A record with no node goes on the simulator's resource.
	d = logsRequest([]EventRecord{{Event: semconv.EventSimNodeReady, Time: testNow}}, "smoke", testNow, testResource())
	if a := testAttrs(d.ResourceLogs[0].Resource.Attributes); a[semconv.AttrServiceInstanceID] != "joulie-simulator-0" || a[semconv.AttrK8sNodeName] != "" {
		t.Errorf("node-less record resource %v", a)
	}
}

// EV03: node.ready appears once per node on the first snapshot after a
// start, with joulie.sim.tree.resumed and the number of entries a resume
// put back, and never on later snapshots.
func TestEV03NodeReadyOncePerStart(t *testing.T) {
	first := testSnapshot()
	first.Nodes[2].Started = true
	ready := map[string]map[string]string{}
	for _, r := range HardwareEvents(first) {
		if r.Event.Name != semconv.EventSimNodeReady.Name {
			continue
		}
		if _, dup := ready[r.Node]; dup {
			t.Errorf("two node.ready for %s", r.Node)
		}
		ready[r.Node] = testAttrs(r.Attributes)
		if !r.Time.Equal(first.Time) {
			t.Errorf("node.ready at %v, want the snapshot time", r.Time)
		}
	}
	want := map[string]map[string]string{
		"intel-0": {semconv.AttrSimProfile: "intel-xeon-2s-rapl", semconv.AttrSimPool: "intel", semconv.AttrSimTreeResumed: "false", semconv.AttrSimTreeAdded: "0"},
		"nvl-0":   {semconv.AttrSimProfile: "nvidia-h100-nvl-8gpu", semconv.AttrSimPool: "nvl", semconv.AttrSimTreeResumed: "true", semconv.AttrSimTreeAdded: "2"},
		"vm-0":    {semconv.AttrSimProfile: "vm-no-powercap", semconv.AttrSimPool: "vm", semconv.AttrSimTreeResumed: "true", semconv.AttrSimTreeAdded: "0"},
	}
	if mustJSON(ready) != mustJSON(want) {
		t.Errorf("node.ready attributes\n%v\nwant\n%v", ready, want)
	}
	later := testSnapshot()
	later.Seq++
	for i := range later.Nodes {
		later.Nodes[i].Started = false
	}
	for _, r := range HardwareEvents(later) {
		if r.Event.Name == semconv.EventSimNodeReady.Name {
			t.Errorf("node.ready for %s on a later snapshot", r.Node)
		}
	}
	if HardwareEvents(nil) != nil {
		t.Error("HardwareEvents(nil) is not empty")
	}
}
