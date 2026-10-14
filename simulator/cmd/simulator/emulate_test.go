package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// emulateTestStart is when the emulated nodes' clocks start in these tests;
// ticks are at whole seconds after it, never the wall clock.
var emulateTestStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// emulateTestSpec is a small spec: a two-socket Intel host, an 8-GPU NVL
// host and a VM, on a constant background load.
const emulateTestSpec = `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: setest}
spec:
  defaults:
    labels: {joulie.io/managed: "true"}
    baseLoad: {cpuUtil: 0.6, gpuUtil: 0.5, memoryIntensity: 0.2}
  facility: {ambientC: 20, amplitudeC: 0, periodH: 24, coolingZones: [{name: zone-a, offsetC: 2}]}
  pools:
  - {name: intel, profile: intel-xeon-2s-rapl, count: 1, rack: rack-1, coolingZone: zone-a}
  - {name: nvl, profile: nvidia-h100-nvl-8gpu, count: 1, rack: rack-1, coolingZone: zone-a}
  - {name: vm, profile: vm-no-powercap, count: 1}
`

// emulateTest is a simulator in emulate mode over real hwemu trees.
type emulateTest struct {
	s       *simulator
	rt      *emulateRuntime
	reg     *prometheus.Registry
	root    string
	fileDir string
}

// emulateTestConfig adjusts the options of newEmulateTest.
type emulateTestConfig struct {
	spec      string // YAML; emulateTestSpec when empty
	specPath  string // a spec file instead of spec
	root      string // a fresh directory when empty
	start     time.Time
	exporters []string
}

// newEmulateTest starts emulate mode as startEmulate does, without running
// the emulator: tests tick it with emulateTestTick.
func newEmulateTest(t *testing.T, c emulateTestConfig) *emulateTest {
	t.Helper()
	t.Setenv(envProfilesDir, "")
	path := c.specPath
	if path == "" {
		spec := c.spec
		if spec == "" {
			spec = emulateTestSpec
		}
		path = filepath.Join(t.TempDir(), "spec.yaml")
		if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := c.root
	if root == "" {
		root = filepath.Join(t.TempDir(), "hwemu")
	}
	start := c.start
	if start.IsZero() {
		start = emulateTestStart
	}
	reg := prometheus.NewRegistry()
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 1000, reg)
	s.facilityBaseAmbientTempC, s.facilityTempAmplitudeC, s.facilityTempPeriodH = 20, 5, 24
	fileDir := t.TempDir()
	rt, err := s.newEmulateRuntime(emulateOptions{
		SpecPath:   path,
		Root:       root,
		Tick:       time.Second,
		Exporters:  c.exporters,
		Telemetry:  telemetry.Config{FileDir: fileDir},
		Resource:   telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "sim-test"},
		Start:      start,
		Registerer: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.emu = rt
	et := &emulateTest{s: s, rt: rt, reg: reg, root: root, fileDir: fileDir}
	t.Cleanup(et.close)
	return et
}

// close releases the root, as stopEmulate does.
func (et *emulateTest) close() {
	_ = et.rt.emu.Close()
	_ = et.rt.sink.Close()
}

// tick runs one emulator tick at start+at with the simulator's callbacks
// and publishes the snapshot, as Run does.
func (et *emulateTest) tick(t *testing.T, at time.Time) *emulate.Snapshot {
	t.Helper()
	loads, base := et.s.emulateLoads()
	snap, err := et.rt.emu.Tick(at, loads, base, et.s.emulateFacility)
	if err != nil {
		t.Fatalf("tick at %v: %v", at, err)
	}
	et.s.publishEmulateSnapshot(snap)
	return snap
}

// node returns the spec node of pool, named by the spec's expansion.
func (et *emulateTest) node(t *testing.T, pool string) string {
	t.Helper()
	for _, n := range et.rt.names {
		if m, _ := et.rt.emu.Member(n); m.Pool == pool {
			return n
		}
	}
	t.Fatalf("no spec node in pool %s; nodes %v", pool, et.rt.names)
	return ""
}

// stateJSON is node's legacy state, encoded.
func (et *emulateTest) stateJSON(t *testing.T, node string) string {
	t.Helper()
	et.s.mu.RLock()
	defer et.s.mu.RUnlock()
	b, err := json.Marshal(et.s.state[node])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func emulateTestDo(t *testing.T, h http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, body))
	return w
}

