package semconv

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The job identity every joulie.sim.job.* event carries, which
// frozenEvents lists through withFrozenJob.
var frozenWorkloadJobAttributes = []string{
	"joulie.sim.job.id", "joulie.sim.workload.id", "joulie.sim.workload.type", "joulie.sim.pod.role",
	"joulie.workload.class", "k8s.namespace.name", "k8s.job.name", "k8s.pod.name", "k8s.pod.uid",
}

func withFrozenJob(extra ...string) []string {
	return append(append([]string(nil), frozenWorkloadJobAttributes...), extra...)
}

// Every job event carries the job's identity, so the evaluator can join the
// events of one job without knowing which event it is reading.
func TestWorkloadJobEventsCarryJobIdentity(t *testing.T) {
	n := 0
	for _, e := range Events() {
		if !strings.HasPrefix(e.Name, "joulie.sim.job.") {
			continue
		}
		n++
		for _, a := range frozenWorkloadJobAttributes {
			if !e.HasAttribute(a) {
				t.Errorf("%s does not list %s", e.Name, a)
			}
		}
	}
	if n != 9 {
		t.Errorf("found %d joulie.sim.job.* events, want 9", n)
	}
}

// A scenario names its own PromQL captures under CaptureMetricPrefix, so
// the only registered metrics there are the runner's three NodeTwin
// samples; a metric another package registers under the prefix could
// collide with a capture a scenario names.
func TestWorkloadCapturePrefixHoldsOnlyNodeTwinSamples(t *testing.T) {
	var got []string
	for _, m := range Metrics() {
		if strings.HasPrefix(m.Name, CaptureMetricPrefix) {
			got = append(got, m.Name)
		}
	}
	sort.Strings(got)
	want := []string{"joulie.capture.nodetwin.control_result", "joulie.capture.nodetwin.measured_power", "joulie.capture.nodetwin.schedulable_class"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registered metrics under %q: %v, want %v", CaptureMetricPrefix, got, want)
	}
}
