package hwemu

import (
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// Fact is one hardware fact and where it comes from. Src is
// published:<id>[#loc], corpus:<id>[#file], derived:<id>[#formula] or
// assumed:<reason>.
type Fact[T any] struct {
	V   T      `json:"v"`
	Src string `json:"src"`
}

// Source is one entry of a profile's sources, cited by id from Fact.Src.
type Source struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// Profile describes one kind of node. A nil Powercap, CPUFreq, HSMP or GPUs
// means the node has none: no class/powercap, no cpufreq policies, no HSMP
// hwmon, no GPU.
type Profile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Extends       string            `json:"extends"`
	Sources       map[string]Source `json:"sources"`
	Node          NodeSpec          `json:"node"`
	PCI           []PCIDevice       `json:"pci"`
	CPU           CPUSpec           `json:"cpu"`
	Powercap      *PowercapSpec     `json:"powercap"`
	CPUFreq       *CPUFreqSpec      `json:"cpufreq"`
	HSMP          *HSMPSpec         `json:"hsmp"`
	GPUs          *GPUSpec          `json:"gpus"`
	Physics       PhysicsSpec       `json:"physics"`
}

// NodeSpec is the Kubernetes Node of a profile. NFD is present or absent;
// capacity is in logical CPUs and Ki.
type NodeSpec struct {
	NFD      string                `json:"nfd"`
	Labels   map[string]LabelGroup `json:"labels"`
	Taints   []corev1.Taint        `json:"taints"`
	Capacity struct {
		CPU      Fact[int64] `json:"cpu"`
		MemoryKi Fact[int64] `json:"memoryKi"`
	} `json:"capacity"`
	Reserved struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
	} `json:"reserved"`
	ExtendedResources map[string]int64 `json:"extendedResources"`
}

// LabelGroup is a set of node labels with one source.
type LabelGroup struct {
	Src    string            `json:"src"`
	Values map[string]string `json:"values"`
}

// PCIDevice is a PCI function by class, vendor and device ID, in 0x hex. DRM
// also gives it a DRM card.
type PCIDevice struct {
	Class  Fact[string] `json:"class"`
	Vendor Fact[string] `json:"vendor"`
	Device Fact[string] `json:"device"`
	DRM    bool         `json:"drm"`
}

// CPUSpec is what /proc/cpuinfo and the CPU topology show.
type CPUSpec struct {
	VendorID         Fact[string]  `json:"vendorID"`
	ModelName        Fact[string]  `json:"modelName"`
	Family           Fact[int]     `json:"family"`
	Model            Fact[int]     `json:"model"`
	Stepping         Fact[int]     `json:"stepping"`
	Sockets          Fact[int]     `json:"sockets"`
	DiesPerPackage   Fact[int]     `json:"diesPerPackage"`
	CoresPerSocket   Fact[int]     `json:"coresPerSocket"`
	ThreadsPerCore   Fact[int]     `json:"threadsPerCore"`
	Numbering        string        `json:"numbering"`
	OneSocketPerVCPU bool          `json:"oneSocketPerVCPU"`
	Hypervisor       bool          `json:"hypervisor"`
	CPUMHz           Fact[float64] `json:"cpuMHz"`
	Flags            string        `json:"flags"`
	CPUInfoVerbatim  string        `json:"cpuinfoVerbatim"`
}

// PowercapSpec lists the powercap control types, such as intel-rapl and
// intel-rapl-mmio.
type PowercapSpec struct {
	ControlTypes []ControlTypeSpec `json:"controlTypes"`
}

// ControlTypeSpec is one control type and its top-level zones.
type ControlTypeSpec struct {
	Name  string     `json:"name"`
	Zones []ZoneSpec `json:"zones"`
}

// ZoneSpec is a powercap zone. Repeat is per-package, per-die or once.
// NameFormat fills the zone's name file, such as package-%d; a format without
// verbs is the name itself, as for psys and the dram and core children.
// LimitBits is the width of the power limit field of the zone's registers,
// in raw power units: unset, the MSR interface's, 15 bits for PL1 and PL2,
// 17 on psys and 13 for PL4; 18 on TPMI (intel_rapl_common.c, the
// *_POWER_LIMIT*_MASK values). A limit written past it wraps.
type ZoneSpec struct {
	Repeat           string            `json:"repeat"`
	NameFormat       string            `json:"nameFormat"`
	Enabled          Fact[int]         `json:"enabled"`
	EnergyUnitNJ     Fact[int64]       `json:"energyUnitNJ"`
	PowerUnitUW      Fact[int64]       `json:"powerUnitUW"`
	LimitBits        Fact[int]         `json:"limitBits"`
	MaxEnergyRangeUJ *Fact[int64]      `json:"maxEnergyRangeUJ"`
	Constraints      []ConstraintSpec  `json:"constraints"`
	Children         []ZoneSpec        `json:"children"`
	ExtraFiles       map[string]string `json:"extraFiles"`
}

