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

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// nodeTwinFromMap decodes an unstructured NodeTwin the way the typed cache
// does, so the test still exercises the int and float shapes the API server
// hands out.
func nodeTwinFromMap(t *testing.T, obj map[string]any) *v1alpha1.NodeTwin {
	t.Helper()
	nt := &v1alpha1.NodeTwin{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, nt); err != nil {
		t.Fatalf("decode NodeTwin: %v", err)
	}
	return nt
}

func TestParseNodeTwinAsProfileWithIntAndFloatCaps(t *testing.T) {
	t.Parallel()
	intObj := map[string]any{
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
	}
	npInt := nodeTwinSpecFromObject(nodeTwinFromMap(t, intObj))
	if npInt.PowerWatts == nil || *npInt.PowerWatts != 120 {
		t.Fatalf("int cap parse failed: %#v", npInt.PowerWatts)
	}
	if npInt.PowerPctOfMax != nil || npInt.GPU != nil {
		t.Fatalf("absent caps must stay nil: pct=%v gpu=%v", npInt.PowerPctOfMax, npInt.GPU)
	}

	floatObj := map[string]any{
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
			"gpu": map[string]any{
				"powerCap": map[string]any{
					"capPctOfMax": int64(80),
				},
			},
		},
	}
	npFloat := nodeTwinSpecFromObject(nodeTwinFromMap(t, floatObj))
	if npFloat.PowerWatts == nil || *npFloat.PowerWatts != 5000 {
		t.Fatalf("float cap parse failed: %#v", npFloat.PowerWatts)
	}
	if npFloat.GPU == nil || npFloat.GPU.Scope != "perGpu" || npFloat.GPU.CapPctOfMax == nil || *npFloat.GPU.CapPctOfMax != 80 || npFloat.GPU.CapWattsPerGPU != nil {
		t.Fatalf("gpu cap parse failed: %#v", npFloat.GPU)
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
	reader, _ := newTestClients(t, nodeTwinWithCPUCap("node-a", 120))

	state, src, err := resolveDesiredStateForNode(context.Background(), reader, "node-a")
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

// intelNode is the Node every reconcile test reads: the NFD vendor label
// makes discoverHardware pick a RAPL-capable vendor without touching sysfs.
func intelNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"feature.node.kubernetes.io/cpu-model.vendor_id": "GenuineIntel"},
		},
	}
}

func nodeTwinWithCPUCap(nodeName string, watts float64) *v1alpha1.NodeTwin {
	return &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: sanitizeNodeObjectName(nodeName)},
		Spec: v1alpha1.NodeTwinSpec{
			NodeName: nodeName,
			Profile:  "eco",
			CPU:      &v1alpha1.NodeTwinCPU{PackagePowerCapWatts: &watts},
		},
	}
}

// newTestClients seeds the reader reconcile reads through and the dynamic
// client it writes through from one list of objects, so the two views
// cannot drift. The dynamic client converts the typed objects itself.
func newTestClients(t *testing.T, objs ...client.Object) (kube.Reader, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	reader := crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	dyn := dynamicfake.NewSimpleDynamicClient(s, runtimeObjs...)
	return reader, dyn
}

