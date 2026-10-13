package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/yaml"
)

// The reference scenario: a small heterogeneous inventory and 40 hand-written
// jobs over 2 h, run in standalone mode under baselines A, B and C. Its
// golden pins the simulated numbers, so any change to the physics, the job
// speed or the policy shows up as a diff to review, and -update rewrites it
// (golden_flag_test.go). B and C give the same numbers here: every hardware
// family keeps one performance node (policy.PerformanceSet), which on these
// nine nodes puts the same nodes in eco under both.
//
//	go test ./simulator/cmd/simulator/ -run ReferenceScenario -update
var (
	referenceDir    = filepath.Join("testdata", "reference")
	referenceGolden = filepath.Join(referenceDir, "reference.golden.json")
)

// referenceRelTol is the comparison tolerance, relative to the golden value.
// The run is deterministic on one architecture; another may fuse
// multiply-adds and round differently, so the test runs on amd64 only.
const referenceRelTol = 1e-9

// referenceGoldenFile is the golden: per baseline, flat keys named after the
// OpenTelemetry names they report, in joules, seconds or counts.
type referenceGoldenFile struct {
	Description string                        `json:"description"`
	Semconv     string                        `json:"semconv"`
	Baselines   map[string]map[string]float64 `json:"baselines"`
}

func TestReferenceScenarioStandalone(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("the golden is computed on amd64; %s may fuse multiply-adds and round differently", runtime.GOARCH)
	}
	got := referenceGoldenFile{
		Description: "TestReferenceScenarioStandalone: testdata/reference/inventory.yaml and trace.jsonl in standalone mode, SIM_SA_WORKERS=1, SIM_SA_DT=5, SIM_SA_TIMEOUT=7200, base model from the code defaults, embedded catalogue. Regenerate with: go test ./simulator/cmd/simulator/ -run ReferenceScenario -update",
		Semconv:     strings.TrimPrefix(semconv.SchemaURL, "https://opentelemetry.io/schemas/"),
		Baselines:   map[string]map[string]float64{},
	}
	for _, baseline := range []string{"A", "B", "C"} {
		got.Baselines[baseline] = runReferenceBaseline(t, baseline)
	}

	if *updateGoldens {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(referenceGolden, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", referenceGolden)
		return
	}
	raw, err := os.ReadFile(referenceGolden)
	if err != nil {
		t.Fatalf("%v (create it with -update)", err)
	}
	var want referenceGoldenFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: %v", referenceGolden, err)
	}
	if want.Semconv != got.Semconv {
		t.Errorf("golden semconv %s, code %s: check the names, then regenerate with -update", want.Semconv, got.Semconv)
	}
	if table := referenceDiff(want.Baselines, got.Baselines); table != "" {
		t.Errorf("the reference scenario moved (relative tolerance %g); review, then regenerate with -update:\n%s", referenceRelTol, table)
	}
}