// ConstraintSpec is constraint_<i>_* of a zone. RejectWrites is "", locked or
// disabled.
type ConstraintSpec struct {
	Name         string       `json:"name"`
	PowerLimitUW Fact[int64]  `json:"powerLimitUW"`
	MaxPowerUW   Fact[int64]  `json:"maxPowerUW"`
	TimeWindowUS Fact[int64]  `json:"timeWindowUS"`
	RejectWrites Fact[string] `json:"rejectWrites"`
}

// CPUFreqSpec describes the cpufreq policies. PolicyGrouping is per-cpu or
// per-core; AvailableKHz is set only for table drivers.
//
// The governor facts follow. DriverMode is active, passive or guided, as
// intel_pstate/status or amd_pstate/status shows it (empty for a driver
// without modes); HWP says the CPU has hardware P-states; EPP lists the
// energy-performance preferences, when the policies have them; BoostScope is
// global (cpufreq/boost), per-cpu (each policy's boost) or no-turbo
// (intel_pstate/no_turbo, inverted); Tunables are the governors' own
// attributes; Uncore, when set, renders intel_uncore_frequency. BaseKHz is
// the base frequency, the top non-turbo one: the maximum a policy keeps
// with turbo off. PolicyBoost is what every policyN/boost shows at boot
// where the boost switch is global and the driver has set_boost
// (acpi-cpufreq): 0 when unset, as on the acpi-cpufreq nodes captured,
// which show 1 in cpufreq/boost and 0 in every policy's file while the
// CPUs boost.
type CPUFreqSpec struct {
	Driver             Fact[string]  `json:"driver"`
	Governor           Fact[string]  `json:"governor"`
	AvailableGovernors Fact[string]  `json:"availableGovernors"`
	PolicyGrouping     Fact[string]  `json:"policyGrouping"`
	CPUInfoMinKHz      Fact[int64]   `json:"cpuinfoMinKHz"`
	CPUInfoMaxKHz      Fact[int64]   `json:"cpuinfoMaxKHz"`
	AvailableKHz       Fact[[]int64] `json:"availableKHz"`
	Boost              Fact[int]     `json:"boost"`
	Minimal            bool          `json:"minimal"`

	DriverMode  Fact[string]          `json:"driverMode"`
	HWP         Fact[bool]            `json:"hwp"`
	EPP         *EPPSpec              `json:"epp"`
	BoostScope  Fact[string]          `json:"boostScope"`
	Tunables    *GovernorTunablesSpec `json:"tunables"`
	Uncore      *UncoreSpec           `json:"uncore"`
	BaseKHz     Fact[int64]           `json:"baseKHz"`
	PolicyBoost Fact[int]             `json:"policyBoost"`
}

// EPPSpec is energy_performance_available_preferences (Available, the words
// separated by spaces, as the file lists them) and the
// energy_performance_preference a policy starts with (Default).
type EPPSpec struct {
	Available Fact[string] `json:"available"`
	Default   Fact[string] `json:"default"`
}

// GovernorTunablesSpec holds the tunables of the governors that have them,
// under the names of their sysfs attributes (cpufreq.rst, "Generic Scaling
// Governors"). Times are in us.
type GovernorTunablesSpec struct {
	Schedutil struct {
		RateLimitUS Fact[int64] `json:"rate_limit_us"`
	} `json:"schedutil"`
	Ondemand struct {
		UpThreshold  Fact[int64] `json:"up_threshold"`
		SamplingRate Fact[int64] `json:"sampling_rate"`
	} `json:"ondemand"`
	Conservative struct {
		FreqStep      Fact[int64] `json:"freq_step"`
		UpThreshold   Fact[int64] `json:"up_threshold"`
		DownThreshold Fact[int64] `json:"down_threshold"`
	} `json:"conservative"`
}

