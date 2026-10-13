package main

// A v1 workload run (D12): one trace driven through the cluster as
// batch/v1 Jobs, its results written as OTLP JSON Lines under
// <SIM_RUNS_DIR>/<runId>/. A run starts from POST /sim/v1/runs, or from
// SIM_WORKLOAD_TRACE_PATH at startup as an implicit run, and ends when every
// job is terminal or when POST /sim/v1/runs/current/finish ends it. One run
// is active at a time; simulator.run holds the active or last one.
//
// The run's state is guarded by its lock. The emulator's goroutine takes it
// for each tick (workload_v1.go), the run API for a status or a finish.
// Kubernetes I/O runs on the run's own goroutine (runKube) and never touches
// the run's state.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// runKubeTimeout is the request timeout of a run's Kubernetes client.
const runKubeTimeout = 10 * time.Second

// v1State is where a job of a run is. The order matters: every state from
// v1Completed on is terminal.
type v1State int

const (
	// v1Waiting: no object yet; not due, or its create is in flight.
	v1Waiting v1State = iota
	// v1Submitted: its object exists, its pod is not bound.
	v1Submitted
	// v1Scheduled: its pod is bound to a node and not Running.
	v1Scheduled
	// v1Running: its pod is Running; it holds CPUs and GPU units.
	v1Running
	// v1Completing: its work is done and its completion is in flight.
	v1Completing
	v1Completed
	v1Failed
	v1Incomplete
)

func (s v1State) terminal() bool { return s >= v1Completed }

// v1Job is one trace job of a run: one pod.
type v1Job struct {
	sim   *simJob
	group *v1Group
	// index is the job's completion index in its group.
	index int
	// class is its workload class, performance or standard.
	class string
	state v1State

	podName, podUID string
	node            string
	// Times on the run's clock, the emulator's snapshot times.
	submittedAt, scheduledAt, runningAt, doneAt, lastProgressAt time.Time

	// unschedulable is true while its pod is Pending with PodScheduled
	// False, reason Unschedulable.
	unschedulable bool
	// gangWait is true when the last tick held it back for its gang.
	gangWait bool
	// The job's stall, while stalled: since when, and why.
	stalled     bool
	stallReason string
	stallRef    time.Time

	workCPU, workGPU float64
	energy           workload.EnergySplit
	// attempts counts the failed tries of the completion in flight.
	attempts int
}

// v1Group is the jobs that share one object: a Job object, or with
// workload.OwnerPod one bare pod.
type v1Group struct {
	key       string
	jobs      []*v1Job
	offset    float64
	namespace string
	name      string
	isJob     bool
	object    runtime.Object
	// queued is true while its create is in flight; created once it
	// exists.
	queued, created bool
}

// runSettings are a run's settings: the simulator's options with the run
// request's overrides applied.
type runSettings struct {
	owner        workload.Owner
	intent       workload.Intent
	completion   workload.Completion
	namespace    string
	nodeSelector map[string]string
	tolerations  []corev1.Toleration
	jobTTL       time.Duration
	maxCreates   int
	stallReport  time.Duration
	baseSpeed    float64
	fileInterval time.Duration
	qps          float32
	burst        int
	image        string
}

// v1NodeObs is what the run has seen of a Node and its NodeTwin. It only
// observes them (D14).
type v1NodeObs struct {
	powerProfile string
	class        string
	classSince   time.Time
}

// v1Node is an emulated node while a run drives it.
type v1Node struct {
	info emuNode
	cpu  *workload.CPUSlots
	gpu  *workload.GPUSlots
}

// workloadRun is one v1 run.
type workloadRun struct {
	s       *simulator
	id      string
	dir     string
	cluster string
	set     runSettings
	created time.Time
	jobs    []*v1Job
	groups  []*v1Group
	byID    map[string]*v1Job
	// byObject maps a group's namespace/name to it.
	byObject map[string]*v1Group
	gangs    map[string][]*v1Job
	kube     *runKube
	rec      *runRecorder

	mu       sync.Mutex
	t0       time.Time
	lastTick time.Time
	ended    bool
	phase    workload.Phase
	endedAt  time.Time
	// loads is each node's load for the next emulator step; loaded the
	// jobs on each node during the step that just ended, nextLoaded those
	// of the next one.
	loads              map[string]hwemu.Load
	loaded, nextLoaded map[string][]workload.JobLoad
	nodes              map[string]*v1Node
	nodeObs            map[string]v1NodeObs
	energyPrev         map[string]workload.NodeEnergy
	deferred           map[string]podObs
	runStalled         bool
	blockedSince       time.Time
	lastCreateErr      string
}

