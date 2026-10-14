package main

// The v1 workload engine. A run's tick runs on the emulator's
// goroutine, in the publish callback, right after each emulator step, and
// takes the snapshot's time as now and the time since the last snapshot
// as dt. Its steps, in order:
//
//  1. submit: take the results of the Kubernetes calls of earlier ticks,
//     and queue the creates of the groups now due (no per-tick cap unless
//     SIM_WORKLOAD_MAX_CREATES_PER_TICK sets one);
//  2. observe: the run's pods as the informer last saw them, and the
//     Nodes' power profile labels and NodeTwins' schedulable classes;
//  3. GPU slots: a job whose pod became Running takes its CPUs and GPU
//     units (whole GPUs 1 per GPU, MIG slices migDevicesPerGPU, time-slice
//     replicas timesliceReplicas);
//  4. progress: each job Running since an earlier tick does work at the
//     rates of workload.Rates, with the throughput its node's controls
//     leave it (CPUs and GPUs it holds) over the step that just ended;
//  5. complete: a job whose work is done is marked complete on its pod
//     (the KWOK Stage pod-complete then ends the pod) or, with
//     SIM_WORKLOAD_COMPLETION=delete, its pod is deleted; its event, the
//     legacy log line and counters follow once the call succeeds;
//  6. load: each node's load for the next emulator step, from the jobs
//     that still hold its CPUs and GPUs;
//  7. energy: each node's ledger deltas over the step that just ended are
//     attributed to the jobs that loaded it (workload.Attribute);
//  8. stalls: a Running job with no progress, and the whole run when every
//     submitted job is blocked, are reported after SIM_WORKLOAD_STALL_REPORT,
//     never completed by force;
//  9. run end: once every job is terminal the run ends.
//
// Kubernetes calls (creates, the completion patch, deletes) and the
// informers run on the run's own goroutines (runKube); the two exchange
// state only through runKube's lock. A call's result reaches the first tick
// after it returns, so a job's submit, start and end are stamped within a
// tick of when the API server took them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Engine constants.
const (
	// v1WorkEpsilon is the work left that counts as none, so float
	// rounding never keeps a job alive for one more tick.
	v1WorkEpsilon = 1e-9
	// v1CompletionAttempts is how often a completion call is tried before
	// the job fails with patch_failed.
	v1CompletionAttempts = 3
	// runKubeWorkers is how many Kubernetes calls run at once, each on a
	// worker of its own; the client's QPS and burst still limit them.
	runKubeWorkers = 8
	// runKubeTwinPoll is how often NodeTwins are listed. assumed: a
	// schedulable class changes over minutes, so the time a class took
	// effect is known to within this period.
	runKubeTwinPoll = 10 * time.Second
	// runKubeCleanupTimeout bounds the background cleanup of a finish.
	runKubeCleanupTimeout = 2 * time.Minute
)

// podObs is one pod of a run as the informer saw it.
type podObs struct {
	key, ns, name, uid string
	// jobName and index are its Job object and completion index; jobID
	// is its trace jobId, set on a bare pod.
	jobName       string
	index         int
	jobID         string
	node          string
	phase         corev1.PodPhase
	unschedulable bool
	deleted       bool
}

// podObsOf is what the engine reads of pod p.
func podObsOf(p *corev1.Pod, deleted bool) podObs {
	o := podObs{
		key: p.Namespace + "/" + p.Name, ns: p.Namespace, name: p.Name, uid: string(p.UID),
		jobName: p.Labels[batchv1.JobNameLabel],
		jobID:   p.Annotations[workload.AnnotationJobID],
		node:    p.Spec.NodeName,
		phase:   p.Status.Phase,
		deleted: deleted,
	}
	if v, ok := p.Annotations[batchv1.JobCompletionIndexAnnotation]; ok {
		if i, err := strconv.Atoi(v); err == nil {
			o.index = i
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			o.unschedulable = true
		}
	}
	return o
}

type kubeCmdKind int

const (
	cmdCreate kubeCmdKind = iota
	cmdComplete
	cmdDelete
)

// kubeCmd is one Kubernetes call a tick asks for. obj is never changed
// after the command is queued.
type kubeCmd struct {
	kind  kubeCmdKind
	group *v1Group
	job   *v1Job
	ns    string
	name  string
	obj   runtime.Object
	at    time.Time
}

type kubeResult struct {
	cmd kubeCmd
	err error
}

// kubeIn is what a tick takes from runKube: the results of finished calls
// and what changed since the last take.
type kubeIn struct {
	results  []kubeResult
	pods     map[string]podObs
	profiles map[string]string
	classes  map[string]string
}

