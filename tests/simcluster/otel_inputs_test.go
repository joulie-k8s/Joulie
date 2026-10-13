//go:build simcluster

package simcluster_test

// The input checks of the end-to-end test: what Joulie's controller manager
// reads through its default input presets equals what the simulator
// emulated.
//
//   - E07: every check query of the telemetry mode (simcluster.Telemetry)
//     equals the truth within 3 % (1 W floor). E07c, in mode kepler-legacy:
//     the Kepler 0.7 platform series are two per Node and sum to the host
//     power.
//   - E08: every managed Node the mode measures has a NodeTwin that reads its
//     power from Prometheus (source prometheus) within max(2 W, 2 %) of the
//     truth under the configured scope; a Node nothing measures, such as a
//     VM, reads prometheus-no-data. In mode otel-labels-unpromoted every
//     Node reads prometheus-no-data and the controller manager logs the
//     promotion hint.
//   - E16: the controller manager's facility gauges equal the facility
//     truth, and the cooling stress of one CPU-only NodeTwin is the twin's
//     formula of its measured power, its TDP and the temperature its Node
//     breathes.
//
// The truth comes from the simulator's /debug/emulate snapshots, which a
// recorder samples in the background twice a second for as long as the
// checks run. rate() over a window W ending at t reads
// (E(t) - E(t-W)) / W, so a truth needs a sample near both ends of the
// window a check reads, not only the power at the end.
//
// e2e_test.go calls e2eInputChecks through e2eInputHook; the other
// package-level names of this file carry the prefix input.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/controller/twin"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
)

func init() { e2eInputHook = e2eInputChecks }

// e2eInputChecks runs the input checks E07, E07c, E08 and E16 against the
// cluster under test, with the truth recorder running for all of them.
func e2eInputChecks(t *testing.T, env *e2eEnv) {
	t.Helper()
	rec := startInputRecorder(env.snapshot, inputSampleInterval, inputKeep(env.Timing))
	defer rec.stop()
	env.check(t, "E07", "every check query of the mode equals the emulated truth within 3 % (1 W floor)",
		func(c *e2eCase) { inputE07(c, rec, false) })
	env.check(t, "E07c", "Kepler 0.7 platform energy: two series per Node, summing to the host power",
		func(c *e2eCase) { inputE07(c, rec, true) })
	env.check(t, "E08", "each NodeTwin's measured power is the emulated truth, or prometheus-no-data where nothing measures",
		func(c *e2eCase) { inputE08(c, rec) })
	env.check(t, "E16", "the facility gauges equal the facility truth, and the cooling stress follows the inlet",
		func(c *e2eCase) { inputE16(c, rec) })
}

// The truth recorder's settings: how often it reads /debug/emulate (the
// emulator ticks every second, chart value emulate.tick) and how far a
// sample may be from the time a truth needs.
const (
	inputSampleInterval = 500 * time.Millisecond
	inputSampleSlack    = 3 * time.Second
)

// Tolerances of E08 and E16.
const (
	inputTwinTolerance   = 0.02 // E08: the NodeTwin's measured power, relative
	inputTwinFloorW      = 2.0  // E08: and at least 2 W
	inputAmbientTolC     = 0.1  // E16: the facility ambient gauge
	inputITTolerance     = 0.02 // E16: the facility IT power gauge, relative
	inputStressTolPoints = 2.0  // E16: the cooling stress score
)

// The controller manager's metrics Service (charts/joulie, port name
// metrics), its pods' app label, and the facility gauges it serves
// (cmd/controller-manager/input_metrics.go).
const (
	inputControllerMetrics = "joulie-controller-manager-metrics:metrics"
	inputControllerApp     = "joulie-controller-manager"
	inputAmbientGauge      = "joulie_twin_facility_ambient_temperature_celsius"
	inputITGauge           = "joulie_twin_facility_it_power_watts"
)

// inputPromotionHint is what the controller manager's hint names when the
// node power series lack the node label: Prometheus' setting that promotes
// k8s.node.name (cmd/controller-manager/nodepower.go).
const inputPromotionHint = "otlp.promote_resource_attributes"

// inputPollLag is how old a facility reading of the controller manager may
// be: the poll interval, plus the export interval and a scrape interval
// before the sample the poll read reached Prometheus, plus the recorder's
// slack.
func inputPollLag(t simcluster.Timing) time.Duration {
	return t.FacilityPoll + t.OTLPExportInterval + t.ScrapeInterval + inputSampleSlack
}

// inputKeep is how much history the recorder keeps: two rate windows for
// the steadiness of E07, and a window plus the poll lag for E16.
func inputKeep(t simcluster.Timing) time.Duration {
	return 2*t.RateWindow + inputPollLag(t) + time.Minute
}

// inputRecorder samples the simulator's truth in the background and keeps
// one snapshot per emulator tick, by time, for keep.
type inputRecorder struct {
	keep       time.Duration
	quit, done chan struct{}

	mu      sync.Mutex
	samples []*emulate.Snapshot
	fails   int
	lastErr error
}

// startInputRecorder reads a snapshot with get every interval until stop.
func startInputRecorder(get func() (*emulate.Snapshot, error), every, keep time.Duration) *inputRecorder {
	r := &inputRecorder{keep: keep, quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			s, err := get()
			r.add(s, err)
			select {
			case <-r.quit:
				return
			case <-tick.C:
			}
		}
	}()
	return r
}

// stop ends the sampling and waits for the last read.
func (r *inputRecorder) stop() {
	close(r.quit)
	<-r.done
}

// add records one read. A snapshot no newer than the last one is the same
// tick read again and is dropped; samples older than keep go.
func (r *inputRecorder) add(s *emulate.Snapshot, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && s == nil {
		err = errors.New("an empty snapshot")
	}
	if err != nil {
		r.fails++
		r.lastErr = err
		return
	}
	if n := len(r.samples); n > 0 && !s.Time.After(r.samples[n-1].Time) {
		return
	}
	r.samples = append(r.samples, s)
	cut := s.Time.Add(-r.keep)
	if i := sort.Search(len(r.samples), func(i int) bool { return !r.samples[i].Time.Before(cut) }); i > 0 {
		r.samples = append([]*emulate.Snapshot(nil), r.samples[i:]...)
	}
}

