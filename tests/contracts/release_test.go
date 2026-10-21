package contracts_test

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The release workflow and the images it publishes form a contract with every
// adopter who pulls ":latest": that tag names the newest stable release. A
// pre-release that moves it hands a release candidate to everyone. v0.2.0-rc0
// did exactly that, because the workflow tagged ":latest" on every release.

// releaseVersion runs scripts/release-version.sh the way the workflow does and
// returns its key=value output lines as a map.
func releaseVersion(t *testing.T, tag, prerelease string) map[string]string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release-version.sh"), tag, prerelease)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release-version.sh %s %s: %v\n%s", tag, prerelease, err, out)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("release-version.sh printed %q, want key=value lines", line)
		}
		vars[k] = v
	}
	return vars
}

func TestReleaseVersionMovesLatestOnlyForStableReleases(t *testing.T) {
	cases := []struct {
		name, tag, prerelease string
		wantVersion           string
		wantLatest            string
	}{
		{"stable release", "v0.2.0", "false", "0.2.0", "true"},
		{"rc tag not flagged as pre-release on GitHub", "v0.2.0-rc0", "false", "0.2.0-rc0", "false"},
		{"rc tag flagged as pre-release", "v0.2.0-rc0", "true", "0.2.0-rc0", "false"},
		{"plain version flagged as pre-release", "v0.2.1", "true", "0.2.1", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := releaseVersion(t, tc.tag, tc.prerelease)
			if got["version"] != tc.wantVersion {
				t.Errorf("version = %q, want %q", got["version"], tc.wantVersion)
			}
			if got["latest"] != tc.wantLatest {
				t.Errorf("latest = %q, want %q", got["latest"], tc.wantLatest)
			}
		})
	}
}

// TestReleaseWorkflowTagsLatestOnlyForStableReleases guards the wiring: a
// correct decision in the script is worthless if a tags list in the workflow
// pushes ":latest" without asking for it.
func TestReleaseWorkflowTagsLatestOnlyForStableReleases(t *testing.T) {
	path := filepath.Join(repoRoot(t), ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	found := 0
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, ":latest") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		found++
		if !strings.Contains(line, "steps.vars.outputs.latest == 'true'") {
			t.Errorf("release.yml:%d pushes :latest on every release, pre-releases included:\n  %s", i+1, strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("release.yml tags no image :latest; update this test if that is intended")
	}
	if !strings.Contains(string(data), "scripts/release-version.sh") {
		t.Error("release.yml does not derive its version and latest flag from scripts/release-version.sh")
	}
}

// releaseWorkflow is the part of .github/workflows/release.yml the
// platform checks read.
type releaseWorkflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix struct {
				Include []struct {
					GOOS   string `json:"goos"`
					GOARCH string `json:"goarch"`
				} `json:"include"`
			} `json:"matrix"`
		} `json:"strategy"`
		Steps []struct {
			Name string            `json:"name"`
			Uses string            `json:"uses"`
			With map[string]string `json:"with"`
		} `json:"steps"`
	} `json:"jobs"`
}

// TestReleaseImagesAreMultiArch checks that every image the release
// workflow pushes is built for every release platform, so an arm64 managed
// node gets an agent: each docker/build-push-action step lists the
// platforms, and the Dockerfile it builds cross-compiles. Its builder stages
// run on the build platform (FROM --platform=$BUILDPLATFORM) and build with
// GOOS=${TARGETOS} GOARCH=${TARGETARCH}, and its final stage runs nothing,
// since a RUN there would need an emulator for the other platform. The
// kubectl plugin is built for linux on every release platform too.
func TestReleaseImagesAreMultiArch(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf releaseWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	dockerfiles := map[string]bool{}
	images, plugin := 0, map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		job := wf.Jobs[name]
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "docker/build-push-action@") {
				continue
			}
			images++
			platforms := strings.Split(strings.ReplaceAll(step.With["platforms"], " ", ""), ",")
			for _, p := range releasePlatforms {
				if !slices.Contains(platforms, p) {
					t.Errorf("release.yml job %s, step %q: platforms %q has no %s", name, step.Name, step.With["platforms"], p)
				}
			}
			file := step.With["file"]
			if file == "" {
				file = "Dockerfile"
			}
			dockerfiles[file] = true
		}
		for _, m := range job.Strategy.Matrix.Include {
			plugin[m.GOOS+"/"+m.GOARCH] = true
		}
	}
	if images < 4 {
		t.Fatalf("release.yml builds %d images, want at least the agent, controller manager, scheduler extender and simulator", images)
	}
	for _, p := range releasePlatforms {
		if !plugin[p] {
			t.Errorf("release.yml builds the kubectl plugin for %v, not for %s", plugin, p)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(dockerfiles)) {
		checkCrossCompilingDockerfile(t, root, rel)
	}
}