// runKube is a run's Kubernetes side: its calls, on runKubeWorkers
// workers, and its informers, on goroutines of their own. A tick hands it
// commands with push and takes results and observations with take. Each
// result is there for the next take as soon as its call returns, so a
// burst of creates delays no other result, and completion calls run ahead
// of queued creates.
type runKube struct {
	kube      kubernetes.Interface
	runLabel  string
	listTwins func(context.Context) ([]v1alpha1.NodeTwin, error)

	mu sync.Mutex
	// completions holds the queued completion calls (patches and
	// deletes), creates the queued creates; a worker takes the first
	// completion, else the first create.
	completions []kubeCmd
	creates     []kubeCmd
	results     []kubeResult
	pods        map[string]podObs
	profiles    map[string]string
	classes     map[string]string
	lastTwin    map[string]string
	// queued holds a token per command pushed while every worker may be
	// waiting, up to one per worker, so a worker that found the queues
	// empty wakes for the next push.
	queued chan struct{}

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// newRunKube returns the Kubernetes side of the run whose objects carry
// runLabel. Nothing runs until start.
func newRunKube(kube kubernetes.Interface, runLabel string) *runKube {
	return &runKube{
		kube: kube, runLabel: runLabel, listTwins: listNodeTwinsREST(kube),
		pods: map[string]podObs{}, profiles: map[string]string{}, classes: map[string]string{},
		lastTwin: map[string]string{},
		queued:   make(chan struct{}, runKubeWorkers),
	}
}

// start runs the informers, the NodeTwin poll and the workers until stop.
func (k *runKube) start() {
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel, k.done = cancel, make(chan struct{})
	go func() {
		defer close(k.done)
		k.loop(ctx)
	}()
}

// stop ends the goroutines of start, if they run. Commands still queued
// are dropped.
func (k *runKube) stop() {
	k.stopOnce.Do(func() {
		if k.cancel != nil {
			k.cancel()
		}
	})
}

func (k *runKube) loop(ctx context.Context) {
	pods := informers.NewSharedInformerFactoryWithOptions(k.kube, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.LabelSelector = workload.LabelRunID + "=" + k.runLabel
	}))
	_, _ = pods.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { k.observePodObject(obj, false) },
		UpdateFunc: func(_, obj any) { k.observePodObject(obj, false) },
		DeleteFunc: func(obj any) { k.observePodObject(obj, true) },
	})
	nodes := informers.NewSharedInformerFactory(k.kube, 0)
	_, _ = nodes.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { k.observeNodeObject(obj) },
		UpdateFunc: func(_, obj any) { k.observeNodeObject(obj) },
	})
	pods.Start(ctx.Done())
	nodes.Start(ctx.Done())
	defer pods.Shutdown()
	defer nodes.Shutdown()

	var workers sync.WaitGroup
	for i := 0; i < runKubeWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			k.work(ctx)
		}()
	}
	defer workers.Wait()

	twinBeat := time.NewTicker(runKubeTwinPoll)
	defer twinBeat.Stop()
	k.pollTwins(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-twinBeat.C:
			k.pollTwins(ctx)
		}
	}
}

// work runs queued commands one at a time until ctx ends, and keeps each
// result for the next take as soon as its call returns.
func (k *runKube) work(ctx context.Context) {
	for {
		c, ok := k.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-k.queued:
			}
			continue
		}
		k.keep(c, k.exec(ctx, c))
	}
}

// next takes the first queued completion call, else the first queued
// create; false when both queues are empty.
func (k *runKube) next() (kubeCmd, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, q := range []*[]kubeCmd{&k.completions, &k.creates} {
		if len(*q) > 0 {
			c := (*q)[0]
			(*q)[0] = kubeCmd{}
			*q = (*q)[1:]
			return c, true
		}
	}
	return kubeCmd{}, false
}

// keep stores the result of command c for the next take.
func (k *runKube) keep(c kubeCmd, err error) {
	k.mu.Lock()
	k.results = append(k.results, kubeResult{cmd: c, err: err})
	k.mu.Unlock()
}

// exec makes the call of c.
func (k *runKube) exec(ctx context.Context, c kubeCmd) error {
	switch c.kind {
	case cmdCreate:
		switch o := c.obj.(type) {
		case *batchv1.Job:
			_, err := k.kube.BatchV1().Jobs(c.ns).Create(ctx, o.DeepCopy(), metav1.CreateOptions{})
			return err
		case *corev1.Pod:
			_, err := k.kube.CoreV1().Pods(c.ns).Create(ctx, o.DeepCopy(), metav1.CreateOptions{})
			return err
		}
		return fmt.Errorf("create %s/%s: unknown object %T", c.ns, c.name, c.obj)
	case cmdComplete:
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
			workload.AnnotationComplete:    workload.CompleteValue,
			workload.AnnotationCompletedAt: c.at.UTC().Format(time.RFC3339Nano),
		}}})
		if err != nil {
			return err
		}
		_, err = k.kube.CoreV1().Pods(c.ns).Patch(ctx, c.name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	case cmdDelete:
		grace := int64(0)
		err := k.kube.CoreV1().Pods(c.ns).Delete(ctx, c.name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return fmt.Errorf("unknown command %d", c.kind)
}

// push queues cmds and wakes a waiting worker for each, up to all of
// them. It never blocks, so a tick never waits for the API server.
func (k *runKube) push(cmds ...kubeCmd) {
	if len(cmds) == 0 {
		return
	}
	k.mu.Lock()
	for _, c := range cmds {
		if c.kind == cmdCreate {
			k.creates = append(k.creates, c)
		} else {
			k.completions = append(k.completions, c)
		}
	}
	k.mu.Unlock()
	for range cmds {
		select {
		case k.queued <- struct{}{}:
		default:
			return
		}
	}
}

// take returns the results and the changes since the last take.
func (k *runKube) take() kubeIn {
	k.mu.Lock()
	defer k.mu.Unlock()
	in := kubeIn{results: k.results, pods: k.pods, profiles: k.profiles, classes: k.classes}
	k.results = nil
	k.pods, k.profiles, k.classes = map[string]podObs{}, map[string]string{}, map[string]string{}
	return in
}

func (k *runKube) observePodObject(obj any, deleted bool) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	if p, ok := obj.(*corev1.Pod); ok {
		k.observePod(p, deleted)
	}
}