// runReferenceBaseline runs the scenario once under baseline and returns its
// golden keys.
func runReferenceBaseline(t *testing.T, baseline string) map[string]float64 {
	t.Helper()
	clearSimEnv(t)
	for k, v := range map[string]string{
		"SIM_STANDALONE_INVENTORY": filepath.Join(referenceDir, "inventory.yaml"),
		"SIM_STANDALONE_BASELINE":  baseline,
		"SIM_SA_WORKERS":           "1",
		"SIM_SA_DT":                "5",
		"SIM_SA_TIMEOUT":           "7200",
	} {
		t.Setenv(k, v)
	}
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 0, prometheus.NewRegistry())
	catalog, err := hw.LoadCatalog("")
	if err != nil {
		t.Fatalf("embedded catalogue: %v", err)
	}
	s.catalog = catalog
	if err := s.initWorkloadEngineFromTrace(filepath.Join(referenceDir, "trace.jsonl")); err != nil {
		t.Fatal(err)
	}
	runStandalone(s)

	out := map[string]float64{}
	energy := semconv.HwEnergy.Name
	for _, typ := range []string{semconv.HwTypeCPU, semconv.HwTypeGPU} {
		out[fmt.Sprintf("%s{%s=%s}", energy, semconv.AttrHwType, typ)] = s.energyJByHwType[typ]
	}
	groups := referenceNodeGroups(t)
	for node, j := range s.energyJByNode {
		group, ok := groups[node]
		if !ok {
			t.Fatalf("node %s is not in the inventory", node)
		}
		out[fmt.Sprintf("%s{%s=%s}", energy, semconv.AttrK8sNodeName, group)] += j
	}

	var durations, waits []float64
	completed := 0
	for _, j := range s.workload.jobs {
		if !j.Completed {
			continue
		}
		completed++
		durations = append(durations, j.CompletedAt.Sub(j.SubmittedAt).Seconds())
		waits = append(waits, j.SubmittedAt.Sub(s.workload.startTime).Seconds()-j.SubmitOffsetSec)
	}
	out[semconv.AttrSimRunJobs] = float64(len(s.workload.jobs))
	out[semconv.AttrSimRunCompletedJobs] = float64(completed)
	out[semconv.AttrSimRunIncompleteJobs] = float64(len(s.workload.jobs) - completed)
	for name, values := range map[string][]float64{semconv.AttrSimJobDuration: durations, semconv.AttrSimJobQueueWait: waits} {
		mean, p50, max := referenceStats(values)
		out[name+"{stat=mean}"] = mean
		out[name+"{stat=p50}"] = p50
		out[name+"{stat=max}"] = max
	}
	return out
}

// referenceNodeGroups maps each node of the inventory to its entry: the
// node_name_prefix followed by -*, or the node_name.
func referenceNodeGroups(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(referenceDir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var inv inventoryFile
	if err := yaml.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, n := range inv.Nodes {
		if n.Replicas <= 1 && n.NodeName != "" {
			groups[n.NodeName] = n.NodeName
			continue
		}
		for i := 0; i < n.Replicas; i++ {
			groups[fmt.Sprintf("%s-%d", n.NodeNamePrefix, i)] = n.NodeNamePrefix + "-*"
		}
	}
	return groups
}

// referenceStats is the mean, the median (the lower middle value) and the
// maximum of values, 0 for none.
func referenceStats(values []float64) (mean, p50, max float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return sum / float64(len(sorted)), sorted[(len(sorted)-1)/2], sorted[len(sorted)-1]
}

// referenceDiff is a markdown table of every key that differs from the
// golden beyond referenceRelTol, or is on one side only; empty when none.
func referenceDiff(want, got map[string]map[string]float64) string {
	type row struct{ baseline, key, want, got, rel string }
	var rows []row
	baselines := map[string]bool{}
	for b := range want {
		baselines[b] = true
	}
	for b := range got {
		baselines[b] = true
	}
	for b := range baselines {
		keys := map[string]bool{}
		for k := range want[b] {
			keys[k] = true
		}
		for k := range got[b] {
			keys[k] = true
		}
		for k := range keys {
			w, wok := want[b][k]
			g, gok := got[b][k]
			switch {
			case !wok:
				rows = append(rows, row{b, k, "missing", fmt.Sprintf("%.10g", g), ""})
			case !gok:
				rows = append(rows, row{b, k, fmt.Sprintf("%.10g", w), "missing", ""})
			default:
				rel := math.Abs(g-w) / math.Max(math.Abs(w), 1e-12)
				if g != w && rel > referenceRelTol {
					rows = append(rows, row{b, k, fmt.Sprintf("%.10g", w), fmt.Sprintf("%.10g", g), fmt.Sprintf("%.3g", rel)})
				}
			}
		}
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].baseline != rows[j].baseline {
			return rows[i].baseline < rows[j].baseline
		}
		return rows[i].key < rows[j].key
	})
	var sb strings.Builder
	sb.WriteString("| baseline | key | golden | got | relative difference |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n", r.baseline, r.key, r.want, r.got, r.rel)
	}
	return sb.String()
}
