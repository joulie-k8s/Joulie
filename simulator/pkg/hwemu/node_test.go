package hwemu

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The physics tests build trees by hand with the layout package, in the shape
// Render gives them, so they run without the renderer. Fixtures are the
// builtin profiles cut to two cores per socket.

const (
	nodeR = layout.OwnerRender
	nodeS = layout.OwnerEmulator
	nodeA = layout.OwnerAgent
)

var nodeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func nodeF[T any](v T, src string) Fact[T] { return Fact[T]{V: v, Src: src} }

const nodeSmallTopology = "assumed:test fixture, two cores per socket keep the tree small"

// nodeIntelProfile is intel-xeon-2s-rapl: intel_pstate per-cpu policies, RAPL
// PL1 and PL2 per package, and a disabled DRAM sub-zone that rejects writes.
func nodeIntelProfile() *Profile {
	p := &Profile{
		SchemaVersion: 1,
		Name:          "test-intel-2s",
		Sources: map[string]Source{
			"corpus":     {Kind: "corpus", Ref: "cmd/agent/testdata/hardware/xeon-4socket-nfd-labels"},
			"ark8260":    {Kind: "published", Ref: "https://www.intel.com/content/www/us/en/products/sku/192474/intel-xeon-platinum-8260-processor-35-75m-cache-2-40-ghz/specifications.html", Note: "2.40 GHz base, 3.90 GHz turbo, TDP 165 W"},
			"raplcommon": {Kind: "published", Ref: "linux v7.3-rc5 drivers/powercap/intel_rapl_common.c:33,50,215,479-486,558-563,741,744"},
			"raplmsr":    {Kind: "published", Ref: "linux v7.3-rc5 drivers/powercap/intel_rapl_msr.c:349-354,432", Note: "dram_domain_energy_unit = 15300 for INTEL_SKYLAKE_X"},
			"pstate":     {Kind: "published", Ref: "linux v7.3-rc5 Documentation/admin-guide/pm/intel_pstate.rst:39-41,90-91"},
		},
	}
	p.CPU = CPUSpec{
		VendorID:       nodeF("GenuineIntel", "corpus:corpus#cpuinfo"),
		Sockets:        nodeF(2, "assumed:two socket cut of the four socket corpus machine"),
		CoresPerSocket: nodeF(2, nodeSmallTopology),
		ThreadsPerCore: nodeF(2, "corpus:corpus#cpuinfo"),
		Numbering:      "socket-major-smt-last",
	}
	p.Powercap = &PowercapSpec{ControlTypes: []ControlTypeSpec{{
		Name: "intel-rapl",
		Zones: []ZoneSpec{{
			Repeat:       "per-package",
			NameFormat:   "package-%d",
			Enabled:      nodeF(1, "corpus:corpus"),
			EnergyUnitNJ: nodeF[int64](61035, "derived:raplcommon#1e9>>ESU14, reproduces corpus 262143328850"),
			PowerUnitUW:  nodeF[int64](125000, "assumed:PU=3; every corpus limit is a multiple of 125000 uW"),
			Constraints: []ConstraintSpec{
				{Name: "long_term", PowerLimitUW: nodeF[int64](165000000, "corpus:corpus"), MaxPowerUW: nodeF[int64](165000000, "corpus:corpus"), TimeWindowUS: nodeF[int64](999424, "corpus:corpus")},
				{Name: "short_term", PowerLimitUW: nodeF[int64](198000000, "corpus:corpus"), MaxPowerUW: nodeF[int64](413000000, "corpus:corpus"), TimeWindowUS: nodeF[int64](976, "corpus:corpus")},
			},
			Children: []ZoneSpec{{
				NameFormat:   "dram",
				Enabled:      nodeF(0, "corpus:corpus"),
				EnergyUnitNJ: nodeF[int64](15300, "published:raplmsr"),
				Constraints: []ConstraintSpec{
					{Name: "long_term", PowerLimitUW: nodeF[int64](0, "corpus:corpus"), MaxPowerUW: nodeF[int64](47250000, "corpus:corpus"), TimeWindowUS: nodeF[int64](976, "corpus:corpus"), RejectWrites: nodeF("disabled", "corpus:corpus#machine.yaml writeRejectingZones")},
				},
			}},
		}},
	}}}
	p.CPUFreq = &CPUFreqSpec{
		Driver:             nodeF("intel_pstate", "corpus:corpus#cpufreq-driver"),
		Governor:           nodeF("powersave", "corpus:corpus#node-labels.json"),
		AvailableGovernors: nodeF("performance powersave", "published:pstate"),
		PolicyGrouping:     nodeF("per-cpu", "published:pstate#39-41"),
		CPUInfoMinKHz:      nodeF[int64](1000000, "assumed:matches corpus idle 'cpu MHz : 1000.000'"),
		CPUInfoMaxKHz:      nodeF[int64](3900000, "published:ark8260"),
	}
	p.Physics.CPU.Model = "analytic"
	p.Physics.CPU.MaxPkgW = nodeF(165.0, "published:ark8260")
	p.Physics.CPU.IdlePkgW = nodeF(40.0, "assumed:no public package idle figure")
	p.Physics.CPU.AlphaUtil = nodeF(1.15, "assumed:simulator default SIM_ALPHA_UTIL")
	p.Physics.CPU.BetaFreq = nodeF(1.35, "assumed:simulator default SIM_BETA_FREQ")
	p.Physics.CPU.Knee = nodeF(0.7, "assumed:phys default knee of MeasuredCurveCPUModel")
	p.Physics.CPU.FreqRampMS = nodeF(500, "assumed:simulator default SIM_DVFS_RAMP_MS")
	p.Physics.DRAM.WattsPerSocket = nodeF(8.0, "assumed:placeholder, no public DRAM figure")
	p.Physics.RAPL.SettleTau = "timeWindow"
	p.Physics.RAPL.FloorW = nodeF(0.0, "assumed:MIN_POWER is not exposed in sysfs (raplcommon#479-486)")
	return p
}

