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

// NodeTwinState is the digital-twin output for one node.
// Computed every ~1 min by the operator from NodeHardware + NodePowerProfile + WorkloadProfiles.
// Read by the scheduler extender to make placement decisions.
type NodeTwinState struct {
	NodeName string `json:"nodeName,omitempty"`
	// SchedulableClass: "eco", "performance", "draining", "unknown".
	//
	// draining = node is transitioning from performance to eco:
	//   it has been set to eco profile but still has performance pods.
	//   The operator tries to reschedule those pods to performance nodes.
	//   New pods should avoid draining nodes (scored down, not hard-filtered).
	SchedulableClass string `json:"schedulableClass,omitempty"`
	// PredictedPowerHeadroomScore: 0-100. Available power budget on this node.
	// Higher = more room to place new workloads without stressing cooling/PSU.
	PredictedPowerHeadroomScore float64 `json:"predictedPowerHeadroomScore,omitempty"`
	// PredictedCoolingStressScore: 0-100. Fraction of cooling capacity predicted to be used.
	// High = risk of thermal throttling; operator triggers migration of reschedulable pods.
	PredictedCoolingStressScore float64 `json:"predictedCoolingStressScore,omitempty"`
	// PredictedPsuStressScore: 0-100. Fraction of PDU/PSU capacity predicted to be used.
	// High = risk of power brownout; operator reduces caps or triggers migration.
	PredictedPsuStressScore     float64                    `json:"predictedPsuStressScore,omitempty"`
	EffectiveCapState           CapState                   `json:"effectiveCapState,omitempty"`
	HardwareDensityScore        float64                    `json:"hardwareDensityScore,omitempty"`
	RescheduleRecommendations   []RescheduleRecommendation `json:"rescheduleRecommendations,omitempty"`
	LastUpdated                 time.Time                  `json:"lastUpdated,omitempty"`
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