// observePod records the state of pod p, the latest per pod.
func (k *runKube) observePod(p *corev1.Pod, deleted bool) {
	o := podObsOf(p, deleted)
	k.mu.Lock()
	k.pods[o.key] = o
	k.mu.Unlock()
}

func (k *runKube) observeNodeObject(obj any) {
	if n, ok := obj.(*corev1.Node); ok {
		k.observeNode(n.Name, n.Labels[workload.LabelPowerProfile])
	}
}

// observeNode records node's joulie.io/power-profile label, "" without it.
func (k *runKube) observeNode(node, profile string) {
	k.mu.Lock()
	k.profiles[node] = profile
	k.mu.Unlock()
}

// observeTwin records node's NodeTwin schedulable class when it changed.
func (k *runKube) observeTwin(node, class string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if prev, ok := k.lastTwin[node]; ok && prev == class {
		return
	}
	k.lastTwin[node] = class
	k.classes[node] = class
}

// pollTwins lists the NodeTwins and records each class under its node,
// spec.nodeName: the object name is pkg/api.ObjectNameForNode of the node,
// which differs from it for a name with dots or capitals. Without the CRD,
// or on an error, it records nothing.
func (k *runKube) pollTwins(ctx context.Context) {
	if k.listTwins == nil {
		return
	}
	twins, err := k.listTwins(ctx)
	if err != nil {
		return
	}
	for _, t := range twins {
		node := t.Spec.NodeName
		if node == "" {
			node = t.Name
		}
		k.observeTwin(node, t.Status.SchedulableClass)
	}
}

// cleanup deletes the run's objects, in the background of a finish.
func (k *runKube) cleanup(runID string, namespaces []string, jobs bool) {
	ctx, cancel := context.WithTimeout(context.Background(), runKubeCleanupTimeout)
	defer cancel()
	cleanupObjects(ctx, k, runID, namespaces, jobs)
}

// errNoNodeTwins: the cluster serves no NodeTwin resource.
var errNoNodeTwins = errors.New("no NodeTwin resource")

// listNodeTwinsREST lists NodeTwins through kube's REST client, decoded
// into the typed api/v1alpha1 structs, when the cluster serves them.
func listNodeTwinsREST(kube kubernetes.Interface) func(context.Context) ([]v1alpha1.NodeTwin, error) {
	return func(ctx context.Context) ([]v1alpha1.NodeTwin, error) {
		res, err := kube.Discovery().ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
		if err != nil {
			return nil, err
		}
		found := false
		for _, r := range res.APIResources {
			found = found || r.Name == "nodetwins"
		}
		rc := kube.CoreV1().RESTClient()
		if !found || rc == nil || reflect.ValueOf(rc).IsNil() {
			return nil, errNoNodeTwins
		}
		raw, err := rc.Get().AbsPath("/apis", v1alpha1.GroupVersion.Group, v1alpha1.GroupVersion.Version, "nodetwins").DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		var list v1alpha1.NodeTwinList
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list.Items, nil
	}
}

// tickActivity is what happened in one tick, for the run stall.
type tickActivity struct {
	progress, started, completed, completing bool
}

// tick is one step of the run at now, dt after the last snapshot, on the
// emulator's goroutine right after it published snap. nodes gives the
// emulated nodes; hw are the snapshot's hardware events, copied into the
// run's logs.
func (r *workloadRun) tick(now time.Time, dt time.Duration, snap *emulate.Snapshot, nodes emuNodes, hw []telemetry.EventRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return
	}
	first := r.t0.IsZero()
	if first {
		r.t0 = now
		r.rec.start = now
		r.rec.event(semconv.EventSimRunStarted, now, fmt.Sprintf("run %s started with %d jobs", r.id, len(r.jobs)),
			otlpjson.Int(semconv.AttrSimRunJobs, int64(len(r.jobs))))
	}
	r.lastTick = now
	r.rec.hardware(hw, now)
	if first {
		dt = 0
	}

	in := r.kube.take()
	var act tickActivity
	// 1. submit.
	cmds := r.applyResults(in.results, now, &act)
	cmds = append(cmds, r.submit(now)...)
	// 2. observe.
	r.observeNodes(in, now)
	started := r.observePods(in.pods, now, nodes)
	// 3. GPU slots.
	r.startJobs(started, now, nodes, &act)
	// 4. progress, over the step that just ended: it reads the loads that
	// step ran with (r.loaded), never the next step's.
	r.progress(now, dt, &act)
	// 5. complete.
	cmds = append(cmds, r.complete(now, &act)...)
	// 6. load, once: progress changes nothing it reads, and complete frees
	// the jobs that leave it.
	r.computeLoads()
	// 7. energy.
	r.attribute(now, snap, nodes)
	// 8. stalls.
	r.stalls(now, &act)
	// 9. run end.
	r.kube.push(cmds...)
	if r.allTerminal() {
		r.endLocked(now, workload.PhaseFinished)
		return
	}
	r.rec.flushLogs()
	r.rec.maybeWriteMetrics(now, r.activeCounts(), false)
}

