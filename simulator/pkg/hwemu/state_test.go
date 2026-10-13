package hwemu

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// stateComponent returns the component of s with hw.id id.
func stateComponent(tb testing.TB, s State, id string) ComponentState {
	tb.Helper()
	for _, c := range s.Components {
		if c.ID == id {
			return c
		}
	}
	tb.Fatalf("no component %s in %d components", id, len(s.Components))
	return ComponentState{}
}

// stateIDs lists the hw.id of every component, in order.
func stateIDs(s State) string {
	var ids []string
	for _, c := range s.Components {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ",")
}

// H30: components carry the identity of section 1.5 of the telemetry
// contract: one cpu<s> per package (package-<s>, the cpuinfo vendor and
// model), one memory<s> per socket (dram, parent cpu<s>), one gpu<i> per GPU
// (the product, the UUID and bus id NVML state holds). Measured follows the
// files a collector reads, so the VM host and its packages are unmeasured and
// the energy-only EPYC host, which has no DRAM zone, has no measured memory.
func TestH30ComponentIdentityAndMeasured(t *testing.T) {
	t.Parallel()
	state := func(t *testing.T, name string) (*Tree, State) {
		p := ensureBuiltin(t, name)
		tree := renderTestTree(t, p)
		n := nodeNewNode(t, p, tree)
		nodeStep(t, n, time.Second)
		return tree, n.State()
	}

	t.Run("intel-xeon-2s-rapl", func(t *testing.T) {
		t.Parallel()
		_, s := state(t, "intel-xeon-2s-rapl")
		if got := stateIDs(s); got != "cpu0,cpu1,memory0,memory1" {
			t.Fatalf("components %s, want cpu0,cpu1,memory0,memory1", got)
		}
		for i := 0; i < 2; i++ {
			cpu := stateComponent(t, s, fmt.Sprintf("cpu%d", i))
			want := ComponentState{ID: cpu.ID, Type: "cpu", Name: fmt.Sprintf("package-%d", i), Vendor: "Intel", Model: "Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz",
				SysfsPath: layout.PowercapZoneDir("intel-rapl", i), Index: i, Measured: true}
			if got := (ComponentState{ID: cpu.ID, Type: cpu.Type, Name: cpu.Name, Vendor: cpu.Vendor, Model: cpu.Model, SysfsPath: cpu.SysfsPath, Index: cpu.Index, Measured: cpu.Measured}); got != want {
				t.Errorf("cpu%d identity %+v, want %+v", i, got, want)
			}
			mem := stateComponent(t, s, fmt.Sprintf("memory%d", i))
			if mem.Type != "memory" || mem.Name != "dram" || mem.Parent != cpu.ID || mem.Index != i || !mem.Measured || mem.SysfsPath != layout.PowercapZoneDir("intel-rapl", i, 0) {
				t.Errorf("memory%d = %+v, want dram of %s, measured, at its DRAM zone", i, mem, cpu.ID)
			}
			if mem.Vendor != "" || mem.Model != "" || mem.UUID != "" {
				t.Errorf("memory%d carries cpu or gpu identity: %+v", i, mem)
			}
		}
		if !s.Host.Measured || s.DriverVersion != "" {
			t.Errorf("host %+v, driver %q; want a measured host and no NVML driver", s.Host, s.DriverVersion)
		}
	})

	t.Run("nvidia-h100-nvl-8gpu", func(t *testing.T) {
		t.Parallel()
		tree, s := state(t, "nvidia-h100-nvl-8gpu")
		if got, want := stateIDs(s), "cpu0,cpu1,memory0,memory1,gpu0,gpu1,gpu2,gpu3,gpu4,gpu5,gpu6,gpu7"; got != want {
			t.Fatalf("components %s, want %s", got, want)
		}
		for i := 0; i < 8; i++ {
			g := stateComponent(t, s, fmt.Sprintf("gpu%d", i))
			uuid := strings.TrimSpace(nodeRead(t, tree, nodeNVMLFile(i, "uuid")))
			bus := strings.TrimSpace(nodeRead(t, tree, nodeNVMLFile(i, "pci_bus_id")))
			if g.Type != "gpu" || g.Name != "NVIDIA H100 NVL" || g.Vendor != "NVIDIA" || g.Model != "NVIDIA H100 NVL" || g.Index != i || !g.Measured {
				t.Errorf("gpu%d = %+v, want a measured NVIDIA H100 NVL", i, g)
			}
			if g.UUID != uuid || g.PCIBusID != bus || !strings.HasPrefix(uuid, "GPU-") {
				t.Errorf("gpu%d uuid %q bus %q, want the NVML files %q %q", i, g.UUID, g.PCIBusID, uuid, bus)
			}
			if g.Parent != "" {
				t.Errorf("gpu%d has parent %q", i, g.Parent)
			}
		}
		if want := strings.TrimSpace(nodeRead(t, tree, path.Join(layout.StateDir, layout.NVMLDir, "driver_version"))); s.DriverVersion != want {
			t.Errorf("DriverVersion %q, want the NVML file %q", s.DriverVersion, want)
		}
	})

	t.Run("vm-no-powercap", func(t *testing.T) {
		t.Parallel()
		_, s := state(t, "vm-no-powercap")
		if s.Host.Measured {
			t.Errorf("the VM host is measured: %+v", s.Host)
		}
		if len(s.Components) != 16 {
			t.Errorf("%d components, want a cpu and a memory per vCPU package (16)", len(s.Components))
		}
		for _, c := range s.Components {
			if c.Measured || c.SysfsPath != "" {
				t.Errorf("%s on a VM without powercap: measured %v at %q, want unmeasured with no zone", c.ID, c.Measured, c.SysfsPath)
			}
		}
	})

	t.Run("amd-epyc-2s-energy-only", func(t *testing.T) {
		t.Parallel()
		_, s := state(t, "amd-epyc-2s-energy-only")
		for i := 0; i < 2; i++ {
			cpu, mem := stateComponent(t, s, fmt.Sprintf("cpu%d", i)), stateComponent(t, s, fmt.Sprintf("memory%d", i))
			if !cpu.Measured || cpu.Vendor != "AMD" || cpu.Model != "AMD EPYC 9654 96-Core Processor" {
				t.Errorf("cpu%d = %+v, want a measured AMD package", i, cpu)
			}
			if mem.Measured || mem.SysfsPath != "" {
				t.Errorf("memory%d measured %v at %q, want unmeasured: the host has no DRAM zone", i, mem.Measured, mem.SysfsPath)
			}
		}
	})

	t.Run("amd-instinct-mi300x-8gpu", func(t *testing.T) {
		t.Parallel()
		_, s := state(t, "amd-instinct-mi300x-8gpu")
		g := stateComponent(t, s, "gpu1")
		if g.Vendor != "AMD" || !g.Measured || g.PCIBusID != "0000:11:00.0" || g.SysfsPath != layout.PCIDevDir(0x11) || g.DeviceEnergyJ != 0 {
			t.Errorf("gpu1 = %+v, want a measured AMD GPU at its PCI device, with no device counter", g)
		}
	})
}

