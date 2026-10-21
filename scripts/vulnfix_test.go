package scripts

// Tests of vulnfix.sh against an osv-scanner JSON report and a govulncheck
// stream written here, in a throwaway repository with two Go modules and a
// requirements file. A fake go first on PATH logs each call with its
// directory and edits go.mod as go get would, so nothing reaches the
// network: the cases are a package fixed by two advisories, one of them
// listed twice, a package with no fix, a go get that fails and must be
// reverted, a standard library finding from each scanner, a second module,
// and a Python pin next to a requirement that is not one.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const vulnfixScript = "vulnfix.sh"

// vulnfixFakeGo is go get and go mod tidy, nothing else: get rewrites the
// module's require line to the asked version, fails for a module listed in
// $FAKE_GO_FAIL after changing go.mod and go.sum (as a partial run does),
// and every call is logged to $FAKE_GO_LOG as "<dir>: <args>".
const vulnfixFakeGo = `#!/usr/bin/env bash
set -u
echo "$PWD: $*" >>"$FAKE_GO_LOG"
case "$1 ${2:-}" in
"get "*)
  mod=${2%@*} ver=${2#*@}
  if [[ " ${FAKE_GO_FAIL:-} " == *" $mod "* ]]; then
    echo "broken" >>go.mod
    echo "broken" >>go.sum
    echo "go: $mod@$ver: conflicting requirements" >&2
    exit 1
  fi
  sed -i "s#^\(\s*\)$mod v[^ ]*#\1$mod $ver#" go.mod
  echo "$mod $ver h1:fake=" >>go.sum
  ;;
"mod tidy") ;;
*) echo "fake go: unexpected $*" >&2; exit 2 ;;
esac
`

// vulnfixOSV is the osv-scanner report: absolute paths for the root module,
// as the scan in the container writes them (ROOT is replaced), relative
// ones elsewhere.
const vulnfixOSV = `{"results": [
 {"source": {"path": "ROOT/go.mod", "type": "lockfile"}, "packages": [
  {"package": {"name": "example.com/fixable", "version": "1.0.0", "ecosystem": "Go"}, "vulnerabilities": [
   {"id": "GO-2026-0001", "affected": [
     {"package": {"name": "example.com/other", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "9.9.9"}]}]},
     {"package": {"name": "example.com/fixable", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "1.2.0"}]}]}]},
   {"id": "GO-2026-0002", "affected": [
     {"package": {"name": "example.com/fixable", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "1.10.0"}]}]}]},
   {"id": "GO-2026-0001", "affected": [
     {"package": {"name": "example.com/fixable", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "1.1.0"}]}]}]}]},
  {"package": {"name": "example.com/unfixable", "version": "1.0.0", "ecosystem": "Go"}, "vulnerabilities": [
   {"id": "GO-2026-0003", "affected": [{"package": {"name": "example.com/unfixable", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}]}]}]}]},
  {"package": {"name": "example.com/failing", "version": "1.0.0", "ecosystem": "Go"}, "vulnerabilities": [
   {"id": "GO-2026-0004", "affected": [{"package": {"name": "example.com/failing", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "2.0.0"}]}]}]}]},
  {"package": {"name": "stdlib", "version": "1.26.0", "ecosystem": "Go"}, "vulnerabilities": [
   {"id": "GO-2026-0005", "affected": [{"package": {"name": "stdlib", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "1.26.1"}]}]}]}]}]},
 {"source": {"path": "sub/go.mod", "type": "lockfile"}, "packages": [
  {"package": {"name": "example.com/fixable", "version": "1.0.0", "ecosystem": "Go"}, "vulnerabilities": [
   {"id": "GO-2026-0002", "affected": [{"package": {"name": "example.com/fixable", "ecosystem": "Go"}, "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "1.10.0"}]}]}]}]}]},
 {"source": {"path": "experiments/x/requirements.txt", "type": "lockfile"}, "packages": [
  {"package": {"name": "pandas", "version": "1.0.0", "ecosystem": "PyPI"}, "vulnerabilities": [
   {"id": "PYSEC-2026-1", "affected": [{"package": {"name": "pandas", "ecosystem": "PyPI"}, "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.5.3"}]}]}]}]},
  {"package": {"name": "numpy", "version": "1.0.0", "ecosystem": "PyPI"}, "vulnerabilities": [
   {"id": "PYSEC-2026-2", "affected": [{"package": {"name": "numpy", "ecosystem": "PyPI"}, "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "2.0.0"}]}]}]}]}]}
]}`

