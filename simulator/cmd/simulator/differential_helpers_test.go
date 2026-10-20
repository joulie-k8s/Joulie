package main

// The differential harness: one builtin hwemu profile run twice, once as an
// emulated node (package hwemu, as emulate mode runs it) and once through the
// legacy model the profile adapter derives (simModelFromProfile and stepNode,
// as standalone mode runs it), through the same segments of load, power
// limits and frequency limits. The two sides share no code above package phys,
// so a reading on which they agree is one both arrive at independently.

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

const (
	// diffSegmentSeconds is the length of a segment, and diffMeanSeconds
	// the tail of it whose mean power is compared, once the lags settled.
	diffSegmentSeconds = 60
	diffMeanSeconds    = 30
	// diffCoarseSubstep is the single substep of the coarse legacy side:
	// standalone's SIM_SA_SUBSTEP at the default SIM_SA_DT of 5 s.
	diffCoarseSubstep = 5 * time.Second
	// diffCPUCapFraction and diffFreqFraction set the CPU segments, and
	// diffGPUCapFraction the capped GPU segment, each a fraction of the
	// highest value the limit takes.
	diffCPUCapFraction = 0.6
	diffFreqFraction   = 0.6
	diffGPUCapFraction = 0.7
)

// Tolerances, relative to the emulated reading. A frequency-limited segment
// gets diffFreqMeanRel because the legacy throttle is a whole percent of the
// maximum frequency, while scaling_max_freq resolves to any kHz value.
const (
	diffMeanAbsW     = 0.5
	diffMeanRel      = 0.005
	diffFreqMeanRel  = 0.015
	diffEnergyRel    = 0.01
	diffCoarseEnergy = 0.02
)

// diffInputs is the work both sides run. Only the class and the memory
// intensity reach hwemu (hwemu.Load); the legacy side keeps its IO and CPU
// feed intensities at 0, which is what hwemu's phys.DeviceState holds
// (physics.go, derive).
type diffInputs struct {
	cpuClass, gpuClass string
	memoryIntensity    float64
}

// diffSegment is one stretch of the run: the load and the limits written at
// its start. cpuCap, freq and gpuCap are fractions of the limit's highest
// value; 0 restores the limit as rendered (cpuCap, freq) or writes the
// maximum (gpuCap).
type diffSegment struct {
	name    string
	cpuUtil float64
	cpuCap  float64
	freq    float64
	gpuUtil float64 // the node's mean, spread over the devices as gpuDevicePowers spreads it
	gpuCap  float64
}

// diffSegments is the run for a node of profile p: the CPU segments, then the
// GPU segments when p has GPUs. The capped CPU segment needs a limit someone
// can write, a PL1 or an HSMP cap; the frequency segments need cpufreq. The
// capped GPU segment needs a GPU limit range: a pinned one, minimum equal to
// maximum, refuses every other value.
func diffSegments(p *hwemu.Profile) []diffSegment {
	segs := []diffSegment{
		{name: "cpu idle"},
		{name: "cpu half", cpuUtil: 0.5},
		{name: "cpu full", cpuUtil: 1},
	}
	if lim, err := profileCPULimits(p); err == nil && lim.ok && (lim.writablePL1 || p.HSMP != nil) {
		segs = append(segs, diffSegment{name: "cpu capped", cpuUtil: 1, cpuCap: diffCPUCapFraction})
	}
	if p.CPUFreq != nil {
		segs = append(segs,
			diffSegment{name: "cpu frequency-limited", cpuUtil: 1, freq: diffFreqFraction},
			diffSegment{name: "cpu frequency-limited light", cpuUtil: 0.1, freq: diffFreqFraction},
		)
	}
	if p.GPUs != nil && p.GPUs.Count.V > 0 {
		segs = append(segs,
			diffSegment{name: "gpu idle"},
			diffSegment{name: "gpu full", gpuUtil: 1},
			diffSegment{name: "gpu spread", gpuUtil: 0.5},
		)
		if l := p.GPUs.Limits; l.MinW.V < l.MaxW.V {
			segs = append(segs, diffSegment{name: "gpu capped", gpuUtil: 1, gpuCap: diffGPUCapFraction})
		}
	}
	return segs
}

