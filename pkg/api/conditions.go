package api

import "time"

// StatusRewriteInterval is how often a writer rewrites a status that has not
// changed: the agent skips an unchanged NodeHardware or controlStatus write
// for at most this long, which bounds how long a lost object stays
// unrepaired.
const StatusRewriteInterval = 5 * time.Minute

// The NodeTwin condition the controller manager writes about the node's
// agent, in status.conditions, and its reasons.
const (
	// ConditionAgentReporting is True while an agent publishes the node's
	// NodeHardware. False means the caps in spec are planned and nothing
	// enforces them.
	ConditionAgentReporting = "AgentReporting"
	// ReasonNodeHardwareCurrent: the NodeHardware was written within
	// AgentReportingStaleAfter.
	ReasonNodeHardwareCurrent = "NodeHardwareCurrent"
	// ReasonNoNodeHardware: the node has no NodeHardware, so no agent has
	// run there.
	ReasonNoNodeHardware = "NoNodeHardware"
	// ReasonNodeHardwareStale: the last NodeHardware write is older than
	// AgentReportingStaleAfter, so the agent that wrote it stopped.
	ReasonNodeHardwareStale = "NodeHardwareStale"
)

// AgentReportingStaleAfter is the age of a NodeHardware write past which no
// agent counts as running on the node. A running agent rewrites it at least
// every StatusRewriteInterval plus one reconcile interval.
const AgentReportingStaleAfter = 3 * StatusRewriteInterval

// The NodeTwin conditions that record why the controller manager decided
// what it did for a node, one per decision, in status.conditions next to
// AgentReporting. Each is True while its decision is in force: the node
// runs eco, its CPU or GPU takes the eco cap, a governor preset is written
// in spec. The reason names the branch that decided it, the message says it
// in a sentence.
const (
	// ConditionEcoProfile is True while the node runs the eco profile with
	// eco caps, and False while it runs performance or drains to eco.
	ConditionEcoProfile = "EcoProfile"
	// ConditionCPUEcoCap is True while the node's CPU takes the eco cap,
	// and False while it takes the performance cap.
	ConditionCPUEcoCap = "CPUEcoCap"
	// ConditionGPUEcoCap is True while the node's GPUs take an eco cap that
	// applies, and False otherwise, a node without GPUs included.
	ConditionGPUEcoCap = "GPUEcoCap"
	// ConditionCPUGovernorPreset is True while spec.cpu.governor holds a
	// preset.
	ConditionCPUGovernorPreset = "CPUGovernorPreset"
	// ConditionGPUGovernorPreset is True while spec.gpu.governor holds a
	// preset.
	ConditionGPUGovernorPreset = "GPUGovernorPreset"
)

// Reasons of the profile decision, ConditionEcoProfile.
const (
	// ReasonPerformanceSlot: the policy keeps the node in one of the
	// performance slots of its hardware family.
	ReasonPerformanceSlot = "PerformanceSlot"
	// ReasonPerformanceDemand: queue_aware_v1 keeps more nodes in
	// performance than its base fraction, QUEUE_HP_MIN and the one slot of
	// each hardware family would, for the performance pods that run or
	// wait, and this node is one of them.
	ReasonPerformanceDemand = "PerformanceDemand"
	// ReasonNoPerformanceSlot: other nodes of its hardware family hold the
	// performance slots of static_partition.
	ReasonNoPerformanceSlot = "NoPerformanceSlot"
	// ReasonNoPerformanceDemand: no performance pod runs or waits, so
	// queue_aware_v1 keeps only its base fraction in performance.
	ReasonNoPerformanceDemand = "NoPerformanceDemand"
	// ReasonPerformanceDemandMet: the performance pods that run or wait fit
	// on the nodes queue_aware_v1 keeps in performance.
	ReasonPerformanceDemandMet = "PerformanceDemandMet"
	// ReasonPerformanceNodesAtMax: the performance pods need more
	// performance nodes than QUEUE_HP_MAX allows.
	ReasonPerformanceNodesAtMax = "PerformanceNodesAtMax"
	// ReasonRuleSwapPhase: rule_swap_v1 alternates eco between nodes on a
	// timer, and this is the node's turn in the current phase.
	ReasonRuleSwapPhase = "RuleSwapPhase"
	// ReasonDrainingPerformancePods: the policy planned eco, and
	// performance pods still run on the node, so it keeps the performance
	// caps and governors until they finish. Also a reason of the caps and
	// governors.
	ReasonDrainingPerformancePods = "DrainingPerformancePods"
)