// applyResults takes the outcome of the calls of earlier ticks. It returns
// the completion calls to try again.
func (r *workloadRun) applyResults(results []kubeResult, now time.Time, act *tickActivity) []kubeCmd {
	var retry []kubeCmd
	for _, res := range results {
		switch res.cmd.kind {
		case cmdCreate:
			r.applyCreate(res.cmd.group, res.err, now)
		case cmdComplete, cmdDelete:
			j := res.cmd.job
			if j.state != v1Completing {
				continue
			}
			switch {
			case res.err == nil:
				r.completeJob(j, now)
				act.completed = true
			case apierrors.IsNotFound(res.err):
				r.endJob(j, now, v1Failed, semconv.ErrorTypePodDeleted)
			default:
				j.attempts++
				if j.attempts >= v1CompletionAttempts {
					log.Printf("warning: run %s: job %s: completion failed %d times: %v", r.id, j.sim.JobID, j.attempts, res.err)
					r.endJob(j, now, v1Failed, semconv.ErrorTypePatchFailed)
					continue
				}
				retry = append(retry, res.cmd)
			}
		}
	}
	return retry
}

// applyCreate takes the outcome of group g's create.
func (r *workloadRun) applyCreate(g *v1Group, err error, now time.Time) {
	g.queued = false
	adopted := apierrors.IsAlreadyExists(err)
	switch {
	case err == nil || adopted:
		g.created = true
		for _, j := range g.jobs {
			if j.state != v1Waiting {
				continue
			}
			j.state, j.submittedAt = v1Submitted, now
			r.rec.addSubmitted(j.class)
			r.s.jobSubmitted.WithLabelValues(j.class).Inc()
			if adopted {
				r.rec.event(semconv.EventSimJobAdopted, now,
					fmt.Sprintf("job %s: %s/%s already existed and was taken over; its work restarts from full", j.sim.JobID, g.namespace, g.name),
					j.attrs()...)
				continue
			}
			r.rec.event(semconv.EventSimJobSubmitted, now, fmt.Sprintf("job %s submitted as %s/%s", j.sim.JobID, g.namespace, g.name),
				j.attrs(otlpjson.Bool(semconv.AttrSimGang, j.sim.Gang))...)
		}
	case createRejected(err):
		log.Printf("warning: run %s: create %s/%s rejected: %v", r.id, g.namespace, g.name, err)
		for _, j := range g.jobs {
			if !j.state.terminal() {
				r.endJob(j, now, v1Failed, semconv.ErrorTypeCreateRejected)
			}
		}
	default:
		if msg := err.Error(); msg != r.lastCreateErr {
			log.Printf("warning: run %s: create %s/%s: %v; retried next tick (logged when it changes)", r.id, g.namespace, g.name, err)
			r.lastCreateErr = msg
		}
	}
}

// createRejected reports whether a create failed for good: the API server
// refused the object, rather than failing to answer.
func createRejected(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) ||
		apierrors.IsNotFound(err) || apierrors.IsRequestEntityTooLargeError(err) || apierrors.IsMethodNotSupported(err)
}

// submit queues the create of every group due at now, up to the per-tick
// cap.
func (r *workloadRun) submit(now time.Time) []kubeCmd {
	var cmds []kubeCmd
	for _, g := range r.groups {
		if g.queued || g.created || !r.due(g, now) || groupEnded(g) {
			continue
		}
		if !underCap(len(cmds), r.set.maxCreates) {
			break
		}
		g.queued = true
		cmds = append(cmds, kubeCmd{kind: cmdCreate, group: g, ns: g.namespace, name: g.name, obj: g.object})
	}
	return cmds
}

func groupEnded(g *v1Group) bool {
	for _, j := range g.jobs {
		if !j.state.terminal() {
			return false
		}
	}
	return true
}

// observeNodes takes the Nodes' labels and NodeTwins' classes.
func (r *workloadRun) observeNodes(in kubeIn, now time.Time) {
	for node, p := range in.profiles {
		o := r.nodeObs[node]
		o.powerProfile = p
		r.nodeObs[node] = o
	}
	for node, c := range in.classes {
		o := r.nodeObs[node]
		if o.class != c || o.classSince.IsZero() {
			o.class, o.classSince = c, now
		}
		r.nodeObs[node] = o
	}
}

// jobOfPod is the job pod o runs, nil for none of this run's.
func (r *workloadRun) jobOfPod(o podObs) *v1Job {
	if o.jobID != "" {
		if j := r.byID[o.jobID]; j != nil && !j.group.isJob {
			return j
		}
	}
	g := r.byObject[o.ns+"/"+o.jobName]
	if g == nil || !g.isJob || o.index < 0 || o.index >= len(g.jobs) {
		return nil
	}
	return g.jobs[o.index]
}

