package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestNormalizeCPUVendor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"AMD", "AuthenticAMD"},
		{"AuthenticAMD", "AuthenticAMD"},
		{"intel", "GenuineIntel"},
		{"GenuineIntel", "GenuineIntel"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeCPUVendor(tt.in); got != tt.want {
			t.Fatalf("normalizeCPUVendor(%q) got=%q want=%q", tt.in, got, tt.want)
		}
	}
}

func TestDiscoverCPUVendorPrefersNFDVendorLabel(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"feature.node.kubernetes.io/cpu-vendor":            "AMD",
		"feature.node.kubernetes.io/cpu-model.vendor_id":   "GenuineIntel",
		"feature.node.kubernetes.io/pci-0300_10de.present": "true",
	}
	if got := discoverCPUVendor(labels); got != "AuthenticAMD" {
		t.Fatalf("discoverCPUVendor got=%q", got)
	}
}

func TestDiscoverHardwareGPUVendors(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"feature.node.kubernetes.io/cpu-model.vendor_id":   "GenuineIntel",
		"feature.node.kubernetes.io/pci-0300_10de.present": "true",
		"feature.node.kubernetes.io/pci-0302_1002.present": "true",
	}
	hw := discoverHardwareFromLabels(labels)
	if hw.CPUVendor != "GenuineIntel" {
		t.Fatalf("cpu vendor got=%q", hw.CPUVendor)
	}
	if len(hw.GPUVendors) != 2 {
		t.Fatalf("gpu vendors len got=%d want=2", len(hw.GPUVendors))
	}
}

func TestCPUIndexFromPath(t *testing.T) {
	t.Parallel()
	if got, ok := dvfs.CPUIndexFromPath("/host-sys/devices/system/cpu/cpufreq/policy11"); !ok || got != 11 {
		t.Fatalf("policy path parse failed: got=%d ok=%v", got, ok)
	}
	if got, ok := dvfs.CPUIndexFromPath("/host-sys/devices/system/cpu/cpu7/cpufreq"); !ok || got != 7 {
		t.Fatalf("cpu path parse failed: got=%d ok=%v", got, ok)
	}
	if _, ok := dvfs.CPUIndexFromPath("/not/a/cpu/path"); ok {
		t.Fatalf("invalid path should not parse")
	}
}

func TestParseNodeTwinAsProfileWithIntAndFloatCaps(t *testing.T) {
	t.Parallel()
	intObj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata": map[string]any{
				"name": "node-a",
			},
			"spec": map[string]any{
				"nodeName": "node-a",
				"profile":  "eco",
				"cpu": map[string]any{
					"packagePowerCapWatts": int64(120),
				},
			},
		},
	}
	npInt := parseNodeTwinAsProfile(intObj)
	if npInt.PowerWatts == nil || *npInt.PowerWatts != 120 {
		t.Fatalf("int cap parse failed: %#v", npInt.PowerWatts)
	}

	floatObj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata": map[string]any{
				"name": "node-b",
			},
			"spec": map[string]any{
				"nodeName": "node-b",
				"profile":  "performance",
				"cpu": map[string]any{
					"packagePowerCapWatts": 5000.0,
				},
			},
		},
	}
	npFloat := parseNodeTwinAsProfile(floatObj)
	if npFloat.PowerWatts == nil || *npFloat.PowerWatts != 5000 {
		t.Fatalf("float cap parse failed: %#v", npFloat.PowerWatts)
	}
}

func TestResolveTelemetryConfigFromEnv(t *testing.T) {
	t.Setenv("TELEMETRY_CPU_SOURCE", "http")
	t.Setenv("TELEMETRY_CPU_CONTROL", "http")
	t.Setenv("TELEMETRY_CPU_HTTP_ENDPOINT", "http://sim.local/nodes/{node}")
	t.Setenv("TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT", "http://sim.local/control/{node}")
	t.Setenv("TELEMETRY_CPU_CONTROL_MODE", "dvfs")
	t.Setenv("TELEMETRY_HTTP_TIMEOUT_SECONDS", "5")

	cfg := resolveTelemetryConfigFromEnv()
	if cfg == nil {
		t.Fatalf("expected non-nil config")
	}
	if cfg.CPUSourceType != "http" || cfg.HTTPEndpoint != "http://sim.local/nodes/{node}" || cfg.TimeoutSeconds != 5 {
		t.Fatalf("unexpected telemetry config: %#v", cfg)
	}
	if cfg.CPUControlType != "http" || cfg.ControlHTTPEndpoint != "http://sim.local/control/{node}" || cfg.ControlMode != "dvfs" {
		t.Fatalf("unexpected telemetry config: %#v", cfg)
	}
}

