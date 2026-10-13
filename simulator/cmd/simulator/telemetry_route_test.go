package main

import (
	"bytes"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// telemetryRouteGoldenDir holds the legacy JSON of /telemetry/{node} as the
// code before F8 wrote it; TL02 keeps ?format=legacy equal to it.
var telemetryRouteGoldenDir = filepath.Join("testdata", "telemetry-legacy")

// telemetryLegacyTS is the wall-clock "ts" of an analytic node's legacy
// JSON, the one field of it a golden cannot hold.
var telemetryLegacyTS = regexp.MustCompile(`"ts":"[^"]*"`)

// otlpTimes masks the time of every point: an analytic node's OTLP export
// is stamped with the wall clock of the read.
var otlpTimes = regexp.MustCompile(`"timeUnixNano":"[0-9]+"`)

// telemetryRouteAnalytic is a simulator of analytic nodes: node-cpu with no
// GPU and node-gpu with two L40S, both under load, and its mux.
func telemetryRouteAnalytic(t *testing.T) (*simulator, http.Handler) {
	t.Helper()
	s := newSimulatorWithRegisterer(otlpTestModel(90, 2), nil, nil, 10, prometheus.NewRegistry())
	s.nodeModels = map[string]simModel{"node-cpu": otlpTestModel(110, 0)}
	for _, n := range []string{"node-cpu", "node-gpu"} {
		otlpLoad(s, n)
	}
	return s, s.newMux()
}

// telemetryRouteGet reads path through h and fails unless it answers 200
// with JSON.
func telemetryRouteGet(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	w := emulateTestDo(t, h, http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d; body %s", path, w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s: Content-Type %q, want application/json", path, ct)
	}
	return w.Body.Bytes()
}

// telemetryRouteWatts reads body as an HTTP power input does: strictly
// decoded OTLP, through otlpjson.PowerReader, for the CPU and GPU
// components and for the host.
func telemetryRouteWatts(t *testing.T, node string, body []byte) (components, host float64) {
	t.Helper()
	md, err := otlpjson.DecodeMetricsStrict(body)
	if err != nil {
		t.Fatalf("%s: not an OTLP metrics export: %v\n%s", node, err, body)
	}
	var r otlpjson.PowerReader
	components, ok := r.Watts(md, otlpjson.Selector{Node: node, HwTypes: []string{semconv.HwTypeCPU, semconv.HwTypeGPU}})
	if !ok {
		t.Fatalf("%s: PowerReader read no CPU and GPU power from\n%s", node, body)
	}
	host, ok = r.Watts(md, otlpjson.Selector{Node: node, Host: true})
	if !ok {
		t.Fatalf("%s: PowerReader read no host power from\n%s", node, body)
	}
	return components, host
}

// telemetryRouteWarnings captures the log while f runs and counts the
// legacy route's deprecation warnings in it, starting from a process that
// has not warned yet.
func telemetryRouteWarnings(t *testing.T, f func()) (int, string) {
	t.Helper()
	legacyTelemetryWarned.Store(false)
	t.Cleanup(func() { legacyTelemetryWarned.Store(false) })
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	f()
	out := buf.String()
	return strings.Count(out, "format=legacy is deprecated"), out
}

// /telemetry/{node} serves by default the document of /otlp/telemetry/{node},
// for an analytic and an emulated node, and an HTTP power input reads it.
func TestTL01TelemetryDefaultsToOTLP(t *testing.T) {
	t.Run("analytic", func(t *testing.T) {
		s, mux := telemetryRouteAnalytic(t)
		n, _ := telemetryRouteWarnings(t, func() {
			for _, node := range []string{"node-cpu", "node-gpu"} {
				def := telemetryRouteGet(t, mux, "/telemetry/"+node)
				explicit := telemetryRouteGet(t, mux, "/telemetry/"+node+"?format=otlp")
				otlp := telemetryRouteGet(t, mux, "/otlp/telemetry/"+node)
				for name, got := range map[string][]byte{"default": def, "format=otlp": explicit} {
					if g, w := otlpTimes.ReplaceAll(got, nil), otlpTimes.ReplaceAll(otlp, nil); !bytes.Equal(g, w) {
						t.Errorf("%s %s differs from /otlp/telemetry beyond the point times:\n%s\nwant\n%s", node, name, g, w)
					}
				}
				comps, host := telemetryRouteWatts(t, node, def)
				legacy := lastTelemetryEvent(t, s)["packagePowerWatts"].(float64)
				if legacy <= 0 || math.Abs(comps-legacy) > 0.01 || math.Abs(host-legacy) > 0.01 {
					t.Errorf("%s: PowerReader components %.4f W, host %.4f W; want the node's %.2f W", node, comps, host, legacy)
				}
			}
		})
		if n != 0 {
			t.Errorf("the OTLP reads logged %d deprecation warnings, want none", n)
		}
		m := &dto.Metric{}
		if err := s.requestsTotal.WithLabelValues("telemetry", http.MethodGet, "200").Write(m); err != nil {
			t.Fatal(err)
		}
		if got := m.GetCounter().GetValue(); got != 4 {
			t.Errorf("requests{route=telemetry,method=GET,status=200} = %v, want 4", got)
		}
	})

	t.Run("emulated", func(t *testing.T) {
		et := newEmulateTest(t, emulateTestConfig{})
		snap := et.tick(t, emulateTestStart.Add(time.Second))
		mux := et.s.newMux()
		for _, pool := range []string{"intel", "nvl"} {
			node := et.node(t, pool)
			def := telemetryRouteGet(t, mux, "/telemetry/"+node)
			if otlp := telemetryRouteGet(t, mux, "/otlp/telemetry/"+node); !bytes.Equal(def, otlp) {
				t.Errorf("%s: /telemetry differs from /otlp/telemetry:\n%s\nwant\n%s", node, def, otlp)
			}
			var pkgW, gpuW, hostW float64
			for _, n := range snap.Nodes {
				if n.Name != node {
					continue
				}
				hostW = n.Host.PowerW
				for _, c := range n.Components {
					switch c.Type {
					case semconv.HwTypeCPU:
						pkgW += c.PowerW
					case semconv.HwTypeGPU:
						gpuW += c.PowerW
					}
				}
			}
			comps, host := telemetryRouteWatts(t, node, def)
			emulateNear(t, node+" PowerReader cpu and gpu", comps, pkgW+gpuW, 1e-6)
			emulateNear(t, node+" PowerReader host", host, hostW, 1e-6)
		}
	})
}

