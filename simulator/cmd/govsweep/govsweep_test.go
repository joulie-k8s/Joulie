package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

var update = flag.Bool("update", false, "rewrite testdata/gx01.golden.csv")

// experimentConfigs are the committed sweep configs of experiment 05.
var experimentConfigs = filepath.Join("..", "..", "..", "experiments", "05-governor-exploration", "configs")

// sweep runs govsweep with args plus -out and -work in fresh directories,
// and returns the output directory, the work directory, stdout and stderr.
func sweep(t *testing.T, want int, args ...string) (out, work string, stdout, stderr string) {
	t.Helper()
	out, work = t.TempDir(), t.TempDir()
	var o, e bytes.Buffer
	code := run(context.Background(), append(args, "-out", out, "-work", work), &o, &e, func(string) string { return "" })
	if code != want {
		t.Fatalf("govsweep %v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", args, code, want, o.String(), e.String())
	}
	return out, work, o.String(), e.String()
}

// writeConfig writes a sweep config into a temporary file.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sweep.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// readCSV is the records of a CSV file.
func readCSV(t *testing.T, p string) [][]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return recs
}

// readLines is the lines of a file, without the final newline.
func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// gx01Config is one CPU cell and one GPU cell, short enough for a unit test:
// the efficiency preset on the intel_cpufreq Xeon at a steady 50 % load, and
// the efficient preset at 75 % on two L40S GPUs with peer transfers.
const gx01Config = `
name: gx01
seeds: [7]
durationS: 6
settleS: 2
cpu:
  profiles: [intel-xeon-6530-2s-rapl]
  arms: [{preset: efficiency}]
  steadyPct: [50]
  classes: {cpu.compute_bound: {memoryIntensity: 0.2, ioIntensity: 0.05}}
gpu:
  profiles: [nvidia-l40s-4gpu]
  arms: [{preset: efficient, ceilingPct: [75]}]
  capsPct: [100]
  receiverGates: [profile]
  classes: {gpu.memory_bound: {memoryIntensity: 0.85}}
  jobs: [{gpus: 2, peer: [0.3]}]
  cpuUtil: 0.05
`

// TestGX01OneCellIsDeterministicForASeed runs the same cells twice, in
// separate directories, and checks that both runs write the same CSV rows,
// equal to the golden rows.
func TestGX01OneCellIsDeterministicForASeed(t *testing.T) {
	cfg := writeConfig(t, gx01Config)
	var runs [2][]string
	for i := range runs {
		out, _, _, _ := sweep(t, 0, "-config", cfg, "-parallel", "2")
		for _, g := range []string{gridCPU, gridGPU} {
			lines := readLines(t, filepath.Join(out, "gx01-"+g+".csv"))
			if len(lines) != 2 {
				t.Fatalf("%s CSV has %d lines, want a header and one row:\n%s", g, len(lines), strings.Join(lines, "\n"))
			}
			runs[i] = append(runs[i], lines...)
		}
	}
	if !slices.Equal(runs[0], runs[1]) {
		t.Fatalf("two runs of the same cells differ:\n%s\n---\n%s", strings.Join(runs[0], "\n"), strings.Join(runs[1], "\n"))
	}
	for _, line := range runs[0] {
		if strings.Contains(line, ",ok,") || strings.HasPrefix(line, "cell,") {
			continue
		}
		t.Errorf("a cell did not run: %s", line)
	}
	golden := filepath.Join("testdata", "gx01.golden.csv")
	got := strings.Join(runs[0], "\n") + "\n"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (go test ./simulator/cmd/govsweep -run GX01 -update writes it)", err)
	}
	if got != string(want) {
		t.Errorf("the rows moved from %s (rerun with -update and review the diff):\ngot:\n%s\nwant:\n%s", golden, got, want)
	}
}

