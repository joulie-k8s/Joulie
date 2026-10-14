package main

// Tests of the governor control over synthetic sysfs trees and fake tools.
// The clock values are neutral, not those of any machine.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// govRig is one node under the governor control: the Node and its NodeTwin
// in a reader the test edits, the dynamic client the agent writes through,
// the agent's controller and a clock the test moves.
type govRig struct {
	t      *testing.T
	name   string
	reader client.Client
	dyn    *dynamicfake.FakeDynamicClient
	nc     *NodeController
	clock  time.Time
}

// forgetWrites drops what the agent remembers having written for the node,
// so a test does not depend on whatever ran before it in this process.
func forgetWrites(name string) {
	obj := sanitizeNodeObjectName(name)
	lastWrites.Delete("nodeHardware|" + obj)
	for _, c := range []string{"cpu", "gpu", componentCPUGovernor, componentGPUGovernor} {
		lastWrites.Delete("controlStatus|" + obj + "|" + c)
	}
}

// newGovRig sets env (GOVERNOR_* and the telemetry variables) and builds
// the clients and the controller for node, with a NodeTwin carrying cpu and
// gpu. Call it after pointing the agent at its tree.
func newGovRig(t *testing.T, node *corev1.Node, env map[string]string, cpu *v1alpha1.CPUGovernorIntent, gpu *v1alpha1.GPUGovernorIntent) *govRig {
	t.Helper()
	for _, k := range []string{"GOVERNOR_WRITES", "GOVERNOR_RESTORE_ON_EXIT", "TELEMETRY_CPU_SOURCE", "TELEMETRY_CPU_CONTROL", "TELEMETRY_GPU_CONTROL"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	forgetWrites(node.Name)
	twin := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: sanitizeNodeObjectName(node.Name)},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: node.Name, Profile: "eco"},
	}
	setGovernorIntents(twin, cpu, gpu)
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	r := &govRig{
		t:      t,
		name:   node.Name,
		reader: crfake.NewClientBuilder().WithScheme(s).WithObjects(node, twin).Build(),
		dyn:    dynamicfake.NewSimpleDynamicClient(s, node.DeepCopy(), twin.DeepCopy()),
		clock:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	r.restart()
	return r
}

// restart builds a new controller, as a restarted agent does.
func (r *govRig) restart() {
	r.t.Helper()
	nc, err := newNodeController(r.name, false)
	if err != nil {
		r.t.Fatal(err)
	}
	nc.governor.now = func() time.Time { return r.clock }
	r.nc = nc
}

func setGovernorIntents(twin *v1alpha1.NodeTwin, cpu *v1alpha1.CPUGovernorIntent, gpu *v1alpha1.GPUGovernorIntent) {
	twin.Spec.CPU, twin.Spec.GPU = nil, nil
	if cpu != nil {
		twin.Spec.CPU = &v1alpha1.NodeTwinCPU{Governor: cpu}
	}
	if gpu != nil {
		twin.Spec.GPU = &v1alpha1.NodeTwinGPU{Governor: gpu}
	}
}

// setIntents replaces the governor intents of the NodeTwin, as the
// controller manager does.
func (r *govRig) setIntents(cpu *v1alpha1.CPUGovernorIntent, gpu *v1alpha1.GPUGovernorIntent) {
	r.t.Helper()
	twin := &v1alpha1.NodeTwin{}
	if err := r.reader.Get(context.Background(), client.ObjectKey{Name: sanitizeNodeObjectName(r.name)}, twin); err != nil {
		r.t.Fatal(err)
	}
	setGovernorIntents(twin, cpu, gpu)
	if err := r.reader.Update(context.Background(), twin); err != nil {
		r.t.Fatal(err)
	}
}

// syncStatus copies the NodeTwin status the agent wrote into the reader,
// as the informer cache would see it.
func (r *govRig) syncStatus() {
	r.t.Helper()
	obj, err := r.dyn.Resource(nodeTwinGVR).Get(context.Background(), sanitizeNodeObjectName(r.name), metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	var written v1alpha1.NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &written); err != nil {
		r.t.Fatal(err)
	}
	twin := &v1alpha1.NodeTwin{}
	if err := r.reader.Get(context.Background(), client.ObjectKey{Name: sanitizeNodeObjectName(r.name)}, twin); err != nil {
		r.t.Fatal(err)
	}
	twin.Status = written.Status
	if err := r.reader.Update(context.Background(), twin); err != nil {
		r.t.Fatal(err)
	}
}

