package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeTwinSpec is the desired power state of a node. The operator writes it
// from the policy decision; the agent applies it to the hardware.
type NodeTwinSpec struct {
	NodeName string `json:"nodeName"`
	// +kubebuilder:validation:Enum=performance;eco
	Profile    string              `json:"profile"`
	CPU        *NodeTwinCPU        `json:"cpu,omitempty"`
	GPU        *NodeTwinGPU        `json:"gpu,omitempty"`
	Policy     *NodeTwinPolicy     `json:"policy,omitempty"`
	Scheduling *NodeTwinScheduling `json:"scheduling,omitempty"`
}

// NodeTwinCPU is the CPU package power cap, as an absolute value or as a
// fraction of the hardware maximum. At most one is set.
type NodeTwinCPU struct {
	// +kubebuilder:validation:Minimum=1
	PackagePowerCapWatts *float64 `json:"packagePowerCapWatts,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=100
	PackagePowerCapPctOfMax *float64 `json:"packagePowerCapPctOfMax,omitempty"`
}

// NodeTwinGPU holds the GPU power controls.
type NodeTwinGPU struct {
	PowerCap *GPUPowerCap `json:"powerCap,omitempty"`
}

// GPUPowerCap is the GPU power cap, as an absolute value per GPU or as a
// fraction of the hardware maximum.
type GPUPowerCap struct {
	// +kubebuilder:validation:Enum=perGpu
	Scope string `json:"scope,omitempty"`
	// +kubebuilder:validation:Minimum=1
	CapWattsPerGPU *float64 `json:"capWattsPerGpu,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=100
	CapPctOfMax *float64 `json:"capPctOfMax,omitempty"`
}

// NodeTwinPolicy names the policy that produced this spec.
type NodeTwinPolicy struct {
	Name string `json:"name,omitempty"`
}

// NodeTwinScheduling carries scheduling hints derived from the policy.
type NodeTwinScheduling struct {
	Draining bool `json:"draining,omitempty"`
}

// NodeTwinStatus is the digital twin output for one node, computed by the
// operator from NodeHardware and NodeTwinSpec and read by the scheduler
// extender. controlStatus is the exception: the agent writes it.
type NodeTwinStatus struct {
	// +kubebuilder:validation:Enum=eco;performance;draining;unknown
	SchedulableClass string `json:"schedulableClass,omitempty"`
	// Measured power data, TDP/cap breakdowns, and power trend.
	PowerMeasurement *PowerMeasurement `json:"powerMeasurement,omitempty"`
	// 0=no headroom, 100=full headroom
	PredictedPowerHeadroomScore float64 `json:"predictedPowerHeadroomScore,omitempty"`
	// 0=no stress, 100=max stress
	PredictedCoolingStressScore float64 `json:"predictedCoolingStressScore,omitempty"`
	// Reserved for future rack-topology-aware extensions. Not used in scoring. 0=no stress, 100=max stress
	PredictedPsuStressScore float64   `json:"predictedPsuStressScore,omitempty"`
	EffectiveCapState       *CapState `json:"effectiveCapState,omitempty"`
	// Relative compute density score
	HardwareDensityScore float64 `json:"hardwareDensityScore,omitempty"`
	// Reserved for future rack-topology-aware extensions. Not used in scoring. Estimated Power Usage Effectiveness (1.05-1.40)
	EstimatedPUE float64 `json:"estimatedPUE,omitempty"`
	// Control feedback from the agent
	ControlStatus *ControlStatus `json:"controlStatus,omitempty"`
	// LastUpdated is the RFC3339 time of the last operator write.
	LastUpdated string `json:"lastUpdated,omitempty"`
}

// PowerMeasurement holds the measured power data and TDP/cap breakdowns.
type PowerMeasurement struct {
	// The enum must accept every value in joulie.PowerSourceValues
	// (pkg/api/types.go); tests/contracts enforces it.

	// Measurement source: kepler, utilization, or static
	// +kubebuilder:validation:Enum=kepler;utilization;static;http;http-error;http-no-endpoint;prometheus;prometheus-no-data
	Source string `json:"source,omitempty"`
	// Measured total node power draw in watts
	MeasuredNodePowerW float64 `json:"measuredNodePowerW,omitempty"`
	// CPU power budget based on TDP and cap percentage
	CPUCappedPowerW float64 `json:"cpuCappedPowerW,omitempty"`
	// GPU power budget based on TDP and cap percentage
	GPUCappedPowerW float64 `json:"gpuCappedPowerW,omitempty"`
	// Total node power budget (cpuCappedPowerW + gpuCappedPowerW)
	NodeCappedPowerW float64 `json:"nodeCappedPowerW,omitempty"`
	// CPU thermal design power (uncapped hardware maximum)
	CPUTdpW float64 `json:"cpuTdpW,omitempty"`
	// GPU thermal design power (uncapped hardware maximum)
	GPUTdpW float64 `json:"gpuTdpW,omitempty"`
	// Total node TDP (cpuTdpW + gpuTdpW)
	NodeTdpW float64 `json:"nodeTdpW,omitempty"`
	// Rate of change of measured power in watts per minute
	PowerTrendWPerMin float64 `json:"powerTrendWPerMin,omitempty"`
}

// CapState is the cap the operator believes is in effect, as a percentage
// of the hardware maximum.
type CapState struct {
	CPUPct float64 `json:"cpuPct,omitempty"`
	GPUPct float64 `json:"gpuPct,omitempty"`
}

// ControlStatus holds the agent's feedback on the applied controls.
type ControlStatus struct {
	CPU *ControlResult `json:"cpu,omitempty"`
	GPU *ControlResult `json:"gpu,omitempty"`
}

// ControlResult is the outcome of the last control write for one component.
type ControlResult struct {
	Backend string `json:"backend,omitempty"`
	Result  string `json:"result,omitempty"`
	Message string `json:"message,omitempty"`
	// UpdatedAt is the RFC3339 time of the last agent write.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nt
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=".spec.profile"
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=".status.schedulableClass"
// +kubebuilder:printcolumn:name="PowerHeadroom",type=number,JSONPath=".status.predictedPowerHeadroomScore"
// +kubebuilder:printcolumn:name="CoolingStress",type=number,JSONPath=".status.predictedCoolingStressScore"
// +kubebuilder:printcolumn:name="PSUStress",type=number,JSONPath=".status.predictedPsuStressScore"
// +kubebuilder:printcolumn:name="CPU-Cap",type=string,JSONPath=".spec.cpu.packagePowerCapPctOfMax"
// +kubebuilder:printcolumn:name="Draining",type=boolean,JSONPath=".spec.scheduling.draining"

// NodeTwin combines a node's desired power state and its computed twin
// output. spec is written by the operator, status by the operator's twin
// model plus the agent's control feedback.
type NodeTwin struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Desired power state for this node, written by the operator.
	Spec NodeTwinSpec `json:"spec,omitempty"`
	// Computed twin state, written by the operator's digital twin model.
	Status NodeTwinStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeTwinList contains a list of NodeTwin.
type NodeTwinList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeTwin `json:"items"`
}
