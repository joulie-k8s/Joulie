package api

import "time"

// NodeHardware holds hardware capabilities of a node, as published by the agent.
// It is the single source of truth for static node hardware facts:
// CPU/GPU specs, power cap ranges, frequency landmarks, GPU slicing modes,
// memory, network, and inventory resolution confidence.
type NodeHardware struct {
	NodeName    string              `json:"nodeName"`
	CPU         NodeHardwareCPU     `json:"cpu,omitempty"`
	GPU         NodeHardwareGPU     `json:"gpu,omitempty"`
	Memory      NodeHardwareMemory  `json:"memory,omitempty"`
	Network     NodeHardwareNetwork `json:"network,omitempty"`
	Inventory   InventoryResolution `json:"inventoryResolution,omitempty"`
	Quality     NodeHardwareQuality `json:"quality,omitempty"`
	LastUpdated time.Time           `json:"lastUpdated,omitempty"`
}

type NodeHardwareCPU struct {
	Vendor         string       `json:"vendor,omitempty"`
	RawModel       string       `json:"rawModel,omitempty"`
	Model          string       `json:"model,omitempty"`
	Sockets        int          `json:"sockets,omitempty"`
	CoresPerSocket int          `json:"coresPerSocket,omitempty"`
	TotalCores     int          `json:"totalCores,omitempty"`
	DriverFamily   string       `json:"driverFamily,omitempty"`
	// CapRange is the RAPL/MSR power cap range (per socket).
	CapRange           CPUCapRange  `json:"capRange,omitempty"`
	Landmarks          CPULandmarks `json:"landmarks,omitempty"`
	ControlAvailable   bool         `json:"controlAvailable,omitempty"`
	TelemetryAvailable bool         `json:"telemetryAvailable,omitempty"`
	Warnings           []string     `json:"warnings,omitempty"`
}

type CPULandmarks struct {
	MinFreqMHz             float64 `json:"minFreqMHz,omitempty"`
	NominalFreqMHz         float64 `json:"nominalFreqMHz,omitempty"`
	MaxBoostMHz            float64 `json:"maxBoostMHz,omitempty"`
	LowestNonlinearFreqMHz float64 `json:"lowestNonlinearFreqMHz,omitempty"`
}

type CPUCapRange struct {
	Type              string  `json:"type,omitempty"` // "package"
	MinWattsPerSocket float64 `json:"minWattsPerSocket,omitempty"`
	MaxWattsPerSocket float64 `json:"maxWattsPerSocket,omitempty"`
}

type NodeHardwareGPU struct {
	Present  bool       `json:"present,omitempty"`
	Vendor   string     `json:"vendor,omitempty"`
	RawModel string     `json:"rawModel,omitempty"`
	Model    string     `json:"model,omitempty"`
	Count    int        `json:"count,omitempty"`
	// CapRange is per-GPU power cap range (not total for all GPUs).
	CapRange           GPUCapRange `json:"capRangePerGpu,omitempty"`
	CurrentCapWatts    float64     `json:"currentCapWatts,omitempty"`
	Slicing            GPUSlicing  `json:"slicing,omitempty"`
	ControlAvailable   bool        `json:"controlAvailable,omitempty"`
	TelemetryAvailable bool        `json:"telemetryAvailable,omitempty"`
	Warnings           []string    `json:"warnings,omitempty"`
}

type GPUCapRange struct {
	MinWatts     float64 `json:"minWatts,omitempty"`
	MaxWatts     float64 `json:"maxWatts,omitempty"`
	DefaultWatts float64 `json:"defaultWatts,omitempty"`
}

type GPUSlicing struct {
	Supported bool     `json:"supported,omitempty"`
	Modes     []string `json:"modes,omitempty"`
}

type NodeHardwareMemory struct {
	TotalBytes int64 `json:"totalBytes,omitempty"`
}

type NodeHardwareNetwork struct {
	LinkClass string `json:"linkClass,omitempty"`
}

type NodeHardwareQuality struct {
	Overall  string   `json:"overall,omitempty"` // "full", "partial", "unknown"
	Warnings []string `json:"warnings,omitempty"`
}