// UncoreSpec is the Intel uncore frequency driver. Layout is per-die
// (package_<p>_die_<d> directories) or per-cluster (uncore<n> directories
// of the TPMI interface, ClustersPerDie of them per die); every domain
// starts at the same initial range. ELC says the efficiency latency control
// attributes are present.
type UncoreSpec struct {
	Layout         Fact[string] `json:"layout"`
	ClustersPerDie Fact[int]    `json:"clustersPerDie"`
	InitialMinKHz  Fact[int64]  `json:"initialMinKHz"`
	InitialMaxKHz  Fact[int64]  `json:"initialMaxKHz"`
	ELC            Fact[bool]   `json:"elc"`
}

// HSMPSpec describes the AMD HSMP socket power caps.
type HSMPSpec struct {
	Layout       Fact[string] `json:"layout"`
	CapMaxUW     Fact[int64]  `json:"capMaxUW"`
	CapDefaultUW Fact[int64]  `json:"capDefaultUW"`
}

// GPUTool is a GPU tool on the node's PATH and the variant of it fakesmi
// emulates (package smi). Requires lists, in order, the files the real tool
// needs before it runs; Render copies them to state/tools.json without their
// sources, and fakesmi checks them under its root file system.
type GPUTool struct {
	Name     string               `json:"name"`
	Variant  string               `json:"variant"`
	Requires []GPUToolRequirement `json:"requires,omitempty"`
}

// GPUToolRequirement is one file a real tool needs before it runs, such as
// the ELF loader of nvidia-smi or the interpreter of rocm-smi's script.
// Paths are absolute in the agent container, and one of them existing
// satisfies the requirement. When none exists, the tool prints Stderr, if
// any, and exits with Exit. Src is the provenance of the whole entry, as a
// LabelGroup's is, so the entry is kept as written when the profile loads.
type GPUToolRequirement struct {
	Paths  []string `json:"paths"`
	Exit   int      `json:"exit"`
	Stderr string   `json:"stderr,omitempty"`
	Src    string   `json:"src"`
}

// GPUSpec describes the node's GPUs, how Kubernetes sees them and which tool
// variants report on them.
type GPUSpec struct {
	Vendor  string       `json:"vendor"`
	Count   Fact[int]    `json:"count"`
	Product Fact[string] `json:"product"`
	PCI     PCIDevice    `json:"pci"`
	Limits  struct {
		MinW     Fact[float64] `json:"minW"`
		MaxW     Fact[float64] `json:"maxW"`
		DefaultW Fact[float64] `json:"defaultW"`
		// CurrentW is the limit in force when the node starts, when an admin
		// set it away from the default; unset, the default is in force.
		CurrentW Fact[float64] `json:"currentW"`
	} `json:"limits"`
	PowerSensor       Fact[string]      `json:"powerSensor"`
	PPT1              Fact[string]      `json:"ppt1"`
	VBIOSVersion      Fact[string]      `json:"vbiosVersion"`
	GFXVersion        Fact[string]      `json:"gfxVersion"`
	Exposure          Fact[string]      `json:"exposure"`
	Labeller          Fact[string]      `json:"labeller"`
	MIGProfile        string            `json:"migProfile"`
	MIGDevicesPerGPU  int               `json:"migDevicesPerGPU"`
	TimesliceReplicas int               `json:"timesliceReplicas"`
	FieldSupport      map[string]string `json:"fieldSupport"`
	SecondaryDies     []int             `json:"secondaryDies"`
	Tools             []GPUTool         `json:"tools"`
	// PersistenceMode is PersistenceEnabled or PersistenceDisabled, NVIDIA
	// only. With persistence off, the driver unloads when no client holds
	// the GPU, and the power limit goes back to its default.
	PersistenceMode Fact[string] `json:"persistenceMode"`
	// Architecture, ComputeCapability ("9.0"), MemoryMiB and MIGCapable feed
	// the GPU feature discovery labels gpu.family, gpu.compute.major and
	// minor, gpu.memory and mig.capable. NVIDIA only.
	Architecture      Fact[string] `json:"architecture"`
	ComputeCapability Fact[string] `json:"computeCapability"`
	MemoryMiB         Fact[int]    `json:"memoryMiB"`
	MIGCapable        Fact[bool]   `json:"migCapable"`
	// DPM is the amdgpu performance level and clock tables, AMD only.
	DPM *DPMSpec `json:"dpm"`
	// Clocks are the NVIDIA clock ranges and lock behaviour, NVIDIA only.
	Clocks *NVClocksSpec `json:"clocks"`
	// Transfer is the peer link between the node's GPUs.
	Transfer *GPUTransferSpec `json:"transfer"`
}

