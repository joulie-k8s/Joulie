package hwemu

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

const nodeDefaultSubstep = 100 * time.Millisecond

// nodeDefaultMinScale is the frequency floor of a package without cpufreq
// information, the simulator's minFreqScale default.
const nodeDefaultMinScale = 0.35

// nodeDRMClassDir is where DRM cards are listed (drm_sysfs.c, class "drm").
const nodeDRMClassDir = "class/drm"

var (
	nodeCardName       = regexp.MustCompile(`^card([0-9]+)$`)
	nodePackageName    = regexp.MustCompile(`^package-([0-9]+)$`)
	nodePackageDieName = regexp.MustCompile(`^package-([0-9]+)-die-([0-9]+)$`)
)

// Node runs the physics of one rendered tree. Each Step reads back what the
// agent and the GPU tools wrote, advances power, energy, frequency and limits
// in substeps on a fixed grid, and writes the readings that changed. Its
// methods are safe for concurrent use; file I/O happens under the Node's own
// lock only.
type Node struct {
	mu sync.Mutex

	profile   *Profile
	tree      *Tree
	root      string
	realRoot  string
	sysRoot   string
	stateRoot string
	tmpDir    string
	owners    map[string]layout.FileSpec
	substepD  time.Duration
	frozen    bool
	leases    bool // A-file rewrites run under a lease (nodeLeasesWork)

	now  time.Time // the node's clock
	grid time.Time // last substep boundary: the physics state holds there
	segT time.Time // start of the current constant-power segment

	topo      nodeTopology
	load      Load
	cpuUtil   []float64
	cpuPolicy []*nodePolicy

	cpuModel     phys.PowerModel
	gpuModel     phys.PowerModel
	floorW       float64
	coreFraction float64
	raplFixedTau float64 // < 0: each constraint's time window
	rampTau      float64
	hsmpTau      float64
	gpuTau       float64
	gpuWindow    float64

	pkgs     []*nodePackage
	zones    []*nodeZone
	policies []*nodePolicy
	gpus     []*nodeGPU
	other    nodeChannel

	afiles []*nodeAFile
	rfiles []*nodeRFile
	events []Event
}

// nodeTopology locates logical CPUs. Numbering socket-major-smt-last puts cpu k
// on socket (k mod S*C)/C, its SMT siblings S*C apart (corpus: 192 of 192
// CPUs match).
type nodeTopology struct {
	sockets, cores, threads, dies int
}

func (t nodeTopology) cpus() int { return t.sockets * t.cores * t.threads }

func (t nodeTopology) socketOf(cpu int) int { return (cpu % (t.sockets * t.cores)) / t.cores }

// NewNode builds the physics of tree t, rendered from profile p. Counters,
// limits, frequencies and NVML state are seeded from the files on disk rather
// than from the profile, so a Node created over a live tree continues where
// the last one stopped and an energy counter never reads as a reset.
// There is no randomness: the same tree, options, loads and steps give the
// same files. A zero o.Start means time.Now(). A Frozen node reads back and
// restores files but advances no reading, which keeps a captured tree as
// captured.
func NewNode(p *Profile, t *Tree, o Options) (*Node, error) {
	if p == nil || t == nil || t.Root == "" {
		return nil, errors.New("hwemu: NewNode needs a profile and a rendered tree")
	}
	n := &Node{
		profile:   p,
		tree:      t,
		root:      t.Root,
		realRoot:  t.Root,
		sysRoot:   t.SysRoot,
		stateRoot: t.StateRoot,
		owners:    map[string]layout.FileSpec{},
		substepD:  o.Substep,
		frozen:    o.Frozen,
	}
	if r, err := filepath.EvalSymlinks(t.Root); err == nil {
		n.realRoot = r
	}
	if n.sysRoot == "" {
		n.sysRoot = filepath.Join(t.Root, layout.SysDir)
	}
	if n.stateRoot == "" {
		n.stateRoot = filepath.Join(t.Root, layout.StateDir)
	}
	n.tmpDir = filepath.Join(n.stateRoot, layout.TmpDir)
	if err := os.MkdirAll(n.tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("hwemu: %w", err)
	}
	n.leases = nodeLeases && nodeLeasesWork(n.tmpDir)
	if n.substepD <= 0 {
		n.substepD = nodeDefaultSubstep
	}
	start := o.Start
	if start.IsZero() {
		start = time.Now()
	}
	n.now, n.grid, n.segT = start, start, start
	for _, f := range t.Files {
		n.owners[f.Rel] = f
	}

	steps := []func() error{n.buildTopology, n.buildModels, n.buildPowercap, n.buildCPUFreq, n.buildHSMP, n.buildGPUs, n.snapshotRenderFiles}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	n.cpuUtil = make([]float64, n.topo.cpus())
	n.derive()
	n.assign()
	n.seedWindows()
	return n, nil
}