// describeLocked says what the recorder holds, for error messages.
func (r *inputRecorder) describeLocked() string {
	out := fmt.Sprintf("%d samples", len(r.samples))
	if n := len(r.samples); n > 0 {
		out += fmt.Sprintf(" from %s to %s", r.samples[0].Time.Format(time.RFC3339), r.samples[n-1].Time.Format(time.RFC3339))
	}
	if r.fails > 0 {
		out += fmt.Sprintf(", %d failed reads (last: %v)", r.fails, r.lastErr)
	}
	return out
}

// latest returns the newest sample.
func (r *inputRecorder) latest() (*emulate.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return nil, fmt.Errorf("no /debug/emulate sample yet (%s)", r.describeLocked())
	}
	return r.samples[len(r.samples)-1], nil
}

// at returns the sample nearest to t, which must be within the slack.
func (r *inputRecorder) at(t time.Time) (*emulate.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.atLocked(t)
}

func (r *inputRecorder) atLocked(t time.Time) (*emulate.Snapshot, error) {
	i := sort.Search(len(r.samples), func(i int) bool { return !r.samples[i].Time.Before(t) })
	var best *emulate.Snapshot
	for _, j := range []int{i - 1, i} {
		if j >= 0 && j < len(r.samples) && (best == nil || inputAbs(r.samples[j].Time.Sub(t)) < inputAbs(best.Time.Sub(t))) {
			best = r.samples[j]
		}
	}
	if best == nil || inputAbs(best.Time.Sub(t)) > inputSampleSlack {
		return nil, fmt.Errorf("no /debug/emulate sample within %s of %s (%s)", inputSampleSlack, t.Format(time.RFC3339Nano), r.describeLocked())
	}
	return best, nil
}

func inputAbs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// truthOver returns the truth of every Node over the window w that ends at
// end, as rate() reads it there (simcluster.NodeTruthsOver), and the sample
// it ends on.
func (r *inputRecorder) truthOver(end time.Time, w time.Duration) (map[string]simcluster.NodeTruth, *emulate.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	to, err := r.atLocked(end)
	if err != nil {
		return nil, nil, err
	}
	from, err := r.atLocked(to.Time.Add(-w))
	if err != nil {
		return nil, nil, fmt.Errorf("the start of the %s window: %w", w, err)
	}
	truth, err := simcluster.NodeTruthsOver(from, to)
	return truth, to, err
}

// waitSpan waits, for at most within, until the samples cover d.
func (r *inputRecorder) waitSpan(d, within time.Duration) error {
	return e2eEventually(within, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if n := len(r.samples); n > 1 && r.samples[n-1].Time.Sub(r.samples[0].Time) >= d {
			return nil
		}
		return fmt.Errorf("the truth samples cover less than %s (%s)", d, r.describeLocked())
	})
}

// inputRange is the smallest and largest of some values.
type inputRange struct {
	lo, hi float64
	n      int
}

func (g *inputRange) add(v float64) {
	if g.n == 0 || v < g.lo {
		g.lo = v
	}
	if g.n == 0 || v > g.hi {
		g.hi = v
	}
	g.n++
}

// holds reports whether v lies within the range widened by abs, and by rel
// of each end.
func (g inputRange) holds(v, abs, rel float64) bool {
	return g.n > 0 && v >= g.lo-abs-rel*math.Abs(g.lo) && v <= g.hi+abs+rel*math.Abs(g.hi)
}

func (g inputRange) String() string {
	return fmt.Sprintf("%.3f to %.3f (%d samples)", g.lo, g.hi, g.n)
}

// facilityRange returns what a facility poll between from and to can have
// read: the range of the facility ambient, and of the IT power over the
// window w ending at each sample, (E(t) - E(t-w)) / w.
func (r *inputRecorder) facilityRange(from, to time.Time, w time.Duration) (ambient, it inputRange, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.samples {
		if s.Time.Before(from) || s.Time.After(to) {
			continue
		}
		ambient.add(s.Facility.AmbientC)
		f, err := r.atLocked(s.Time.Add(-w))
		if err != nil {
			continue
		}
		if dt := s.Time.Sub(f.Time).Seconds(); dt > 0 {
			it.add((s.Facility.ITEnergyJ - f.Facility.ITEnergyJ) / dt)
		}
	}
	switch {
	case ambient.n == 0:
		err = fmt.Errorf("no truth sample between %s and %s (%s)", from.Format(time.RFC3339), to.Format(time.RFC3339), r.describeLocked())
	case it.n == 0:
		err = fmt.Errorf("no truth sample a %s window before those between %s and %s (%s)", w, from.Format(time.RFC3339), to.Format(time.RFC3339), r.describeLocked())
	}
	return ambient, it, err
}

// unsteady lists the Nodes whose truth changed, beyond the check tolerance,
// between the two rate windows before end: E07 needs the load steady for
// two windows.
func (r *inputRecorder) unsteady(truth string, end time.Time, w time.Duration) []string {
	now, to, err := r.truthOver(end, w)
	if err != nil {
		return []string{fmt.Sprintf("steadiness unknown: %v", err)}
	}
	before, _, err := r.truthOver(to.Time.Add(-w), w)
	if err != nil {
		return []string{fmt.Sprintf("steadiness unknown: %v", err)}
	}
	steady := simcluster.Check{Tolerance: simcluster.CheckTolerance, AbsFloorW: simcluster.CheckAbsFloorW}
	var out []string
	for _, name := range e2eKeys(now) {
		b, ok := before[name]
		if !ok {
			continue
		}
		was, err1 := simcluster.EvalTruth(truth, b)
		is, err2 := simcluster.EvalTruth(truth, now[name])
		if err := errors.Join(err1, err2); err != nil {
			return []string{fmt.Sprintf("steadiness unknown: %v", err)}
		}
		if !steady.Within(is, was) {
			out = append(out, fmt.Sprintf("node %s: %.1f W, then %.1f W", name, was, is))
		}
	}
	return out
}

// inputSteadyTruth is the truth whose steadiness E07 reports: Joulie's, or
// the host power where the mode leaves Joulie without data.
func inputSteadyTruth(e *e2eEnv) string {
	if t := e.Parts.Checks.JoulieTruth; t != "" {
		return t
	}
	return simcluster.TruthHost
}

