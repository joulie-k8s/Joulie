package scripts

// The assertions of verify-chart-renders.sh pipe a rendered document into
// grep under pipefail. grep -q exits at the first match; a writer with more
// left to write then dies of SIGPIPE, the pipeline fails with 141, and the
// assertion reports "no line matches" for a line that is there. It failed CI
// that way on documents a little larger than a pipe buffer.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const chartRendersScript = "verify-chart-renders.sh"

// shellFunction returns the definition of the bash function name in script:
// from "name() {" at the start of a line to the first "}" alone on a line.
func shellFunction(t *testing.T, script, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{\n.*?^\}\n`).FindString(script)
	if m == "" {
		t.Fatalf("%s defines no function %s", chartRendersScript, name)
	}
	return m
}

// TestVCR01AssertDocFindsALineOfADocumentLargerThanAPipe: assert_doc finds a
// line at the top of a document of 1 MiB, much more than a pipe holds, under
// the script's own shell options.
func TestVCR01AssertDocFindsALineOfADocumentLargerThanAPipe(t *testing.T) {
	src, err := os.ReadFile(chartRendersScript)
	if err != nil {
		t.Fatal(err)
	}
	script := string(src)
	work := t.TempDir()
	var doc strings.Builder
	doc.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n  first: line\n")
	for doc.Len() < 1<<20 {
		doc.WriteString("  filler: 0123456789012345678901234567890123456789012345678901234567890123456789\n")
	}
	if err := os.WriteFile(filepath.Join(work, "big.yaml"), []byte(doc.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := "set -u -o pipefail\nWORKDIR=" + work + "\nFAILURES=0\n" +
		shellFunction(t, script, "fail") + shellFunction(t, script, "doc") + shellFunction(t, script, "assert_doc") +
		`assert_doc big has ConfigMap big '^  first: line$' "the first line"
assert_doc big lacks ConfigMap big '^  absent: line$' "a line that is not there"
exit "${FAILURES}"
`
	out, err := exec.Command("bash", "-c", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("assert_doc on a 1 MiB document: %v\n%s", err, out)
	}
}

// TestVCR02NoGrepExitsEarlyOnAPipe: no grep in the script exits before it has
// read its whole input, which is what makes TestVCR01's case pass for every
// helper, not only assert_doc.
func TestVCR02NoGrepExitsEarlyOnAPipe(t *testing.T) {
	src, err := os.ReadFile(chartRendersScript)
	if err != nil {
		t.Fatal(err)
	}
	early := regexp.MustCompile(`\|\s*grep\s+(-[A-Za-z]*[qm][A-Za-z]*|--quiet|--silent|--max-count)`)
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if early.MatchString(line) {
			t.Errorf("%s:%d: a grep that stops reading a pipe early: %s", chartRendersScript, i+1, strings.TrimSpace(line))
		}
	}
}