// SetLoad sets the work the node runs from now on. CPUUtil holds one value for
// every package, one for every logical CPU, or a single value for all; GPUUtil
// one value per GPU or a single value for all. Missing entries are idle.
func (n *Node) SetLoad(l Load) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.load = Load{
		CPUUtil:         append([]float64(nil), l.CPUUtil...),
		CPUClass:        l.CPUClass,
		GPUUtil:         append([]float64(nil), l.GPUUtil...),
		GPUClass:        l.GPUClass,
		MemoryIntensity: l.MemoryIntensity,
	}
	for c := range n.cpuUtil {
		n.cpuUtil[c] = n.loadCPUUtil(c)
	}
	n.derive()
	n.adopt(n.now)
}

func (n *Node) loadCPUUtil(cpu int) float64 {
	u := n.load.CPUUtil
	switch {
	case len(u) == 0:
		return 0
	case len(u) == 1:
		return u[0]
	case len(u) == len(n.cpuUtil):
		return u[cpu]
	}
	if s := n.topo.socketOf(cpu); s < len(u) {
		return u[s]
	}
	return 0
}

func (n *Node) gpuUtil(i int) float64 {
	u := n.load.GPUUtil
	switch {
	case len(u) == 1:
		return u[0]
	case i < len(u):
		return u[i]
	}
	return 0
}

// Step advances the node by dt: one readback, then whole substeps up to the
// new time, then the write of every reading that changed. A dt that ends
// between two substep boundaries is integrated at the powers in effect, and the
// next Step continues from the last boundary.
func (n *Node) Step(dt time.Duration) error {
	if dt < 0 {
		return fmt.Errorf("hwemu: step by negative %v", dt)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.step(n.now.Add(dt))
}

// AdvanceTo steps the node to t. A t before the node's time is an error.
func (n *Node) AdvanceTo(t time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t.Before(n.now) {
		return fmt.Errorf("hwemu: advance to %v, before the node's time %v", t, n.now)
	}
	return n.step(t)
}

func (n *Node) step(end time.Time) error {
	n.events = nil
	err := n.readback()
	if n.frozen {
		n.now, n.grid, n.segT = end, end, end
		return err
	}
	for next := n.grid.Add(n.substepD); !next.After(end); next = n.grid.Add(n.substepD) {
		n.substep(next)
	}
	n.now = end
	return errors.Join(err, n.writeOutputs())
}

// State returns a snapshot after the last step, with that step's events.
// PackageState.EnergyUJ is the package zone's energy_uj without its wrap: the
// value on disk when the Node started plus the energy since.
func (n *Node) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	dt := n.now.Sub(n.segT).Seconds()
	s := State{Time: n.now, Events: append([]Event(nil), n.events...)}
	for _, pk := range n.pkgs {
		s.Packages = append(s.Packages, PackageState{
			Socket:        pk.socket,
			PowerW:        pk.pwr.W,
			CapW:          pk.capW,
			RequestedCapW: pk.requestedW,
			FreqScale:     pk.effScale,
			EnergyUJ:      pk.seedUJ + int64(math.Floor((pk.pwr.J+pk.pwr.W*dt)*1e6)),
		})
	}
	for _, g := range n.gpus {
		s.GPUs = append(s.GPUs, GPUState{Index: g.index, PowerW: g.pwr.W, AvgPowerW: g.avgW, LimitW: g.limitW(), EnforcedLimitW: g.settledW})
	}
	return s
}

