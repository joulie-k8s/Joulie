package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Config is a sweep file: the grids to run, each a cross product of its
// axes, every cell once per seed. Every governor arm names a vendor-neutral
// preset (pkg/api); the sweep turns it into native writes through
// pkg/governor, as the agent does, so a native word in a config is an
// error.
type Config struct {
	// Name names the sweep: the k8s.cluster.name of its OTLP export and
	// the prefix of its output files.
	Name string `json:"name"`
	// Seeds run every cell once each. hwemu has no randomness of its own:
	// a seed moves the first energy counter values, the phase of the job
	// against the physics substeps and the arrival jitter of bursts.
	Seeds []uint64 `json:"seeds"`
	// DurationS is how long a job runs at the reference (no cap, the
	// performance governor or the auto GPU preset, nothing slowed), and how
	// long an idle cell lasts. SettleS is the idle time after the arm is set
	// and before the job starts; StepS the step the job advances by;
	// SubstepMS the physics substep (hwemu.Options.Substep). LimitFactor
	// stops a job that has not finished after LimitFactor x DurationS.
	DurationS   float64 `json:"durationS"`
	SettleS     float64 `json:"settleS"`
	StepS       float64 `json:"stepS"`
	SubstepMS   int     `json:"substepMS"`
	LimitFactor float64 `json:"limitFactor"`

	CPU         *CPUGrid      `json:"cpu,omitempty"`
	GPU         *GPUGrid      `json:"gpu,omitempty"`
	Sensitivity []Sensitivity `json:"sensitivity,omitempty"`
}

// CPUGrid is experiment E-G1: CPU-only profiles under each CPU preset.
type CPUGrid struct {
	Profiles []string `json:"profiles"`
	Arms     []CPUArm `json:"arms"`
	// Idle adds an idle cell per profile and arm; SteadyPct the steady
	// loads, in percent of every logical CPU, each with every class.
	Idle      bool                 `json:"idle"`
	SteadyPct []float64            `json:"steadyPct"`
	Classes   map[string]Intensity `json:"classes"`
	Bursty    *Bursty              `json:"bursty,omitempty"`
}

// CPUArm is one CPU governor arm: a pkg/api CPU preset, or ArmDefault for
// the profile's own governor with nothing written, and a boost word (on,
// off, or empty to leave boost as the profile sets it).
type CPUArm struct {
	Preset string `json:"preset"`
	Boost  string `json:"boost,omitempty"`
}

// ArmDefault is the CPU arm that writes nothing: the governor the profile
// renders, as a node runs before Joulie touches it.
const ArmDefault = "default"

// Intensity is a workload class's memory and IO intensity, the
// hwemu.Load and workload.RateInputs fields of the same names.
type Intensity struct {
	MemoryIntensity float64 `json:"memoryIntensity"`
	IOIntensity     float64 `json:"ioIntensity"`
}

// Bursty is the bursty CPU load: Bursts bursts of BurstMS at full load on
// every logical CPU, one every PeriodMS, each arriving up to JitterMS late
// (seeded), of class Class. A burst still running when the next arrives
// delays it. Its cells run with SubstepMS as both step and substep.
type Bursty struct {
	Class     string `json:"class"`
	BurstMS   int    `json:"burstMS"`
	PeriodMS  int    `json:"periodMS"`
	JitterMS  int    `json:"jitterMS"`
	Bursts    int    `json:"bursts"`
	SubstepMS int    `json:"substepMS"`
}

// GPUGrid is experiment E-G2: GPU profiles under each GPU preset, cap and
// receiver gate.
type GPUGrid struct {
	Profiles []string `json:"profiles"`
	Arms     []GPUArm `json:"arms"`
	// CapsPct are GPU power caps in percent of the board maximum, set as
	// the agent sets them (nvidia-smi -pl, rocm-smi --setpoweroverdrive).
	CapsPct []float64 `json:"capsPct"`
	// ReceiverGates are hwemu.Options.ReceiverGate values. A gate other
	// than profile runs only for jobs with peer transfers, the only work it
	// changes.
	ReceiverGates []string             `json:"receiverGates"`
	Classes       map[string]Intensity `json:"classes"`
	Jobs          []GPUJob             `json:"jobs"`
	// Idle adds an idle cell per profile, arm and cap.
	Idle bool `json:"idle"`
	// CPUUtil is the host CPU load of every GPU cell, in 0..1.
	CPUUtil float64 `json:"cpuUtil"`
}