// nodeEPYCProfile is amd-epyc-2s-energy-only: acpi-cpufreq with a table and
// the performance governor, RAPL energy counters without constraints.
func nodeEPYCProfile() *Profile {
	p := &Profile{
		SchemaVersion: 1,
		Name:          "test-epyc-2s",
		Sources: map[string]Source{
			"amd9654":    {Kind: "published", Ref: "https://www.amd.com/en/products/processors/server/epyc/4th-generation-9004-and-8004-series/amd-epyc-9654.html", Note: "96 cores, base 2.4 GHz, default TDP 360W"},
			"raplcommon": {Kind: "published", Ref: "linux v7.3-rc5 drivers/powercap/intel_rapl_common.c:33,50,215,279-296,689-698,741,1023-1049"},
			"acpicf":     {Kind: "published", Ref: "linux v7.3-rc5 drivers/cpufreq/acpi-cpufreq.c:990"},
		},
	}
	p.CPU = CPUSpec{
		VendorID:       nodeF("AuthenticAMD", "assumed:x86 vendor string for AMD"),
		Sockets:        nodeF(2, "assumed:generic 2S node"),
		CoresPerSocket: nodeF(2, nodeSmallTopology),
		ThreadsPerCore: nodeF(2, "derived:amd9654#192 threads / 96 cores"),
		Numbering:      "socket-major-smt-last",
	}
	p.Powercap = &PowercapSpec{ControlTypes: []ControlTypeSpec{{
		Name: "intel-rapl",
		Zones: []ZoneSpec{{
			Repeat:       "per-package",
			NameFormat:   "package-%d",
			Enabled:      nodeF(0, "derived:raplcommon#1032-1049"),
			EnergyUnitNJ: nodeF[int64](15258, "assumed:ESU 16 (AMD PPR not read); formula published:raplcommon"),
			Children:     []ZoneSpec{{NameFormat: "core", Enabled: nodeF(0, "assumed:no limit MSR"), EnergyUnitNJ: nodeF[int64](15258, "assumed:ESU 16")}},
		}},
	}}}
	p.CPUFreq = &CPUFreqSpec{
		Driver:             nodeF("acpi-cpufreq", "published:acpicf"),
		Governor:           nodeF("performance", "assumed:common server setting"),
		AvailableGovernors: nodeF("performance powersave ondemand schedutil", "assumed:governors of a typical distribution kernel"),
		PolicyGrouping:     nodeF("per-cpu", "assumed:policy sharing follows firmware _PSD, not read"),
		CPUInfoMinKHz:      nodeF[int64](1500000, "assumed:lowest P-state"),
		CPUInfoMaxKHz:      nodeF[int64](2400000, "derived:amd9654#base 2.4 GHz as P0"),
		AvailableKHz:       nodeF([]int64{2400000, 1900000, 1500000}, "assumed:3 P-states, P0 = published base amd9654"),
		Boost:              nodeF(1, "assumed:boost enabled in firmware"),
	}
	p.Physics.CPU.Model = "analytic"
	p.Physics.CPU.MaxPkgW = nodeF(360.0, "published:amd9654#Default TDP")
	p.Physics.CPU.IdlePkgW = nodeF(40.0, "assumed:no public package idle figure")
	p.Physics.CPU.AlphaUtil = nodeF(1.15, "assumed:simulator default SIM_ALPHA_UTIL")
	p.Physics.CPU.BetaFreq = nodeF(1.35, "assumed:simulator default SIM_BETA_FREQ")
	p.Physics.CPU.Knee = nodeF(0.7, "assumed:phys default knee of MeasuredCurveCPUModel")
	p.Physics.CPU.FreqRampMS = nodeF(500, "assumed:simulator default SIM_DVFS_RAMP_MS")
	p.Physics.CPU.CoreFraction = nodeF(0.7, "assumed:placeholder")
	p.Physics.DRAM.WattsPerSocket = nodeF(8.0, "assumed:placeholder")
	p.Physics.RAPL.SettleTau = "timeWindow"
	return p
}

