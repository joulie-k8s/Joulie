package contracts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// stageDoc is the part of a KWOK Stage these tests read.
type stageDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Selector struct {
			MatchExpressions []struct {
				Key      string   `json:"key"`
				Operator string   `json:"operator"`
				Values   []string `json:"values"`
			} `json:"matchExpressions"`
		} `json:"selector"`
		Next struct {
			StatusTemplate string `json:"statusTemplate"`
		} `json:"next"`
		Delay map[string]any `json:"delay"`
	} `json:"spec"`
}

// readStages decodes every document of a Stage manifest.
func readStages(t *testing.T, path string) []stageDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []stageDoc
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(b), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var s stageDoc
		if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if s.Kind != "" {
			out = append(out, s)
		}
	}
	return out
}

// TestKWOKPodCompleteStageSelectsSimAnnotation: the simulator ends a v1
// pod by patching workload.AnnotationComplete onto it, and only the
// override Stage turns that into Succeeded. The override has the name of
// the vendored Stage it replaces, selects Running pods carrying the
// annotation, and has no delay and no owner selector; up.sh applies the
// overrides after the vendored Stages, so the override wins; and no
// override keeps the vendored selector on Job-owned pods, which would end
// every Job pod at once, before it did its work.
func TestKWOKPodCompleteStageSelectsSimAnnotation(t *testing.T) {
	root := repoRoot(t)
	manifests := filepath.Join(root, "scripts", "simcluster", "manifests")

	stages := readStages(t, filepath.Join(manifests, "stage-pod-complete.yaml"))
	if len(stages) != 1 {
		t.Fatalf("stage-pod-complete.yaml holds %d Stages, want 1", len(stages))
	}
	s := stages[0]
	if s.Kind != "Stage" || s.Metadata.Name != workload.StagePodComplete {
		t.Errorf("override is %s %q, want Stage %q", s.Kind, s.Metadata.Name, workload.StagePodComplete)
	}
	wantKey := `.metadata.annotations["` + workload.AnnotationComplete + `"]`
	var annotated, running bool
	for _, e := range s.Spec.Selector.MatchExpressions {
		if strings.Contains(e.Key, "ownerReferences") {
			t.Errorf("override selects on %s: the owner selector must be dropped", e.Key)
		}
		if e.Key == wantKey && e.Operator == "In" && len(e.Values) == 1 && e.Values[0] == workload.CompleteValue {
			annotated = true
		}
		if e.Key == ".status.phase" && e.Operator == "In" && len(e.Values) == 1 && e.Values[0] == "Running" {
			running = true
		}
	}
	if !annotated || !running {
		t.Errorf("override selector %+v: want %s In [%s] and .status.phase In [Running]", s.Spec.Selector.MatchExpressions, wantKey, workload.CompleteValue)
	}
	if len(s.Spec.Delay) > 0 {
		t.Errorf("override has a delay %v; the simulator decides when a pod ends", s.Spec.Delay)
	}
	if !strings.Contains(s.Spec.Next.StatusTemplate, "phase: Succeeded") {
		t.Errorf("override does not move the pod to Succeeded:\n%s", s.Spec.Next.StatusTemplate)
	}

	// The vendored Stage of that name exists, so the override replaces it.
	vendored, _ := filepath.Glob(filepath.Join(manifests, "kwok-*", "stage-*.yaml"))
	found := false
	for _, p := range vendored {
		for _, v := range readStages(t, p) {
			found = found || v.Metadata.Name == workload.StagePodComplete
		}
	}
	if !found {
		t.Errorf("no vendored Stage named %s in %v; the override replaces nothing", workload.StagePodComplete, vendored)
	}

	// No override keeps the Job-owner selector.
	overrides, _ := filepath.Glob(filepath.Join(manifests, "stage-*.yaml"))
	for _, p := range overrides {
		for _, o := range readStages(t, p) {
			for _, e := range o.Spec.Selector.MatchExpressions {
				if strings.Contains(e.Key, "ownerReferences") {
					t.Errorf("%s: Stage %s selects on %s", filepath.Base(p), o.Metadata.Name, e.Key)
				}
			}
		}
	}

	// up.sh applies the vendored Stages, then every override.
	b, err := os.ReadFile(filepath.Join(root, "scripts", "simcluster", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	up := string(b)
	vend := strings.Index(up, `stage-fast.yaml"`)
	over := strings.Index(up, `-name 'stage-*.yaml'`)
	if vend < 0 || over < 0 || over < vend {
		t.Errorf("up.sh must apply the vendored stage-fast.yaml and then the stage-*.yaml overrides (vendored at %d, overrides at %d)", vend, over)
	}
}