// DPMSpec is the amdgpu dynamic power management of a family.
// Levels.Accept lists the power_dpm_force_performance_level words the
// firmware takes and Levels.Reject the errno (EINVAL, EOPNOTSUPP or EBUSY)
// of each word it refuses; Default is the level at boot. Tables are the
// pp_dpm_* clock levels; PState the clocks of profile_standard and
// profile_peak; OD the overdrive file pp_od_clk_voltage; PowerProfiles the
// pp_power_profile_mode table; PMPolicy the pm_policy attributes.
// AutoHoldS is how long the data path stays raised under auto after the
// GPU's own memory activity stops. UnprivilegedSetIsNoop says a level set
// through rocm-smi without root changes nothing and still exits 0.
type DPMSpec struct {
	Levels struct {
		Accept Fact[[]string]          `json:"accept"`
		Reject map[string]Fact[string] `json:"reject"`
	} `json:"levels"`
	Default               Fact[string]       `json:"default"`
	Tables                DPMTablesSpec      `json:"tables"`
	PState                DPMPStatesSpec     `json:"pstate"`
	OD                    *ODSpec            `json:"od"`
	PowerProfiles         *PowerProfilesSpec `json:"powerProfiles"`
	PMPolicy              *PMPolicySpec      `json:"pmPolicy"`
	AutoHoldS             Fact[float64]      `json:"autoHoldS"`
	UnprivilegedSetIsNoop Fact[bool]         `json:"unprivilegedSetIsNoop"`
}

// DPMTablesSpec holds the clock levels of each pp_dpm_* file, in MHz,
// ascending. A fine-grained sclk lists only its range, as the kernel prints
// it; PCIe lists each level as pp_dpm_pcie prints it after the index, such
// as "16.0GT/s, x16 619Mhz".
type DPMTablesSpec struct {
	Sclk struct {
		Fine   Fact[bool]  `json:"fine"`
		MinMHz Fact[int64] `json:"minMHz"`
		MaxMHz Fact[int64] `json:"maxMHz"`
	} `json:"sclk"`
	Mclk   Fact[[]int64]  `json:"mclk"`
	Fclk   Fact[[]int64]  `json:"fclk"`
	Socclk Fact[[]int64]  `json:"socclk"`
	PCIe   Fact[[]string] `json:"pcie"`
}

// DPMPStatesSpec holds the clocks the profile levels pin.
type DPMPStatesSpec struct {
	Standard DPMPStateSpec `json:"standard"`
	Peak     DPMPStateSpec `json:"peak"`
}

// DPMPStateSpec is one pinned state, in MHz per clock domain.
type DPMPStateSpec struct {
	SclkMHz   Fact[int64] `json:"sclkMHz"`
	MclkMHz   Fact[int64] `json:"mclkMHz"`
	FclkMHz   Fact[int64] `json:"fclkMHz"`
	SocclkMHz Fact[int64] `json:"socclkMHz"`
}

// ODSpec is pp_od_clk_voltage: Enabled renders it, Writable lists the
// bounds a write may set, from sclkMin, sclkMax, uclkMin and uclkMax.
type ODSpec struct {
	Enabled  Fact[bool]     `json:"enabled"`
	Writable Fact[[]string] `json:"writable"`
}

// PowerProfilesSpec is pp_power_profile_mode: the mode names in index
// order and the default.
type PowerProfilesSpec struct {
	Modes   Fact[[]string] `json:"modes"`
	Default Fact[string]   `json:"default"`
}

// PMPolicySpec holds the pm_policy attributes a family has.
type PMPolicySpec struct {
	SocPState *PMPolicyAttrSpec `json:"soc_pstate"`
	XGMIPLPD  *PMPolicyAttrSpec `json:"xgmi_plpd"`
}

// PMPolicyAttrSpec is one pm_policy attribute: its policy names in index
// order and the index it starts at.
type PMPolicyAttrSpec struct {
	Policies Fact[[]string] `json:"policies"`
	Default  Fact[int]      `json:"default"`
}