func TestResolveTelemetryConfigFromEnvReturnsNilWhenEmpty(t *testing.T) {
	t.Setenv("TELEMETRY_CPU_SOURCE", "")
	t.Setenv("TELEMETRY_CPU_CONTROL", "")
	t.Setenv("TELEMETRY_GPU_CONTROL", "")
	cfg := resolveTelemetryConfigFromEnv()
	if cfg != nil {
		t.Fatalf("expected nil config when no env vars set, got %#v", cfg)
	}
}

func TestExtractFloat(t *testing.T) {
	t.Parallel()
	m := map[string]any{"v1": float64(12.5), "v2": int64(7)}
	if v, ok := extractFloat(m, "v1"); !ok || v != 12.5 {
		t.Fatalf("extractFloat v1 failed: v=%f ok=%v", v, ok)
	}
	if v, ok := extractFloat(m, "v2"); !ok || v != 7 {
		t.Fatalf("extractFloat v2 failed: v=%f ok=%v", v, ok)
	}
	if _, ok := extractFloat(m, "none"); ok {
		t.Fatalf("extractFloat none should fail")
	}
}

func TestHTTPPowerReader(t *testing.T) {
	t.Parallel()
	t.Run("top-level", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"packagePowerWatts": 222.5})
		}))
		defer srv.Close()
		r := &HTTPPowerReader{Endpoint: srv.URL, NodeName: "node-a", Client: srv.Client()}
		p, ok, err := r.ReadPowerWatts()
		if err != nil || !ok || p != 222.5 {
			t.Fatalf("unexpected result p=%v ok=%v err=%v", p, ok, err)
		}
	})

	t.Run("nested", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"cpu": map[string]any{"packagePowerWatts": 111.0}})
		}))
		defer srv.Close()
		r := &HTTPPowerReader{Endpoint: srv.URL, NodeName: "node-a", Client: srv.Client()}
		p, ok, err := r.ReadPowerWatts()
		if err != nil || !ok || p != 111.0 {
			t.Fatalf("unexpected result p=%v ok=%v err=%v", p, ok, err)
		}
	})

	t.Run("status-error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		r := &HTTPPowerReader{Endpoint: srv.URL, NodeName: "node-a", Client: srv.Client()}
		_, _, err := r.ReadPowerWatts()
		if err == nil {
			t.Fatalf("expected error for non-2xx status")
		}
	})
}

func TestControlClientFromTelemetry(t *testing.T) {
	t.Parallel()
	cfg := &TelemetryConfig{
		CPUControlType:      "http",
		ControlHTTPEndpoint: "http://sim/control/{node}",
	}
	client := controlClientFromTelemetry(cfg, "node-a")
	if client == nil {
		t.Fatalf("expected http control client")
	}
	if got := client.Endpoint; got != "http://sim/control/{node}" {
		t.Fatalf("unexpected endpoint %q", got)
	}

	cfg.CPUControlType = "host"
	if got := controlClientFromTelemetry(cfg, "node-a"); got != nil {
		t.Fatalf("expected nil for host control type")
	}
}

func TestOwnsNodeForShardDeterministic(t *testing.T) {
	t.Parallel()
	node := "kwok-node-17"
	a := ownsNodeForShard(node, 7, 3)
	b := ownsNodeForShard(node, 7, 3)
	if a != b {
		t.Fatalf("ownership must be deterministic for node=%s", node)
	}
}

func TestOwnsNodeForShardDistributionSanity(t *testing.T) {
	t.Parallel()
	shards := 5
	counts := make([]int, shards)
	for i := 0; i < 500; i++ {
		node := fmt.Sprintf("kwok-node-%d", i)
		for shard := 0; shard < shards; shard++ {
			if ownsNodeForShard(node, shards, shard) {
				counts[shard]++
				break
			}
		}
	}
	for shard, c := range counts {
		if c < 60 || c > 140 {
			t.Fatalf("unexpected shard skew shard=%d count=%d", shard, c)
		}
	}
}