func TestSE01CheckModesRefusesWhatEmulateCannotRun(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string // substrings of the error; none means no error
	}{
		{"legacy", map[string]string{"SIM_STANDALONE": "true", workload.EnvTracePath: "/t.jsonl"}, nil},
		{"emulate with a spec", map[string]string{envEmulate: "true", envClusterSpec: "/s.yaml"}, nil},
		{"emulate is off", map[string]string{envEmulate: "false", "SIM_STANDALONE": "true"}, nil},
		{"with standalone", map[string]string{envEmulate: "true", envClusterSpec: "/s.yaml", "SIM_STANDALONE": "1"}, []string{"SIM_STANDALONE"}},
		{"without a spec", map[string]string{envEmulate: "yes", envClusterSpec: "  "}, []string{"SIM_CLUSTER_SPEC"}},
		{"with a v1 trace", map[string]string{envEmulate: "TRUE", envClusterSpec: "/s.yaml", workload.EnvTracePath: "/t.jsonl"}, nil},
		{"with a legacy trace", map[string]string{envEmulate: "TRUE", envClusterSpec: "/s.yaml", workload.EnvTracePath: "/t.jsonl", "SIM_WORKLOAD_MODEL": " legacy "}, []string{"SIM_WORKLOAD_TRACE_PATH", "SIM_WORKLOAD_MODEL=legacy"}},
		{"all three", map[string]string{envEmulate: "true", "SIM_STANDALONE": "true", workload.EnvTracePath: "/t", "SIM_WORKLOAD_MODEL": "legacy"}, []string{"SIM_STANDALONE", "SIM_CLUSTER_SPEC", "SIM_WORKLOAD_TRACE_PATH"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkModes(func(k string) string { return tc.env[k] })
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("checkModes: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkModes accepted %v", tc.env)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %s", err, w)
				}
			}
		})
	}
}