// GPUArm is one GPU governor arm: a pkg/api GPU preset and, for efficient,
// the compute clock ceilings in percent of the maximum, one cell each.
type GPUArm struct {
	Preset     string    `json:"preset"`
	CeilingPct []float64 `json:"ceilingPct,omitempty"`
}

// GPUJob is a job on GPUs 0..GPUs-1 at full load. Peer lists peer transfer
// intensities: the share of the job's GPU time spent in a ring of GPU to
// GPU copies over the peer link, one cell each. NIC lists the same for
// copies from the host or a NIC into each GPU, which no receiver gate
// slows. Gather lists the same for a job that computes on GPUs 1..GPUs-1,
// each sending to GPU 0, which runs nothing: the idle receiver of the
// receiver gate. A job with none moves no data.
type GPUJob struct {
	GPUs   int       `json:"gpus"`
	Peer   []float64 `json:"peer,omitempty"`
	NIC    []float64 `json:"nic,omitempty"`
	Gather []float64 `json:"gather,omitempty"`
}

// Sensitivity sweeps one assumed fact over Values, one at a time: every
// cell of a profile that has the fact runs again once per value, with only
// that fact changed (section 5.4 of the design).
type Sensitivity struct {
	Fact   string    `json:"fact"`
	Values []float64 `json:"values"`
}

// sensitivityFacts are the facts a Sensitivity may name: the grid whose
// cells the fact changes, and what it sets on a profile. ok is false when
// the profile has no such fact, and the profile then runs without a
// variant. The CPU facts vary only CPU cells, since a GPU cell leaves the
// host's governor as the profile renders it.
var sensitivityFacts = map[string]struct {
	grid string
	set  func(p *hwemu.Profile, v float64) (ok bool)
}{
	"idleGovernorDeltaPctOfIdle": {gridCPU, func(p *hwemu.Profile, v float64) bool {
		c := &p.Physics.CPU
		c.IdleGovernorDeltaW = sensFact(v / 100 * c.IdlePkgW.V)
		return p.CPUFreq != nil
	}},
	"eppGain": {gridCPU, func(p *hwemu.Profile, v float64) bool {
		f := sensFact(v)
		p.Physics.CPU.EPPGain = &f
		return p.CPUFreq != nil && p.CPUFreq.EPP != nil
	}},
	"eppCeilingDrop": {gridCPU, func(p *hwemu.Profile, v float64) bool {
		f := sensFact(v)
		p.Physics.CPU.EPPCeilingDrop = &f
		return p.CPUFreq != nil && p.CPUFreq.EPP != nil
	}},
	"dataPathHoldW":   {gridGPU, sensGPU(func(p *hwemu.Profile) *hwemu.Fact[float64] { return &p.Physics.GPU.DataPathHoldW })},
	"gatingOffIdleW":  {gridGPU, sensGPU(func(p *hwemu.Profile) *hwemu.Fact[float64] { return &p.Physics.GPU.GatingOffIdleW })},
	"clockBeta":       {gridGPU, sensGPU(func(p *hwemu.Profile) *hwemu.Fact[float64] { return &p.Physics.GPU.ClockBeta })},
	"memoryKneeFrac":  {gridGPU, sensGPU(func(p *hwemu.Profile) *hwemu.Fact[float64] { return &p.Physics.GPU.MemoryKneeFrac })},
	"memoryKneeGamma": {gridGPU, sensGPU(func(p *hwemu.Profile) *hwemu.Fact[float64] { return &p.Physics.GPU.MemoryKneeGamma })},
	"slowFraction": {gridGPU, func(p *hwemu.Profile, v float64) bool {
		if p.GPUs == nil || p.GPUs.Transfer == nil {
			return false
		}
		p.GPUs.Transfer.ReceiverGate.SlowFraction = sensFact(v)
		return true
	}},
	"autoHoldS": {gridGPU, func(p *hwemu.Profile, v float64) bool {
		if p.GPUs == nil || p.GPUs.DPM == nil {
			return false
		}
		p.GPUs.DPM.AutoHoldS = sensFact(v)
		return true
	}},
}

