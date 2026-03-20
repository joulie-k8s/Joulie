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

// PowerMeasurement holds the measured power data and TDP/cap breakdowns.
type PowerMeasurement struct {
	Source             string  `json:"source,omitempty"`             // "kepler", "utilization", "static"
	MeasuredNodePowerW float64 `json:"measuredNodePowerW,omitempty"`
	CpuCappedPowerW    float64 `json:"cpuCappedPowerW,omitempty"`
	GpuCappedPowerW    float64 `json:"gpuCappedPowerW,omitempty"`
	NodeCappedPowerW   float64 `json:"nodeCappedPowerW,omitempty"`
	CpuTdpW            float64 `json:"cpuTdpW,omitempty"`
	GpuTdpW            float64 `json:"gpuTdpW,omitempty"`
	NodeTdpW           float64 `json:"nodeTdpW,omitempty"`
	PowerTrendWPerMin  float64 `json:"powerTrendWPerMin,omitempty"`
}

// NodeTwinStatus is the digital-twin output for one node.
// Computed by the operator from NodeHardware + NodeTwinSpec.
// Read by the scheduler extender to make placement decisions.
type NodeTwinStatus struct {
	// SchedulableClass: "eco", "performance", "draining", "unknown".
	SchedulableClass string `json:"schedulableClass,omitempty"`
	// PowerMeasurement holds measured power data, TDP/cap breakdowns, and power trend.
	PowerMeasurement *PowerMeasurement `json:"powerMeasurement,omitempty"`
	// PredictedPowerHeadroomScore: 0-100. Available power budget on this node.
	PredictedPowerHeadroomScore float64 `json:"predictedPowerHeadroomScore,omitempty"`
	// PredictedCoolingStressScore: 0-100. Fraction of cooling capacity predicted to be used.
	PredictedCoolingStressScore float64 `json:"predictedCoolingStressScore,omitempty"`
	// PredictedPsuStressScore: 0-100. Fraction of PDU/PSU capacity predicted to be used.
	// Reserved for future rack-topology-aware extensions. Not used in scoring.
	PredictedPsuStressScore float64  `json:"predictedPsuStressScore,omitempty"`
	EffectiveCapState       CapState `json:"effectiveCapState,omitempty"`
	HardwareDensityScore    float64  `json:"hardwareDensityScore,omitempty"`
	// EstimatedPUE is the estimated Power Usage Effectiveness for this node.
	// PUE = 1.0 + overhead. Ranges from ~1.05 (idle, cool) to ~1.40 (stressed cooling).
	// Reserved for future rack-topology-aware extensions. Not used in scoring.
	EstimatedPUE    float64        `json:"estimatedPUE,omitempty"`
	ControlStatus   *ControlStatus `json:"controlStatus,omitempty"`
	LastUpdated     time.Time      `json:"lastUpdated,omitempty"`
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

type CapState struct {
	CPUPct float64 `json:"cpuPct,omitempty"`
	GPUPct float64 `json:"gpuPct,omitempty"`
}

