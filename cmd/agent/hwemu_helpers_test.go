package main

// Helpers for the emulated-node tests in hwemu_test.go. simulator/pkg/hwemu
// renders a node of a hardware profile (sysfs tree, cpuinfo, the state the
// fake GPU tools read) and runs its physics; these helpers point the agent's
// host paths and command runner at that node, so the agent's real discovery
// and control code runs over it as it would on the hardware. Kernel source
// citations (file.c:N) are against Linux v7.3-rc5.

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// emulatedNode is an emulated node the agent has been pointed at.
type emulatedNode struct {
	// name is the node name the tree was rendered for, unique per test,
	// because the agent skips a status write whose payload it already wrote
	// for that object (lastWrites).
	name   string
	tree   *hwemu.Tree
	runner *recordingRunner
}

// useEmulatedNode points every host path and tool the agent reads at tree,
// for the duration of the test, the way pointAgentAtFixture points it at a
// corpus capture, and sets env. It must run before newNodeController, because
// dvfs.New lists the cpufreq entries once, when the controller is built. The
// seams are process globals, so a test that uses it never calls t.Parallel.
func useEmulatedNode(t *testing.T, tree *hwemu.Tree, env map[string]string) *emulatedNode {
	t.Helper()
	// The catalog loads once per process; empty means the embedded one, as
	// in a deployed agent. Any non-empty telemetry variable would hand the
	// agent an HTTP backend instead of the host (resolveTelemetryConfigFromEnv).
	for _, key := range []string{"HARDWARE_CATALOG_PATH", "TELEMETRY_CPU_SOURCE", "TELEMETRY_CPU_CONTROL", "TELEMETRY_GPU_CONTROL"} {
		t.Setenv(key, "")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Setenv(k, env[k])
	}

	oldHostSys := dvfs.HostSysRoot
	dvfs.HostSysRoot = tree.SysRoot
	t.Cleanup(func() { dvfs.HostSysRoot = oldHostSys })

	oldRoot := dvfs.PowercapRoot
	dvfs.PowercapRoot = filepath.Join(tree.SysRoot, "class", "powercap")
	t.Cleanup(func() { dvfs.PowercapRoot = oldRoot })

	oldCPUInfo := procCPUInfoPath
	procCPUInfoPath = tree.CPUInfoPath
	t.Cleanup(func() { procCPUInfoPath = oldCPUInfo })

	// A node without cpufreq has no policy0; point at a path that does not
	// exist, so the driver family comes out empty rather than describing the
	// host running the test.
	oldDriver := cpufreqDriverPath
	cpufreqDriverPath = filepath.Join(tree.SysRoot, filepath.FromSlash(layout.PolicyDir(0)), "scaling_driver")
	if _, err := os.Stat(cpufreqDriverPath); err != nil {
		cpufreqDriverPath = filepath.Join(t.TempDir(), "absent-scaling-driver")
	}
	t.Cleanup(func() { cpufreqDriverPath = oldDriver })

	// Render writes dev/nvidiactl on NVIDIA nodes only, as a real NVIDIA
	// node has it, and detectGPUVendor stats it; elsewhere the probe must not
	// see the host's.
	oldNvidiactl := nvidiaControlDevicePath
	nvidiaControlDevicePath = filepath.Join(tree.Root, filepath.FromSlash(layout.DevNvidiactl))
	if _, err := os.Stat(nvidiaControlDevicePath); err != nil {
		nvidiaControlDevicePath = filepath.Join(t.TempDir(), "absent-nvidiactl")
	}
	t.Cleanup(func() { nvidiaControlDevicePath = oldNvidiactl })

	runner := &recordingRunner{runner: smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}}
	oldRunner := commandRunner
	commandRunner = runner
	t.Cleanup(func() { commandRunner = oldRunner })

	return &emulatedNode{name: renderedNodeName(t, tree), tree: tree, runner: runner}
}

// renderedNodeName is the node name Render recorded in state/hwemu.json.
func renderedNodeName(t *testing.T, tree *hwemu.Tree) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tree.StateRoot, "hwemu.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(b, &meta); err != nil || meta.Node == "" {
		t.Fatalf("state/hwemu.json names no node: %v (%s)", err, b)
	}
	return meta.Node
}

