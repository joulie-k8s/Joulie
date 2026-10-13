package calib

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

func readFixture(t *testing.T, name, node string) (*Data, error) {
	t.Helper()
	fh, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	return ReadOTLP(fh, node)
}

// CB03: the reader takes int64 values and times as strings and as numbers,
// sums delta increments, drops the interval of a counter reset, reads one
// MetricsData per line with blank lines between, and keeps the resources of
// the node asked for.
func TestCB03DecodesIntsDeltasResetsAndLines(t *testing.T) {
	d, err := readFixture(t, "cb03-decode.jsonl", fixtureNode)
	if err != nil {
		t.Fatal(err)
	}
	if d.Node != fixtureNode {
		t.Errorf("node %q, want %q", d.Node, fixtureNode)
	}
	host := d.Select(semconv.HwHostEnergy.Name, map[string]string{semconv.AttrHwID: semconv.HwIDHost})
	if len(host) != 1 || !host[0].Counter || host[0].Delta {
		t.Fatalf("host energy series %+v, want one cumulative counter", host)
	}
	var got []float64
	for _, p := range host[0].Points {
		got = append(got, p.Value)
	}
	if want := []float64{1000, 1100, 1200, 50, 150, 250, 350}; !equalFloats(got, want) {
		t.Errorf("host energy %v, want %v (asInt as strings and as numbers)", got, want)
	}
	all := time.Unix(0, 0)
	end := time.Unix(1<<40, 0)
	if w, n := host[0].rate(all, end); n != 5 || math.Abs(w-100) > 1e-9 {
		t.Errorf("host rate %.6g W over %d intervals, want 100 W over 5: the reset interval is dropped", w, n)
	}

	cpu := d.Select(semconv.HwEnergy.Name, map[string]string{semconv.AttrHwID: "cpu0"})
	if len(cpu) != 1 || !cpu[0].Delta {
		t.Fatalf("cpu0 energy %+v, want one delta counter", cpu)
	}
	first := cpu[0].Points[0].Time
	if w, n := cpu[0].rate(first, end); n != 6 || math.Abs(w-40) > 1e-9 {
		t.Errorf("cpu0 rate %.6g W over %d increments, want 40 W over 6: deltas are summed, the one starting before the window is left out", w, n)
	}

	util := d.Select(semconv.SystemCPUUtilization.Name, map[string]string{semconv.AttrCPULogicalNumber: "1"})
	if len(util) != 1 || len(util[0].Points) != 7 {
		t.Fatalf("utilization of CPU 1: %+v, want 7 points with intValue and timeUnixNano as strings and numbers", util)
	}
	if segs, err := Segments(d); err != nil || len(segs) != 1 || len(segs[0].Times) != 7 {
		t.Errorf("segments %+v, %v; want one of 7 samples", segs, err)
	}
	if got := d.Select(semconv.HwHostPower.Name, nil); len(got) != 0 {
		t.Errorf("the other node's series leaked in: %+v", got)
	}

	other, err := readFixture(t, "cb03-decode.jsonl", "other-node")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Series) != 1 || other.Series[0].Metric != semconv.HwHostPower.Name {
		t.Errorf("other-node has %d series, want only its host power", len(other.Series))
	}
	if _, err := readFixture(t, "cb03-decode.jsonl", ""); err == nil || !strings.Contains(err.Error(), "other-node, "+fixtureNode) {
		t.Errorf("no node chosen in a two-node file: err %v, want one naming both nodes", err)
	}
	if _, err := readFixture(t, "cb03-decode.jsonl", "absent-node"); err == nil {
		t.Error("a node the file does not hold gave no error")
	}
	if _, err := ReadOTLP(strings.NewReader("{\"resourceMetrics\": [\n"), ""); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("a broken line: err %v, want one naming line 1", err)
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CB09: every name a sweep carries comes from pkg/otel/semconv, every
// resource and scope of the fixtures and of a generated sweep declares
// schemaUrl 1.44.0, and the package spells no name itself.
func TestCB09NamesComeFromSemconvAndFixturesCarryTheSchema(t *testing.T) {
	registered := map[string]bool{}
	for _, m := range semconv.Metrics() {
		registered[m.Name] = true
	}
	attrs := map[string]bool{}
	for _, a := range semconv.Attributes() {
		attrs[a] = true
	}
	check := func(where string, line []byte) {
		t.Helper()
		md, err := otlpjson.DecodeMetricsStrict(line)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		for _, rm := range md.ResourceMetrics {
			if rm.SchemaURL != semconv.SchemaURL {
				t.Errorf("%s: resource schemaUrl %q, want %q", where, rm.SchemaURL, semconv.SchemaURL)
			}
			for _, kv := range rm.Resource.Attributes {
				if !attrs[kv.Key] {
					t.Errorf("%s: resource attribute %q is not in semconv", where, kv.Key)
				}
			}
			for _, sm := range rm.ScopeMetrics {
				if sm.SchemaURL != semconv.SchemaURL {
					t.Errorf("%s: scope %q schemaUrl %q, want %q", where, sm.Scope.Name, sm.SchemaURL, semconv.SchemaURL)
				}
				for _, m := range sm.Metrics {
					if !registered[m.Name] {
						t.Errorf("%s: metric %q is not in semconv", where, m.Name)
					}
					var pts []otlpjson.NumberDataPoint
					switch {
					case m.Sum != nil:
						pts = m.Sum.DataPoints
					case m.Gauge != nil:
						pts = m.Gauge.DataPoints
					}
					for _, p := range pts {
						for _, kv := range p.Attributes {
							if !attrs[kv.Key] {
								t.Errorf("%s: %s point attribute %q is not in semconv", where, m.Name, kv.Key)
							}
						}
					}
				}
			}
		}
	}

	files, err := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, maxLine)
		for n := 1; sc.Scan(); n++ {
			if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
				check(f+":"+strconv.Itoa(n), line)
			}
		}
	}
	sw := generateSweep(t, sweepSpec{
		profile: "nvidia-h100-nvl-8gpu", period: time.Second,
		steps: []sweepStep{{knobs: map[string]float64{semconv.KnobGPUUtilization: 1, semconv.KnobGPUPowerLimit: 300}, gpuClass: "gpu.compute_bound", samples: 2}},
	})
	for i, line := range sw.lines {
		check("generated sweep line "+strconv.Itoa(i+1), line)
	}

	// No literal in the package's code spells a semconv name: a string
	// constant starting with one of the namespaces the table uses.
	spelled := regexp.MustCompile(`^"(hw|joulie|system|cpu|k8s|host)\.[a-z]`)
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, src := range srcs {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, src, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && spelled.MatchString(lit.Value) {
				t.Errorf("%s: %s spells a name; use the pkg/otel/semconv constant", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}