// diffSide is what one side drew in a segment: the mean power over the
// segment's last diffMeanSeconds and the energy over the whole segment.
type diffSide struct {
	cpuW, gpuW float64
	cpuJ, gpuJ float64
}

// diffResult is one segment on the three sides: the emulated node, the
// legacy model in physMaxSubstep substeps, and the legacy model in one
// diffCoarseSubstep substep per step.
type diffResult struct {
	seg                diffSegment
	freqLimited        bool
	emu, fine, coarse  diffSide
	throttlePct        int
	emuFreqScale       float64
	legacyFreqScale    float64
	cpuCapW, gpuLimitW float64
}

// diffRun drives both sides of profile through segs and returns one result
// per segment. perturb, when not nil, changes the legacy model after the
// adapter derived it, which is how the comparator is shown to catch a model
// that drifted.
func diffRun(t *testing.T, profile string, in diffInputs, segs func(*hwemu.Profile) []diffSegment, perturb func(*simModel)) []diffResult {
	t.Helper()
	node, tree, p := hwemutest.NewNode(t, profile, func(p *hwemu.Profile) {
		if p.CPUFreq != nil {
			p.CPUFreq.Governor.V = "performance"
		}
	})
	model, err := simModelFromProfile(p, defaultBaseModelFromEnv())
	if err != nil {
		t.Fatalf("profile %s has no legacy model: %v", profile, err)
	}
	if perturb != nil {
		perturb(&model)
	}
	ctl := diffFindControls(t, tree)
	start := node.State().Time
	fine := &nodeState{SimTime: start}
	coarse := &nodeState{SimTime: start}

	var out []diffResult
	elapsed := 0
	for _, seg := range segs(p) {
		r := diffResult{seg: seg, freqLimited: seg.freq > 0}
		at := start.Add(time.Duration(elapsed) * time.Second)
		memory := in.memoryIntensity

		// The emulated side: the load from now, the limits as the agent
		// or a GPU tool writes them, read back by a step of zero length.
		gpuUtil := diffGPUSpread(model.GPU.Count, seg.gpuUtil)
		node.SetLoad(hwemu.Load{CPUUtil: []float64{seg.cpuUtil}, CPUClass: in.cpuClass, GPUUtil: gpuUtil, GPUClass: in.gpuClass, MemoryIntensity: memory})
		r.cpuCapW = model.DefaultCapW
		if seg.cpuCap > 0 {
			r.cpuCapW = seg.cpuCap * model.RaplCapMaxW
		}
		ctl.writeCPUCap(t, seg.cpuCap)
		ctl.writeFreq(t, seg.freq)
		r.gpuLimitW = ctl.writeGPULimit(t, seg.gpuCap, model.GPU.MaxWattsPerGPU)
		diffAdvance(t, node, at)
		if r.freqLimited {
			// The legacy throttle is the share of the maximum frequency
			// the resolved scaling_max_freq leaves out.
			limit, maxHz := diffCPUSpeed(node.State(), func(c hwemu.ComponentState) float64 { return c.SpeedLimitHz })
			r.throttlePct = 100 - int(math.Round(100*limit/maxHz))
		}

		// The legacy sides: the same load and limits as targets.
		for _, st := range []*nodeState{fine, coarse} {
			st.CPUUtil, st.CPUWorkClass = seg.cpuUtil, in.cpuClass
			st.GPUUtil, st.GPUWorkClass = seg.gpuUtil, in.gpuClass
			st.MemoryIntensity, st.IOIntensity, st.CPUFeedIntensity = memory, 0, 0
			st.TargetCapWatts = r.cpuCapW
			st.TargetThrottlePct = r.throttlePct
			st.GPUTargetCapWattsPerGpu = r.gpuLimitW
		}

		emuJ0 := diffEmuEnergy(node.State())
		var emuW, fineW diffSide
		for k := 1; k <= diffSegmentSeconds; k++ {
			diffAdvance(t, node, at.Add(time.Duration(k)*time.Second))
			s := node.State()
			cpuJ, gpuJ := stepNode(fine, model, 1, physMaxSubstep)
			r.fine.cpuJ += cpuJ
			r.fine.gpuJ += gpuJ
			if k%int(diffCoarseSubstep/time.Second) == 0 {
				cpuJ, gpuJ := stepNode(coarse, model, diffCoarseSubstep.Seconds(), diffCoarseSubstep)
				r.coarse.cpuJ += cpuJ
				r.coarse.gpuJ += gpuJ
			}
			if k > diffSegmentSeconds-diffMeanSeconds {
				cpuW, gpuW := diffEmuPower(s)
				emuW.cpuW += cpuW / diffMeanSeconds
				emuW.gpuW += gpuW / diffMeanSeconds
				fineW.cpuW += fine.CPUInstantPowerWatts / diffMeanSeconds
				fineW.gpuW += fine.GPUPowerWatts / diffMeanSeconds
			}
		}
		emuJ1 := diffEmuEnergy(node.State())
		r.emu = diffSide{cpuW: emuW.cpuW, gpuW: emuW.gpuW, cpuJ: emuJ1.cpuJ - emuJ0.cpuJ, gpuJ: emuJ1.gpuJ - emuJ0.gpuJ}
		r.fine.cpuW, r.fine.gpuW = fineW.cpuW, fineW.gpuW
		_, maxHz := diffCPUSpeed(node.State(), nil)
		cur, _ := diffCPUSpeed(node.State(), func(c hwemu.ComponentState) float64 { return c.SpeedHz })
		r.emuFreqScale, r.legacyFreqScale = cur/maxHz, fine.FreqScale
		out = append(out, r)
		elapsed += diffSegmentSeconds
	}
	return out
}