func TestResolvePoolShardIDFromPodName(t *testing.T) {
	t.Setenv("POOL_SHARD_ID", "")
	t.Setenv("POD_NAME", "joulie-agent-pool-3")
	if got := resolvePoolShardID(); got != 3 {
		t.Fatalf("resolvePoolShardID=%d want=3", got)
	}
}

func TestDVFSSetPowerReaderForTelemetry(t *testing.T) {
	t.Parallel()
	d := &DVFSController{}
	setDVFSPowerReaderForTelemetry(d, nil, "node-a")
	if d.PowerReader != nil {
		t.Fatalf("expected nil powerReader")
	}
	cfg := &TelemetryConfig{CPUSourceType: "http", HTTPEndpoint: "http://sim/telemetry/{node}"}
	setDVFSPowerReaderForTelemetry(d, cfg, "node-a")
	if d.PowerReader == nil {
		t.Fatalf("expected powerReader")
	}
	cfg.CPUSourceType = "host"
	setDVFSPowerReaderForTelemetry(d, cfg, "node-a")
	if d.PowerReader != nil {
		t.Fatalf("expected nil powerReader for host type")
	}
}

func TestHTTPControlClientApplyCPUControl(t *testing.T) {
	t.Parallel()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/control/node-a") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	if err := c.ApplyCPUControl("dvfs.set_throttle_pct", 120, 30); err != nil {
		t.Fatalf("ApplyCPUControl error: %v", err)
	}
	if got, _ := seen["action"].(string); got != "dvfs.set_throttle_pct" {
		t.Fatalf("unexpected action: %v", seen["action"])
	}
	if got, _ := seen["node"].(string); got != "node-a" {
		t.Fatalf("unexpected node: %v", seen["node"])
	}
	if got, _ := seen["throttlePct"].(float64); got != 30 {
		t.Fatalf("unexpected throttlePct: %v", seen["throttlePct"])
	}
}

func TestApplyThrottlePctHTTPControlNoCPUs(t *testing.T) {
	t.Parallel()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &DVFSController{}
	c := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	written, err := d.ApplyThrottlePct(40, c, 120)
	if err != nil {
		t.Fatalf("applyThrottlePct returned error: %v", err)
	}
	if written != 1 {
		t.Fatalf("written=%d want=1", written)
	}
	if got, _ := seen["action"].(string); got != "dvfs.set_throttle_pct" {
		t.Fatalf("unexpected action: %v", seen["action"])
	}
}

func TestApplyThrottlePctFailsWithoutBackends(t *testing.T) {
	t.Parallel()
	d := &DVFSController{}
	_, err := d.ApplyThrottlePct(10, nil, 120)
	if err == nil {
		t.Fatalf("expected error when no cpufreq and no http control")
	}
}

func TestDVFSReconcileUsesHTTPControl(t *testing.T) {
	t.Parallel()
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestDVFSMetrics("node-a", "reconcile_http")

	d := &DVFSController{
		Metrics:     m,
		EmaAlpha:    1.0,
		HighMarginW: 0,
		LowMarginW:  0,
		StepPct:     10,
		TripCount:   1,
	}
	d.PowerReader = powerReaderStub{power: 200, ok: true}
	c := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	action, err := d.Reconcile(120, c)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !strings.Contains(action, "throttle-up") {
		t.Fatalf("expected throttle-up action, got %q", action)
	}
	if got, _ := last["action"].(string); got != "dvfs.set_throttle_pct" {
		t.Fatalf("unexpected control action: %v", last["action"])
	}
}

func TestDVFSReconcileThrottleDown(t *testing.T) {
	t.Parallel()
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestDVFSMetrics("node-down", "down")
	d := &DVFSController{
		Metrics:     m,
		EmaAlpha:    1.0,
		HighMarginW: 0,
		LowMarginW:  0,
		StepPct:     10,
		TripCount:   1,
		ThrottlePct: 20,
		PowerReader: powerReaderStub{power: 80, ok: true},
		Cooldown:    0,
		LastAction:  time.Time{},
		AboveCount:  0,
		BelowCount:  0,
	}
	c := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-down",
		Client:   srv.Client(),
	}
	action, err := d.Reconcile(120, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(action, "throttle-down") {
		t.Fatalf("expected throttle-down action, got %q", action)
	}
	if got, _ := last["action"].(string); got != "dvfs.set_throttle_pct" {
		t.Fatalf("unexpected control action: %v", last["action"])
	}
	if got, _ := last["throttlePct"].(float64); got != 10 {
		t.Fatalf("unexpected throttlePct: %v", last["throttlePct"])
	}
}

