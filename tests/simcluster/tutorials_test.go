package simcluster_test

// scripts/simcluster/tutorials.sh runs the tutorials under examples/ as
// written, with the checks their READMEs carry in HTML comments, and the
// nightly workflow runs it on the tutorials of the simulated cluster.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const tuScript = "scripts/simcluster/tutorials.sh"

// tuRun runs tutorials.sh on the fixture tutorial with extra environment.
func tuRun(t *testing.T, env ...string) (string, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command("bash", filepath.Join(scRepo, tuScript), "tests/simcluster/testdata/tutorial")
	cmd.Env = append(append(os.Environ(), "TU_OUT="+out), env...)
	log, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(out)
	return string(b), string(log), err
}

// TestTU01RunnerRunsBashBlocksAndChecksInOrderAndSkipsMarkedBlocks: the bash
// blocks and the checks run in document order in one shell; a text block and
// a block after a skip marker do not; a failing check fails the run.
func TestTU01RunnerRunsBashBlocksAndChecksInOrderAndSkipsMarkedBlocks(t *testing.T) {
	got, log, err := tuRun(t)
	if err != nil {
		t.Fatalf("tutorials.sh: %v\n%s", err, log)
	}
	if want := "first\nchecked\nlast\n"; got != want {
		t.Errorf("ran %q, want %q\n%s", got, want, log)
	}
	if _, log, err := tuRun(t, "TU_FAIL=1"); err == nil {
		t.Errorf("tutorials.sh passed a failing check\n%s", log)
	}
}

// TestTU02TheNightlyRunsEveryTutorialOfTheSimulatedCluster: every tutorial
// whose README brings up a simulated cluster (make simcluster-up) or runs on
// the cluster of tutorial 01 is named by a job of simcluster-nightly.yml, and
// each named tutorial carries at least one check.
func TestTU02TheNightlyRunsEveryTutorialOfTheSimulatedCluster(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scRepo, ".github/workflows/simcluster-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	run := map[string]bool{}
	call := regexp.MustCompile(`tutorials\.sh((?:\s+examples/[^\s]+)+)`)
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, m := range call.FindAllStringSubmatch(step.Run, -1) {
				for _, dir := range strings.Fields(m[1]) {
					run[strings.TrimSuffix(dir, "/")] = true
				}
			}
		}
	}
	readmes, err := filepath.Glob(filepath.Join(scRepo, "examples/*/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, readme := range readmes {
		b, err := os.ReadFile(readme)
		if err != nil {
			t.Fatal(err)
		}
		dir := "examples/" + filepath.Base(filepath.Dir(readme))
		text := string(b)
		simulated := strings.Contains(text, "make simcluster-up") || strings.Contains(text, "(../01-simulated-cluster/README.md)")
		switch {
		case simulated && !run[dir]:
			t.Errorf("%s runs on the simulated cluster, but no job of simcluster-nightly.yml runs it with %s", dir, tuScript)
		case run[dir] && !strings.Contains(text, "<!-- tutorials: check"):
			t.Errorf("%s runs nightly with no check: add a <!-- tutorials: check --> next to what it says happens", dir)
		}
	}
	if len(run) == 0 {
		t.Fatalf("no job of simcluster-nightly.yml runs %s", tuScript)
	}
}