// nodeHSMPProfile is amd-epyc-2s-hsmp: amd-pstate-epp and HSMP socket caps.
func nodeHSMPProfile() *Profile {
	p := nodeEPYCProfile()
	p.Name = "test-epyc-2s-hsmp"
	p.Sources["amd9655"] = Source{Kind: "published", Ref: "https://www.amd.com/en/products/processors/server/epyc/9005-series/amd-epyc-9655.html", Note: "boost up to 4.5 GHz, default TDP 400W, cTDP 320-400W"}
	p.Sources["pstate"] = Source{Kind: "published", Ref: "linux v7.3-rc5 drivers/cpufreq/amd-pstate.c:2185 (.name amd-pstate-epp)"}
	p.CPUFreq.Driver = nodeF("amd-pstate-epp", "published:pstate")
	p.CPUFreq.Governor = nodeF("powersave", "assumed:active-mode default")
	p.CPUFreq.AvailableGovernors = nodeF("performance powersave", "assumed:setpolicy driver in active mode offers these two")
	p.CPUFreq.AvailableKHz = nodeF([]int64{}, "derived:amd-pstate has no frequency table")
	p.CPUFreq.CPUInfoMaxKHz = nodeF[int64](4500000, "published:amd9655#boost up to 4.5 GHz")
	p.HSMP = &HSMPSpec{
		Layout:       nodeF("platform", "assumed:firmware exposes no ACPI HSMP device"),
		CapMaxUW:     nodeF[int64](400000000, "published:amd9655#cTDP 320-400W, top of range"),
		CapDefaultUW: nodeF[int64](400000000, "published:amd9655#Default TDP 400W"),
	}
	p.Physics.CPU.MaxPkgW = nodeF(400.0, "published:amd9655#Default TDP")
	p.Physics.CPU.IdlePkgW = nodeF(45.0, "assumed:no public package idle figure")
	p.Physics.HSMP.SettleTauMS = nodeF(1000, "assumed:no public settling data")
	return p
}

// nodeNVLProfile is nvidia-h100-nvl-8gpu on the Intel host, as the builtin
// profile has it.
func nodeNVLProfile() *Profile {
	p := nodeIntelProfile()
	p.Name = "test-nvidia-nvl"
	p.Sources["pb11773"] = Source{Kind: "published", Ref: "https://www.nvidia.com/content/dam/en-zz/Solutions/Data-Center/h100/PB-11773-001_v01.pdf", Note: "400 W maximum (default), 200 W minimum"}
	p.Sources["nvml"] = Source{Kind: "published", Ref: "https://docs.nvidia.com/deploy/nvml-api/api/group__nvmlDeviceQueries.html", Note: "power averaged over 1 s; limits in mW"}
	p.GPUs = &GPUSpec{Vendor: "nvidia", Count: nodeF(8, "assumed:generic 8-GPU node"), Product: nodeF("NVIDIA H100 NVL", "assumed:nvidia-smi name")}
	p.GPUs.Limits.MinW = nodeF(200.0, "published:pb11773#Table 1")
	p.GPUs.Limits.MaxW = nodeF(400.0, "published:pb11773#Table 1")
	p.GPUs.Limits.DefaultW = nodeF(400.0, "published:pb11773#Table 1")
	p.Physics.GPU.IdleW = nodeF(50.0, "assumed:placeholder")
	p.Physics.GPU.NaturalMaxW = nodeF(400.0, "published:pb11773")
	p.Physics.GPU.CapSettleTauMS = nodeF(150, "assumed:simulator default SIM_GPU_CAP_APPLY_TAU_MS")
	p.Physics.GPU.TelemetryWindowMS = nodeF(1000, "published:nvml")
	return p
}

// nodeMI300XProfile is amd-instinct-mi300x-8gpu with two GPUs, behind a BMC
// VGA card that is not an AMD GPU.
func nodeMI300XProfile() *Profile {
	p := nodeEPYCProfile()
	p.Name = "test-amd-mi300x"
	p.Sources["mi300xds"] = Source{Kind: "published", Ref: "https://www.amd.com/content/dam/amd/en/documents/instinct-tech-docs/data-sheets/amd-instinct-mi300x-data-sheet.pdf", Note: "Maximum TBP 750W"}
	p.GPUs = &GPUSpec{Vendor: "amd", Count: nodeF(2, "assumed:test fixture, two GPUs keep the tree small"), Product: nodeF("AMD Instinct MI300X", "assumed:product_name")}
	p.GPUs.Limits.MinW = nodeF(0.0, "assumed:by analogy with the amd-smi MI300A example")
	p.GPUs.Limits.MaxW = nodeF(750.0, "published:mi300xds")
	p.GPUs.Limits.DefaultW = nodeF(750.0, "assumed:equal to max")
	p.GPUs.PowerSensor = nodeF("input", "assumed:average vs input is per product")
	p.Physics.GPU.IdleW = nodeF(120.0, "assumed:placeholder")
	p.Physics.GPU.NaturalMaxW = nodeF(750.0, "published:mi300xds")
	p.Physics.GPU.CapSettleTauMS = nodeF(350, "assumed:no public settling data")
	p.Physics.GPU.TelemetryWindowMS = nodeF(1000, "assumed:no public averaging window")
	return p
}