// inputPlatformCheck reports whether ck checks the Kepler 0.7 platform
// series (E07c) rather than a power reading (E07).
func inputPlatformCheck(ck simcluster.Check) bool {
	return ck.Truth == simcluster.TruthKepler07PlatformSeries || ck.Truth == simcluster.TruthKepler07Host
}

// inputQuery runs query in the harness Prometheus at time at and returns
// each series' value by its label label.
func inputQuery(e *e2eEnv, query, label string, at time.Time) (map[string]float64, error) {
	b, err := e.proxyGet(scenario.NamespaceMonitoring, scenario.PrometheusService, "api/v1/query",
		map[string]string{"query": query, "time": strconv.FormatFloat(float64(at.UnixNano())/1e9, 'f', 3, 64)})
	if err != nil {
		return nil, err
	}
	r, err := scenario.ParsePromResponse(b, 200)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", query, err)
	}
	return inputByLabel(r, query, label)
}

// inputByLabel returns each series' last value by its label label. A series
// without the label or a value, or two series of one Node, is an error.
func inputByLabel(r scenario.PromResult, query, label string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, s := range r.Series {
		node := s.Labels[label]
		if node == "" || len(s.Samples) == 0 {
			return nil, fmt.Errorf("query %s: a series without %s or a value: %v", query, label, s.Labels)
		}
		if _, dup := out[node]; dup {
			return nil, fmt.Errorf("query %s: two series with %s=%s", query, label, node)
		}
		out[node] = s.Samples[len(s.Samples)-1].Value
	}
	return out, nil
}

// E07: once every Node's series is older than the rate window plus two
// export intervals, and the truth has been recorded for two windows, every
// check query of the mode, run in Prometheus at the time of the newest
// truth sample, equals the truth over the window ending there within the
// check's tolerance. With platform, E07c: only the Kepler 0.7 platform
// checks, which mode kepler-legacy alone has.
func inputE07(c *e2eCase, rec *inputRecorder, platform bool) {
	e := c.env
	var checks []simcluster.Check
	for _, ck := range e.Parts.Checks.Checks {
		if inputPlatformCheck(ck) == platform {
			checks = append(checks, ck)
		}
	}
	switch {
	case len(checks) == 0 && platform:
		c.Skipf("mode %s has no Kepler 0.7 platform series; E07c runs in mode %s", e.Mode, simcluster.ModeKeplerLegacy)
	case len(checks) == 0:
		c.Fatalf("mode %s has no check query", e.Mode)
	}
	w := e.Timing.RateWindow
	if err := e.waitSeriesAge(c.T); err != nil {
		c.Fatalf("%v", err)
	}
	if err := rec.waitSpan(2*w, 2*w+time.Minute); err != nil {
		c.Fatalf("%v", err)
	}
	var failures []string
	var end time.Time
	err := e2eEventually(e.twinFollow(), func() error {
		failures = failures[:0]
		latest, err := rec.latest()
		if err != nil {
			return err
		}
		end = latest.Time
		truth, _, err := rec.truthOver(end, w)
		if err != nil {
			return err
		}
		for _, ck := range checks {
			observed, err := inputQuery(e, ck.Query, e.Parts.Checks.NodeLabel, end)
			if err != nil {
				return err
			}
			if _, err := simcluster.Evaluate(ck, observed, truth); err != nil {
				failures = append(failures, err.Error())
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("%d of %d check queries are off the truth over %s", len(failures), len(checks), w)
		}
		return nil
	})
	if err != nil {
		for _, f := range failures {
			c.Errorf("%s", f)
		}
		c.Errorf("%v", err)
		if u := rec.unsteady(inputSteadyTruth(e), end, w); len(u) > 0 {
			c.Errorf("the load was not steady over the last two rate windows: %s", strings.Join(u, "; "))
		}
		return
	}
	c.Logf("%d check queries of mode %s equal the truth over %s", len(checks), e.Mode, w)
}

// inputTwinTime is the time of a NodeTwin's last update. lastUpdated is
// written in whole seconds, so the middle of that second is the closest
// guess.
func inputTwinTime(tw v1alpha1.NodeTwin) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, tw.Status.LastUpdated)
	if err != nil {
		return time.Time{}, fmt.Errorf("NodeTwin %s lastUpdated %q: %w", tw.Name, tw.Status.LastUpdated, err)
	}
	return t.Add(500 * time.Millisecond), nil
}