// active reports whether the run has not ended.
func (r *workloadRun) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.ended
}

// nodeLoads returns the loads of the next emulator step, and false once
// the run has ended, when the nodes' base load applies again (X9).
func (r *workloadRun) nodeLoads() (map[string]hwemu.Load, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return nil, false
	}
	out := make(map[string]hwemu.Load, len(r.loads))
	for k, v := range r.loads {
		out[k] = v
	}
	return out, true
}

// newRunSettings applies the run request's overrides ro to the simulator's
// options o. cluster is the spec name, for the default node selector;
// baseSpeed is SIM_BASE_SPEED_PER_CORE. An invalid override is an error.
func newRunSettings(o workloadOptions, ro workload.RunOptions, cluster string, baseSpeed float64) (runSettings, error) {
	s := runSettings{
		owner:        o.Owner,
		intent:       o.Intent,
		completion:   o.Completion,
		namespace:    o.Namespace,
		nodeSelector: o.NodeSelector,
		tolerations:  o.Tolerations,
		jobTTL:       o.JobTTL,
		maxCreates:   o.maxCreatesPerTick(),
		stallReport:  o.StallReport,
		baseSpeed:    baseSpeed,
		fileInterval: o.OTLPFileInterval,
		qps:          o.kubeQPS(),
		burst:        o.kubeBurst(),
		image:        envOrDefault(workload.EnvImage, workload.DefaultImage),
	}
	if s.owner == "" {
		s.owner = workload.DefaultOwner
	}
	if s.intent == "" {
		s.intent = workload.DefaultIntent
	}
	if s.completion == "" {
		s.completion = workload.DefaultCompletion
	}
	if s.stallReport <= 0 {
		s.stallReport = workload.DefaultStallReport
	}
	if s.fileInterval <= 0 {
		s.fileInterval = workload.DefaultOTLPFileInterval
	}
	if s.nodeSelector == nil {
		s.nodeSelector = workload.DefaultNodeSelector(cluster)
	}
	var errs []error
	if ro.Owner != "" {
		v, err := workload.ParseOwner(string(ro.Owner))
		errs = append(errs, err)
		s.owner = v
	}
	if ro.Intent != "" {
		v, err := workload.ParseIntent(string(ro.Intent))
		errs = append(errs, err)
		s.intent = v
	}
	if ro.Completion != "" {
		v, err := workload.ParseCompletion(string(ro.Completion))
		errs = append(errs, err)
		s.completion = v
	}
	if ro.Namespace != "" {
		s.namespace = ro.Namespace
	}
	if ro.JobTTL != nil {
		if ro.JobTTL.Duration < 0 {
			errs = append(errs, fmt.Errorf("jobTTL %v: want 0 or more", ro.JobTTL.Duration))
		}
		s.jobTTL = ro.JobTTL.Duration
	}
	if ro.NodeSelector != nil {
		s.nodeSelector = ro.NodeSelector
	}
	if ro.Tolerations != nil {
		s.tolerations = ro.Tolerations
	}
	if ro.MaxCreatesPerTick != nil {
		if n := *ro.MaxCreatesPerTick; n < 1 && n != workload.Unlimited {
			errs = append(errs, fmt.Errorf("maxCreatesPerTick %d: want a positive count, or %d for no cap", n, workload.Unlimited))
		}
		s.maxCreates = *ro.MaxCreatesPerTick
	}
	if ro.StallReport != nil {
		if ro.StallReport.Duration <= 0 {
			errs = append(errs, fmt.Errorf("stallReport %v: want a positive duration", ro.StallReport.Duration))
		}
		s.stallReport = ro.StallReport.Duration
	}
	if ro.BaseSpeedPerCore != nil {
		if v := *ro.BaseSpeedPerCore; !(v > 0) || math.IsInf(v, 0) {
			errs = append(errs, fmt.Errorf("baseSpeedPerCore %v: want a positive number", v))
		}
		s.baseSpeed = *ro.BaseSpeedPerCore
	}
	if !(s.baseSpeed > 0) {
		s.baseSpeed = 1
	}
	return s, errors.Join(errs...)
}