type InventoryResolution struct {
	HardwareCatalogKey CatalogKey       `json:"hardwareCatalogKey,omitempty"`
	Exactness          CatalogExactness `json:"exactness,omitempty"`
}

type CatalogKey struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

type CatalogExactness struct {
	CPU string `json:"cpu,omitempty"` // "exact", "proxy", "generic"
	GPU string `json:"gpu,omitempty"`
}

// WorkloadProfileStatus mirrors the joulie.io/v1alpha1 WorkloadProfile CRD status.
type WorkloadProfileStatus struct {
	Criticality           WorkloadCriticality   `json:"criticality,omitempty"`
	Migratability         WorkloadMigratability `json:"migratability,omitempty"`
	CPU                   WorkloadCPUProfile    `json:"cpu,omitempty"`
	GPU                   WorkloadGPUProfile    `json:"gpu,omitempty"`
	RescheduleRecommended bool                  `json:"rescheduleRecommended,omitempty"`
	RescheduleReason      string                `json:"rescheduleReason,omitempty"`
	ClassificationReason  string                `json:"classificationReason,omitempty"`
	Confidence            float64               `json:"confidence,omitempty"`
	LastUpdated           time.Time             `json:"lastUpdated,omitempty"`
}

type WorkloadCriticality struct {
	// Class is the QoS class for power-aware scheduling.
	//
	// performance: must run on uncapped (performance) nodes.
	//   The scheduler extender rejects eco nodes for these pods.
	// standard: prefers performance nodes, tolerates eco with some slowdown.
	// best-effort: fine on eco nodes; scheduler gives eco nodes a slight bonus
	//   to preserve performance capacity for critical work.
	Class string `json:"class,omitempty"`
}

type WorkloadMigratability struct {
	// Reschedulable: pod can be safely restarted on another node.
	// Set via joulie.io/reschedulable=true pod annotation.
	// Used by the operator's migration controller under thermal/PSU pressure.
	Reschedulable bool `json:"reschedulable,omitempty"`
}

type WorkloadCPUProfile struct {
	Intensity         string  `json:"intensity,omitempty"`      // "high", "medium", "low"
	Bound             string  `json:"bound,omitempty"`          // "compute", "memory", "io", "mixed"
	AvgUtilizationPct float64 `json:"avgUtilizationPct,omitempty"`
	CapSensitivity    string  `json:"capSensitivity,omitempty"` // "high", "medium", "low"
}

type WorkloadGPUProfile struct {
	Intensity         string  `json:"intensity,omitempty"`
	Bound             string  `json:"bound,omitempty"`          // "compute", "memory", "mixed", "none"
	AvgUtilizationPct float64 `json:"avgUtilizationPct,omitempty"`
	CapSensitivity    string  `json:"capSensitivity,omitempty"`
}

// NodeTwinSpec is the desired power state for a node, written by the operator.
// This is the spec portion of the NodeTwin CRD.
type NodeTwinSpec struct {
	NodeName    string       `json:"nodeName"`
	Profile     string       `json:"profile"`               // "performance" or "eco"
	CPU         *NodeTwinCPU `json:"cpu,omitempty"`
	GPU         *NodeTwinGPU `json:"gpu,omitempty"`
	PolicyName  string       `json:"policyName,omitempty"`
	Draining    bool         `json:"draining,omitempty"`
}

type NodeTwinCPU struct {
	PackagePowerCapWatts    *float64 `json:"packagePowerCapWatts,omitempty"`
	PackagePowerCapPctOfMax *float64 `json:"packagePowerCapPctOfMax,omitempty"`
}

type NodeTwinGPU struct {
	Scope          string   `json:"scope,omitempty"` // "perGpu"
	CapWattsPerGPU *float64 `json:"capWattsPerGpu,omitempty"`
	CapPctOfMax    *float64 `json:"capPctOfMax,omitempty"`
}

