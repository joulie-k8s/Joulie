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
type ZoneSpec struct {
	Repeat           string            `json:"repeat"`
	NameFormat       string            `json:"nameFormat"`
	Enabled          Fact[int]         `json:"enabled"`
	EnergyUnitNJ     Fact[int64]       `json:"energyUnitNJ"`
	PowerUnitUW      Fact[int64]       `json:"powerUnitUW"`
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
}

// HSMPSpec describes the AMD HSMP socket power caps.
type HSMPSpec struct {
	Layout       Fact[string] `json:"layout"`
	CapMaxUW     Fact[int64]  `json:"capMaxUW"`
	CapDefaultUW Fact[int64]  `json:"capDefaultUW"`
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
	Tools             []struct {
		Name    string `json:"name"`
		Variant string `json:"variant"`
	} `json:"tools"`
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
	} `json:"gpu"`
}

// Tree is a rendered node. Root holds sys/, cpuinfo, state/ and bin/; Files
// lists every entry Render created. NodeName is the node the tree was
// rendered for.
type Tree struct {
	Root        string
	NodeName    string
	SysRoot     string
	CPUInfoPath string
	StateRoot   string
	Files       []layout.FileSpec
}

// RenderOptions are the per-node inputs of Render.
type RenderOptions struct {
	NodeName      string
	Seed          uint64
	EnergyStartUJ map[string]int64
}

// Load is the work a node runs, set with Node.SetLoad.
type Load struct {
	CPUUtil         []float64
	CPUClass        string
	GPUUtil         []float64
	GPUClass        string
	MemoryIntensity float64
}

// Options configure a Node. A zero Substep means 100 ms. A positive
// RenderCheckInterval checks render-owned files only once per interval;
// agent-writable files are still read back every step. Zero checks every
// step.
type Options struct {
	Substep             time.Duration
	Start               time.Time
	Frozen              bool
	RenderCheckInterval time.Duration
}

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
// product name). Index is the socket or the GPU index; SysfsPath is the
// component's powercap zone or PCI device, relative to sys/.
//
// EnergyJ integrates PowerW since the Node started; DeviceEnergyJ is the
// GPU's own counter, total_energy_mj / 1000. LimitW is the limit in effect
// and RequestedLimitW the one written; both are 0 without a power limit.
// SpeedHz, SpeedMaxHz and SpeedLimitHz are a package's current, hardware
// maximum and effective maximum frequency, from cpufreq. Utilization is in
// 0..1. Measured says a real collector could read the component's energy or
// power: a package or DRAM zone with energy_uj, an NVIDIA GPU with
// total_energy_mj, an AMD GPU with power1_average.
type ComponentState struct {
	ID              string
	Type            string
	Name            string
	Vendor          string
	Model           string
	UUID            string
	PCIBusID        string
	SysfsPath       string
	Parent          string
	Index           int
	Measured        bool
	PowerW          float64
	EnergyJ         float64
	DeviceEnergyJ   float64
	LimitW          float64
	RequestedLimitW float64
	SpeedHz         float64
	SpeedMaxHz      float64
	SpeedLimitHz    float64
	Utilization     float64
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
// kinds. Component is the hw.id of the component the file belongs to, such
// as cpu1 or gpu3, and empty for a file of the whole node.
type Event struct {
	Time      time.Time `json:"time"`
	File      string    `json:"file"`
	Kind      string    `json:"kind"`
	Written   string    `json:"written"`
	Effective string    `json:"effective"`
	Component string    `json:"component,omitempty"`
}

// Event kinds, the values of Event.Kind. EventResetPersistenceOff is a GPU
// limit put back to its default because persistence mode is off and no
// client holds the GPU.
const (
	EventAccepted            = "accepted"
	EventQuantized           = "quantized"
	EventResolved            = "resolved"
	EventClamped             = "clamped"
	EventRevertedOutOfRange  = "reverted-out-of-range"
	EventParseError          = "parse-error"
	EventRestored            = "restored"
	EventResetPersistenceOff = "reset-persistence-off"
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
