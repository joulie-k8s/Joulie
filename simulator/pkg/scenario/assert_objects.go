package scenario

import (
	"os"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// controlBlocked is the NodeTwin control result of a component the agent
// could not control.
const controlBlocked = "blocked"

// objectKinds are the end snapshot kinds each objects check reads.
var objectKinds = map[string][]string{
	"jobsNotComplete":      {KindJobs},
	"strayPodsOnFakeNodes": {KindPods, KindNodes},
	"nodeHardwares":        {KindNodeHardwares},
	"controlBlocked":       {KindNodeTwins},
}

// observeObjects counts, in the end snapshots: the run's Job objects that
// are not Complete, the workload pods still pending or running on a fake
// Node (KWOK annotation), the NodeHardware objects, and the NodeTwins with
// a blocked component. Objects being deleted are not counted. A check
// whose kind has no end snapshot file has no data: a missing pods file
// would otherwise read as no stray pod.
func observeObjects(ev *evaluation, a Assertion) map[string]observation {
	end := ev.rd.KubeEnd
	out := map[string]observation{}
	for key, kinds := range objectKinds {
		for _, kind := range kinds {
			rel := KubeSnapshotFile(kind, SnapshotEnd)
			if _, err := os.Stat(filepath.Join(ev.rd.Dir, rel)); err != nil {
				out[key] = noData("no %s", rel)
				break
			}
		}
	}
	runLabel := ""
	if ev.rd.RunID != "" {
		runLabel = workload.RunLabelValue(ev.rd.RunID)
	}
	jobs := 0
	for _, j := range end.Jobs {
		if j.Labels[workload.LabelPartOf] != workload.PartOfWorkload || j.DeletionTimestamp != nil {
			continue
		}
		if runLabel != "" && j.Labels[workload.LabelRunID] != runLabel {
			continue
		}
		if !jobComplete(j) {
			jobs++
		}
	}
	fake := map[string]bool{}
	for _, n := range end.Nodes {
		if n.Annotations[clusterspec.KWOKAnnotation] == clusterspec.KWOKValue {
			fake[n.Name] = true
		}
	}
	stray := 0
	for _, p := range end.Pods {
		if p.Labels[workload.LabelPartOf] != workload.PartOfWorkload || p.DeletionTimestamp != nil || !fake[p.Spec.NodeName] {
			continue
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			stray++
		}
	}
	blocked := 0
	for _, t := range end.NodeTwins {
		if cs := t.Status.ControlStatus; cs != nil &&
			((cs.CPU != nil && cs.CPU.Result == controlBlocked) || (cs.GPU != nil && cs.GPU.Result == controlBlocked)) {
			blocked++
		}
	}
	msg := "end snapshot: %d workload Jobs not complete, %d stray workload pods on fake Nodes, %d NodeHardwares, %d NodeTwins with a blocked component"
	args := []any{jobs, stray, len(end.NodeHardwares), blocked}
	for key, v := range map[string]int{"jobsNotComplete": jobs, "strayPodsOnFakeNodes": stray, "nodeHardwares": len(end.NodeHardwares), "controlBlocked": blocked} {
		if _, missing := out[key]; !missing {
			out[key] = observed(float64(v), msg, args...)
		}
	}
	return out
}

func jobComplete(j batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
