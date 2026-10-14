package main

// Tests of the governor baseline and of what the agent keeps through a GPU
// discovery that comes back empty, over synthetic trees and emulated nodes.

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// The baseline follows every discovery until the agent's first write: a
// governor an operator sets while GOVERNOR_WRITES is false is what the
// restore puts back once writes are on, and the baseline stays at it while
// the agent's own preset runs.
func TestGovernorBaselineFollowsDiscoveryUntilTheFirstWrite(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	r := newGovRig(t, plainNode("gov-baseline-follows"), nil, efficiency, nil)
	r.reconcile()

	operator := map[string]string{}
	for p := 0; p < 4; p++ {
		operator[fmt.Sprintf("devices/system/cpu/cpufreq/policy%d/scaling_governor", p)] = "performance\n"
	}
	sysfsFiles(t, root, operator)
	r.reconcile()
	r.syncStatus()

	t.Setenv("GOVERNOR_WRITES", "true")
	r.restart()
	r.reconcile()
	if got := policyGovernors(t, root); !allEqual(got, "conservative") {
		t.Fatalf("scaling_governor %v with writes on, want conservative", got)
	}
	want := []v1alpha1.CPUGovernorState{{Policies: "0-3", Governor: "performance", Boost: joulie.CPUGovernorBoostOn}}
	if g := r.hardware().CPU.Governor; g == nil || g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Groups, want) {
		t.Fatalf("cpu.governor %+v under the agent's preset, want the baseline %+v the operator set", g, want)
	}

	r.setIntents(nil, nil)
	r.reconcile()
	if got := policyGovernors(t, root); !allEqual(got, "performance") {
		t.Fatalf("scaling_governor %v after the field went away, want performance, the operator's governor before the first write", got)
	}
}

// A GPU reset or a driver reload brings an amdgpu level back to auto; the
// agent applies the preset again at once, without counting a drift, so
// resets never make the setting contended.
func TestGovernorGPUResetReappliesThePreset(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdDevice(files, "0000:10:00.0", "auto")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)
	node := plainNode("gov-gpu-reset")
	node.Labels = map[string]string{"feature.node.kubernetes.io/pci-1002.present": "true"}
	r := newGovRig(t, node, governorOn, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	level := filepath.Join(root, "bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level")

	r.reconcile()
	r.reconcile()
	for reset := 1; reset <= governorDriftLimit+1; reset++ {
		sysfsFiles(t, root, map[string]string{"bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level": "auto\n"})
		r.reconcile()
		if got := readTrimmed(t, level); got != "high" {
			t.Fatalf("reset %d: level %q, want high applied again", reset, got)
		}
		if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultApplied {
			t.Fatalf("reset %d: gpuGovernor status %+v, want applied, not a drift", reset, s)
		}
		r.reconcile()
	}
}

// failingClockQuery runs every command on inner, but fails the nvidia-smi
// clock query while fail is set, as a driver that times out does.
type failingClockQuery struct {
	inner CommandRunner
	fail  *atomic.Bool
}

func (r failingClockQuery) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.fail.Load() && len(args) > 0 && strings.HasPrefix(args[0], "--query-gpu=pci.bus_id,clocks.sm") {
		return nil, &smi.ExitError{Code: 15}
	}
	return r.inner.Run(ctx, name, args...)
}

// A GPU discovery that comes back empty because nvidia-smi did not answer
// keeps the GPU governor block NodeHardware lists, presets included, both
// within a run and right after a restart, and the agent never resets the
// locks it set: the controller manager keeps the intent, so there is
// nothing to restore and apply again.
func TestGovernorEmptyGPUDiscoveryKeepsThePresets(t *testing.T) {
	g := emulatedGovernorNode(t, "nvidia-h100-sxm-4gpu", nil)
	fail := &atomic.Bool{}
	commandRunner = failingClockQuery{inner: g.e.runner, fail: fail}
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})

	r.reconcile()
	before := r.hardware().GPU.Governor
	if before == nil || len(before.Presets) == 0 || before.Mechanism != joulie.GovernorBackendNvidiaSMI {
		t.Fatalf("gpu.governor %+v, want the nvidia-smi block with its presets", before)
	}
	if c := lockCommands(g.e, "-lgc"); len(c) != g.p.GPUs.Count.V {
		t.Fatalf("efficient ran %v, want one -lgc per GPU", c)
	}
	check := func(when string) {
		t.Helper()
		got := r.hardware().GPU.Governor
		if got == nil || got.Mechanism != before.Mechanism || !reflect.DeepEqual(got.Presets, before.Presets) {
			t.Fatalf("%s: gpu.governor %+v, want mechanism %s and presets %+v kept", when, got, before.Mechanism, before.Presets)
		}
		if c := lockCommands(g.e, "-rgc", "-rmc"); len(c) != 0 {
			t.Fatalf("%s: the agent reset its locks: %v", when, c)
		}
	}

	fail.Store(true)
	r.reconcile()
	check("during an empty discovery")
	r.syncStatus()
	r.restart()
	r.reconcile()
	check("after a restart into an empty discovery")
	fail.Store(false)
	r.reconcile()
	check("once nvidia-smi answers again")
}

// rejectingField runs every command on inner, but answers a --query-gpu
// that names field the way an nvidia-smi without that field does: the
// message on stdout and exit code 2.
type rejectingField struct {
	inner CommandRunner
	field string
}

func (r rejectingField) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	for _, a := range args {
		if q, ok := strings.CutPrefix(a, "--query-gpu="); ok && strings.Contains(","+q+",", ","+r.field+",") {
			return []byte(fmt.Sprintf("Field \"%s\" is not a valid field to query.\n", r.field)), &smi.ExitError{Code: 2}
		}
	}
	return r.inner.Run(ctx, name, args...)
}

// The clock query works on a driver that knows only one name of the clock
// reasons field: R525 and older reject clocks_event_reasons.active, and a
// driver that drops the clocks_throttle_reasons.active alias gets the
// event name instead.
func TestGovernorClockQueryWorksWithEitherReasonsField(t *testing.T) {
	for _, rejected := range []string{"clocks_event_reasons.active", "clocks_throttle_reasons.active"} {
		t.Run(rejected, func(t *testing.T) {
			g := emulatedGovernorNode(t, "nvidia-l40s-4gpu", nil)
			commandRunner = rejectingField{inner: g.e.runner, field: rejected}
			status := publishedStatus(t, g.e.name, g.e.nodeObject(g.p).Labels)
			if status.GPU == nil || status.GPU.Governor == nil || len(status.GPU.Governor.Presets) == 0 {
				t.Fatalf("no gpu.governor presets with %s rejected; gpu status %+v", rejected, status.GPU)
			}
			for _, w := range status.GPU.Warnings {
				if w == warnNvidiaClockQuery {
					t.Errorf("gpu.warnings %v with %s rejected", status.GPU.Warnings, rejected)
				}
			}
			rows, err := discover.QueryNvidiaClocks(context.Background(), runTool)
			if err != nil || len(rows) != g.p.GPUs.Count.V || !rows[0].ReasonsOK {
				t.Errorf("clock query rows %+v, %v; want one per GPU with the reasons", rows, err)
			}
		})
	}
}