// NVClocksSpec is the clock behaviour of an NVIDIA GPU. SM and Mem are the
// ranges of the graphics and memory clocks, the clock each runs at idle and,
// for SM, the highest it sustains under load (a higher lock is clamped);
// Mem.RuntimeLock says -lmc takes effect at run time. IdlePState is the
// performance state at idle; Supported lists the clocks -q -d
// SUPPORTED_CLOCKS prints, and SMMaxByMem the memory clocks under which the
// GPU offers only the lower graphics clocks, each with the highest one (a
// T4 lists 87 graphics clocks at its 5001 MHz memory clock and 24 at
// 405 MHz); a memory clock it does not list offers every graphics clock.
// LockFloorAtIdle says an idle GPU sits at the
// lower bound of its lock (default true); LocksResetOnDeinit says the locks
// go when the driver deinitializes the GPU with persistence off (default
// false).
type NVClocksSpec struct {
	SM struct {
		MinMHz          Fact[int64] `json:"minMHz"`
		MaxMHz          Fact[int64] `json:"maxMHz"`
		IdleMHz         Fact[int64] `json:"idleMHz"`
		SustainedMaxMHz Fact[int64] `json:"sustainedMaxMHz"`
	} `json:"sm"`
	Mem struct {
		MinMHz      Fact[int64] `json:"minMHz"`
		MaxMHz      Fact[int64] `json:"maxMHz"`
		IdleMHz     Fact[int64] `json:"idleMHz"`
		RuntimeLock Fact[bool]  `json:"runtimeLock"`
	} `json:"mem"`
	IdlePState Fact[string] `json:"idlePState"`
	Supported  struct {
		SMMHz      Fact[[]int64]        `json:"smMHz"`
		MemMHz     Fact[[]int64]        `json:"memMHz"`
		SMMaxByMem []NVClockCeilingSpec `json:"smMaxByMem"`
	} `json:"supported"`
	LockFloorAtIdle    *Fact[bool] `json:"lockFloorAtIdle"`
	LocksResetOnDeinit *Fact[bool] `json:"locksResetOnDeinit"`
}

// NVClockCeilingSpec is the highest graphics clock, SMMaxMHz, a GPU offers
// at memory clock MemMHz.
type NVClockCeilingSpec struct {
	MemMHz   Fact[int64] `json:"memMHz"`
	SMMaxMHz Fact[int64] `json:"smMaxMHz"`
}

// GPUTransferSpec is the peer link between the GPUs of a node, in GB/s per
// receiving GPU, and the receiver gate.
type GPUTransferSpec struct {
	PeerLinkGBps Fact[float64]    `json:"peerLinkGBps"`
	ReceiverGate ReceiverGateSpec `json:"receiverGate"`
}

// ReceiverGateSpec is a family whose inbound peer transfers are bounded by
// the receiving GPU's data-path clocks. Enabled is off in every repository
// profile. A receiver forced to its lowest data-path level gets
// SlowFraction of the link; an idle receiver under auto gets MidFraction
// from a MidPairFraction share of its sources and SlowFraction from the
// rest.
type ReceiverGateSpec struct {
	Enabled         Fact[bool]    `json:"enabled"`
	SlowFraction    Fact[float64] `json:"slowFraction"`
	MidFraction     Fact[float64] `json:"midFraction"`
	MidPairFraction Fact[float64] `json:"midPairFraction"`
}

// GPU persistence modes, the values of GPUSpec.PersistenceMode.
const (
	PersistenceEnabled  = "enabled"
	PersistenceDisabled = "disabled"
)

