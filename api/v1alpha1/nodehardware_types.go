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