// nodeTestTree assembles a tree and its file list, as Render returns them.
type nodeTestTree struct {
	tb    testing.TB
	root  string
	files []layout.FileSpec
	dirs  map[string]bool
}

func (b *nodeTestTree) abs(rel string) string { return filepath.Join(b.root, filepath.FromSlash(rel)) }

func (b *nodeTestTree) dir(rel string) {
	if rel == "." || rel == "" || b.dirs[rel] {
		return
	}
	b.dir(path.Dir(rel))
	if err := os.Mkdir(b.abs(rel), 0o755); err != nil && !os.IsExist(err) {
		b.tb.Fatal(err)
	}
	b.dirs[rel] = true
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindDir, Owner: nodeR, Mode: fs.ModeDir | 0o755})
}

func (b *nodeTestTree) file(rel string, owner layout.Owner, mode fs.FileMode, content string) {
	b.dir(path.Dir(rel))
	if err := os.WriteFile(b.abs(rel), []byte(content), 0o644); err != nil {
		b.tb.Fatal(err)
	}
	if err := os.Chmod(b.abs(rel), mode); err != nil {
		b.tb.Fatal(err)
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindFile, Owner: owner, Mode: mode})
}

func (b *nodeTestTree) sys(rel string, owner layout.Owner, mode fs.FileMode, content string) {
	b.file(path.Join(layout.SysDir, rel), owner, mode, content)
}

// reject puts a directory in place of an attribute that rejects writes (D).
func (b *nodeTestTree) reject(rel string) {
	rel = path.Join(layout.SysDir, rel)
	b.dir(path.Dir(rel))
	if err := os.Mkdir(b.abs(rel), 0o755); err != nil {
		b.tb.Fatal(err)
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindRejectingDir, Owner: nodeR, Mode: fs.ModeDir | 0o755})
}

func (b *nodeTestTree) link(rel, target string) {
	rel = path.Join(layout.SysDir, rel)
	b.dir(path.Dir(rel))
	if err := os.Symlink(target, b.abs(rel)); err != nil {
		b.tb.Fatal(err)
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindSymlink, Owner: nodeR, Mode: fs.ModeSymlink | 0o777, Target: target})
}