// H31: the host is every package, DRAM zone and GPU plus the platform's
// otherW: its power is their sum and its energy their energies plus otherW
// over the time run, across irregular steps and load changes.
func TestH31HostIsTheSumOfItsComponents(t *testing.T) {
	t.Parallel()
	p := nodeNVLProfile()
	p.Physics.Platform.OtherW = nodeF(95.0, "assumed:test fixture")
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	elapsed := time.Duration(0)
	for i, dt := range []time.Duration{730 * time.Millisecond, 2 * time.Second, 50 * time.Millisecond, 4100 * time.Millisecond} {
		n.SetLoad(Load{CPUUtil: []float64{0.2 * float64(i+1), 0.9}, GPUUtil: []float64{1, 0.1 * float64(i)}, MemoryIntensity: 0.4})
		nodeStep(t, n, dt)
		elapsed += dt
		s := n.State()
		var sumW, sumJ float64
		for _, c := range s.Components {
			sumW += c.PowerW
			sumJ += c.EnergyJ
		}
		if s.Host.OtherW != 95 || math.Abs(s.Host.PowerW-(sumW+95)) > 1e-9*s.Host.PowerW {
			t.Errorf("step %d: host power %.6f W with otherW %.1f, want components %.6f W + 95", i, s.Host.PowerW, s.Host.OtherW, sumW)
		}
		want := sumJ + 95*elapsed.Seconds()
		if math.Abs(s.Host.EnergyJ-want) > 1e-9*want {
			t.Errorf("step %d: host energy %.9f J, want components plus otherW*t = %.9f J", i, s.Host.EnergyJ, want)
		}
		if !s.Host.Measured {
			t.Errorf("step %d: a bare-metal host is unmeasured", i)
		}
	}
}