func TestDVFSReconcileCooldownHold(t *testing.T) {
	t.Parallel()
	m := newTestDVFSMetrics("node-cooldown", "cooldown")
	d := &DVFSController{
		Metrics:     m,
		EmaAlpha:    1.0,
		HighMarginW: 0,
		LowMarginW:  0,
		StepPct:     10,
		TripCount:   1,
		ThrottlePct: 10,
		PowerReader: powerReaderStub{power: 300, ok: true},
		Cooldown:    10 * time.Minute,
		LastAction:  time.Now(),
	}
	action, err := d.Reconcile(120, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(action, "hold(cooldown)") {
		t.Fatalf("expected cooldown hold, got %q", action)
	}
}

func TestDVFSReadPowerWattsUsesReader(t *testing.T) {
	t.Parallel()
	d := &DVFSController{PowerReader: powerReaderStub{power: 42, ok: true}}
	p, ok, err := d.ReadPowerWatts()
	if err != nil || !ok || p != 42 {
		t.Fatalf("unexpected p=%v ok=%v err=%v", p, ok, err)
	}
}

func TestDVFSReadPowerWattsReaderError(t *testing.T) {
	t.Parallel()
	d := &DVFSController{PowerReader: powerReaderStub{err: errors.New("boom")}}
	_, _, err := d.ReadPowerWatts()
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestApplyThrottlePctHostWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f1 := filepath.Join(dir, "cpu0_scaling_max_freq")
	f2 := filepath.Join(dir, "cpu1_scaling_max_freq")
	if err := os.WriteFile(f1, []byte("3000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("3000000"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &DVFSController{
		Cpus: []DVFSCpu{
			{Index: 0, MaxFile: f1, MinKHz: 1000000, MaxKHz: 3000000},
			{Index: 1, MaxFile: f2, MinKHz: 1000000, MaxKHz: 3000000},
		},
		MinFreqKHz: 1500000,
	}
	written, err := d.ApplyThrottlePct(50, nil, 120)
	if err != nil {
		t.Fatalf("applyThrottlePct error: %v", err)
	}
	if written != 2 {
		t.Fatalf("written=%d want=2", written)
	}
	v1, _ := os.ReadFile(f1)
	v2, _ := os.ReadFile(f2)
	if strings.TrimSpace(string(v1)) != "1500000" {
		t.Fatalf("cpu0 expected 1500000 got %s", strings.TrimSpace(string(v1)))
	}
	if strings.TrimSpace(string(v2)) != "3000000" {
		t.Fatalf("cpu1 expected 3000000 got %s", strings.TrimSpace(string(v2)))
	}
}

func TestApplyCPUPercentIntentHTTP(t *testing.T) {
	t.Parallel()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &DVFSController{
		PowerReader: powerReaderStub{power: 250, ok: true},
	}
	c := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	backend, result, msg, err := applyCPUPercentIntent(100, c, d, nil)
	if err != nil {
		t.Fatalf("applyCPUPercentIntent err: %v", err)
	}
	if backend != "dvfs" || result != "applied" {
		t.Fatalf("unexpected backend/result: %s/%s", backend, result)
	}
	if !strings.Contains(msg, "throttle=0%") {
		t.Fatalf("unexpected msg: %q", msg)
	}
	if got, _ := seen["action"].(string); got != "dvfs.set_throttle_pct" {
		t.Fatalf("unexpected action: %v", seen["action"])
	}
	if got, _ := seen["throttlePct"].(float64); got != 0 {
		t.Fatalf("unexpected throttlePct: %v", seen["throttlePct"])
	}
}

func TestApplyCPUPercentIntentBlockedWithoutBackends(t *testing.T) {
	t.Parallel()
	backend, result, _, err := applyCPUPercentIntent(60, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if backend != "none" || result != "blocked" {
		t.Fatalf("unexpected backend/result: %s/%s", backend, result)
	}
}

func TestResolveDesiredStateFromNodeTwin(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme,
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata": map[string]any{
				"name": "node-a",
			},
			"spec": map[string]any{
				"nodeName": "node-a",
				"profile":  "eco",
				"cpu": map[string]any{
					"packagePowerCapWatts": 120.0,
				},
			},
		}},
	)

	state, src, err := resolveDesiredStateForNode(context.Background(), dyn, "node-a")
	if err != nil {
		t.Fatalf("resolveDesiredStateForNode error: %v", err)
	}
	if state == nil || src != "nodetwin" || state.PowerWatts == nil || *state.PowerWatts != 120.0 {
		t.Fatalf("unexpected desired state: %#v src=%s", state, src)
	}
}