func TestReconcileOnceNoProfileWritesNoneStatus(t *testing.T) {
	t.Parallel()
	nodeName := "node-a"
	reader, dyn := newTestClients(t, intelNode(nodeName))
	metrics := newTestAgentMetrics("reconcile-no-profile")
	nc := &NodeController{
		nodeName:               nodeName,
		metrics:                metrics,
		simulateOnly:           false,
		lastRaplKey:            "prev",
		lastSuccessfulSpecRead: time.Now(),
		specReadTimeout:        5 * time.Minute,
	}
	if err := reconcileOnce(context.Background(), reader, dyn, nc); err != nil {
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
	reader, dyn := newTestClients(t, intelNode(nodeName), nodeTwinWithCPUCap(nodeName, 120))
	metrics := newTestAgentMetrics("reconcile-sim-only")
	nc := &NodeController{
		nodeName:               nodeName,
		metrics:                metrics,
		simulateOnly:           true,
		lastSuccessfulSpecRead: time.Now(),
		specReadTimeout:        5 * time.Minute,
	}
	if err := reconcileOnce(context.Background(), reader, dyn, nc); err != nil {
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
	// No NodeTwin objects - simulates the controller manager being gone.
	reader, dyn := newTestClients(t, intelNode(nodeName))
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
	err := reconcileOnce(context.Background(), reader, dyn, nc)
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

// raplFixture builds a powercap tree with one named zone per entry and the
// given constraint values in microwatts. A value of -1 omits the file.
func raplFixture(t *testing.T, zones []struct {
	Dir, Name    string
	MaxUW, MinUW int64
}) string {
	t.Helper()
	root := t.TempDir()
	for _, z := range zones {
		dir := filepath.Join(root, z.Dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte(z.Name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "constraint_0_power_limit_uw"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		if z.MaxUW >= 0 {
			if err := os.WriteFile(filepath.Join(dir, "constraint_0_max_power_uw"), []byte(fmt.Sprintf("%d", z.MaxUW)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if z.MinUW >= 0 {
			if err := os.WriteFile(filepath.Join(dir, "constraint_0_min_power_uw"), []byte(fmt.Sprintf("%d", z.MinUW)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := dvfs.PowercapRoot
	dvfs.PowercapRoot = root
	t.Cleanup(func() { dvfs.PowercapRoot = old })
	return root
}

// n2-atos: 4 package zones at 165 W each, DRAM sub-zones at 47.25 W.
func fourSocketFixture(t *testing.T) string {
	t.Helper()
	return raplFixture(t, []struct {
		Dir, Name    string
		MaxUW, MinUW int64
	}{
		{"intel-rapl:0", "package-0", 165_000_000, 90_000_000},
		{"intel-rapl:0:0", "dram", 47_250_000, -1},
		{"intel-rapl:1", "package-1", 165_000_000, 90_000_000},
		{"intel-rapl:1:0", "dram", 47_250_000, -1},
		{"intel-rapl:2", "package-2", 165_000_000, 90_000_000},
		{"intel-rapl:2:0", "dram", 47_250_000, -1},
		{"intel-rapl:3", "package-3", 165_000_000, 90_000_000},
		{"intel-rapl:3:0", "dram", 47_250_000, -1},
	})
}

func TestReadRAPLPackageCapRangeIgnoresDRAMSubZones(t *testing.T) {
	fourSocketFixture(t)

	maxW, minW, ok := readRAPLPackageCapRangeWatts()
	if !ok {
		t.Fatal("expected a cap range")
	}
	if maxW != 165 {
		t.Fatalf("maxW=%v want=165 (DRAM sub-zone 47.25 W must be ignored)", maxW)
	}
	if minW != 90 {
		t.Fatalf("minW=%v want=90", minW)
	}
}

func TestDiscoverCPUSocketsFallsBackToPackageZoneCount(t *testing.T) {
	fourSocketFixture(t)
	withoutProcCPUInfo(t) // isolate from the host: exercise the RAPL fallback

	// n2-atos has NFD installed but exposes no cpu-sockets label.
	got := discoverCPUSockets(map[string]string{
		"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
	})
	if got != 4 {
		t.Fatalf("sockets=%d want=4 (one per package-* zone)", got)
	}
}

func TestDiscoverCPUSocketsPrefersLabel(t *testing.T) {
	fourSocketFixture(t)

	got := discoverCPUSockets(map[string]string{"feature.node.kubernetes.io/cpu-sockets": "2"})
	if got != 2 {
		t.Fatalf("sockets=%d want=2 (label wins over zone count)", got)
	}
}

func TestApplyRAPLPackageCapWritesEverySocketDespiteOneFailure(t *testing.T) {
	root := fourSocketFixture(t)
	// Make socket 2's limit file unwritable by replacing it with a directory,
	// the way a disabled zone rejects writes with ENODATA on real hardware.
	bad := filepath.Join(root, "intel-rapl:2", "constraint_0_power_limit_uw")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}

	applied, count, err := applyRAPLPackageCap(HardwareInfo{CPUVendor: "GenuineIntel"}, 120)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !applied || count != 3 {
		t.Fatalf("applied=%v count=%d want applied=true count=3", applied, count)
	}
	for _, zone := range []string{"intel-rapl:0", "intel-rapl:1", "intel-rapl:3"} {
		b, err := os.ReadFile(filepath.Join(root, zone, "constraint_0_power_limit_uw"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(b)) != "120000000" {
			t.Fatalf("%s limit=%s want=120000000", zone, strings.TrimSpace(string(b)))
		}
	}
}

// procCPUInfo4Socket is a trimmed /proc/cpuinfo from a 4-socket Xeon: two
// logical processors per socket is enough to exercise the parser.
const procCPUInfo4Socket = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz
physical id	: 0
siblings	: 48
core id		: 0
cpu cores	: 24

processor	: 1
model name	: Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz
physical id	: 0
core id		: 1

processor	: 2
model name	: Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz
physical id	: 1
core id		: 0

processor	: 3
model name	: Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz
physical id	: 2
core id		: 0

processor	: 4
model name	: Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz
physical id	: 3
core id		: 0
`

func withProcCPUInfo(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procCPUInfoPath
	procCPUInfoPath = path
	t.Cleanup(func() { procCPUInfoPath = old })
}

// withoutProcCPUInfo points the parser at a file that does not exist, so a
// test exercises the fallbacks instead of the CPU of the machine running it.
func withoutProcCPUInfo(t *testing.T) {
	t.Helper()
	old := procCPUInfoPath
	procCPUInfoPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { procCPUInfoPath = old })
}

func TestProcCPUInfoModelAndSockets(t *testing.T) {
	withProcCPUInfo(t, procCPUInfo4Socket)

	model, sockets := readProcCPUInfo()
	if model != "Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz" {
		t.Fatalf("model=%q want the /proc/cpuinfo model name", model)
	}
	if sockets != 4 {
		t.Fatalf("sockets=%d want=4 (distinct physical id values)", sockets)
	}
}

func TestProcCPUInfoMissingFileIsHarmless(t *testing.T) {
	withoutProcCPUInfo(t)

	model, sockets := readProcCPUInfo()
	if model != "" || sockets != 0 {
		t.Fatalf("model=%q sockets=%d want empty/0", model, sockets)
	}
}

func TestDiscoverCPUSocketsUsesCPUInfoWithoutRAPL(t *testing.T) {
	withProcCPUInfo(t, procCPUInfo4Socket)
	// No powercap zones at all: the socket count must not depend on RAPL.
	old := dvfs.PowercapRoot
	dvfs.PowercapRoot = filepath.Join(t.TempDir(), "powercap")
	t.Cleanup(func() { dvfs.PowercapRoot = old })

	if got := discoverCPUSockets(nil); got != 4 {
		t.Fatalf("sockets=%d want=4", got)
	}
}

func TestDiscoverCPURawModelFallsBackToCPUInfo(t *testing.T) {
	withProcCPUInfo(t, procCPUInfo4Socket)

	// n2-atos runs NFD but NFD publishes no cpu-model.name label.
	got := discoverCPURawModel(map[string]string{
		"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
	})
	if got != "Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz" {
		t.Fatalf("rawModel=%q want the /proc/cpuinfo model name", got)
	}

	labelled := discoverCPURawModel(map[string]string{
		"feature.node.kubernetes.io/cpu-model.name": "AMD EPYC 9654 96-Core Processor",
	})
	if labelled != "AMD EPYC 9654 96-Core Processor" {
		t.Fatalf("rawModel=%q want the label to win", labelled)
	}
}

func nodeTwinObject(name, nodeName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"nodeName": nodeName, "profile": "eco"},
	}}
}

// countingReader records which verbs a reconcile path uses on the reader.
type countingReader struct {
	kube.Reader
	gets, lists int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Reader.List(ctx, list, opts...)
}

func TestGetNodeTwinSpecGetsByNameWithoutListing(t *testing.T) {
	t.Parallel()
	inner, _ := newTestClients(t,
		nodeTwinWithCPUCap("node-get", 100),
		&v1alpha1.NodeTwin{
			ObjectMeta: metav1.ObjectMeta{Name: "node-other"},
			Spec:       v1alpha1.NodeTwinSpec{NodeName: "someone-else", Profile: "eco"},
		},
	)
	reader := &countingReader{Reader: inner}

	np, err := getNodeTwinSpec(context.Background(), reader, "node-get")
	if err != nil || np == nil || np.Profile != "eco" {
		t.Fatalf("np=%+v err=%v want eco profile", np, err)
	}
	if reader.lists != 0 || reader.gets != 1 {
		t.Fatalf("gets=%d lists=%d; agent must Get its own NodeTwin by name, never list", reader.gets, reader.lists)
	}
	np, err = getNodeTwinSpec(context.Background(), reader, "node-absent")
	if err != nil || np != nil {
		t.Fatalf("absent twin: np=%+v err=%v want nil/nil", np, err)
	}
	// A twin whose spec.nodeName disagrees with its object name is an error,
	// not a profile to apply.
	if np, err = getNodeTwinSpec(context.Background(), reader, "node-other"); err == nil || np != nil {
		t.Fatalf("mismatched twin: np=%+v err=%v want error", np, err)
	}
}

func TestDaemonsetCacheSelectsOnlyThisNodeByName(t *testing.T) {
	t.Parallel()
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	opts := cacheOptionsForNode(s, "n2.atos")
	if opts.Scheme != s {
		t.Fatal("cache options must carry the scheme")
	}
	got := map[string]string{}
	for obj, by := range opts.ByObject {
		if by.Label != nil {
			t.Fatalf("%T: daemonset cache must not filter by label, got %q", obj, by.Label.String())
		}
		if by.Field == nil {
			t.Fatalf("%T: daemonset cache must select by name, got no field selector", obj)
		}
		got[fmt.Sprintf("%T", obj)] = by.Field.String()
	}
	want := map[string]string{
		"*v1.Node":           "metadata.name=n2.atos",
		"*v1alpha1.NodeTwin": "metadata.name=n2-atos",
	}
	if len(got) != len(want) {
		t.Fatalf("ByObject=%v want exactly %v", got, want)
	}
	for kind, sel := range want {
		if got[kind] != sel {
			t.Fatalf("%s selector=%q want=%q (whole ByObject=%v)", kind, got[kind], sel, got)
		}
	}
}

func TestPoolCacheSelectsNodesByLabel(t *testing.T) {
	t.Parallel()
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
	}
	opts := cacheOptionsForPool(s, selector)
	if opts.Scheme != s {
		t.Fatal("cache options must carry the scheme")
	}
	if len(opts.ByObject) != 1 {
		t.Fatalf("ByObject has %d entries, want only Node (NodeTwin stays unrestricted)", len(opts.ByObject))
	}
	for obj, by := range opts.ByObject {
		if _, ok := obj.(*corev1.Node); !ok {
			t.Fatalf("restricted kind is %T, want *v1.Node", obj)
		}
		if by.Field != nil {
			t.Fatalf("pool cache must not select nodes by name, got %q", by.Field.String())
		}
		if by.Label == nil || by.Label.String() != "joulie.io/managed=true" {
			t.Fatalf("node label selector=%v want joulie.io/managed=true", by.Label)
		}
	}
}

func TestControlStatusWriteSkippedWhenUnchanged(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), nodeTwinObject("node-dedupe", "node-dedupe"))
	write := func(result string) {
		if err := updateNodeTwinControlStatus(context.Background(), dyn, "node-dedupe", "cpu", "rapl", result, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	patches := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "patch" {
				n++
			}
		}
		return n
	}

	write("applied")
	write("applied")
	if got := patches(); got != 1 {
		t.Fatalf("patches=%d want=1 (identical payload must not be rewritten)", got)
	}
	write("blocked")
	if got := patches(); got != 2 {
		t.Fatalf("patches=%d want=2 (changed payload must be written)", got)
	}
}

func TestControlStatusPatchTouchesOnlyItsOwnSubtree(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), nodeTwinObject("node-shape", "node-shape"))
	var captured []byte
	dyn.PrependReactor("patch", "nodetwins", func(action clienttesting.Action) (bool, runtime.Object, error) {
		captured = action.(clienttesting.PatchAction).GetPatch()
		return false, nil, nil
	})
	if err := updateNodeTwinControlStatus(context.Background(), dyn, "node-shape", "gpu", "nvml", "applied", "ok"); err != nil {
		t.Fatal(err)
	}

	var patch map[string]any
	if err := json.Unmarshal(captured, &patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	// Contract: the agent owns status.controlStatus.<component> and nothing else.
	if len(patch) != 1 || patch["status"] == nil {
		t.Fatalf("patch touches keys other than status: %v", patch)
	}
	status := patch["status"].(map[string]any)
	if len(status) != 1 || status["controlStatus"] == nil {
		t.Fatalf("patch touches status fields other than controlStatus: %v", status)
	}
	cs := status["controlStatus"].(map[string]any)
	if len(cs) != 1 || cs["gpu"] == nil {
		t.Fatalf("patch touches components other than gpu: %v", cs)
	}
}

func TestNodeHardwareStatusWriteSkippedWhenUnchanged(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeHardwareGVR: "NodeHardwareList",
	})
	hw := HardwareInfo{CPUVendor: "GenuineIntel", CPUSockets: 4, CPUTotalCores: 192, CPUCapKnown: true, CPUCapMaxWatts: 165}
	statusWrites := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "update" && a.GetSubresource() == "status" {
				n++
			}
		}
		return n
	}

	for i := 0; i < 3; i++ {
		if err := upsertNodeHardwareStatus(context.Background(), dyn, "node-hw-dedupe", hw); err != nil {
			t.Fatal(err)
		}
	}
	if got := statusWrites(); got != 1 {
		t.Fatalf("status writes=%d want=1 (unchanged hardware must not be rewritten each tick)", got)
	}
	hw.CPUCapMaxWatts = 200
	if err := upsertNodeHardwareStatus(context.Background(), dyn, "node-hw-dedupe", hw); err != nil {
		t.Fatal(err)
	}
	if got := statusWrites(); got != 2 {
		t.Fatalf("status writes=%d want=2 (changed hardware must be written)", got)
	}
}

func TestObjectNameSharedWithOperator(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"n2.atos": "n2-atos", "Node_A/1": "node-a-1", "gpu:01": "gpu-01"} {
		if got := sanitizeNodeObjectName(in); got != want {
			t.Fatalf("sanitizeNodeObjectName(%q)=%q want=%q", in, got, want)
		}
	}
}