// reconcile runs one reconcile at the rig's clock, then moves the clock by
// one reconcile interval.
func (r *govRig) reconcile() {
	r.t.Helper()
	if err := reconcileOnce(context.Background(), r.reader, r.dyn, r.nc); err != nil {
		r.t.Fatalf("reconcileOnce: %v", err)
	}
	r.clock = r.clock.Add(20 * time.Second)
}

// status is status.controlStatus.<component> of the NodeTwin, typed; nil
// when absent.
func (r *govRig) status(component string) *v1alpha1.ControlResult {
	r.t.Helper()
	m := controlStatus(r.t, r.dyn, r.name, component)
	if m == nil {
		return nil
	}
	var res v1alpha1.ControlResult
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &res); err != nil {
		r.t.Fatal(err)
	}
	return &res
}

// hardware is the NodeHardware status the agent published, typed.
func (r *govRig) hardware() v1alpha1.NodeHardwareStatus {
	r.t.Helper()
	obj, err := r.dyn.Resource(nodeHardwareGVR).Get(context.Background(), sanitizeNodeObjectName(r.name), metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	var nh v1alpha1.NodeHardware
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &nh); err != nil {
		r.t.Fatal(err)
	}
	return nh.Status
}

// writeLog records every attribute write of the agent, and can fail some.
type writeLog struct {
	mu     sync.Mutex
	writes []string // "<path relative to the tree>=<value>"
	fail   func(path string) error
}

// watchWrites wraps writeSysfsAttr for the test; paths are reported
// relative to root.
func watchWrites(t *testing.T, root string) *writeLog {
	t.Helper()
	l := &writeLog{}
	old := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error {
		rel, _ := filepath.Rel(root, path)
		l.mu.Lock()
		l.writes = append(l.writes, filepath.ToSlash(rel)+"="+value)
		fail := l.fail
		l.mu.Unlock()
		if fail != nil {
			if err := fail(path); err != nil {
				return err
			}
		}
		return old(path, value)
	}
	t.Cleanup(func() { writeSysfsAttr = old })
	return l
}

func (l *writeLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.writes
	l.writes = nil
	return out
}

// policyGovernors is scaling_governor of policies 0 to 3 of the tree.
func policyGovernors(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for p := 0; p < 4; p++ {
		out = append(out, readTrimmed(t, filepath.Join(root, fmt.Sprintf("devices/system/cpu/cpufreq/policy%d/scaling_governor", p))))
	}
	return out
}

func allEqual(xs []string, want string) bool {
	for _, x := range xs {
		if x != want {
			return false
		}
	}
	return len(xs) > 0
}

func plainNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

var (
	efficiency = &v1alpha1.CPUGovernorIntent{Preset: joulie.CPUGovernorPresetEfficiency}
	governorOn = map[string]string{"GOVERNOR_WRITES": "true"}
)

// GA19: with GOVERNOR_WRITES false and a governor spec present, the agent
// changes no file and reports disabled.
func TestGA19WritesDisabledChangeNothing(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	r := newGovRig(t, plainNode("ga19-disabled"), nil, efficiency, nil)

	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("the agent wrote %v with GOVERNOR_WRITES unset", w)
	}
	if got := policyGovernors(t, root); !allEqual(got, "schedutil") {
		t.Errorf("scaling_governor %v, want schedutil everywhere", got)
	}
	if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultDisabled {
		t.Errorf("cpuGovernor status %+v, want result disabled", s)
	}
}