// observePods applies the pod changes, in key order, and returns the jobs
// whose pods became Running. The change of a job whose create result has
// not come back yet waits for the next tick.
func (r *workloadRun) observePods(changes map[string]podObs, now time.Time, nodes emuNodes) []*v1Job {
	for k, o := range r.deferred {
		if _, newer := changes[k]; !newer {
			changes[k] = o
		}
	}
	r.deferred = map[string]podObs{}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var started []*v1Job
	for _, k := range keys {
		o := changes[k]
		j := r.jobOfPod(o)
		switch {
		case j == nil || j.state.terminal():
			continue
		case j.state == v1Waiting:
			r.deferred[k] = o
			continue
		case j.podUID != "" && o.uid != "" && o.uid != j.podUID:
			// Another pod of the job, such as a replacement the Job
			// controller made after its pod went away: the job has
			// failed already, or the first pod still counts.
			continue
		}
		j.podName = o.name
		if o.uid != "" {
			j.podUID = o.uid
		}
		completing := j.state == v1Completing
		switch {
		case o.deleted:
			if !completing {
				r.endJob(j, now, v1Failed, semconv.ErrorTypePodDeleted)
			}
			continue
		case o.phase == corev1.PodFailed:
			if !completing {
				r.endJob(j, now, v1Failed, semconv.ErrorTypePodFailed)
			}
			continue
		case o.phase == corev1.PodSucceeded:
			// Succeeded before its work was done: something other than
			// the simulator ended it.
			if !completing {
				r.endJob(j, now, v1Failed, semconv.ErrorTypePodFailed)
			}
			continue
		}
		j.unschedulable = o.unschedulable && o.node == ""
		if o.node != "" && j.node == "" {
			r.scheduleJob(j, o.node, now, nodes)
		}
		if o.phase == corev1.PodRunning && (j.state == v1Submitted || j.state == v1Scheduled) && j.node != "" {
			started = append(started, j)
		}
	}
	return started
}

// scheduleJob records that job j's pod was bound to node at now.
func (r *workloadRun) scheduleJob(j *v1Job, node string, now time.Time, nodes emuNodes) {
	j.node, j.state, j.scheduledAt = node, v1Scheduled, now
	wait := math.Max(0, now.Sub(j.submittedAt).Seconds())
	r.rec.observe(r.rec.queueWait, recorderSecondsBounds, j.class, wait)
	extra := []otlpjson.KeyValue{otlpjson.String(semconv.AttrK8sNodeName, node)}
	o, seen := r.nodeObs[node]
	if seen && o.powerProfile != "" {
		extra = append(extra, otlpjson.String(semconv.AttrJoulieNodePowerProfile, o.powerProfile))
	}
	if seen && o.class != "" {
		extra = append(extra,
			otlpjson.String(semconv.AttrJoulieNodeSchedulableClass, o.class),
			otlpjson.Double(semconv.AttrJoulieNodeClassSince, math.Max(0, now.Sub(o.classSince).Seconds())))
	}
	if info, ok := nodes.emuNode(node); ok {
		extra = append(extra, otlpjson.String(semconv.AttrSimProfile, info.Profile))
	}
	extra = append(extra, otlpjson.Double(semconv.AttrSimJobQueueWait, wait))
	r.rec.event(semconv.EventSimJobScheduled, now, fmt.Sprintf("job %s scheduled on %s after %.1fs", j.sim.JobID, node, wait), j.attrs(extra...)...)
}

// node returns the run's view of node, made on first use. A node that is
// not emulated has no CPUs or GPUs to hold and no controls: its jobs run
// at full speed and load nothing.
func (r *workloadRun) node(name string, nodes emuNodes) *v1Node {
	if n := r.nodes[name]; n != nil {
		return n
	}
	info, ok := nodes.emuNode(name)
	if !ok {
		log.Printf("warning: run %s: node %s is not emulated; its jobs run at full speed and load no emulated hardware", r.id, name)
	}
	n := &v1Node{info: info, cpu: workload.NewCPUSlots(info.CPUs)}
	exposure, units := "", 0
	if info.TP != nil {
		exposure, units = info.TP.GPUExposure()
	}
	n.gpu = workload.NewGPUSlots(info.GPUs, exposure, units)
	r.nodes[name] = n
	return n
}

// startJobs moves the jobs whose pods became Running to v1Running: each
// takes its CPUs and GPU units on its node.
func (r *workloadRun) startJobs(started []*v1Job, now time.Time, nodes emuNodes, act *tickActivity) {
	for _, j := range started {
		n := r.node(j.node, nodes)
		n.cpu.Assign(j.sim.JobID, j.sim.RequestedCPUCores)
		var devices []int64
		if units := int(math.Ceil(j.sim.RequestedGPUs)); units > 0 {
			devs, ok := n.gpu.Assign(j.sim.JobID, units, j.sim.GPUUtilTarget)
			if !ok && n.info.TP != nil {
				log.Printf("warning: run %s: job %s asks for %d GPU units on %s, which has %d free of %d; it runs without GPU units",
					r.id, j.sim.JobID, units, j.node, r.freeGPUUnits(n), n.gpu.Units())
			}
			for _, d := range devs {
				devices = append(devices, int64(d))
			}
		}
		j.state, j.runningAt = v1Running, now
		act.started = true
		r.rec.event(semconv.EventSimJobStarted, now, fmt.Sprintf("job %s started on %s", j.sim.JobID, j.node),
			j.attrs(otlpjson.String(semconv.AttrK8sNodeName, j.node), otlpjson.IntArray(semconv.AttrSimGPUIndices, devices...))...)
	}
}