// H32: a limit the agent writes shows as requested at once and as the limit
// in effect settling toward it: PL1 120 W on intel-rapl:1 for cpu1 only, and
// an NVML limit of 250000 mW for gpu3 only.
func TestH32LimitsRequestedAndSettling(t *testing.T) {
	t.Parallel()
	p := nodeNVLProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	nodeStep(t, n, time.Second)
	s := n.State()
	if c := stateComponent(t, s, "cpu1"); c.RequestedLimitW != 165 || c.LimitW != 165 {
		t.Fatalf("cpu1 before the write: requested %v, limit %v; want 165 and 165", c.RequestedLimitW, c.LimitW)
	}
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 1), "120000000")
	nodeWrite(t, tr, nodeNVMLFile(3, "power_limit_mw"), "250000")
	cpuLast, gpuLast := 165.0, 400.0
	for step := 0; step < 8; step++ {
		nodeStep(t, n, 500*time.Millisecond)
		s = n.State()
		cpu1, gpu3 := stateComponent(t, s, "cpu1"), stateComponent(t, s, "gpu3")
		if cpu1.RequestedLimitW != 120 || gpu3.RequestedLimitW != 250 {
			t.Fatalf("step %d: requested cpu1 %v, gpu3 %v; want 120 and 250", step, cpu1.RequestedLimitW, gpu3.RequestedLimitW)
		}
		// The GPU settles faster than PL1 and reaches its request within
		// float precision after a few steps; in the first three it is still
		// strictly between the request and the step before.
		gpuNotSettling := step < 3 && (gpu3.LimitW >= gpuLast || gpu3.LimitW <= 250)
		if cpu1.LimitW >= cpuLast || cpu1.LimitW < 120 || gpu3.LimitW > gpuLast || gpu3.LimitW < 250 || gpuNotSettling {
			t.Fatalf("step %d: limits cpu1 %v (was %v), gpu3 %v (was %v); want each settling down toward its request", step, cpu1.LimitW, cpuLast, gpu3.LimitW, gpuLast)
		}
		cpuLast, gpuLast = cpu1.LimitW, gpu3.LimitW
	}
	if cpuLast-120 > 3 || gpuLast-250 > 0.01 {
		t.Errorf("after 4 s: cpu1 at %v, gpu3 at %v; want them near 120 and 250", cpuLast, gpuLast)
	}
	if c := stateComponent(t, s, "cpu0"); c.RequestedLimitW != 165 || c.LimitW != 165 {
		t.Errorf("cpu0 changed: requested %v, limit %v", c.RequestedLimitW, c.LimitW)
	}
	if g := stateComponent(t, s, "gpu2"); g.RequestedLimitW != 400 || g.LimitW != 400 {
		t.Errorf("gpu2 changed: requested %v, limit %v", g.RequestedLimitW, g.LimitW)
	}
	if m := stateComponent(t, s, "memory1"); m.LimitW != 0 || m.RequestedLimitW != 0 {
		t.Errorf("memory1 has a limit: %+v", m)
	}
}