// diffCompare lists every reading of results outside its tolerance against
// the emulated node, one line each; none means the sides agree.
func diffCompare(results []diffResult) []string {
	var bad []string
	check := func(seg, what string, got, want, rel, abs float64, unit string) {
		tol := math.Max(abs, rel*math.Abs(want))
		if math.Abs(got-want) > tol {
			bad = append(bad, fmt.Sprintf("%s: %s %.3f %s, emulated %.3f %s (off by %.3f, tolerance %.3f)", seg, what, got, unit, want, unit, got-want, tol))
		}
	}
	for _, r := range results {
		meanRel := diffMeanRel
		if r.freqLimited {
			meanRel = diffFreqMeanRel
		}
		check(r.seg.name, "CPU mean power", r.fine.cpuW, r.emu.cpuW, meanRel, diffMeanAbsW, "W")
		check(r.seg.name, "GPU mean power", r.fine.gpuW, r.emu.gpuW, meanRel, diffMeanAbsW, "W")
		check(r.seg.name, "CPU energy", r.fine.cpuJ, r.emu.cpuJ, diffEnergyRel, 0, "J")
		check(r.seg.name, "GPU energy", r.fine.gpuJ, r.emu.gpuJ, diffEnergyRel, 0, "J")
		check(r.seg.name, "CPU energy with 5 s substeps", r.coarse.cpuJ, r.emu.cpuJ, diffCoarseEnergy, 0, "J")
		check(r.seg.name, "GPU energy with 5 s substeps", r.coarse.gpuJ, r.emu.gpuJ, diffCoarseEnergy, 0, "J")
	}
	return bad
}