// vulnfixGovulncheck is a govulncheck JSON stream: a standard library
// finding the code reaches, one it does not (no function in the trace), and
// a module finding, which osv-scanner already covers.
const vulnfixGovulncheck = `{"config": {"protocol_version": "v1.0.0"}}
{"finding": {"osv": "GO-2026-0006", "fixed_version": "v1.26.2", "trace": [{"module": "stdlib", "version": "v1.26.0", "package": "crypto/x509", "function": "Verify"}]}}
{"finding": {"osv": "GO-2026-0007", "fixed_version": "v1.26.3", "trace": [{"module": "stdlib", "version": "v1.26.0", "package": "net/mail"}]}}
{"finding": {"osv": "GO-2026-0004", "fixed_version": "v2.0.0", "trace": [{"module": "example.com/failing", "version": "v1.0.0", "package": "example.com/failing", "function": "Do"}]}}
`

const vulnfixGoMod = `module example.com/root

go 1.26.0

require (
	example.com/failing v1.0.0
	example.com/fixable v1.0.0
	example.com/unfixable v1.0.0
)
`

const vulnfixRequirements = `PyYAML
pandas == 1.0.0  # analysis
numpy>=1.0.0
`

type vulnfixEnv struct {
	t                  *testing.T
	repo, bin, goLog   string
	osvPath, govulnRel string
}

