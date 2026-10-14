package scenario

import "github.com/matbun/joulie/pkg/otel/semconv"

// observeJobs counts the due jobs the filter selects by outcome. A job
// without a terminal event counts as incomplete: the run ended before it
// did. Jobs not due are reported in the message only. With no due job the
// filter selects, every check has no data, so a filter that matches
// nothing cannot pass a count of zero.
func observeJobs(ev *evaluation, a Assertion) map[string]observation {
	f := a.Jobs.JobFilter
	if !ev.hasRunRecords() {
		out := map[string]observation{}
		for _, k := range []string{"completedFraction", "completed", "failed", "incomplete"} {
			out[k] = noData("the simulator logs hold no %s record", semconv.EventSimRunStarted.Name)
		}
		return out
	}
	var due, notDue, completed, failed, incomplete int
	for _, j := range ev.jobs {
		if !f.matches(j) {
			continue
		}
		if !j.Due {
			notDue++
			continue
		}
		due++
		switch j.Outcome {
		case semconv.JobOutcomeCompleted:
			completed++
		case semconv.JobOutcomeFailed:
			failed++
		default:
			incomplete++
		}
	}
	if due == 0 {
		out := map[string]observation{}
		for _, k := range []string{"completedFraction", "completed", "failed", "incomplete"} {
			out[k] = noData("no due job matches the filter (%d not due)", notDue)
		}
		return out
	}
	counts := func(n int) observation {
		return observed(float64(n), "%d due jobs (%d not due): %d completed, %d failed, %d incomplete", due, notDue, completed, failed, incomplete)
	}
	return map[string]observation{
		"completedFraction": observed(float64(completed)/float64(due), "%d of %d due jobs completed (%d not due)", completed, due, notDue),
		"completed":         counts(completed), "failed": counts(failed), "incomplete": counts(incomplete),
	}
}