func TestUpdateNodeTwinControlStatus(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme,
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata": map[string]any{
				"name": "node-a",
			},
			"spec": map[string]any{
				"nodeName": "node-a",
				"profile":  "eco",
			},
		}},
	)
	if err := updateNodeTwinControlStatus(context.Background(), dyn, "node-a", "cpu", "dvfs", "applied", "ok"); err != nil {
		t.Fatalf("updateNodeTwinControlStatus error: %v", err)
	}
	obj, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin: %v", err)
	}
	backend, _, _ := unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "backend")
	result, _, _ := unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "result")
	if backend != "dvfs" || result != "applied" {
		t.Fatalf("unexpected status backend=%s result=%s", backend, result)
	}
}

type powerReaderStub struct {
	power float64
	ok    bool
	err   error
}

func (p powerReaderStub) ReadPowerWatts() (float64, bool, error) {
	return p.power, p.ok, p.err
}

func newTestDVFSMetrics(node, prefix string) *dvfs.Metrics {
	return &dvfs.Metrics{
		Node: node,
		ObservedPowerW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_observed_power_watts", Help: "test",
		}, []string{"node"}),
		EMAPowerW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_ema_power_watts", Help: "test",
		}, []string{"node"}),
		ThrottlePct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_throttle_pct", Help: "test",
		}, []string{"node"}),
		TripAbove: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_above_trip_count", Help: "test",
		}, []string{"node"}),
		TripBelow: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_below_trip_count", Help: "test",
		}, []string{"node"}),
		CPUCurFreqKHz: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_cpu_cur_freq_khz", Help: "test",
		}, []string{"node", "cpu"}),
		CPUMaxFreqKHz: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_dvfs_cpu_max_freq_khz", Help: "test",
		}, []string{"node", "cpu"}),
		ActionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_" + prefix + "_dvfs_actions_total", Help: "test",
		}, []string{"node", "action"}),
	}
}

func newTestAgentMetrics(prefix string) *AgentMetrics {
	return &AgentMetrics{
		node: prefix,
		backendMode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_backend_mode", Help: "test",
		}, []string{"node", "mode"}),
		policyCapWatts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_" + prefix + "_policy_cap_watts", Help: "test",
		}, []string{"node", "policy"}),
	}
}

func TestReconcileOnceNoProfileWritesNoneStatus(t *testing.T) {
	t.Parallel()
	nodeName := "node-a"
	kube := k8sfake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{"feature.node.kubernetes.io/cpu-model.vendor_id": "GenuineIntel"},
			},
		},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeTwinGVR:     "NodeTwinList",
		nodeHardwareGVR: "NodeHardwareList",
	})
	metrics := newTestAgentMetrics("reconcile-no-profile")
	nc := &NodeController{
		nodeName:               nodeName,
		metrics:                metrics,
		simulateOnly:           false,
		lastRaplKey:            "prev",
		lastSuccessfulSpecRead: time.Now(),
		specReadTimeout:        5 * time.Minute,
	}
	if err := reconcileOnce(context.Background(), kube, dyn, nc); err != nil {
		t.Fatalf("reconcileOnce error: %v", err)
	}
	if nc.lastRaplKey != "" {
		t.Fatalf("expected lastRaplKey reset, got %q", nc.lastRaplKey)
	}
	hwObj, err := dyn.Resource(nodeHardwareGVR).Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get nodehardware: %v", err)
	}
	if got, _, _ := unstructured.NestedString(hwObj.Object, "spec", "nodeName"); got != nodeName {
		t.Fatalf("unexpected nodehardware nodeName=%q", got)
	}
}