func (n *Node) relPath(abs string) string {
	for _, root := range []string{n.root, n.realRoot} {
		if rel, err := filepath.Rel(root, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(abs)
}

func (n *Node) rootPath(rel string) string { return filepath.Join(n.root, filepath.FromSlash(rel)) }

func (n *Node) sysPath(rel string) string { return filepath.Join(n.sysRoot, filepath.FromSlash(rel)) }

func (n *Node) buildTopology() error {
	c := n.profile.CPU
	n.topo = nodeTopology{sockets: max(1, c.Sockets.V), cores: max(1, c.CoresPerSocket.V), threads: max(1, c.ThreadsPerCore.V), dies: max(1, c.DiesPerPackage.V)}
	if c.Numbering != "" && c.Numbering != "socket-major-smt-last" {
		return fmt.Errorf("hwemu: profile %s: unknown cpu numbering %q", n.profile.Name, c.Numbering)
	}
	n.cpuPolicy = make([]*nodePolicy, n.topo.cpus())
	minScale := nodeDefaultMinScale
	if cf := n.profile.CPUFreq; cf != nil && cf.CPUInfoMaxKHz.V > 0 && cf.CPUInfoMinKHz.V > 0 {
		minScale = float64(cf.CPUInfoMinKHz.V) / float64(cf.CPUInfoMaxKHz.V)
	}
	ph := n.profile.Physics
	for s := 0; s < n.topo.sockets; s++ {
		n.pkgs = append(n.pkgs, &nodePackage{socket: s, minScale: minScale, natScale: 1, effScale: 1, dram: nodeChannel{W: ph.DRAM.WattsPerSocket.V}})
	}
	for cpu := 0; cpu < n.topo.cpus(); cpu++ {
		pk := n.pkgs[n.topo.socketOf(cpu)]
		pk.cpus = append(pk.cpus, cpu)
	}
	n.other.W = ph.Platform.OtherW.V
	return nil
}

func (n *Node) buildModels() error {
	ph := n.profile.Physics
	switch ph.CPU.Model {
	case "", "analytic":
		n.cpuModel = phys.AnalyticCPUModel{IdleW: ph.CPU.IdlePkgW.V, MaxW: ph.CPU.MaxPkgW.V, AlphaUtil: ph.CPU.AlphaUtil.V, BetaFreq: ph.CPU.BetaFreq.V, Knee: ph.CPU.Knee.V}
	case "catalog-curve":
		points, err := nodeCatalogCurve(n.profile.CPU.ModelName.V)
		if err != nil {
			return fmt.Errorf("hwemu: profile %s: %w", n.profile.Name, err)
		}
		n.cpuModel = phys.MeasuredCurveCPUModel{Points: points, Knee: ph.CPU.Knee.V}
	default:
		return fmt.Errorf("hwemu: profile %s: unknown cpu model %q", n.profile.Name, ph.CPU.Model)
	}
	// MaxCapWatts stays 0, so the 90 % idle floor of
	// CappedBoardGPUModel.Power never fires.
	n.gpuModel = phys.CappedBoardGPUModel{IdleW: ph.GPU.IdleW.V, MaxW: ph.GPU.NaturalMaxW.V, ComputeGamma: ph.GPU.ComputeGamma.V, MemoryEpsilon: ph.GPU.MemoryEpsilon.V, MemoryGamma: ph.GPU.MemoryGamma.V}
	n.floorW = ph.RAPL.FloorW.V
	n.coreFraction = ph.CPU.CoreFraction.V
	n.raplFixedTau = -1
	switch ph.RAPL.SettleTau {
	case "", "timeWindow":
	default:
		d, err := time.ParseDuration(ph.RAPL.SettleTau)
		if err != nil || d < 0 {
			return fmt.Errorf("hwemu: profile %s: physics.rapl.settleTau %q is neither timeWindow nor a duration", n.profile.Name, ph.RAPL.SettleTau)
		}
		n.raplFixedTau = d.Seconds()
	}
	n.rampTau = float64(ph.CPU.FreqRampMS.V) / 1000
	n.hsmpTau = float64(ph.HSMP.SettleTauMS.V) / 1000
	n.gpuTau = float64(ph.GPU.CapSettleTauMS.V) / 1000
	n.gpuWindow = float64(ph.GPU.TelemetryWindowMS.V) / 1000
	if n.gpuWindow <= 0 {
		n.gpuWindow = 1 // NVML averages power over 1 s; assumed for amdgpu
	}
	return nil
}

// nodeCatalogCurve is the per-package curve of a catalog CPU: the node2S
// SPECpower curve split over its two sockets (assumed: SPECpower measures wall
// power, so the share overstates the package).
func nodeCatalogCurve(modelName string) ([]hw.PowerPoint, error) {
	cat, err := hw.LoadCatalog("")
	if err != nil {
		return nil, err
	}
	_, spec, ok := cat.MatchCPU(modelName)
	if !ok || spec.MeasuredCurves == nil || spec.MeasuredCurves.Node2S == nil || len(spec.MeasuredCurves.Node2S.Points) == 0 {
		return nil, fmt.Errorf("no catalog curve for cpu %q", modelName)
	}
	var points []hw.PowerPoint
	for _, pt := range spec.MeasuredCurves.Node2S.Points {
		points = append(points, hw.PowerPoint{LoadPct: pt.LoadPct, PowerW: pt.PowerW / 2})
	}
	return points, nil
}

// buildPowercap expands the profile's zones the way Render lays them out: in
// listing order, per-package zones once per socket, per-die zones once per die
// at index X*D+Y, once zones once, and children nested under each copy.
func (n *Node) buildPowercap() error {
	pc := n.profile.Powercap
	if pc == nil {
		return nil
	}
	for _, ct := range pc.ControlTypes {
		ctEnabled := 1
		h, cur := nodeBoolHandler(&ctEnabled) // read back only: a control type's enabled is not modelled (assumed)
		n.addAFile(filepath.Join(n.sysPath(layout.PowercapControlDir(ct.Name)), "enabled"), h, cur)

		var zones []*nodeZone
		idx := 0
		add := func(zs ZoneSpec, role, socket int, name string) error {
			idx++
			return n.buildZone(ct.Name, []int{idx - 1}, nil, zs, role, socket, name, &zones)
		}
		for _, zs := range ct.Zones {
			switch zs.Repeat {
			case "per-package":
				for x := 0; x < n.topo.sockets; x++ {
					if err := add(zs, nodeRolePackage, x, nodeZoneName(zs.NameFormat, x)); err != nil {
						return err
					}
				}
			case "per-die":
				for x := 0; x < n.topo.sockets; x++ {
					for y := 0; y < n.topo.dies; y++ {
						if err := add(zs, nodeRoleDie, x, nodeZoneName(zs.NameFormat, x, y)); err != nil {
							return err
						}
					}
				}
			case "once", "":
				name := nodeZoneName(zs.NameFormat, 0)
				role, socket := nodeRoleOf(name)
				if err := add(zs, role, socket, name); err != nil {
					return err
				}
			default:
				return fmt.Errorf("hwemu: profile %s: zone repeat %q", n.profile.Name, zs.Repeat)
			}
		}
		dies := map[int]int{}
		for _, z := range zones {
			if z.role == nodeRoleDie {
				dies[z.socket]++
			}
		}
		for _, z := range zones {
			z.dies = dies[z.socket]
			if (z.role == nodeRolePackage || z.role == nodeRoleDie) && z.socket < len(n.pkgs) {
				pk := n.pkgs[z.socket]
				pk.limits = append(pk.limits, z)
			}
		}
		n.zones = append(n.zones, zones...)
	}
	// The package's energy as the agent reads it: its first package zone, or
	// the sum of its die zones.
	for _, pk := range n.pkgs {
		for _, z := range pk.limits {
			if z.energy == nil {
				continue
			}
			if z.role == nodeRolePackage {
				pk.seedUJ = z.seedUJ
				break
			}
			pk.seedUJ += z.seedUJ
		}
	}
	return nil
}

// nodeZoneName fills a zone's name file from its format, such as package-%d
// or package-%d-die-%d: a format without verbs is the name itself.
func nodeZoneName(format string, args ...int) string {
	verbs := strings.Count(format, "%d")
	vals := make([]any, verbs)
	for i := range vals {
		if i < len(args) {
			vals[i] = args[i]
		} else {
			vals[i] = 0
		}
	}
	if verbs == 0 {
		return format
	}
	return fmt.Sprintf(format, vals...)
}

// nodeRoleOf classifies a zone by its name, as intel_rapl_common.c names them.
func nodeRoleOf(name string) (role, socket int) {
	if m := nodePackageName.FindStringSubmatch(name); m != nil {
		s, _ := strconv.Atoi(m[1])
		return nodeRolePackage, s
	}
	if m := nodePackageDieName.FindStringSubmatch(name); m != nil {
		s, _ := strconv.Atoi(m[1])
		return nodeRoleDie, s
	}
	switch name {
	case "psys":
		return nodeRolePsys, 0
	case "dram":
		return nodeRoleDRAM, 0
	case "core":
		return nodeRoleCore, 0
	}
	return nodeRoleOther, 0
}

func nodeConstraintKind(name string) int {
	switch name {
	case "long_term":
		return nodePL1
	case "short_term":
		return nodePL2
	case "peak_power":
		return nodePL4
	}
	return nodePLOther
}

// buildZone builds the zone at idx under parent and its children, seeding
// enabled, limits, time windows and the energy counter from the files.
func (n *Node) buildZone(ct string, idx []int, parent *nodeZone, zs ZoneSpec, role, socket int, name string, out *[]*nodeZone) error {
	rel := layout.PowercapZoneDir(ct, idx...)
	dir := n.sysPath(rel)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("hwemu: profile %s: zone %s (%s) is not in the tree", n.profile.Name, rel, name)
	}
	z := &nodeZone{role: role, socket: socket, parent: parent, enabled: zs.Enabled.V, powerUnitUW: zs.PowerUnitUW.V}
	*out = append(*out, z)

	enabled := filepath.Join(dir, "enabled")
	if h, cur := nodeBoolHandler(&z.enabled); !n.addAFile(enabled, h, cur) {
		if v, ok := nodeReadInt(enabled); ok {
			z.enabled = int(v)
		}
	}
	if sf := n.sfile(filepath.Join(dir, "energy_uj")); sf != nil {
		unit := zs.EnergyUnitNJ.V
		if unit <= 0 {
			if r, ok := nodeReadInt(filepath.Join(dir, "max_energy_range_uj")); ok && r > 0 {
				unit = nodeUnitFromRange(r)
			} else if zs.MaxEnergyRangeUJ != nil && zs.MaxEnergyRangeUJ.V > 0 {
				unit = nodeUnitFromRange(zs.MaxEnergyRangeUJ.V)
			}
		}
		if unit <= 0 {
			return fmt.Errorf("hwemu: profile %s: zone %s has energy_uj but no energy unit", n.profile.Name, rel)
		}
		uj, _ := nodeReadInt(sf.path)
		z.counter, z.energy, z.seedUJ = nodeNewCounter(unit, uj), sf, uj
	}
	for i, cs := range zs.Constraints {
		c := &nodeConstraint{kind: nodeConstraintKind(cs.Name), limitUW: cs.PowerLimitUW.V, maxUW: cs.MaxPowerUW.V, windowUS: cs.TimeWindowUS.V, rejectWrites: cs.RejectWrites.V}
		prefix := filepath.Join(dir, fmt.Sprintf("constraint_%d_", i))
		if v, ok := nodeReadInt(prefix + "max_power_uw"); ok {
			c.maxUW = v
		}
		// A rejecting constraint is a directory: its limit stays the profile's.
		if h, cur := c.limitHandler(z.powerUnitUW); !n.addAFile(prefix+"power_limit_uw", h, cur) {
			if v, ok := nodeReadInt(prefix + "power_limit_uw"); ok {
				c.limitUW = v
			}
		}
		if h, cur := c.windowHandler(); !n.addAFile(prefix+"time_window_us", h, cur) {
			if v, ok := nodeReadInt(prefix + "time_window_us"); ok && v > 0 {
				c.windowUS = v
			}
		}
		c.settledW = c.targetW()
		z.constraints = append(z.constraints, c)
	}
	for j, cs := range zs.Children {
		childName := nodeZoneName(cs.NameFormat, socket)
		childRole, _ := nodeRoleOf(childName)
		if err := n.buildZone(ct, append(append([]int(nil), idx...), j), z, cs, childRole, socket, childName, out); err != nil {
			return err
		}
	}
	return nil
}

// buildCPUFreq finds the policies in the tree, each with the CPUs of its
// related_cpus, and seeds limits, governor and frequency from their files.
func (n *Node) buildCPUFreq() error {
	cf := n.profile.CPUFreq
	if cf == nil {
		return nil
	}
	cpufreqDir := n.sysPath(path.Dir(layout.PolicyDir(0)))
	entries, err := os.ReadDir(cpufreqDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hwemu: %w", err)
	}
	boost := cf.Boost.V
	h, cur := nodeBoolHandler(&boost)
	n.addAFile(filepath.Join(cpufreqDir, "boost"), h, cur) // no effect: cpuinfo_max_freq is fixed
	var indices []int
	for _, e := range entries {
		if k, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "policy")); err == nil && strings.HasPrefix(e.Name(), "policy") && e.IsDir() {
			indices = append(indices, k)
		}
	}
	sort.Ints(indices)
	for _, k := range indices {
		dir := n.sysPath(layout.PolicyDir(k))
		file := func(name string) string { return filepath.Join(dir, name) }
		p := &nodePolicy{index: k, minKHz: cf.CPUInfoMinKHz.V, maxKHz: cf.CPUInfoMaxKHz.V, table: append([]int64(nil), cf.AvailableKHz.V...), governor: cf.Governor.V}
		if s, ok := nodeReadString(file("related_cpus")); ok {
			for _, f := range strings.Fields(s) {
				if c, err := strconv.Atoi(f); err == nil {
					p.cpus = append(p.cpus, c)
				}
			}
		}
		if len(p.cpus) == 0 {
			p.cpus = []int{k}
		}
		if v, ok := nodeReadInt(file("cpuinfo_min_freq")); ok {
			p.minKHz = v
		}
		if v, ok := nodeReadInt(file("cpuinfo_max_freq")); ok {
			p.maxKHz = v
		}
		if s, ok := nodeReadString(file("scaling_available_frequencies")); ok {
			p.table = nil
			for _, f := range strings.Fields(s) {
				if v, err := strconv.ParseInt(f, 10, 64); err == nil {
					p.table = append(p.table, v)
				}
			}
		}
		driver := cf.Driver.V
		if s, ok := nodeReadString(file("scaling_driver")); ok {
			driver = s
		}
		p.setpolicy = driver == "intel_pstate" || driver == "amd-pstate-epp"
		p.governors = strings.Fields(cf.AvailableGovernors.V)
		if s, ok := nodeReadString(file("scaling_available_governors")); ok {
			p.governors = strings.Fields(s)
		}
		p.reqMinKHz, p.reqMaxKHz = p.minKHz, p.maxKHz
		p.socket = n.topo.socketOf(p.cpus[0])
		for _, c := range p.cpus {
			if c >= 0 && c < len(n.cpuPolicy) {
				n.cpuPolicy[c] = p
			}
		}
		h, cur = p.freqHandler(true)
		n.addAFile(file("scaling_max_freq"), h, cur)
		h, cur = p.freqHandler(false)
		n.addAFile(file("scaling_min_freq"), h, cur)
		h, cur = nodeEnumHandler(&p.governor, p.governors)
		n.addAFile(file("scaling_governor"), h, cur)
		h, cur = nodeSetspeedHandler()
		n.addAFile(file("scaling_setspeed"), h, cur)
		if s, ok := nodeReadString(file("energy_performance_available_preferences")); ok {
			h, cur = nodeEnumHandler(&p.epp, strings.Fields(s))
			n.addAFile(file("energy_performance_preference"), h, cur) // no effect on power (assumed)
		}
		if p.cur = n.sfile(file("scaling_cur_freq")); p.cur != nil {
			if v, ok := nodeParseInt(strings.TrimSpace(p.cur.last)); ok {
				p.curKHz, p.curSeeded = float64(v), true
			}
		}
		n.policies = append(n.policies, p)
	}
	// The lowest frequency the package reaches, from its policies' cpuinfo
	// range when the tree has them.
	for _, pk := range n.pkgs {
		lowest := math.Inf(1)
		for _, c := range pk.cpus {
			if p := n.cpuPolicy[c]; p != nil && p.maxKHz > 0 {
				lowest = math.Min(lowest, float64(p.minKHz)/float64(p.maxKHz))
			}
		}
		if !math.IsInf(lowest, 1) {
			pk.minScale = lowest
		}
	}
	return nil
}