// E08: every managed Node's NodeTwin reads, at its last update, the mode's
// truth over the rate window ending then, under the configured scope:
// source prometheus within max(2 W, 2 %) where the truth is not zero,
// source prometheus-no-data where it is (a VM, which no collector reads, in
// either scope). Where the mode leaves Joulie without data
// (otel-labels-unpromoted), every NodeTwin reads prometheus-no-data and
// the controller manager's log names the Prometheus setting that would fix
// it. The check observes the largest deviation over its tolerance.
func inputE08(c *e2eCase, rec *inputRecorder) {
	e := c.env
	all, err := e.nodes()
	if err != nil {
		c.Fatalf("spec Nodes: %v", err)
	}
	var managed []e2eNode
	for _, n := range all {
		if n.Managed {
			managed = append(managed, n)
		}
	}
	if len(managed) == 0 {
		c.Skipf("the spec has no managed Node")
	}
	truthName, w := e.Parts.Checks.JoulieTruth, e.Timing.RateWindow
	if err := rec.waitSpan(w, w+time.Minute); err != nil {
		c.Fatalf("%v", err)
	}
	var problems []string
	var worst float64
	var compared int
	err = e2eEventually(e.twinFollow(), func() error {
		problems, worst, compared = problems[:0], 0, 0
		twins, err := e.twins()
		if err != nil {
			return err
		}
		for _, n := range managed {
			p, dev, ok := inputTwinProblem(rec, truthName, w, n.Name, twins)
			if p != "" {
				problems = append(problems, fmt.Sprintf("node %s: %s", n.Name, p))
			}
			if ok {
				compared++
				worst = math.Max(worst, dev)
			}
		}
		if truthName == "" {
			logs, err := inputControllerLogs(e)
			if err != nil {
				return err
			}
			if !strings.Contains(logs, inputPromotionHint) {
				problems = append(problems, fmt.Sprintf("the controller manager's log does not name %s", inputPromotionHint))
			}
		} else if compared == 0 {
			problems = append(problems, fmt.Sprintf("no managed Node has a non-zero truth (%s) to compare", truthName))
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if compared > 0 {
		c.observe(math.Round(worst*1000)/1000, "<= 1")
	}
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
}

// inputTwinProblem checks the NodeTwin of node against truth over w ending
// at its last update, and returns what is wrong ("" when nothing is). ok
// says whether its measured power was compared with a non-zero truth, and
// dev is then its deviation over the tolerance.
func inputTwinProblem(rec *inputRecorder, truth string, w time.Duration, node string, twins map[string]v1alpha1.NodeTwin) (problem string, dev float64, ok bool) {
	tw, found := twins[node]
	if !found {
		return "no NodeTwin", 0, false
	}
	pm := tw.Status.PowerMeasurement
	if pm == nil {
		return "the NodeTwin has no powerMeasurement", 0, false
	}
	if truth == "" {
		if pm.Source != joulie.PowerSourcePrometheusNoData {
			return fmt.Sprintf("source %s, want %s: Prometheus does not promote the node label the labels form needs", pm.Source, joulie.PowerSourcePrometheusNoData), 0, false
		}
		return "", 0, false
	}
	end, err := inputTwinTime(tw)
	if err != nil {
		return err.Error(), 0, false
	}
	truths, _, err := rec.truthOver(end, w)
	if err != nil {
		return fmt.Sprintf("no truth at the NodeTwin's last update: %v", err), 0, false
	}
	nt, emulated := truths[node]
	if !emulated {
		return "the simulator does not emulate it", 0, false
	}
	want, err := simcluster.EvalTruth(truth, nt)
	if err != nil {
		return err.Error(), 0, false
	}
	if want == 0 {
		if pm.Source != joulie.PowerSourcePrometheusNoData {
			return fmt.Sprintf("source %s, want %s: nothing measures it (%s is 0)", pm.Source, joulie.PowerSourcePrometheusNoData, truth), 0, false
		}
		return "", 0, false
	}
	tol := math.Max(inputTwinFloorW, inputTwinTolerance*want)
	dev = math.Abs(pm.MeasuredNodePowerW-want) / tol
	switch {
	case pm.Source != joulie.PowerSourcePrometheus:
		problem = fmt.Sprintf("source %s, want %s (%s %.1f W)", pm.Source, joulie.PowerSourcePrometheus, truth, want)
	case dev > 1:
		problem = fmt.Sprintf("measuredNodePowerW %.1f, want %.1f (%s over %s) within %.1f W", pm.MeasuredNodePowerW, want, truth, w, tol)
	}
	return problem, dev, true
}

// inputControllerLogs returns the logs of the controller manager's pods.
func inputControllerLogs(e *e2eEnv) (string, error) {
	pods, err := e.pods(scenario.NamespaceJoulie, client.MatchingLabels{"app.kubernetes.io/name": inputControllerApp})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, p := range pods {
		ctx, cancel := e.ctx()
		b, err := e.Clientset.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		cancel()
		if err != nil {
			return "", fmt.Errorf("logs of %s/%s: %w", p.Namespace, p.Name, err)
		}
		out.Write(b)
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("no controller manager pod with app.kubernetes.io/name=%s in %s", inputControllerApp, scenario.NamespaceJoulie)
	}
	return out.String(), nil
}

// inputSeriesValue returns the value of series, written as a Prometheus
// text exposition writes it (name{label="value"}), in text; ok is false
// when the exposition has no such sample.
func inputSeriesValue(text []byte, series string) (v float64, ok bool, err error) {
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), series)
		if !found || !strings.HasPrefix(rest, " ") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false, fmt.Errorf("%s: no value in %q", series, sc.Text())
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s: %q: %w", series, sc.Text(), err)
		}
		return v, true, nil
	}
	return 0, false, sc.Err()
}

// inputFacilitySeries is a facility gauge of the otel facility preset, the
// default in every mode.
func inputFacilitySeries(gauge string) string {
	return fmt.Sprintf("%s{preset=%q}", gauge, inputs.FacilityPresetOTel)
}

// E16: the controller manager's facility gauges hold what a poll within the
// poll lag read: the ambient within 0.1 °C of the facility ambient then,
// the IT power within 2 % of the IT power over the rate window ending then.
// And the cooling stress of one CPU-only NodeTwin is the twin's formula of
// the NodeTwin's measured power and TDP and of the inlet temperature its
// Node had (twin.go computeCoolingStress), within 2 points; that part is
// skipped, with the reason logged, when no such NodeTwin is below the
// clamp at 100.
func inputE16(c *e2eCase, rec *inputRecorder) {
	e := c.env
	w, lag := e.Timing.RateWindow, inputPollLag(e.Timing)
	if err := rec.waitSpan(w+lag, w+lag+time.Minute); err != nil {
		c.Fatalf("%v", err)
	}
	ambientSeries, itSeries := inputFacilitySeries(inputAmbientGauge), inputFacilitySeries(inputITGauge)
	var problems []string
	var skip string
	err := e2eEventually(e.twinFollow(), func() error {
		problems, skip = problems[:0], ""
		text, err := e.proxyGet(scenario.NamespaceJoulie, inputControllerMetrics, "metrics", nil)
		if err != nil {
			return err
		}
		now := time.Now()
		ambient, it, err := rec.facilityRange(now.Add(-lag), now, w)
		if err != nil {
			return err
		}
		for _, g := range []struct {
			series   string
			truth    inputRange
			abs, rel float64
		}{
			{ambientSeries, ambient, inputAmbientTolC, 0},
			{itSeries, it, 0, inputITTolerance},
		} {
			v, ok, err := inputSeriesValue(text, g.series)
			switch {
			case err != nil:
				return err
			case !ok:
				problems = append(problems, fmt.Sprintf("the controller manager serves no %s", g.series))
			case !g.truth.holds(v, g.abs, g.rel):
				problems = append(problems, fmt.Sprintf("%s is %.3f, the truth over the last %s was %s", g.series, v, lag, g.truth))
			}
		}
		var p string
		if p, skip, err = inputCoolingStress(e, rec); err != nil {
			return err
		} else if p != "" {
			problems = append(problems, p)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problems", len(problems))
		}
		return nil
	})
	if err != nil {
		for _, p := range problems {
			c.Errorf("%s", p)
		}
		c.Errorf("%v", err)
	}
	if skip != "" {
		e.warn(c.T, "E16 did not compare a cooling stress: %s", skip)
	}
}

