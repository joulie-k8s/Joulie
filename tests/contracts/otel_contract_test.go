package contracts_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

// The simulator writes OpenTelemetry metrics and the controller manager
// reads them back by name, so the export must use exactly the names, units
// and point attributes of pkg/otel/semconv, under the producer rules of the
// semantic conventions: whole-host energy only as hw.host.*, which carries
// no hw.type. These tests run the emulator over the harness's smoke spec,
// with real hwemu trees, and check the export it pushes.

// otelContractExport is the metrics export of the smoke spec after a few
// ticks, as the simulator pushes it.
func otelContractExport(t *testing.T) otlpjson.MetricsData {
	t.Helper()
	spec, err := clusterspec.LoadFile(filepath.Join(repoRoot(t), "tests", "simcluster", "specs", "smoke.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	members, err := spec.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e, err := emulate.New(emulate.Config{
		Root:     filepath.Join(t.TempDir(), "hwemu"),
		Cluster:  spec.Metadata.Name,
		Members:  members,
		Facility: spec.Spec.Facility,
		Start:    start,
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	var snap *emulate.Snapshot
	for k := 1; k <= 3; k++ {
		if snap, err = e.Tick(start.Add(time.Duration(k)*time.Second), nil, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	d, dropped := telemetry.MetricsRequest(snap, telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "contract"})
	if dropped != 0 {
		t.Errorf("%d values dropped from the export", dropped)
	}
	if len(d.ResourceMetrics) == 0 {
		t.Fatal("the export has no resource")
	}
	return d
}

// otelContractPoints calls f with every number point of d, its metric and
// its resource attributes.
func otelContractPoints(d otlpjson.MetricsData, f func(res map[string]string, m otlpjson.Metric, p otlpjson.NumberDataPoint)) {
	for _, rm := range d.ResourceMetrics {
		res := otelContractAttrs(rm.Resource.Attributes)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				var points []otlpjson.NumberDataPoint
				switch {
				case m.Sum != nil:
					points = m.Sum.DataPoints
				case m.Gauge != nil:
					points = m.Gauge.DataPoints
				}
				for _, p := range points {
					f(res, m, p)
				}
			}
		}
	}
}

// otelContractAttrs is a list of attributes as a map of their string form.
func otelContractAttrs(kvs []otlpjson.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.Value; {
		case v.String != nil:
			out[kv.Key] = *v.String
		default:
			out[kv.Key] = ""
		}
	}
	return out
}

func TestCO01EmittedNamesAreInTheSemconvTable(t *testing.T) {
	d := otelContractExport(t)
	table := map[string]semconv.Metric{}
	for _, m := range semconv.Metrics() {
		table[m.Name] = m
	}
	nodes := 0
	for _, rm := range d.ResourceMetrics {
		res := otelContractAttrs(rm.Resource.Attributes)
		if node, ok := res[semconv.AttrK8sNodeName]; ok {
			nodes++
			if inst := res[semconv.AttrServiceInstanceID]; inst != node {
				t.Errorf("resource of node %s has service.instance.id %q", node, inst)
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				def, ok := table[m.Name]
				if !ok {
					t.Errorf("metric %q is not in semconv.Metrics()", m.Name)
					continue
				}
				if m.Unit != def.Unit {
					t.Errorf("metric %s has unit %q, the table says %q", m.Name, m.Unit, def.Unit)
				}
			}
		}
	}
	if nodes == 0 {
		t.Error("no node resource in the export")
	}
	otelContractPoints(d, func(res map[string]string, m otlpjson.Metric, p otlpjson.NumberDataPoint) {
		def, ok := table[m.Name]
		if !ok {
			return
		}
		for _, kv := range p.Attributes {
			if !def.HasAttribute(kv.Key) {
				t.Errorf("a point of %s on %s has attribute %s, which the table does not list for it (%v)",
					m.Name, res[semconv.AttrServiceInstanceID], kv.Key, def.Attributes)
			}
		}
	})
}

func TestCO02HostEnergyIsOnlyHwHost(t *testing.T) {
	d := otelContractExport(t)
	hostPoints := 0
	otelContractPoints(d, func(res map[string]string, m otlpjson.Metric, p otlpjson.NumberDataPoint) {
		attrs := otelContractAttrs(p.Attributes)
		node := res[semconv.AttrServiceInstanceID]
		switch m.Name {
		case semconv.HwEnergy.Name, semconv.HwPower.Name:
			if attrs[semconv.AttrHwID] == semconv.HwIDHost {
				t.Errorf("%s on %s has hw.id=%s: whole-host energy goes only to hw.host.*", m.Name, node, semconv.HwIDHost)
			}
		case semconv.HwHostEnergy.Name, semconv.HwHostPower.Name, semconv.HwHostAmbientTemperature.Name:
			hostPoints++
			if _, ok := attrs[semconv.AttrHwType]; ok {
				t.Errorf("%s on %s carries hw.type", m.Name, node)
			}
		}
	})
	if hostPoints == 0 {
		t.Error("no hw.host.* point in the export")
	}
}

// otelContractLiteral matches a string literal that starts with a name of
// the two OpenTelemetry namespaces Joulie writes: hw. followed by a letter,
// or joulie.<word>. (joulie.io/ label keys do not match).
var otelContractLiteral = regexp.MustCompile(`"hw\.[a-z]|"joulie\.[a-z_]+\.`)

func TestCO04OTelNamesAreSpelledOnlyInSemconv(t *testing.T) {
	root := repoRoot(t)
	semconvDir := filepath.Join(root, "pkg", "otel", "semconv")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") || path == semconvDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for line := 1; sc.Scan(); line++ {
			if otelContractLiteral.MatchString(sc.Text()) {
				rel, _ := filepath.Rel(root, path)
				found = append(found, rel+":"+intToStr(line)+": "+strings.TrimSpace(sc.Text()))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s\n\tan OpenTelemetry name spelled outside pkg/otel/semconv; use its constant", f)
	}
}
