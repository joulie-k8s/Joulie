//go:build simcluster

package simcluster_test

import (
	"regexp"
	"sort"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// The known-defect registry of the end-to-end test. A defect here is
// one the run reproduces today and that is waiting for a fix or a decision;
// its check asserts the defect's observation, so the run stays green while
// the defect lasts and turns red the day it no longer reproduces. That run
// fails with "remove it from tests/simcluster/known_defects_test.go": the
// fix landed, and the check must then assert the fixed behaviour instead.
// summary.json lists every defect that reproduced under knownDefects.
//
// This follows the characterisation tests of cmd/agent (TestHwemuA06 for
// D1): they too assert today's wrong behaviour, with the defect named.

// e2eDefect is one registered defect: the check that observes it, what the
// check sees while the defect lasts, and what will change it.
type e2eDefect struct {
	ID, Check, Expected, FlipsWith string
}

// e2eDefects is the registry.
var e2eDefects = []e2eDefect{
	{
		ID:    "D1",
		Check: "E11",
		Expected: "the MI300X agent's GPU inventory fails, because no rocm-smi release has --showpowercap: " +
			"GPU control status none/error with \"unrecognized arguments: --showpowercap\"",
		FlipsWith: "F3, the agent reading the MI300X power cap without --showpowercap",
	},
	{
		ID:    "D15",
		Check: "E12",
		Expected: "a managed Node the agent DaemonSet does not reach (an untolerated taint) gets a NodeTwin with a cap, " +
			"but no NodeHardware and no controlStatus, and its hardware keeps the uncapped limit: the cap is planned, not enforced",
		FlipsWith: "a D15 chart decision: tolerating every taint, or the controller planning only Nodes that run an agent",
	},
	{
		ID:    "D18",
		Check: "MD01",
		Expected: "MIG-mixed and renamed time-slicing GPU Nodes get no GPU intent (none/none/\"no GPU cap intent\") " +
			"and a CPU percent cap, as if they had no GPU",
		FlipsWith: "the controller counting nvidia.com/mig-* and nvidia.com/gpu.shared as GPUs",
	},
}

// e2eDefectByID returns the registered defect id.
func e2eDefectByID(id string) (e2eDefect, bool) {
	for _, d := range e2eDefects {
		if d.ID == id {
			return d, true
		}
	}
	return e2eDefect{}, false
}

// knownDefect reports the observation of registered defect id on the check
// c. While the defect reproduces, the observation is recorded under the
// summary's knownDefects and the check passes. When it no longer does, the
// check fails and asks for the registry entry to go.
func (c *e2eCase) knownDefect(id string, reproduced bool, observed string) {
	c.Helper()
	d, ok := e2eDefectByID(id)
	if !ok {
		c.Fatalf("known defect %s is not in tests/simcluster/known_defects_test.go", id)
	}
	if !reproduced {
		c.Errorf("known defect %s no longer reproduces on %s: observed %s; expected while it lasts: %s (flips with %s). "+
			"The fix has landed: remove it from tests/simcluster/known_defects_test.go and assert the fixed behaviour",
			id, c.name, observed, d.Expected, d.FlipsWith)
		return
	}
	c.Logf("known defect %s reproduces: %s", id, observed)
	c.defects = append(c.defects, scenario.KnownDefect{ID: id, Assertion: c.name, Observed: observed})
}

// e2eCheckID matches the IDs of the end-to-end checks, those of
// e2e_test.go and otel_inputs_test.go.
var e2eCheckID = regexp.MustCompile(`^(E(0[1-9]|1[0-6])|MD0[1-4])$`)

// TestE2EKnownDefectRegistry keeps the registry well formed: unique IDs,
// each on an end-to-end check, with what it looks like and what flips it.
// It needs no cluster.
func TestE2EKnownDefectRegistry(t *testing.T) {
	seen := map[string]bool{}
	ids := make([]string, 0, len(e2eDefects))
	for _, d := range e2eDefects {
		ids = append(ids, d.ID)
		if seen[d.ID] {
			t.Errorf("known defect %s is registered twice", d.ID)
		}
		seen[d.ID] = true
		if !regexp.MustCompile(`^D[0-9]+$`).MatchString(d.ID) {
			t.Errorf("known defect id %q is not D<n>", d.ID)
		}
		if !e2eCheckID.MatchString(d.Check) {
			t.Errorf("known defect %s: check %q is not an end-to-end check id", d.ID, d.Check)
		}
		if d.Expected == "" || d.FlipsWith == "" {
			t.Errorf("known defect %s: say what the check sees while it lasts and what flips it", d.ID)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("known defects %v: keep them in id order", ids)
	}
}