// NodeTwinStatus is the digital-twin output for one node.
// Computed by the operator from NodeHardware + NodeTwinSpec + WorkloadProfiles.
// Read by the scheduler extender to make placement decisions.
type NodeTwinStatus struct {
	// SchedulableClass: "eco", "performance", "draining", "unknown".
	SchedulableClass string `json:"schedulableClass,omitempty"`
	// PredictedPowerHeadroomScore: 0-100. Available power budget on this node.
	PredictedPowerHeadroomScore float64 `json:"predictedPowerHeadroomScore,omitempty"`
	// PredictedCoolingStressScore: 0-100. Fraction of cooling capacity predicted to be used.
	PredictedCoolingStressScore float64 `json:"predictedCoolingStressScore,omitempty"`
	// PredictedPsuStressScore: 0-100. Fraction of PDU/PSU capacity predicted to be used.
	PredictedPsuStressScore     float64                    `json:"predictedPsuStressScore,omitempty"`
	EffectiveCapState           CapState                   `json:"effectiveCapState,omitempty"`
	HardwareDensityScore        float64                    `json:"hardwareDensityScore,omitempty"`
	RescheduleRecommendations   []RescheduleRecommendation `json:"rescheduleRecommendations,omitempty"`
	GPUSlicingRecommendation    *GPUSlicingRecommendation  `json:"gpuSlicingRecommendation,omitempty"`
	ControlStatus               *ControlStatus             `json:"controlStatus,omitempty"`
	LastUpdated                 time.Time                  `json:"lastUpdated,omitempty"`
}

// ControlStatus holds agent feedback on applied controls.
type ControlStatus struct {
	CPU *ControlResult `json:"cpu,omitempty"`
	GPU *ControlResult `json:"gpu,omitempty"`
}

type ControlResult struct {
	Backend   string `json:"backend,omitempty"`
	Result    string `json:"result,omitempty"`
	Message   string `json:"message,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// NodeTwin is the combined CRD for a node's desired power state and computed twin output.
// spec = desired state (operator writes), status = twin output + control feedback.
type NodeTwin struct {
	Spec   NodeTwinSpec   `json:"spec"`
	Status NodeTwinStatus `json:"status,omitempty"`
}

// GPUSlicingRecommendation is a suggestion from the digital twin to the
// cluster admin about how to configure GPU slicing (MIG or time-slicing)
// on a node for optimal power efficiency and resource utilization.
//
// The twin analyzes historical WorkloadProfile data to determine the
// dominant GPU usage pattern and recommends a slicing configuration that
// best matches that pattern. The admin reviews the recommendation and
// applies it manually (MIG reconfiguration requires GPU reset / pod eviction).
//
// This is advisory only. The operator never changes GPU slicing at runtime.
type GPUSlicingRecommendation struct {
	// Mode: "mig", "time-slicing", or "none" (whole GPU).
	Mode string `json:"mode,omitempty"`
	// SliceType: for MIG, the recommended profile (e.g. "3g.40gb", "1g.10gb").
	// For time-slicing, empty (k8s handles partitioning).
	SliceType string `json:"sliceType,omitempty"`
	// SlicesPerGPU: how many slices per physical GPU.
	SlicesPerGPU int `json:"slicesPerGPU,omitempty"`
	// TotalSlices: slicesPerGPU * GPU count on this node.
	TotalSlices int `json:"totalSlices,omitempty"`
	// Reason: human-readable explanation of why this config was chosen.
	Reason string `json:"reason,omitempty"`
	// EstimatedUtilizationGain: predicted improvement in GPU utilization (0-100 pct points).
	EstimatedUtilizationGain float64 `json:"estimatedUtilizationGain,omitempty"`
	// Confidence: 0-1 confidence in the recommendation based on data quality.
	Confidence float64 `json:"confidence,omitempty"`
}

type CapState struct {
	CPUPct float64 `json:"cpuPct,omitempty"`
	GPUPct float64 `json:"gpuPct,omitempty"`
}

type RescheduleRecommendation struct {
	WorkloadRef WorkloadRef `json:"workloadRef,omitempty"`
	Reason      string      `json:"reason,omitempty"`
}

type WorkloadRef struct {
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}
