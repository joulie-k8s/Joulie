package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// armResult is what setting an arm gave: the pkg/api GovernorResult* value
// (empty for the default CPU arm, which writes nothing), the native state in
// the driver's own words, the number of writes and commands issued, and a
// message when the result is not the plan's.
type armResult struct {
	Result  string
	Native  string
	Writes  int
	Message string
}

// tail is the last two elements of a sysfs path, how an event's file is
// matched to a write: policy0/scaling_governor, intel_pstate/no_turbo,
// 0000:c1:00.0/power_dpm_force_performance_level.
func tail(p string) string {
	return path.Base(path.Dir(p)) + "/" + path.Base(p)
}

// applyWrites writes a plan's sysfs writes in order. Consecutive writes to
// files of the same name go out together and are read back after one step
// of zero length, the hwemu readback the kernel's would be; a change of
// file name, or a second write to a file in the group, starts a new group:
// so the governor of every policy is set before any preference, and the
// manual level before an overdrive edit, as the agent orders them. A
// rejected write, or one that reads back otherwise, ends the plan with the
// pkg/governor.Classify result and is returned as failed. Overdrive edits
// ("s 1 <MHz>", "c") read back as a table, not as the value, so only their
// rejection counts.
func applyWrites(n *hwemu.Node, sys string, ws []governor.Write) (written int, result, msg string, failed governor.Write, err error) {
	var group []governor.Write
	flush := func() (string, string, governor.Write, error) {
		if len(group) == 0 {
			return "", "", governor.Write{}, nil
		}
		defer func() { group = group[:0] }()
		if err := n.Step(0); err != nil {
			return "", "", governor.Write{}, err
		}
		for _, e := range n.State().Events {
			if e.Kind != hwemu.EventRejected {
				continue
			}
			for _, w := range group {
				if strings.HasSuffix(e.File, "/"+tail(w.RelPath)) {
					return joulie.GovernorResultUnsupported, fmt.Sprintf("%s: %s rejected with %s", w.RelPath, w.Value, e.Errno), w, nil
				}
			}
		}
		for _, w := range group {
			if governor.IsODWrite(w) {
				continue
			}
			if r, m := governor.Classify(nil, w.Value, readWord(filepath.Join(sys, w.RelPath))); r != joulie.GovernorResultApplied {
				return r, w.RelPath + ": " + m, w, nil
			}
		}
		return "", "", governor.Write{}, nil
	}
	for _, w := range ws {
		if len(group) > 0 {
			last := group[len(group)-1]
			repeat := false
			for _, g := range group {
				repeat = repeat || g.RelPath == w.RelPath
			}
			if path.Base(last.RelPath) != path.Base(w.RelPath) || repeat {
				if r, m, f, err := flush(); err != nil || r != "" {
					return written, r, m, f, err
				}
			}
		}
		p := filepath.Join(sys, w.RelPath)
		if werr := discover.WriteAttr(p, w.Value); werr != nil {
			r, m := governor.Classify(werr, w.Value, "")
			return written, r, w.RelPath + ": " + m, w, nil
		}
		written++
		group = append(group, w)
	}
	r, m, f, err := flush()
	return written, r, m, f, err
}

// lockstep orders the writes of plans as the agent writes the GPUs of a
// node (writeAMDSteps in cmd/agent): the first write of every plan, then
// the second, and so on. A plan's writes keep their order, and applyWrites
// sends each round of writes to different files in one step; a device's
// validation of a write reads only that device's state, so sharing the
// step changes no outcome.
func lockstep(plans []governor.Plan) []governor.Write {
	var out []governor.Write
	for k := 0; ; k++ {
		more := false
		for _, p := range plans {
			if k < len(p.Writes) {
				out = append(out, p.Writes[k])
				more = true
			}
		}
		if !more {
			return out
		}
	}
}

// applyCPU sets c's CPU arm: the pkg/governor plan of the preset on the
// tree's facts, or nothing for the default arm, whose native state is what
// the first policy runs.
func applyCPU(n *hwemu.Node, t *hwemu.Tree, c Cell) (armResult, error) {
	cpu := discover.ReadCPU(t.SysRoot, t.CPUInfoPath)
	var f governor.CPUFacts
	if cpu != nil {
		f = cpu.Facts
	}
	if c.Preset == ArmDefault {
		if cpu == nil {
			return armResult{}, nil
		}
		return armResult{Native: governor.NativeCPU(cpu.Policies[0].Governor, cpu.Policies[0].EPP)}, nil
	}
	plan, err := governor.PlanCPU(f, c.Preset, c.Boost)
	if err != nil {
		return armResult{}, err
	}
	out := armResult{Result: plan.Result, Native: plan.Native}
	if plan.Result == joulie.GovernorResultUnsupported {
		return out, nil
	}
	written, r, msg, _, err := applyWrites(n, t.SysRoot, plan.Writes)
	out.Writes = written
	if r != "" {
		out.Result, out.Message = r, msg
	}
	return out, err
}

