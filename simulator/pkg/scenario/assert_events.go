package scenario

import (
	"path/filepath"
	"strconv"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// harnessEvents are the events the runner writes to HarnessLogsFile; the
// simulator writes every other registered event to its own logs.
var harnessEvents = map[string]bool{
	semconv.EventScenarioStarted.Name:            true,
	semconv.EventScenarioVariantStarted.Name:     true,
	semconv.EventScenarioFinished.Name:           true,
	semconv.EventScenarioAssertionEvaluated.Name: true,
}

// eventSource is the logs file, relative to the variant directory, that
// holds the records named event.
func eventSource(event string) string {
	if harnessEvents[event] {
		return filepath.Join(workload.OTLPDir, HarnessLogsFile)
	}
	return filepath.Join(workload.OTLPDir, workload.SimulatorLogsFile)
}

// observeEvents counts the records named Event whose attributes equal every
// where value. A where value is text: a number matches a numeric attribute
// of the same value, true or false a boolean, anything else a string
// attribute exactly. Without a record in the logs file that holds such
// events (the simulator's, or the harness's for joulie.scenario.*) there
// is nothing to count, which is no data: the other file being present says
// nothing about this one.
func observeEvents(ev *evaluation, a Assertion) map[string]observation {
	e := a.Events
	src := eventSource(e.Event)
	n, records := 0, 0
	for _, r := range ev.rd.Events {
		if r.File != src {
			continue
		}
		records++
		if r.Name != e.Event {
			continue
		}
		match := true
		for k, want := range e.Where {
			if !attributeEquals(r.Attributes[k], string(want)) {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	if records == 0 {
		return map[string]observation{"": noData("%s holds no record", src)}
	}
	return map[string]observation{"": observed(float64(n), "%d %s records match", n, e.Event)}
}

func attributeEquals(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case bool:
		b, err := strconv.ParseBool(want)
		return err == nil && b == x
	case int64:
		f, err := strconv.ParseFloat(want, 64)
		return err == nil && f == float64(x)
	case float64:
		f, err := strconv.ParseFloat(want, 64)
		return err == nil && f == x
	}
	return false
}