func TestSE02ControlOfAnEmulatedNodeIsAConflict(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	et.tick(t, emulateTestStart.Add(time.Second))
	node := et.node(t, "intel")
	before := et.stateJSON(t, node)
	seq := et.rt.emu.Latest().Seq
	mux := et.s.newMux()

	body := `{"action":"rapl.set_power_cap_watts","capWatts":100}`
	w := emulateTestDo(t, mux, http.MethodPost, "/control/"+node, strings.NewReader(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("POST /control/%s: status %d, want 409; body %s", node, w.Code, w.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != false || resp["result"] != "conflict" || resp["message"] != node+" is emulated; write its files" {
		t.Errorf("409 body %v", resp)
	}
	if after := et.stateJSON(t, node); after != before {
		t.Errorf("the control changed the node's state:\nbefore %s\nafter  %s", before, after)
	}
	if got := et.rt.emu.Latest().Seq; got != seq {
		t.Errorf("Seq %d after the control, want %d", got, seq)
	}
	if n := emulateCounter(t, et.reg, "joulie_sim_controls_total", map[string]string{"node": node}); n != 0 {
		t.Errorf("joulie_sim_controls_total{node=%s} = %v, want 0", node, n)
	}

	// A node outside the spec keeps the modelled path in the same simulator.
	w = emulateTestDo(t, mux, http.MethodPost, "/control/legacy-0", strings.NewReader(body))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /control/legacy-0: status %d, want 200; body %s", w.Code, w.Body)
	}
}

func TestSE03TelemetryReadsDoNotStepEmulatedNodes(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	snap := et.tick(t, emulateTestStart.Add(time.Second))
	mux := et.s.newMux()
	for _, pool := range []string{"intel", "nvl"} {
		node := et.node(t, pool)
		before := et.stateJSON(t, node)
		var n emulate.NodeSnapshot
		for _, x := range snap.Nodes {
			if x.Name == node {
				n = x
			}
		}
		var pkgW, gpuW float64
		for _, c := range n.Components {
			switch c.Type {
			case semconv.HwTypeCPU:
				pkgW += c.PowerW
			case semconv.HwTypeGPU:
				gpuW += c.PowerW
			}
		}
		// The default route answers with OTLP JSON and ?format=legacy with
		// the legacy JSON; reads of neither may step the node.
		for _, path := range []string{"/telemetry/" + node, "/telemetry/" + node + "?format=legacy"} {
			legacy := strings.HasSuffix(path, "?format=legacy")
			for i := 0; i < 50; i++ {
				w := emulateTestDo(t, mux, http.MethodGet, path, nil)
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s: status %d; body %s", path, w.Code, w.Body)
				}
				if i > 0 {
					continue
				}
				if !legacy {
					var req struct {
						ResourceMetrics []json.RawMessage `json:"resourceMetrics"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil || len(req.ResourceMetrics) == 0 {
						t.Errorf("GET %s: %v, %d resourceMetrics; want an OTLP JSON export", path, err, len(req.ResourceMetrics))
					}
					continue
				}
				var resp struct {
					Emulated          bool    `json:"emulated"`
					Seq               uint64  `json:"seq"`
					PackagePowerWatts float64 `json:"packagePowerWatts"`
					HostPowerWatts    float64 `json:"hostPowerWatts"`
					CPU               struct {
						PackagePowerWatts float64 `json:"packagePowerWatts"`
					} `json:"cpu"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if !resp.Emulated || resp.Seq != snap.Seq {
					t.Errorf("%s: emulated %v seq %d, want true and %d", node, resp.Emulated, resp.Seq, snap.Seq)
				}
				emulateNear(t, node+" packagePowerWatts", resp.PackagePowerWatts, pkgW+gpuW, 0.006)
				emulateNear(t, node+" cpu.packagePowerWatts", resp.CPU.PackagePowerWatts, pkgW, 0.006)
				emulateNear(t, node+" hostPowerWatts", resp.HostPowerWatts, n.Host.PowerW, 0.006)
				if pkgW <= 0 || (pool == "nvl" && gpuW <= 0) {
					t.Errorf("%s: package power %v W, GPU power %v W, want both above 0 under load", node, pkgW, gpuW)
				}
			}
			if got := et.rt.emu.Latest().Seq; got != snap.Seq {
				t.Errorf("Seq %d after 50 reads of %s, want %d", got, path, snap.Seq)
			}
			if after := et.stateJSON(t, node); after != before {
				t.Errorf("50 reads of %s changed %s's mirrored state:\nbefore %s\nafter  %s", path, node, before, after)
			}
		}
	}
}

func TestSE04OTLPTelemetryOfAnEmulatedNodeUsesSemconvNames(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	mux := et.s.newMux()
	node := et.node(t, "intel")
	if w := emulateTestDo(t, mux, http.MethodGet, "/otlp/telemetry/"+node, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("before the first tick: status %d, want 503", w.Code)
	}
	et.tick(t, emulateTestStart.Add(time.Second))
	et.tick(t, emulateTestStart.Add(2*time.Second))

	w := emulateTestDo(t, mux, http.MethodGet, "/otlp/telemetry/"+node, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d; body %s", w.Code, w.Body)
	}
	md, err := otlpjson.DecodeMetricsStrict(w.Body.Bytes())
	if err != nil {
		t.Fatalf("strict decode: %v\n%s", err, w.Body)
	}
	if len(md.ResourceMetrics) != 1 {
		t.Fatalf("%d resources, want 1", len(md.ResourceMetrics))
	}
	rm := md.ResourceMetrics[0]
	attrs := map[string]string{}
	for _, kv := range rm.Resource.Attributes {
		if kv.Value.String != nil {
			attrs[kv.Key] = *kv.Value.String
		}
	}
	for k, want := range map[string]string{
		semconv.AttrServiceInstanceID: node,
		semconv.AttrK8sNodeName:       node,
		semconv.AttrHostName:          node,
		semconv.AttrHostID:            telemetry.HostID(node),
		semconv.AttrK8sClusterName:    "setest",
		semconv.AttrServiceName:       telemetry.DefaultServiceName,
	} {
		if attrs[k] != want {
			t.Errorf("resource %s = %q, want %q", k, attrs[k], want)
		}
	}
	known := map[string]semconv.Metric{}
	for _, m := range semconv.Metrics() {
		known[m.Name] = m
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != telemetry.ScopeName {
			t.Errorf("scope %q, want %q", sm.Scope.Name, telemetry.ScopeName)
		}
		for _, m := range sm.Metrics {
			def, ok := known[m.Name]
			if !ok {
				t.Errorf("metric %q is not in semconv.Metrics()", m.Name)
				continue
			}
			if m.Unit != def.Unit {
				t.Errorf("metric %s unit %q, want %q", m.Name, m.Unit, def.Unit)
			}
			seen[m.Name] = true
		}
	}
	for _, want := range []semconv.Metric{semconv.HwHostEnergy, semconv.HwHostPower, semconv.HwEnergy, semconv.HwPower} {
		if !seen[want.Name] {
			t.Errorf("no %s in the export; got %v", want.Name, seen)
		}
	}

	// A VM exports no hw.* metric: no resource at all.
	w = emulateTestDo(t, mux, http.MethodGet, "/otlp/telemetry/"+et.node(t, "vm"), nil)
	vm, err := otlpjson.DecodeMetricsStrict(w.Body.Bytes())
	if err != nil || w.Code != http.StatusOK || len(vm.ResourceMetrics) != 0 {
		t.Errorf("VM: status %d, %v, %d resources, want 200 and none", w.Code, err, len(vm.ResourceMetrics))
	}
}

func TestSE07SpecNodeKeepsItsProfileCapacity(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	s := et.s
	sel, err := labels.Parse("example.com/simulated=true")
	if err != nil {
		t.Fatal(err)
	}
	s.selector = sel
	node := et.node(t, "intel")
	m, _ := et.rt.emu.Member(node)
	wantCPU := float64(m.Profile.Node.Capacity.CPU.V)
	nodeLabels := map[string]map[string]string{
		// Labels of another machine, and none matching the selector.
		node: {
			"joulie.io/hw.cpu-model": "AMD EPYC 9654",
			"joulie.io/hw.cpu-cores": "7",
			"joulie.io/hw.gpu-count": "4",
		},
		"legacy-0": {"example.com/simulated": "true"},
	}
	counts := map[string]int{node: 3, "legacy-0": 3}
	var pods []runningPodInfo
	for _, n := range []string{node, "legacy-0"} {
		for i := 0; i < 3; i++ {
			pods = append(pods, runningPodInfo{Namespace: "default", Name: n + "-p" + string(rune('a'+i)), Node: n, IntentClass: "performance"})
		}
	}
	for round := 0; round < 2; round++ {
		s.refreshNodeStateFromKubeData(counts, pods, nodeLabels)
		s.mu.RLock()
		st := s.state[node]
		if st == nil {
			s.mu.RUnlock()
			t.Fatalf("no state for spec node %s", node)
		}
		if st.CPUCapacityCores != wantCPU || st.GPUCapacityDevices != 0 {
			t.Errorf("round %d: capacity %v CPUs %v GPUs, want the profile's %v and 0", round, st.CPUCapacityCores, st.GPUCapacityDevices, wantCPU)
		}
		if st.CPUUtil != 0 || st.GPUUtil != 0 || st.PodsRunning != 3 {
			t.Errorf("round %d: util %v/%v with %d pods, want 0 and 3 pods", round, st.CPUUtil, st.GPUUtil, st.PodsRunning)
		}
		if _, ok := s.nodeModels[node]; ok {
			t.Errorf("round %d: the spec node got a label-based model", round)
		}
		legacy := s.state["legacy-0"]
		if legacy == nil || legacy.CPUUtil <= 0 {
			t.Errorf("round %d: legacy node state %+v, want the pod-count utilization", round, legacy)
		}
		s.mu.RUnlock()
	}
}

func TestSE11EmulateMuxServesExportersAndDropsTheFakePrometheus(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{exporters: []string{"kepler-0.7"}})
	et.tick(t, emulateTestStart.Add(time.Second))
	et.tick(t, emulateTestStart.Add(2*time.Second))
	mux := et.s.newMux()

	w := emulateTestDo(t, mux, http.MethodGet, "/exporters/kepler-0.7/metrics", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "kepler_node_platform_joules_total") {
		t.Errorf("/exporters/kepler-0.7/metrics: status %d, body without kepler_node_platform_joules_total:\n%.400s", w.Code, w.Body)
	}
	if w := emulateTestDo(t, mux, http.MethodGet, "/exporters/dcgm-4.8.4/metrics", nil); w.Code != http.StatusNotFound {
		t.Errorf("a disabled exporter: status %d, want 404", w.Code)
	}
	w = emulateTestDo(t, mux, http.MethodGet, "/debug/emulate", nil)
	var snap emulate.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil || w.Code != http.StatusOK || len(snap.Nodes) != 3 || snap.Seq != 2 {
		t.Errorf("/debug/emulate: status %d, %v, %d nodes, seq %d; want 200, 3 nodes, seq 2", w.Code, err, len(snap.Nodes), snap.Seq)
	}
	if w := emulateTestDo(t, mux, http.MethodGet, "/api/v1/query?query=pue", nil); w.Code != http.StatusNotFound {
		t.Errorf("/api/v1/query in emulate mode: status %d, want 404", w.Code)
	}
	if w := emulateTestDo(t, mux, http.MethodGet, "/readyz", nil); w.Code != http.StatusOK {
		t.Errorf("/readyz after a tick: status %d, want 200", w.Code)
	}

	// The legacy mux keeps its routes and gains none.
	legacy := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry()).newMux()
	for _, path := range []string{"/healthz", "/telemetry/n1", "/otlp/telemetry/n1", "/state/n1", "/debug/nodes", "/debug/jobs", "/debug/events", "/debug/energy", "/api/v1/query?query=pue"} {
		if w := emulateTestDo(t, legacy, http.MethodGet, path, nil); w.Code == http.StatusNotFound {
			t.Errorf("legacy %s: 404", path)
		}
	}
	for _, path := range []string{"/exporters/kepler-0.7/metrics", "/debug/emulate", "/readyz"} {
		if w := emulateTestDo(t, legacy, http.MethodGet, path, nil); w.Code != http.StatusNotFound {
			t.Errorf("legacy %s: status %d, want 404", path, w.Code)
		}
	}
}

func TestSE13NodeMismatchIsEmittedOncePerChange(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	intel, nvl, vm := et.node(t, "intel"), et.node(t, "nvl"), et.node(t, "vm")
	profileOf := func(n string) string {
		m, _ := et.rt.emu.Member(n)
		return m.Profile.Name
	}
	nodes := map[string]map[string]string{
		// intel is missing; nvl names another profile.
		nvl: {clusterspec.AnnotationProfile: "nvidia-h100-sxm-8gpu"},
		vm:  {clusterspec.AnnotationProfile: profileOf(vm)},
	}
	poll := func(n int) {
		for i := 0; i < n; i++ {
			et.s.checkEmulatedNodes(nodes)
		}
	}
	poll(3)
	want := map[string]string{intel: semconv.ErrorTypeNodeMissing, nvl: semconv.ErrorTypeProfileMismatch}
	if got := emulateMismatches(t, et); !emulateSameMap(got, want) {
		t.Fatalf("after 3 polls: node.mismatch %v, want one each %v", got, want)
	}

	// intel appears with its profile; vm disappears; nvl stays wrong.
	nodes[intel] = map[string]string{clusterspec.AnnotationProfile: profileOf(intel)}
	delete(nodes, vm)
	poll(3)
	want[vm] = semconv.ErrorTypeNodeMissing
	if got := emulateMismatches(t, et); !emulateSameMap(got, want) {
		t.Fatalf("after the change: node.mismatch %v, want %v (one more, for vm)", got, want)
	}

	// The events file has the same records, one per change.
	recs := emulateEventsFile(t, filepath.Join(et.fileDir, telemetry.EventsFile), semconv.EventSimNodeMismatch.Name)
	if len(recs) != 3 {
		t.Errorf("%s holds %d node.mismatch records, want 3", telemetry.EventsFile, len(recs))
	}
}

func TestSE14ReadyzAfterTheFirstSnapshot(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	mux := et.s.newMux()
	if w := emulateTestDo(t, mux, http.MethodGet, "/readyz", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before the first snapshot: status %d, want 503", w.Code)
	}
	if w := emulateTestDo(t, mux, http.MethodGet, "/debug/emulate", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/debug/emulate before the first snapshot: status %d, want 503", w.Code)
	}
	et.tick(t, emulateTestStart.Add(time.Second))
	if w := emulateTestDo(t, mux, http.MethodGet, "/readyz", nil); w.Code != http.StatusOK {
		t.Fatalf("/readyz after the first snapshot: status %d, want 200", w.Code)
	}
}

// emulateMismatches is the error.type of every node.mismatch in
// /debug/events, by node; a node listed twice fails the test.
func emulateMismatches(t *testing.T, et *emulateTest) map[string]string {
	t.Helper()
	out := map[string]string{}
	et.s.mu.RLock()
	defer et.s.mu.RUnlock()
	for _, e := range et.s.events {
		if e.Kind != semconv.EventSimNodeMismatch.Name {
			continue
		}
		if _, dup := out[e.Node]; dup {
			t.Errorf("node %s has more than one node.mismatch: %v", e.Node, et.s.events)
		}
		out[e.Node], _ = e.Payload[semconv.AttrErrorType].(string)
	}
	return out
}

func emulateSameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// emulateEventsFile returns the log records named event in an events.jsonl.
func emulateEventsFile(t *testing.T, path, event string) []otlpjson.LogRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []otlpjson.LogRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		d, err := otlpjson.DecodeLogsStrict(sc.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, rl := range d.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					if r.EventName == event {
						out = append(out, r)
					}
				}
			}
		}
	}
	return out
}