// PhysicsSpec holds the parameters that drive power, energy and settling.
type PhysicsSpec struct {
	CPU struct {
		Model        string        `json:"model"`
		MaxPkgW      Fact[float64] `json:"maxPkgW"`
		IdlePkgW     Fact[float64] `json:"idlePkgW"`
		AlphaUtil    Fact[float64] `json:"alphaUtil"`
		BetaFreq     Fact[float64] `json:"betaFreq"`
		Knee         Fact[float64] `json:"knee"`
		CoreFraction Fact[float64] `json:"coreFraction"`
		FreqRampMS   Fact[int]     `json:"freqRampMS"`
		// IdleGovernorDeltaW is drawn while the package idles and its
		// governor holds the highest frequency (default 0). EPPGain scales
		// how much a lower EPP raises the frequency at a given load
		// (default 1.0); EPPCeilingDrop is how far EPP 128 lowers the
		// full-load frequency, as a fraction of the range (default 0.2).
		IdleGovernorDeltaW Fact[float64]  `json:"idleGovernorDeltaW"`
		EPPGain            *Fact[float64] `json:"eppGain"`
		EPPCeilingDrop     *Fact[float64] `json:"eppCeilingDrop"`
	} `json:"cpu"`
	DRAM struct {
		WattsPerSocket Fact[float64] `json:"wattsPerSocket"`
	} `json:"dram"`
	Platform struct {
		OtherW Fact[float64] `json:"otherW"`
	} `json:"platform"`
	RAPL struct {
		SettleTau string        `json:"settleTau"`
		FloorW    Fact[float64] `json:"floorW"`
	} `json:"rapl"`
	HSMP struct {
		SettleTauMS Fact[int] `json:"settleTauMS"`
	} `json:"hsmp"`
	GPU struct {
		IdleW             Fact[float64] `json:"idleW"`
		NaturalMaxW       Fact[float64] `json:"naturalMaxW"`
		ComputeGamma      Fact[float64] `json:"computeGamma"`
		MemoryEpsilon     Fact[float64] `json:"memoryEpsilon"`
		MemoryGamma       Fact[float64] `json:"memoryGamma"`
		CapSettleTauMS    Fact[int]     `json:"capSettleTauMS"`
		TelemetryWindowMS Fact[int]     `json:"telemetryWindowMS"`
		// The clock model: ClockBeta is the exponent of the compute clock
		// in dynamic power; MemoryKneeFrac the fraction of the maximum
		// compute clock below which memory-bound work slows, by
		// MemoryKneeGamma; DataPathHoldW the idle power of a data path held
		// at its maximum; GatingOffIdleW the idle power with idle gating
		// off; DataPathGamma how much memory-bound work slows with the data
		// path at its minimum.
		ClockBeta       Fact[float64] `json:"clockBeta"`
		MemoryKneeFrac  Fact[float64] `json:"memoryKneeFrac"`
		MemoryKneeGamma Fact[float64] `json:"memoryKneeGamma"`
		DataPathHoldW   Fact[float64] `json:"dataPathHoldW"`
		GatingOffIdleW  Fact[float64] `json:"gatingOffIdleW"`
		DataPathGamma   Fact[float64] `json:"dataPathGamma"`
	} `json:"gpu"`
}

// Tree is a rendered node. Root holds sys/, cpuinfo, state/ and bin/; Files
// lists every entry Render created, sorted by Rel. NodeName is the node the
// tree was rendered for.
type Tree struct {
	Root        string
	NodeName    string
	SysRoot     string
	CPUInfoPath string
	StateRoot   string
	Files       []layout.FileSpec

	// rendered is the content of every file of a tree Render or Ensure laid
	// out, by Rel; nil for a tree assembled otherwise.
	rendered map[string]string
}

// RenderOptions are the per-node inputs of Render.
type RenderOptions struct {
	NodeName      string
	Seed          uint64
	EnergyStartUJ map[string]int64
}

// Load is the work a node runs, set with Node.SetLoad. PeerTransfers are
// the GPU to GPU copies the work makes.
type Load struct {
	CPUUtil         []float64
	CPUClass        string
	GPUUtil         []float64
	GPUClass        string
	MemoryIntensity float64
	PeerTransfers   []PeerTransfer
}

// PeerTransfer is a copy from GPU Src to GPU Dst of the same node, by GPU
// index, at the demanded rate BytesPerS.
type PeerTransfer struct {
	Src, Dst  int
	BytesPerS float64
}

// Options configure a Node. A zero Substep means 100 ms. A positive
// RenderCheckInterval checks render-owned files only once per interval;
// agent-writable files are still read back every step. Zero checks every
// step. ReceiverGate is one of the ReceiverGate* values; empty means
// ReceiverGateProfile. Live makes the Node the content of a live view of
// its tree, such as a FUSE mount (LiveRead, LiveWrite): every write reaches
// the Node through the view, so a Step reads back no agent-writable file and
// checks no render-owned one, and the files on disk follow the Node, for a
// restart to resume from.
type Options struct {
	Substep             time.Duration
	Start               time.Time
	Frozen              bool
	RenderCheckInterval time.Duration
	ReceiverGate        string
	Live                bool
}

// Values of Options.ReceiverGate: the profile's receiverGate.enabled, off on
// every GPU, or on every GPU profile with the profile's fractions.
const (
	ReceiverGateProfile = "profile"
	ReceiverGateOff     = "off"
	ReceiverGateOn      = "on"
)

