// Command simscenario validates, plans, runs and evaluates the scenarios of
// the simulated cluster harness (tests/simcluster/scenarios/<name>.yaml).
// scripts/simcluster/scenario.sh drives it around the cluster lifecycle:
//
//	simscenario validate <file> [-repo DIR]
//	simscenario plan <file> -out RUNDIR [-repo DIR] [-workloadgen BIN]
//	simscenario run -variant-dir DIR [-kubeconfig FILE]
//	simscenario evaluate DIR
//	simscenario summarize RUNDIR
//
// validate checks the scenario rules. plan writes the run directory:
// scenario.effective.yaml, and per variant its trace.jsonl and variant.env,
// and prints the offered load. run drives one variant on the cluster the
// kubeconfig names, through the API server's proxy only: snapshots,
// NodeTwin captures, the simulator's run API, Prometheus captures and the
// component logs. evaluate checks one variant's results and writes its
// summary.json; summarize writes the run's summary.json and the comparisons
// across variants.
//
// Exit codes: 0 success, 1 a failed assertion or comparison of severity
// fail (evaluate and summarize), 2 a usage error, an invalid scenario, or a
// run that could not be carried out.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

const usage = `usage:
  simscenario validate <file> [-repo DIR]
  simscenario plan <file> -out RUNDIR [-repo DIR] [-workloadgen BIN]
  simscenario run -variant-dir DIR [-kubeconfig FILE]
  simscenario evaluate DIR
  simscenario summarize RUNDIR
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the command: it returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return scenario.ExitError
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) int{
		"validate": cmdValidate, "plan": cmdPlan, "run": cmdRun, "evaluate": cmdEvaluate, "summarize": cmdSummarize,
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "simscenario: unknown command %q\n%s", args[0], usage)
		return scenario.ExitError
	}
	return cmd(ctx, args[1:], stdout, stderr)
}

// parse parses flags and positional arguments in any order, and checks
// the positional count.
func parse(fs *flag.FlagSet, args []string, positional int, stderr io.Writer) ([]string, bool) {
	fs.SetOutput(stderr)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, false
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != positional {
		fmt.Fprintf(stderr, "simscenario %s: %d arguments, want %d\n%s", fs.Name(), len(pos), positional, usage)
		return nil, false
	}
	return pos, true
}

func fail(stderr io.Writer, cmd string, err error) int {
	fmt.Fprintf(stderr, "simscenario %s: %v\n", cmd, err)
	return scenario.ExitError
}

// readValid reads and validates a scenario file.
func readValid(file, repo string) (scenario.Scenario, error) {
	s, err := scenario.ReadScenario(file)
	if err != nil {
		return s, err
	}
	return s, scenario.Validate(s, scenario.ValidateOptions{File: file, RepoRoot: repo})
}

func cmdValidate(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root the scenario's paths are relative to")
	pos, ok := parse(fs, args, 1, stderr)
	if !ok {
		return scenario.ExitError
	}
	s, err := readValid(pos[0], *repo)
	if err != nil {
		return fail(stderr, "validate", err)
	}
	fmt.Fprintf(stdout, "scenario %s is valid: %d variants, %d assertions, %d comparisons\n",
		s.Name, len(s.Variants), len(s.Assertions), len(s.Compare))
	return scenario.ExitPass
}

func cmdPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root the scenario's paths are relative to")
	out := fs.String("out", "", "run directory to write (required)")
	wg := fs.String("workloadgen", "", "workloadgen binary, for a scenario that generates its trace")
	pos, ok := parse(fs, args, 1, stderr)
	if !ok {
		return scenario.ExitError
	}
	if *out == "" {
		fmt.Fprintf(stderr, "simscenario plan: -out is required\n%s", usage)
		return scenario.ExitError
	}
	s, err := readValid(pos[0], *repo)
	if err != nil {
		return fail(stderr, "plan", err)
	}
	git := scenario.ReadGit(*repo)
	p, err := scenario.MakePlan(ctx, s, scenario.PlanOptions{RepoRoot: *repo, RunDir: *out, Workloadgen: *wg, GitSHA: git.SHA, GitDirty: git.Dirty})
	if p != nil {
		printPlan(stdout, *out, p)
	}
	if err != nil {
		return fail(stderr, "plan", err)
	}
	return scenario.ExitPass
}

func printPlan(w io.Writer, runDir string, p *scenario.Plan) {
	fmt.Fprintf(w, "run directory %s: %d jobs due within %s", runDir, p.Jobs, p.Effective.Duration.Duration)
	if p.Dropped > 0 {
		fmt.Fprintf(w, " (%d later jobs dropped)", p.Dropped)
	}
	fmt.Fprintln(w)
	for _, v := range p.Variants {
		name := v.Variant
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(w, "variant %s: run id %s, joulie enabled %t, telemetry %s\n", name, v.RunID, v.JoulieEnabled, v.Telemetry)
	}
	if len(p.Load) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tOFFERED LOAD\tDEMAND\tCAPACITY")
	for _, l := range p.Load {
		fmt.Fprintf(tw, "%s\t%.3f\t%.4g\t%.4g\n", l.Resource, l.Load, l.Demand, l.Capacity)
	}
	tw.Flush()
}

func cmdEvaluate(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("evaluate", flag.ContinueOnError)
	pos, ok := parse(fs, args, 1, stderr)
	if !ok {
		return scenario.ExitError
	}
	dir := pos[0]
	rd, env, err := scenario.ReadRunData(dir)
	if err != nil {
		return writeAborted(stderr, dir, env, err)
	}
	end, err := scenario.ReadRunEnd(dir)
	if err != nil {
		return writeAborted(stderr, dir, env, err)
	}
	results := scenario.Evaluate(rd, env)
	sum := scenario.VariantSummary(rd, env, results)
	runErr := end.Err()
	if rd.Status == nil {
		runErr = fmt.Errorf("no %s: the run never started", scenario.RunStatusFile)
	}
	sum.Finish(runErr)
	h := scenario.Recorder{Dir: dir, ClusterName: env.ClusterName, RunID: env.RunID, Scenario: env.Scenario, Variant: env.Variant}
	skip := map[string]bool{}
	for _, a := range rd.Scenario.Assertions {
		if a.Type == scenario.AssertPromQL {
			skip[a.Name] = true
		}
	}
	now := time.Now()
	recs, values := scenario.ResultRecords(results, now, skip)
	recs = append(recs, h.ScenarioEvent(semconv.EventScenarioFinished, now, otlpjson.String(semconv.AttrTestSuiteRunStatus, sum.Status)))
	if err := h.WriteLogs(recs...); err != nil {
		return fail(stderr, "evaluate", err)
	}
	if values != nil {
		if err := h.WriteMetrics(*values); err != nil {
			return fail(stderr, "evaluate", err)
		}
	}
	if err := scenario.WriteSummary(filepath.Join(dir, scenario.SummaryFile), sum); err != nil {
		return fail(stderr, "evaluate", err)
	}
	printChecks(stdout, sum)
	return sum.ExitCode
}

// writeAborted writes the summary of a variant whose results could not be
// read, so the run directory still says what happened.
func writeAborted(stderr io.Writer, dir string, env scenario.VariantEnv, err error) int {
	sum := scenario.NewSummary(scenario.SuiteName(env.Scenario, env.Variant))
	sum.Scenario, sum.Variant, sum.RunID = env.Scenario, env.Variant, env.RunID
	sum.Finish(err)
	if werr := scenario.WriteSummary(filepath.Join(dir, scenario.SummaryFile), sum); werr != nil {
		fmt.Fprintf(stderr, "simscenario: %v\n", werr)
	}
	return fail(stderr, "evaluate", err)
}

func printChecks(w io.Writer, s scenario.Summary) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tSEVERITY\tOBSERVED\tEXPECTED")
	for _, a := range s.Assertions {
		got := scenario.NoData
		if a.Observed != nil {
			got = fmt.Sprintf("%.4g", *a.Observed)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.Status, a.Severity, got, a.Expected)
	}
	for _, c := range s.Comparisons {
		got := scenario.NoData
		if c.Observed != nil {
			got = fmt.Sprintf("%.4g", *c.Observed)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.Status, c.Severity, got, c.Expected)
	}
	tw.Flush()
	if len(s.Classes) > 0 {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CLASS\tJOBS\tCOMPLETED\tJOBS/H\tENERGY J\tJ/JOB\tSLOWDOWN P50\tDURATION P95 S")
		classes := make([]string, 0, len(s.Classes))
		for c := range s.Classes {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for _, name := range classes {
			c := s.Classes[name]
			fmt.Fprintf(tw, "%s\t%d\t%d\t%.1f\t%.4g\t%.4g\t%.3f\t%.1f\n", name, c.Jobs, c.Completed, c.ThroughputJobsPerHour,
				c.EnergyJoules.Total, c.EnergyPerJobJoules, c.Slowdown.P50, c.Duration.P95)
		}
		tw.Flush()
	}
	fmt.Fprintf(w, "%s: %s (exit %d)\n", s.SuiteName, s.Status, s.ExitCode)
}

func cmdSummarize(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	pos, ok := parse(fs, args, 1, stderr)
	if !ok {
		return scenario.ExitError
	}
	runDir := pos[0]
	s, _, err := scenario.ReadEffectiveScenario(runDir)
	if err != nil {
		return fail(stderr, "summarize", err)
	}
	if len(s.Variants) == 0 {
		// The run directory is the variant directory: evaluate wrote the
		// one summary.
		sum, err := scenario.ReadSummary(filepath.Join(runDir, scenario.SummaryFile))
		if err != nil {
			return fail(stderr, "summarize", err)
		}
		printChecks(stdout, *sum)
		return sum.ExitCode
	}
	variants := map[string]*scenario.Summary{}
	for _, name := range s.RunVariants() {
		sum, err := scenario.ReadSummary(filepath.Join(runDir, name, scenario.SummaryFile))
		if err != nil {
			fmt.Fprintf(stderr, "simscenario summarize: variant %s: %v\n", name, err)
			continue
		}
		variants[name] = sum
	}
	top := scenario.TopSummary(s, variants)
	top.Finish(nil)
	if err := scenario.WriteSummary(filepath.Join(runDir, scenario.SummaryFile), top); err != nil {
		return fail(stderr, "summarize", err)
	}
	for _, v := range top.Variants {
		fmt.Fprintf(stdout, "variant %s: %s (exit %d)\n", v.Name, v.Status, v.ExitCode)
	}
	printChecks(stdout, top)
	return top.ExitCode
}

// restConfig loads the kubeconfig: the flag, else KUBECONFIG, else the
// default loading rules.
func restConfig(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("variant-dir", "", "variant directory plan wrote (required)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig of the harness cluster (default KUBECONFIG)")
	if _, ok := parse(fs, args, 0, stderr); !ok {
		return scenario.ExitError
	}
	if *dir == "" {
		fmt.Fprintf(stderr, "simscenario run: -variant-dir is required\n%s", usage)
		return scenario.ExitError
	}
	c, err := connect(*kubeconfig)
	if err != nil {
		return fail(stderr, "run", err)
	}
	if err := runVariant(ctx, *dir, c, stdout, stderr); err != nil {
		return fail(stderr, "run", err)
	}
	return scenario.ExitPass
}

// cluster is how run reaches the harness cluster, all through the API
// server: typed reads, pod logs, and Prometheus and the simulator through
// the service proxy.
type cluster struct {
	kube client.Reader
	logs kubernetes.Interface
	prom *scenario.PromClient
	sim  *scenario.SimClient
}

// connect builds the clients from the kubeconfig.
func connect(kubeconfig string) (*cluster, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	c := &cluster{}
	if c.kube, err = scenario.NewKubeClient(cfg); err != nil {
		return nil, err
	}
	if c.logs, err = kubernetes.NewForConfig(cfg); err != nil {
		return nil, err
	}
	if c.prom, err = scenario.NewProxyPromClient(cfg); err != nil {
		return nil, err
	}
	if c.sim, err = scenario.NewProxySimClient(cfg); err != nil {
		return nil, err
	}
	return c, nil
}

// runVariant runs one variant on the cluster: preflight, start snapshot,
// the run request, a capture loop until the run finishes, its deadline
// (duration plus drain) or a run stall longer than abortOnRunStalled, the
// end snapshot, finish, the Prometheus captures and promql assertions, and
// the component logs. The simulator's own files stay in the simulator; the
// harness copies them out.
func runVariant(ctx context.Context, dir string, c *cluster, stdout, stderr io.Writer) error {
	env, err := scenario.ReadVariantEnv(dir)
	if err != nil {
		return err
	}
	eff, _, err := scenario.ReadEffectiveScenario(dir)
	if err != nil {
		return err
	}
	s, err := eff.ForVariant(env.Variant)
	if err != nil {
		return err
	}
	kc, sim := c.kube, c.sim
	stage, err := scenario.GetPodCompleteStage(ctx, kc)
	if err != nil {
		return err
	}
	if err := scenario.CheckPodCompleteStage(stage); err != nil {
		return err
	}
	h := scenario.Recorder{Dir: dir, ClusterName: env.ClusterName, RunID: env.RunID, Scenario: env.Scenario, Variant: env.Variant}
	start := time.Now()
	var recs []otlpjson.LogRecord
	if names := eff.RunVariants(); names[0] == env.Variant {
		recs = append(recs, h.ScenarioEvent(semconv.EventScenarioStarted, start))
	}
	if env.Variant != "" {
		recs = append(recs, h.ScenarioEvent(semconv.EventScenarioVariantStarted, start))
	}
	if err := h.WriteLogs(recs...); err != nil {
		return err
	}
	snap, err := scenario.TakeKubeSnapshot(ctx, kc, scenario.SnapshotStart)
	if err != nil {
		return err
	}
	if err := scenario.WriteKubeSnapshot(dir, scenario.SnapshotStart, snap); err != nil {
		return err
	}
	started, err := sim.Start(ctx, scenario.RunRequestFor(s, env))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "run %s started: %d jobs in %d groups\n", started.RunID, started.Jobs, started.Groups)
	status, reason, loopErr := captureLoop(ctx, s, h, kc, sim, start, stdout, stderr)
	// The end snapshot shows the cluster as the run left it, before finish
	// deletes the run's Job objects.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if snap, err := scenario.TakeKubeSnapshot(endCtx, kc, scenario.SnapshotEnd); err != nil {
		fmt.Fprintf(stderr, "simscenario run: end snapshot: %v\n", err)
	} else if err := scenario.WriteKubeSnapshot(dir, scenario.SnapshotEnd, snap); err != nil {
		return err
	}
	if status.Phase == workload.PhaseRunning || status.Phase == "" {
		if status, err = sim.Finish(endCtx, true); err != nil {
			return errors.Join(loopErr, err)
		}
	}
	if err := writeJSON(filepath.Join(dir, scenario.RunStatusFile), status); err != nil {
		return err
	}
	end := time.Now()
	if reason == scenario.RunEndDeadline && status.Phase == workload.PhaseFinished {
		reason = scenario.RunEndFinished
	}
	if err := writeJSON(filepath.Join(dir, scenario.RunEndFile), scenario.RunEnd{Reason: reason, At: end}); err != nil {
		return err
	}
	recordPrometheus(endCtx, s, h, c.prom, start, end, stderr)
	if err := scenario.CollectLogs(endCtx, c.logs, dir); err != nil {
		fmt.Fprintf(stderr, "simscenario run: logs: %v\n", err)
	}
	fmt.Fprintf(stdout, "run %s %s after %.0fs: %d completed, %d failed, %d incomplete, %d not due\n", status.RunID, status.Phase,
		status.ElapsedSeconds, status.Jobs.Completed, status.Jobs.Failed, status.Jobs.Incomplete, status.Jobs.NotDue)
	return loopErr
}

// captureLoop captures the NodeTwins every capture interval and polls the
// run until it ends, its deadline passes, it stays stalled for
// abortOnRunStalled, or ctx ends. It returns the last status and the
// scenario.RunEnd reason.
func captureLoop(ctx context.Context, s scenario.Scenario, h scenario.Recorder, kc client.Reader, sim *scenario.SimClient,
	start time.Time, stdout, stderr io.Writer) (workload.RunStatus, string, error) {
	interval := scenario.DefaultCaptureInterval
	if s.Capture.Interval != nil {
		interval = s.Capture.Interval.Duration
	}
	deadline := start.Add(s.Duration.Duration)
	if s.Drain != nil {
		deadline = deadline.Add(s.Drain.Duration)
	}
	var status workload.RunStatus
	var stalledSince time.Time
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		now := time.Now()
		var twins v1alpha1.NodeTwinList
		if err := kc.List(ctx, &twins); err != nil {
			fmt.Fprintf(stderr, "simscenario run: nodetwins: %v\n", err)
		} else if err := h.WriteMetrics(scenario.CaptureNodeTwins(twins.Items, now, start)...); err != nil {
			return status, scenario.RunEndInterrupted, err
		}
		st, err := sim.Current(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "simscenario run: status: %v\n", err)
		} else {
			status = st
		}
		switch {
		case status.Phase == workload.PhaseFinished || status.Phase == workload.PhaseAborted:
			return status, scenario.RunEndFinished, nil
		case !now.Before(deadline):
			fmt.Fprintf(stdout, "run deadline %s reached\n", deadline.Sub(start))
			return status, scenario.RunEndDeadline, nil
		}
		if status.RunStalled {
			if stalledSince.IsZero() {
				stalledSince = now
			}
			if a := s.AbortOnRunStalled; a != nil && now.Sub(stalledSince) >= a.Duration {
				fmt.Fprintf(stdout, "run stalled for %s, aborting\n", now.Sub(stalledSince))
				return status, scenario.RunEndStalled, nil
			}
		} else {
			stalledSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return status, scenario.RunEndInterrupted, ctx.Err()
		case <-tick.C:
		}
	}
}

// recordPrometheus records the scenario's Prometheus captures over the run
// and the end value of each promql assertion. A query that fails is a
// warning: the assertions that need it report no data.
func recordPrometheus(ctx context.Context, s scenario.Scenario, h scenario.Recorder, prom *scenario.PromClient, start, end time.Time, stderr io.Writer) {
	step := scenario.DefaultCaptureInterval
	if s.Capture.Interval != nil {
		step = s.Capture.Interval.Duration
	}
	for _, c := range s.Capture.Prometheus {
		r, err := prom.QueryRange(ctx, c.Query, start, end, step)
		if err != nil {
			fmt.Fprintf(stderr, "simscenario run: capture %s: %v\n", c.Name, err)
			continue
		}
		if err := h.WriteMetrics(scenario.RangeToGauge(c.Name, c.Unit, r)); err != nil {
			fmt.Fprintf(stderr, "simscenario run: capture %s: %v\n", c.Name, err)
		}
	}
	for _, a := range s.Assertions {
		if a.PromQL == nil {
			continue
		}
		r, err := prom.Query(ctx, a.PromQL.Query, end)
		if err != nil {
			fmt.Fprintf(stderr, "simscenario run: assertion %s: %v\n", a.Name, err)
			continue
		}
		if len(r.Series) != 1 || len(r.Series[0].Samples) != 1 {
			fmt.Fprintf(stderr, "simscenario run: assertion %s: %d series, want one sample\n", a.Name, len(r.Series))
			continue
		}
		v := r.Series[0].Samples[0].Value
		m := semconv.JoulieScenarioAssertionValue
		err = h.WriteMetrics(otlpjson.Metric{Name: m.Name, Unit: m.Unit, Gauge: &otlpjson.Gauge{DataPoints: []otlpjson.NumberDataPoint{{
			Attributes: []otlpjson.KeyValue{otlpjson.String(semconv.AttrJoulieAssertionName, a.Name),
				otlpjson.String(semconv.AttrJoulieAssertionSource, semconv.AssertionSourceObserved)},
			TimeUnixNano: otlpjson.Uint64String(end.UnixNano()), AsDouble: &v,
		}}}})
		if err != nil {
			fmt.Fprintf(stderr, "simscenario run: assertion %s: %v\n", a.Name, err)
		}
	}
}

// writeJSON writes v as indented JSON to path.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
