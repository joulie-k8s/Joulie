package contracts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

// The Krew manifest, the archives and the release form one contract: Krew
// downloads the uri of the user's platform, checks its sha256 and runs bin
// from inside it. krew-release-bot renders cmd/kubectl-joulie/krew.yaml on
// every release, and krew-index merges the bump unreviewed, so a platform
// the release does not build, an archive name the script does not write or
// a bin the archive does not hold reaches every Krew user as a failed
// install.

const krewTemplate = "cmd/kubectl-joulie/krew.yaml"

type krewPlugin struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version          string `json:"version"`
		ShortDescription string `json:"shortDescription"`
		Description      string `json:"description"`
		Platforms        []struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			URI    string `json:"uri"`
			SHA256 string `json:"sha256"`
			Bin    string `json:"bin"`
		} `json:"platforms"`
	} `json:"spec"`
}

// renderKrew renders the template as krew-release-bot v0.0.51 does
// (pkg/source/template.go): addURIAndSha renders the URL with the tag and
// writes "uri: <url>" and, four spaces in, "sha256: <sum>". sha returns
// the sum of a URL.
func renderKrew(t *testing.T, tag string, sha func(url string) string) krewPlugin {
	t.Helper()
	path := filepath.Join(repoRoot(t), krewTemplate)
	tpl, err := template.New(filepath.Base(path)).Funcs(template.FuncMap{
		"addURIAndSha": func(url, tag string) string {
			var b bytes.Buffer
			if err := template.Must(template.New("url").Parse(url)).Execute(&b, struct{ TagName string }{tag}); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("uri: %s\n    sha256: %s", b.String(), sha(b.String()))
		},
	}).ParseFiles(path)
	if err != nil {
		t.Fatalf("%s: %v", krewTemplate, err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, struct{ TagName string }{tag}); err != nil {
		t.Fatalf("%s: %v", krewTemplate, err)
	}
	var p krewPlugin
	if err := yaml.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("rendered %s: %v\n%s", krewTemplate, err, out.String())
	}
	return p
}

// pluginMatrix is the os/arch list of the release workflow's
// publish-kubectl-plugin job.
func pluginMatrix(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf releaseWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range wf.Jobs["publish-kubectl-plugin"].Strategy.Matrix.Include {
		out = append(out, m.GOOS+"/"+m.GOARCH)
	}
	slices.Sort(out)
	return out
}

// Every platform the release builds has a manifest entry and the reverse,
// each pointing at the archive scripts/kubectl-plugin-archive.sh writes for
// it and running the binary it packs; the texts fit what krew-index review
// asks for (a short description of at most 50 characters, a description
// wrapped at 80).
func TestKrewManifestMatchesTheReleaseArchives(t *testing.T) {
	const tag = "v1.2.3"
	p := renderKrew(t, tag, func(string) string { return strings.Repeat("0", 64) })
	if p.Metadata.Name != "joulie" || p.Spec.Version != tag {
		t.Errorf("name %q version %q, want joulie and %s", p.Metadata.Name, p.Spec.Version, tag)
	}
	if n := len(p.Spec.ShortDescription); n == 0 || n > 50 {
		t.Errorf("shortDescription is %d characters, want 1 to 50", n)
	}
	for _, line := range strings.Split(p.Spec.Description, "\n") {
		if len(line) > 80 {
			t.Errorf("description line over 80 characters: %q", line)
		}
	}
	var platforms []string
	for _, pl := range p.Spec.Platforms {
		goos, goarch := pl.Selector.MatchLabels["os"], pl.Selector.MatchLabels["arch"]
		platforms = append(platforms, goos+"/"+goarch)
		wantURI := fmt.Sprintf("https://github.com/joulie-k8s/Joulie/releases/download/%s/kubectl-joulie_%s_%s_%s.tar.gz", tag, tag, goos, goarch)
		if pl.URI != wantURI {
			t.Errorf("%s/%s: uri %s, want %s", goos, goarch, pl.URI, wantURI)
		}
		wantBin := "kubectl-joulie"
		if goos == "windows" {
			wantBin += ".exe"
		}
		if pl.Bin != wantBin {
			t.Errorf("%s/%s: bin %q, want %q", goos, goarch, pl.Bin, wantBin)
		}
	}
	slices.Sort(platforms)
	if want := pluginMatrix(t); !slices.Equal(platforms, want) {
		t.Errorf("%s platforms %v, release.yml publish-kubectl-plugin builds %v", krewTemplate, platforms, want)
	}
}