// State is a snapshot of a Node after a step. Components and Host are what a
// collector on the node could read, in the identity telemetry uses;
// DriverVersion is the NVML driver version, empty without NVIDIA GPUs.
type State struct {
	Time          time.Time
	Packages      []PackageState
	GPUs          []GPUState
	Events        []Event
	Components    []ComponentState
	Host          HostState
	DriverVersion string
}

// ComponentState is one component of a node: a CPU package (Type cpu, ID
// cpu<socket>, Name package-<socket>), its DRAM zone (memory,
// memory<socket>, dram, Parent cpu<socket>) or a GPU (gpu, gpu<index>, the
// product name). Vendor and Model are set for CPU packages (Intel or AMD,
// the cpuinfo model name) and GPUs; UUID and PCIBusID are a GPU's as its
// tool prints them. Index is the socket or the GPU index; SysfsPath is the
// component's powercap zone or PCI device, relative to sys/, and empty when
// the tree has none, as for an NVIDIA GPU, whose state is NVML state.
//
// EnergyJ integrates PowerW since the Node started; DeviceEnergyJ is the
// GPU's own counter, total_energy_mj / 1000. LimitW is the limit in effect
// and RequestedLimitW the one written; both are 0 without a power limit.
// SpeedHz, SpeedMaxHz and SpeedLimitHz are a package's current, hardware
// maximum and effective maximum frequency, from cpufreq: the mean
// scaling_cur_freq, the cpuinfo_max_freq and the mean resolved
// scaling_max_freq of its policies. Utilization is in 0..1. Measured says a
// real collector could read the component's energy or power: a package or
// DRAM zone with energy_uj, an NVIDIA GPU with total_energy_mj, an AMD GPU
// with power1_input or power1_average.
//
// ClockHz is a GPU's current clock per domain, keyed by the
// semconv.ClockDomain* values, nil when the GPU has no clock model.
// ClockMaxHz and ClockLimitHz have the same keys: ClockMaxHz is the
// domain's hardware maximum (the top of its pp_dpm_* table, or NVML's
// maximum clock), ClockLimitHz the maximum in effect, the clock the domain
// reaches at full load under the present performance level, overdrive
// range or clock lock, as SpeedLimitHz is for a package.
// GovernorNative is the governor state in the driver's own words (a
// package's scaling_governor when its policies agree, a GPU's performance
// level or lock state) and GovernorPreset the preset it maps to, empty when
// no single one does. PeerRxBytes and PeerTxBytes count the peer transfer
// bytes a GPU received and sent since the Node started.
//
// The JSON names are the wire format of the emulator's snapshots
// (emulate.ComponentReading is this type).
type ComponentState struct {
	ID              string           `json:"id"`
	Type            string           `json:"type"`
	Name            string           `json:"name"`
	Vendor          string           `json:"vendor,omitempty"`
	Model           string           `json:"model,omitempty"`
	UUID            string           `json:"uuid,omitempty"`
	PCIBusID        string           `json:"pciBusId,omitempty"`
	SysfsPath       string           `json:"sysfsPath,omitempty"`
	Parent          string           `json:"parent,omitempty"`
	Index           int              `json:"index"`
	Measured        bool             `json:"measured"`
	PowerW          float64          `json:"powerW"`
	EnergyJ         float64          `json:"energyJ"`
	DeviceEnergyJ   float64          `json:"deviceEnergyJ,omitempty"`
	LimitW          float64          `json:"limitW,omitempty"`
	RequestedLimitW float64          `json:"requestedLimitW,omitempty"`
	SpeedHz         float64          `json:"speedHz,omitempty"`
	SpeedMaxHz      float64          `json:"speedMaxHz,omitempty"`
	SpeedLimitHz    float64          `json:"speedLimitHz,omitempty"`
	Utilization     float64          `json:"utilization"`
	ClockHz         map[string]int64 `json:"clockHz,omitempty"`
	ClockMaxHz      map[string]int64 `json:"clockMaxHz,omitempty"`
	ClockLimitHz    map[string]int64 `json:"clockLimitHz,omitempty"`
	GovernorNative  string           `json:"governorNative,omitempty"`
	GovernorPreset  string           `json:"governorPreset,omitempty"`
	PeerRxBytes     uint64           `json:"peerRxBytes,omitempty"`
	PeerTxBytes     uint64           `json:"peerTxBytes,omitempty"`
}

