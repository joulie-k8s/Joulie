// Command govsweep runs experiment 05's offline sweep (Tier A of the
// governor exploration, experiments/05-governor-exploration): hwemu nodes in
// virtual time, one fresh node per cell, each set to a governor arm the way
// the agent would set it and then loaded with a job, an idle period or
// bursts:
//
//	govsweep -config FILE -out DIR [-work DIR] [-parallel N] [-match REGEXP]
//	         [-profiles-dir DIR] [-keep] [-force]
//
// The config (experiments/05-governor-exploration/configs/*.yaml) lists the
// grids: E-G1, CPU profiles under each CPU preset, idle, at steady loads of
// each workload class, and under bursts; E-G2, GPU profiles under each GPU
// preset, power cap and receiver gate, idle and with jobs of each GPU class
// on 1 to N GPUs, with peer or NIC transfers. Every arm names a
// vendor-neutral preset (pkg/api); govsweep reads the emulated node's
// cpufreq, amdgpu and nvidia-smi facts as the agent's discovery does, plans
// the preset with pkg/governor and writes the plan's sysfs attributes or
// runs its nvidia-smi commands against the node's fake tools. A native word
// in a config is rejected.
//
// Each cell renders its node into its own directory, <work>/cells/<cell>,
// removed when the cell ends unless -keep is given; cells run in parallel,
// -parallel at a time (all CPUs by default), and never share a file. A job
// does a fixed amount of work: what it does at the reference (no cap,
// performance or auto, nothing slowed) in durationS. It progresses at the
// simulator's v1 workload rate (simulator/pkg/workload.Rates) under the
// throughput the node reports after each step (hwemu.Node.CPUThroughput,
// GPUThroughput, PeerTransferRates), and its energy is the node's ledger
// from its start to its completion.
//
// Output, in -out: <name>-cpu.csv and <name>-gpu.csv, one row per cell (the
// columns are listed in output.go), and <name>-cpu.otlp.jsonl and
// <name>-gpu.otlp.jsonl, one OTLP JSON metrics export per cell that ran: the
// node at the end of the cell, as the simulator exports an emulated node
// (hw.host.energy, hw.energy, hw.gpu.io, joulie.hw.governor and the rest of
// pkg/otel/semconv), with the cell id as its host name. An existing output
// file is refused unless -force is given. -match runs only the cells whose
// id matches. -profiles-dir (default SIM_PROFILES_DIR) adds local profiles.
//
// The exit status is 0 when every cell ran, 1 when a cell failed or an
// output could not be written, and 2 on a usage or config error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

const usage = `usage: govsweep -config FILE -out DIR [-work DIR] [-parallel N] [-match REGEXP] [-profiles-dir DIR] [-keep] [-force]`

// serviceName is the service.name of the sweep's OTLP export.
const serviceName = "joulie-govsweep"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("govsweep", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "sweep config file")
	out := fs.String("out", "", "output directory")
	work := fs.String("work", "", "directory for the cells' node trees (default: a new temporary directory)")
	parallel := fs.Int("parallel", runtime.GOMAXPROCS(0), "cells run at once")
	match := fs.String("match", "", "run only the cells whose id matches this regular expression")
	profilesDir := fs.String("profiles-dir", getenv("SIM_PROFILES_DIR"), "local hardware profiles, as SIM_PROFILES_DIR")
	keep := fs.Bool("keep", false, "keep each cell's node tree")
	force := fs.Bool("force", false, "replace existing output files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" || *out == "" || fs.NArg() > 0 || *parallel < 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "govsweep: %v\n", err)
		return 2
	}
	var re *regexp.Regexp
	if *match != "" {
		if re, err = regexp.Compile(*match); err != nil {
			fmt.Fprintf(stderr, "govsweep: -match: %v\n", err)
			return 2
		}
	}
	profiles := newProfileCache(*profilesDir)
	if _, _, err := profiles.get("", nil, ""); err != nil {
		fmt.Fprintf(stderr, "govsweep: %v\n", err)
		return 2
	}
	var cells []Cell
	for _, c := range cfg.expand() {
		if _, ok := profiles.base[c.Profile]; !ok {
			fmt.Fprintf(stderr, "govsweep: profile %q is not a builtin or local profile\n", c.Profile)
			return 2
		}
		if re == nil || re.MatchString(c.ID()) {
			cells = append(cells, c)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(stderr, "govsweep: %v\n", err)
		return 1
	}
	outputs := map[string]string{}
	for _, g := range []string{gridCPU, gridGPU} {
		outputs[g+".csv"] = filepath.Join(*out, cfg.Name+"-"+g+".csv")
		outputs[g+".otlp"] = filepath.Join(*out, cfg.Name+"-"+g+".otlp.jsonl")
	}
	if !*force {
		for _, p := range outputs {
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(stderr, "govsweep: %s exists; remove it or pass -force\n", p)
				return 1
			}
		}
	}
	dir := *work
	if dir == "" {
		if dir, err = os.MkdirTemp("", "govsweep-"); err != nil {
			fmt.Fprintf(stderr, "govsweep: %v\n", err)
			return 1
		}
		if !*keep {
			defer os.RemoveAll(dir)
		}
	}
	r := &runner{cfg: cfg, profiles: profiles.get, work: dir, keep: *keep,
		resource: telemetry.Resource{ServiceName: serviceName, Instance: cfg.Name}}
	rows, exports, errs := runCells(ctx, r, cells, *parallel, stderr)
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "govsweep: cell %s: %v\n", cells[i].ID(), err)
		}
	}
	if err := writeOutputs(outputs, rows, exports, cells); err != nil {
		fmt.Fprintf(stderr, "govsweep: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "govsweep: %d cells, %d failed; results in %s\n", len(cells), failed, *out)
	if failed > 0 {
		return 1
	}
	return 0
}

// runCells runs cells on parallel workers and returns, by cell index, each
// row, export and error. A cell whose profile has no fact its sensitivity
// variant sets gives no row and no error.
func runCells(ctx context.Context, r *runner, cells []Cell, parallel int, progress io.Writer) ([]Row, []*otlpjson.MetricsData, []error) {
	rows := make([]Row, len(cells))
	exports := make([]*otlpjson.MetricsData, len(cells))
	errs := make([]error, len(cells))
	next := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, start := 0, time.Now()
	for w := 0; w < min(parallel, max(1, len(cells))); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				row, d, err := r.run(ctx, cells[i])
				if errors.Is(err, errNoVariant) {
					err = nil
				} else if err == nil {
					rows[i] = row
					if len(d.ResourceMetrics) > 0 {
						exports[i] = &d
					}
				}
				errs[i] = err
				mu.Lock()
				done++
				if done%200 == 0 || done == len(cells) {
					fmt.Fprintf(progress, "govsweep: %d of %d cells after %s\n", done, len(cells), time.Since(start).Round(time.Second))
				}
				mu.Unlock()
			}
		}()
	}
	for i := range cells {
		next <- i
	}
	close(next)
	wg.Wait()
	return rows, exports, errs
}