// nodeRender renders p by hand. energyUJ gives a start value per zone
// directory, relative to sys/.
func nodeRender(tb testing.TB, p *Profile, energyUJ map[string]int64) *Tree {
	tb.Helper()
	b := &nodeTestTree{tb: tb, root: tb.TempDir(), dirs: map[string]bool{}}
	b.dir(layout.SysDir)
	b.dir(layout.StateDir)
	topo := nodeTopology{sockets: max(1, p.CPU.Sockets.V), cores: max(1, p.CPU.CoresPerSocket.V), threads: max(1, p.CPU.ThreadsPerCore.V), dies: max(1, p.CPU.DiesPerPackage.V)}
	var ci strings.Builder
	for cpu := 0; cpu < topo.cpus(); cpu++ {
		fmt.Fprintf(&ci, "processor\t: %d\nvendor_id\t: %s\nphysical id\t: %d\n\n", cpu, p.CPU.VendorID.V, topo.socketOf(cpu))
	}
	b.file(layout.CPUInfoFile, nodeR, 0o444, ci.String())
	b.file(path.Join(layout.StateDir, layout.ToolsFile), nodeR, 0o444, `{"schemaVersion":1,"tools":{}}`+"\n")
	if p.Powercap != nil {
		for _, ct := range p.Powercap.ControlTypes {
			l, tg := layout.PowercapClassLink(ct.Name)
			b.link(l, tg)
			b.sys(path.Join(layout.PowercapControlDir(ct.Name), "enabled"), nodeA, 0o644, "1\n")
			idx := 0
			for _, zs := range ct.Zones {
				var names []string
				switch zs.Repeat {
				case "per-package":
					for x := 0; x < topo.sockets; x++ {
						names = append(names, nodeZoneName(zs.NameFormat, x))
					}
				case "per-die":
					for x := 0; x < topo.sockets; x++ {
						for y := 0; y < topo.dies; y++ {
							names = append(names, nodeZoneName(zs.NameFormat, x, y))
						}
					}
				default:
					names = append(names, nodeZoneName(zs.NameFormat))
				}
				for _, name := range names {
					nodeRenderZone(b, ct.Name, []int{idx}, zs, name, energyUJ)
					idx++
				}
			}
		}
	}
	if cf := p.CPUFreq; cf != nil {
		cpufreqDir := path.Dir(layout.PolicyDir(0))
		for k := 0; k < topo.cpus(); k++ {
			dir := layout.PolicyDir(k)
			put := func(name string, owner layout.Owner, mode fs.FileMode, content string) {
				b.sys(path.Join(dir, name), owner, mode, content)
			}
			put("scaling_driver", nodeR, 0o444, cf.Driver.V+"\n")
			put("scaling_available_governors", nodeR, 0o444, cf.AvailableGovernors.V+" \n")
			put("scaling_governor", nodeA, 0o644, cf.Governor.V+"\n")
			put("cpuinfo_min_freq", nodeR, 0o444, nodeItoa(cf.CPUInfoMinKHz.V)+"\n")
			put("cpuinfo_max_freq", nodeR, 0o444, nodeItoa(cf.CPUInfoMaxKHz.V)+"\n")
			put("cpuinfo_transition_latency", nodeR, 0o444, "0\n")
			put("scaling_min_freq", nodeA, 0o644, nodeItoa(cf.CPUInfoMinKHz.V)+"\n")
			put("scaling_max_freq", nodeA, 0o644, nodeItoa(cf.CPUInfoMaxKHz.V)+"\n")
			put("scaling_cur_freq", nodeS, 0o444, nodeItoa(cf.CPUInfoMaxKHz.V)+"\n")
			put("scaling_setspeed", nodeR, 0o644, "<unsupported>\n")
			put("affected_cpus", nodeR, 0o444, strconv.Itoa(k)+" \n")
			put("related_cpus", nodeR, 0o444, strconv.Itoa(k)+" \n")
			if len(cf.AvailableKHz.V) > 0 {
				var s strings.Builder
				for _, f := range cf.AvailableKHz.V {
					s.WriteString(nodeItoa(f) + " ")
				}
				put("scaling_available_frequencies", nodeR, 0o444, s.String()+"\n")
			}
			if cf.Driver.V == "amd-pstate-epp" {
				put("energy_performance_preference", nodeA, 0o644, "balance_performance\n")
				put("energy_performance_available_preferences", nodeR, 0o444, "default performance balance_performance balance_power power \n")
			}
			l, tg := layout.CPUFreqLink(k, k)
			b.link(l, tg)
		}
		if cf.Driver.V == "acpi-cpufreq" {
			b.sys(path.Join(cpufreqDir, "boost"), nodeA, 0o644, strconv.Itoa(cf.Boost.V)+"\n")
		}
	}
	if hs := p.HSMP; hs != nil {
		for s := 0; s < topo.sockets; s++ {
			dir := layout.HSMPHwmonDir(s)
			b.sys(path.Join(dir, "name"), nodeR, 0o444, "amd_hsmp_hwmon\n")
			b.sys(path.Join(dir, "power1_input"), nodeS, 0o444, "0\n")
			b.sys(path.Join(dir, "power1_cap"), nodeA, 0o644, nodeItoa(hs.CapDefaultUW.V)+"\n")
			b.sys(path.Join(dir, "power1_cap_max"), nodeR, 0o444, nodeItoa(hs.CapMaxUW.V)+"\n")
			l, tg := layout.HwmonClassLink(s, "devices/platform/amd_hsmp")
			b.link(l, tg)
		}
	}
	if gs := p.GPUs; gs != nil {
		switch gs.Vendor {
		case "nvidia":
			b.file(layout.DevNvidiactl, nodeR, 0o666, "")
			b.file(path.Join(layout.StateDir, layout.NVMLDir, "driver_version"), nodeR, 0o444, "test\n")
			mw := func(w float64) string { return nodeItoa(int64(math.Round(w*1000))) + "\n" }
			for i := 0; i < gs.Count.V; i++ {
				put := func(name string, owner layout.Owner, mode fs.FileMode, content string) {
					b.file(path.Join(layout.StateDir, layout.NVMLGPUDir(i), name), owner, mode, content)
				}
				put("name", nodeR, 0o444, gs.Product.V+"\n")
				put("uuid", nodeR, 0o444, fmt.Sprintf("GPU-test-%d\n", i))
				put("pci_bus_id", nodeR, 0o444, fmt.Sprintf("00000000:%02x:00.0\n", 0x10+i))
				put("power_min_limit_mw", nodeR, 0o444, mw(gs.Limits.MinW.V))
				put("power_max_limit_mw", nodeR, 0o444, mw(gs.Limits.MaxW.V))
				put("power_default_limit_mw", nodeR, 0o444, mw(gs.Limits.DefaultW.V))
				put("power_limit_mw", nodeA, 0o644, mw(gs.Limits.DefaultW.V))
				put("enforced_power_limit_mw", nodeS, 0o444, mw(gs.Limits.DefaultW.V))
				put("power_draw_mw", nodeS, 0o444, "0\n")
				put("total_energy_mj", nodeS, 0o444, "0\n")
				put("utilization_gpu_pct", nodeS, 0o444, "0\n")
			}
		case "amd":
			nodeRenderCard(b, 0x02, 0, "0x1a03", "0x030000")
			uw := func(w float64) string { return nodeItoa(int64(math.Round(w*1e6))) + "\n" }
			for i := 0; i < gs.Count.V; i++ {
				bus, hwmon := 0x10+i, i+2
				nodeRenderCard(b, bus, i+1, "0x1002", "0x120000")
				dir := path.Join(layout.PCIDevDir(bus), "hwmon", "hwmon"+strconv.Itoa(hwmon))
				b.sys(path.Join(dir, "name"), nodeR, 0o444, "amdgpu\n")
				b.sys(path.Join(dir, "power1_cap"), nodeA, 0o644, uw(gs.Limits.DefaultW.V))
				b.sys(path.Join(dir, "power1_cap_min"), nodeR, 0o444, uw(gs.Limits.MinW.V))
				b.sys(path.Join(dir, "power1_cap_max"), nodeR, 0o444, uw(gs.Limits.MaxW.V))
				b.sys(path.Join(dir, "power1_cap_default"), nodeR, 0o444, uw(gs.Limits.DefaultW.V))
				b.sys(path.Join(dir, "power1_input"), nodeS, 0o444, "0\n")
				l, tg := layout.HwmonClassLink(hwmon, layout.PCIDevDir(bus))
				b.link(l, tg)
			}
		}
	}
	return &Tree{Root: b.root, SysRoot: b.abs(layout.SysDir), CPUInfoPath: b.abs(layout.CPUInfoFile), StateRoot: b.abs(layout.StateDir), Files: b.files}
}

