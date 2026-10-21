package scripts

// Tests of gh-pages-push.sh against a bare repository in a temporary
// directory. A fake git first on PATH pushes a competing commit to gh-pages
// just before the script's own push, as another docs workflow does when two
// run at once, so the race is reproduced on every run.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ghPagesScript = "gh-pages-push.sh"

// ghPagesFakeGit runs a competing push before the script's push, the first
// $FAKE_COMPETE_PUSHES times (1), then hands every call to the real git.
const ghPagesFakeGit = `#!/usr/bin/env bash
if [[ " $* " == *" push "* && -z ${FAKE_COMPETING:-} ]]; then
  n=$(cat "$FAKE_STATE/competed" 2>/dev/null || echo 0)
  if ((n < ${FAKE_COMPETE_PUSHES:-1})); then
    echo $((n + 1)) >"$FAKE_STATE/competed"
    (
      export FAKE_COMPETING=1
      cd "$FAKE_COMPETITOR" &&
        "$REAL_GIT" pull --quiet --rebase origin gh-pages &&
        mkdir -p previews/pr-2 && echo "competitor $n" >"previews/pr-2/push-$n.html" &&
        "$REAL_GIT" add -A && "$REAL_GIT" commit --quiet -m "competitor $n" &&
        "$REAL_GIT" push --quiet origin HEAD:gh-pages
    ) >&2 || { echo "fake git: the competing push failed" >&2; exit 99; }
  fi
fi
exec "$REAL_GIT" "$@"
`

type ghPagesEnv struct {
	t                         *testing.T
	remote, competitor, state string
	bin, realGit              string
}