// GA20: the baseline is in NodeHardware before the first governor write,
// and a restarted agent keeps it: it does not record the state it set.
func TestGA20BaselineRecordedBeforeTheFirstWriteAndKeptAcrossARestart(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	r := newGovRig(t, plainNode("ga20-baseline"), governorOn, efficiency, nil)
	want := []v1alpha1.CPUGovernorState{{Policies: "0-3", Governor: "schedutil", Boost: joulie.CPUGovernorBoostOn}}
	var atFirstWrite []v1alpha1.CPUGovernorState
	writes.fail = func(string) error {
		if atFirstWrite == nil {
			if g := r.hardware().CPU.Governor; g != nil && g.Baseline != nil {
				atFirstWrite = g.Baseline.Groups
			} else {
				atFirstWrite = []v1alpha1.CPUGovernorState{}
			}
		}
		return nil
	}

	r.reconcile()
	if !reflect.DeepEqual(atFirstWrite, want) {
		t.Fatalf("baseline in NodeHardware at the first write %+v, want %+v", atFirstWrite, want)
	}
	if got := policyGovernors(t, root); !allEqual(got, "conservative") {
		t.Fatalf("scaling_governor %v, want conservative", got)
	}

	r.syncStatus()
	r.restart()
	r.reconcile()
	g := r.hardware().CPU.Governor
	if g == nil || g.Current != "conservative" || g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Groups, want) {
		t.Fatalf("after a restart cpu.governor %+v, want current conservative and the baseline %+v kept", g, want)
	}

	// The restarted agent knows from its last status that it changed the
	// governor, and restores the stored baseline.
	r.setIntents(nil, nil)
	r.reconcile()
	if got := policyGovernors(t, root); !allEqual(got, "schedutil") {
		t.Fatalf("scaling_governor %v after the field went away, want the baseline schedutil", got)
	}
}

// GA21: removing the field restores the baseline once, result restored;
// the next reconcile writes nothing.
func TestGA21FieldRemovedRestoresOnce(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	r := newGovRig(t, plainNode("ga21-restore"), governorOn, efficiency, nil)
	r.reconcile()
	r.reconcile()
	writes.take()

	r.setIntents(nil, nil)
	r.reconcile()
	if got := policyGovernors(t, root); !allEqual(got, "schedutil") {
		t.Fatalf("scaling_governor %v, want schedutil restored", got)
	}
	s := r.status(componentCPUGovernor)
	if s == nil || s.Result != joulie.GovernorResultRestored || s.Applied != "schedutil" {
		t.Fatalf("cpuGovernor status %+v, want restored, applied schedutil", s)
	}
	if w := writes.take(); len(w) != 4 {
		t.Errorf("restore wrote %v, want the four scaling_governor files only (boost was never written)", w)
	}
	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("a second reconcile without the field wrote %v; the restore happens once", w)
	}
}

// GA22: SIGTERM ends the agent's context, and the exit restores the
// baseline with GOVERNOR_RESTORE_ON_EXIT true and not with false.
func TestGA22SigtermRestoresOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		restore string
		want    string
	}{{"true", "schedutil"}, {"false", "conservative"}} {
		t.Run("restore="+tc.restore, func(t *testing.T) {
			root := acpiCPUFreqTree(t)
			pointAgentAtSysfs(t, root, "", nil)
			r := newGovRig(t, plainNode("ga22-exit-"+tc.restore), map[string]string{"GOVERNOR_WRITES": "true", "GOVERNOR_RESTORE_ON_EXIT": tc.restore}, efficiency, nil)
			r.reconcile()
			if got := policyGovernors(t, root); !allEqual(got, "conservative") {
				t.Fatalf("scaling_governor %v, want conservative before the signal", got)
			}

			ctx, stop := signalContext()
			defer stop()
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("SIGTERM did not end the agent's context")
			}
			restoreGovernorsOnExit(r.dyn, r.nc)
			if got := policyGovernors(t, root); !allEqual(got, tc.want) {
				t.Fatalf("scaling_governor %v after SIGTERM, want %s", got, tc.want)
			}
		})
	}
}

