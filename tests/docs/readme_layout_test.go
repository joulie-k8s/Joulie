// Package docs holds tests that keep prose which names files honest.
//
// The repository layout block in the README drifted for months: it listed a
// package nothing imports as the agent's discovery path, it had no entry for
// api/, which is the source of truth for the CRDs, and it was missing four
// directories that had appeared since it was written. None of that is visible
// to a reader, which is exactly why it survived. These tests make the drift
// fail the build instead.
package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is this package's distance from the top of the repository.
const repoRoot = "../.."

// layoutHeading opens the block, and the block itself is the first fenced
// section after it.
const layoutHeading = "## Repository layout"

// gitignoredRootDir reports whether a pattern of the root .gitignore ignores
// the top level directory name: such a directory, bin/ or a local tmp/, never
// reaches a clone, so the layout block need not list it. Only patterns that
// name one path segment are considered (tmp/, /bin/, results*/); negations
// and nested paths never match a root directory here.
func gitignoredRootDir(t *testing.T, name string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	return gitignoreMatchesRootDir(string(raw), name)
}

func gitignoreMatchesRootDir(gitignore, name string) bool {
	for _, line := range strings.Split(gitignore, "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" || strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "!") {
			continue
		}
		pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
		if strings.Contains(pattern, "/") {
			continue
		}
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// readLayoutPaths returns every path the layout block names, already joined to
// the repository root. An indented line is relative to the last line that was
// not indented, which is how the experiments are written.
func readLayoutPaths(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == layoutHeading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("README.md has no %q section. Fix: restore it, or update layoutHeading here.", layoutHeading)
	}

	var (
		paths  []string
		parent string
		inside bool
	)
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "```") {
			if inside {
				break
			}
			inside = true
			continue
		}
		if !inside || strings.TrimSpace(line) == "" {
			continue
		}
		indented := strings.HasPrefix(line, " ")
		fields := strings.Fields(line)
		p := strings.TrimSuffix(fields[0], "/")
		if indented {
			if parent == "" {
				t.Fatalf("indented layout entry %q has no parent above it", p)
			}
			p = filepath.Join(parent, p)
		} else {
			parent = p
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		t.Fatalf("the %q block is empty", layoutHeading)
	}
	return paths
}

// TestREADMELayoutPathsExist catches an entry that was renamed or removed
// without the README following it.
func TestREADMELayoutPathsExist(t *testing.T) {
	for _, p := range readLayoutPaths(t) {
		if _, err := os.Stat(filepath.Join(repoRoot, p)); err != nil {
			t.Errorf("README.md repository layout names %s, which does not exist. "+
				"Fix: update the block, or restore the directory.", p)
		}
	}
}

// TestREADMELayoutListsEveryRootDirectory catches the other direction: a new
// top level directory that the README never mentions. The root listing is the
// first thing a newcomer reads, so an entry missing from it is an entry they
// will not find.
func TestREADMELayoutListsEveryRootDirectory(t *testing.T) {
	listed := readLayoutPaths(t)

	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatalf("read the repository root: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		// Hidden directories are tooling, not the project's shape.
		if !e.IsDir() || strings.HasPrefix(name, ".") || gitignoredRootDir(t, name) {
			continue
		}
		found := false
		for _, p := range listed {
			if p == name || strings.HasPrefix(p, name+string(filepath.Separator)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s/ is a top level directory that the README repository layout does not mention. "+
				"Fix: add a line for it, or nest it under an existing directory (see CLAUDE.md).", name)
		}
	}
}

// The .gitignore matcher skips exactly the directories git would ignore at the
// root, and nothing the repository tracks.
func TestGitignoreMatchesRootDir(t *testing.T) {
	const gitignore = "# build output\nbin/\n/tmp/\nresults*/\n!keep/\ndocs/generated/\n"
	for name, want := range map[string]bool{
		"bin": true, "tmp": true, "results-2026": true,
		"pkg": false, "keep": false, "docs": false, "generated": false,
	} {
		if got := gitignoreMatchesRootDir(gitignore, name); got != want {
			t.Errorf("%s: ignored = %v, want %v", name, got, want)
		}
	}
}