// inputCoolingStress compares the cooling stress of one CPU-only NodeTwin
// (inputStressNode) with what the twin computes for it (inputStressProblem).
// skip says why no NodeTwin was compared.
func inputCoolingStress(e *e2eEnv, rec *inputRecorder) (problem, skip string, err error) {
	nodes, err := e.nodes()
	if err != nil {
		return "", "", err
	}
	twins, err := e.twins()
	if err != nil {
		return "", "", err
	}
	n, tw, skip := inputStressNode(nodes, twins)
	if skip != "" {
		return "", skip, nil
	}
	return inputStressProblem(rec, n.Name, tw), "", nil
}

// inputStressProblem checks the cooling stress of node's NodeTwin against
// inputExpectedStress of its measured power, its TDP and the Node's inlet
// temperature at its last update, which is the temperature the controller
// manager reads for it: the inlet, else the cooling zone's, else the
// facility ambient, each equal to the inlet the emulator reports. It
// returns what is wrong, or "".
func inputStressProblem(rec *inputRecorder, node string, tw v1alpha1.NodeTwin) string {
	pm := tw.Status.PowerMeasurement
	if pm == nil {
		return fmt.Sprintf("node %s: the NodeTwin has no powerMeasurement", node)
	}
	end, err := inputTwinTime(tw)
	if err != nil {
		return err.Error()
	}
	s, err := rec.at(end)
	if err != nil {
		return fmt.Sprintf("node %s: no truth at the NodeTwin's last update: %v", node, err)
	}
	sn, ok := e2eSnapshotNode(s, node)
	if !ok {
		return fmt.Sprintf("node %s: the simulator does not emulate it", node)
	}
	want := inputExpectedStress(pm.MeasuredNodePowerW, pm.NodeTdpW, sn.Host.InletC)
	if got := tw.Status.PredictedCoolingStressScore; math.Abs(got-want) > inputStressTolPoints {
		return fmt.Sprintf("node %s: predictedCoolingStressScore %.2f, want %.2f within %.0f points (measured %.1f W of %.1f W TDP at %.2f °C inlet)",
			node, got, want, inputStressTolPoints, pm.MeasuredNodePowerW, pm.NodeTdpW, sn.Host.InletC)
	}
	return ""
}

// inputStressNode returns the first managed CPU-only Node, by name, whose
// NodeTwin has a measured power and a TDP and a cooling stress below the
// clamp at 100, or why there is none.
func inputStressNode(nodes []e2eNode, twins map[string]v1alpha1.NodeTwin) (e2eNode, v1alpha1.NodeTwin, string) {
	clamped := 0
	for _, n := range nodes {
		if !n.Managed || n.Profile == nil || n.Profile.GPUs != nil {
			continue
		}
		tw, ok := twins[n.Name]
		if pm := tw.Status.PowerMeasurement; !ok || pm == nil || pm.MeasuredNodePowerW <= 0 || pm.NodeTdpW <= 0 {
			continue
		}
		if tw.Status.PredictedCoolingStressScore >= 100 {
			clamped++
			continue
		}
		return n, tw, ""
	}
	if clamped > 0 {
		return e2eNode{}, v1alpha1.NodeTwin{}, fmt.Sprintf("the cooling stress of every CPU-only NodeTwin with a measured power (%d) is clamped at 100", clamped)
	}
	return e2eNode{}, v1alpha1.NodeTwin{}, "no managed CPU-only Node has a NodeTwin with a measured power and a TDP"
}

// inputExpectedStress is the twin's cooling stress of a Node drawing
// measuredW of tdpW at ambientC (pkg/controller/twin computeCoolingStress):
// (measured / TDP) / max(0.5, 1 - (T - 20) * 0.02) * 100, clamped to 0..100.
func inputExpectedStress(measuredW, tdpW, ambientC float64) float64 {
	return math.Min(100, math.Max(0, measuredW/tdpW/math.Max(0.5, 1-(ambientC-20)*0.02)*100))
}

// TestInputChecksAreHooked: TestSimclusterE2E runs the input checks.
func TestInputChecksAreHooked(t *testing.T) {
	if e2eInputHook == nil {
		t.Fatal("e2eInputHook is nil: TestSimclusterE2E would skip E07, E08 and E16")
	}
}

// inputTestSnapshot is a snapshot at start+sec of node n drawing 100 W on
// cpu0, 50 W on gpu0 and 200 W at the host, of node s drawing 40 W on cpu0
// and 50 W at the host, of node vm, which nothing measures, and of a
// facility of 300 W IT power whose ambient rises 0.01 °C a second.
func inputTestSnapshot(start time.Time, sec float64) *emulate.Snapshot {
	return &emulate.Snapshot{
		Time: start.Add(time.Duration(sec * float64(time.Second))),
		Nodes: []emulate.NodeSnapshot{{
			Name: "n",
			Host: emulate.HostReading{Measured: true, PowerW: 200, EnergyJ: 200 * sec, InletC: 21},
			Components: []emulate.ComponentReading{
				{ID: "cpu0", Type: "cpu", Measured: true, PowerW: 100, EnergyJ: 100 * sec},
				{ID: "gpu0", Type: "gpu", Vendor: "nvidia", Measured: true, PowerW: 50, EnergyJ: 50 * sec},
			},
		}, {
			Name: "s",
			Host: emulate.HostReading{Measured: true, PowerW: 50, EnergyJ: 50 * sec, InletC: 21},
			Components: []emulate.ComponentReading{
				{ID: "cpu0", Type: "cpu", Measured: true, PowerW: 40, EnergyJ: 40 * sec},
			},
		}, {
			Name: "vm",
			Host: emulate.HostReading{InletC: 20},
		}},
		Facility: emulate.FacilitySnapshot{AmbientC: 20 + 0.01*sec, ITPowerW: 300, ITEnergyJ: 300 * sec},
	}
}