// buildHSMP seeds the HSMP socket caps of the platform layout, one hwmon
// device per socket (hsmp/plat.c:198).
func (n *Node) buildHSMP() error {
	hs := n.profile.HSMP
	if hs == nil {
		return nil
	}
	if hs.Layout.V != "" && hs.Layout.V != "platform" {
		return fmt.Errorf("hwemu: profile %s: hsmp layout %q is not modelled", n.profile.Name, hs.Layout.V)
	}
	for _, pk := range n.pkgs {
		dir := n.sysPath(layout.HSMPHwmonDir(pk.socket))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		h := &nodeHSMP{capUW: hs.CapDefaultUW.V, capMaxUW: hs.CapMaxUW.V}
		if v, ok := nodeReadInt(filepath.Join(dir, "power1_cap_max")); ok {
			h.capMaxUW = v
		}
		hh, cur := h.capHandler()
		n.addAFile(filepath.Join(dir, "power1_cap"), hh, cur)
		h.settledW = h.targetW()
		h.input = n.sfile(filepath.Join(dir, "power1_input"))
		pk.hsmp = h
	}
	return nil
}

// buildGPUs seeds each GPU from its NVML state directory or its amdgpu hwmon
// device.
func (n *Node) buildGPUs() error {
	gs := n.profile.GPUs
	if gs == nil || gs.Count.V <= 0 {
		return nil
	}
	var amd []string
	if gs.Vendor == "amd" {
		var err error
		if amd, err = n.amdgpuHwmonDirs(); err != nil {
			return err
		}
	}
	for i := 0; i < gs.Count.V; i++ {
		g := &nodeGPU{index: i, minW: gs.Limits.MinW.V, maxW: gs.Limits.MaxW.V}
		switch gs.Vendor {
		case "nvidia":
			g.nvml = true
			g.limitRaw = int64(math.Round(gs.Limits.DefaultW.V * 1000))
			dir := filepath.Join(n.stateRoot, filepath.FromSlash(layout.NVMLGPUDir(i)))
			file := func(name string) string { return filepath.Join(dir, name) }
			if v, ok := nodeReadInt(file("power_min_limit_mw")); ok {
				g.minW = float64(v) / 1000
			}
			if v, ok := nodeReadInt(file("power_max_limit_mw")); ok {
				g.maxW = float64(v) / 1000
			}
			h, cur := g.nvmlLimitHandler()
			n.addAFile(file("power_limit_mw"), h, cur)
			g.settledW = g.targetW()
			if g.enforced = n.sfile(file("enforced_power_limit_mw")); g.enforced != nil {
				if v, ok := nodeParseInt(strings.TrimSpace(g.enforced.last)); ok {
					g.settledW = float64(v) / 1000
				}
			}
			if g.draw = n.sfile(file("power_draw_mw")); g.draw != nil {
				if v, ok := nodeParseInt(strings.TrimSpace(g.draw.last)); ok {
					g.avgW, g.avgSeed = float64(v)/1000, true
				}
			}
			if g.energy = n.sfile(file("total_energy_mj")); g.energy != nil {
				g.seedMJ, _ = nodeParseInt(strings.TrimSpace(g.energy.last))
			}
			g.utilFile = n.sfile(file("utilization_gpu_pct"))
		case "amd":
			g.limitRaw = int64(math.Round(gs.Limits.DefaultW.V * 1e6))
			if i < len(amd) {
				file := func(name string) string { return filepath.Join(amd[i], name) }
				if v, ok := nodeReadInt(file("power1_cap_min")); ok {
					g.minW = float64(v) / 1e6
				}
				if v, ok := nodeReadInt(file("power1_cap_max")); ok {
					g.maxW = float64(v) / 1e6
				}
				h, cur := g.amdgpuCapHandler()
				n.addAFile(file("power1_cap"), h, cur)
				g.input = n.sfile(file("power1_input"))
				if g.average = n.sfile(file("power1_average")); g.average != nil {
					if v, ok := nodeParseInt(strings.TrimSpace(g.average.last)); ok {
						g.avgW, g.avgSeed = float64(v)/1e6, true
					}
				}
			}
			g.settledW = g.targetW()
		default:
			g.limitRaw = int64(math.Round(gs.Limits.DefaultW.V * 1e6))
			g.settledW = g.targetW()
		}
		n.gpus = append(n.gpus, g)
	}
	return nil
}