// GovernorState is the governor state of a CPU package or a GPU: Native in
// the driver's own words and Preset, the preset it maps to, empty when no
// single one does. The zero value is a component without a governor.
type GovernorState struct {
	Native string `json:"native"`
	Preset string `json:"preset,omitempty"`
}

// Governor is the governor state of c.
func (c ComponentState) Governor() GovernorState {
	return GovernorState{Native: c.GovernorNative, Preset: c.GovernorPreset}
}

// HostState is the whole node: PowerW is the sum of every package, DRAM
// zone and GPU plus the platform's OtherW, and EnergyJ integrates it since
// the Node started. Measured is false on a hypervisor guest, where no
// collector reads host power.
type HostState struct {
	Measured bool
	PowerW   float64
	EnergyJ  float64
	OtherW   float64
}

// PackageState is one CPU package. CapW is the effective cap and
// RequestedCapW the limit in the file.
type PackageState struct {
	Socket        int
	PowerW        float64
	CapW          float64
	RequestedCapW float64
	FreqScale     float64
	EnergyUJ      int64
}

// GPUState is one GPU. LimitW is the limit as set and EnforcedLimitW the one
// in effect.
type GPUState struct {
	Index          int
	PowerW         float64
	AvgPowerW      float64
	LimitW         float64
	EnforcedLimitW float64
}

// Event records what a step did with a file. Kind is one of the Event*
// kinds. Component is the hw.id of the component whose directory holds the
// file, such as cpu1 for a policy of socket 1 or gpu3 for its NVML state,
// and empty for any other file (Components in the package doc). Errno is
// set on an EventRejected: the Errno* name the real driver fails the write
// with.
type Event struct {
	Time      time.Time `json:"time"`
	File      string    `json:"file"`
	Kind      string    `json:"kind"`
	Written   string    `json:"written"`
	Effective string    `json:"effective"`
	Component string    `json:"component,omitempty"`
	Errno     string    `json:"errno,omitempty"`
}

// Event kinds, the values of Event.Kind. EventResetPersistenceOff is a GPU
// limit put back to its default because persistence mode is off and no
// client holds the GPU. EventRejected is a value the real driver refuses
// with an errno, reverted at the step that reads it (rejectWrite).
const (
	EventAccepted            = "accepted"
	EventQuantized           = "quantized"
	EventResolved            = "resolved"
	EventClamped             = "clamped"
	EventRevertedOutOfRange  = "reverted-out-of-range"
	EventParseError          = "parse-error"
	EventRestored            = "restored"
	EventResetPersistenceOff = "reset-persistence-off"
	EventRejected            = "rejected"
)

// EnsureReport says what Ensure did. Fresh is set when it rendered the tree
// into an absent or empty root; otherwise it resumed an existing tree, and
// Added lists the entries, relative to the root, it put back from a fresh
// render.
type EnsureReport struct {
	Fresh bool
	Added []string
}

// Errors of Ensure over an existing tree. Both mean the tree cannot be
// resumed; with no agent mounting the node, remove its root and start again.
var (
	// ErrProfileChanged: the tree was rendered from another profile, for
	// another node, or from other profile values.
	ErrProfileChanged = errors.New("hwemu: the tree was rendered from another profile, node or profile values")
	// ErrTreeConflict: the root is not a tree hwemu rendered, or one of its
	// entries is of another kind than the profile renders.
	ErrTreeConflict = errors.New("hwemu: the tree conflicts with what the profile renders")
)

// MaxEnergyRangeUJ is max_energy_range_uj of a zone whose energy unit is
// unitNJ: the 32-bit counter span converted to uJ, floor((2^32-1)*unitNJ/1000)
// (intel_rapl_common.c:33, 50, 215, 741).
func MaxEnergyRangeUJ(unitNJ int64) int64 {
	return (1<<32 - 1) * unitNJ / 1000
}

// QuantizeUW is the power limit a RAPL zone reads back after v is written: the
// kernel stores whole power units and does not round up, so the result is
// floor(v/unitUW)*unitUW (intel_rapl_common.c:558-563, 744). v is
// non-negative, as the kernel parses it into a u64 (powercap_sys.c:101); a
// unit of zero or less leaves v as written.
func QuantizeUW(v, unitUW int64) int64 {
	if unitUW <= 0 {
		return v
	}
	return v / unitUW * unitUW
}