// checkRunID refuses a run id that cannot name a directory under the runs
// directory.
func checkRunID(id string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return errors.New("runId is empty")
	case id == "." || id == ".." || strings.ContainsAny(id, `/\`):
		return fmt.Errorf("runId %q names a directory under the runs directory, so it may not contain a slash or be . or ..", id)
	}
	return nil
}

// loadRunTrace loads the trace at path, a file or a directory of .jsonl
// parts, for a v1 run, in submit order with ties in trace order. A trace
// error is a *workload.RunError whose Line is the 1-based line of the file
// it is in. A trace with no job, or one jobId on two jobs, is an error.
func loadRunTrace(path string, opts workloadOptions) (*workloadEngine, error) {
	opts.Model = workload.ModelV1
	engine := &workloadEngine{
		startTime:      time.Now().UTC(),
		baseSpeedCore:  floatEnv(workload.EnvBaseSpeedPerCore, 1.0),
		jobs:           []*simJob{},
		jobByID:        map[string]*simJob{},
		jobsByWorkload: map[string][]*simJob{},
		opts:           opts,
	}
	paths, err := resolveTracePaths(path)
	if err != nil {
		return nil, &workload.RunError{Message: err.Error()}
	}
	// seenIn is the part each jobId was first read from.
	seenIn := map[string]string{}
	for _, p := range paths {
		// Lines are counted per part, so the error message and Line name
		// the same line of the part they are in.
		lineNum, jobsBefore := 0, len(engine.jobs)
		if err := loadTraceFileIntoEngine(p, engine, &lineNum); err != nil {
			line := lineNum
			if errors.Is(err, bufio.ErrTooLong) {
				line++
			}
			return nil, &workload.RunError{Message: err.Error(), Line: line}
		}
		for _, j := range engine.jobs[jobsBefore:] {
			first, dup := seenIn[j.JobID]
			if !dup {
				seenIn[j.JobID] = p
				continue
			}
			// The repeat is the second record of the id in this part
			// when the first is here too, else its first one here.
			nth := 1
			if first == p {
				nth = 2
			}
			return nil, &workload.RunError{
				Message: fmt.Sprintf("file %s: jobId %q appears twice, first in %s; every job of a run needs its own", p, j.JobID, first),
				Line:    traceLineOfJob(p, j.JobID, nth),
			}
		}
	}
	if len(engine.jobs) == 0 {
		return nil, &workload.RunError{Message: fmt.Sprintf("trace %s holds no job record", path)}
	}
	sort.SliceStable(engine.jobs, func(i, j int) bool { return engine.jobs[i].SubmitOffsetSec < engine.jobs[j].SubmitOffsetSec })
	return engine, nil
}

// traceLineOfJob is the 1-based line of the nth job record of path with
// jobId id, 0 when there is none.
func traceLineOfJob(path, id string, nth int) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxTraceLineBytes)
	for line := 1; sc.Scan(); line++ {
		var rec struct {
			Type  string `json:"type"`
			JobID string `json:"jobId"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != workload.TraceRecordJob || rec.JobID != id {
			continue
		}
		if nth--; nth == 0 {
			return line
		}
	}
	return 0
}

// jobSpecOf is the pod of trace job j as the workload library builds it.
func jobSpecOf(j *simJob, class string) workload.JobSpec {
	req := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(strconv.FormatFloat(j.RequestedCPUCores, 'f', -1, 64)),
		corev1.ResourceMemory: resource.MustParse(formatMemoryMiB(j.RequestedMemoryMiB)),
	}
	if j.RequestedGPUs > 0 {
		name := j.GPUResourceName
		if name == "" {
			name = "nvidia.com/gpu"
		}
		req[corev1.ResourceName(name)] = resource.MustParse(strconv.FormatFloat(j.RequestedGPUs, 'f', -1, 64))
	}
	return workload.JobSpec{JobID: j.JobID, Template: workload.Template{
		Namespace:       j.Namespace,
		WorkloadID:      j.WorkloadID,
		WorkloadType:    j.WorkloadType,
		PodRole:         j.PodRole,
		Class:           class,
		Gang:            j.Gang,
		SubmitOffsetSec: j.SubmitOffsetSec,
		Requests:        req,
		Placement:       workload.Placement{Affinity: j.Affinity, NodeSelector: j.NodeSelector, Tolerations: j.Tolerations},
		Profile: workload.Profile{
			CPUUtil:         j.CPUUtilTarget,
			GPUUtil:         j.GPUUtilTarget,
			MemoryIntensity: j.MemoryIntensity,
			IOIntensity:     j.IOIntensity,
		},
	}}
}

// v1Class is a trace job's class as a v1 pod declares it: performance, or
// standard for anything else.
func v1Class(c string) string {
	if c == workload.ClassPerformance {
		return workload.ClassPerformance
	}
	return workload.ClassStandard
}