// applyCap sets c's GPU power cap on every GPU, as the agent sets it
// (nvidia-smi -i <index> -pl, rocm-smi -d <index> --setpoweroverdrive), in
// W of the board maximum, and steps once so the node reads it. A cap of 100
// percent writes nothing. The run sets the cap once the job holds its GPUs:
// a GPU without persistence mode drops its limit back to the default
// whenever it idles, as the agent sees on such a node.
func applyCap(ctx context.Context, n *hwemu.Node, t *hwemu.Tree, p *hwemu.Profile, c Cell) error {
	g := p.GPUs
	if c.CapPct >= 100 || g == nil {
		return nil
	}
	r := smi.Runner{Env: smi.Env{SysRoot: t.SysRoot, StateRoot: t.StateRoot}}
	w := strconv.Itoa(int(math.Round(c.CapPct / 100 * g.Limits.MaxW.V)))
	for i := 0; i < g.Count.V; i++ {
		var args []string
		switch g.Vendor {
		case "nvidia":
			args = []string{"nvidia-smi", "-i", strconv.Itoa(i), "-pl", w}
		case "amd":
			args = []string{"rocm-smi", "-d", strconv.Itoa(i), "--setpoweroverdrive", w}
		default:
			return fmt.Errorf("profile %s: GPU vendor %q has no cap command", p.Name, g.Vendor)
		}
		if b, err := r.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
		}
	}
	return n.Step(0)
}

// applyGPU sets c's GPU preset on every GPU: its pkg/governor plan per GPU,
// keyed by bus id. The result is the plan's when every GPU gives the same,
// and the first failure otherwise.
func applyGPU(ctx context.Context, n *hwemu.Node, t *hwemu.Tree, p *hwemu.Profile, c Cell) (armResult, error) {
	g := p.GPUs
	if g == nil || g.Count.V <= 0 {
		return armResult{}, fmt.Errorf("profile %s has no GPUs", p.Name)
	}
	r := smi.Runner{Env: smi.Env{SysRoot: t.SysRoot, StateRoot: t.StateRoot}}
	var out armResult
	var natives, results []string
	switch g.Vendor {
	case "amd":
		var plans []governor.Plan
		native := map[string]string{} // by RelPath, the native state of the plan that writes it
		for _, f := range amdFacts(t.SysRoot) {
			plan, err := governor.PlanAMD(f, c.Preset, gpuCeiling(c))
			if err != nil {
				return out, err
			}
			natives, results = append(natives, plan.Native), append(results, plan.Result)
			if plan.Result == joulie.GovernorResultUnsupported {
				continue
			}
			plans = append(plans, plan)
			for _, w := range plan.Writes {
				native[w.RelPath] = plan.Native
			}
		}
		written, res, msg, failed, err := applyWrites(n, t.SysRoot, lockstep(plans))
		out.Writes += written
		if err != nil {
			return out, err
		}
		if res != "" {
			return armResult{Result: res, Native: native[failed.RelPath], Writes: out.Writes, Message: msg}, nil
		}
	case "nvidia":
		gpus, err := nvFacts(ctx, r, p)
		if err != nil {
			return out, err
		}
		for _, gpu := range gpus {
			plan, err := governor.PlanNvidia(gpu.Facts, gpu.BusID, c.Preset, gpuCeiling(c))
			if err != nil {
				return out, err
			}
			natives, results = append(natives, plan.Native), append(results, plan.Result)
			for _, a := range plan.Args {
				b, err := r.Run(ctx, "nvidia-smi", a...)
				out.Writes++
				if err != nil {
					res, msg := governor.Classify(err, "", "")
					return armResult{Result: res, Native: plan.Native, Writes: out.Writes, Message: msg + ": " + strings.TrimSpace(string(b))}, nil
				}
			}
		}
		if out.Writes > 0 {
			if err := n.Step(0); err != nil {
				return out, err
			}
		}
	}
	if len(results) == 0 {
		return armResult{Result: joulie.GovernorResultUnsupported}, nil
	}
	out.Result, out.Native = uniform(results), uniform(natives)
	return out, nil
}

// gpuCeiling is the efficient ceiling of c, and the default for any other
// preset, which ignores it.
func gpuCeiling(c Cell) float64 {
	if c.CeilingPct > 0 {
		return c.CeilingPct
	}
	return governor.DefaultCeilingPctOfMax
}

// uniform is the one value of vs, or mixed.
func uniform(vs []string) string {
	for _, v := range vs[1:] {
		if v != vs[0] {
			return joulie.GovernorCurrentMixed
		}
	}
	return vs[0]
}

var errNoVariant = errors.New("the profile has no fact the sensitivity variant sets")
