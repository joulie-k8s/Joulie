package scenario

import "github.com/matbun/joulie/pkg/otel/semconv"

// observeStall counts the stall events of each reason: job stalls by
// joulie.sim.stall.reason, and run stalls.
func observeStall(ev *evaluation, a Assertion) map[string]observation {
	keys := []string{"gangIncomplete", "noProgress", "runStalled"}
	out := map[string]observation{}
	if !ev.hasRunRecords() {
		for _, k := range keys {
			out[k] = noData("the simulator logs hold no %s record", semconv.EventSimRunStarted.Name)
		}
		return out
	}
	var gang, progress, run int
	for _, e := range ev.rd.Events {
		reason, _ := e.Attributes.String(semconv.AttrSimStallReason)
		switch {
		case e.Name == semconv.EventSimRunStalled.Name:
			run++
		case e.Name == semconv.EventSimJobStalled.Name && reason == semconv.StallReasonGangIncomplete:
			gang++
		case e.Name == semconv.EventSimJobStalled.Name && reason == semconv.StallReasonNoProgress:
			progress++
		}
	}
	msg := "%d gang_incomplete, %d no_progress job stalls; %d run stalls"
	for k, n := range map[string]int{"gangIncomplete": gang, "noProgress": progress, "runStalled": run} {
		out[k] = observed(float64(n), msg, gang, progress, run)
	}
	return out
}