// checkCrossCompilingDockerfile checks one Dockerfile the release builds;
// see TestReleaseImagesAreMultiArch.
func checkCrossCompilingDockerfile(t *testing.T, root, rel string) {
	t.Helper()
	type stage struct {
		from            string
		line            int
		runs, goBuilds  []int
		args            map[string]bool
		fixedArchBuilds []int
	}
	var stages []*stage
	lines(t, root, rel, func(n int, line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			return
		}
		f := strings.Fields(trimmed)
		switch strings.ToUpper(f[0]) {
		case "FROM":
			stages = append(stages, &stage{from: trimmed, line: n, args: map[string]bool{}})
			return
		}
		if len(stages) == 0 {
			return
		}
		s := stages[len(stages)-1]
		switch strings.ToUpper(f[0]) {
		case "ARG":
			for _, a := range f[1:] {
				name, _, _ := strings.Cut(a, "=")
				s.args[name] = true
			}
		case "RUN":
			s.runs = append(s.runs, n)
			if strings.Contains(trimmed, "go build") {
				if strings.Contains(trimmed, "GOOS=${TARGETOS}") && strings.Contains(trimmed, "GOARCH=${TARGETARCH}") {
					s.goBuilds = append(s.goBuilds, n)
				} else {
					s.fixedArchBuilds = append(s.fixedArchBuilds, n)
				}
			}
		}
	})
	if len(stages) < 2 {
		t.Errorf("%s: %d stages, want a builder and a final stage", rel, len(stages))
		return
	}
	builds := 0
	for i, s := range stages {
		final := i == len(stages)-1
		if final {
			if len(s.runs) > 0 {
				t.Errorf("%s:%v: the final stage runs commands, which an image of another platform can only run under emulation; build in a builder stage", rel, s.runs)
			}
			continue
		}
		if !strings.Contains(s.from, "--platform=$BUILDPLATFORM") {
			t.Errorf("%s:%d: builder stage %q does not run on the build platform; write FROM --platform=$BUILDPLATFORM", rel, s.line, s.from)
		}
		for _, n := range s.fixedArchBuilds {
			t.Errorf("%s:%d: go build does not set GOOS=${TARGETOS} GOARCH=${TARGETARCH}, so every platform gets the build platform's binary", rel, n)
		}
		if len(s.goBuilds) > 0 && (!s.args["TARGETOS"] || !s.args["TARGETARCH"]) {
			t.Errorf("%s:%d: the stage builds for ${TARGETOS}/${TARGETARCH} without ARG TARGETOS and ARG TARGETARCH, so both are empty", rel, s.line)
		}
		builds += len(s.goBuilds)
	}
	if builds == 0 {
		t.Errorf("%s: no go build for ${TARGETOS}/${TARGETARCH}", rel)
	}
}

// Every release carries notes: a release published without any gets its
// CHANGELOG.md section, through scripts/changelog-section.sh, which fails on a
// tag the changelog does not describe.
func TestReleaseWorkflowPublishesChangelogNotes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			If    string `json:"if"`
			Steps []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["release-notes"]
	if !ok {
		t.Fatal("release.yml has no release-notes job")
	}
	if job.If != "${{ github.event.release.body == '' }}" {
		t.Errorf("release-notes runs if %q, want only for a release without notes", job.If)
	}
	var run string
	for _, s := range job.Steps {
		run += s.Run
	}
	if !strings.Contains(run, "scripts/changelog-section.sh") || !strings.Contains(run, "gh release edit") {
		t.Errorf("release-notes does not publish the changelog section:\n%s", run)
	}
}
