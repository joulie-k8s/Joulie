package main

// Regression tests of the loops and the node model: the workload and pod
// polling ticks (FC01, FC08), cap ranges (FC02), GPU counts (FC03), the
// analytic cap solver (FC04) and the emulated clock (FC05). FC07 is in
// pkg/hwinv/catalog_test.go.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fcStopLoop is the panic a fake client raises to end a loop that has no
// other way out, once the test has seen what it needs.
type fcStopLoop struct{}

// fcCaptureLog sends the standard logger to a buffer for the rest of the
// test.
func fcCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// FC01: SIM_WORKLOAD_TICK of 0 or less reached time.NewTicker, which panics
// on a non-positive interval, and took the simulator down. The loop now
// warns and ticks at the default, as SIM_PHYSICS_TICK does. The fake client
// ends the loop at its first pod create, which comes after the ticker.
func TestFC01WorkloadLoopSurvivesANonPositiveTick(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			logs := fcCaptureLog(t)
			fake := k8sfake.NewSimpleClientset()
			fake.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				panic(fcStopLoop{})
			})
			prev := kubeClientFactory
			kubeClientFactory = func(float32, int, time.Duration) (kubernetes.Interface, error) { return fake, nil }
			t.Cleanup(func() { kubeClientFactory = prev })
			s := newLoaderTestSimulator()
			s.workload = dueJobs(1, workloadOptions{})

			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				s.startWorkloadLoop(interval)
			}()
			select {
			case r := <-done:
				if _, ok := r.(fcStopLoop); !ok {
					t.Fatalf("SIM_WORKLOAD_TICK=%s: the workload loop ended with %v before its first pod create; want it to tick at the default", interval, r)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("SIM_WORKLOAD_TICK=%s: the workload loop created no pod in 30 s", interval)
			}
			if want := fmt.Sprintf("SIM_WORKLOAD_TICK=%s is not positive, using 1s", interval); !strings.Contains(logs.String(), want) {
				t.Errorf("log %q, want a warning containing %q", logs.String(), want)
			}
		})
	}
}