// TestGX02EveryArmResolvesThroughPkgGovernor checks that a config naming a
// native word, under any key, is refused, that every committed config of
// experiment 05 loads, and that what a cell writes is the pkg/governor plan
// of its preset: the files of a kept tree hold the plan's values, and the
// row's native state and write count are the plan's.
func TestGX02EveryArmResolvesThroughPkgGovernor(t *testing.T) {
	base := "name: gx02\nseeds: [1]\ndurationS: 1\n"
	cpuGrid := func(arm string) string {
		return base + "cpu:\n  profiles: [amd-epyc-9534-2s-energy-only]\n  arms: [" + arm + "]\n  idle: true\n"
	}
	gpuGrid := func(arm string) string {
		return base + "gpu:\n  profiles: [nvidia-l40s-4gpu]\n  arms: [" + arm + "]\n  capsPct: [100]\n  receiverGates: [profile]\n  idle: true\n"
	}
	for name, body := range map[string]string{
		"cpu governor":           cpuGrid("{preset: schedutil}"),
		"cpu governor conserv":   cpuGrid("{preset: conservative}"),
		"cpu epp":                cpuGrid("{preset: balance_power}"),
		"cpu boost bit":          cpuGrid("{preset: performance, boost: \"1\"}"),
		"cpu native key":         cpuGrid("{preset: performance, governor: schedutil}"),
		"amdgpu level":           gpuGrid("{preset: high}"),
		"amdgpu profile_peak":    gpuGrid("{preset: profile_peak}"),
		"nvidia lock":            gpuGrid("{preset: \"-lgc 300,1500\"}"),
		"nvidia lock key":        gpuGrid("{preset: auto, lgc: \"300,1500\"}"),
		"ceiling on another arm": gpuGrid("{preset: peak, ceilingPct: [50]}"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, stderr := sweep(t, 2, "-config", writeConfig(t, body))
			if !strings.Contains(stderr, "preset") && !strings.Contains(stderr, "unknown field") && !strings.Contains(stderr, "ceilingPct") {
				t.Errorf("the refusal does not say why:\n%s", stderr)
			}
		})
	}

	configs, err := filepath.Glob(filepath.Join(experimentConfigs, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sweeps := 0
	for _, p := range configs {
		if strings.HasPrefix(filepath.Base(p), "tier-b") {
			continue // Helm values, not sweeps
		}
		sweeps++
		if _, err := loadConfig(p); err != nil {
			t.Errorf("committed config: %v", err)
		}
	}
	if sweeps == 0 {
		t.Errorf("no sweep config under %s", experimentConfigs)
	}

	// What a cell writes is the plan.
	cfg := writeConfig(t, base+`cpu:
  profiles: [intel-xeon-2s-rapl]
  arms: [{preset: efficiency, boost: "off"}]
  idle: true
gpu:
  profiles: [nvidia-l40s-4gpu]
  arms: [{preset: efficient, ceilingPct: [60]}]
  capsPct: [100]
  receiverGates: [profile]
  idle: true
`)
	out, work, _, _ := sweep(t, 0, "-config", cfg, "-keep")
	rows := map[string][]string{}
	for _, g := range []string{gridCPU, gridGPU} {
		recs := readCSV(t, filepath.Join(out, "gx02-"+g+".csv"))
		if len(recs) != 2 {
			t.Fatalf("%s: %d records, want a header and one row", g, len(recs))
		}
		rows[g] = recs[1]
	}
	cpuCell := filepath.Join(work, "cells", rows[gridCPU][0])
	sys := filepath.Join(cpuCell, layout.SysDir)
	f := cpuFacts(sys, filepath.Join(cpuCell, layout.CPUInfoFile))
	plan, err := governor.PlanCPU(f, joulie.CPUGovernorPresetEfficiency, joulie.CPUGovernorBoostOff)
	if err != nil || plan.Result != joulie.GovernorResultApplied || len(plan.Writes) == 0 {
		t.Fatalf("PlanCPU on the kept tree: %+v, %v", plan, err)
	}
	for _, w := range plan.Writes {
		if got := readWord(filepath.Join(sys, w.RelPath)); got != w.Value {
			t.Errorf("%s holds %q, the plan writes %q", w.RelPath, got, w.Value)
		}
	}
	if i := slices.Index(cpuColumns, "native"); rows[gridCPU][i] != plan.Native {
		t.Errorf("the CPU row's native state is %q, the plan's %q", rows[gridCPU][i], plan.Native)
	}
	if i := slices.Index(cpuColumns, "writes"); rows[gridCPU][i] != strconv.Itoa(len(plan.Writes)) {
		t.Errorf("the CPU row counts %s writes, the plan has %d", rows[gridCPU][i], len(plan.Writes))
	}

	gpuCell := filepath.Join(work, "cells", rows[gridGPU][0])
	locks, _ := filepath.Glob(filepath.Join(gpuCell, layout.StateDir, layout.NVMLDir, "gpu*", layout.NVMLLockedGPUClocksFile))
	if len(locks) != 4 {
		t.Fatalf("%d graphics lock files in the kept L40S tree, want 4", len(locks))
	}
	i := slices.Index(gpuColumns, "native")
	native := rows[gridGPU][i]
	lock := governor.ParseNative(joulie.GovernorBackendNvidiaSMI, native).GPULockMHz
	if lock == nil {
		t.Fatalf("the GPU row's native state is %q, want the plan's graphics lock", native)
	}
	for _, l := range locks {
		if got, want := readWord(l), fmt.Sprintf("%d,%d", lock[0], lock[1]); got != want {
			t.Errorf("%s holds %q, the plan locks %q", l, got, want)
		}
	}
}

// TestGX03OTLPOutputDecodes checks that every line of each grid's OTLP file
// decodes strictly with pkg/otel/otlpjson, one export per cell that ran,
// whose resource is the cell and which carries the host energy.
func TestGX03OTLPOutputDecodes(t *testing.T) {
	out, _, _, _ := sweep(t, 0, "-config", writeConfig(t, gx01Config))
	for _, g := range []string{gridCPU, gridGPU} {
		rows := readCSV(t, filepath.Join(out, "gx01-"+g+".csv"))[1:]
		f, err := os.Open(filepath.Join(out, "gx01-"+g+".otlp.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 64<<20)
		n := 0
		for sc.Scan() {
			d, err := otlpjson.DecodeMetricsStrict(sc.Bytes())
			if err != nil {
				t.Fatalf("%s line %d: %v", g, n+1, err)
			}
			if len(d.ResourceMetrics) != 1 {
				t.Fatalf("%s line %d: %d resources, want the cell's node", g, n+1, len(d.ResourceMetrics))
			}
			rm := d.ResourceMetrics[0]
			cell := rows[n][0]
			host := ""
			for _, kv := range rm.Resource.Attributes {
				if kv.Key == semconv.AttrHostName && kv.Value.String != nil {
					host = *kv.Value.String
				}
			}
			if host != cell {
				t.Errorf("%s line %d: host.name %q, want the cell %q", g, n+1, host, cell)
			}
			var names []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					names = append(names, m.Name)
				}
			}
			for _, want := range []string{semconv.HwHostEnergy.Name, semconv.HwEnergy.Name} {
				if !slices.Contains(names, want) {
					t.Errorf("%s line %d: no %s among %v", g, n+1, want, names)
				}
			}
			if g == gridGPU && !slices.Contains(names, semconv.HwGPUIO.Name) {
				t.Errorf("gpu line %d: no %s for a job with peer transfers", n+1, semconv.HwGPUIO.Name)
			}
			n++
		}
		f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if n != len(rows) {
			t.Errorf("%s: %d exports for %d cells", g, n, len(rows))
		}
	}
}

// TestGX04ParallelCellsUseSeparateRoots checks that every cell of the
// committed Tier A grid has its own id, and so its own root, and that cells
// run in parallel leave one tree each, rendered for that cell alone, with
// the same rows as the cells run one at a time.
func TestGX04ParallelCellsUseSeparateRoots(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(experimentConfigs, "tier-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cfg.expand() {
		if seen[c.ID()] {
			t.Fatalf("two cells share the id, and so the root, %s", c.ID())
		}
		seen[c.ID()] = true
	}

	// A value repeated on an axis would give two cells one id, and so one
	// root: the config is refused before any cell runs.
	for name, axis := range map[string]string{
		"seeds":   "seeds: [1, 1]\n  steadyPct: [50]",
		"a value": "seeds: [1]\n  steadyPct: [50, 50]",
	} {
		t.Run("repeated "+name, func(t *testing.T) {
			seeds, steady, _ := strings.Cut(axis, "\n  ")
			body := "name: gx04dup\n" + seeds + "\ndurationS: 1\ncpu:\n  profiles: [intel-xeon-2s-rapl]\n  arms: [{preset: efficiency}]\n  " +
				steady + "\n  classes: {cpu.mixed: {memoryIntensity: 0.5}}\n"
			_, work, _, stderr := sweep(t, 2, "-config", writeConfig(t, body), "-parallel", "4")
			if !strings.Contains(stderr, "cpu_intel-xeon-2s-rapl_efficiency_steady50_cpu.mixed_s1") {
				t.Errorf("the refusal does not name the repeated cell:\n%s", stderr)
			}
			if _, err := os.Stat(filepath.Join(work, "cells")); err == nil {
				t.Errorf("a cell ran before the refusal")
			}
		})
	}

	body := `
name: gx04
seeds: [1, 2]
durationS: 3
settleS: 1
cpu:
  profiles: [amd-epyc-2s-hsmp, intel-xeon-2s-rapl]
  arms: [{preset: performance}, {preset: efficiency}]
  steadyPct: [70]
  classes: {cpu.mixed: {memoryIntensity: 0.5, ioIntensity: 0.1}}
`
	cfgPath := writeConfig(t, body)
	par, work, _, _ := sweep(t, 0, "-config", cfgPath, "-parallel", "8", "-keep")
	seq, _, _, _ := sweep(t, 0, "-config", cfgPath, "-parallel", "1")
	a, b := readLines(t, filepath.Join(par, "gx04-cpu.csv")), readLines(t, filepath.Join(seq, "gx04-cpu.csv"))
	if !slices.Equal(a, b) {
		t.Errorf("parallel and sequential runs differ:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	entries, err := os.ReadDir(filepath.Join(work, "cells"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(a)-1 {
		t.Fatalf("%d cell roots for %d cells", len(entries), len(a)-1)
	}
	// No file is in two roots: a file at the same place in two trees is two
	// files, not one reached through a link.
	type place struct {
		cell string
		fi   os.FileInfo
	}
	byRel := map[string][]place{}
	for _, e := range entries {
		root := filepath.Join(work, "cells", e.Name())
		if _, err := os.Stat(filepath.Join(root, layout.SysDir)); err != nil {
			t.Errorf("cell %s has no tree of its own: %v", e.Name(), err)
		}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			fi, err := os.Stat(p)
			if err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			for _, o := range byRel[rel] {
				if os.SameFile(o.fi, fi) {
					t.Errorf("%s is shared by cells %s and %s", rel, o.cell, e.Name())
				}
			}
			byRel[rel] = append(byRel[rel], place{e.Name(), fi})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