// newWorkloadRun builds run id over the loaded trace: its jobs, its groups
// and their objects. It neither writes nor starts anything.
func newWorkloadRun(s *simulator, id, cluster string, engine *workloadEngine, set runSettings) *workloadRun {
	r := &workloadRun{
		s: s, id: id, cluster: cluster, set: set,
		created:    time.Now().UTC(),
		byID:       map[string]*v1Job{},
		byObject:   map[string]*v1Group{},
		gangs:      map[string][]*v1Job{},
		phase:      workload.PhaseRunning,
		loads:      map[string]hwemu.Load{},
		loaded:     map[string][]workload.JobLoad{},
		nextLoaded: map[string][]workload.JobLoad{},
		nodes:      map[string]*v1Node{},
		nodeObs:    map[string]v1NodeObs{},
		energyPrev: map[string]workload.NodeEnergy{},
		deferred:   map[string]podObs{},
	}
	build := workload.BuildOptions{
		RunID: id, Intent: set.intent, Image: set.image, Namespace: set.namespace,
		NodeSelector: set.nodeSelector, Tolerations: set.tolerations, JobTTL: set.jobTTL,
	}
	specs := make([]workload.JobSpec, len(engine.jobs))
	for i, sj := range engine.jobs {
		specs[i] = jobSpecOf(sj, v1Class(sj.Class))
		j := &v1Job{sim: sj, class: v1Class(sj.Class)}
		r.jobs = append(r.jobs, j)
		r.byID[sj.JobID] = j
		if sj.Gang && sj.WorkloadID != "" {
			r.gangs[sj.WorkloadID] = append(r.gangs[sj.WorkloadID], j)
		}
	}
	addGroup := func(g *v1Group, ids []string) {
		for i, id := range ids {
			j := r.byID[id]
			j.group, j.index = g, i
			g.jobs = append(g.jobs, j)
		}
		r.groups = append(r.groups, g)
		r.byObject[g.namespace+"/"+g.name] = g
	}
	if set.owner == workload.OwnerPod {
		for _, spec := range specs {
			pod := workload.BuildPod(spec, build)
			addGroup(&v1Group{key: spec.JobID, offset: spec.SubmitOffsetSec, namespace: pod.Namespace, name: pod.Name, object: pod}, []string{spec.JobID})
		}
		return r
	}
	groups, split := workload.GroupJobs(specs)
	if len(split) > 0 {
		log.Printf("run %s: the jobs of %s differ in their pods; each distinct pod became a Job object of its own", id, strings.Join(split, ", "))
	}
	for _, g := range groups {
		job := workload.BuildJob(g, build)
		addGroup(&v1Group{key: g.Key, offset: g.SubmitOffsetSec, namespace: job.Namespace, name: job.Name, isJob: true, object: job}, g.JobIDs)
	}
	return r
}

// runService serves the run API.
type runService struct {
	s    *simulator
	opts workloadOptions
	// mu makes a start's check for an active run and its store one step.
	mu sync.Mutex
}

// registerRunHandlers adds the run API to mux. Outside emulate mode with
// the v1 model every route answers 412.
func (s *simulator) registerRunHandlers(mux *http.ServeMux, opts workloadOptions) *runService {
	rs := &runService{s: s, opts: opts}
	mux.HandleFunc(workload.PathRuns, rs.handleRuns)
	mux.HandleFunc(workload.PathRunCurrent, rs.handleCurrent)
	mux.HandleFunc(workload.PathRunFinish, rs.handleFinish)
	return rs
}

func (rs *runService) enabled() bool {
	return rs.s.emu != nil && rs.opts.model() == workload.ModelV1
}

func writeRunJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRunError(w http.ResponseWriter, status int, err error) {
	var re *workload.RunError
	if !errors.As(err, &re) {
		re = &workload.RunError{Message: err.Error()}
	}
	writeRunJSON(w, status, re)
}

// guard answers 412 outside v1 emulate mode and 405 for another method,
// and reports whether the request may go on.
func (rs *runService) guard(w http.ResponseWriter, r *http.Request, method string, status *int) bool {
	if !rs.enabled() {
		*status = http.StatusPreconditionFailed
		writeRunError(w, *status, fmt.Errorf("the run API needs %s=true and %s=%s", envEmulate, workload.EnvModel, workload.ModelV1))
		return false
	}
	if r.Method != method {
		*status = http.StatusMethodNotAllowed
		writeRunError(w, *status, fmt.Errorf("method %s not allowed, want %s", r.Method, method))
		return false
	}
	return true
}