func vulnfixNew(t *testing.T) *vulnfixEnv {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	root := t.TempDir()
	e := &vulnfixEnv{t: t, repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin"), goLog: filepath.Join(root, "go.log")}
	e.write(filepath.Join(e.repo, "go.mod"), vulnfixGoMod)
	e.write(filepath.Join(e.repo, "go.sum"), "example.com/fixable v1.0.0 h1:old=\n")
	e.write(filepath.Join(e.repo, "sub", "go.mod"), strings.Replace(vulnfixGoMod, "example.com/root", "example.com/sub", 1))
	e.write(filepath.Join(e.repo, "experiments", "x", "requirements.txt"), vulnfixRequirements)
	e.osvPath = filepath.Join(root, "osv.json")
	e.write(e.osvPath, strings.ReplaceAll(vulnfixOSV, "ROOT", e.repo))
	e.write(filepath.Join(root, "govulncheck-root.json"), vulnfixGovulncheck)
	e.govulnRel = ".=" + filepath.Join(root, "govulncheck-root.json")
	e.write(filepath.Join(e.bin, "go"), vulnfixFakeGo)
	if err := os.Chmod(filepath.Join(e.bin, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *vulnfixEnv) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vulnfixEnv) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.repo, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// run runs vulnfix.sh at the repository root with the fake go first on
// PATH and returns the report, its output and exit code.
func (e *vulnfixEnv) run(fail string, args ...string) (report, out string, code int) {
	e.t.Helper()
	script, err := filepath.Abs(vulnfixScript)
	if err != nil {
		e.t.Fatal(err)
	}
	reportPath := filepath.Join(e.t.TempDir(), "report.md")
	cmd := exec.Command("bash", append([]string{script, e.osvPath, reportPath}, args...)...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GO_LOG="+e.goLog, "FAKE_GO_FAIL="+fail)
	b, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	if r, err := os.ReadFile(reportPath); err == nil {
		report = string(r)
	}
	return report, string(b), code
}

func TestVF01FixesAppliesRevertsAndLists(t *testing.T) {
	e := vulnfixNew(t)
	report, out, code := e.run("example.com/failing", e.govulnRel)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	// The highest fix of the package's advisories, GO-2026-0001 counted once
	// with its highest fix, in each module that lists it.
	log, err := os.ReadFile(e.goLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{
		e.repo + ": get example.com/fixable@v1.10.0",
		e.repo + ": mod tidy",
		e.repo + ": get example.com/failing@v2.0.0",
		filepath.Join(e.repo, "sub") + ": get example.com/fixable@v1.10.0",
		filepath.Join(e.repo, "sub") + ": mod tidy",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("go calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	goMod := e.read("go.mod")
	for _, w := range []string{"example.com/fixable v1.10.0", "example.com/failing v1.0.0", "example.com/unfixable v1.0.0"} {
		if !strings.Contains(goMod, w) {
			t.Errorf("go.mod lacks %q:\n%s", w, goMod)
		}
	}
	// The failing package is reverted: the go get changed both files before
	// failing, and only the fixable package's line remains in go.sum.
	if strings.Contains(goMod, "broken") || strings.Contains(e.read("go.sum"), "broken") {
		t.Errorf("the failing go get was not reverted:\ngo.mod:\n%s\ngo.sum:\n%s", goMod, e.read("go.sum"))
	}
	if sum := e.read("go.sum"); !strings.Contains(sum, "example.com/fixable v1.10.0") {
		t.Errorf("go.sum lost the fixable package's bump:\n%s", sum)
	}
	if !strings.Contains(e.read("sub/go.mod"), "example.com/fixable v1.10.0") {
		t.Errorf("sub/go.mod was not bumped:\n%s", e.read("sub/go.mod"))
	}
	for _, leftover := range []string{"go.mod.vulnfix", "go.sum.vulnfix"} {
		if _, err := os.Stat(filepath.Join(e.repo, leftover)); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}

	// The plain pin is bumped in place, keeping its spacing and comment; the
	// other requirement is listed.
	req := e.read("experiments/x/requirements.txt")
	if !strings.Contains(req, "pandas == 1.5.3  # analysis") || !strings.Contains(req, "numpy>=1.0.0") {
		t.Errorf("requirements.txt:\n%s", req)
	}

	for _, w := range []string{
		"## Bumped",
		"| `go.mod` | `example.com/fixable` | 1.0.0 | 1.10.0 | [GO-2026-0001](https://osv.dev/vulnerability/GO-2026-0001), [GO-2026-0002](https://osv.dev/vulnerability/GO-2026-0002) |",
		"| `sub/go.mod` | `example.com/fixable` | 1.0.0 | 1.10.0 |",
		"| `experiments/x/requirements.txt` | `pandas` | 1.0.0 | 1.5.3 |",
		"## Needs a toolchain bump",
		"| `go.mod` | 1.26.0 | 1.26.2 | [GO-2026-0005](https://osv.dev/vulnerability/GO-2026-0005), [GO-2026-0006](https://osv.dev/vulnerability/GO-2026-0006) |",
		"## Not fixed",
		"| `go.mod` | `example.com/unfixable` | 1.0.0 | [GO-2026-0003](https://osv.dev/vulnerability/GO-2026-0003) | no fixed version |",
		"| `go.mod` | `example.com/failing` | 1.0.0 | [GO-2026-0004](https://osv.dev/vulnerability/GO-2026-0004) | `go get example.com/failing@v2.0.0` or `go mod tidy` failed; reverted |",
		"| `experiments/x/requirements.txt` | `numpy` | 1.0.0 | [PYSEC-2026-2](https://osv.dev/vulnerability/PYSEC-2026-2) | not a plain `numpy==1.0.0` line; fixed in 2.0.0 |",
	} {
		if !strings.Contains(report, w) {
			t.Errorf("report lacks %q:\n%s", w, report)
		}
	}
	// The unreachable standard library finding is not listed, and stdlib is
	// never passed to go get.
	if strings.Contains(report, "GO-2026-0007") || strings.Contains(string(log), "stdlib") {
		t.Errorf("an unreachable stdlib finding or a stdlib go get:\n%s\n%s", report, log)
	}
}

func TestVF02CleanScanWritesAReportAndTouchesNothing(t *testing.T) {
	e := vulnfixNew(t)
	e.write(e.osvPath, `{"results": []}`)
	report, out, code := e.run("")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(report, "No finding.") {
		t.Errorf("report:\n%s", report)
	}
	if _, err := os.Stat(e.goLog); err == nil {
		t.Error("go ran for a clean scan")
	}
	if e.read("go.mod") != vulnfixGoMod {
		t.Error("go.mod changed for a clean scan")
	}
}

func TestVF03BadInputFails(t *testing.T) {
	e := vulnfixNew(t)
	e.write(e.osvPath, "not json")
	if _, out, code := e.run(""); code != 1 || !strings.Contains(out, "is not an osv-scanner JSON report") {
		t.Errorf("exit %d, want 1 naming the report:\n%s", code, out)
	}
	e.write(e.osvPath, `{"results": []}`)
	if _, out, code := e.run("", "no-equals-sign"); code != 1 || !strings.Contains(out, "is not <module dir>=<govulncheck json>") {
		t.Errorf("exit %d, want 1 naming the argument:\n%s", code, out)
	}
}