// diffGPUSpread is the per-device utilization of n GPUs whose mean is util,
// spread as gpuDevicePowers spreads it: as many devices as the mean needs run
// at the utilization that gives it, the rest idle.
func diffGPUSpread(n int, util float64) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, n)
	active := min(n, int(math.Ceil(clamp01(util)*float64(n))))
	for i := 0; i < active; i++ {
		out[i] = clamp01(util * float64(n) / float64(active))
	}
	return out
}

// diffAdvance steps node to at and fails on any write it did not take as
// written, resolved or quantized: a limit the test meant to set that the
// node refused or put back would make the comparison meaningless.
func diffAdvance(t *testing.T, node *hwemu.Node, at time.Time) {
	t.Helper()
	if err := node.AdvanceTo(at); err != nil {
		t.Fatalf("advance to %v: %v", at, err)
	}
	for _, e := range node.State().Events {
		switch e.Kind {
		case hwemu.EventAccepted, hwemu.EventResolved, hwemu.EventQuantized:
		default:
			t.Fatalf("emulated node did not take a write: %s %s, written %q, effective %q", e.Kind, e.File, e.Written, e.Effective)
		}
	}
}

// diffEmuPower is the instantaneous CPU package and GPU power of s; DRAM and
// the rest of the platform have no counterpart in the legacy model.
func diffEmuPower(s hwemu.State) (cpuW, gpuW float64) {
	for _, c := range s.Components {
		switch c.Type {
		case semconv.HwTypeCPU:
			cpuW += c.PowerW
		case semconv.HwTypeGPU:
			gpuW += c.PowerW
		}
	}
	return cpuW, gpuW
}

// diffEmuEnergy is the CPU package and GPU energy of s since the node
// started.
func diffEmuEnergy(s hwemu.State) diffSide {
	var d diffSide
	for _, c := range s.Components {
		switch c.Type {
		case semconv.HwTypeCPU:
			d.cpuJ += c.EnergyJ
		case semconv.HwTypeGPU:
			d.gpuJ += c.EnergyJ
		}
	}
	return d
}

// diffCPUSpeed is the mean over the packages of s of read(package), and the
// highest cpuinfo_max_freq, both in Hz. A nil read gives 0.
func diffCPUSpeed(s hwemu.State, read func(hwemu.ComponentState) float64) (mean, maxHz float64) {
	n := 0
	for _, c := range s.Components {
		if c.Type != semconv.HwTypeCPU {
			continue
		}
		if read != nil {
			mean += read(c)
		}
		maxHz = math.Max(maxHz, c.SpeedMaxHz)
		n++
	}
	if n > 0 {
		mean /= float64(n)
	}
	return mean, maxHz
}

// diffLimitFile is one agent-writable limit of the tree: its path, the value
// it was rendered with, the highest value it takes, and the value last
// written to it.
type diffLimitFile struct {
	path     string
	rendered int64
	maxV     int64
	written  int64
}

// diffControls are the limits the test writes: the long_term (PL1) limit of
// every enabled package and die zone, the HSMP socket caps, every policy's
// scaling_max_freq, and every GPU's power limit (amdgpu power1_cap, an Intel
// GPU's power1_max or power2_max, NVML power_limit_mw) with its units per
// watt.
type diffControls struct {
	pl1, hsmp, freq, gpu []diffLimitFile
	gpuPerW              float64
}

var (
	diffConstraintLimit = regexp.MustCompile(`^constraint_(\d+)_power_limit_uw$`)
	diffPackageZone     = regexp.MustCompile(`^package-\d+(-die-\d+)?$`)
)