// nodeObject is the Node a real node of profile p registers as.
func (e *emulatedNode) nodeObject(p *hwemu.Profile) *corev1.Node {
	return p.NodeObject(e.name)
}

// controller is the agent's own controller for the node, built from the
// environment as the daemonset builds it.
func (e *emulatedNode) controller(t *testing.T) *NodeController {
	t.Helper()
	nc, err := newNodeController(e.name, false)
	if err != nil {
		t.Fatal(err)
	}
	return nc
}

// reconcile runs one reconcile of nc against a fake API server that holds
// node and twin, and returns the client it wrote through and the error
// reconcileOnce returned.
func reconcileEmulated(t *testing.T, nc *NodeController, node *corev1.Node, twin *v1alpha1.NodeTwin) (*dynamicfake.FakeDynamicClient, error) {
	t.Helper()
	reader, dyn := newTestClients(t, node, twin)
	return dyn, reconcileOnce(context.Background(), reader, dyn, nc)
}

// controlStatus is status.controlStatus.<component> of the node's NodeTwin,
// nil when the agent wrote none.
func controlStatus(t *testing.T, dyn *dynamicfake.FakeDynamicClient, nodeName, component string) map[string]any {
	t.Helper()
	obj, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), sanitizeNodeObjectName(nodeName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin: %v", err)
	}
	m, found, err := unstructured.NestedMap(obj.Object, "status", "controlStatus", component)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	return m
}

// nodeTwinWithGPUCap is a NodeTwin whose only intent is a per-GPU cap, in W
// or in percent of the device maximum.
func nodeTwinWithGPUCap(nodeName string, capWatts, capPct *float64) *v1alpha1.NodeTwin {
	return &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: sanitizeNodeObjectName(nodeName)},
		Spec: v1alpha1.NodeTwinSpec{
			NodeName: nodeName,
			Profile:  "eco",
			GPU: &v1alpha1.NodeTwinGPU{PowerCap: &v1alpha1.GPUPowerCap{
				Scope:          "perGpu",
				CapWattsPerGPU: capWatts,
				CapPctOfMax:    capPct,
			}},
		},
	}
}

func float64Ptr(v float64) *float64 { return &v }

// recordingRunner runs every command on the emulated node's fake tools and
// keeps each call and its error, so a test can tell which probe decided what.
type recordingRunner struct {
	runner smi.Runner
	mu     sync.Mutex
	calls  []runnerCall
}

type runnerCall struct {
	command string
	err     error
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.runner.Run(ctx, name, args...)
	r.mu.Lock()
	r.calls = append(r.calls, runnerCall{command: strings.Join(append([]string{name}, args...), " "), err: err})
	r.mu.Unlock()
	return out, err
}

// ran reports whether command ran, and whether one run of it succeeded.
func (r *recordingRunner) ran(command string) (ran, succeeded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.command == command {
			ran = true
			succeeded = succeeded || c.err == nil
		}
	}
	return ran, succeeded
}

// commands lists the commands that ran, in order.
func (r *recordingRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.command)
	}
	return out
}

