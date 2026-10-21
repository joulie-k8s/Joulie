package scripts

// Tests of changelog-section.sh, which the release workflow runs to publish a
// version's CHANGELOG.md section as the notes of a release published without
// any: the notes are the section's body alone, and a version the changelog
// does not describe fails rather than publishing empty notes.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const changelogFixture = `# Changelog

Intro.

## Unreleased

- next

## v1.2.0 (2026-01-02)

### Fixes

- one (#2)

- two (#3)


## v1.1.0 (2026-01-01)

- old

## v1.0.0 (2025-12-01)

## Earlier releases

- links
`

func changelogSection(t *testing.T, tag, file string) (string, error) {
	t.Helper()
	out, err := exec.Command("bash", "changelog-section.sh", tag, file).Output()
	return string(out), err
}

func TestCS01PrintsTheBodyOfTheVersionSection(t *testing.T) {
	file := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(file, []byte(changelogFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := changelogSection(t, "v1.2.0", file)
	if err != nil {
		t.Fatal(err)
	}
	if want := "### Fixes\n\n- one (#2)\n\n- two (#3)\n"; got != want {
		t.Errorf("v1.2.0 notes %q, want %q", got, want)
	}
	if got, _ := changelogSection(t, "v1.1.0", file); got != "- old\n" {
		t.Errorf("v1.1.0 notes %q, want the section up to the next heading", got)
	}
}

// A version with no section, or an empty one, exits 1 with no notes, so the
// release job fails instead of publishing an empty release description.
func TestCS02RefusesAMissingOrEmptySection(t *testing.T) {
	file := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(file, []byte(changelogFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v9.9.9", "v1.0.0", "v1.2"} {
		got, err := changelogSection(t, tag, file)
		var exit *exec.ExitError
		if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || got != "" {
			t.Errorf("%s: output %q, error %v, want exit 1 and no output", tag, got, err)
		}
	}
}

// Every version section of the repository's changelog has notes, so the
// release job can publish them.
func TestCS03EveryVersionInTheChangelogHasNotes(t *testing.T) {
	path := filepath.Join("..", "CHANGELOG.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	versions := 0
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "##" || !strings.HasPrefix(f[1], "v") {
			continue
		}
		versions++
		if got, err := changelogSection(t, f[1], path); err != nil || strings.TrimSpace(got) == "" {
			t.Errorf("CHANGELOG.md %s: %v, notes %q", f[1], err, got)
		}
	}
	if versions == 0 {
		t.Error("CHANGELOG.md has no version section")
	}
}