// diffFindControls finds the limits in the agent-writable files of tree.
func diffFindControls(t *testing.T, tree *hwemu.Tree) *diffControls {
	t.Helper()
	c := &diffControls{}
	for _, f := range tree.Files {
		if f.Owner != layout.OwnerAgent || f.Kind != layout.KindFile {
			continue
		}
		file := filepath.Join(tree.Root, filepath.FromSlash(f.Rel))
		dir, base := filepath.Dir(file), path.Base(f.Rel)
		sibling := func(name string) string { return filepath.Join(dir, name) }
		limit := func(maxFile string) diffLimitFile {
			v := diffReadInt(t, file)
			l := diffLimitFile{path: file, rendered: v, written: v}
			if maxFile != "" {
				l.maxV = diffReadInt(t, sibling(maxFile))
			}
			return l
		}
		switch {
		case diffConstraintLimit.MatchString(base):
			k := diffConstraintLimit.FindStringSubmatch(base)[1]
			if diffReadString(t, sibling("enabled")) != "1" || !diffPackageZone.MatchString(diffReadString(t, sibling("name"))) || diffReadString(t, sibling("constraint_"+k+"_name")) != "long_term" {
				continue
			}
			c.pl1 = append(c.pl1, limit("constraint_"+k+"_max_power_uw"))
		case base == "power1_cap" && strings.Contains(f.Rel, "/amd_hsmp/"):
			c.hsmp = append(c.hsmp, limit("power1_cap_max"))
		case base == "power1_cap", base == "power1_max", base == "power2_max":
			c.gpu, c.gpuPerW = append(c.gpu, limit("")), 1e6
		case base == "power_limit_mw" && strings.HasPrefix(f.Rel, path.Join(layout.StateDir, layout.NVMLDir)+"/"):
			c.gpu, c.gpuPerW = append(c.gpu, limit("")), 1000
		case base == "scaling_max_freq":
			c.freq = append(c.freq, limit("cpuinfo_max_freq"))
		}
	}
	return c
}

// write writes value(f) to every file of fs whose value it changes, in
// place, as the agent and the GPU tools write.
func (c *diffControls) write(t *testing.T, fs []diffLimitFile, value func(diffLimitFile) int64) {
	t.Helper()
	for i := range fs {
		v := value(fs[i])
		if v == fs[i].written {
			continue
		}
		if err := os.WriteFile(fs[i].path, []byte(strconv.FormatInt(v, 10)), 0); err != nil {
			t.Fatal(err)
		}
		fs[i].written = v
	}
}

// diffFraction is frac of f's highest value, or the value f was rendered with
// when frac is 0.
func diffFraction(frac float64) func(diffLimitFile) int64 {
	return func(f diffLimitFile) int64 {
		if frac <= 0 {
			return f.rendered
		}
		return int64(math.Round(frac * float64(f.maxV)))
	}
}

// writeCPUCap writes frac of each enabled PL1's maximum, or of each HSMP
// cap's on a node without one, as the profile adapter takes the PL1 first;
// frac 0 puts back the rendered limits.
func (c *diffControls) writeCPUCap(t *testing.T, frac float64) {
	t.Helper()
	if len(c.pl1) > 0 {
		c.write(t, c.pl1, diffFraction(frac))
		return
	}
	c.write(t, c.hsmp, diffFraction(frac))
}

// writeFreq writes frac of cpuinfo_max_freq to every scaling_max_freq, or
// puts back the rendered value when frac is 0.
func (c *diffControls) writeFreq(t *testing.T, frac float64) {
	t.Helper()
	c.write(t, c.freq, diffFraction(frac))
}

// writeGPULimit writes frac of maxW, or maxW when frac is 0, as every GPU's
// power limit, in whole watts as amdgpu keeps it, and returns the limit in W.
func (c *diffControls) writeGPULimit(t *testing.T, frac, maxW float64) float64 {
	t.Helper()
	if len(c.gpu) == 0 {
		return 0
	}
	if frac <= 0 {
		frac = 1
	}
	w := math.Round(frac * maxW)
	c.write(t, c.gpu, func(diffLimitFile) int64 { return int64(math.Round(w * c.gpuPerW)) })
	return w
}

func diffReadString(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func diffReadInt(t *testing.T, file string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(diffReadString(t, file), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return v
}
