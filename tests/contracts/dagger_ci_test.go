package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The pull request jobs run the functions of the Dagger module in ci/, and
// `make ci-local` runs the module's `all`, so a job run on a laptop is the
// job CI runs. These tests keep the three from drifting apart: a job that
// stops calling the module, or calls a function the module lacks, or one
// that `all` skips, or a Dagger version other than the module's.

// daggerJobs are the pull request workflows, whose jobs each run one
// function of the module, and the jobs that may not: a docs preview's
// cleanup only deletes a directory of gh-pages.
var daggerJobs = map[string]map[string]bool{
	"ci.yml":                {},
	"integration-tests.yml": {},
	"docs-preview.yml":      {"cleanup": true},
	"vulnscan.yml":          {},
}

// notInAll are the functions a pull request job runs that `all`, and so
// make ci-local, leaves out, and why.
var notInAll = map[string]string{
	"vulnscan": "a new advisory fails it whatever the change; the gate runs only when a manifest changes",
}

const daggerAction = "dagger/dagger-for-github@"

var (
	daggerFunctionRE = regexp.MustCompile(`(?m)^\s*@function\s*\n\s*(?:async\s+)?def\s+([a-z_][a-z0-9_]*)\(`)
	allChecksRE      = regexp.MustCompile(`(?s)\nALL_CHECKS = \((.*?)\n\)`)
	quotedRE         = regexp.MustCompile(`"([^"]+)"`)
)

// readDaggerModule returns the module's functions, as the CLI names them
// (kebab case), the names ALL_CHECKS lists, and the engine version of
// ci/dagger.json without its "v".
func readDaggerModule(t *testing.T, root string) (functions, all map[string]bool, version string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "ci", "src", "joulie_ci", "*.py"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Python files in ci/src/joulie_ci: %v", err)
	}
	functions, all = map[string]bool{}, map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range daggerFunctionRE.FindAllSubmatch(b, -1) {
			functions[strings.ReplaceAll(string(m[1]), "_", "-")] = true
		}
		if m := allChecksRE.FindSubmatch(b); m != nil {
			for _, q := range quotedRE.FindAllSubmatch(m[1], -1) {
				all[string(q[1])] = true
			}
		}
	}
	if len(functions) == 0 || len(all) == 0 {
		t.Fatalf("ci/src/joulie_ci declares %d functions and ALL_CHECKS lists %d; the parser no longer reads the module", len(functions), len(all))
	}
	var cfg struct {
		EngineVersion string `json:"engineVersion"`
	}
	b, err := os.ReadFile(filepath.Join(root, "ci", "dagger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.EngineVersion == "" {
		t.Fatalf("ci/dagger.json: no engineVersion (%v)", err)
	}
	return functions, all, strings.TrimPrefix(cfg.EngineVersion, "v")
}

type workflowStep struct {
	Name string            `json:"name"`
	Uses string            `json:"uses"`
	Run  string            `json:"run"`
	With map[string]string `json:"with"`
}

type workflowJob struct {
	Steps []workflowStep `json:"steps"`
}

func TestPullRequestJobsRunTheDaggerModule(t *testing.T) {
	root := repoRoot(t)
	functions, all, version := readDaggerModule(t, root)
	for name := range all {
		if !functions[name] {
			t.Errorf("ALL_CHECKS lists %q, which is not a function of the module", name)
		}
		if _, ok := notInAll[name]; ok {
			t.Errorf("ALL_CHECKS lists %q, which notInAll keeps out of it", name)
		}
	}
	shas := map[string]string{}
	for file, exempt := range daggerJobs {
		var wf struct {
			Jobs map[string]workflowJob `json:"jobs"`
		}
		b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(wf.Jobs) == 0 {
			t.Fatalf("%s has no jobs", file)
		}
		for id, job := range wf.Jobs {
			if exempt[id] {
				continue
			}
			where := file + " job " + id
			calls := 0
			for _, s := range job.Steps {
				if strings.HasPrefix(s.Uses, "actions/setup-") || strings.HasPrefix(s.Uses, "azure/setup-") || strings.HasPrefix(s.Uses, "peaceiris/actions-hugo") {
					t.Errorf("%s: step %q installs %s on the runner; the job's tools belong in its Dagger function", where, s.Name, s.Uses)
				}
				if !strings.HasPrefix(s.Uses, daggerAction) {
					continue
				}
				calls++
				shas[s.Uses] = where
				if s.With["module"] != "./ci" || s.With["verb"] != "call" {
					t.Errorf("%s: step %q runs module %q verb %q, want ./ci and call", where, s.Name, s.With["module"], s.With["verb"])
				}
				if s.With["version"] != version {
					t.Errorf("%s: step %q runs Dagger %q, but ci/dagger.json pins engineVersion v%s", where, s.Name, s.With["version"], version)
				}
				fn := strings.Fields(s.With["args"] + " ")[0]
				_, exempt := notInAll[fn]
				switch {
				case !functions[fn]:
					t.Errorf("%s: step %q calls %q, which is not a function of the module in ci/", where, s.Name, fn)
				case !all[fn] && !exempt:
					t.Errorf("%s: step %q calls %q, which ALL_CHECKS leaves out, so make ci-local would not run it", where, s.Name, fn)
				}
			}
			if calls != 1 {
				t.Errorf("%s has %d %s steps, want 1: every pull request job runs one function of the module", where, calls, daggerAction)
			}
		}
	}
	// Any other workflow that calls the module calls a function it has, at
	// its engine version.
	others, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range others {
		file := filepath.Base(path)
		if _, ok := daggerJobs[file]; ok {
			continue
		}
		var wf struct {
			Jobs map[string]workflowJob `json:"jobs"`
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for id, job := range wf.Jobs {
			for _, s := range job.Steps {
				if !strings.HasPrefix(s.Uses, daggerAction) {
					continue
				}
				shas[s.Uses] = file + " job " + id
				fn := strings.Fields(s.With["args"] + " ")[0]
				if s.With["module"] != "./ci" || s.With["version"] != version || !functions[fn] {
					t.Errorf("%s job %s: step %q runs module %q at %q calling %q; want ./ci at %s and a function of the module", file, id, s.Name, s.With["module"], s.With["version"], fn, version)
				}
			}
		}
	}
	if len(shas) > 1 {
		t.Errorf("the workflows pin %s at different commits: %v", daggerAction, shas)
	}

	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^ci-local:\n\tdagger -m \./ci call all `).Match(makefile) {
		t.Error("Makefile: ci-local does not run `dagger -m ./ci call all`, so it is not what a pull request runs")
	}
}
