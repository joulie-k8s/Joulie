package main

// A percent CPU cap on a node without RAPL files, controlled over HTTP (the
// pool agent driving the simulator's KWOK nodes), resolves against the CPU
// package cap range of the hardware catalog, the range the simulator models
// the node with, and goes out as rapl.set_power_cap_watts (issue #36).

import (
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

	"github.com/matbun/joulie/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// controlRecorder is an HTTP control backend that keeps every CPU control
// payload it receives, and answers 400 to the actions in refuse.
type controlRecorder struct {
	mu     sync.Mutex
	got    []map[string]any
	refuse map[string]bool
}

func (c *controlRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, body)
	if action, _ := body["action"].(string); c.refuse[action] {
		http.Error(w, "refused", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// setRefused makes the backend answer 400 to action, or accept it again.
func (c *controlRecorder) setRefused(action string, refused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse == nil {
		c.refuse = map[string]bool{}
	}
	c.refuse[action] = refused
}

// take returns the payloads received since the last take, each as its
// action followed by its capWatts or throttlePct ("rapl.set_power_cap_watts
// 640", "dvfs.set_throttle_pct 40").
func (c *controlRecorder) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.got))
	for _, p := range c.got {
		action, _ := p["action"].(string)
		v := p["throttlePct"]
		if action == "rapl.set_power_cap_watts" {
			v = p["capWatts"]
		}
		out = append(out, fmt.Sprintf("%s %v", action, v))
	}
	c.got = nil
	return out
}

// hostCPUInfo is the /proc/cpuinfo of a pool agent's own host, four sockets
// of a CPU unlike any simulated node's.
const hostCPUInfo = `processor	: 0
model name	: Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz
physical id	: 0

processor	: 1
model name	: Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz
physical id	: 1

processor	: 2
model name	: Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz
physical id	: 2

processor	: 3
model name	: Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz
physical id	: 3
`

// useHTTPCPUControl points the agent's CPU control at a recording backend
// with mode (empty, rapl or dvfs), and hides the host's powercap tree, as a
// pool agent sees no RAPL files of the nodes it drives; its /proc/cpuinfo is
// hostCPUInfo. Not parallel: it sets environment variables and process
// globals.
func useHTTPCPUControl(t *testing.T, mode string) *controlRecorder {
	t.Helper()
	pointAgentAtSysfs(t, t.TempDir(), hostCPUInfo, nil)
	rec := &controlRecorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	t.Setenv("TELEMETRY_CPU_SOURCE", "")
	t.Setenv("TELEMETRY_GPU_CONTROL", "")
	t.Setenv("TELEMETRY_CPU_CONTROL", "http")
	t.Setenv("TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT", srv.URL+"/control/{node}")
	t.Setenv("TELEMETRY_CPU_CONTROL_MODE", mode)
	return rec
}

// kwokNode is a fake node labelled as the KWOK nodes of experiments 01 to
// 03 are (experiments/kwok-cluster/manifests/00-kwok-nodes.yaml); an empty
// sockets leaves the socket label out.
func kwokNode(name, cpuModel, sockets string) *corev1.Node {
	labels := map[string]string{"joulie.io/hw.cpu-model": cpuModel}
	if sockets != "" {
		labels["joulie.io/hw.cpu-sockets"] = sockets
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func nodeTwinWithCPUPct(nodeName string, pct float64) *v1alpha1.NodeTwin {
	return &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: sanitizeNodeObjectName(nodeName)},
		Spec: v1alpha1.NodeTwinSpec{
			NodeName: nodeName,
			Profile:  "eco",
			CPU:      &v1alpha1.NodeTwinCPU{PackagePowerCapPctOfMax: &pct},
		},
	}
}

// pctCapAgent is an agent for node under a percent CPU cap: each call
// runs one reconcile with the same controller and returns the CPU control
// status the agent wrote.
func pctCapAgent(t *testing.T, node *corev1.Node, pct float64) func() (backend, result, message string) {
	t.Helper()
	reader, dyn := newTestClients(t, node, nodeTwinWithCPUPct(node.Name, pct))
	nc := &NodeController{
		nodeName:               node.Name,
		metrics:                newTestAgentMetrics(strings.ReplaceAll(node.Name, "-", "_")),
		lastSuccessfulSpecRead: time.Now(),
		specReadTimeout:        5 * time.Minute,
	}
	return func() (backend, result, message string) {
		t.Helper()
		if err := reconcileOnce(context.Background(), reader, dyn, nc); err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		obj, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), sanitizeNodeObjectName(node.Name), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get NodeTwin: %v", err)
		}
		backend, _, _ = unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "backend")
		result, _, _ = unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "result")
		message, _, _ = unstructured.NestedString(obj.Object, "status", "controlStatus", "cpu", "message")
		return backend, result, message
	}
}

// reconcilePctCap runs one reconcile of node under a percent CPU cap and
// returns the CPU control status the agent wrote.
func reconcilePctCap(t *testing.T, node *corev1.Node, pct float64) (backend, result, message string) {
	t.Helper()
	return pctCapAgent(t, node, pct)()
}