// Reasons of the cap decisions, ConditionCPUEcoCap and ConditionGPUEcoCap.
const (
	// ReasonEcoProfile: the node runs eco, so the component takes the eco
	// cap. Also a reason of the governors: the eco preset is written.
	ReasonEcoProfile = "EcoProfile"
	// ReasonPerformanceProfile: the node runs performance, so the component
	// takes the performance cap. Also a reason of the governors: the
	// performance preset is written.
	ReasonPerformanceProfile = "PerformanceProfile"
	// ReasonGPUNodeCPUUncapped: a GPU node's CPU keeps the performance cap
	// in eco, since a CPU cap slows the GPU data feed by more than it saves.
	ReasonGPUNodeCPUUncapped = "GPUNodeCPUUncapped"
	// ReasonCPUEcoCapOnUncappableGPUNode: the node's GPUs cannot be capped
	// and CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES is on, so its CPU takes the
	// eco cap.
	ReasonCPUEcoCapOnUncappableGPUNode = "CPUEcoCapOnUncappableGPUNode"
	// ReasonNoGPU: the node has no GPU. Also a reason of the GPU governor.
	ReasonNoGPU = "NoGPU"
	// ReasonGPUCapNotConfigured: GPU_ECO_CAP_PCT_OF_MAX is 0 or below, so
	// no GPU cap is written.
	ReasonGPUCapNotConfigured = "GPUCapNotConfigured"
	// ReasonGPUCapBlocked: the agent reports that the node offers no way to
	// apply the GPU cap (status.controlStatus.gpu.result blocked).
	ReasonGPUCapBlocked = "GPUCapBlocked"
	// ReasonGPUCapFailed: the agent's last GPU cap write failed
	// (status.controlStatus.gpu.result error).
	ReasonGPUCapFailed = "GPUCapFailed"
	// ReasonGPUCapNotApplicable: the agent publishes the node's GPUs without
	// power control (NodeHardware status.gpu.controlAvailable false), so a
	// GPU cap cannot apply.
	ReasonGPUCapNotApplicable = "GPUCapNotApplicable"
)

// Reasons of the governor decisions, ConditionCPUGovernorPreset and
// ConditionGPUGovernorPreset, besides ReasonEcoProfile,
// ReasonPerformanceProfile, ReasonDrainingPerformancePods and ReasonNoGPU.
const (
	// ReasonModelPreset: GPU_GOVERNOR_MODEL_PRESETS_JSON sets the preset
	// for the node's GPU model, and it is written.
	ReasonModelPreset = "ModelPreset"
	// ReasonMultiGPUPodRunning: the sustained preset is written while a pod
	// using several GPUs runs (GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU).
	ReasonMultiGPUPodRunning = "MultiGPUPodRunning"
	// ReasonGovernorDiscoveryGap: the agent's last GPU governor discovery
	// listed nothing, so the configured preset is written and the agent
	// holds it until it can plan.
	ReasonGovernorDiscoveryGap = "GovernorDiscoveryGap"
	// ReasonGovernorControlOff: GOVERNOR_CONTROL leaves this component's
	// governor alone.
	ReasonGovernorControlOff = "GovernorControlOff"
	// ReasonGPUNodeCPUGovernorUnset: a GPU node's CPU governor is left
	// alone unless CPU_GOVERNOR_ON_GPU_NODES is on.
	ReasonGPUNodeCPUGovernorUnset = "GPUNodeCPUGovernorUnset"
	// ReasonNoPresetConfigured: no preset is configured for the profile the
	// node runs at.
	ReasonNoPresetConfigured = "NoPresetConfigured"
	// ReasonPresetNotListed: the node does not list the configured preset
	// in NodeHardware, so none is written.
	ReasonPresetNotListed = "PresetNotListed"
	// ReasonNoMultiGPUPod: the sustained preset is written only while a pod
	// using several GPUs runs (GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU), and
	// none runs.
	ReasonNoMultiGPUPod = "NoMultiGPUPod"
)

// DecisionConditionTypes lists the decision conditions in the order a
// reader shows them.
var DecisionConditionTypes = []string{
	ConditionEcoProfile,
	ConditionCPUEcoCap,
	ConditionGPUEcoCap,
	ConditionCPUGovernorPreset,
	ConditionGPUGovernorPreset,
}

// DecisionReasonValues lists, per decision condition, every reason the
// controller manager writes on it.
var DecisionReasonValues = map[string][]string{
	ConditionEcoProfile: {
		ReasonPerformanceSlot, ReasonPerformanceDemand, ReasonNoPerformanceSlot, ReasonNoPerformanceDemand,
		ReasonPerformanceDemandMet, ReasonPerformanceNodesAtMax, ReasonRuleSwapPhase, ReasonDrainingPerformancePods,
	},
	ConditionCPUEcoCap: {
		ReasonEcoProfile, ReasonCPUEcoCapOnUncappableGPUNode, ReasonPerformanceProfile,
		ReasonDrainingPerformancePods, ReasonGPUNodeCPUUncapped,
	},
	ConditionGPUEcoCap: {
		ReasonEcoProfile, ReasonPerformanceProfile, ReasonDrainingPerformancePods, ReasonNoGPU,
		ReasonGPUCapNotConfigured, ReasonGPUCapBlocked, ReasonGPUCapFailed, ReasonGPUCapNotApplicable,
	},
	ConditionCPUGovernorPreset: {
		ReasonPerformanceProfile, ReasonDrainingPerformancePods, ReasonEcoProfile,
		ReasonGovernorControlOff, ReasonGPUNodeCPUGovernorUnset, ReasonNoPresetConfigured, ReasonPresetNotListed,
	},
	ConditionGPUGovernorPreset: {
		ReasonPerformanceProfile, ReasonDrainingPerformancePods, ReasonEcoProfile, ReasonModelPreset,
		ReasonMultiGPUPodRunning, ReasonGovernorDiscoveryGap, ReasonGovernorControlOff, ReasonNoGPU,
		ReasonNoPresetConfigured, ReasonPresetNotListed, ReasonNoMultiGPUPod,
	},
}
