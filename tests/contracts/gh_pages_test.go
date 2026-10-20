package contracts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The docs workflows all write gh-pages, each in a concurrency group of its
// own, so two of them can push at once and the later push is rejected: two
// previews failed that way when eight pull requests opened together.
// scripts/gh-pages-push.sh rebases a rejected push and tries again, so every
// write to gh-pages goes through it: no actions-gh-pages step, which pushes
// once, and no git push in a run block.
func TestGitHubPagesWritesGoThroughTheRetryingScript(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows under %s: %v", dir, err)
	}
	gitPush := regexp.MustCompile(`(^|[;&|(]\s*|\s)git(\s+-C\s+\S+)?\s+push\b`)
	used := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") {
				continue
			}
			where := fmt.Sprintf("%s:%d", filepath.Base(f), i+1)
			if strings.Contains(code, "peaceiris/actions-gh-pages") {
				t.Errorf("%s: actions-gh-pages pushes once, and loses to a concurrent push; use scripts/gh-pages-push.sh publish", where)
			}
			if gitPush.MatchString(code) {
				t.Errorf("%s: %q pushes once; use scripts/gh-pages-push.sh push", where, code)
			}
			if strings.Contains(code, "scripts/gh-pages-push.sh") {
				used++
			}
		}
	}
	// The preview, its cleanup, main and its metadata, a release's two
	// deploys and its metadata.
	if used < 7 {
		t.Errorf("scripts/gh-pages-push.sh is used %d times in the workflows, want every gh-pages write (7)", used)
	}
}