func nodeRenderZone(b *nodeTestTree, ct string, idx []int, zs ZoneSpec, name string, energyUJ map[string]int64) {
	dir := layout.PowercapZoneDir(ct, idx...)
	l, tg := layout.PowercapClassLink(ct, idx...)
	b.link(l, tg)
	b.sys(path.Join(dir, "name"), nodeR, 0o444, name+"\n")
	enabledOwner := nodeR
	for _, c := range zs.Constraints {
		if c.RejectWrites.V == "" {
			enabledOwner = nodeA
		}
	}
	b.sys(path.Join(dir, "enabled"), enabledOwner, 0o644, strconv.Itoa(zs.Enabled.V)+"\n")
	b.sys(path.Join(dir, "energy_uj"), nodeS, 0o400, nodeItoa(energyUJ[dir])+"\n")
	b.sys(path.Join(dir, "max_energy_range_uj"), nodeR, 0o444, nodeItoa(MaxEnergyRangeUJ(zs.EnergyUnitNJ.V))+"\n")
	for i, c := range zs.Constraints {
		pre := path.Join(dir, fmt.Sprintf("constraint_%d_", i))
		b.sys(pre+"name", nodeR, 0o444, c.Name+"\n")
		if c.RejectWrites.V != "" {
			b.reject(pre + "power_limit_uw")
			b.sys(pre+"time_window_us", nodeR, 0o644, nodeItoa(c.TimeWindowUS.V)+"\n")
		} else {
			b.sys(pre+"power_limit_uw", nodeA, 0o644, nodeItoa(c.PowerLimitUW.V)+"\n")
			b.sys(pre+"time_window_us", nodeA, 0o644, nodeItoa(c.TimeWindowUS.V)+"\n")
		}
		b.sys(pre+"max_power_uw", nodeR, 0o444, nodeItoa(c.MaxPowerUW.V)+"\n")
	}
	for j, ch := range zs.Children {
		nodeRenderZone(b, ct, append(append([]int(nil), idx...), j), ch, nodeZoneName(ch.NameFormat), energyUJ)
	}
}

// nodeRenderCard renders a PCI function with a DRM card and the class link
// tools enumerate.
func nodeRenderCard(b *nodeTestTree, bus, card int, vendor, class string) {
	dev := layout.PCIDevDir(bus)
	b.sys(path.Join(dev, "vendor"), nodeR, 0o444, vendor+"\n")
	b.sys(path.Join(dev, "class"), nodeR, 0o444, class+"\n")
	cardDir := layout.DRMCardDir(bus, card)
	b.dir(path.Join(layout.SysDir, cardDir))
	b.link(path.Join(cardDir, "device"), path.Join("..", "..", "..", path.Base(dev)))
	b.link(path.Join(nodeDRMClassDir, "card"+strconv.Itoa(card)), path.Join("..", "..", cardDir))
}

func nodeNewNode(tb testing.TB, p *Profile, tr *Tree) *Node {
	tb.Helper()
	n, err := NewNode(p, tr, Options{Start: nodeStart})
	if err != nil {
		tb.Fatalf("NewNode: %v", err)
	}
	return n
}

func nodeStep(tb testing.TB, n *Node, dt time.Duration) {
	tb.Helper()
	if err := n.Step(dt); err != nil {
		tb.Fatalf("Step(%v): %v", dt, err)
	}
}

// nodeRead returns a file of the tree by its path relative to the root.
func nodeRead(tb testing.TB, tr *Tree, rel string) string {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(tr.Root, filepath.FromSlash(rel)))
	if err != nil {
		tb.Fatal(err)
	}
	return string(b)
}

func nodeReadInt64(tb testing.TB, tr *Tree, rel string) int64 {
	tb.Helper()
	v, err := strconv.ParseInt(strings.TrimSpace(nodeRead(tb, tr, rel)), 10, 64)
	if err != nil {
		tb.Fatal(err)
	}
	return v
}

