package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// failingInventory runs every command on inner, but fails the nvidia-smi
// GPU inventory query while fail is set.
type failingInventory struct {
	inner *recordingRunner
	fail  *atomic.Bool
}

func (r failingInventory) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.fail.Load() && len(args) > 0 && strings.HasPrefix(args[0], "--query-gpu=index,") {
		r.inner.mu.Lock()
		r.inner.calls = append(r.inner.calls, runnerCall{command: name + " " + strings.Join(args, " "), err: errors.New("failed")})
		r.inner.mu.Unlock()
		return nil, errors.New("exit status 15")
	}
	return r.inner.Run(ctx, name, args...)
}

// countCommands counts the commands that contain sub.
func countCommands(cmds []string, sub string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// On an 8-GPU NVIDIA node a reconcile that changes nothing runs at most two
// tools: the GPU inventory once, shared by discovery and the cap, no -pl
// for a GPU whose limit already is the cap, and no static governor query.
// A changed cap is written to every GPU again, and the static governor
// facts are queried again after a failed inventory, after a change of the
// GPU list the inventory shows, and once they are nvidiaFactsTTL old.
func TestSteadyGPUReconcileRunsFewTools(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "nvidia-h100-sxm-8gpu", nil)
	e := useEmulatedNode(t, tree, map[string]string{"GOVERNOR_WRITES": "true"})
	fail := &atomic.Bool{}
	commandRunner = failingInventory{inner: e.runner, fail: fail}
	node := e.nodeObject(p)
	capW := 500.0
	twin := nodeTwinWithGPUCap(e.name, &capW, nil)
	twin.Spec.GPU.Governor = &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetAuto}
	reader, dyn := newTestClients(t, node, twin)
	nc := e.controller(t)
	reconcile := func() []string {
		t.Helper()
		e.runner.reset()
		if err := reconcileOnce(context.Background(), reader, dyn, nc); err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		return e.runner.commands()
	}
	gpus := p.GPUs.Count.V

	first := reconcile()
	if n := countCommands(first, " -pl "); n != gpus {
		t.Fatalf("first reconcile ran %d -pl, want %d: %v", n, gpus, first)
	}
	if n := countCommands(first, "SUPPORTED_CLOCKS"); n != 1 {
		t.Fatalf("first reconcile queried the supported clocks %d times, want 1: %v", n, first)
	}
	steady := reconcile()
	if len(steady) > 2 {
		t.Fatalf("a reconcile that changes nothing ran %d tools, want at most 2: %v", len(steady), steady)
	}
	t.Logf("first reconcile: %d tools; steady reconcile: %d tools %v", len(first), len(steady), steady)

	capW = 450
	twin.Spec.GPU.PowerCap.CapWattsPerGPU = &capW
	updateTwinSpec(t, reader, twin)
	if n := countCommands(reconcile(), " -pl 450"); n != gpus {
		t.Fatalf("a changed cap ran %d -pl, want %d", n, gpus)
	}

	fail.Store(true)
	reconcile()
	fail.Store(false)
	if n := countCommands(reconcile(), "SUPPORTED_CLOCKS"); n != 1 {
		t.Fatalf("after a failed inventory the supported clocks were queried %d times, want 1", n)
	}
	if steady := reconcile(); len(steady) > 2 {
		t.Fatalf("a reconcile that changes nothing ran %d tools, want at most 2: %v", len(steady), steady)
	}

	maxFile := filepath.Join(tree.StateRoot, filepath.FromSlash(layout.NVMLGPUDir(0)), "power_max_limit_mw")
	if err := os.Chmod(maxFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(maxFile, []byte("650000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := countCommands(reconcile(), "SUPPORTED_CLOCKS"); n != 1 {
		t.Fatalf("after the GPU list changed the supported clocks were queried %d times, want 1", n)
	}

	nc.gpuFacts.now = func() time.Time { return time.Now().Add(nvidiaFactsTTL + time.Minute) }
	if n := countCommands(reconcile(), "SUPPORTED_CLOCKS"); n != 1 {
		t.Fatalf("once the cached facts are older than %s the supported clocks were queried %d times, want 1", nvidiaFactsTTL, n)
	}
}

// updateTwinSpec writes twin's spec into the reader, as the controller
// manager does.
func updateTwinSpec(t *testing.T, r kube.Reader, twin *v1alpha1.NodeTwin) {
	t.Helper()
	reader, ok := r.(client.Client)
	if !ok {
		t.Fatalf("the test reader %T cannot update", r)
	}
	cur := &v1alpha1.NodeTwin{}
	if err := reader.Get(context.Background(), client.ObjectKey{Name: twin.Name}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec = twin.Spec
	if err := reader.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
}

// A GPU whose driver put its power limit back to the default (persistence
// mode off and no client holding it) gets the cap again on the next
// reconcile: the listing of that reconcile shows the default limit.
func TestGPUCapIsSetAgainAfterTheDriverResetsIt(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "nvidia-l40s-4gpu", nil)
	e := useEmulatedNode(t, tree, nil)
	capW := 200.0
	twin := nodeTwinWithGPUCap(e.name, &capW, nil)
	reader, dyn := newTestClients(t, e.nodeObject(p), twin)
	nc := e.controller(t)
	reconcile := func() []string {
		t.Helper()
		e.runner.reset()
		if err := reconcileOnce(context.Background(), reader, dyn, nc); err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		return e.runner.commands()
	}
	gpus := p.GPUs.Count.V
	if c := countCommands(reconcile(), " -pl 200"); c != gpus {
		t.Fatalf("first reconcile ran %d -pl, want %d", c, gpus)
	}
	if c := countCommands(reconcile(), " -pl "); c != 0 {
		t.Fatalf("a reconcile with the cap in place ran %d -pl, want none", c)
	}
	if err := n.Step(time.Second); err != nil {
		t.Fatal(err)
	}
	if c := countCommands(reconcile(), " -pl 200"); c != gpus {
		t.Fatalf("after the driver reset the limits, %d -pl ran, want %d", c, gpus)
	}
}