// freeGPUUnits counts node n's free GPU units, for a log line.
func (r *workloadRun) freeGPUUnits(n *v1Node) int {
	held := 0
	for _, j := range r.jobs {
		if j.state == v1Running && r.nodes[j.node] == n {
			for _, u := range n.gpu.Uses(j.sim.JobID) {
				held += u.Units
			}
		}
	}
	return n.gpu.Units() - held
}

// runningOn returns the Running jobs of each node, in run order.
func (r *workloadRun) runningOn() map[string][]*v1Job {
	out := map[string][]*v1Job{}
	for _, j := range r.jobs {
		if j.state == v1Running {
			out[j.node] = append(out[j.node], j)
		}
	}
	return out
}

// cpuUtilOf is the CPU util of each of jobs, the input of CPUSlots.
func cpuUtilOf(jobs []*v1Job) map[string]float64 {
	out := make(map[string]float64, len(jobs))
	for _, j := range jobs {
		out[j.sim.JobID] = j.sim.CPUUtilTarget
	}
	return out
}

// computeLoads sets each emulated node's load for the next step from the
// jobs Running on it, and the jobs the energy of that step goes to.
func (r *workloadRun) computeLoads() {
	loads := map[string]hwemu.Load{}
	next := map[string][]workload.JobLoad{}
	for node, jobs := range r.runningOn() {
		n := r.nodes[node]
		if n == nil || n.info.TP == nil {
			continue
		}
		cpuClass, gpuClass := map[string]float64{}, map[string]float64{}
		memW, memSum := 0.0, 0.0
		for _, j := range jobs {
			w := math.Max(0.1, j.sim.RequestedCPUCores)
			cpuClass[j.sim.CPUWorkClass] += w
			memW += w * clamp01(j.sim.MemoryIntensity)
			memSum += w
			if j.sim.RequestedGPUs > 0 {
				gw := math.Max(0.1, j.sim.RequestedGPUs)
				gpuClass[j.sim.GPUWorkClass] += gw
				memW += gw * clamp01(j.sim.MemoryIntensity)
				memSum += gw
			}
			next[node] = append(next[node], workload.JobLoad{
				ID: j.sim.JobID, Class: j.class, CPURequest: j.sim.RequestedCPUCores,
				CPUUtil: j.sim.CPUUtilTarget, GPUUtil: j.sim.GPUUtilTarget, GPUs: n.gpu.Uses(j.sim.JobID),
			})
		}
		l := hwemu.Load{
			CPUUtil:  n.cpu.Util(cpuUtilOf(jobs)),
			CPUClass: dominantWorkClass(cpuClass, "cpu.mixed"),
			GPUUtil:  n.gpu.DeviceUtil(),
			GPUClass: dominantWorkClass(gpuClass, "gpu.mixed"),
		}
		if memSum > 0 {
			l.MemoryIntensity = memW / memSum
		}
		l.PeerTransfers, _ = r.peerRing(next[node], n.info.PeerLinkBps)
		loads[node] = l
	}
	r.loads, r.nextLoaded = loads, next
}

// peerRater is what the engine reads of an emulated node whose jobs move
// data between its GPUs: the rate each transfer of the node's load got in
// the step that just ended, in load order (hwemu.Node.PeerTransferRates). A
// node without it slows no transfer.
type peerRater interface {
	PeerTransferRates() []float64
}

var _ peerRater = (*hwemu.Node)(nil)

// peerRing is the peer transfers of jobs, the jobs loading one node with a
// peer link of linkBps bytes per second, in job order, and the job of each.
// A job whose trace gives peerTransferIntensity p > 0 and that holds n >= 2
// GPUs sends a ring over its GPUs in ascending order, GPU k to the next and
// the last to the first, n transfers each demanding p x linkBps; two GPUs
// send to each other. A job on one GPU, and every job on a node without a
// peer link, sends none. computeLoads and progress both build the ring from
// the jobs of a step, so the transfers and the rates hwemu gives back for
// them line up.
func (r *workloadRun) peerRing(jobs []workload.JobLoad, linkBps float64) ([]hwemu.PeerTransfer, []string) {
	if linkBps <= 0 {
		return nil, nil
	}
	var out []hwemu.PeerTransfer
	var owner []string
	for _, jl := range jobs {
		j := r.byID[jl.ID]
		if j == nil || j.sim.PeerTransferIntensity <= 0 {
			continue
		}
		var devs []int
		for _, u := range jl.GPUs {
			if !slices.Contains(devs, u.Device) {
				devs = append(devs, u.Device)
			}
		}
		if len(devs) < 2 {
			continue
		}
		slices.Sort(devs)
		demand := clamp01(j.sim.PeerTransferIntensity) * linkBps
		for k, d := range devs {
			out = append(out, hwemu.PeerTransfer{Src: d, Dst: devs[(k+1)%len(devs)], BytesPerS: demand})
			owner = append(owner, jl.ID)
		}
	}
	return out, owner
}