// A node without RAPL files whose CPU is in the catalog gets a watt cap: pct
// of its sockets' TDP, clamped up to the low end of the cTDP range (or 0.55
// x TDP without one), sent as rapl.set_power_cap_watts for the whole node.
// The same percent gives different watts on different CPU families. The old
// agent resolved only from RAPL files and sent dvfs.set_throttle_pct.
func TestPoolNodeWithoutRAPLFilesGetsACatalogWattCap(t *testing.T) {
	for _, tc := range []struct {
		name, cpu, sockets, catalogKey string
		pct, wantW                     float64
	}{
		// 0.95 x 360 W = 342 W per socket, inside 320 to 360 W.
		{"epyc9654-95", "AMD-EPYC-9654-96-Core-Processor", "2", "AMD_EPYC_9654", 95, 684},
		// 0.60 x 360 W = 216 W per socket, clamped to the cTDP low end 320 W.
		{"epyc9654-60", "AMD-EPYC-9654-96-Core-Processor", "2", "AMD_EPYC_9654", 60, 640},
		// 0.95 x 500 W = 475 W per socket, inside 450 to 500 W.
		{"epyc9965-95", "AMD-EPYC-9965-192-Core-Processor", "2", "AMD_EPYC_9965", 95, 950},
		// No cTDP range: 0.50 x 165 W = 82.5 W, clamped to 0.55 x 165 W.
		{"xeon8260-50", "INTEL XEON PLATINUM 8260", "1", "INTEL_XEON_PLATINUM_8260", 50, 0.55 * 165},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := useHTTPCPUControl(t, "rapl")
			backend, result, msg := reconcilePctCap(t, kwokNode("cap-range-"+tc.name, tc.cpu, tc.sockets), tc.pct)
			got := rec.take()
			if len(got) == 0 || !strings.HasPrefix(got[0], "rapl.set_power_cap_watts ") {
				t.Fatalf("%s at %v%%: the control backend got %q, want rapl.set_power_cap_watts with %v W first: a node without RAPL files must resolve the percent against the catalog range, not fall back to the DVFS throttle",
					tc.cpu, tc.pct, got, tc.wantW)
			}
			var w float64
			fmt.Sscanf(strings.TrimPrefix(got[0], "rapl.set_power_cap_watts "), "%g", &w)
			if math.Abs(w-tc.wantW) > 1e-6 {
				t.Fatalf("%s x %s at %v%%: capWatts %v, want %v", tc.cpu, tc.sockets, tc.pct, w, tc.wantW)
			}
			if backend != "rapl" || result != "applied" || !strings.Contains(msg, "hardware catalog "+tc.catalogKey) {
				t.Fatalf("control status %s/%s %q, want rapl/applied naming the hardware catalog %s as the range's source", backend, result, msg, tc.catalogKey)
			}
		})
	}
}

// Without a range the percent keeps its DVFS throttle: a CPU the catalog
// does not know, a node without the socket label, whatever the pool agent's
// own host has, and an HTTP backend in dvfs mode, which takes no RAPL
// action.
func TestPercentCapWithoutACatalogRangeKeepsTheDVFSThrottle(t *testing.T) {
	for _, tc := range []struct {
		name, mode, cpu, sockets string
	}{
		{"unknown-cpu", "rapl", "Some Future CPU 9999", "2"},
		{"no-sockets", "rapl", "AMD-EPYC-9654-96-Core-Processor", ""},
		{"dvfs-mode", "dvfs", "AMD-EPYC-9654-96-Core-Processor", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := useHTTPCPUControl(t, tc.mode)
			backend, result, _ := reconcilePctCap(t, kwokNode("dvfs-fallback-"+tc.name, tc.cpu, tc.sockets), 60)
			if got := rec.take(); len(got) != 1 || got[0] != "dvfs.set_throttle_pct 40" {
				t.Fatalf("control backend got %q, want only dvfs.set_throttle_pct 40 for a 60%% cap", got)
			}
			if backend != "dvfs" || result != "applied" {
				t.Fatalf("control status %s/%s, want dvfs/applied", backend, result)
			}
		})
	}
}

// The catalog range is the one the simulator models the node with, so the
// agent reads the CPU model and socket count as the simulator does
// (hwinv.SimulatedNodeCPU): joulie.io/hw.cpu-model before NFD's
// cpu-model.name, and never its own host's /proc/cpuinfo (hostCPUInfo, four
// Xeon 8260 sockets). The agent used to take NFD's label first and fall back
// to its host, so the cap was off by the socket or the CPU of another
// machine.
func TestCatalogRangeReadsTheNodeLabelsTheSimulatorReads(t *testing.T) {
	rec := useHTTPCPUControl(t, "rapl")
	node := kwokNode("labels-like-the-simulator", "AMD-EPYC-9654-96-Core-Processor", "2")
	node.Labels["feature.node.kubernetes.io/cpu-model.name"] = "INTEL XEON PLATINUM 8260"
	reconcilePctCap(t, node, 60)
	if got := rec.take(); len(got) == 0 || got[0] != "rapl.set_power_cap_watts 640" {
		t.Fatalf("control backend got %q, want rapl.set_power_cap_watts 640 first: 2 x the EPYC 9654's 320 W cTDP low end, the CPU the simulator models from joulie.io/hw.cpu-model", got)
	}
}

