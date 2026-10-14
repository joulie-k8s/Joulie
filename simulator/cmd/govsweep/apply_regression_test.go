package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// sysFiles is every regular file under a tree's sys/, by relative path.
func sysFiles(t *testing.T, sys string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(sys, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(sys, p)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sweepProfile is the builtin profile name as the sweep loads it.
func sweepProfile(t *testing.T, name string) *hwemu.Profile {
	t.Helper()
	p, ok, err := newProfileCache("").get("", nil, name)
	if err != nil || !ok {
		t.Fatalf("profile %s: %v", name, err)
	}
	return p
}

// sweepNode renders p and starts a node over it as the sweep does, with a
// fixed node name, seed and start, so two nodes of p are the same node.
func sweepNode(t *testing.T, p *hwemu.Profile) (*hwemu.Node, *hwemu.Tree) {
	t.Helper()
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: "lockstep", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	n, err := hwemu.NewNode(p, tree, hwemu.Options{Substep: 100 * time.Millisecond, Start: sweepEpoch, RenderCheckInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Step(0); err != nil {
		t.Fatal(err)
	}
	return n, tree
}

// The GPUs of an AMD node take their plans in lockstep, one step per round
// of writes, and end in the state the device-by-device writes give: the
// same files, the same State and the same arm result.
func TestAMDPlansInLockstepMatchDeviceByDevice(t *testing.T) {
	for _, preset := range []string{joulie.GPUGovernorPresetEfficient, joulie.GPUGovernorPresetAuto} {
		t.Run(preset, func(t *testing.T) {
			p := sweepProfile(t, "amd-instinct-mi300x-8gpu")
			na, ta := sweepNode(t, p)
			nb, tb := sweepNode(t, p)
			c := Cell{Grid: gridGPU, Preset: preset, CeilingPct: 75}
			got, err := applyGPU(context.Background(), na, ta, p, c)
			if err != nil {
				t.Fatal(err)
			}
			want := armResult{}
			var natives, results []string
			for _, f := range amdFacts(tb.SysRoot) {
				plan, err := governor.PlanAMD(f, c.Preset, gpuCeiling(c))
				if err != nil {
					t.Fatal(err)
				}
				natives, results = append(natives, plan.Native), append(results, plan.Result)
				written, res, _, _, err := applyWrites(nb, tb.SysRoot, plan.Writes)
				want.Writes += written
				if err != nil || res != "" {
					t.Fatalf("device by device: %s %v", res, err)
				}
			}
			want.Result, want.Native = uniform(results), uniform(natives)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("lockstep arm %+v, device by device %+v", got, want)
			}
			for _, n := range []*hwemu.Node{na, nb} {
				if err := n.Step(0); err != nil {
					t.Fatal(err)
				}
			}
			sa, sb := na.State(), nb.State()
			sa.Events, sb.Events = nil, nil
			if !reflect.DeepEqual(sa, sb) {
				t.Errorf("State after lockstep differs from State after device by device")
			}
			fa, fb := sysFiles(t, ta.SysRoot), sysFiles(t, tb.SysRoot)
			for rel, v := range fb {
				if fa[rel] != v {
					t.Errorf("%s holds %q after lockstep, %q after device by device", rel, fa[rel], v)
				}
			}
		})
	}
}

// A cell of an arm already found unsupported skips the render and the boot:
// with the work directory unusable, it still reports the row the first
// cell of that arm reported.
func TestUnsupportedArmIsDecidedOnce(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
name: unsupported-once
seeds: [1, 2]
durationS: 2
settleS: 1
gpu:
  profiles: [amd-radeon-pro-w7900-4gpu]
  arms: [{preset: efficient, ceilingPct: [75]}]
  capsPct: [100]
  receiverGates: [profile]
  idle: true
`))
	if err != nil {
		t.Fatal(err)
	}
	cells := cfg.expand()
	if len(cells) != 2 {
		t.Fatalf("%d cells, want 2", len(cells))
	}
	profiles := newProfileCache("")
	r := &runner{cfg: cfg, profiles: profiles.get, work: t.TempDir()}
	first, _, err := r.run(context.Background(), cells[0])
	if err != nil || first.Status != statusUnsupported {
		t.Fatalf("first cell: %+v, %v; want unsupported", first, err)
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.work = blocked
	second, _, err := r.run(context.Background(), cells[1])
	if err != nil {
		t.Fatalf("second cell rendered a tree for an unsupported arm: %v", err)
	}
	want := first
	want.Cell = cells[1]
	if !reflect.DeepEqual(second, want) {
		t.Errorf("second cell %+v, want %+v", second, want)
	}
}
