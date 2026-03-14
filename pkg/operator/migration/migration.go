// Package migration implements the migration/rescheduling recommendation logic.
// It identifies workloads that are candidates for rescheduling based on
// node stress levels and workload migratability.
package migration

import (
	"fmt"

	joulie "github.com/matbun/joulie/pkg/api"
)

// PolicyConfig controls migration recommendation behavior.
type PolicyConfig struct {
	// CoolingStressThreshold triggers rescheduling when cooling stress exceeds it.
	CoolingStressThreshold float64
	// PSUStressThreshold triggers rescheduling when PSU stress exceeds it.
	PSUStressThreshold float64
	// OnlyReschedulable: only recommend reschedulable workloads.
	OnlyReschedulable bool
	// OnlyBestEffort: only recommend best-effort workloads.
	OnlyBestEffort bool
}

// DefaultPolicy returns the default migration policy configuration.
func DefaultPolicy() PolicyConfig {
	return PolicyConfig{
		CoolingStressThreshold: 70,
		PSUStressThreshold:     70,
		OnlyReschedulable:      true,
		OnlyBestEffort:         false,
	}
}

// WorkloadOnNode pairs a workload reference with its profile.
type WorkloadOnNode struct {
	Ref     joulie.WorkloadRef
	Profile joulie.WorkloadProfileStatus
}

// EvaluateNode returns reschedule recommendations for workloads on the given node.
func EvaluateNode(twinState joulie.NodeTwinState, workloads []WorkloadOnNode, cfg PolicyConfig) []joulie.RescheduleRecommendation {
	var recs []joulie.RescheduleRecommendation

	highStress := twinState.PredictedCoolingStressScore > cfg.CoolingStressThreshold ||
		twinState.PredictedPsuStressScore > cfg.PSUStressThreshold

	if !highStress && twinState.SchedulableClass != "draining" {
		return recs
	}

	for _, w := range workloads {
		if cfg.OnlyReschedulable && !w.Profile.Migratability.Reschedulable {
			continue
		}
		if cfg.OnlyBestEffort && w.Profile.Criticality.Class != "best-effort" {
			continue
		}
		recs = append(recs, joulie.RescheduleRecommendation{
			WorkloadRef: w.Ref,
			Reason:      buildReason(twinState, w),
		})
	}
	return recs
}

func buildReason(twinState joulie.NodeTwinState, w WorkloadOnNode) string {
	if twinState.SchedulableClass == "draining" {
		return "node is draining; workload eligible for rescheduling"
	}
	reason := fmt.Sprintf("node under stress (cooling=%.0f psu=%.0f); workload is reschedulable",
		twinState.PredictedCoolingStressScore, twinState.PredictedPsuStressScore)
	return reason
}