func (rs *runService) handleRuns(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusCreated
	defer rs.s.observeRequest("sim_runs", r.Method, &status, start)
	if !rs.guard(w, r, http.MethodPost, &status) {
		return
	}
	var req workload.RunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		status = http.StatusBadRequest
		writeRunError(w, status, fmt.Errorf("run request: %w", err))
		return
	}
	started, code, err := rs.start(req)
	if err != nil {
		status = code
		writeRunError(w, status, err)
		return
	}
	writeRunJSON(w, status, started)
}

func (rs *runService) handleCurrent(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer rs.s.observeRequest("sim_runs_current", r.Method, &status, start)
	if !rs.guard(w, r, http.MethodGet, &status) {
		return
	}
	run := rs.s.run.Load()
	if run == nil {
		status = http.StatusNotFound
		writeRunError(w, status, errors.New("no run has started"))
		return
	}
	writeRunJSON(w, status, run.status())
}

func (rs *runService) handleFinish(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer rs.s.observeRequest("sim_runs_finish", r.Method, &status, start)
	if !rs.guard(w, r, http.MethodPost, &status) {
		return
	}
	cleanup := false
	if v := r.URL.Query().Get(workload.QueryCleanup); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			status = http.StatusBadRequest
			writeRunError(w, status, fmt.Errorf("%s=%q: want true or false", workload.QueryCleanup, v))
			return
		}
		cleanup = b
	}
	run := rs.s.run.Load()
	if run == nil {
		status = http.StatusNotFound
		writeRunError(w, status, errors.New("no run has started"))
		return
	}
	writeRunJSON(w, status, run.finish(cleanup))
}

// start starts the run req asks for, and returns its answer, or the status
// code and error of a refusal.
func (rs *runService) start(req workload.RunRequest) (workload.RunStarted, int, error) {
	if err := checkRunID(req.RunID); err != nil {
		return workload.RunStarted{}, http.StatusBadRequest, err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if cur := rs.s.run.Load(); cur != nil && cur.active() {
		return workload.RunStarted{}, http.StatusConflict, fmt.Errorf("run %s is active; finish it first", cur.id)
	}
	cluster := req.ClusterName
	if cluster == "" {
		cluster = rs.s.emu.cluster
	}
	runsDir := rs.opts.RunsDir
	if runsDir == "" {
		runsDir = filepath.Join(defaultHwemuRoot, workload.RunsDirName)
	}
	dir := filepath.Join(runsDir, req.RunID)
	for _, f := range []string{workload.SimulatorMetricsFile, workload.SimulatorLogsFile} {
		if _, err := os.Stat(filepath.Join(dir, workload.OTLPDir, f)); err == nil {
			return workload.RunStarted{}, http.StatusConflict, fmt.Errorf("run %s already has results in %s; use another runId", req.RunID, dir)
		}
	}
	trace := req.Trace
	if trace == "" {
		trace = filepath.Join(dir, workload.TraceFile)
	}
	engine, err := loadRunTrace(trace, rs.opts)
	if err != nil {
		return workload.RunStarted{}, http.StatusBadRequest, err
	}
	set, err := newRunSettings(rs.opts, req.Options, cluster, engine.baseSpeedCore)
	if err != nil {
		return workload.RunStarted{}, http.StatusBadRequest, &workload.RunError{Message: err.Error()}
	}
	kube, err := kubeClientFactory(set.qps, set.burst, runKubeTimeout)
	if err != nil {
		return workload.RunStarted{}, http.StatusInternalServerError, fmt.Errorf("run %s: Kubernetes client: %w", req.RunID, err)
	}
	rec, err := newRunRecorder(dir, rs.s.emu.resource, cluster, req.RunID, set.fileInterval)
	if err != nil {
		return workload.RunStarted{}, http.StatusInternalServerError, err
	}
	run := newWorkloadRun(rs.s, req.RunID, cluster, engine, set)
	run.dir, run.rec = dir, rec
	run.kube = newRunKube(kube, workload.RunLabelValue(req.RunID))
	run.kube.start()
	rs.s.run.Store(run)
	log.Printf("run %s started jobs=%d objects=%d trace=%s dir=%s owner=%s intent=%s completion=%s",
		run.id, len(run.jobs), len(run.groups), trace, dir, set.owner, set.intent, set.completion)
	return workload.RunStarted{RunID: run.id, Jobs: len(run.jobs), Groups: len(run.groups), StartedAt: run.created}, http.StatusCreated, nil
}

// startImplicitRun starts the run of SIM_WORKLOAD_TRACE_PATH at startup,
// with id SIM_RUN_ID or a generated one. A failure is logged; the
// simulator keeps serving and the run API stays open.
func (rs *runService) startImplicitRun(trace string) {
	id := rs.opts.RunID
	if id == "" {
		id = "run-" + time.Now().UTC().Format("20060102-150405")
	}
	if _, _, err := rs.start(workload.RunRequest{RunID: id, Trace: trace}); err != nil {
		log.Printf("warning: the startup run of %s=%s did not start: %v", workload.EnvTracePath, trace, err)
	}
}

// counts returns the jobs by state at now.
func (r *workloadRun) counts(now time.Time) workload.RunJobCounts {
	c := workload.RunJobCounts{Total: len(r.jobs)}
	for _, j := range r.jobs {
		switch j.state {
		case v1Waiting:
			if r.due(j.group, now) {
				c.Pending++
			} else {
				c.NotDue++
			}
		case v1Submitted:
			c.Pending++
		case v1Scheduled:
			c.Scheduled++
		case v1Running, v1Completing:
			c.Running++
			if j.stalled {
				c.Stalled++
			}
		case v1Completed:
			c.Completed++
		case v1Failed:
			c.Failed++
		case v1Incomplete:
			c.Incomplete++
		}
	}
	return c
}

// due reports whether group g's submit offset has come at now.
func (r *workloadRun) due(g *v1Group, now time.Time) bool {
	if r.t0.IsZero() {
		return false
	}
	return !now.Before(r.t0.Add(time.Duration(g.offset * float64(time.Second))))
}

// status is the run's RunStatus.
func (r *workloadRun) status() workload.RunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}