// inputStepSnapshot is inputTestSnapshot with a load step at 60 s: from
// then on node n's cpu0, its host and the facility's IT power each draw
// 60 W more. A power is the draw at that instant and an energy its
// integral, so the truth over a window across the step is not the power
// at its end.
func inputStepSnapshot(start time.Time, sec float64) *emulate.Snapshot {
	s := inputTestSnapshot(start, sec)
	if sec <= 60 {
		return s
	}
	extraJ := 60 * (sec - 60)
	n := &s.Nodes[0]
	n.Components[0].PowerW += 60
	n.Components[0].EnergyJ += extraJ
	n.Host.PowerW += 60
	n.Host.EnergyJ += extraJ
	s.Facility.ITPowerW += 60
	s.Facility.ITEnergyJ += extraJ
	return s
}

// TestInputRecorderTruthOverAWindow: the truth over a window comes from the
// samples nearest its two ends; a tick read twice is kept once; samples
// older than keep go; a time no sample is near is an error.
func TestInputRecorderTruthOverAWindow(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := &inputRecorder{keep: 60 * time.Second}
	for sec := 0; sec <= 90; sec++ {
		r.add(inputTestSnapshot(start, float64(sec)), nil)
		r.add(inputTestSnapshot(start, float64(sec)), nil)
	}
	r.add(nil, errors.New("proxy down"))
	if len(r.samples) != 61 || !r.samples[0].Time.Equal(start.Add(30*time.Second)) {
		t.Fatalf("kept %d samples from %s, want 61 from %s", len(r.samples), r.samples[0].Time, start.Add(30*time.Second))
	}
	truth, to, err := r.truthOver(start.Add(80*time.Second+400*time.Millisecond), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !to.Time.Equal(start.Add(80 * time.Second)) {
		t.Errorf("the window ends on %s, want the nearest sample %s", to.Time, start.Add(80*time.Second))
	}
	n := truth["n"]
	if len(n.PackagesW) != 1 || math.Abs(n.PackagesW[0]-100) > 1e-9 || len(n.GPUsW) != 1 || math.Abs(n.GPUsW[0]-50) > 1e-9 || math.Abs(n.HostW-200) > 1e-9 {
		t.Errorf("truth %+v, want packages [100], gpus [50], host 200", n)
	}
	if _, _, err := r.truthOver(start.Add(70*time.Second), 45*time.Second); err == nil || !strings.Contains(err.Error(), "proxy down") {
		t.Errorf("a window starting before the first sample: err %v, want one that names the failed read", err)
	}
	if _, err := r.at(start.Add(95 * time.Second)); err == nil {
		t.Error("5 s past the last sample: want an error")
	}
	if u := r.unsteady(simcluster.TruthPackagesGPUs, start.Add(90*time.Second), 30*time.Second); len(u) != 0 {
		t.Errorf("a steady load reads unsteady: %v", u)
	}
}

// TestInputFacilityRange: a poll between from and to read an ambient of one
// of those samples and an IT power over the window ending at one of them.
func TestInputFacilityRange(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := &inputRecorder{keep: time.Hour}
	for sec := 0; sec <= 60; sec++ {
		r.add(inputTestSnapshot(start, float64(sec)), nil)
	}
	ambient, it, err := r.facilityRange(start.Add(40*time.Second), start.Add(50*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ambient.n != 11 || math.Abs(ambient.lo-20.4) > 1e-9 || math.Abs(ambient.hi-20.5) > 1e-9 {
		t.Errorf("ambient %s, want 20.400 to 20.500 over 11 samples", ambient)
	}
	if math.Abs(it.lo-300) > 1e-9 || math.Abs(it.hi-300) > 1e-9 {
		t.Errorf("IT power %s, want 300", it)
	}
	if !ambient.holds(20.55, 0.1, 0) || ambient.holds(20.65, 0.1, 0) || !it.holds(305, 0, 0.02) || it.holds(307, 0, 0.02) {
		t.Errorf("tolerances: ambient %s, IT %s", ambient, it)
	}
	if _, _, err := r.facilityRange(start.Add(10*time.Second), start.Add(20*time.Second), 30*time.Second); err == nil {
		t.Error("no sample a window before: want an error")
	}
}

// TestInputSeriesValue reads one series of a text exposition.
func TestInputSeriesValue(t *testing.T) {
	text := []byte(`# HELP joulie_twin_facility_ambient_temperature_celsius Outdoor ambient.
# TYPE joulie_twin_facility_ambient_temperature_celsius gauge
joulie_twin_facility_ambient_temperature_celsius{preset="legacy"} 30
joulie_twin_facility_ambient_temperature_celsius{preset="otel"} 21.25
joulie_twin_facility_it_power_watts_extra{preset="otel"} 5
joulie_twin_facility_it_power_watts{preset="otel"} 1.5e+03
`)
	for series, want := range map[string]float64{
		inputFacilitySeries(inputAmbientGauge): 21.25,
		inputFacilitySeries(inputITGauge):      1500,
	} {
		v, ok, err := inputSeriesValue(text, series)
		if err != nil || !ok || v != want {
			t.Errorf("%s: %v %v %v, want %v", series, v, ok, err, want)
		}
	}
	if _, ok, err := inputSeriesValue(text, `joulie_twin_facility_cooling_power_watts{preset="otel"}`); ok || err != nil {
		t.Errorf("an absent series: ok %v err %v, want not ok and no error", ok, err)
	}
	if _, _, err := inputSeriesValue([]byte("x{preset=\"otel\"} warm\n"), `x{preset="otel"}`); err == nil {
		t.Error("a value that is not a number: want an error")
	}
}

// TestInputByLabel: series by their node label; a missing label or two
// series of one node are errors.
func TestInputByLabel(t *testing.T) {
	s := func(node string, v float64) scenario.PromSeries {
		return scenario.PromSeries{Labels: map[string]string{"k8s_node_name": node}, Samples: []scenario.PromSample{{Value: v}}}
	}
	got, err := inputByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("a", 1), s("b", 2)}}, "q", "k8s_node_name")
	if err != nil || len(got) != 2 || got["a"] != 1 || got["b"] != 2 {
		t.Errorf("got %v %v", got, err)
	}
	if _, err := inputByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("a", 1), s("a", 2)}}, "q", "k8s_node_name"); err == nil {
		t.Error("two series of one node: want an error")
	}
	if _, err := inputByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("", 1)}}, "q", "k8s_node_name"); err == nil {
		t.Error("a series without the label: want an error")
	}
}