// writeOutputs writes each grid's CSV and OTLP file, in cell order; a grid
// with no cell writes neither.
func writeOutputs(outputs map[string]string, rows []Row, exports []*otlpjson.MetricsData, cells []Cell) error {
	for _, g := range []string{gridCPU, gridGPU} {
		var gr []Row
		var ex []*otlpjson.MetricsData
		for i, c := range cells {
			if c.Grid == g {
				gr = append(gr, rows[i])
				ex = append(ex, exports[i])
			}
		}
		if len(gr) == 0 {
			continue
		}
		if err := writeFile(outputs[g+".csv"], func(w io.Writer) error { return writeCSV(w, g, gr) }); err != nil {
			return err
		}
		if err := writeFile(outputs[g+".otlp"], func(w io.Writer) error {
			for _, d := range ex {
				if d == nil {
					continue
				}
				if err := writeJSONLine(w, d); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a file through a temporary file in its directory, so a
// failed run leaves no half-written result.
func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// profileCache holds the builtin and local profiles, and a fresh copy of
// them for each sensitivity variant, which changes its copy only.
type profileCache struct {
	dir      string
	mu       sync.Mutex
	base     map[string]*hwemu.Profile
	variants map[string]map[string]*hwemu.Profile
	ok       map[string]bool
}

func newProfileCache(dir string) *profileCache {
	return &profileCache{dir: dir, variants: map[string]map[string]*hwemu.Profile{}, ok: map[string]bool{}}
}

// get returns profile name, as the sensitivity variant named variant sets
// it with sens (nil for the base profile). ok is false when the variant sets nothing on it.
// The profiles are shared by the cells that ask: hwemu only reads them.
func (pc *profileCache) get(variant string, sens func(*hwemu.Profile) bool, name string) (*hwemu.Profile, bool, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.base == nil {
		ps, _, err := hwemu.LoadProfileSet(pc.dir)
		if err != nil {
			return nil, false, err
		}
		pc.base = ps
	}
	if sens == nil {
		p, ok := pc.base[name]
		return p, ok, nil
	}
	key := variant
	set, seen := pc.variants[key]
	if !seen {
		ps, _, err := hwemu.LoadProfileSet(pc.dir)
		if err != nil {
			return nil, false, err
		}
		set = ps
		pc.variants[key] = set
	}
	p := set[name]
	if p == nil {
		return nil, false, nil
	}
	okKey := key + "/" + name
	ok, done := pc.ok[okKey]
	if !done {
		ok = sens(p)
		if ok {
			if err := p.Validate(); err != nil {
				return nil, false, fmt.Errorf("profile %s with a sensitivity value: %w", name, err)
			}
		}
		pc.ok[okKey] = ok
	}
	return p, ok, nil
}