func (r *recordingRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// readTrimmed reads a sysfs or state file without its trailing newline.
func readTrimmed(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func readFileInt(t *testing.T, file string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(readTrimmed(t, file), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return v
}

// zoneDir is a powercap zone of the tree by its class name, such as
// intel-rapl:0 or intel-rapl:0:0, through the class link the agent globs.
func zoneDir(e *emulatedNode, zone string) string {
	return filepath.Join(e.tree.SysRoot, "class", "powercap", zone)
}

// policyMaxFreqs is scaling_max_freq of every cpufreq policy, by policy
// directory, and cpuinfo_max_freq of each.
func policyMaxFreqs(t *testing.T, e *emulatedNode) (scalingMax, cpuinfoMax map[string]int64) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(e.tree.SysRoot, filepath.FromSlash(path.Dir(layout.PolicyDir(0))), "policy*"))
	if err != nil {
		t.Fatal(err)
	}
	scalingMax, cpuinfoMax = map[string]int64{}, map[string]int64{}
	for _, d := range dirs {
		scalingMax[filepath.Base(d)] = readFileInt(t, filepath.Join(d, "scaling_max_freq"))
		cpuinfoMax[filepath.Base(d)] = readFileInt(t, filepath.Join(d, "cpuinfo_max_freq"))
	}
	return scalingMax, cpuinfoMax
}

// setScalingMaxToCPUInfoMin writes every cpufreq policy's cpuinfo_min_freq
// into its scaling_max_freq, and returns the minimum of each, by policy
// directory.
func setScalingMaxToCPUInfoMin(t *testing.T, e *emulatedNode) map[string]int64 {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(e.tree.SysRoot, filepath.FromSlash(path.Dir(layout.PolicyDir(0))), "policy*"))
	if err != nil {
		t.Fatal(err)
	}
	cpuinfoMin := map[string]int64{}
	for _, d := range dirs {
		v := readFileInt(t, filepath.Join(d, "cpuinfo_min_freq"))
		if err := os.WriteFile(filepath.Join(d, "scaling_max_freq"), []byte(strconv.FormatInt(v, 10)), 0); err != nil {
			t.Fatal(err)
		}
		cpuinfoMin[filepath.Base(d)] = v
	}
	return cpuinfoMin
}

// nvmlFile is one NVML state file of GPU i.
func nvmlFile(e *emulatedNode, i int, name string) string {
	return filepath.Join(e.tree.StateRoot, filepath.FromSlash(layout.NVMLGPUDir(i)), name)
}

// amdgpuHwmonDirs lists the hwmon directory of every AMD GPU in the order the
// tools number them: DRM cards in numeric order, vendor 0x1002 only.
func amdgpuHwmonDirs(t *testing.T, e *emulatedNode) []string {
	t.Helper()
	dir := filepath.Join(e.tree.SysRoot, "class", "drm")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cards []int
	for _, en := range entries {
		n, ok := strings.CutPrefix(en.Name(), "card")
		if c, err := strconv.Atoi(n); ok && err == nil && "card"+strconv.Itoa(c) == en.Name() {
			cards = append(cards, c)
		}
	}
	sort.Ints(cards)
	var out []string
	for _, c := range cards {
		dev := filepath.Join(dir, "card"+strconv.Itoa(c), "device")
		if v, err := os.ReadFile(filepath.Join(dev, "vendor")); err != nil || strings.TrimSpace(string(v)) != "0x1002" {
			continue
		}
		hw, err := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
		if err != nil || len(hw) != 1 {
			t.Fatalf("card%d has hwmon %v, want one", c, hw)
		}
		out = append(out, hw[0])
	}
	return out
}

// Profile variants. Each sets only what its case needs, and every fact it
// sets carries an assumed: source naming the test variant, as Validate
// requires.

const variantSrc = "assumed:test variant built in cmd/agent/hwemu_helpers_test.go"

func intFact(v int) hwemu.Fact[int]             { return hwemu.Fact[int]{V: v, Src: variantSrc} }
func int64Fact(v int64) hwemu.Fact[int64]       { return hwemu.Fact[int64]{V: v, Src: variantSrc} }
func stringFact(v string) hwemu.Fact[string]    { return hwemu.Fact[string]{V: v, Src: variantSrc} }
func float64Fact(v float64) hwemu.Fact[float64] { return hwemu.Fact[float64]{V: v, Src: variantSrc} }

// withPsys adds the platform zone psys to intel-rapl, after the packages,
// where the kernel puts it (intel_rapl_common.c:510-515).
func withPsys(p *hwemu.Profile) {
	rapl := &p.Powercap.ControlTypes[0]
	rapl.Zones = append(rapl.Zones, hwemu.ZoneSpec{
		Repeat:       "once",
		NameFormat:   "psys",
		Enabled:      intFact(1),
		EnergyUnitNJ: int64Fact(61035),
		PowerUnitUW:  int64Fact(125000),
		Constraints: []hwemu.ConstraintSpec{{
			Name: "long_term", PowerLimitUW: int64Fact(500000000), MaxPowerUW: int64Fact(500000000), TimeWindowUS: int64Fact(27983872),
		}},
	})
	p.Physics.Platform.OtherW = float64Fact(60)
}