// GA23: an external change is a drift: re-applied once 5 min have passed
// since the last write; the third drift within 30 min gives contended, and
// the agent stops writing.
func TestGA23DriftIsReappliedThenContended(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	r := newGovRig(t, plainNode("ga23-drift"), governorOn, efficiency, nil)
	policy2 := filepath.Join(root, "devices/system/cpu/cpufreq/policy2/scaling_governor")
	r.reconcile() // apply
	r.reconcile() // verified

	for drift := 1; drift <= 3; drift++ {
		if err := os.WriteFile(policy2, []byte("schedutil\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writes.take()
		r.reconcile()
		s := r.status(componentCPUGovernor)
		if drift == 3 {
			if s == nil || s.Result != joulie.GovernorResultContended {
				t.Fatalf("third drift: status %+v, want contended", s)
			}
			break
		}
		if s == nil || s.Result != joulie.GovernorResultDrift {
			t.Fatalf("drift %d: status %+v, want drift", drift, s)
		}
		if w := writes.take(); len(w) != 0 {
			t.Fatalf("drift %d re-applied at once (%v); the agent waits 5 min after its last write", drift, w)
		}
		r.clock = r.clock.Add(governorReapplyEvery)
		r.reconcile()
		if got := readTrimmed(t, policy2); got != "conservative" {
			t.Fatalf("drift %d: policy2 %q after 5 min, want conservative re-applied", drift, got)
		}
		if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultApplied {
			t.Fatalf("drift %d: status %+v after the re-apply, want applied", drift, s)
		}
		r.reconcile() // verified
	}
	writes.take()
	r.clock = r.clock.Add(10 * time.Minute)
	r.reconcile()
	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("a contended component was written: %v", w)
	}
	if got := readTrimmed(t, policy2); got != "schedutil" {
		t.Errorf("policy2 %q, want the other writer's schedutil left alone", got)
	}
}

// GA24: a write refused with EACCES gives refused, NodeHardware writable
// false, and no retry.
func TestGA24RefusedWriteIsNotRetried(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	writes.fail = func(path string) error {
		return &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	r := newGovRig(t, plainNode("ga24-refused"), governorOn, efficiency, nil)

	r.reconcile()
	if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultRefused || !strings.Contains(s.Message, "EACCES") {
		t.Fatalf("cpuGovernor status %+v, want refused naming EACCES", s)
	}
	if w := writes.take(); len(w) != 1 {
		t.Fatalf("writes %v, want the one refused write", w)
	}
	r.clock = r.clock.Add(time.Hour)
	r.reconcile()
	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("a refused component was written again: %v", w)
	}
	if g := r.hardware().CPU.Governor; g == nil || g.Writable {
		t.Errorf("cpu.governor %+v, want writable false after a refusal", g)
	}
}

// GA25: the governor status patch touches status.controlStatus.<component>
// only, carries the native setting in applied, is skipped when unchanged,
// and removes the entry with null.
func TestGA25GovernorStatusPatchTouchesOnlyItsSubtree(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), nodeTwinObject("ga25-shape", "ga25-shape"))
	forgetWrites("ga25-shape")
	var patches []map[string]any
	dyn.PrependReactor("patch", "nodetwins", func(action clienttesting.Action) (bool, runtime.Object, error) {
		var p map[string]any
		if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &p); err != nil {
			t.Fatal(err)
		}
		patches = append(patches, p)
		return false, nil, nil
	})
	st := &governorStatus{backend: joulie.GovernorBackendCPUFreq, result: joulie.GovernorResultApplied, applied: "conservative"}
	for i := 0; i < 2; i++ {
		if err := updateNodeTwinGovernorStatus(context.Background(), dyn, "ga25-shape", componentCPUGovernor, st); err != nil {
			t.Fatal(err)
		}
	}
	if len(patches) != 1 {
		t.Fatalf("%d patches for two identical statuses, want 1", len(patches))
	}
	entry := onlyControlStatusEntry(t, patches[0], componentCPUGovernor)
	if entry["applied"] != "conservative" || entry["result"] != joulie.GovernorResultApplied || entry["backend"] != joulie.GovernorBackendCPUFreq {
		t.Errorf("cpuGovernor entry %v, want backend cpufreq, result applied, applied conservative", entry)
	}

	if err := updateNodeTwinGovernorStatus(context.Background(), dyn, "ga25-shape", componentCPUGovernor, nil); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 2 {
		t.Fatalf("%d patches, want the removal patched", len(patches))
	}
	cs := patches[1]["status"].(map[string]any)["controlStatus"].(map[string]any)
	if v, ok := cs[componentCPUGovernor]; !ok || v != nil || len(cs) != 1 {
		t.Errorf("removal patch controlStatus %v, want only cpuGovernor: null", cs)
	}
}