// TestInputTwinProblem: E08 on one NodeTwin, against the truth over the
// window ending at its last update, in each scope and where the mode
// leaves Joulie without data.
func TestInputTwinProblem(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := &inputRecorder{keep: time.Hour}
	for sec := 0; sec <= 90; sec++ {
		r.add(inputTestSnapshot(start, float64(sec)), nil)
	}
	mk := func(source string, w float64) v1alpha1.NodeTwin {
		var tw v1alpha1.NodeTwin
		tw.Status.LastUpdated = start.Add(80 * time.Second).Format(time.RFC3339)
		tw.Status.PowerMeasurement = &v1alpha1.PowerMeasurement{Source: source, MeasuredNodePowerW: w}
		return tw
	}
	for _, tc := range []struct {
		name, truth, node string
		tw                v1alpha1.NodeTwin
		problem           string
		compared          bool
	}{
		{"components within 2 W", simcluster.TruthPackagesGPUs, "n", mk(joulie.PowerSourcePrometheus, 152), "", true},
		{"components off by 4 W", simcluster.TruthPackagesGPUs, "n", mk(joulie.PowerSourcePrometheus, 154), "measuredNodePowerW 154.0, want 150.0", true},
		{"host within 2 %", simcluster.TruthHost, "n", mk(joulie.PowerSourcePrometheus, 196.5), "", true},
		{"a small Node within the 2 W floor", simcluster.TruthPackagesGPUs, "s", mk(joulie.PowerSourcePrometheus, 41.5), "", true},
		{"a small Node beyond the 2 W floor", simcluster.TruthPackagesGPUs, "s", mk(joulie.PowerSourcePrometheus, 42.5), "measuredNodePowerW 42.5, want 40.0", true},
		{"another source", simcluster.TruthPackagesGPUs, "n", mk(joulie.PowerSourceStatic, 150), "source static, want prometheus", true},
		{"a VM reads no data", simcluster.TruthHost, "vm", mk(joulie.PowerSourcePrometheusNoData, 0), "", false},
		{"a VM reads data", simcluster.TruthPackagesGPUs, "vm", mk(joulie.PowerSourcePrometheus, 0), "nothing measures it", false},
		{"no data expected, data read", "", "n", mk(joulie.PowerSourcePrometheus, 150), "does not promote", false},
		{"no data expected, none read", "", "n", mk(joulie.PowerSourcePrometheusNoData, 0), "", false},
	} {
		p, _, ok := inputTwinProblem(r, tc.truth, 30*time.Second, tc.node, map[string]v1alpha1.NodeTwin{tc.node: tc.tw})
		if ok != tc.compared || (tc.problem == "") != (p == "") || !strings.Contains(p, tc.problem) {
			t.Errorf("%s: problem %q compared %v, want a problem with %q compared %v", tc.name, p, ok, tc.problem, tc.compared)
		}
	}
	if p, _, _ := inputTwinProblem(r, simcluster.TruthHost, 30*time.Second, "n", nil); p != "no NodeTwin" {
		t.Errorf("a missing NodeTwin: problem %q", p)
	}
}

// TestInputTruthIsTheEnergyOverTheWindow: across a load step, each truth
// is the energy added over the window divided by its length, as rate()
// reads it, not the power drawn at its end: a Node's truth, the facility
// IT power, the power a NodeTwin last updated then is held to, and the
// steadiness E07 reports.
func TestInputTruthIsTheEnergyOverTheWindow(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return start.Add(time.Duration(sec) * time.Second) }
	w := 30 * time.Second
	r := &inputRecorder{keep: time.Hour}
	for sec := 0; sec <= 120; sec++ {
		r.add(inputStepSnapshot(start, float64(sec)), nil)
	}
	// From 50 s to 80 s, cpu0 drew 100 W for 10 s and 160 W for 20 s, so
	// 140 W, and the host 240 W, though at 80 s they draw 160 W and 260 W.
	truth, _, err := r.truthOver(at(80), w)
	if err != nil {
		t.Fatal(err)
	}
	if n := truth["n"]; len(n.PackagesW) != 1 || math.Abs(n.PackagesW[0]-140) > 1e-9 || math.Abs(n.HostW-240) > 1e-9 {
		t.Errorf("truth of n over 50 s to 80 s %+v, want packages [140] and host 240", n)
	}
	if _, it, err := r.facilityRange(at(80), at(80), w); err != nil || it.n != 1 || math.Abs(it.lo-340) > 1e-9 {
		t.Errorf("IT power over 50 s to 80 s %s (err %v), want 340", it, err)
	}
	twin := func(measuredW float64) map[string]v1alpha1.NodeTwin {
		var tw v1alpha1.NodeTwin
		tw.Status.LastUpdated = at(80).Format(time.RFC3339)
		tw.Status.PowerMeasurement = &v1alpha1.PowerMeasurement{Source: joulie.PowerSourcePrometheus, MeasuredNodePowerW: measuredW}
		return map[string]v1alpha1.NodeTwin{"n": tw}
	}
	if p, _, ok := inputTwinProblem(r, simcluster.TruthPackagesGPUs, w, "n", twin(190)); p != "" || !ok {
		t.Errorf("a NodeTwin at 190 W, the truth over the window ending at its last update: problem %q compared %v", p, ok)
	}
	if p, _, _ := inputTwinProblem(r, simcluster.TruthPackagesGPUs, w, "n", twin(210)); !strings.Contains(p, "measuredNodePowerW 210.0") {
		t.Errorf("a NodeTwin at 210 W, the power drawn at its last update: problem %q, want one", p)
	}
	if u := r.unsteady(simcluster.TruthPackagesGPUs, at(90), w); len(u) != 1 || !strings.Contains(u[0], "node n: 150.0 W, then 210.0 W") {
		t.Errorf("the windows on either side of the step: unsteady %q, want node n alone", u)
	}
	for _, end := range []int{60, 120} {
		if u := r.unsteady(simcluster.TruthPackagesGPUs, at(end), w); len(u) != 0 {
			t.Errorf("two steady windows ending at %d s read unsteady: %v", end, u)
		}
	}
}

