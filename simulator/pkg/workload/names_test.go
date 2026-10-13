package workload_test

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Keys that sanitize to the same text, keys that agree on their first 37 or
// 50 characters, one key in two runs, and a run id and key that concatenate
// alike all get distinct names. Every name is a DNS label of at most 50
// characters, so an Indexed pod's hostname <name>-<index> fits in 63.
func TestPodNamesNoCollision(t *testing.T) {
	long := strings.Repeat("w", 60)
	cases := []struct{ run, key string }{
		{"run-1", "a_b"},
		{"run-1", "a.b"},
		{"run-1", "A B"},
		{"run-1", "a-b"},
		{"run-1", long + "-1"},
		{"run-1", long + "-2"},
		{"run-1", strings.Repeat("x", 37) + "/worker@0"},
		{"run-1", strings.Repeat("x", 37) + "/worker@10"},
		{"run-2", "a_b"},
		// Same concatenation and same sanitized key "b": only the hash's
		// separator between run id and key tells them apart.
		{"a_", "b"},
		{"a", "_b"},
		{"run-1", ""},
		{"run-1", "___"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		name := workload.ObjectName(c.run, c.key)
		if prev, dup := seen[name]; dup {
			t.Errorf("%q in %q and %s both named %q", c.key, c.run, prev, name)
		}
		seen[name] = c.run + "/" + c.key
		if len(name) > workload.MaxObjectNameLen {
			t.Errorf("name %q has %d characters, max %d", name, len(name), workload.MaxObjectNameLen)
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Errorf("name %q is not a DNS label: %v", name, errs)
		}
		if host := name + "-9999999"; len(validation.IsDNS1123Label(host)) > 0 {
			t.Errorf("hostname %q of an Indexed pod is not a DNS label", host)
		}
		if again := workload.ObjectName(c.run, c.key); again != name {
			t.Errorf("name of %q in %q is not stable: %q then %q", c.key, c.run, name, again)
		}
	}
	if got := workload.ObjectName("r", "wl-000042/worker@120"); !strings.HasPrefix(got, "sim-wl-000042-worker-120-") {
		t.Errorf("name %q does not start with the readable group key", got)
	}

	// The keys GroupJobs makes are distinct too, so their names are: a jobId
	// written like a workload's key, or like the <key>#2 of its split, does
	// not give a second group that key, and is not reported as a split.
	wl := func(id string, class string) workload.JobSpec {
		return workload.JobSpec{JobID: id, Template: workload.Template{WorkloadID: "w", PodRole: "r", SubmitOffsetSec: 1, Class: class}}
	}
	solo := func(id string) workload.JobSpec {
		return workload.JobSpec{JobID: id, Template: workload.Template{Class: workload.ClassStandard}}
	}
	for _, c := range []struct {
		jobs  []workload.JobSpec
		split []string
	}{
		{[]workload.JobSpec{wl("w1", workload.ClassStandard), wl("w2", workload.ClassPerformance), solo("w/r@1#2"), solo("w/r@1")}, []string{"w/r@1"}},
		{[]workload.JobSpec{solo("w/r@1#2"), solo("w/r@1"), wl("w1", workload.ClassStandard), wl("w2", workload.ClassPerformance), wl("w3", workload.ClassIdle)}, []string{"w/r@1"}},
		{[]workload.JobSpec{wl("w1", workload.ClassStandard), solo("w/r@1")}, nil},
	} {
		groups, split := workload.GroupJobs(c.jobs)
		if len(groups) != len(c.jobs) {
			t.Errorf("%d groups of %d jobs that all differ", len(groups), len(c.jobs))
		}
		names := map[string]string{}
		for _, g := range groups {
			name := workload.ObjectName("run-1", g.Key)
			if prev, dup := names[name]; dup {
				t.Errorf("groups %q and %q both named %q", prev, g.Key, name)
			}
			names[name] = g.Key
		}
		if !reflect.DeepEqual(split, c.split) {
			t.Errorf("split %v, want %v: only a workload's pods that differ split it", split, c.split)
		}
	}
}

// A job with a workloadId is grouped by workload, role and submit offset; a
// job without one is a group of its own, keyed by its jobId.
func TestGroupKey(t *testing.T) {
	if got := workload.GroupKey("wl-1-worker-01", "wl-1", "worker", 12.5); got != "wl-1/worker@12.5" {
		t.Errorf("GroupKey with workload = %q", got)
	}
	if got := workload.GroupKey("wl-1-worker-01", "wl-1", "worker", 30); got != "wl-1/worker@30" {
		t.Errorf("GroupKey with whole offset = %q", got)
	}
	if got := workload.GroupKey("solo-7", "", "worker", 30); got != "solo-7" {
		t.Errorf("GroupKey without workload = %q, want the jobId", got)
	}
}
