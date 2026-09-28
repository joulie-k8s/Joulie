package contracts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
