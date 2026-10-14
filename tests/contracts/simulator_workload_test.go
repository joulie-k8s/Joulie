package contracts_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// --------------------------------------------------------------------------
// Simulator v1 workload objects and the components that read them
// --------------------------------------------------------------------------

// TestSimulatorJobPodClassReadByFSM: the pod a v1 Job object creates, after
// a JSON round trip as the API server stores it, is a performance pod to the
// controller's FSM exactly when the simulator declared it one, whether the
// declaration is the annotation, the affinity or both; a pod that declares
// nothing, or declares standard, never holds a node in performance. Each
// declaration is also read on its own, with the other stripped, so under
// IntentBoth neither can go missing behind the other.
func TestSimulatorJobPodClassReadByFSM(t *testing.T) {
	cases := []struct {
		class  string
		intent workload.Intent
		// byAnnotation and byAffinity: the FSM finds a performance
		// declaration in the annotation alone and in the affinity alone.
		byAnnotation, byAffinity bool
	}{
		{workload.ClassPerformance, workload.IntentAnnotation, true, false},
		{workload.ClassPerformance, workload.IntentAffinity, false, true},
		{workload.ClassPerformance, workload.IntentBoth, true, true},
		{workload.ClassPerformance, workload.IntentNone, false, false},
		{workload.ClassStandard, workload.IntentAnnotation, false, false},
		{workload.ClassStandard, workload.IntentBoth, false, false},
	}
	for _, c := range cases {
		g := workload.Group{
			Key: "wl-1/worker@0",
			Template: workload.Template{
				WorkloadID: "wl-1", PodRole: "worker", Class: c.class,
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				// A trace term the merged affinity must keep.
				Placement: workload.Placement{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
					}}},
				}}},
			},
			JobIDs: []string{"wl-1-worker-01"},
		}
		job := workload.BuildJob(g, workload.BuildOptions{RunID: "run-1", Intent: c.intent})
		b, err := json.Marshal(corev1.Pod{ObjectMeta: job.Spec.Template.ObjectMeta, Spec: job.Spec.Template.Spec})
		if err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := json.Unmarshal(b, &pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
		want := c.byAnnotation || c.byAffinity
		if got := fsm.PodExcludesEco(&pod); got != want {
			t.Errorf("%s under %s: PodExcludesEco = %v, want %v", c.class, c.intent, got, want)
		}
		if got := fsm.CountPerformanceSensitivePods([]corev1.Pod{pod}); (got == 1) != want {
			t.Errorf("%s under %s: CountPerformanceSensitivePods = %d", c.class, c.intent, got)
		}

		annOnly := pod.DeepCopy()
		annOnly.Spec.Affinity = nil
		if got := fsm.PodExcludesEco(annOnly); got != c.byAnnotation {
			t.Errorf("%s under %s, affinity stripped: PodExcludesEco = %v, want %v", c.class, c.intent, got, c.byAnnotation)
		}
		affOnly := pod.DeepCopy()
		delete(affOnly.Annotations, fsm.WorkloadClassAnnotation)
		if got := fsm.PodExcludesEco(affOnly); got != c.byAffinity {
			t.Errorf("%s under %s, annotation stripped: PodExcludesEco = %v, want %v", c.class, c.intent, got, c.byAffinity)
		}
	}
}

// pyRegexRe finds JOB_COMPLETED_RE = re.compile(r"...") in a collect script.
var pyRegexRe = regexp.MustCompile(`(?m)^JOB_COMPLETED_RE\s*=\s*re\.compile\(r"(.*)"\)\s*$`)

// TestJobCompletedLogLineMatchesCollectRegex: the experiments' collect
// scripts read job completions from the simulator's log with
// JOB_COMPLETED_RE. The regex is read from each 06_collect.py, so a change on
// either side fails here: the line the v1 engine prints, behind the log
// package's timestamp, yields the job id, node, class and elapsed seconds,
// and every completion line the simulator prints by hand uses the same
// format.
func TestJobCompletedLogLineMatchesCollectRegex(t *testing.T) {
	root := repoRoot(t)
	scripts, err := filepath.Glob(filepath.Join(root, "experiments", "*", "scripts", "06_collect.py"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, path := range scripts {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := pyRegexRe.FindSubmatch(src)
		if m == nil {
			continue
		}
		found++
		rel, _ := filepath.Rel(root, path)
		re, err := regexp.Compile(string(m[1]))
		if err != nil {
			t.Errorf("%s: JOB_COMPLETED_RE does not compile in Go: %v", rel, err)
			continue
		}
		line := "2026/10/05 12:00:00 " + workload.FormatJobCompletedLog("wl-000042-worker-01", "kwok-node-3", "performance", 1234567*time.Millisecond)
		sub := re.FindStringSubmatch(line)
		if sub == nil {
			t.Errorf("%s: JOB_COMPLETED_RE does not match %q", rel, line)
			continue
		}
		want := map[string]string{"job": "wl-000042-worker-01", "node": "kwok-node-3", "class": "performance", "elapsed": "1234.6"}
		for name, v := range want {
			if i := re.SubexpIndex(name); i < 0 || sub[i] != v {
				t.Errorf("%s: group %s of %q is not %q", rel, name, line, v)
			}
		}
		// A job whose node or class is empty, or has a space, still gives
		// a line the collector reads, with its id intact.
		for _, f := range [][3]string{{"j-1", "", "performance"}, {"j-1", "kwok-node-3", ""}, {"j-1", "kwok node", "eco mode"}} {
			line := workload.FormatJobCompletedLog(f[0], f[1], f[2], time.Second)
			if sub := re.FindStringSubmatch(line); sub == nil || sub[re.SubexpIndex("job")] != f[0] {
				t.Errorf("%s: JOB_COMPLETED_RE does not read job %q from %q", rel, f[0], line)
			}
		}
	}
	if found < 2 {
		t.Fatalf("found JOB_COMPLETED_RE in %d collect scripts, want at least 2 (experiments 02 and 03)", found)
	}

	// Every printf-style call in the simulator binary whose format starts a
	// completion line uses the shared format, so legacy and v1 cannot drift.
	files, err := filepath.Glob(filepath.Join(root, "simulator", "cmd", "simulator", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && strings.HasPrefix(s, "job completed id=") && s != workload.JobCompletedLogFormat {
				t.Errorf("%s: completion format %q differs from workload.JobCompletedLogFormat %q", fset.Position(lit.Pos()), s, workload.JobCompletedLogFormat)
			}
			return true
		})
	}
}