// amdgpuHwmonDirs lists the hwmon directory of every AMD GPU in the order the
// tools number them: DRM cards sorted by number, keeping vendor 0x1002, the
// way rocm-smi enumerates devices. Paths are resolved, so events name the
// devices/ path rather than the class link.
func (n *Node) amdgpuHwmonDirs() ([]string, error) {
	classDir := n.sysPath(nodeDRMClassDir)
	entries, err := os.ReadDir(classDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hwemu: %w", err)
	}
	type card struct {
		num  int
		name string
	}
	var cards []card
	for _, e := range entries {
		if m := nodeCardName.FindStringSubmatch(e.Name()); m != nil {
			k, _ := strconv.Atoi(m[1])
			cards = append(cards, card{k, e.Name()})
		}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].num < cards[j].num })
	var dirs []string
	for _, c := range cards {
		dev := filepath.Join(classDir, c.name, "device")
		if v, _ := nodeReadString(filepath.Join(dev, "vendor")); v != "0x1002" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
		if len(matches) == 0 {
			continue
		}
		sort.Strings(matches)
		real, err := filepath.EvalSymlinks(matches[0])
		if err != nil {
			return nil, fmt.Errorf("hwemu: %w", err)
		}
		dirs = append(dirs, real)
	}
	return dirs, nil
}