// ?format=legacy returns the legacy JSON byte for byte as the code before
// F8 served it on /telemetry/{node}, the analytic nodes' wall-clock "ts"
// aside. Regenerate with -update only for a change of the simulated
// numbers, never of the document's shape.
func TestTL02LegacyFormatIsTheOldDocument(t *testing.T) {
	got := map[string][]byte{}
	_, mux := telemetryRouteAnalytic(t)
	for _, node := range []string{"node-cpu", "node-gpu"} {
		body := telemetryRouteGet(t, mux, "/telemetry/"+node+"?format=legacy")
		if !telemetryLegacyTS.Match(body) {
			t.Fatalf("%s: no ts in %s", node, body)
		}
		got["analytic-"+node] = telemetryLegacyTS.ReplaceAll(body, []byte(`"ts":"<wall clock>"`))
	}
	et := newEmulateTest(t, emulateTestConfig{})
	et.tick(t, emulateTestStart.Add(time.Second))
	emux := et.s.newMux()
	for _, pool := range []string{"intel", "nvl"} {
		got["emulated-"+pool] = telemetryRouteGet(t, emux, "/telemetry/"+et.node(t, pool)+"?format=legacy")
	}

	for name, body := range got {
		path := filepath.Join(telemetryRouteGoldenDir, name+".json")
		if *updateGoldens {
			if err := os.MkdirAll(telemetryRouteGoldenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		if !bytes.Equal(body, want) {
			t.Errorf("%s: ?format=legacy is not the old document\ngot  %s\nwant %s", name, body, want)
		}
	}
}

// The legacy JSON warns once per process, naming its removal release and
// the default route, however many nodes read it.
func TestTL03LegacyFormatWarnsOncePerProcess(t *testing.T) {
	_, mux := telemetryRouteAnalytic(t)
	n, out := telemetryRouteWarnings(t, func() {
		for i := 0; i < 3; i++ {
			for _, node := range []string{"node-cpu", "node-gpu"} {
				telemetryRouteGet(t, mux, "/telemetry/"+node+"?format=legacy")
				telemetryRouteGet(t, mux, "/telemetry/"+node)
			}
		}
	})
	if n != 1 {
		t.Fatalf("%d deprecation warnings over 6 legacy reads, want 1; log:\n%s", n, out)
	}
	for _, want := range []string{legacyTelemetryRemovedIn, "/telemetry/{node}", "OTLP"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not name %q:\n%s", want, out)
		}
	}
}

// A format other than otlp or legacy, or a method other than GET, is
// refused before the node is read: no event, no warning.
func TestTL04TelemetryRefusesAnUnknownFormat(t *testing.T) {
	s, mux := telemetryRouteAnalytic(t)
	s.mu.Lock()
	before := len(s.events)
	s.mu.Unlock()
	n, _ := telemetryRouteWarnings(t, func() {
		for _, tc := range []struct {
			method, path string
			status       int
		}{
			{http.MethodGet, "/telemetry/node-cpu?format=xml", http.StatusBadRequest},
			{http.MethodGet, "/telemetry/node-cpu?format=LEGACY", http.StatusBadRequest},
			{http.MethodPost, "/telemetry/node-cpu?format=legacy", http.StatusMethodNotAllowed},
			{http.MethodGet, "/telemetry/?format=legacy", http.StatusBadRequest},
		} {
			if w := emulateTestDo(t, mux, tc.method, tc.path, nil); w.Code != tc.status {
				t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, w.Code, tc.status)
			}
		}
	})
	if n != 0 {
		t.Errorf("refused requests logged %d deprecation warnings, want none", n)
	}
	s.mu.Lock()
	after := len(s.events)
	s.mu.Unlock()
	if after != before {
		t.Errorf("refused requests recorded %d events", after-before)
	}
}