// FC02: a cap minimum set by hand above the maximum the catalogue fills in
// left the range inverted: SIM_GPU_MIN_CAP_WATTS_PER_GPU=800 on an H100 SXM
// (700 W board power) gave 800 to 700 W. The resolved range is now ordered,
// with a warning. (The CPU socket minimum, SIM_CPU_SOCKET_CAP_MIN_W, is
// gone: no power model read it.)
func TestFC02ExplicitCapMinimumAboveTheCatalogueMaximumIsOrdered(t *testing.T) {
	clearSimEnv(t)
	t.Setenv("SIM_GPU_MIN_CAP_WATTS_PER_GPU", "800")
	removedKnobWarnMu.Lock()
	removedKnobWarnSeen = map[string]struct{}{}
	removedKnobWarnMu.Unlock()
	logs := fcCaptureLog(t)
	s, _ := mrSimulator(t, nil)
	model, class := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, map[string]string{
		"joulie.io/hw.cpu-model":   "INTEL XEON PLATINUM 8260",
		"joulie.io/hw.cpu-sockets": "2",
	}))
	if model.CPUModel != "INTEL_XEON_PLATINUM_8260" || model.GPU.MaxWattsPerGPU != 700 {
		t.Fatalf("test setup: class %s, CPU %q, GPU max %v W; want the 8260 and the H100 SXM at 700 W from the catalogue",
			class, model.CPUModel, model.GPU.MaxWattsPerGPU)
	}
	if model.GPU.MinCapWattsPerGPU > model.GPU.MaxWattsPerGPU {
		t.Errorf("GPU cap range %v to %v W, want the minimum at or below the maximum", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	if want := "GPU cap minimum 800 W is above its maximum 700 W"; !strings.Contains(logs.String(), want) {
		t.Errorf("log %q, want a warning containing %q", logs.String(), want)
	}
}

// FC03: a node whose GPU the catalogue does not know kept GPU.Count 0, or
// SIM_GPU_COUNT, even with joulie.io/hw.gpu-count set, so its GPUs drew no
// power. The label count now applies whatever the catalogue knows.
func TestFC03UnknownGPUTakesTheLabelCount(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(map[string]string{
		"joulie.io/hw.gpu-model": "Generic PCIe Accelerator",
		"joulie.io/hw.gpu-count": "4",
	})
	if model.GPU.Product != "" {
		t.Fatalf("test setup: the catalogue matched GPU %q, want none", model.GPU.Product)
	}
	if model.GPU.Count != 4 {
		t.Fatalf("unknown GPU with joulie.io/hw.gpu-count=4: GPU.Count %d, want 4", model.GPU.Count)
	}
	if model, _ := s.resolveModelForNode(mrH100SXMLabels); model.GPU.Count != 8 {
		t.Errorf("H100 SXM with joulie.io/hw.gpu-count=8: GPU.Count %d, want 8", model.GPU.Count)
	}
}

// FC04: the analytic solver inverted the power formula without the memory
// and IO activity factor, so a capped node with memory traffic settled at a
// scale where it drew below its cap: 262.5 W under 300 W at util 1 and
// memory intensity 0.5 on the golden model. It now draws the cap.
func TestFC04CappedAnalyticNodeWithMemoryTrafficDrawsItsCap(t *testing.T) {
	model := cpupowerGoldenModel()
	for _, tc := range []struct {
		capW, mem, io float64
	}{
		{300, 0.5, 0},
		{250, 0.2, 0},
		{200, 0, 0.3},
		{300, 0, 0},
	} {
		st := &nodeState{CPUUtil: 1, FreqScale: 1, CapWatts: tc.capW, MemoryIntensity: tc.mem, IOIntensity: tc.io}
		power := cpuPowerWithModel(st, model)
		if st.CapSaturated || st.FreqScale >= 1 {
			t.Fatalf("test setup: util 1, memory %.1f, IO %.1f under %.0f W: saturated %v, scale %.4f; want a cap the node reaches below scale 1",
				tc.mem, tc.io, tc.capW, st.CapSaturated, st.FreqScale)
		}
		if math.Abs(power-tc.capW) > 0.005*tc.capW {
			t.Errorf("util 1, memory %.1f, IO %.1f under %.0f W: power %.2f W at scale %.4f, want the cap within 0.5 %%",
				tc.mem, tc.io, tc.capW, power, st.FreqScale)
		}
	}
}

// FC05: an emulated node's legacy state never set SimTime, so /debug/nodes
// showed 0001-01-01. It now follows the emulated clock: the clock's start
// before the first snapshot, then each snapshot's time.
func TestFC05EmulatedNodeStateFollowsTheEmulatedClock(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	node := et.rt.names[0]
	simTime := func() time.Time {
		t.Helper()
		et.s.mu.RLock()
		defer et.s.mu.RUnlock()
		st := et.s.state[node]
		if st == nil {
			t.Fatalf("no state for spec node %s", node)
		}
		return st.SimTime
	}

	et.s.refreshNodeStateFromKubeData(map[string]int{}, nil, map[string]map[string]string{node: {}})
	if got := simTime(); !got.Equal(emulateTestStart) {
		t.Errorf("before the first snapshot: SimTime %v, want the clock's start %v", got, emulateTestStart)
	}
	for _, at := range []time.Duration{time.Second, 3 * time.Second} {
		snap := et.tick(t, emulateTestStart.Add(at))
		if got := simTime(); !got.Equal(snap.Time) || got.IsZero() {
			t.Errorf("after the snapshot at %v: SimTime %v, want %v", snap.Time, got, snap.Time)
		}
	}

	// A node whose state the first snapshot creates starts at that
	// snapshot's time too.
	fresh := newEmulateTest(t, emulateTestConfig{})
	snap := fresh.tick(t, emulateTestStart.Add(time.Second))
	fresh.s.mu.RLock()
	defer fresh.s.mu.RUnlock()
	for _, n := range snap.Nodes {
		st := fresh.s.state[n.Name]
		if st == nil {
			t.Errorf("node %s has no state after the first snapshot", n.Name)
		} else if !st.SimTime.Equal(snap.Time) {
			t.Errorf("node %s after the first snapshot: SimTime %v, want %v", n.Name, st.SimTime, snap.Time)
		}
	}
}

// FC08: SIM_POLL_INTERVAL of 0 or less reached time.NewTicker in
// startPodPolling, which panics before the first poll and takes the
// simulator down. It now warns and polls at the default.
func TestFC08PodPollingSurvivesANonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			logs := fcCaptureLog(t)
			prev := kubeClientFactory
			kubeClientFactory = func(float32, int, time.Duration) (kubernetes.Interface, error) {
				return k8sfake.NewSimpleClientset(), nil
			}
			t.Cleanup(func() { kubeClientFactory = prev })
			prevList := podPollFunc
			podPollFunc = func(context.Context, kubernetes.Interface) (map[string]int, []runningPodInfo, error) {
				panic(fcStopLoop{})
			}
			t.Cleanup(func() { podPollFunc = prevList })

			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				newLoaderTestSimulator().startPodPolling(interval)
			}()
			select {
			case r := <-done:
				if _, ok := r.(fcStopLoop); !ok {
					t.Fatalf("SIM_POLL_INTERVAL=%s: pod polling ended with %v before its first poll", interval, r)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("SIM_POLL_INTERVAL=%s: pod polling made no poll in 30 s", interval)
			}
			if want := fmt.Sprintf("SIM_POLL_INTERVAL=%s is not positive, using 15s", interval); !strings.Contains(logs.String(), want) {
				t.Errorf("log %q, want a warning containing %q", logs.String(), want)
			}
		})
	}
}