// onlyControlStatusEntry checks that patch holds status.controlStatus with
// component alone, and returns its entry.
func onlyControlStatusEntry(t *testing.T, patch map[string]any, component string) map[string]any {
	t.Helper()
	if len(patch) != 1 || patch["status"] == nil {
		t.Fatalf("patch touches keys other than status: %v", patch)
	}
	status := patch["status"].(map[string]any)
	if len(status) != 1 || status["controlStatus"] == nil {
		t.Fatalf("patch touches status fields other than controlStatus: %v", status)
	}
	cs := status["controlStatus"].(map[string]any)
	if len(cs) != 1 || cs[component] == nil {
		t.Fatalf("patch touches components other than %s: %v", component, cs)
	}
	return cs[component].(map[string]any)
}

// GA26: joulie_agent_governor_writes_total counts one per write, with its
// preset and result: an apply, nothing for a reconcile that only checks,
// and the restore under preset baseline.
func TestGA26WritesCounterCountsEachWrite(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	const node = "ga26-counter"
	r := newGovRig(t, plainNode(node), governorOn, efficiency, nil)
	writes, state := governorMetrics()
	count := func(preset, result string) float64 {
		return testutil.ToFloat64(writes.WithLabelValues(node, "cpu", preset, result))
	}

	r.reconcile()
	r.reconcile()
	if got := count(joulie.CPUGovernorPresetEfficiency, joulie.GovernorResultApplied); got != 1 {
		t.Errorf("writes_total{efficiency,applied} = %v after an apply and a check, want 1", got)
	}
	if got := testutil.ToFloat64(state.WithLabelValues(node, "cpu", joulie.CPUGovernorPresetEfficiency)); got != 1 {
		t.Errorf("governor_state{efficiency} = %v, want 1", got)
	}
	r.setIntents(nil, nil)
	r.reconcile()
	r.reconcile()
	if got := count(governorRestorePreset, joulie.GovernorResultRestored); got != 1 {
		t.Errorf("writes_total{baseline,restored} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(state.WithLabelValues(node, "cpu", joulie.CPUGovernorPresetEfficiency)); got != 0 {
		t.Errorf("governor_state{efficiency} = %v after the restore, want 0", got)
	}
}

// GA28: NVIDIA auto with no lock in the agent's record issues no reset and
// is equivalent; after the agent's own -lgc, auto issues -rgc once per GPU.
func TestGA28NvidiaAutoResetsOnlyTheAgentsLocks(t *testing.T) {
	g := emulatedGovernorNode(t, "nvidia-h100-sxm-4gpu", nil)
	e, p := g.e, g.p
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetAuto})

	r.reconcile()
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultEquivalent {
		t.Fatalf("auto without a record: status %+v, want equivalent", s)
	}
	if c := lockCommands(e, "-rgc", "-rmc", "-lgc", "-lmc"); len(c) != 0 {
		t.Fatalf("auto without a record ran %v; a reset would clear an operator's lock", c)
	}
	r.setIntents(nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})
	r.reconcile()
	if c := lockCommands(e, "-lgc"); len(c) != p.GPUs.Count.V {
		t.Fatalf("efficient ran %v, want one -lgc per GPU", c)
	}
	r.setIntents(nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetAuto})
	r.reconcile()
	r.reconcile()
	if c := lockCommands(e, "-rgc"); len(c) != p.GPUs.Count.V {
		t.Fatalf("auto after the agent's lock ran %v, want -rgc once per GPU", c)
	}
	if c := lockCommands(e, "-rmc"); len(c) != 0 {
		t.Errorf("auto ran %v; the agent locked no memory clock", c)
	}
}