func TestReconcileOnceSimulateOnlyWritesAppliedStatus(t *testing.T) {
	t.Parallel()
	nodeName := "node-a"
	kube := k8sfake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{"feature.node.kubernetes.io/cpu-model.vendor_id": "GenuineIntel"},
			},
		},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeTwinGVR:     "NodeTwinList",
		nodeHardwareGVR: "NodeHardwareList",
	},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodeTwin",
			"metadata":   map[string]any{"name": "node-a"},
			"spec": map[string]any{
				"nodeName": nodeName,
				"profile":  "eco",
				"cpu": map[string]any{
					"packagePowerCapWatts": 120.0,
				},
			},
		}},
	)
	metrics := newTestAgentMetrics("reconcile-sim-only")
	nc := &NodeController{
		nodeName:               nodeName,
		metrics:                metrics,
		simulateOnly:           true,
		lastSuccessfulSpecRead: time.Now(),
		specReadTimeout:        5 * time.Minute,
	}
	if err := reconcileOnce(context.Background(), kube, dyn, nc); err != nil {
		t.Fatalf("reconcileOnce error: %v", err)
	}
	obj, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin: %v", err)
	}
	backend, _, _ := unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "backend")
	result, _, _ := unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "result")
	msg, _, _ := unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "message")
	if backend != "none" || result != "applied" || !strings.Contains(msg, "simulate-only") {
		t.Fatalf("unexpected status backend=%q result=%q msg=%q", backend, result, msg)
	}
}

func TestReconcileOnceRelaxesCapsWhenSpecReadTimesOut(t *testing.T) {
	t.Parallel()
	nodeName := "node-a"
	kube := k8sfake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{"feature.node.kubernetes.io/cpu-model.vendor_id": "GenuineIntel"},
			},
		},
	)
	// No NodeTwin objects - simulates operator being gone.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeTwinGVR:     "NodeTwinList",
		nodeHardwareGVR: "NodeHardwareList",
	})
	metrics := newTestAgentMetrics("reconcile-timeout")
	nc := &NodeController{
		nodeName:               nodeName,
		metrics:                metrics,
		simulateOnly:           false,
		lastSuccessfulSpecRead: time.Now().Add(-10 * time.Minute), // 10 min ago
		specReadTimeout:        5 * time.Minute,
	}

	// With no NodeTwin objects, resolveDesiredStateForNode returns nil/nil (no error, no profile).
	// This counts as a successful API read (API was reachable, just no profile).
	err := reconcileOnce(context.Background(), kube, dyn, nc)
	if err != nil {
		t.Fatalf("reconcileOnce error: %v", err)
	}
	// lastSuccessfulSpecRead should be updated since API call succeeded
	if time.Since(nc.lastSuccessfulSpecRead) > 2*time.Second {
		t.Fatalf("lastSuccessfulSpecRead should have been refreshed")
	}
}

func TestNodeControllerCapsRelaxedFlag(t *testing.T) {
	t.Parallel()
	nc := &NodeController{
		nodeName:               "test-node",
		specReadTimeout:        5 * time.Minute,
		lastSuccessfulSpecRead: time.Now(),
	}
	if nc.capsRelaxed {
		t.Fatal("capsRelaxed should be false initially")
	}
}

func TestDetectGPUVendor(t *testing.T) {
	old := commandRunner
	defer func() { commandRunner = old }()

	commandRunner = fakeCommandRunner{
		responses: map[string]fakeCommandResult{
			"nvidia-smi -L": {out: "GPU 0: L40S"},
		},
	}
	if got := detectGPUVendor(context.Background(), map[string]string{
		"feature.node.kubernetes.io/pci-10de.present": "true",
	}); got != "nvidia" {
		t.Fatalf("vendor=%q want=nvidia", got)
	}

	commandRunner = fakeCommandRunner{
		responses: map[string]fakeCommandResult{
			"nvidia-smi -L":              {err: errors.New("not found")},
			"rocm-smi --showproductname": {out: "card0"},
		},
	}
	if got := detectGPUVendor(context.Background(), map[string]string{
		"feature.node.kubernetes.io/pci-1002.present": "true",
	}); got != "amd" {
		t.Fatalf("vendor=%q want=amd", got)
	}
}

func TestResolveGPUCapPerDevice(t *testing.T) {
	t.Parallel()
	devs := []GPUDevice{
		{Index: 0, MinCapWatts: 200, MaxCapWatts: 350},
		{Index: 1, MinCapWatts: 200, MaxCapWatts: 350},
	}
	pct := 60.0
	got, _, ok := resolveGPUCapPerDevice(&GPUPowerCap{CapPctOfMax: &pct}, devs)
	if !ok || got != 210 {
		t.Fatalf("resolved cap got=%v ok=%v want=210/true", got, ok)
	}
}

