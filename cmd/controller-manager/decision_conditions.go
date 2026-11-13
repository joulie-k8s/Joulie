package main

// The decision conditions of a NodeTwin: why the node got its profile, its
// CPU and GPU caps and its governors, one condition per decision, from the
// reasons the plan carries (policy.NodeAssignment). They go out in the
// status write of the twin, next to AgentReporting.

import (
	"fmt"

	joulie "github.com/matbun/joulie/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// decisionMessages is the message of each decision condition per reason: a
// sentence without numbers that change between reconciles, so a message
// moves only with its decision. A message of a governor that is written
// names the preset (%q).
var decisionMessages = map[string]map[string]string{
	joulie.ConditionEcoProfile: {
		joulie.ReasonPerformanceSlot:         "the policy keeps this node in one of the performance slots of its hardware family",
		joulie.ReasonPerformanceDemand:       "the performance pods that run or wait need more performance nodes than the base fraction and QUEUE_HP_MIN keep, and this node is one of them",
		joulie.ReasonNoPerformanceSlot:       "other nodes of its hardware family hold the performance slots, so this node runs eco",
		joulie.ReasonNoPerformanceDemand:     "no performance pod runs or waits, so the policy keeps only its base fraction in performance and this node runs eco",
		joulie.ReasonPerformanceDemandMet:    "the performance pods that run or wait fit on the nodes kept in performance, so this node runs eco",
		joulie.ReasonPerformanceNodesAtMax:   "the performance pods need more performance nodes than QUEUE_HP_MAX allows, so this node runs eco",
		joulie.ReasonRuleSwapPhase:           "rule_swap_v1 alternates eco between nodes on a timer, and this phase sets this node's profile",
		joulie.ReasonDrainingPerformancePods: "the policy planned eco, but performance pods still run here, so the node keeps the performance caps and governors until they finish",
	},
	joulie.ConditionCPUEcoCap: {
		joulie.ReasonEcoProfile:                   "the node runs eco, so its CPU takes the eco cap",
		joulie.ReasonCPUEcoCapOnUncappableGPUNode: "the node's GPUs cannot be capped and CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES is on, so its CPU takes the eco cap",
		joulie.ReasonPerformanceProfile:           "the node runs performance, so its CPU takes the performance cap",
		joulie.ReasonDrainingPerformancePods:      "performance pods still run on the node, so its CPU keeps the performance cap until they finish",
		joulie.ReasonGPUNodeCPUUncapped:           "a GPU node's CPU keeps the performance cap in eco, since a CPU cap slows the GPU data feed by more than it saves",
	},
	joulie.ConditionGPUEcoCap: {
		joulie.ReasonEcoProfile:              "the node runs eco, so its GPUs take the eco cap",
		joulie.ReasonPerformanceProfile:      "the node runs performance, so its GPUs take the performance cap",
		joulie.ReasonDrainingPerformancePods: "performance pods still run on the node, so its GPUs keep the performance cap until they finish",
		joulie.ReasonNoGPU:                   "the node has no GPU",
		joulie.ReasonGPUCapNotConfigured:     "GPU_ECO_CAP_PCT_OF_MAX is 0 or below, so no GPU cap is written",
		joulie.ReasonGPUCapBlocked:           "the agent reports that the node offers no way to apply the GPU cap; see status.controlStatus.gpu",
		joulie.ReasonGPUCapFailed:            "the agent's last GPU cap write failed; see status.controlStatus.gpu",
		joulie.ReasonGPUCapNotApplicable:     "the agent publishes the node's GPUs without power control, so a GPU cap cannot apply",
	},
	joulie.ConditionCPUGovernorPreset: {
		joulie.ReasonPerformanceProfile:      "the node runs performance, so its CPU governor takes the performance preset %q",
		joulie.ReasonEcoProfile:              "the node runs eco, so its CPU governor takes the eco preset %q",
		joulie.ReasonDrainingPerformancePods: "performance pods still run on the node, so its CPU governor keeps the performance preset %q until they finish",
		joulie.ReasonGovernorControlOff:      "GOVERNOR_CONTROL leaves the CPU governor alone",
		joulie.ReasonGPUNodeCPUGovernorUnset: "a GPU node's CPU governor is left alone unless CPU_GOVERNOR_ON_GPU_NODES is on",
		joulie.ReasonNoPresetConfigured:      "no CPU governor preset is configured for the profile the node runs at",
		joulie.ReasonPresetNotListed:         "the node does not list the configured CPU governor preset in NodeHardware, so none is written",
	},
	joulie.ConditionGPUGovernorPreset: {
		joulie.ReasonPerformanceProfile:      "the node runs performance, so its GPU governor takes the performance preset %q",
		joulie.ReasonEcoProfile:              "the node runs eco, so its GPU governor takes the eco preset %q",
		joulie.ReasonDrainingPerformancePods: "performance pods still run on the node, so its GPU governor keeps the performance preset %q until they finish",
		joulie.ReasonModelPreset:             "GPU_GOVERNOR_MODEL_PRESETS_JSON sets the preset %q for the node's GPU model",
		joulie.ReasonMultiGPUPodRunning:      "a pod using several GPUs runs on the node, so its GPU governor holds the preset %q",
		joulie.ReasonGovernorDiscoveryGap:    "the agent's last GPU governor discovery listed no presets, so the preset %q is written and the agent holds it until it can plan",
		joulie.ReasonGovernorControlOff:      "GOVERNOR_CONTROL leaves the GPU governor alone",
		joulie.ReasonNoGPU:                   "the node has no GPU",
		joulie.ReasonNoPresetConfigured:      "no GPU governor preset is configured for the profile the node runs at",
		joulie.ReasonPresetNotListed:         "the node does not list the configured GPU governor preset in NodeHardware, so none is written",
		joulie.ReasonNoMultiGPUPod:           "the sustained preset is written only while a pod using several GPUs runs (GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU), and none runs",
	},
}

// decisionConditions are the decision conditions of assignment a, in
// joulie.DecisionConditionTypes order. gpuCapBlock is why a planned GPU cap
// does not apply (twin.GPUCapBlockReason), "" when it does; it replaces the
// GPU reason of an eco node. A decision without a reason, from a plan that
// set none, writes no condition, since the CRD requires one.
func decisionConditions(a NodeAssignment, gpuCapBlock string) []metav1.Condition {
	gpuReason := a.GPUCapReason
	if gpuReason == joulie.ReasonEcoProfile && gpuCapBlock != "" {
		gpuReason = gpuCapBlock
	}
	var cpuPreset, gpuPreset string
	if a.CPUGovernor != nil {
		cpuPreset = a.CPUGovernor.Preset
	}
	if a.GPUGovernor != nil {
		gpuPreset = a.GPUGovernor.Preset
	}
	eco := a.Profile == profileEco && !a.Draining
	cpuEco := a.CPUCapReason == joulie.ReasonEcoProfile || a.CPUCapReason == joulie.ReasonCPUEcoCapOnUncappableGPUNode
	decisions := []struct {
		condition, reason, preset string
		inForce                   bool
	}{
		{joulie.ConditionEcoProfile, a.ProfileReason, "", eco},
		{joulie.ConditionCPUEcoCap, a.CPUCapReason, "", cpuEco},
		{joulie.ConditionGPUEcoCap, gpuReason, "", gpuReason == joulie.ReasonEcoProfile},
		{joulie.ConditionCPUGovernorPreset, a.CPUGovernorReason, cpuPreset, a.CPUGovernor != nil},
		{joulie.ConditionGPUGovernorPreset, a.GPUGovernorReason, gpuPreset, a.GPUGovernor != nil},
	}
	out := make([]metav1.Condition, 0, len(decisions))
	for _, d := range decisions {
		if d.reason == "" {
			continue
		}
		status := metav1.ConditionFalse
		if d.inForce {
			status = metav1.ConditionTrue
		}
		msg := decisionMessages[d.condition][d.reason]
		if d.inForce && d.preset != "" {
			msg = fmt.Sprintf(msg, d.preset)
		}
		out = append(out, metav1.Condition{Type: d.condition, Status: status, Reason: d.reason, Message: msg})
	}
	return out
}
