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

// rootDirsNotListed are the top level directories the layout block is allowed
// to leave out: bin/ is build output (.gitignore line 94) and anything hidden
// is tooling, not the project's shape.
var rootDirsNotListed = map[string]bool{"bin": true}

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
		if !e.IsDir() || strings.HasPrefix(name, ".") || rootDirsNotListed[name] {
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