// The archive holds what the manifest's bin names, at its root, executable,
// and the LICENSE Krew requires; two builds give the same sha256, so a
// re-run release job cannot change a sum krew-index already holds.
func TestKrewArchiveHoldsTheBinaryAndLicense(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	root := repoRoot(t)
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	var sums []string
	for i := 0; i < 2; i++ {
		out := t.TempDir()
		cmd := exec.Command("bash", filepath.Join(root, "scripts", "kubectl-plugin-archive.sh"), "v1.2.3", "linux", "amd64", out)
		cmd.Env = append(os.Environ(), "GO="+goBin)
		stdout, err := cmd.Output()
		if err != nil {
			t.Fatalf("kubectl-plugin-archive.sh: %v\n%s", err, stdout)
		}
		archive := strings.TrimSpace(string(stdout))
		if want := filepath.Join(out, "kubectl-joulie_v1.2.3_linux_amd64.tar.gz"); archive != want {
			t.Fatalf("archive %s, want %s", archive, want)
		}
		if _, err := os.Stat(filepath.Join(out, "kubectl-joulie-linux-amd64")); err != nil {
			t.Errorf("no bare binary: %v", err)
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, fmt.Sprintf("%x", sha256.Sum256(data)))
		if i > 0 {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		entries := map[string]int64{}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			entries[h.Name] = h.Mode
		}
		if len(entries) != 2 || entries["LICENSE"] == 0 || entries["kubectl-joulie"]&0o111 == 0 {
			t.Errorf("archive entries %v, want an executable kubectl-joulie and LICENSE", entries)
		}
	}
	if sums[0] != sums[1] {
		t.Errorf("two builds give sha256 %s and %s", sums[0], sums[1])
	}
}

// The krew-index job runs the template the contract above checks, after
// the archives are attached, never for a pre-release, and only once the
// first version is in krew-index (the bot cannot submit it).
func TestReleaseWorkflowBumpsKrewIndexAfterTheArchives(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs interface{} `json:"needs"`
			If    string      `json:"if"`
			Steps []struct {
				Uses string            `json:"uses"`
				With map[string]string `json:"with"`
				Run  string            `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["krew-index"]
	if !ok {
		t.Fatal("release.yml has no krew-index job")
	}
	if fmt.Sprint(job.Needs) != "publish-kubectl-plugin" {
		t.Errorf("krew-index needs %v, want publish-kubectl-plugin", job.Needs)
	}
	for _, want := range []string{"vars.KREW_INDEX == 'true'", "!github.event.release.prerelease", "!contains(github.event.release.tag_name, '-')"} {
		if !strings.Contains(job.If, want) {
			t.Errorf("krew-index if %q lacks %q", job.If, want)
		}
	}
	bot := 0
	for _, s := range job.Steps {
		if strings.HasPrefix(s.Uses, "rajatjindal/krew-release-bot@") {
			bot++
			if s.With["krew_template_file"] != krewTemplate {
				t.Errorf("krew-release-bot reads %q, want %s", s.With["krew_template_file"], krewTemplate)
			}
		}
	}
	if bot != 1 {
		t.Errorf("krew-index runs krew-release-bot %d times, want once", bot)
	}
	builds := false
	for _, s := range wf.Jobs["publish-kubectl-plugin"].Steps {
		builds = builds || strings.Contains(s.Run, "scripts/kubectl-plugin-archive.sh")
	}
	if !builds {
		t.Error("publish-kubectl-plugin does not build with scripts/kubectl-plugin-archive.sh")
	}
}