func (r *workloadRun) statusLocked() workload.RunStatus {
	now := r.lastTick
	if r.ended {
		now = r.endedAt
	}
	st := workload.RunStatus{
		RunID:      r.id,
		Phase:      r.phase,
		StartedAt:  r.created,
		Jobs:       r.counts(now),
		RunStalled: r.runStalled,
	}
	if !r.t0.IsZero() {
		st.StartedAt = r.t0
		st.ElapsedSeconds = math.Max(0, now.Sub(r.t0).Seconds())
	}
	st.AllSubmitted = true
	for _, g := range r.groups {
		if !g.created {
			for _, j := range g.jobs {
				if !j.state.terminal() {
					st.AllSubmitted = false
				}
			}
		}
	}
	return st
}

// finish ends the run by request: jobs still running end incomplete, the
// files get their last records and close. With cleanup the run's objects
// are deleted in the background. It returns the final status; a run that
// has ended already is only cleaned up.
func (r *workloadRun) finish(cleanup bool) workload.RunStatus {
	r.mu.Lock()
	if !r.ended {
		now := r.lastTick
		if now.IsZero() {
			now = time.Now().UTC()
		}
		r.endLocked(now, workload.PhaseAborted)
	}
	st := r.statusLocked()
	r.mu.Unlock()
	if cleanup {
		go r.kube.cleanup(r.id, r.namespaces(), r.set.owner == workload.OwnerJob)
	}
	return st
}

// namespaces lists the namespaces of the run's objects.
func (r *workloadRun) namespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range r.groups {
		if !seen[g.namespace] {
			seen[g.namespace] = true
			out = append(out, g.namespace)
		}
	}
	sort.Strings(out)
	return out
}

// endLocked ends the run at now. phase is PhaseAborted for a finish by
// request; it becomes PhaseFinished when no job remains. A job whose
// completion is in flight has done its work and counts as completed; every
// other job that is not terminal ends incomplete.
func (r *workloadRun) endLocked(now time.Time, phase workload.Phase) {
	remaining := false
	for _, j := range r.jobs {
		switch {
		case j.state == v1Completing:
			r.completeJob(j, now)
		case !j.state.terminal():
			remaining = true
			r.endJob(j, now, v1Incomplete, "")
		}
	}
	if !remaining {
		phase = workload.PhaseFinished
	}
	c := r.counts(now)
	r.rec.event(semconv.EventSimRunFinished, now,
		fmt.Sprintf("run %s %s: %d completed, %d failed, %d incomplete of %d", r.id, phase, c.Completed, c.Failed, c.Incomplete, c.Total),
		otlpjson.Int(semconv.AttrSimRunJobs, int64(c.Total)),
		otlpjson.Int(semconv.AttrSimRunCompletedJobs, int64(c.Completed)),
		otlpjson.Int(semconv.AttrSimRunFailedJobs, int64(c.Failed)),
		otlpjson.Int(semconv.AttrSimRunIncompleteJobs, int64(c.Incomplete)))
	r.rec.flushLogs()
	r.rec.maybeWriteMetrics(now, r.activeCounts(), true)
	if err := r.rec.close(); err != nil {
		log.Printf("warning: run %s: close its files: %v", r.id, err)
	}
	r.ended, r.phase, r.endedAt = true, phase, now
	r.loads = map[string]hwemu.Load{}
	r.kube.stop()
	log.Printf("run %s %s: %d completed, %d failed, %d incomplete of %d", r.id, phase, c.Completed, c.Failed, c.Incomplete, c.Total)
}