// peerMuls is, for each job that sent peer transfers on node in the step
// that just ended, the rate its transfers got over the rate they demanded:
// the lowest over its ring, since a ring moves at its slowest link. A job
// not in it sent none. Without the node's rates, or with rates that do not
// match the step's transfers, every transfer counts at its demand.
func (r *workloadRun) peerMuls(node string, n *v1Node) map[string]float64 {
	ts, owner := r.peerRing(r.loaded[node], n.info.PeerLinkBps)
	if len(ts) == 0 {
		return nil
	}
	var rates []float64
	if pr, ok := n.info.TP.(peerRater); ok {
		rates = pr.PeerTransferRates()
	}
	out := make(map[string]float64, len(ts))
	for i, t := range ts {
		g := 1.0
		if len(rates) == len(ts) {
			g = clamp01(rates[i] / t.BytesPerS)
		}
		if prev, ok := out[owner[i]]; !ok || g < prev {
			out[owner[i]] = g
		}
	}
	return out
}

// gangHeld reports whether gang job j waits for the other pods of its
// gang: none of them progresses until every one runs.
func (r *workloadRun) gangHeld(j *v1Job) bool {
	if !j.sim.Gang || j.sim.WorkloadID == "" {
		return false
	}
	for _, m := range r.gangs[j.sim.WorkloadID] {
		if m.state != v1Running && m.state != v1Completing && m.state != v1Completed {
			return true
		}
	}
	return false
}

// progress advances every job that was Running over the step that just
// ended by its rates times dt.
func (r *workloadRun) progress(now time.Time, dt time.Duration, act *tickActivity) {
	secs := dt.Seconds()
	if secs <= 0 {
		return
	}
	for node, jobs := range r.runningOn() {
		n := r.nodes[node]
		cpuShare := n.cpu.Share(cpuUtilOf(jobs))
		peer := r.peerMuls(node, n)
		for _, j := range jobs {
			if !j.runningAt.Before(now) {
				continue
			}
			j.gangWait = r.gangHeld(j)
			if j.gangWait {
				continue
			}
			id := j.sim.JobID
			cpuMul, gpuMul := 1.0, 1.0
			if tp := n.info.TP; tp != nil {
				if cpus := n.cpu.CPUs(id); len(cpus) > 0 {
					cpuMul = tp.CPUThroughput(cpus, j.sim.CPUWorkClass, j.sim.MemoryIntensity, j.sim.IOIntensity)
				}
				for _, u := range n.gpu.Uses(id) {
					gpuMul = math.Min(gpuMul, tp.GPUThroughput(u.Device))
				}
			}
			in := workload.RateInputs{
				BaseSpeed: r.set.baseSpeed, CPURequest: j.sim.RequestedCPUCores, GPURequest: j.sim.RequestedGPUs,
				CPUUtil: j.sim.CPUUtilTarget, MemoryIntensity: j.sim.MemoryIntensity, IOIntensity: j.sim.IOIntensity,
				CPUFeedIntensity: j.sim.CPUFeedIntensity, SensitivityCPU: j.sim.SensitivityCPU, SensitivityGPU: j.sim.SensitivityGPU,
				CPUMul: cpuMul, GPUMul: gpuMul,
				CPUShare: 1, GPUShare: n.gpu.Share(id),
			}
			if n.cpu.Holds(id) {
				in.CPUShare = cpuShare
			}
			if g, ok := peer[id]; ok {
				in.PeerTransferIntensity, in.PeerMul = j.sim.PeerTransferIntensity, g
			}
			cpuRate, gpuRate := workload.Rates(workload.ModelV1, in)
			progressed := false
			if rem := j.sim.CPUUnitsRemaining; rem > 0 {
				if d := math.Min(rem, cpuRate*secs); d > 0 {
					j.sim.CPUUnitsRemaining -= d
					j.workCPU += d
					r.rec.addWork(j.class, semconv.WorkKindCPU, d)
					progressed = true
				}
			}
			if rem := j.sim.GPUUnitsRemaining; rem > 0 {
				if d := math.Min(rem, gpuRate*secs); d > 0 {
					j.sim.GPUUnitsRemaining -= d
					j.workGPU += d
					r.rec.addWork(j.class, semconv.WorkKindGPU, d)
					progressed = true
				}
			}
			if progressed {
				j.lastProgressAt = now
				act.progress = true
			}
		}
	}
}

// complete ends the work of every Running job with none left: it frees
// what the job holds and returns the completion calls.
func (r *workloadRun) complete(now time.Time, act *tickActivity) []kubeCmd {
	var cmds []kubeCmd
	for _, j := range r.jobs {
		if j.state != v1Running || j.sim.CPUUnitsRemaining > v1WorkEpsilon || j.sim.GPUUnitsRemaining > v1WorkEpsilon {
			continue
		}
		j.sim.CPUUnitsRemaining = math.Max(0, j.sim.CPUUnitsRemaining)
		j.sim.GPUUnitsRemaining = math.Max(0, j.sim.GPUUnitsRemaining)
		j.state, j.doneAt = v1Completing, now
		if j.stalled {
			r.rec.event(semconv.EventSimJobResumed, now, fmt.Sprintf("job %s resumed", j.sim.JobID),
				j.attrs(otlpjson.String(semconv.AttrSimStallReason, j.stallReason),
					otlpjson.Double(semconv.AttrSimStallAge, now.Sub(j.stallRef).Seconds()))...)
			j.stalled = false
		}
		r.release(j)
		act.completing = true
		kind := cmdComplete
		if r.set.completion == workload.CompletionDelete {
			kind = cmdDelete
		}
		cmds = append(cmds, kubeCmd{kind: kind, job: j, ns: j.group.namespace, name: j.podName, at: now})
	}
	return cmds
}