// nodeWrite writes as the agent does: truncate, then the value without a
// newline (applyRAPLPackageCap in cmd/agent).
func nodeWrite(tb testing.TB, tr *Tree, rel, v string) {
	tb.Helper()
	if err := os.WriteFile(filepath.Join(tr.Root, filepath.FromSlash(rel)), []byte(v), 0); err != nil {
		tb.Fatal(err)
	}
}

func nodeZoneFile(name string, idx ...int) string {
	return path.Join(layout.SysDir, layout.PowercapZoneDir("intel-rapl", idx...), name)
}

func nodePolicyFile(k int, name string) string {
	return path.Join(layout.SysDir, layout.PolicyDir(k), name)
}

func nodeNVMLFile(i int, name string) string {
	return path.Join(layout.StateDir, layout.NVMLGPUDir(i), name)
}

func nodeEvents(n *Node, rel string) []Event {
	var out []Event
	for _, e := range n.State().Events {
		if e.File == rel {
			out = append(out, e)
		}
	}
	return out
}

// nodeSnapshot maps every entry under root to its content, link target or kind.
func nodeSnapshot(tb testing.TB, root string) map[string]string {
	tb.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			tg, err := os.Readlink(p)
			out[rel] = "-> " + tg
			return err
		case d.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			out[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// H12: the same profile, load, writes and steps give a byte-identical tree.
// Nothing may depend on map order, wall time or randomness.
func TestH12SameInputsGiveAByteIdenticalTree(t *testing.T) {
	t.Parallel()
	run := func() map[string]string {
		p := nodeNVLProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		n.SetLoad(Load{CPUUtil: []float64{1, 0.4}, GPUUtil: []float64{1, 0.5, 0.2}, MemoryIntensity: 0.3})
		nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000001")
		nodeWrite(t, tr, nodeNVMLFile(1, "power_limit_mw"), "250000")
		nodeWrite(t, tr, nodePolicyFile(3, "scaling_max_freq"), "2000000")
		for i := 0; i < 37; i++ {
			nodeStep(t, n, 130*time.Millisecond)
		}
		return nodeSnapshot(t, tr.Root)
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("trees differ in size: %d and %d entries", len(a), len(b))
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	changed := 0
	for _, k := range keys {
		if a[k] != b[k] {
			t.Errorf("%s differs: %q and %q", k, a[k], b[k])
		}
		if strings.HasSuffix(k, "energy_uj") && a[k] != "0\n" {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("no energy counter advanced, the comparison proves nothing")
	}
}

// H13: AdvanceTo over irregular intervals integrates the same energy as
// uniform steps, while caps, frequencies and averages are still settling and
// while inputs change between two substep boundaries. Loads change through
// SetLoad, PL1 and the NVML limit settle over substeps, and PL2 is a ceiling
// that takes effect at the readback; each change first commits the energy at
// the old power, so neither run sees a counter go backwards.
func TestH13IrregularIntervalsIntegrateTheSameEnergy(t *testing.T) {
	t.Parallel()
	ms := time.Millisecond
	setup := func() (*Node, *Tree) {
		p := nodeNVLProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		n.SetLoad(Load{CPUUtil: []float64{1, 0.4}, GPUUtil: []float64{1}})
		nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000000")
		nodeWrite(t, tr, nodeNVMLFile(0, "power_limit_mw"), "250000")
		return n, tr
	}
	// Every instant is off the 100 ms grid.
	events := map[time.Duration]func(*Node, *Tree){
		740 * ms: func(n *Node, _ *Tree) {
			n.SetLoad(Load{CPUUtil: []float64{0.2, 1}, GPUUtil: []float64{0.3, 1}})
		},
		1460 * ms: func(_ *Node, tr *Tree) {
			nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 1), "100000000")
		},
		2220 * ms: func(_ *Node, tr *Tree) {
			nodeWrite(t, tr, nodeZoneFile("constraint_1_power_limit_uw", 0), "60000000")
		},
		3020 * ms: func(n *Node, _ *Tree) {
			n.SetLoad(Load{CPUUtil: []float64{1, 0.4}, GPUUtil: []float64{1}})
		},
		4340 * ms: func(_ *Node, tr *Tree) {
			nodeWrite(t, tr, nodeZoneFile("constraint_1_power_limit_uw", 0), "198000000")
		},
		5560 * ms: func(_ *Node, tr *Tree) {
			nodeWrite(t, tr, nodeNVMLFile(0, "power_limit_mw"), "300000")
		},
		6780 * ms: func(n *Node, _ *Tree) {
			n.SetLoad(Load{CPUUtil: []float64{0.6}, GPUUtil: []float64{0.1, 1}})
		},
	}
	files := []struct {
		rel   string
		count int64
	}{
		{nodeZoneFile("energy_uj", 0), 61},
		{nodeZoneFile("energy_uj", 1), 61},
		{nodeZoneFile("energy_uj", 0, 0), 15},
		{nodeNVMLFile(0, "total_energy_mj"), 1},
		{nodeNVMLFile(1, "total_energy_mj"), 1},
	}
	// run reaches each instant, checks that no counter went backwards, then
	// applies the event due there.
	run := func(name string, instants []time.Duration) *Tree {
		n, tr := setup()
		last := map[string]int64{}
		for _, at := range instants {
			if err := n.AdvanceTo(nodeStart.Add(at)); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				v := nodeReadInt64(t, tr, f.rel)
				if v < last[f.rel] {
					t.Errorf("%s: %s went backwards at %v: %d -> %d", name, f.rel, at, last[f.rel], v)
				}
				last[f.rel] = v
			}
			if ev := events[at]; ev != nil {
				ev(n, tr)
			}
		}
		return tr
	}
	var fine []time.Duration
	for at := 20 * ms; at <= 10*time.Second; at += 20 * ms {
		fine = append(fine, at)
	}
	var coarse []time.Duration
	for _, v := range []int{370, 420, 740, 1460, 1461, 2220, 3020, 3759, 4340, 5560, 6780, 8690, 10000} {
		coarse = append(coarse, time.Duration(v)*ms)
	}
	ut, it := run("20 ms steps", fine), run("irregular", coarse)

	for _, f := range files {
		u, i := nodeReadInt64(t, ut, f.rel), nodeReadInt64(t, it, f.rel)
		if u == 0 {
			t.Fatalf("%s did not advance", f.rel)
		}
		if d := i - u; d > f.count || d < -f.count {
			t.Errorf("%s: irregular %d, uniform %d, want within one count (%d)", f.rel, i, u, f.count)
		}
	}
	if u, i := nodeRead(t, ut, nodeNVMLFile(0, "power_draw_mw")), nodeRead(t, it, nodeNVMLFile(0, "power_draw_mw")); u != i {
		t.Errorf("power_draw_mw: irregular %q, uniform %q", i, u)
	}
}

// H28: a Node created over a tree that already ran continues its counters and
// keeps its limits, so a simulator restart reads as neither a wrap nor a
// reset.
func TestH28NewNodeContinuesFromTheFilesOnDisk(t *testing.T) {
	t.Parallel()
	p := nodeNVLProfile()
	tr := nodeRender(t, p, nil)
	first := nodeNewNode(t, p, tr)
	first.SetLoad(Load{CPUUtil: []float64{1}, GPUUtil: []float64{1}})
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000000")
	for i := 0; i < 30; i++ {
		nodeStep(t, first, 100*time.Millisecond)
	}
	files := []struct {
		rel   string
		count int64
	}{
		{nodeZoneFile("energy_uj", 0), 62},
		{nodeZoneFile("energy_uj", 1), 62},
		{nodeZoneFile("energy_uj", 0, 0), 16},
		{nodeNVMLFile(0, "total_energy_mj"), 0},
	}
	drawMW := nodeReadInt64(t, tr, nodeNVMLFile(0, "power_draw_mw"))
	before := map[string]int64{}
	for _, f := range files {
		before[f.rel] = nodeReadInt64(t, tr, f.rel)
		if before[f.rel] == 0 {
			t.Fatalf("%s did not advance before the restart", f.rel)
		}
	}

	second, err := NewNode(p, tr, Options{Start: nodeStart.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	nodeStep(t, second, 0)
	for _, f := range files {
		if got := nodeReadInt64(t, tr, f.rel); got < before[f.rel] || got > before[f.rel]+f.count {
			t.Errorf("%s after restart = %d, want %d plus at most one count", f.rel, got, before[f.rel])
		}
	}
	st := second.State()
	if st.Packages[0].RequestedCapW != 120 || math.Abs(st.Packages[0].CapW-120) > 1e-9 {
		t.Errorf("package 0 after restart: requested %v W, cap %v W, want the 120 W limit on disk", st.Packages[0].RequestedCapW, st.Packages[0].CapW)
	}
	if got := st.GPUs[0].AvgPowerW * 1000; drawMW < 300000 || math.Abs(got-float64(drawMW)) > 1e-6 {
		t.Errorf("GPU 0 average after restart = %v mW, want the %d mW on disk", got, drawMW)
	}

	second.SetLoad(Load{CPUUtil: []float64{1}, GPUUtil: []float64{1}})
	nodeStep(t, second, 100*time.Millisecond)
	if got := nodeReadInt64(t, tr, nodeNVMLFile(0, "power_draw_mw")); math.Abs(float64(got-drawMW)) > 0.01*float64(drawMW) {
		t.Errorf("GPU 0 power_draw_mw one substep after restart = %d, want the %d mW average to carry on", got, drawMW)
	}
	start := nodeReadInt64(t, tr, nodeZoneFile("energy_uj", 0))
	nodeStep(t, second, time.Second)
	gotW := float64(nodeReadInt64(t, tr, nodeZoneFile("energy_uj", 0))-start) / 1e6
	if want := second.State().Packages[0].PowerW; math.Abs(gotW-want) > 0.01*want {
		t.Errorf("package 0 power from energy_uj after restart = %.2f W, want %.2f W", gotW, want)
	}
}