// TestInputStressProblem: E16 holds a NodeTwin's cooling stress to the
// twin's formula of its measured power, its TDP and its own Node's inlet
// at its last update, not the facility ambient.
func TestInputStressProblem(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := &inputRecorder{keep: time.Hour}
	for sec := 0; sec <= 90; sec++ {
		s := inputTestSnapshot(start, float64(sec))
		s.Nodes[0].Host.InletC = 35 // node n breathes 15 °C above 20: a multiplier of 0.7
		r.add(s, nil)
	}
	mk := func(sec int, stress float64) v1alpha1.NodeTwin {
		var tw v1alpha1.NodeTwin
		tw.Status.LastUpdated = start.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
		tw.Status.PowerMeasurement = &v1alpha1.PowerMeasurement{Source: joulie.PowerSourcePrometheus, MeasuredNodePowerW: 165, NodeTdpW: 330}
		tw.Status.PredictedCoolingStressScore = stress
		return tw
	}
	// 165 W of 330 W at 35 °C: 0.5 / 0.7 * 100; at the facility ambient,
	// 20.8 °C at 80 s, it would be 0.5 / 0.984 * 100, about 50.8.
	if p := inputStressProblem(r, "n", mk(80, 71.4)); p != "" {
		t.Errorf("the stress at the Node's inlet: problem %q", p)
	}
	for name, tc := range map[string]struct {
		node    string
		tw      v1alpha1.NodeTwin
		problem string
	}{
		"the stress at the facility ambient": {"n", mk(80, 50.8), "predictedCoolingStressScore 50.80, want 71.43"},
		"a last update with no truth near":   {"n", mk(200, 71.4), "no truth at the NodeTwin's last update"},
		"a Node the simulator lacks":         {"x", mk(80, 71.4), "does not emulate"},
	} {
		if p := inputStressProblem(r, tc.node, tc.tw); !strings.Contains(p, tc.problem) || p == "" {
			t.Errorf("%s: problem %q, want one with %q", name, p, tc.problem)
		}
	}
}

// TestInputExpectedStressIsTheTwins: the cooling stress E16 expects is the
// one the twin computes, read through twin.Compute, below and at the clamp
// and at the floor of the temperature multiplier.
func TestInputExpectedStressIsTheTwins(t *testing.T) {
	hw := joulie.NodeHardware{CPU: joulie.NodeHardwareCPU{Sockets: 2, CapRange: joulie.CPUCapRange{MaxWattsPerSocket: 165}}}
	for _, tc := range []struct{ measuredW, ambientC float64 }{
		{264, 20}, {264, 23}, {100, -5}, {200, 0}, {200, 50}, {330, 30},
	} {
		out := twin.Compute(twin.Input{NodeName: "n", Hardware: hw, Profile: "performance",
			MeasuredNodePowerW: tc.measuredW, OutsideTempC: tc.ambientC, OutsideTempKnown: true})
		if tdp := out.PowerMeasurement.NodeTdpW; tdp != 330 {
			t.Fatalf("twin node TDP %v, want 330", tdp)
		}
		got := inputExpectedStress(tc.measuredW, out.PowerMeasurement.NodeTdpW, tc.ambientC)
		if math.Abs(got-out.PredictedCoolingStressScore) > 1e-9 {
			t.Errorf("%.0f W at %.0f °C: expected stress %.4f, the twin computes %.4f", tc.measuredW, tc.ambientC, got, out.PredictedCoolingStressScore)
		}
	}
}

// TestInputStressNode: E16 compares the first managed CPU-only NodeTwin
// with a measured power below the clamp, and says why when there is none.
func TestInputStressNode(t *testing.T) {
	cpu, gpu := &hwemu.Profile{}, &hwemu.Profile{GPUs: &hwemu.GPUSpec{}}
	node := func(name string, p *hwemu.Profile, managed bool) e2eNode {
		return e2eNode{Member: clusterspec.Member{Name: name, Profile: p}, Managed: managed}
	}
	mk := func(measuredW, tdpW, stress float64) v1alpha1.NodeTwin {
		var tw v1alpha1.NodeTwin
		tw.Status.PowerMeasurement = &v1alpha1.PowerMeasurement{MeasuredNodePowerW: measuredW, NodeTdpW: tdpW}
		tw.Status.PredictedCoolingStressScore = stress
		return tw
	}
	nodes := []e2eNode{node("a-gpu", gpu, true), node("b-unmanaged", cpu, false), node("c-clamped", cpu, true),
		node("d-no-power", cpu, true), node("e-cpu", cpu, true)}
	twins := map[string]v1alpha1.NodeTwin{"a-gpu": mk(500, 1000, 50), "b-unmanaged": mk(200, 330, 60),
		"c-clamped": mk(400, 330, 100), "d-no-power": mk(0, 330, 0), "e-cpu": mk(264, 330, 80)}
	if n, _, skip := inputStressNode(nodes, twins); skip != "" || n.Name != "e-cpu" {
		t.Errorf("picked %q (skip %q), want e-cpu", n.Name, skip)
	}
	delete(twins, "e-cpu")
	if _, _, skip := inputStressNode(nodes, twins); !strings.Contains(skip, "clamped at 100") {
		t.Errorf("every candidate clamped: skip %q", skip)
	}
	delete(twins, "c-clamped")
	if _, _, skip := inputStressNode(nodes, twins); !strings.Contains(skip, "no managed CPU-only Node") {
		t.Errorf("no candidate: skip %q", skip)
	}
}

// TestInputRecorderSamplesInTheBackground: the recorder reads until stop,
// its samples can be read meanwhile, and nothing is added after stop.
func TestInputRecorderSamplesInTheBackground(t *testing.T) {
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	sec := 0
	get := func() (*emulate.Snapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		sec++
		return inputTestSnapshot(start, float64(sec)), nil
	}
	r := startInputRecorder(get, time.Millisecond, time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s, err := r.latest(); err == nil && s.Time.Sub(start) >= 5*time.Second {
			break
		}
		if time.Now().After(deadline) {
			r.stop()
			t.Fatal("the recorder did not reach 5 samples in 10 s")
		}
		time.Sleep(time.Millisecond)
	}
	r.stop()
	r.mu.Lock()
	n := len(r.samples)
	r.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) != n || n < 5 {
		t.Errorf("%d samples at stop, %d after; want at least 5 and no more after stop", n, len(r.samples))
	}
}