// activeCounts is the number of active jobs by class and state, the
// joulie.sim.workload.jobs.active points.
func (r *workloadRun) activeCounts() map[recorderPair]int64 {
	out := map[recorderPair]int64{}
	for _, j := range r.jobs {
		var st string
		switch {
		case j.state == v1Submitted || j.state == v1Scheduled:
			st = semconv.JobStatePending
		case (j.state == v1Running || j.state == v1Completing) && j.stalled:
			st = semconv.JobStateStalled
		case j.state == v1Running || j.state == v1Completing:
			st = semconv.JobStateRunning
		default:
			continue
		}
		out[recorderPair{j.class, st}]++
	}
	return out
}

// jobAttrs are the attributes every event of job j carries, then extra.
func (j *v1Job) attrs(extra ...otlpjson.KeyValue) []otlpjson.KeyValue {
	a := []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrSimJobID, j.sim.JobID),
		otlpjson.String(semconv.AttrSimWorkloadID, j.sim.WorkloadID),
		otlpjson.String(semconv.AttrSimWorkloadType, j.sim.WorkloadType),
		otlpjson.String(semconv.AttrSimPodRole, j.sim.PodRole),
		otlpjson.String(semconv.AttrJoulieWorkloadClass, j.class),
		otlpjson.String(semconv.AttrK8sNamespaceName, j.group.namespace),
	}
	if j.group.isJob {
		a = append(a, otlpjson.String(semconv.AttrK8sJobName, j.group.name))
	}
	if j.podName != "" {
		a = append(a, otlpjson.String(semconv.AttrK8sPodName, j.podName))
	}
	if j.podUID != "" {
		a = append(a, otlpjson.String(semconv.AttrK8sPodUID, j.podUID))
	}
	return append(a, extra...)
}

// idealRunTime is how long job j takes at full speed with no cap: the
// longer of its CPU and GPU work at the rates of workload.Rates with every
// throughput, share and sensitivity at 1.
func (r *workloadRun) idealRunTime(j *v1Job) float64 {
	cpuRate, gpuRate := workload.Rates(workload.ModelV1, workload.RateInputs{
		BaseSpeed: r.set.baseSpeed, CPURequest: j.sim.RequestedCPUCores, GPURequest: j.sim.RequestedGPUs,
		CPUUtil: j.sim.CPUUtilTarget, MemoryIntensity: j.sim.MemoryIntensity, IOIntensity: j.sim.IOIntensity,
		CPUFeedIntensity: j.sim.CPUFeedIntensity, SensitivityCPU: 1, SensitivityGPU: 1,
		CPUMul: 1, GPUMul: 1, GPUCapFactor: 1, CPUShare: 1, GPUShare: 1,
	})
	ideal := 0.0
	if j.sim.CPUUnitsTotal > 0 && cpuRate > 0 {
		ideal = j.sim.CPUUnitsTotal / cpuRate
	}
	if j.sim.GPUUnitsTotal > 0 && gpuRate > 0 {
		ideal = math.Max(ideal, j.sim.GPUUnitsTotal/gpuRate)
	}
	return ideal
}

