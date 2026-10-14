package hwemu

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// gpCeilings are the ceilings of the efficient preset the preset tests plan
// at: low, the default and the whole range.
var gpCeilings = []float64{10, 75, governor.DefaultCeilingPctOfMax, 100}

// gpCheck reports a GPU whose reported preset is not one pkg/governor names
// for the state the plan set: the reported preset must plan the same state,
// and efficient at the default ceiling and above must read back as
// efficient. Lower, its state can be another preset's (the lowest clock
// alone is low's where memory has no lock).
func gpCheck(t *testing.T, what, preset string, pct float64, plan governor.Plan, got string, replan func(preset string) governor.Plan) {
	t.Helper()
	if got == "" {
		t.Errorf("%s: %s at %v%% sets %q, which the GPU reports as no preset", what, preset, pct, plan.Native)
		return
	}
	if q := replan(got); !reflect.DeepEqual(q.State, plan.State) {
		t.Errorf("%s: %s at %v%% sets %q, reported as %s, whose plan sets %q", what, preset, pct, plan.Native, got, q.Native)
	}
	if preset == api.GPUGovernorPresetEfficient && pct >= governor.DefaultCeilingPctOfMax && got != preset {
		t.Errorf("%s: efficient at %v%% (%q) reported as %s", what, pct, plan.Native, got)
	}
}

// Every NVIDIA builtin profile reports, for the locks of every plan the
// agent makes on the facts it discovers from the rendered node, the preset
// pkg/governor names for those locks.
func TestNVClockReportsThePresetPkgGovernorPlans(t *testing.T) {
	t.Parallel()
	for _, name := range nvclockBuiltinNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ensureBuiltin(t, name)
			tr := renderTestTree(t, p)
			n := nodeNewNode(t, p, tr)
			ctx := context.Background()
			r := smi.Runner{Env: smi.Env{SysRoot: tr.SysRoot, StateRoot: tr.StateRoot}}
			arch := discover.NvidiaArch(p.NodeObject("n").Labels, p.GPUs.Product.V)
			gpus, err := discover.ReadNvidia(ctx, r.Run, arch)
			if err != nil || len(gpus) == 0 {
				t.Fatalf("ReadNvidia: %v, %d GPUs", err, len(gpus))
			}
			g := gpus[0]
			smiRun := func(args []string) {
				t.Helper()
				if out, err := r.Run(ctx, "nvidia-smi", args...); err != nil {
					t.Fatalf("nvidia-smi %s: %v (%s)", strings.Join(args, " "), err, out)
				}
			}
			for _, preset := range api.GPUGovernorPresetValues {
				for _, pct := range gpCeilings {
					locked := g.Facts
					locked.LockedGPU, locked.LockedMem = true, true
					reset, err := governor.PlanNvidia(locked, g.BusID, api.GPUGovernorPresetAuto, pct)
					if err != nil {
						t.Fatal(err)
					}
					for _, a := range reset.Args {
						smiRun(a)
					}
					plan, err := governor.PlanNvidia(g.Facts, g.BusID, preset, pct)
					if err != nil {
						t.Fatal(err)
					}
					if plan.Result == api.GovernorResultUnsupported {
						continue
					}
					for _, a := range plan.Args {
						smiRun(a)
					}
					nodeStep(t, n, time.Second)
					got := stateComponent(t, n.State(), "gpu0").GovernorPreset
					gpCheck(t, name, preset, pct, plan, got, func(q string) governor.Plan {
						qp, _ := governor.PlanNvidia(g.Facts, g.BusID, q, pct)
						return qp
					})
				}
			}
		})
	}
}

// Every AMD builtin profile reports, for the level and overdrive range of
// every plan the agent makes on the facts it discovers from the rendered
// node, the preset pkg/governor names for them.
func TestDPMReportsThePresetPkgGovernorPlans(t *testing.T) {
	t.Parallel()
	for _, name := range []string{w7900Profile, mi300xProfile} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, tr, n := dpmNode(t, name)
			devices := discover.ReadAMD(tr.SysRoot)
			if len(devices) == 0 {
				t.Fatal("ReadAMD found no device")
			}
			f := devices[0].Facts
			for _, preset := range api.GPUGovernorPresetValues {
				for _, pct := range gpCeilings {
					plan, err := governor.PlanAMD(f, preset, pct)
					if err != nil {
						t.Fatal(err)
					}
					if plan.Result == api.GovernorResultUnsupported {
						continue
					}
					w := governor.PlanAMDLevel(f.BusID, governor.AMDLevelAuto)
					for _, w := range append([]governor.Write{w}, plan.Writes...) {
						nodeWrite(t, tr, "sys/"+w.RelPath, w.Value)
						nodeStep(t, n, time.Second)
					}
					c := stateComponent(t, n.State(), "gpu0")
					if c.GovernorNative != plan.Level {
						t.Fatalf("%s: gpu0 runs %q after %s, want %q: gpu0 is not the device at %s", name, c.GovernorNative, preset, plan.Level, f.BusID)
					}
					gpCheck(t, name, preset, pct, plan, c.GovernorPreset, func(q string) governor.Plan {
						qp, _ := governor.PlanAMD(f, q, pct)
						return qp
					})
				}
			}
		})
	}
}