func sensFact(v float64) hwemu.Fact[float64] {
	return hwemu.Fact[float64]{V: v, Src: "assumed:govsweep sensitivity value"}
}

func sensGPU(field func(p *hwemu.Profile) *hwemu.Fact[float64]) func(p *hwemu.Profile, v float64) bool {
	return func(p *hwemu.Profile, v float64) bool {
		if p.GPUs == nil || p.GPUs.Count.V <= 0 {
			return false
		}
		*field(p) = sensFact(v)
		return true
	}
}

// loadConfig reads and checks a sweep file. Unknown keys are errors, so a
// native setting cannot ride in under a key the sweep ignores.
func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.StepS == 0 {
		c.StepS = 1
	}
	if c.SubstepMS == 0 {
		c.SubstepMS = 100
	}
	if c.LimitFactor == 0 {
		c.LimitFactor = 10
	}
	if b := c.CPU; b != nil && b.Bursty != nil && b.Bursty.SubstepMS == 0 {
		b.Bursty.SubstepMS = 10
	}
}

// configNameRE is a sweep name: it names files and a cluster.
var configNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// nativeWordHint ends the error for an arm that names something other than a
// preset.
const nativeWordHint = "a governor arm names a vendor-neutral preset; native words (governors, EPP values, amdgpu levels, clock locks) are written only through pkg/governor"

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !configNameRE.MatchString(c.Name) {
		bad("name %q is not a lower-case DNS label", c.Name)
	}
	if len(c.Seeds) == 0 {
		bad("seeds: at least one seed")
	}
	if c.DurationS <= 0 || c.SettleS < 0 || c.StepS <= 0 || c.SubstepMS <= 0 || c.LimitFactor < 1 {
		bad("durationS and stepS must be positive, settleS not negative, substepMS positive, limitFactor at least 1")
	}
	if c.CPU == nil && c.GPU == nil {
		bad("no cpu and no gpu grid")
	}
	if g := c.CPU; g != nil {
		if len(g.Profiles) == 0 || len(g.Arms) == 0 {
			bad("cpu: profiles and arms must not be empty")
		}
		for i, a := range g.Arms {
			if a.Preset != ArmDefault && !slices.Contains(joulie.CPUGovernorPresetValues, a.Preset) {
				bad("cpu.arms[%d].preset %q is not one of %s or %s: %s", i, a.Preset, strings.Join(joulie.CPUGovernorPresetValues, ", "), ArmDefault, nativeWordHint)
			}
			if a.Boost != "" && !slices.Contains(joulie.CPUGovernorBoostValues, a.Boost) {
				bad("cpu.arms[%d].boost %q is not one of %s: %s", i, a.Boost, strings.Join(joulie.CPUGovernorBoostValues, ", "), nativeWordHint)
			}
			if a.Preset == ArmDefault && a.Boost != "" {
				bad("cpu.arms[%d]: the default arm writes nothing, boost included", i)
			}
		}
		for _, p := range g.SteadyPct {
			if p <= 0 || p > 100 {
				bad("cpu.steadyPct %v is not in (0, 100]", p)
			}
		}
		if len(g.SteadyPct) > 0 && len(g.Classes) == 0 {
			bad("cpu: steady loads need at least one class")
		}
		for name, in := range g.Classes {
			checkClass(bad, "cpu", name, "cpu.", in)
		}
		if b := g.Bursty; b != nil {
			if !strings.HasPrefix(b.Class, "cpu.") {
				bad("cpu.bursty.class %q is not a cpu.* class", b.Class)
			}
			if _, ok := g.Classes[b.Class]; !ok {
				bad("cpu.bursty.class %q is not under cpu.classes", b.Class)
			}
			if b.BurstMS <= 0 || b.PeriodMS < b.BurstMS || b.JitterMS < 0 || b.Bursts <= 0 || b.SubstepMS <= 0 || b.BurstMS%b.SubstepMS != 0 {
				bad("cpu.bursty: burstMS positive and a multiple of substepMS, periodMS at least burstMS, jitterMS not negative, bursts positive")
			}
		}
	}
	if g := c.GPU; g != nil {
		if len(g.Profiles) == 0 || len(g.Arms) == 0 || len(g.CapsPct) == 0 || len(g.ReceiverGates) == 0 {
			bad("gpu: profiles, arms, capsPct and receiverGates must not be empty")
		}
		for i, a := range g.Arms {
			if !slices.Contains(joulie.GPUGovernorPresetValues, a.Preset) {
				bad("gpu.arms[%d].preset %q is not one of %s: %s", i, a.Preset, strings.Join(joulie.GPUGovernorPresetValues, ", "), nativeWordHint)
			}
			if a.Preset != joulie.GPUGovernorPresetEfficient && len(a.CeilingPct) > 0 {
				bad("gpu.arms[%d]: ceilingPct is for efficient only", i)
			}
			for _, p := range a.CeilingPct {
				if p <= 0 || p > 100 {
					bad("gpu.arms[%d].ceilingPct %v is not in (0, 100]", i, p)
				}
			}
		}
		for _, p := range g.CapsPct {
			if p <= 0 || p > 100 {
				bad("gpu.capsPct %v is not in (0, 100]", p)
			}
		}
		for _, gate := range g.ReceiverGates {
			switch gate {
			case hwemu.ReceiverGateProfile, hwemu.ReceiverGateOff, hwemu.ReceiverGateOn:
			default:
				bad("gpu.receiverGates %q is not %s, %s or %s", gate, hwemu.ReceiverGateProfile, hwemu.ReceiverGateOff, hwemu.ReceiverGateOn)
			}
		}
		if len(g.Jobs) > 0 && len(g.Classes) == 0 {
			bad("gpu: jobs need at least one class")
		}
		for name, in := range g.Classes {
			checkClass(bad, "gpu", name, "gpu.", in)
		}
		for i, j := range g.Jobs {
			if j.GPUs <= 0 {
				bad("gpu.jobs[%d].gpus %d is not positive", i, j.GPUs)
			}
			if j.GPUs < 2 && len(j.Peer)+len(j.Gather) > 0 {
				bad("gpu.jobs[%d]: peer and gather transfers need at least 2 GPUs", i)
			}
			for _, p := range append(append(slices.Clone(j.Peer), j.NIC...), j.Gather...) {
				if p <= 0 || p > 1 {
					bad("gpu.jobs[%d]: transfer intensity %v is not in (0, 1]", i, p)
				}
			}
		}
		if g.CPUUtil < 0 || g.CPUUtil > 1 {
			bad("gpu.cpuUtil %v is not in [0, 1]", g.CPUUtil)
		}
	}
	for _, s := range c.Sensitivity {
		if _, ok := sensitivityFacts[s.Fact]; !ok {
			bad("sensitivity.fact %q is not one of %s", s.Fact, strings.Join(sensitivityFactNames(), ", "))
		}
		if len(s.Values) == 0 {
			bad("sensitivity %s has no values", s.Fact)
		}
	}
	if len(errs) == 0 {
		// A value repeated on an axis gives two cells one id, and so one
		// root under the work directory, which parallel cells would share.
		seen := map[string]bool{}
		for _, cell := range c.expand() {
			id := cell.ID()
			if seen[id] {
				bad("cell %s comes twice: a value repeats on one of its axes", id)
				break
			}
			seen[id] = true
		}
	}
	return errors.Join(errs...)
}

func checkClass(bad func(string, ...any), grid, name, prefix string, in Intensity) {
	if !strings.HasPrefix(name, prefix) {
		bad("%s.classes: %q is not a %s* workload class", grid, name, prefix)
	}
	if in.MemoryIntensity < 0 || in.MemoryIntensity > 1 || in.IOIntensity < 0 || in.IOIntensity > 1 {
		bad("%s.classes.%s: intensities must be in [0, 1]", grid, name)
	}
}

func sensitivityFactNames() []string {
	out := make([]string, 0, len(sensitivityFacts))
	for k := range sensitivityFacts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedClasses is the class names of m in order, so a grid expands the
// same way every run.
func sortedClasses(m map[string]Intensity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fmtNum spells a number of a config value in a cell id: 0.3 as 0.3, 75 as 75.
func fmtNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