// lockCommands lists the nvidia-smi commands run since the last call that
// carry one of the flags, and forgets them.
func lockCommands(e *emulatedNode, flags ...string) []string {
	var out []string
	defer e.runner.reset()
	for _, c := range e.runner.commands() {
		for _, f := range flags {
			if strings.Contains(c, " "+f) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// GA29: in the HTTP control mode a governor spec gives unsupported, and the
// agent sends no request for it.
func TestGA29HTTPControlModeHasNoGovernorAction(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	r := newGovRig(t, plainNode("ga29-http"), map[string]string{
		"GOVERNOR_WRITES":                     "true",
		"TELEMETRY_CPU_CONTROL":               "http",
		"TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT": srv.URL + "/control/{node}",
		"TELEMETRY_GPU_CONTROL":               "http",
		"TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT": srv.URL + "/control/{node}",
	}, efficiency, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})

	r.reconcile()
	for _, c := range []string{componentCPUGovernor, componentGPUGovernor} {
		if s := r.status(c); s == nil || s.Result != joulie.GovernorResultUnsupported {
			t.Errorf("%s status %+v, want unsupported in the HTTP control mode", c, s)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d HTTP requests for a governor-only spec, want none", n)
	}
	if w := writes.take(); len(w) != 0 {
		t.Errorf("the agent wrote %v in the HTTP control mode", w)
	}
}

// With CPU_GOVERNOR_ECO_BOOST=off a node that goes back to performance gets
// the governor without boost; the agent puts the baseline boost back
// instead of leaving it off.
func TestGovernorBoostReturnsToBaselineWhenItsFieldDisappears(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	boost := filepath.Join(root, "devices/system/cpu/cpufreq/boost")
	r := newGovRig(t, plainNode("gov-boost-back"), governorOn,
		&v1alpha1.CPUGovernorIntent{Preset: joulie.CPUGovernorPresetEfficiency, Boost: joulie.CPUGovernorBoostOff}, nil)
	r.reconcile()
	if got := readTrimmed(t, boost); got != "0" {
		t.Fatalf("cpufreq/boost %q with boost off, want 0", got)
	}
	r.setIntents(&v1alpha1.CPUGovernorIntent{Preset: joulie.CPUGovernorPresetPerformance}, nil)
	r.reconcile()
	if got := readTrimmed(t, boost); got != "1" {
		t.Fatalf("cpufreq/boost %q after the boost field went away, want the baseline 1", got)
	}
	if got := policyGovernors(t, root); !allEqual(got, "performance") {
		t.Errorf("scaling_governor %v, want performance", got)
	}
}

// A GPU discovery that comes back empty for one reconcile keeps the stored
// baseline, so the next discovery does not take the level the agent set for
// the baseline.
func TestGovernorGPUBaselineSurvivesAnEmptyDiscovery(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdDevice(files, "0000:10:00.0", "auto")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)
	node := plainNode("gov-gpu-nil")
	node.Labels = map[string]string{"feature.node.kubernetes.io/pci-1002.present": "true"}
	sustained := &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained}
	r := newGovRig(t, node, governorOn, nil, sustained)
	level := filepath.Join(root, "bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level")
	want := []v1alpha1.GPUGovernorDeviceState{{BusID: "0000:10:00.0", Level: "auto"}}

	r.reconcile()
	if got := readTrimmed(t, level); got != "high" {
		t.Fatalf("level %q, want high for sustained", got)
	}
	// One reconcile where the devices cannot be read.
	hidden := filepath.Join(root, "bus-hidden")
	if err := os.Rename(filepath.Join(root, "bus"), hidden); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if g := r.hardware().GPU.Governor; g == nil || g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Devices, want) {
		t.Fatalf("gpu.governor %+v during the empty discovery, want the baseline %+v kept", g, want)
	}
	if err := os.Rename(hidden, filepath.Join(root, "bus")); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if g := r.hardware().GPU.Governor; g == nil || g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Devices, want) {
		t.Fatalf("gpu.governor %+v after the devices came back, want the baseline %+v, not the level the agent set", g, want)
	}
	r.setIntents(nil, nil)
	r.reconcile()
	if got := readTrimmed(t, level); got != "auto" {
		t.Fatalf("level %q after the field went away, want the baseline auto", got)
	}
}