// attribute splits each emulated node's ledger growth over the step that
// just ended among the jobs that loaded it during that step.
func (r *workloadRun) attribute(now time.Time, snap *emulate.Snapshot, nodes emuNodes) {
	if snap != nil {
		for i := range snap.Nodes {
			sn := &snap.Nodes[i]
			info, ok := nodes.emuNode(sn.Name)
			if !ok {
				continue
			}
			n := r.node(sn.Name, nodes)
			cur := snapshotEnergy(sn, info, now, n.gpu.UnitsPerDevice())
			if prev, ok := r.energyPrev[sn.Name]; ok {
				a := workload.Attribute(prev, cur, r.loaded[sn.Name])
				for class, e := range a.Classes {
					r.rec.addEnergy(class, semconv.HwTypeCPU, sn.Name, info.Profile, e.CPUJ)
					r.rec.addEnergy(class, semconv.HwTypeGPU, sn.Name, info.Profile, e.GPUJ)
				}
				for id, e := range a.Jobs {
					if j := r.byID[id]; j != nil {
						j.energy.CPUJ += e.CPUJ
						j.energy.GPUJ += e.GPUJ
					}
				}
			}
			r.energyPrev[sn.Name] = cur
		}
	}
	r.loaded = r.nextLoaded
	r.nextLoaded = map[string][]workload.JobLoad{}
}

// stalls reports Running jobs that made no progress for the stall report
// interval, their resumption, and the run stall. Nothing is completed.
func (r *workloadRun) stalls(now time.Time, act *tickActivity) {
	report := r.set.stallReport
	submitted, blocked := false, true
	for _, j := range r.jobs {
		switch j.state {
		case v1Submitted:
			submitted = true
			if j.podName != "" && !j.unschedulable {
				blocked = false
			}
		case v1Scheduled, v1Completing:
			submitted, blocked = true, false
		case v1Running:
			submitted = true
			r.jobStall(j, now, report)
		}
	}
	if !submitted || !blocked || act.progress || act.started || act.completed || act.completing {
		if r.runStalled {
			r.rec.event(semconv.EventSimRunResumed, now, fmt.Sprintf("run %s resumed", r.id),
				otlpjson.String(semconv.AttrSimStallReason, semconv.StallReasonAllBlocked),
				otlpjson.Double(semconv.AttrSimStallAge, now.Sub(r.blockedSince).Seconds()))
		}
		r.runStalled, r.blockedSince = false, time.Time{}
		return
	}
	if r.blockedSince.IsZero() {
		r.blockedSince = now
	}
	if age := now.Sub(r.blockedSince); !r.runStalled && age >= report {
		r.runStalled = true
		r.rec.event(semconv.EventSimRunStalled, now,
			fmt.Sprintf("run %s stalled: every submitted job is blocked for %s", r.id, age.Round(time.Second)),
			otlpjson.String(semconv.AttrSimStallReason, semconv.StallReasonAllBlocked),
			otlpjson.Double(semconv.AttrSimStallAge, age.Seconds()))
		log.Printf("warning: run %s stalled: every submitted job is blocked for %s", r.id, age.Round(time.Second))
	}
}

// jobStall reports Running job j's stall or its end.
func (r *workloadRun) jobStall(j *v1Job, now time.Time, report time.Duration) {
	if j.lastProgressAt.Equal(now) {
		if j.stalled {
			r.rec.event(semconv.EventSimJobResumed, now, fmt.Sprintf("job %s resumed", j.sim.JobID),
				j.attrs(otlpjson.String(semconv.AttrSimStallReason, j.stallReason),
					otlpjson.Double(semconv.AttrSimStallAge, now.Sub(j.stallRef).Seconds()))...)
			j.stalled = false
		}
		return
	}
	if j.stalled || !j.runningAt.Before(now) {
		return
	}
	ref := j.lastProgressAt
	if ref.IsZero() {
		ref = j.runningAt
	}
	if age := now.Sub(ref); age >= report {
		reason := semconv.StallReasonNoProgress
		if j.gangWait {
			reason = semconv.StallReasonGangIncomplete
		}
		j.stalled, j.stallReason, j.stallRef = true, reason, ref
		r.rec.event(semconv.EventSimJobStalled, now, fmt.Sprintf("job %s stalled: %s for %s", j.sim.JobID, reason, age.Round(time.Second)),
			j.attrs(otlpjson.String(semconv.AttrSimStallReason, reason), otlpjson.Double(semconv.AttrSimStallAge, age.Seconds()))...)
	}
}

// allTerminal reports whether every job of the run is terminal.
func (r *workloadRun) allTerminal() bool {
	for _, j := range r.jobs {
		if !j.state.terminal() {
			return false
		}
	}
	return true
}
