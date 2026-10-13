package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeHardwareSpec identifies the node this inventory belongs to.
type NodeHardwareSpec struct {
	// NodeName is the Kubernetes node this inventory describes.
	NodeName string `json:"nodeName"`
}

// NodeHardwareStatus holds the hardware inventory published by the agent.
// It is the single source of truth for static node hardware facts: CPU and
// GPU specs, power cap ranges, frequency landmarks, GPU slicing modes,
// memory, network and inventory resolution confidence.
type NodeHardwareStatus struct {
	CPU                 *NodeHardwareCPU          `json:"cpu,omitempty"`
	GPU                 *NodeHardwareGPU          `json:"gpu,omitempty"`
	Memory              *NodeHardwareMemory       `json:"memory,omitempty"`
	Network             *NodeHardwareNetwork      `json:"network,omitempty"`
	InventoryResolution *InventoryResolution      `json:"inventoryResolution,omitempty"`
	Capabilities        *NodeHardwareCapabilities `json:"capabilities,omitempty"`
	Quality             *NodeHardwareQuality      `json:"quality,omitempty"`
	// UpdatedAt is the RFC3339 time of the last inventory write.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// NodeHardwareCPU describes the CPU package(s) of the node.
type NodeHardwareCPU struct {
	RawModel       string `json:"rawModel,omitempty"`
	Model          string `json:"model,omitempty"`
	Vendor         string `json:"vendor,omitempty"`
	Sockets        int    `json:"sockets,omitempty"`
	TotalCores     int    `json:"totalCores,omitempty"`
	CoresPerSocket int    `json:"coresPerSocket,omitempty"`
	DriverFamily   string `json:"driverFamily,omitempty"`
	// CapRange is the RAPL/MSR power cap range, per socket.
	CapRange           *CPUCapRange  `json:"capRange,omitempty"`
	Landmarks          *CPULandmarks `json:"landmarks,omitempty"`
	ControlAvailable   bool          `json:"controlAvailable,omitempty"`
	TelemetryAvailable bool          `json:"telemetryAvailable,omitempty"`
	Warnings           []string      `json:"warnings,omitempty"`
	// Governor is the CPU frequency governor state the agent discovered.
	Governor *CPUGovernorInventory `json:"governor,omitempty"`
}

// CPUGovernorInventory is the cpufreq governor state of the node, read from
// sysfs without writing anything, and the presets the node accepts.
type CPUGovernorInventory struct {
	// Driver is scaling_driver, such as acpi-cpufreq, intel_pstate or
	// amd-pstate-epp.
	Driver string `json:"driver,omitempty"`
	// DriverMode is active, passive or guided, from intel_pstate/status or
	// amd_pstate/status; empty for a driver without modes.
	DriverMode string `json:"driverMode,omitempty"`
	// Current is scaling_governor when every policy has the same one, else
	// mixed.
	Current string `json:"current,omitempty"`
	// Available is scaling_available_governors.
	Available []string `json:"available,omitempty"`
	// EPP is the energy-performance preference, when the driver has one.
	EPP *CPUEPPInventory `json:"epp,omitempty"`
	// Boost is the frequency boost (turbo) state, when the driver has one.
	Boost *CPUBoostInventory `json:"boost,omitempty"`
	// Presets lists every preset the node accepts with the result it gives;
	// a preset the node cannot express is left out.
	Presets []GovernorPresetSupport `json:"presets,omitempty"`
	// Baseline is the state found before the agent's first write, which the
	// agent restores.
	Baseline *CPUGovernorBaseline `json:"baseline,omitempty"`
	// Writable is true when the agent can write the governor files.
	// +optional
	Writable bool `json:"writable"`
}

// CPUEPPInventory is the energy-performance preference of the policies.
type CPUEPPInventory struct {
	// Current is energy_performance_preference.
	Current string `json:"current,omitempty"`
	// Available is the preferences the policies accept under some governor:
	// energy_performance_available_preferences, except on amd-pstate-epp
	// under the performance governor, where that file lists only performance
	// and the driver's own set is published instead.
	Available []string `json:"available,omitempty"`
}

// CPUBoostInventory is the boost state and where it is set: global
// (cpufreq/boost), per-cpu (each policy's boost) or no-turbo (the inverted
// intel_pstate/no_turbo).
type CPUBoostInventory struct {
	// +kubebuilder:validation:Enum=on;off
	State string `json:"state,omitempty"`
	// +kubebuilder:validation:Enum=global;per-cpu;no-turbo
	Scope string `json:"scope,omitempty"`
}

// GovernorPresetSupport is one preset a node accepts. Result is applied when
// the preset changes the hardware state, equivalent when it maps to a state
// the node already expresses another way; Native is the native setting it
// maps to, in the driver's own words.
type GovernorPresetSupport struct {
	Preset string `json:"preset"`
	// +kubebuilder:validation:Enum=applied;equivalent
	Result string `json:"result,omitempty"`
	Native string `json:"native,omitempty"`
}

// CPUGovernorBaseline is the CPU governor state found before the agent's
// first write, one entry per group of policies that share it.
type CPUGovernorBaseline struct {
	Groups []CPUGovernorState `json:"groups,omitempty"`
	// RecordedAt is the RFC3339 time the baseline was recorded.
	RecordedAt string `json:"recordedAt,omitempty"`
}

// CPUGovernorState is the governor state of a group of cpufreq policies.
type CPUGovernorState struct {
	// Policies lists the policy numbers in the kernel's list form, such as
	// 0-63 or 0,2,4.
	Policies string `json:"policies,omitempty"`
	Governor string `json:"governor,omitempty"`
	EPP      string `json:"epp,omitempty"`
	// +kubebuilder:validation:Enum=on;off
	Boost string `json:"boost,omitempty"`
}

// CPUCapRange is the settable package power cap range of one socket.
type CPUCapRange struct {
	// Type is the cap domain, currently always "package".
	Type              string  `json:"type,omitempty"`
	MinWattsPerSocket float64 `json:"minWattsPerSocket,omitempty"`
	MaxWattsPerSocket float64 `json:"maxWattsPerSocket,omitempty"`
}

// CPULandmarks holds the characteristic frequencies of the CPU.
type CPULandmarks struct {
	MinFreqMHz             float64 `json:"minFreqMHz,omitempty"`
	NominalFreqMHz         float64 `json:"nominalFreqMHz,omitempty"`
	MaxBoostMHz            float64 `json:"maxBoostMHz,omitempty"`
	LowestNonlinearFreqMHz float64 `json:"lowestNonlinearFreqMHz,omitempty"`
}

// NodeHardwareGPU describes the GPUs of the node.
type NodeHardwareGPU struct {
	Present  bool   `json:"present,omitempty"`
	RawModel string `json:"rawModel,omitempty"`
	Model    string `json:"model,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Count    int    `json:"count,omitempty"`
	// CapRangePerGPU is the power cap range of one GPU, not the sum over all GPUs.
	CapRangePerGPU     *GPUCapRange `json:"capRangePerGpu,omitempty"`
	CurrentCapWatts    float64      `json:"currentCapWatts,omitempty"`
	Slicing            *GPUSlicing  `json:"slicing,omitempty"`
	ControlAvailable   bool         `json:"controlAvailable,omitempty"`
	TelemetryAvailable bool         `json:"telemetryAvailable,omitempty"`
	Warnings           []string     `json:"warnings,omitempty"`
	// Governor is the GPU clock governor state the agent discovered.
	Governor *GPUGovernorInventory `json:"governor,omitempty"`
}

// GPUGovernorInventory is the GPU clock governor state of the node and the
// presets its GPUs accept.
type GPUGovernorInventory struct {
	// Mechanism is how the agent reads and sets the governor: amdgpu-sysfs
	// or nvidia-smi.
	// +kubebuilder:validation:Enum=amdgpu-sysfs;nvidia-smi
	Mechanism string `json:"mechanism,omitempty"`
	// Current is the native state: the amdgpu performance level, or on
	// NVIDIA the agent's own record of its locks, unknown without one (a
	// clock lock cannot be read back).
	Current string `json:"current,omitempty"`
	// Presets lists every preset the GPUs accept with the result it gives;
	// a preset the family cannot express is left out.
	Presets []GovernorPresetSupport `json:"presets,omitempty"`
	// ClockRangesMHz is the minimum and maximum of each clock domain.
	ClockRangesMHz *GPUClockRanges `json:"clockRangesMHz,omitempty"`
	// Baseline is the state found before the agent's first write, which the
	// agent restores.
	Baseline *GPUGovernorBaseline `json:"baseline,omitempty"`
	// Writable is true when the agent can set the governor.
	// +optional
	Writable bool `json:"writable"`
	// Uniform is true when every GPU of the node is in the same state.
	// +optional
	Uniform bool `json:"uniform"`
}

// GPUClockRanges holds the clock range of each GPU clock domain: graphics
// (the compute clock), memory, and fabric (the data fabric, AMD only).
type GPUClockRanges struct {
	Graphics *ClockRangeMHz `json:"graphics,omitempty"`
	Memory   *ClockRangeMHz `json:"memory,omitempty"`
	Fabric   *ClockRangeMHz `json:"fabric,omitempty"`
}

// ClockRangeMHz is the lowest and highest clock of a domain, in MHz.
type ClockRangeMHz struct {
	Min float64 `json:"min,omitempty"`
	Max float64 `json:"max,omitempty"`
}

// GPUGovernorBaseline is the GPU governor state found before the agent's
// first write, per GPU.
type GPUGovernorBaseline struct {
	Devices []GPUGovernorDeviceState `json:"devices,omitempty"`
	// RecordedAt is the RFC3339 time the baseline was recorded.
	RecordedAt string `json:"recordedAt,omitempty"`
}

// GPUGovernorDeviceState is the governor state of one GPU, keyed by its PCI
// bus id (domain:bus:device.function), never by a tool's index, since tools
// enumerate GPUs in different orders.
type GPUGovernorDeviceState struct {
	BusID string `json:"busId"`
	// Level is the amdgpu performance level, or unknown on NVIDIA, where a
	// clock lock cannot be read back.
	Level string `json:"level,omitempty"`
}

// GPUCapRange is the settable power cap range of one GPU.
type GPUCapRange struct {
	MinWatts     float64 `json:"minWatts,omitempty"`
	MaxWatts     float64 `json:"maxWatts,omitempty"`
	DefaultWatts float64 `json:"defaultWatts,omitempty"`
}

// GPUSlicing lists the partitioning modes the GPUs support.
type GPUSlicing struct {
	Supported bool     `json:"supported,omitempty"`
	Modes     []string `json:"modes,omitempty"`
}

// NodeHardwareMemory describes the system memory of the node.
type NodeHardwareMemory struct {
	TotalBytes int64 `json:"totalBytes,omitempty"`
}

// NodeHardwareNetwork describes the network attachment of the node.
type NodeHardwareNetwork struct {
	LinkClass string `json:"linkClass,omitempty"`
}

// InventoryResolution records how confidently the detected hardware was
// matched against the hardware catalog.
type InventoryResolution struct {
	HardwareCatalogKey *CatalogKey       `json:"hardwareCatalogKey,omitempty"`
	Exactness          *CatalogExactness `json:"exactness,omitempty"`
}

// CatalogKey is the catalog entry the CPU and GPU resolved to.
type CatalogKey struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

// CatalogExactness is the match quality per component: "exact", "proxy" or
// "generic".
type CatalogExactness struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

// NodeHardwareCapabilities summarises which control and telemetry paths the
// agent found usable on the node.
type NodeHardwareCapabilities struct {
	CPUControl   bool `json:"cpuControl,omitempty"`
	GPUControl   bool `json:"gpuControl,omitempty"`
	CPUTelemetry bool `json:"cpuTelemetry,omitempty"`
	GPUTelemetry bool `json:"gpuTelemetry,omitempty"`
}

// NodeHardwareQuality rates the inventory as a whole: "full", "partial" or
// "unknown", with the warnings that lowered it.
type NodeHardwareQuality struct {
	Overall  string   `json:"overall,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nhw
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=".status.cpu.model"
// +kubebuilder:printcolumn:name="GPU",type=string,JSONPath=".status.gpu.model"
// +kubebuilder:printcolumn:name="GPUCount",type=integer,JSONPath=".status.gpu.count"
// +kubebuilder:printcolumn:name="Quality",type=string,JSONPath=".status.quality.overall"

// NodeHardware holds the hardware capabilities of a node, as published by
// the agent. The spec names the node; the status carries the inventory.
type NodeHardware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeHardwareSpec   `json:"spec"`
	Status NodeHardwareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeHardwareList contains a list of NodeHardware.
type NodeHardwareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeHardware `json:"items"`
}