// emulateCounter sums the counter name over the series whose labels
// include want.
func emulateCounter(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if got[k] != v {
					continue metric
				}
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

func emulateNear(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (within %v)", what, got, want, tol)
	}
}

func TestEmulateOptionsFromEnvReadsTheEmulateVariables(t *testing.T) {
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	s.persistDir = "/tmp/debug-dir"
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	o, err := s.emulateOptionsFromEnv(lookup(map[string]string{envClusterSpec: " /s.yaml "}))
	if err != nil {
		t.Fatal(err)
	}
	if o.SpecPath != "/s.yaml" || o.Root != defaultHwemuRoot || o.FakeSMISource != defaultFakeSMISource || o.Tick != time.Second || o.Workers != 0 || len(o.Exporters) != 0 || o.Kepler012GPU {
		t.Errorf("defaults: %+v", o)
	}
	if o.Telemetry.FileDir != "/tmp/debug-dir" {
		t.Errorf("events directory %q, want the debug persistence directory", o.Telemetry.FileDir)
	}

	o, err = s.emulateOptionsFromEnv(lookup(map[string]string{
		envClusterSpec: "/s.yaml", envHwemuRoot: "/r", envFakeSMISource: "", envEmulateTick: "250ms",
		envEmulateWorkers: "3", envExporters: "kepler-0.7, dcgm-4.8.4", envDCGMHostnameLabel: "Hostname",
		envKepler012GPU: "on", telemetry.EnvFileDir: "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o.Root != "/r" || o.FakeSMISource != "" || o.Tick != 250*time.Millisecond || o.Workers != 3 ||
		len(o.Exporters) != 2 || o.DCGMHostnameLabel != "Hostname" || !o.Kepler012GPU || o.Telemetry.FileDir != "" {
		t.Errorf("set: %+v; a set but empty %s skips the install and an empty %s writes no file", o, envFakeSMISource, telemetry.EnvFileDir)
	}

	_, err = s.emulateOptionsFromEnv(lookup(map[string]string{envEmulateTick: "0s", envEmulateWorkers: "-1", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}))
	for _, want := range []string{envEmulateTick, envEmulateWorkers, "grpc"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %s", err, want)
		}
	}
}

// One pod poll lists the pods once and the Nodes once: the node state, the
// per-node gauges and the check of the spec nodes' Nodes all read those two
// lists. The check still sees each Node's profile annotation.
func TestPollListsPodsAndNodesOnce(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	missing := et.rt.names[0]
	objs := []runtime.Object{&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
		Spec:       corev1.PodSpec{NodeName: et.rt.names[1]},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}}
	for _, name := range et.rt.names[1:] {
		m, _ := et.rt.emu.Member(name)
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{clusterspec.AnnotationProfile: m.Profile.Name}}})
	}
	fake := k8sfake.NewSimpleClientset(objs...)
	et.s.pollOnce(fake)
	lists := map[string]int{}
	for _, a := range fake.Actions() {
		if a.GetVerb() == "list" {
			lists[a.GetResource().Resource]++
		}
	}
	if lists["pods"] != 1 || lists["nodes"] != 1 || len(lists) != 2 {
		t.Errorf("one poll made lists %v, want one of pods and one of nodes", lists)
	}
	if got, want := emulateMismatches(t, et), map[string]string{missing: semconv.ErrorTypeNodeMissing}; !emulateSameMap(got, want) {
		t.Errorf("node.mismatch %v after the poll, want only %v", got, want)
	}
	et.s.mu.RLock()
	running := et.s.state[et.rt.names[1]].PodsRunning
	et.s.mu.RUnlock()
	if running != 1 {
		t.Errorf("%s runs %d pods after the poll, want 1", et.rt.names[1], running)
	}
}