// ghPagesNew makes a bare repository whose gh-pages holds .nojekyll and
// main/index.html, and a clone of it that pushes the competing commits.
func ghPagesNew(t *testing.T) *ghPagesEnv {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	e := &ghPagesEnv{t: t, realGit: realGit,
		remote: filepath.Join(root, "remote.git"), competitor: filepath.Join(root, "competitor"),
		state: filepath.Join(root, "state"), bin: filepath.Join(root, "bin")}
	for _, d := range []string{e.state, e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seed := filepath.Join(root, "seed")
	e.git("", "init", "--quiet", "--bare", e.remote)
	e.git("", "init", "--quiet", "-b", "gh-pages", seed)
	e.write(filepath.Join(seed, ".nojekyll"), "")
	e.write(filepath.Join(seed, "main/index.html"), "main docs\n")
	e.git(seed, "add", "-A")
	e.git(seed, "commit", "--quiet", "-m", "seed")
	e.git(seed, "push", "--quiet", e.remote, "gh-pages")
	e.git("", "clone", "--quiet", "--branch", "gh-pages", e.remote, e.competitor)
	e.git(e.competitor, "config", "user.name", "competitor")
	e.git(e.competitor, "config", "user.email", "competitor@example.com")
	e.write(filepath.Join(e.bin, "git"), ghPagesFakeGit)
	if err := os.Chmod(filepath.Join(e.bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *ghPagesEnv) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// noGitConfig keeps the user's and the system's git configuration out of
// every git call.
func noGitConfig() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

// gitEnv is the environment of the test's own git calls: noGitConfig and a
// fixed identity. The script runs without the identity, so its own
// configuration names the commits it makes.
func (e *ghPagesEnv) gitEnv() []string {
	return append(noGitConfig(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
}

func (e *ghPagesEnv) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(e.realGit, args...)
	cmd.Dir = dir
	cmd.Env = e.gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// run runs the script in dir (the test's directory when empty) with the
// fake git first on PATH and the given extra environment.
func (e *ghPagesEnv) run(dir string, extra map[string]string, args ...string) (string, int) {
	e.t.Helper()
	script, err := filepath.Abs(ghPagesScript)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	env := append(noGitConfig(), "PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GIT="+e.realGit, "FAKE_STATE="+e.state, "FAKE_COMPETITOR="+e.competitor,
		"GH_PAGES_REMOTE="+e.remote, "GH_PAGES_RETRY_SECONDS=0", "TMPDIR="+e.t.TempDir(), "RUNNER_TEMP=")
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), 0
}

// files lists the files of gh-pages in the bare repository.
func (e *ghPagesEnv) files() []string {
	return strings.Fields(e.git(e.remote, "ls-tree", "-r", "--name-only", "gh-pages"))
}

func (e *ghPagesEnv) commits() int {
	return len(strings.Fields(e.git(e.remote, "rev-list", "gh-pages")))
}

func (e *ghPagesEnv) site(files map[string]string) string {
	dir := e.t.TempDir()
	for name, content := range files {
		e.write(filepath.Join(dir, name), content)
	}
	return dir
}

func ghPagesWant(t *testing.T, files []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("gh-pages lacks %s; it holds %v", w, files)
		}
	}
}

// GP01: a publish whose push loses the race to another workflow's push
// rebases onto it and pushes again, keeping both.
func TestGP01PublishPushesAgainAfterAnotherWorkflowPushedFirst(t *testing.T) {
	e := ghPagesNew(t)
	site := e.site(map[string]string{"index.html": "preview\n", "docs/a.html": "a\n"})
	out, code := e.run("", nil, "publish", site, "previews/pr-1", "docs: preview for PR #1")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "rejected (try 1 of 5)") || !strings.Contains(out, "pushed gh-pages (try 2 of 5)") {
		t.Errorf("want a rejected first push and a second that went through:\n%s", out)
	}
	ghPagesWant(t, e.files(), ".nojekyll", "main/index.html", "previews/pr-2/push-0.html",
		"previews/pr-1/index.html", "previews/pr-1/docs/a.html")
	if msg := strings.TrimSpace(e.git(e.remote, "log", "-1", "--format=%s %an", "gh-pages")); msg != "docs: preview for PR #1 github-actions[bot]" {
		t.Errorf("the tip of gh-pages is %q", msg)
	}
}

// GP02: publish keeps the destination's files the site lacks, as
// actions-gh-pages did with keep_files, and a site that changes nothing
// makes no commit.
func TestGP02PublishKeepsOtherFilesAndSkipsAnUnchangedSite(t *testing.T) {
	e := ghPagesNew(t)
	site := e.site(map[string]string{"new.html": "new\n"})
	if out, code := e.run("", map[string]string{"FAKE_COMPETE_PUSHES": "0"}, "publish", site, "main/", "docs: main"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	ghPagesWant(t, e.files(), "main/index.html", "main/new.html")
	before := e.commits()
	out, code := e.run("", map[string]string{"FAKE_COMPETE_PUSHES": "0"}, "publish", site, "main", "docs: main again")
	if code != 0 || !strings.Contains(out, "nothing to commit") {
		t.Fatalf("exit %d, want nothing to commit:\n%s", code, out)
	}
	if after := e.commits(); after != before {
		t.Errorf("an unchanged site made %d commits", after-before)
	}
}

// GP03: remove deletes its directory and nothing else, also across a race.
func TestGP03RemoveDeletesOnlyItsDirectory(t *testing.T) {
	e := ghPagesNew(t)
	site := e.site(map[string]string{"index.html": "preview\n"})
	if out, code := e.run("", map[string]string{"FAKE_COMPETE_PUSHES": "0"}, "publish", site, "previews/pr-1", "docs: preview"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	out, code := e.run("", nil, "remove", "previews/pr-1", "docs: remove preview for PR #1")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	files := e.files()
	for _, f := range files {
		if strings.HasPrefix(f, "previews/pr-1/") {
			t.Errorf("%s survived the removal", f)
		}
	}
	ghPagesWant(t, files, "main/index.html", "previews/pr-2/push-0.html")
}

// GP04: push takes the commits of a gh-pages clone, as the metadata steps of
// docs-main and docs-release make them, and rebases them across a race.
func TestGP04PushRebasesTheCommitsOfAClone(t *testing.T) {
	e := ghPagesNew(t)
	clone := filepath.Join(t.TempDir(), "gh-pages")
	e.git("", "clone", "--quiet", "--depth", "1", "--branch", "gh-pages", "file://"+e.remote, clone)
	e.git(clone, "config", "user.name", "github-actions[bot]")
	e.git(clone, "config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
	e.write(filepath.Join(clone, "versions.json"), `{"default":"main"}`+"\n")
	e.git(clone, "add", "-A")
	e.git(clone, "commit", "--quiet", "-m", "docs: update main docs metadata")
	out, code := e.run(clone, nil, "push")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	ghPagesWant(t, e.files(), "versions.json", "previews/pr-2/push-0.html", "main/index.html")
}

// GP05: a push rejected on every try fails, after GH_PAGES_TRIES of them.
func TestGP05PushFailsAfterItsTries(t *testing.T) {
	e := ghPagesNew(t)
	site := e.site(map[string]string{"index.html": "preview\n"})
	out, code := e.run("", map[string]string{"FAKE_COMPETE_PUSHES": "100", "GH_PAGES_TRIES": "3"},
		"publish", site, "previews/pr-1", "docs: preview")
	if code == 0 {
		t.Fatalf("exit 0 although every push lost the race:\n%s", out)
	}
	if !strings.Contains(out, "the push was rejected 3 times") {
		t.Errorf("want the give-up message:\n%s", out)
	}
	for _, f := range e.files() {
		if strings.HasPrefix(f, "previews/pr-1/") {
			t.Errorf("%s reached gh-pages", f)
		}
	}
}

// GP06: a destination outside the branch's tree is refused before any
// clone.
func TestGP06DestinationMustStayInsideTheBranch(t *testing.T) {
	e := ghPagesNew(t)
	site := e.site(map[string]string{"index.html": "x\n"})
	for _, dest := range []string{"", "/abs", "../up", "previews/../.."} {
		out, code := e.run("", nil, "publish", site, dest, "docs: x")
		if code == 0 || !strings.Contains(out, "the destination must be a relative directory") {
			t.Errorf("destination %q: exit %d:\n%s", dest, code, out)
		}
	}
}