// H33: every event names the component whose directory holds its file: a
// package zone its cpu, a DRAM zone its memory, a policy the cpu of its
// first CPU's socket, an HSMP hwmon device its socket's cpu, an NVML state
// directory, an amdgpu hwmon device or a GPU's PCI device its gpu. A
// restored render-owned file names its component the same way. A file of
// the whole node, such as a control type's enabled or cpuinfo, and a PCI
// device that is no GPU name none.
func TestH33EventsNameTheirComponent(t *testing.T) {
	t.Parallel()
	// stateWrite is one write of a case, and the component and outcome its
	// event must have: restored for a render-owned file, which the test makes
	// writable first, and any handled kind but a parse error otherwise.
	type stateWrite struct {
		value, component string
		restored         bool
	}
	check := func(t *testing.T, p *Profile, writes map[string]stateWrite) {
		t.Helper()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		for rel, w := range writes {
			if w.restored {
				if err := os.Chmod(filepath.Join(tr.Root, filepath.FromSlash(rel)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			nodeWrite(t, tr, rel, w.value)
		}
		nodeStep(t, n, time.Second)
		for rel, w := range writes {
			e := nodeOneEvent(t, n, rel)
			if e.Component != w.component || (e.Kind == EventRestored) != w.restored || e.Kind == EventParseError {
				t.Errorf("%s: event %+v, want component %q, restored %v", rel, e, w.component, w.restored)
			}
		}
	}

	t.Run("intel host with NVIDIA GPUs", func(t *testing.T) {
		t.Parallel()
		// Two sockets of two cores and two threads: cpu k sits on socket
		// (k mod 4) / 2. The DRAM zones reject writes, so their files are
		// render-owned.
		check(t, nodeNVLProfile(), map[string]stateWrite{
			nodeZoneFile("constraint_0_power_limit_uw", 1):                               {value: "100000000", component: "cpu1"},
			nodeZoneFile("constraint_0_time_window_us", 0):                               {value: "2000000", component: "cpu0"},
			nodePolicyFile(2, "scaling_max_freq"):                                        {value: "2000000", component: "cpu1"},
			nodePolicyFile(5, "scaling_max_freq"):                                        {value: "2000000", component: "cpu0"},
			nodeNVMLFile(3, "power_limit_mw"):                                            {value: "300000", component: "gpu3"},
			path.Join(layout.SysDir, layout.PowercapControlDir("intel-rapl"), "enabled"): {value: "1"},
			nodeZoneFile("constraint_0_time_window_us", 0, 0):                            {value: "5000", component: "memory0", restored: true},
			nodePolicyFile(2, "scaling_driver"):                                          {value: "acpi-cpufreq", component: "cpu1", restored: true},
			nodeNVMLFile(3, "power_default_limit_mw"):                                    {value: "350000", component: "gpu3", restored: true},
			layout.CPUInfoFile:                                                           {value: "processor\t: 0\n", restored: true},
		})
	})

	t.Run("DRAM zone that takes writes", func(t *testing.T) {
		t.Parallel()
		p := nodeIntelProfile()
		p.Powercap.ControlTypes[0].Zones[0].Children[0].Constraints[0].RejectWrites = Fact[string]{}
		check(t, p, map[string]stateWrite{
			nodeZoneFile("constraint_0_power_limit_uw", 0, 0): {value: "20000000", component: "memory0"},
			nodeZoneFile("enabled", 1, 0):                     {value: "1", component: "memory1"},
		})
	})

	t.Run("HSMP", func(t *testing.T) {
		t.Parallel()
		hwmon := func(s int, name string) string { return path.Join(layout.SysDir, layout.HSMPHwmonDir(s), name) }
		check(t, nodeHSMPProfile(), map[string]stateWrite{
			hwmon(1, "power1_cap"):     {value: "300000000", component: "cpu1"},
			hwmon(0, "power1_cap_max"): {value: "500000000", component: "cpu0", restored: true},
		})
	})

	t.Run("amdgpu", func(t *testing.T) {
		t.Parallel()
		check(t, nodeMI300XProfile(), map[string]stateWrite{
			nodeMI300XCap(1, "power1_cap"):                             {value: "500000000", component: "gpu1"},
			nodeMI300XCap(0, "power1_cap_max"):                         {value: "800000000", component: "gpu0", restored: true},
			path.Join(layout.SysDir, layout.PCIDevDir(0x11), "vendor"): {value: "0x10de", component: "gpu1", restored: true},
			path.Join(layout.SysDir, layout.PCIDevDir(0x02), "vendor"): {value: "0x10de", restored: true},
		})
	})
}

// H34: a package's SpeedHz is the mean scaling_cur_freq of its policies in
// Hz, SpeedMaxHz their cpuinfo_max_freq, and SpeedLimitHz the mean maximum in
// effect, which falls when the agent lowers scaling_max_freq of one of them.
func TestH34PackageSpeedsFollowCPUFreq(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{CPUUtil: []float64{0.3, 0.7}})
	topo := nodeTopology{sockets: 2, cores: 2, threads: 2}
	meanCur := func(socket int) float64 {
		sum, k := 0.0, 0
		for cpu := 0; cpu < topo.cpus(); cpu++ {
			if topo.socketOf(cpu) == socket {
				sum += float64(nodeReadInt64(t, tr, nodePolicyFile(cpu, "scaling_cur_freq")))
				k++
			}
		}
		return sum / float64(k) * 1000
	}
	for step := 0; step < 4; step++ {
		nodeStep(t, n, 300*time.Millisecond)
		s := n.State()
		for socket := 0; socket < 2; socket++ {
			c := stateComponent(t, s, fmt.Sprintf("cpu%d", socket))
			if want := meanCur(socket); math.Abs(c.SpeedHz-want) > 1e-6 {
				t.Errorf("step %d cpu%d: SpeedHz %v, want the mean scaling_cur_freq %v", step, socket, c.SpeedHz, want)
			}
			if c.SpeedMaxHz != 3.9e9 || c.SpeedLimitHz != 3.9e9 {
				t.Errorf("step %d cpu%d: max %v, limit %v; want 3.9 GHz for both", step, socket, c.SpeedMaxHz, c.SpeedLimitHz)
			}
		}
	}
	// cpu 2 is on socket 1.
	nodeWrite(t, tr, nodePolicyFile(2, "scaling_max_freq"), "1500000")
	nodeStep(t, n, 300*time.Millisecond)
	s := n.State()
	if c := stateComponent(t, s, "cpu1"); c.SpeedLimitHz != (3*3.9e9+1.5e9)/4 || c.SpeedMaxHz != 3.9e9 {
		t.Errorf("cpu1 after policy2 scaling_max_freq 1.5 GHz: limit %v, max %v; want %v and 3.9 GHz", c.SpeedLimitHz, c.SpeedMaxHz, (3*3.9e9+1.5e9)/4)
	}
	if c := stateComponent(t, s, "cpu0"); c.SpeedLimitHz != 3.9e9 {
		t.Errorf("cpu0 limit %v after a write on socket 1, want 3.9 GHz", c.SpeedLimitHz)
	}
	if c := stateComponent(t, s, "cpu1"); math.Abs(c.SpeedHz-meanCur(1)) > 1e-6 {
		t.Errorf("cpu1 SpeedHz %v, want the mean scaling_cur_freq %v", c.SpeedHz, meanCur(1))
	}
}

// H36: with a RenderCheckInterval, a render-owned file someone overwrote is
// restored at the first step that reaches the interval after the last check,
// and not before, while agent-writable files are still read every step.
func TestH36RenderCheckInterval(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n, err := NewNode(p, tr, Options{Start: nodeStart, RenderCheckInterval: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	window := nodeZoneFile("constraint_0_time_window_us", 0, 0)
	pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
	nodeWrite(t, tr, window, "5000")
	for second := 1; second <= 9; second++ {
		nodeWrite(t, tr, pl1, fmt.Sprintf("%d", 100000000+second*1000000))
		nodeStep(t, n, time.Second)
		if got := nodeRead(t, tr, window); got != "5000" {
			t.Fatalf("t+%ds: render-owned file reads %q, want the overwrite kept until t+10s", second, got)
		}
		if e := nodeOneEvent(t, n, pl1); e.Kind != EventAccepted {
			t.Fatalf("t+%ds: PL1 write gave %+v, want it read back at every step", second, e)
		}
	}
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, window); got != "976\n" {
		t.Fatalf("t+10s: render-owned file reads %q, want 976 restored", got)
	}
	if e := nodeOneEvent(t, n, window); e.Kind != EventRestored || e.Written != "5000" {
		t.Errorf("t+10s: event %+v, want restored 5000", e)
	}
	// The next check is 10 s after this one.
	nodeWrite(t, tr, window, "7000")
	nodeStep(t, n, 9500*time.Millisecond)
	if got := nodeRead(t, tr, window); got != "7000" {
		t.Errorf("t+19.5s: render-owned file reads %q, want the overwrite kept until t+20s", got)
	}
	nodeStep(t, n, 500*time.Millisecond)
	if got := nodeRead(t, tr, window); got != "976\n" {
		t.Errorf("t+20s: render-owned file reads %q, want 976 restored", got)
	}
}

// H37: a GPU's DeviceEnergyJ is its own counter, total_energy_mj / 1000, and
// it continues across a new Node over the same tree, as the file does.
func TestH37DeviceEnergyIsTheNVMLCounter(t *testing.T) {
	t.Parallel()
	p := nodeNVLProfile()
	tr := nodeRender(t, p, nil)
	check := func(n *Node, when string) []float64 {
		t.Helper()
		var out []float64
		for _, c := range n.State().Components {
			if c.Type != "gpu" {
				continue
			}
			file := nodeReadInt64(t, tr, nodeNVMLFile(c.Index, "total_energy_mj"))
			if math.Round(c.DeviceEnergyJ*1000) != float64(file) {
				t.Errorf("%s: %s DeviceEnergyJ %v, want total_energy_mj %d / 1000", when, c.ID, c.DeviceEnergyJ, file)
			}
			out = append(out, c.DeviceEnergyJ)
		}
		return out
	}
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1, 0.5, 0.2}})
	for i := 0; i < 5; i++ {
		nodeStep(t, n, 1100*time.Millisecond)
	}
	last := check(n, "first node")
	if last[0] < 100 {
		t.Fatalf("gpu0 counted %v J in 5.5 s at full load, want hundreds", last[0])
	}
	m, err := NewNode(p, tr, Options{Start: nodeStart.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLoad(Load{GPUUtil: []float64{1, 0.5, 0.2}})
	if got := check(m, "new node before a step"); got[0] != last[0] {
		t.Errorf("a new node starts gpu0 at %v J, want the %v J on disk", got[0], last[0])
	}
	nodeStep(t, m, 2*time.Second)
	for i, e := range check(m, "new node") {
		if e <= last[i] {
			t.Errorf("gpu%d DeviceEnergyJ %v after the new node ran, want above %v", i, e, last[i])
		}
	}
}