// withMMIO adds the intel-rapl-mmio control type, whose one zone the
// processor thermal driver registers as package 0 (processor_thermal_rapl.c:151,
// 164), named package-0 like an MSR package zone.
func withMMIO(p *hwemu.Profile) {
	p.Powercap.ControlTypes = append(p.Powercap.ControlTypes, hwemu.ControlTypeSpec{
		Name: "intel-rapl-mmio",
		Zones: []hwemu.ZoneSpec{{
			Repeat:       "once",
			NameFormat:   "package-0",
			Enabled:      intFact(1),
			EnergyUnitNJ: int64Fact(61035),
			PowerUnitUW:  int64Fact(125000),
			Constraints: []hwemu.ConstraintSpec{{
				Name: "long_term", PowerLimitUW: int64Fact(165000000), MaxPowerUW: int64Fact(165000000), TimeWindowUS: int64Fact(999424),
			}},
		}},
	})
}

// withDies gives every package dies zones named package-X-die-Y in place of
// one package zone (intel_rapl_common.c:1788-1792).
func withDies(dies int) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		p.CPU.DiesPerPackage = intFact(dies)
		z := &p.Powercap.ControlTypes[0].Zones[0]
		z.Repeat = "per-die"
		z.NameFormat = "package-%d-die-%d"
	}
}

// withPerCorePolicies shares one cpufreq policy between the SMT siblings of
// a core.
func withPerCorePolicies(p *hwemu.Profile) {
	p.CPUFreq.PolicyGrouping = stringFact("per-core")
}

// withLockedPL1 makes every package's long_term limit reject writes, as a
// BIOS lock does (intel_rapl_common.c:1023-1045, rapl_detect_powerlimit).
func withLockedPL1(p *hwemu.Profile) {
	z := &p.Powercap.ControlTypes[0].Zones[0]
	for i := range z.Constraints {
		if z.Constraints[i].Name == "long_term" {
			z.Constraints[i].RejectWrites = stringFact("locked")
		}
	}
}

// withoutNFD is a node where Node Feature Discovery does not run.
func withoutNFD(p *hwemu.Profile) {
	p.Node.NFD = "absent"
}

// withExposure exposes the GPUs as mode, with the MIG or time-slicing
// parameters it needs.
func withExposure(mode string) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		p.GPUs.Exposure = stringFact(mode)
		switch mode {
		case "mig-single", "mig-mixed":
			p.GPUs.MIGProfile = "1g.12gb"
			p.GPUs.MIGDevicesPerGPU = 7
		case "timeslice-rename", "timeslice-norename":
			p.GPUs.TimesliceReplicas = 4
		}
	}
}

// withUnsupportedLimits makes nvidia-smi report [N/A] for the minimum and
// maximum power limits, as on a board that does not expose them.
func withUnsupportedLimits(p *hwemu.Profile) {
	p.GPUs.FieldSupport = map[string]string{"power.min_limit": "[N/A]", "power.max_limit": "[N/A]"}
}

// withLegacyRocmSMI installs the rocm-smi variant that accepts the agent's
// --showpowercap, so the AMD path gets past inventory.
func withLegacyRocmSMI(p *hwemu.Profile) {
	for i := range p.GPUs.Tools {
		if p.GPUs.Tools[i].Name == "rocm-smi" {
			p.GPUs.Tools[i].Variant = "legacy-agent-contract"
		}
	}
}

// withSecondaryDie marks AMD device d as the secondary die of a package.
func withSecondaryDie(d int) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		p.GPUs.SecondaryDies = append(p.GPUs.SecondaryDies, d)
	}
}

// withDefaultLimit sets the GPUs' default power limit, where they start.
func withDefaultLimit(w float64) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		p.GPUs.Limits.DefaultW = float64Fact(w)
	}
}

// variant applies mutations in order.
func variant(mutations ...func(*hwemu.Profile)) func(*hwemu.Profile) {
	return func(p *hwemu.Profile) {
		for _, m := range mutations {
			m(p)
		}
	}
}