// The node's own RAPL files come first, also under HTTP control. They hold
// one package's range, and the agent writes the cap into each package's
// file; an HTTP backend takes the node's total, so there the range counts
// every package.
func TestRAPLFilesWinOverTheCatalogRange(t *testing.T) {
	fourSocketFixture(t) // 4 packages, 90 to 165 W each
	labels := map[string]string{"joulie.io/hw.cpu-model": "AMD EPYC 9654 96-Core Processor", "joulie.io/hw.cpu-sockets": "2"}
	got, r, ok := resolveCPUCapFromPct(80, labels, false)
	if !ok || r.catalog || got != 132 {
		t.Fatalf("written to the files: resolved %v W from %q (catalog %v, ok %v), want 0.80 x 165 = 132 W per package from the RAPL files", got, r.source, r.catalog, ok)
	}
	got, r, ok = resolveCPUCapFromPct(80, labels, true)
	if !ok || r.catalog || got != 528 || r.minW != 360 || r.maxW != 660 {
		t.Fatalf("over HTTP: resolved %v W from %+v (ok %v), want 4 x 132 = 528 W, the node total of the 4 packages' range 360 to 660 W", got, r, ok)
	}
}

// A node controlled through its own files (no HTTP backend) never takes the
// catalog range: without RAPL files it keeps the DVFS percent fallback.
func TestCatalogRangeIsOnlyForHTTPRAPLControl(t *testing.T) {
	pointAgentAtSysfs(t, t.TempDir(), "", nil)
	labels := map[string]string{"joulie.io/hw.cpu-model": "AMD EPYC 9654 96-Core Processor", "joulie.io/hw.cpu-sockets": "2"}
	if got, r, ok := resolveCPUCapFromPct(80, labels, false); ok {
		t.Fatalf("resolved %v W from %q without RAPL files or HTTP control, want no range", got, r.source)
	}
	got, r, ok := resolveCPUCapFromPct(80, labels, true)
	if !ok || !r.catalog || got != 640 || r.minW != 640 || r.maxW != 720 {
		t.Fatalf("under HTTP control resolved %v W from %+v (ok %v), want 640 W from the catalog range 640 to 720 W", got, r, ok)
	}
}

// A percent cap whose watts the HTTP backend refuses (it rejects the action,
// times out, or restarts) keeps the DVFS throttle it had before it resolved
// into watts. The agent used to fall through to the watt-based DVFS path,
// which without a DVFS controller records "RAPL unavailable and DVFS
// disabled" and applies nothing.
func TestRefusedHTTPWattCapKeepsThePercentThrottle(t *testing.T) {
	rec := useHTTPCPUControl(t, "rapl")
	rec.setRefused("rapl.set_power_cap_watts", true)
	backend, result, msg := reconcilePctCap(t, kwokNode("refused-watts", "AMD-EPYC-9654-96-Core-Processor", "2"), 60)
	if got := rec.take(); strings.Join(got, ", ") != "rapl.set_power_cap_watts 640, dvfs.set_throttle_pct 40" {
		t.Fatalf("control backend got %q, want the refused rapl.set_power_cap_watts 640, then dvfs.set_throttle_pct 40", got)
	}
	if backend != "dvfs" || result != "applied" {
		t.Fatalf("control status %s/%s %q, want dvfs/applied", backend, result, msg)
	}
}

// The HTTP backend keeps a DVFS throttle beside the watt cap, as the
// simulator does. After a watt cap is applied the agent sets the throttle
// to 0 once, so a throttle it, or the agent before a rolling upgrade, sent
// for a percent cap does not stay under the watt cap; it clears it again
// after it next sends one. The agent used to leave it in place for good.
func TestHTTPWattCapClearsTheThrottleOnce(t *testing.T) {
	rec := useHTTPCPUControl(t, "rapl")
	reconcile := pctCapAgent(t, kwokNode("clears-throttle", "AMD-EPYC-9654-96-Core-Processor", "2"), 60)
	for _, step := range []struct {
		name    string
		refused bool
		want    string
	}{
		{"first watt cap", false, "rapl.set_power_cap_watts 640, dvfs.set_throttle_pct 0"},
		{"throttle already cleared", false, "rapl.set_power_cap_watts 640"},
		{"watts refused", true, "rapl.set_power_cap_watts 640, dvfs.set_throttle_pct 40"},
		{"watts accepted again", false, "rapl.set_power_cap_watts 640, dvfs.set_throttle_pct 0"},
	} {
		rec.setRefused("rapl.set_power_cap_watts", step.refused)
		reconcile()
		if got := strings.Join(rec.take(), ", "); got != step.want {
			t.Fatalf("%s: control backend got %q, want %q", step.name, got, step.want)
		}
	}
}