// An agent that restarts after its NodeHardware was lost, with the last
// status saying it set a level, takes the vendor default auto as the GPU
// baseline, not the level it set itself.
func TestGovernorLostNodeHardwareTakesTheVendorDefault(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdDevice(files, "0000:10:00.0", "auto")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)
	node := plainNode("gov-lost-nh")
	node.Labels = map[string]string{"feature.node.kubernetes.io/pci-1002.present": "true"}
	r := newGovRig(t, node, governorOn, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	level := filepath.Join(root, "bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level")

	r.reconcile()
	if got := readTrimmed(t, level); got != "high" {
		t.Fatalf("level %q, want high for sustained", got)
	}
	r.syncStatus()
	if err := r.dyn.Resource(nodeHardwareGVR).Delete(context.Background(), sanitizeNodeObjectName(r.name), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.setIntents(nil, nil)
	r.reconcile()
	want := []v1alpha1.GPUGovernorDeviceState{{BusID: "0000:10:00.0", Level: "auto"}}
	if g := r.hardware().GPU.Governor; g == nil || g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Devices, want) {
		t.Errorf("gpu.governor %+v, want the vendor default baseline %+v", g, want)
	}
	if got := readTrimmed(t, level); got != "auto" {
		t.Errorf("level %q after the field went away, want the vendor default auto", got)
	}
}

// A governor status written by an earlier run of the agent is removed with
// the field, as one written by this run is.
func TestGovernorStatusOfAnEarlierRunIsRemovedWithTheField(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	r := newGovRig(t, plainNode("gov-stale-status"), nil, efficiency, nil)
	r.reconcile()
	if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultDisabled {
		t.Fatalf("cpuGovernor status %+v, want disabled", s)
	}
	r.syncStatus()
	r.restart()
	r.setIntents(nil, nil)
	r.reconcile()
	if s := r.status(componentCPUGovernor); s != nil {
		t.Errorf("cpuGovernor status %+v after the field went away, want none", s)
	}
}

// An agent restarted with GOVERNOR_WRITES false after an earlier run
// changed the governor puts the baseline back once, with the field still in
// the spec and with it gone, and then writes nothing: the restore is the
// only write a discovery-only agent makes.
func TestGovernorWritesTurnedOffRestoresTheBaselineOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field bool
	}{{"field-kept", true}, {"field-removed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := acpiCPUFreqTree(t)
			pointAgentAtSysfs(t, root, "", nil)
			writes := watchWrites(t, root)
			r := newGovRig(t, plainNode("gov-writes-off-"+tc.name), governorOn, efficiency, nil)
			r.reconcile()
			if got := policyGovernors(t, root); !allEqual(got, "conservative") {
				t.Fatalf("scaling_governor %v with writes on, want conservative", got)
			}
			r.syncStatus()
			writes.take()

			t.Setenv("GOVERNOR_WRITES", "false")
			r.restart()
			if !tc.field {
				r.setIntents(nil, nil)
			}
			r.reconcile()
			if got := policyGovernors(t, root); !allEqual(got, "schedutil") {
				t.Fatalf("scaling_governor %v after GOVERNOR_WRITES went false, want the baseline schedutil", got)
			}
			if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultRestored {
				t.Fatalf("cpuGovernor status %+v, want restored", s)
			}
			if w := writes.take(); len(w) != 4 {
				t.Errorf("restore wrote %v, want the four scaling_governor files only", w)
			}

			r.syncStatus()
			r.reconcile()
			r.reconcile()
			if w := writes.take(); len(w) != 0 {
				t.Errorf("reconciles after the restore wrote %v; with GOVERNOR_WRITES false the restore is the only write", w)
			}
			s := r.status(componentCPUGovernor)
			switch {
			case tc.field && (s == nil || s.Result != joulie.GovernorResultDisabled):
				t.Errorf("cpuGovernor status %+v with the field kept, want disabled after the restore", s)
			case !tc.field && s != nil && s.Result != joulie.GovernorResultRestored:
				t.Errorf("cpuGovernor status %+v with the field gone, want restored or none", s)
			}
		})
	}
}

