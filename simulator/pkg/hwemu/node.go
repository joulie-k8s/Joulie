package hwemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
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

var nodeCardName = regexp.MustCompile(`^card([0-9]+)$`)

// Node runs the physics of one rendered tree. Each Step reads back what the
// agent and the GPU tools wrote, advances power, energy, frequency and limits
// in substeps on a fixed grid, and writes the readings that changed. Its
// methods are safe for concurrent use; file I/O happens under the Node's own
// lock only.
type Node struct {
	mu sync.Mutex

	// unobserved is true during StepUnobserved; pendingS holds the S files
	// it computed and did not write, with their latest content.
	unobserved bool
	pendingS   map[*nodeSFile]string

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

	driverVersion string        // NVML driver_version, read once
	renderEvery   time.Duration // Options.RenderCheckInterval
	renderNext    time.Time     // when R files are next checked, with renderEvery

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

	// changedBuf holds the changed A files of a readback, and readBuf the
	// content of the A file it reads; afilesDirty is set when a handler ran
	// since the A files were last compared with the model (readback.go);
	// pending the events of
	// refused CheckWrite calls, which the next step
	// reports (reject.go); seeding is true while NewNode seeds the model
	// from the tree, where a file shows state that already holds.
	changedBuf  []nodeAWrite
	readBuf     []byte
	afilesDirty bool
	pending     []Event
	seeding     bool

	// receiverGate is Options.ReceiverGate; rejectErrno the errno of each
	// file whose write this step rejected (reject.go); cpuGov the CPU
	// governor state beyond the policies (node_governor.go).
	receiverGate string
	rejectErrno  map[string]string
	cpuGov       nodeCPUGovernor

	// live is Options.Live; liveA, liveS and liveR are its agent-writable,
	// emulator-owned and render-owned files by path relative to the root,
	// and liveComp the component of each directory (live.go).
	live     bool
	liveA    map[string]*nodeAFile
	liveS    map[string]*nodeSFile
	liveR    map[string]*nodeRFile
	liveComp map[string]string
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
// limits, frequencies, persistence modes and NVML state are seeded from the
// files on disk rather than from the profile, so a Node created over a live
// tree, such as one Ensure resumed, continues where the last one stopped and
// an energy counter never reads as a reset. The node name, when t has none,
// and the GPU identities and driver version are read from the tree once.
// There is no randomness: the same tree, options, loads and steps give the
// same files. A zero o.Start means time.Now(). A Frozen node reads back and
// restores files but advances no reading, which keeps a captured tree as
// captured.
func NewNode(p *Profile, t *Tree, o Options) (*Node, error) {
	if p == nil || t == nil || t.Root == "" {
		return nil, errors.New("hwemu: NewNode needs a profile and a rendered tree")
	}
	tree := *t
	n := &Node{
		profile:     p,
		tree:        &tree,
		root:        t.Root,
		realRoot:    t.Root,
		sysRoot:     t.SysRoot,
		stateRoot:   t.StateRoot,
		owners:      map[string]layout.FileSpec{},
		substepD:    o.Substep,
		frozen:      o.Frozen,
		renderEvery: o.RenderCheckInterval,
		live:        o.Live,
	}
	if o.Live {
		if len(t.Files) == 0 {
			return nil, errors.New("hwemu: Options.Live needs the layout of the tree (Tree.Files), as Render and Ensure return it")
		}
		n.liveS = map[string]*nodeSFile{}
	}
	switch o.ReceiverGate {
	case "":
		n.receiverGate = ReceiverGateProfile
	case ReceiverGateProfile, ReceiverGateOff, ReceiverGateOn:
		n.receiverGate = o.ReceiverGate
	default:
		return nil, fmt.Errorf("hwemu: Options.ReceiverGate %q is not %s, %s or %s", o.ReceiverGate, ReceiverGateProfile, ReceiverGateOff, ReceiverGateOn)
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
	n.renderNext = start.Add(n.renderEvery)
	for _, f := range t.Files {
		n.owners[f.Rel] = f
	}
	if tree.NodeName == "" {
		var meta renderMeta
		if b, err := n.readFile(n.rootPath(ensureMetaFile)); err == nil && json.Unmarshal(b, &meta) == nil {
			tree.NodeName = meta.Node
		}
	}
	if v, ok := n.readString(filepath.Join(n.stateRoot, layout.NVMLDir, "driver_version")); ok {
		n.driverVersion = v
	}

	steps := []func() error{
		n.buildTopology, n.buildModels, n.buildPowercap, n.buildCPUFreq, n.buildHSMP, n.buildGPUs,
		n.buildCPUGovernor, n.buildGPUDPM, n.buildNVClock,
		n.snapshotRenderFiles,
	}
	n.seeding = true
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	n.seeding = false
	n.afilesDirty = true
	clear(n.rejectErrno)
	n.cpuUtil = make([]float64, n.topo.cpus())
	n.derive()
	n.assign()
	n.seedWindows()
	if n.live {
		n.liveIndex()
	}
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
		PeerTransfers:   append([]PeerTransfer(nil), l.PeerTransfers...),
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

// StepUnobserved advances the node by dt as Step does, for a caller that
// neither writes nor reads the tree until it calls Step or Flush: it reads
// back no file, since nobody wrote one, and holds the files it would write,
// which the next Step or Flush writes with their latest content. State
// after it is State after Step. The governor sweep steps a bursty load
// every 10 ms this way, where the file I/O of Step is nearly all of the
// cost and no file is read.
func (n *Node) StepUnobserved(dt time.Duration) error {
	if dt < 0 {
		return fmt.Errorf("hwemu: step by negative %v", dt)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unobserved = true
	defer func() { n.unobserved = false }()
	return n.step(n.now.Add(dt))
}

// Flush writes the files StepUnobserved held, without stepping.
func (n *Node) Flush() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.flushPending()
}

// flushPending writes every held S file with its latest content.
func (n *Node) flushPending() error {
	var errs []error
	for f, c := range n.pendingS {
		delete(n.pendingS, f)
		errs = append(errs, n.writeS(f, c))
	}
	return errors.Join(errs...)
}

// AdvanceTo steps the node to t. A t before the node's time is an error,
// except on a Live node, where a reader of the view may have stepped the
// node past t: the step then ends at the node's time.
func (n *Node) AdvanceTo(t time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t.Before(n.now) {
		if !n.live {
			return fmt.Errorf("hwemu: advance to %v, before the node's time %v", t, n.now)
		}
		t = n.now
	}
	return n.step(t)
}

func (n *Node) step(end time.Time) error {
	n.events, n.pending = n.pending, nil
	err := errors.Join(n.readback(n.renderCheckDue(end)), n.persistReset())
	n.rejectAttach()
	if n.frozen {
		n.now, n.grid, n.segT = end, end, end
		return err
	}
	for next := n.grid.Add(n.substepD); !next.After(end); next = n.grid.Add(n.substepD) {
		n.substep(next)
	}
	n.now = end
	err = errors.Join(err, n.writeOutputs())
	if !n.unobserved {
		err = errors.Join(err, n.flushPending())
	}
	return err
}

// renderCheckDue reports whether the step to end checks the R files: every
// step without a RenderCheckInterval, otherwise the first step that reaches
// the interval after the last check, NewNode counting as one.
func (n *Node) renderCheckDue(end time.Time) bool {
	if n.renderEvery <= 0 {
		return true
	}
	if end.Before(n.renderNext) {
		return false
	}
	n.renderNext = end.Add(n.renderEvery)
	return true
}

// State returns a snapshot after the last step, with that step's events.
// PackageState.EnergyUJ is the package zone's energy_uj without its wrap: the
// value on disk when the Node started plus the energy since. Components and
// Host integrate energy since the Node started, up to the node's time.
func (n *Node) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	dt := n.now.Sub(n.segT).Seconds()
	s := State{Time: n.now, Events: append([]Event(nil), n.events...), DriverVersion: n.driverVersion}
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
	s.Components, s.Host = n.stateComponents(dt)
	return s
}

// stateComponents is State's Components and Host: every CPU package, then
// every DRAM zone, then every GPU, as section 1.5 of the telemetry contract
// names them, with energy integrated up to the node's time, dt seconds into
// the current segment. Measured follows the files a collector reads: a
// package or DRAM zone with energy_uj, an NVIDIA GPU with total_energy_mj,
// an AMD GPU with power1_input or power1_average, and a host that is not a
// hypervisor guest.
func (n *Node) stateComponents(dt float64) ([]ComponentState, HostState) {
	host := HostState{Measured: !n.profile.CPU.Hypervisor, OtherW: n.other.W, PowerW: n.other.W, EnergyJ: n.other.J + n.other.W*dt}
	var cpus, mems, gpus []ComponentState
	vendor, model := stateCPUVendor(n.profile.CPU.VendorID.V), n.profile.CPU.ModelName.V
	for _, pk := range n.pkgs {
		cpu := ComponentState{
			ID: stateID(semconv.HwTypeCPU, pk.socket), Type: semconv.HwTypeCPU, Name: fmt.Sprintf("package-%d", pk.socket),
			Vendor: vendor, Model: model, Index: pk.socket,
			PowerW: pk.pwr.W, EnergyJ: pk.pwr.J + pk.pwr.W*dt, LimitW: pk.capW, RequestedLimitW: pk.requestedW, Utilization: pk.util,
			GovernorNative: pk.govNative, GovernorPreset: pk.govPreset,
		}
		mem := ComponentState{
			ID: stateID(semconv.HwTypeMemory, pk.socket), Type: semconv.HwTypeMemory, Name: "dram", Parent: cpu.ID, Index: pk.socket,
			PowerW: pk.dram.W, EnergyJ: pk.dram.J + pk.dram.W*dt,
		}
		var diePath string
		for _, z := range n.zones {
			if z.socket != pk.socket {
				continue
			}
			switch z.role {
			case nodeRolePackage:
				if cpu.SysfsPath == "" {
					cpu.SysfsPath = z.rel
				}
				cpu.Measured = cpu.Measured || z.energy != nil
			case nodeRoleDie:
				if diePath == "" {
					diePath = z.rel
				}
				cpu.Measured = cpu.Measured || z.energy != nil
			case nodeRoleDRAM:
				if mem.SysfsPath == "" {
					mem.SysfsPath = z.rel
				}
				mem.Measured = mem.Measured || z.energy != nil
			}
		}
		if cpu.SysfsPath == "" {
			cpu.SysfsPath = diePath
		}
		// The policies of the package, as scaling_cur_freq and the resolved
		// scaling_max_freq show them, and the hardware maximum, the
		// cpuinfo_max_freq of turbo on, in kHz.
		var curKHz, limKHz float64
		var maxKHz int64
		k := 0
		for _, pol := range n.policies {
			if pol.socket == pk.socket {
				curKHz += math.Round(pol.shownKHz)
				limKHz += float64(pol.effMaxKHz())
				maxKHz = max(maxKHz, pol.maxKHz)
				k++
			}
		}
		if k > 0 {
			cpu.SpeedHz = curKHz / float64(k) * 1000
			cpu.SpeedLimitHz = limKHz / float64(k) * 1000
			cpu.SpeedMaxHz = float64(maxKHz) * 1000
		}
		cpus = append(cpus, cpu)
		mems = append(mems, mem)
		host.PowerW += cpu.PowerW + mem.PowerW
		host.EnergyJ += cpu.EnergyJ + mem.EnergyJ
	}
	if gs := n.profile.GPUs; gs != nil {
		for _, g := range n.gpus {
			c := ComponentState{
				ID: g.id(), Type: semconv.HwTypeGPU, Name: gs.Product.V, Vendor: stateGPUVendor(gs.Vendor), Model: gs.Product.V,
				UUID: g.uuid, PCIBusID: g.pciBusID, SysfsPath: g.sysfsPath, Index: g.index,
				PowerW: g.pwr.W, EnergyJ: g.pwr.J + g.pwr.W*dt, LimitW: g.settledW, RequestedLimitW: g.limitW(), Utilization: n.gpuUtil(g.index),
				ClockHz: g.clock.stateClockHz(), ClockMaxHz: g.clock.stateClockMaxHz(), ClockLimitHz: g.clock.stateClockLimitHz(),
				GovernorNative: g.clock.native, GovernorPreset: g.clock.preset,
				PeerRxBytes: stateBytes(g.clock.peerRxBytes), PeerTxBytes: stateBytes(g.clock.peerTxBytes),
			}
			if g.nvml {
				c.Measured = g.energy != nil
				c.DeviceEnergyJ = float64(nodeMilliJoules(g.seedMJ, c.EnergyJ)) / 1000
			} else {
				c.Measured = g.input != nil || g.average != nil || g.energyUJ != nil
			}
			gpus = append(gpus, c)
			host.PowerW += c.PowerW
			host.EnergyJ += c.EnergyJ
		}
	}
	return append(append(cpus, mems...), gpus...), host
}

// stateID is a component's hw.id: its type and index, such as cpu1 or gpu3.
func stateID(typ string, i int) string { return typ + strconv.Itoa(i) }

// stateZoneComponent is the hw.id the files of zone z belong to: a DRAM zone
// is its socket's memory, psys and any other top-level zone the whole node
// (""), and a package or die zone, or a core or other child such as uncore,
// its socket's CPU package.
func stateZoneComponent(z *nodeZone) string {
	switch {
	case z.role == nodeRoleDRAM:
		return stateID(semconv.HwTypeMemory, z.socket)
	case z.role == nodeRolePsys, z.role == nodeRoleOther && z.parent == nil:
		return ""
	}
	return stateID(semconv.HwTypeCPU, z.socket)
}

// stateCPUVendor is hw.vendor of a CPU from its cpuinfo vendor_id: Intel or
// AMD, or the vendor_id itself for any other vendor.
func stateCPUVendor(vendorID string) string {
	switch vendorID {
	case "GenuineIntel":
		return "Intel"
	case "AuthenticAMD":
		return "AMD"
	}
	return vendorID
}

// stateGPUVendor is hw.vendor of a GPU from the profile's gpus.vendor.
func stateGPUVendor(vendor string) string {
	switch vendor {
	case "nvidia":
		return "NVIDIA"
	case "amd":
		return "AMD"
	case "intel":
		return "Intel"
	}
	return vendor
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
		n.cpuModel = phys.MeasuredCurveCPUModel{Points: points, Knee: ph.CPU.Knee.V, BetaFreq: ph.CPU.BetaFreq.V}
	default:
		return fmt.Errorf("hwemu: profile %s: unknown cpu model %q", n.profile.Name, ph.CPU.Model)
	}
	// MaxCapWatts stays 0: without a cap the board's natural maximum bounds
	// CappedBoardGPUModel.Power, and its idle power is the only floor.
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
		n.addAFile(filepath.Join(n.sysPath(layout.PowercapControlDir(ct.Name)), "enabled"), "", h, cur)

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
	if s, die, ok := layout.RAPLPackageZone(name); ok {
		if die < 0 {
			return nodeRolePackage, s
		}
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
	z := &nodeZone{rel: rel, role: role, socket: socket, parent: parent, enabled: zs.Enabled.V, powerUnitUW: zs.PowerUnitUW.V}
	z.component = stateZoneComponent(z)
	*out = append(*out, z)

	enabled := filepath.Join(dir, "enabled")
	if h, cur := nodeBoolHandler(&z.enabled); n.addAFile(enabled, z.component, h, cur) == nil {
		if v, ok := n.readInt(enabled); ok {
			z.enabled = int(v)
		}
	}
	if sf := n.sfile(filepath.Join(dir, "energy_uj")); sf != nil {
		unit := zs.EnergyUnitNJ.V
		if unit <= 0 {
			if r, ok := n.readInt(filepath.Join(dir, "max_energy_range_uj")); ok && r > 0 {
				unit = nodeUnitFromRange(r)
			} else if zs.MaxEnergyRangeUJ != nil && zs.MaxEnergyRangeUJ.V > 0 {
				unit = nodeUnitFromRange(zs.MaxEnergyRangeUJ.V)
			}
		}
		if unit <= 0 {
			return fmt.Errorf("hwemu: profile %s: zone %s has energy_uj but no energy unit", n.profile.Name, rel)
		}
		uj, _ := n.readInt(sf.path)
		z.counter, z.energy, z.seedUJ = nodeNewCounter(unit, uj), sf, uj
	}
	for i, cs := range zs.Constraints {
		c := &nodeConstraint{kind: nodeConstraintKind(cs.Name), limitUW: cs.PowerLimitUW.V, maxUW: cs.MaxPowerUW.V, windowUS: cs.TimeWindowUS.V, rejectWrites: cs.RejectWrites.V}
		c.mask = nodeLimitMask(role, c.kind, zs.LimitBits)
		prefix := filepath.Join(dir, fmt.Sprintf("constraint_%d_", i))
		if v, ok := n.readInt(prefix + "max_power_uw"); ok {
			c.maxUW = v
		}
		if h, cur := n.limitHandler(c, prefix+"power_limit_uw", z.powerUnitUW); n.addAFile(prefix+"power_limit_uw", z.component, h, cur) == nil {
			if v, ok := n.readInt(prefix + "power_limit_uw"); ok {
				c.limitUW = v
			}
		}
		if h, cur := n.windowHandler(c, prefix+"time_window_us"); n.addAFile(prefix+"time_window_us", z.component, h, cur) == nil {
			if v, ok := n.readInt(prefix + "time_window_us"); ok && v > 0 {
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
	n.addAFile(filepath.Join(cpufreqDir, "boost"), "", h, cur) // read by nodePolicy.ceilingKHz
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
		p := &nodePolicy{index: k, minKHz: cf.CPUInfoMinKHz.V, maxKHz: cf.CPUInfoMaxKHz.V, table: append([]int64(nil), cf.AvailableKHz.V...), governor: cf.Governor.V, boost: &boost}
		if s, ok := n.readString(file("related_cpus")); ok {
			for _, f := range strings.Fields(s) {
				if c, err := strconv.Atoi(f); err == nil {
					p.cpus = append(p.cpus, c)
				}
			}
		}
		if len(p.cpus) == 0 {
			p.cpus = []int{k}
		}
		if v, ok := n.readInt(file("cpuinfo_min_freq")); ok {
			p.minKHz = v
		}
		// An emulator-owned cpuinfo_max_freq shows the maximum with turbo
		// in its present state (writeCPUGovernor); the hardware maximum
		// the physics scales by stays the profile's.
		if v, ok := n.readInt(file("cpuinfo_max_freq")); ok && n.owners[n.relPath(file("cpuinfo_max_freq"))].Owner != layout.OwnerEmulator {
			p.maxKHz = v
		}
		if s, ok := n.readString(file("scaling_available_frequencies")); ok {
			p.table = nil
			for _, f := range strings.Fields(s) {
				if v, err := strconv.ParseInt(f, 10, 64); err == nil {
					p.table = append(p.table, v)
				}
			}
		}
		driver := cf.Driver.V
		if s, ok := n.readString(file("scaling_driver")); ok {
			driver = s
		}
		p.setpolicy = driver == "intel_pstate" || driver == "amd-pstate-epp"
		p.governors = strings.Fields(cf.AvailableGovernors.V)
		if s, ok := n.readString(file("scaling_available_governors")); ok {
			p.governors = strings.Fields(s)
		}
		p.reqMinKHz, p.reqMaxKHz = p.minKHz, p.maxKHz
		p.socket = n.topo.socketOf(p.cpus[0])
		for _, c := range p.cpus {
			if c >= 0 && c < len(n.cpuPolicy) {
				n.cpuPolicy[c] = p
			}
		}
		cpu := stateID(semconv.HwTypeCPU, p.socket)
		h, cur = p.freqHandler(true)
		n.addAFile(file("scaling_max_freq"), cpu, h, cur)
		h, cur = p.freqHandler(false)
		n.addAFile(file("scaling_min_freq"), cpu, h, cur)
		h, cur = n.nodeEnumHandler(file("scaling_governor"), &p.governor, p.governors)
		n.addAFile(file("scaling_governor"), cpu, h, cur)
		h, cur = nodeSetspeedHandler()
		n.addAFile(file("scaling_setspeed"), cpu, h, cur)
		if s, ok := n.readString(file("energy_performance_available_preferences")); ok {
			h, cur = n.nodeEnumHandler(file("energy_performance_preference"), &p.epp, strings.Fields(s))
			n.addAFile(file("energy_performance_preference"), cpu, h, cur) // no effect on power (assumed)
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
		if v, ok := n.readInt(filepath.Join(dir, "power1_cap_max")); ok {
			h.capMaxUW = v
		}
		hh, cur := h.capHandler()
		n.addAFile(filepath.Join(dir, "power1_cap"), stateID(semconv.HwTypeCPU, pk.socket), hh, cur)
		h.settledW = h.targetW()
		h.input = n.sfile(filepath.Join(dir, "power1_input"))
		pk.hsmp = h
	}
	return nil
}

// buildGPUs seeds each GPU from its NVML state directory, its amdgpu hwmon
// device or its Intel GPU hwmon device.
func (n *Node) buildGPUs() error {
	gs := n.profile.GPUs
	if gs == nil || gs.Count.V <= 0 {
		return nil
	}
	var amd, intel []string
	var err error
	switch gs.Vendor {
	case "amd":
		amd, err = n.amdgpuHwmonDirs()
	case "intel":
		intel, err = n.intelGPUHwmonDirs()
	}
	if err != nil {
		return err
	}
	for i := 0; i < gs.Count.V; i++ {
		g := &nodeGPU{index: i, minW: gs.Limits.MinW.V, maxW: gs.Limits.MaxW.V}
		switch gs.Vendor {
		case "nvidia":
			g.nvml = true
			g.limitRaw = int64(math.Round(gs.Limits.DefaultW.V * 1000))
			g.defaultMW = g.limitRaw
			g.persistence = persistModeText(gs.PersistenceMode.V)
			dir := filepath.Join(n.stateRoot, filepath.FromSlash(layout.NVMLGPUDir(i)))
			file := func(name string) string { return filepath.Join(dir, name) }
			if v, ok := n.readInt(file("power_min_limit_mw")); ok {
				g.minW = float64(v) / 1000
			}
			if v, ok := n.readInt(file("power_max_limit_mw")); ok {
				g.maxW = float64(v) / 1000
			}
			if v, ok := n.readInt(file("power_default_limit_mw")); ok {
				g.defaultMW = v
			}
			g.uuid, _ = n.readString(file("uuid"))
			g.pciBusID, _ = n.readString(file("pci_bus_id"))
			// A field gpus.fieldSupport marks unsupported holds its token for
			// good, as the driver never reports a value there: its file is
			// neither read back nor written by the physics.
			token := func(name string) bool {
				s, ok := n.readString(file(name))
				return ok && strings.HasPrefix(strings.TrimSpace(s), "[")
			}
			h, cur := g.nvmlLimitHandler()
			if !token("power_limit_mw") {
				g.limitFile = n.addAFile(file("power_limit_mw"), g.id(), h, cur)
			}
			h, cur = n.nodeEnumHandler(file(layout.NVMLPersistenceModeFile), &g.persistence, []string{persistTextEnabled, persistTextDisabled})
			n.addAFile(file(layout.NVMLPersistenceModeFile), g.id(), h, cur)
			g.settledW = g.targetW()
			if !token("enforced_power_limit_mw") {
				if g.enforced = n.sfile(file("enforced_power_limit_mw")); g.enforced != nil {
					if v, ok := nodeParseInt(strings.TrimSpace(g.enforced.last)); ok {
						g.settledW = float64(v) / 1000
					}
				}
			}
			if !token("power_draw_mw") {
				if g.draw = n.sfile(file("power_draw_mw")); g.draw != nil {
					if v, ok := nodeParseInt(strings.TrimSpace(g.draw.last)); ok {
						g.avgW, g.avgSeed = float64(v)/1000, true
					}
				}
			}
			if g.energy = n.sfile(file("total_energy_mj")); g.energy != nil {
				g.seedMJ, _ = nodeParseInt(strings.TrimSpace(g.energy.last))
			}
			g.utilFile = n.sfile(file("utilization_gpu_pct"))
		case "amd":
			g.limitRaw = int64(math.Round(gs.Limits.DefaultW.V * 1e6))
			if g.secondary = slices.Contains(gs.SecondaryDies, i); g.secondary {
				g.limitRaw = 0
			}
			if i < len(amd) {
				file := func(name string) string { return filepath.Join(amd[i], name) }
				if v, ok := n.readInt(file("power1_cap_min")); ok {
					g.minW = float64(v) / 1e6
				}
				if v, ok := n.readInt(file("power1_cap_max")); ok {
					g.maxW = float64(v) / 1e6
				}
				// hwmon/hwmon<N> sits in the PCI device's directory.
				dev := filepath.Dir(filepath.Dir(amd[i]))
				g.pciBusID = filepath.Base(dev)
				g.sysfsPath = strings.TrimPrefix(n.relPath(dev), layout.SysDir+"/")
				h, cur := n.amdgpuCapHandler(g, file("power1_cap"))
				if g.secondary {
					h, cur = n.amdgpuSecondaryCapHandler(file("power1_cap"))
				}
				n.addAFile(file("power1_cap"), g.id(), h, cur)
				g.input = n.sfile(file("power1_input"))
				if g.average = n.sfile(file("power1_average")); g.average != nil {
					if v, ok := nodeParseInt(strings.TrimSpace(g.average.last)); ok {
						g.avgW, g.avgSeed = float64(v)/1e6, true
					}
				}
			}
			g.settledW = g.targetW()
		case "intel":
			dir := ""
			if i < len(intel) {
				dir = intel[i]
			}
			n.buildIntelGPU(g, dir)
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
	return n.gpuHwmonDirs("0x1002", nil)
}

// gpuHwmonDirs lists, in DRM card order, the hwmon directory of every card
// whose PCI device has vendor: its first hwmon device, or with names its
// first one whose name is in names. Paths are resolved.
func (n *Node) gpuHwmonDirs(vendor string, names map[string]bool) ([]string, error) {
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
		if v, _ := n.readString(filepath.Join(dev, "vendor")); v != vendor {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
		sort.Strings(matches)
		var hwmon string
		for _, m := range matches {
			if name, _ := n.readString(filepath.Join(m, "name")); names == nil || names[name] {
				hwmon = m
				break
			}
		}
		if hwmon == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(hwmon)
		if err != nil {
			return nil, fmt.Errorf("hwemu: %w", err)
		}
		dirs = append(dirs, real)
	}
	return dirs, nil
}
