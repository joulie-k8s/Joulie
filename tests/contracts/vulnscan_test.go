package contracts_test

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The vulnerability scan pins its scanners once, in the Dagger module's
// literals.py, and names there every dependency manifest it reads. These
// tests keep the pins the only ones, the manifest list complete, the pull
// request gate and the nightly fix commit covering every manifest, and every
// action of every workflow pinned by commit.

const scanLiterals = "ci/src/joulie_ci/literals.py"

var (
	osvVersionRE     = regexp.MustCompile(`(?m)^OSV_SCANNER_VERSION = "([0-9]+\.[0-9]+\.[0-9]+)"$`)
	osvSHA256RE      = regexp.MustCompile(`(?m)^OSV_SCANNER_SHA256 = "([0-9a-f]{64})"$`)
	govulncheckRE    = regexp.MustCompile(`(?m)^GOVULNCHECK_VERSION = "(v[0-9]+\.[0-9]+\.[0-9]+)"$`)
	pyListRE         = func(name string) *regexp.Regexp { return regexp.MustCompile(`(?s)\n` + name + ` = \[(.*?)\n?\]`) }
	osvMentionRE     = regexp.MustCompile(`osv-scanner(?:_linux_amd64)?[ @/:=_-]*v?([0-9]+\.[0-9]+\.[0-9]+)`)
	govulnMentionRE  = regexp.MustCompile(`govulncheck@(v[0-9]+\.[0-9]+\.[0-9]+)`)
	sha256MentionRE  = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	actionPinRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_./-]+)?@[0-9a-f]{40}$`)
	requirementsName = regexp.MustCompile(`^requirements[^/]*\.txt$`)
)

// scanPins reads the pins and lists of literals.py.
func scanPins(t *testing.T, root string) (osvVersion, osvSHA, govulncheck string, manifests, modules []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, scanLiterals))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		re  *regexp.Regexp
		dst *string
	}{{osvVersionRE, &osvVersion}, {osvSHA256RE, &osvSHA}, {govulncheckRE, &govulncheck}} {
		m := p.re.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no %s", scanLiterals, p.re)
		}
		*p.dst = string(m[1])
	}
	list := func(name string) []string {
		m := pyListRE(name).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no %s list", scanLiterals, name)
		}
		var out []string
		for _, q := range quotedRE.FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
		return out
	}
	return osvVersion, osvSHA, govulncheck, list("VULN_MANIFESTS"), list("GOVULNCHECK_MODULES")
}

// TestScannerPinsLiveInLiteralsOnly: a version or checksum of osv-scanner or
// govulncheck named anywhere else in the workflows or the CI docs is the one
// literals.py pins, so a bump there is a bump everywhere.
func TestScannerPinsLiveInLiteralsOnly(t *testing.T) {
	root := repoRoot(t)
	osvVersion, osvSHA, govulncheck, _, _ := scanPins(t, root)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(root, "ci", "README.md"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		for _, m := range osvMentionRE.FindAllSubmatch(b, -1) {
			if string(m[1]) != osvVersion {
				t.Errorf("%s names osv-scanner %s; %s pins %s", rel, m[1], scanLiterals, osvVersion)
			}
		}
		for _, m := range govulnMentionRE.FindAllSubmatch(b, -1) {
			if string(m[1]) != govulncheck {
				t.Errorf("%s names govulncheck %s; %s pins %s", rel, m[1], scanLiterals, govulncheck)
			}
		}
		if strings.Contains(string(b), "osv-scanner") {
			for _, m := range sha256MentionRE.FindAll(b, -1) {
				if string(m) != osvSHA {
					t.Errorf("%s holds the checksum %s next to osv-scanner; %s pins %s", rel, m, scanLiterals, osvSHA)
				}
			}
		}
	}
}

// TestVulnscanCoversEveryManifest: VULN_MANIFESTS names every go.mod and
// requirements file of the repository, GOVULNCHECK_MODULES only Go modules
// of it, the pull request gate runs when any manifest or its go.sum
// changes, and the nightly fix commit takes every file the fixer may change.
func TestVulnscanCoversEveryManifest(t *testing.T) {
	root := repoRoot(t)
	_, _, _, manifests, modules := scanPins(t, root)

	var found []string
	skip := map[string]bool{".git": true, ".claude": true, "node_modules": true, "bin": true}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" || requirementsName.MatchString(d.Name()) {
			rel, _ := filepath.Rel(root, p)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	listed := append([]string(nil), manifests...)
	sort.Strings(listed)
	if strings.Join(found, " ") != strings.Join(listed, " ") {
		t.Errorf("%s VULN_MANIFESTS lists\n  %v\nbut the repository has\n  %v", scanLiterals, listed, found)
	}
	for _, m := range modules {
		if _, err := os.Stat(filepath.Join(root, m, "go.mod")); err != nil {
			t.Errorf("%s GOVULNCHECK_MODULES lists %q, which has no go.mod", scanLiterals, m)
		}
	}

	// The files a manifest change touches: the manifest, and the go.sum
	// beside a go.mod when there is one.
	var files []string
	for _, m := range manifests {
		files = append(files, m)
		if path.Base(m) == "go.mod" {
			if sum := path.Join(path.Dir(m), "go.sum"); fileExists(filepath.Join(root, sum)) {
				files = append(files, sum)
			}
		}
	}

	// YAML 1.1, which sigs.k8s.io/yaml follows, reads the key on as true.
	var gate struct {
		On map[string]struct {
			Paths []string `json:"paths"`
		} `json:"true"`
	}
	readYAML(t, filepath.Join(root, ".github", "workflows", "vulnscan.yml"), &gate)
	paths := gate.On["pull_request"].Paths
	if len(paths) == 0 {
		t.Fatal("vulnscan.yml: no on.pull_request.paths")
	}
	for _, f := range files {
		if !globsMatch(paths, f) {
			t.Errorf("vulnscan.yml: on.pull_request.paths %v lack %s", paths, f)
		}
	}

	var nightly struct {
		Jobs map[string]workflowJob `json:"jobs"`
	}
	readYAML(t, filepath.Join(root, ".github", "workflows", "vulnscan-nightly.yml"), &nightly)
	var addPaths []string
	for _, job := range nightly.Jobs {
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, "peter-evans/create-pull-request@") {
				addPaths = append(addPaths, strings.Fields(s.With["add-paths"])...)
			}
		}
	}
	if len(addPaths) == 0 {
		t.Fatal("vulnscan-nightly.yml opens no pull request with add-paths")
	}
	for _, f := range files {
		if !globsMatch(addPaths, f) {
			t.Errorf("vulnscan-nightly.yml: the fix commit's add-paths %v lack %s, so a fix of it would be dropped", addPaths, f)
		}
	}
}

// TestWorkflowActionsArePinnedByCommit: every action a workflow uses names
// a full commit SHA, so a moved tag cannot change what CI runs.
func TestWorkflowActionsArePinnedByCommit(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	for _, f := range files {
		var wf struct {
			Jobs map[string]workflowJob `json:"jobs"`
		}
		readYAML(t, f, &wf)
		for id, job := range wf.Jobs {
			for _, s := range job.Steps {
				if s.Uses == "" || strings.HasPrefix(s.Uses, "./") {
					continue
				}
				if !actionPinRE.MatchString(s.Uses) {
					t.Errorf("%s job %s: %q is not pinned by a full commit SHA", filepath.Base(f), id, s.Uses)
				}
			}
		}
	}
}

func readYAML(t *testing.T, file string, into interface{}) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

func globsMatch(globs []string, file string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, file); ok {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