func TestApplyGPUIntentHTTP(t *testing.T) {
	old := commandRunner
	defer func() { commandRunner = old }()
	commandRunner = fakeCommandRunner{
		responses: map[string]fakeCommandResult{
			"nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits": {
				out: "0, 200, 350, 300, 250, NVIDIA L40S\n",
			},
			"nvidia-smi -L": {out: "GPU 0: NVIDIA L40S"},
		},
	}

	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	w := 220.0
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
			},
		},
	}
	backend, result, _, _, err := applyGPUIntent(context.Background(), node, &GPUPowerCap{CapWattsPerGPU: &w}, client)
	if err != nil {
		t.Fatalf("applyGPUIntent err: %v", err)
	}
	if backend != "http" || result != "applied" {
		t.Fatalf("unexpected backend/result %s/%s", backend, result)
	}
	if seen["action"] != "gpu.set_power_cap_watts" {
		t.Fatalf("unexpected action payload: %#v", seen)
	}
}

func TestApplyGPUIntentBlockedOnNonGPUNode(t *testing.T) {
	t.Parallel()
	w := 220.0
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	}
	backend, result, msg, observed, err := applyGPUIntent(context.Background(), node, &GPUPowerCap{CapWattsPerGPU: &w}, nil)
	if err != nil {
		t.Fatalf("applyGPUIntent err: %v", err)
	}
	if backend != "none" || result != "blocked" {
		t.Fatalf("unexpected backend/result %s/%s", backend, result)
	}
	if msg != "node has no allocatable GPU resources" {
		t.Fatalf("unexpected msg: %q", msg)
	}
	if observed == nil || observed["allocatableGPUs"] != 0 {
		t.Fatalf("unexpected observed payload: %#v", observed)
	}
}

func TestApplyGPUIntentHTTPAbsoluteBypassesInventory(t *testing.T) {
	old := commandRunner
	defer func() { commandRunner = old }()
	// No command responses configured: inventory would fail if invoked.
	commandRunner = fakeCommandRunner{responses: map[string]fakeCommandResult{}}

	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &HTTPControlClient{
		Endpoint: srv.URL + "/control/{node}",
		NodeName: "node-a",
		Client:   srv.Client(),
	}
	w := 250.0
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
			},
		},
	}

	backend, result, msg, observed, err := applyGPUIntent(context.Background(), node, &GPUPowerCap{CapWattsPerGPU: &w}, client)
	if err != nil {
		t.Fatalf("applyGPUIntent err: %v", err)
	}
	if backend != "http" || result != "applied" {
		t.Fatalf("unexpected backend/result %s/%s", backend, result)
	}
	if !strings.Contains(msg, "applied gpu cap") {
		t.Fatalf("unexpected msg: %q", msg)
	}
	if observed == nil || observed["capWattsPerGpu"] != 250.0 {
		t.Fatalf("unexpected observed payload: %#v", observed)
	}
	if seen["action"] != "gpu.set_power_cap_watts" {
		t.Fatalf("unexpected action payload: %#v", seen)
	}
}

func TestResolveGPUCapPerDeviceFailsWhenPctNeedsUnknownMax(t *testing.T) {
	t.Parallel()
	pct := 70.0
	_, msg, ok := resolveGPUCapPerDevice(&GPUPowerCap{CapPctOfMax: &pct}, []GPUDevice{
		{Index: 0, MinCapWatts: 150, MaxCapWatts: 0},
	})
	if ok {
		t.Fatalf("expected failure when max cap is unavailable")
	}
	if !strings.Contains(msg, "cannot resolve capPctOfMax") {
		t.Fatalf("unexpected msg: %q", msg)
	}
}

type fakeCommandResult struct {
	out string
	err error
}

type fakeCommandRunner struct {
	responses map[string]fakeCommandResult
}

func (f fakeCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if len(args) > 0 {
		key += " " + strings.Join(args, " ")
	}
	if r, ok := f.responses[key]; ok {
		return []byte(r.out), r.err
	}
	return nil, fmt.Errorf("unexpected command: %s", key)
}