// completeJob ends job j completed at now: its event, the legacy log line
// and counters, and the run's metrics. Its end time is when its work was
// done.
func (r *workloadRun) completeJob(j *v1Job, now time.Time) {
	done := j.doneAt
	if done.IsZero() {
		done = now
	}
	duration := done.Sub(j.submittedAt).Seconds()
	runTime := 0.0
	if !j.runningAt.IsZero() {
		runTime = done.Sub(j.runningAt).Seconds()
	}
	ideal := r.idealRunTime(j)
	r.rec.event(semconv.EventSimJobCompleted, done,
		fmt.Sprintf("job %s completed on %s after %.1fs", j.sim.JobID, j.node, duration),
		j.attrs(
			otlpjson.Double(semconv.AttrSimJobDuration, duration),
			otlpjson.Double(semconv.AttrSimJobRunTime, runTime),
			otlpjson.Double(semconv.AttrSimJobIdealRunTime, ideal),
			otlpjson.Double(semconv.AttrSimJobEnergy, j.energy.Total()),
			otlpjson.Double(semconv.AttrSimWorkCPU, j.workCPU),
			otlpjson.Double(semconv.AttrSimWorkGPU, j.workGPU),
		)...)
	r.rec.observe(r.rec.duration, recorderSecondsBounds, j.class, duration)
	if ideal > 0 {
		r.rec.observe(r.rec.slowdown, recorderSlowdownBounds, j.class, runTime/ideal)
	}
	r.s.jobCompleted.WithLabelValues(j.class, j.node).Inc()
	r.s.jobCompletion.Observe(duration)
	log.Print(workload.FormatJobCompletedLog(j.sim.JobID, j.node, j.class, done.Sub(j.submittedAt)))
	r.endJob(j, now, v1Completed, "")
}

// endJob moves job j to the terminal state to at now and releases what it
// holds. A failure emits job.failed with errorType, an incomplete end
// job.incomplete with the work left; a completion's event is completeJob's.
func (r *workloadRun) endJob(j *v1Job, now time.Time, to v1State, errorType string) {
	switch to {
	case v1Failed:
		r.rec.event(semconv.EventSimJobFailed, now, fmt.Sprintf("job %s failed: %s", j.sim.JobID, errorType),
			j.attrs(otlpjson.String(semconv.AttrErrorType, errorType))...)
		r.rec.addFinished(j.class, semconv.JobOutcomeFailed)
	case v1Incomplete:
		r.rec.event(semconv.EventSimJobIncomplete, now, fmt.Sprintf("job %s incomplete when run %s ended", j.sim.JobID, r.id),
			j.attrs(
				otlpjson.Double(semconv.AttrSimWorkCPURemaining, math.Max(0, j.sim.CPUUnitsRemaining)),
				otlpjson.Double(semconv.AttrSimWorkGPURemaining, math.Max(0, j.sim.GPUUnitsRemaining)),
			)...)
		r.rec.addFinished(j.class, semconv.JobOutcomeIncomplete)
	case v1Completed:
		r.rec.addFinished(j.class, semconv.JobOutcomeCompleted)
	}
	r.release(j)
	j.state = to
	j.stalled = false
}

// release frees the CPUs and GPU units job j holds.
func (r *workloadRun) release(j *v1Job) {
	if n := r.nodes[j.node]; n != nil {
		n.cpu.Release(j.sim.JobID)
		n.gpu.Release(j.sim.JobID)
	}
}

// cleanupObjects lists the run's objects in namespaces and deletes them,
// in the background of a finish. Job objects go with their pods.
func cleanupObjects(ctx context.Context, k *runKube, runID string, namespaces []string, jobs bool) {
	sel := metav1.ListOptions{LabelSelector: workload.LabelRunID + "=" + workload.RunLabelValue(runID)}
	bg := metav1.DeletePropagationBackground
	del := metav1.DeleteOptions{PropagationPolicy: &bg}
	n := 0
	for _, ns := range namespaces {
		if jobs {
			list, err := k.kube.BatchV1().Jobs(ns).List(ctx, sel)
			if err != nil {
				log.Printf("warning: run %s cleanup: list Jobs in %s: %v", runID, ns, err)
				continue
			}
			for _, item := range list.Items {
				if err := k.kube.BatchV1().Jobs(ns).Delete(ctx, item.Name, del); err != nil {
					log.Printf("warning: run %s cleanup: delete Job %s/%s: %v", runID, ns, item.Name, err)
					continue
				}
				n++
			}
			continue
		}
		list, err := k.kube.CoreV1().Pods(ns).List(ctx, sel)
		if err != nil {
			log.Printf("warning: run %s cleanup: list pods in %s: %v", runID, ns, err)
			continue
		}
		for _, item := range list.Items {
			if err := k.kube.CoreV1().Pods(ns).Delete(ctx, item.Name, del); err != nil {
				log.Printf("warning: run %s cleanup: delete pod %s/%s: %v", runID, ns, item.Name, err)
				continue
			}
			n++
		}
	}
	log.Printf("run %s cleanup deleted %d objects", runID, n)
}