// The GPU side of TestGovernorWritesTurnedOffRestoresTheBaselineOnce. An
// AMD level an earlier run set goes back to the baseline once when the agent
// restarts with GOVERNOR_WRITES false and the field still in the spec, and
// later reconciles write nothing and report disabled.
func TestGovernorWritesTurnedOffRestoresTheGPUBaselineOnce(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdDevice(files, "0000:10:00.0", "auto")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)
	writes := watchWrites(t, root)
	node := plainNode("gov-gpu-writes-off")
	node.Labels = map[string]string{"feature.node.kubernetes.io/pci-1002.present": "true"}
	r := newGovRig(t, node, governorOn, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	level := filepath.Join(root, "bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level")

	r.reconcile()
	if got := readTrimmed(t, level); got != "high" {
		t.Fatalf("level %q with writes on, want high for sustained", got)
	}
	r.syncStatus()
	writes.take()

	t.Setenv("GOVERNOR_WRITES", "false")
	r.restart()
	r.reconcile()
	if got := readTrimmed(t, level); got != "auto" {
		t.Fatalf("level %q after GOVERNOR_WRITES went false, want the baseline auto", got)
	}
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultRestored {
		t.Fatalf("gpuGovernor status %+v, want restored", s)
	}
	if w := writes.take(); len(w) == 0 {
		t.Errorf("the restore wrote nothing")
	}

	r.syncStatus()
	r.reconcile()
	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("reconciles after the restore wrote %v; with GOVERNOR_WRITES false the restore is the only write", w)
	}
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultDisabled {
		t.Errorf("gpuGovernor status %+v, want disabled after the restore", s)
	}
}

// On an NVIDIA node whose agent has no working nvidia-smi, a GPU preset is
// an error, as the cap path reports, not unsupported.
func TestGovernorNvidiaWithoutTheToolIsAnError(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	node := plainNode("gov-no-smi")
	node.Labels = map[string]string{
		"feature.node.kubernetes.io/pci-10de.present": "true",
		"nvidia.com/gpu.compute.major":                "9",
		"nvidia.com/gpu.compute.minor":                "0",
	}
	r := newGovRig(t, node, governorOn, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})
	r.reconcile()
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultError || !strings.Contains(s.Message, "nvidia-smi") {
		t.Errorf("gpuGovernor status %+v, want error naming nvidia-smi", s)
	}
}

func TestCPUListHas(t *testing.T) {
	for _, tc := range []struct {
		list string
		id   int
		want bool
	}{
		{"0-3", 3, true}, {"0-3", 4, false}, {"0,2,4-5", 2, true}, {"0,2,4-5", 3, false},
		{"7", 7, true}, {"", 0, false}, {"3-1", 2, false}, {"x,1", 1, true},
		{"0-99999999999", 5, true},
	} {
		if got := cpuListHas(tc.list, tc.id); got != tc.want {
			t.Errorf("cpuListHas(%q, %d) = %v, want %v", tc.list, tc.id, got, tc.want)
		}
	}
}

// A stored baseline whose policy range is far wider than the node maps to
// the node's policies only, without allocating the range: the list comes
// from a CR, and "0-1000000" must not make the agent allocate a million
// entries.
func TestCPUBaselineGroupsNeverExpandTheStoredRange(t *testing.T) {
	d := &cpuGovernorDiscovery{Facts: governor.CPUFacts{Policies: []int{0, 1}}}
	b := &v1alpha1.CPUGovernorBaseline{Groups: []v1alpha1.CPUGovernorState{{Policies: "0-1000000", Governor: "schedutil"}}}
	var groups []governor.CPUGroup
	allocs := testing.AllocsPerRun(1, func() {
		groups = cpuBaselineGroups(d, b)
	})
	if len(groups) != 1 || len(groups[0].Policies) != 2 || allocs > 20 {
		t.Fatalf("got groups %+v with %.0f allocations, want one group of 2 policies and no allocation per listed policy", groups, allocs)
	}
}

func TestODSclkMax(t *testing.T) {
	text := "OD_SCLK:\n0: 500Mhz\n1: 1575Mhz\nOD_MCLK:\n0: 900Mhz\n1: 1300Mhz\n"
	if got, ok := odSclkMax(text); !ok || got != 1575 {
		t.Errorf("odSclkMax = %d, %v, want 1575", got, ok)
	}
	if _, ok := odSclkMax("s 1 1575"); ok {
		t.Error("odSclkMax read a command as the table")
	}
}
