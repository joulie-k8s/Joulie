package contracts_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Joulie images are built by the Dockerfiles (the release workflow builds
// Dockerfile and simulator/Dockerfile) and by the Dagger module of the
// integration run. The golang image sets GOTOOLCHAIN=local and builds with its
// own release whatever go.mod asks for, so its tag must be the go.mod
// toolchain: otherwise a toolchain bump that fixes a vulnerability never
// reaches the shipped binaries. Every base image is named by digest, so a
// moved tag cannot change a shipped image without a diff in this repository.

const daggerModule = "ci/src/joulie_ci/__init__.py"

var (
	golangTagRE = regexp.MustCompile(`golang:([0-9][0-9.]*[0-9])`)
	digestRE    = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	fromRE      = regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	daggerRefRE = regexp.MustCompile(`"((?:golang:|gcr\.io/distroless/)[^"]*)"`)
)

// goModToolchain returns the release of the go.mod toolchain line, without
// the "go" prefix.
func goModToolchain(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "toolchain" {
			return strings.TrimPrefix(f[1], "go")
		}
	}
	t.Fatal("go.mod has no toolchain line; the golang images follow it")
	return ""
}

// repoTextFiles returns the repository files, relative to root, that can name
// an image: Dockerfiles, docs, scripts, workflows and the Dagger module. It
// skips version control, worktrees, dependencies and build output.
func repoTextFiles(t *testing.T, root string) []string {
	t.Helper()
	skipNames := map[string]bool{".git": true, ".claude": true, "node_modules": true}
	skipPaths := map[string]bool{"bin": true, "website/public": true, "website/resources": true}
	exts := map[string]bool{".md": true, ".py": true, ".sh": true, ".yml": true, ".yaml": true, ".env": true, ".mk": true, ".toml": true}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipNames[d.Name()] || skipPaths[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if isDockerfile(d.Name()) || d.Name() == "Makefile" || exts[filepath.Ext(d.Name())] {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func isDockerfile(name string) bool {
	return name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile")
}

// lines calls fn with each line of a repository file and its 1-based number.
func lines(t *testing.T, root, rel string, fn func(n int, line string)) {
	t.Helper()
	f, err := os.Open(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		fn(n, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

func TestGolangImagesAreTheGoModToolchain(t *testing.T) {
	root := repoRoot(t)
	want := goModToolchain(t, root)
	found := 0
	for _, rel := range repoTextFiles(t, root) {
		lines(t, root, rel, func(n int, line string) {
			for _, m := range golangTagRE.FindAllStringSubmatch(line, -1) {
				found++
				if m[1] != want {
					t.Errorf("%s:%d names golang:%s, but go.mod names toolchain go%s; change it to golang:%s, and an image reference's digest with it (docker buildx imagetools inspect golang:%s)", rel, n, m[1], want, want, want)
				}
			}
		})
	}
	if found == 0 {
		t.Fatal("no golang:<version> found in the repository; if the Dockerfiles moved, update repoTextFiles")
	}
}

func TestImageBasesArePinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	// The simulated cluster builds its images on this base, at the paths the
	// release images use, so it must be the release base too.
	var simBase string
	lines(t, root, "scripts/simcluster/versions.env", func(_ int, line string) {
		if v, ok := strings.CutPrefix(line, "DISTROLESS_STATIC="); ok {
			simBase = v
		}
	})
	if simBase == "" {
		t.Fatal("scripts/simcluster/versions.env sets no DISTROLESS_STATIC")
	}

	type ref struct{ where, image string }
	var refs []ref
	dockerfiles := 0
	for _, rel := range repoTextFiles(t, root) {
		if !isDockerfile(filepath.Base(rel)) {
			continue
		}
		dockerfiles++
		stages := map[string]bool{}
		lines(t, root, rel, func(n int, line string) {
			m := fromRE.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				return
			}
			if !stages[strings.ToLower(m[1])] {
				refs = append(refs, ref{rel + ":" + strconv.Itoa(n), m[1]})
			}
			if m[2] != "" {
				stages[strings.ToLower(m[2])] = true
			}
		})
	}
	if dockerfiles < 2 {
		t.Fatalf("found %d Dockerfiles, want at least Dockerfile and simulator/Dockerfile", dockerfiles)
	}
	lines(t, root, daggerModule, func(n int, line string) {
		for _, m := range daggerRefRE.FindAllStringSubmatch(line, -1) {
			refs = append(refs, ref{daggerModule + ":" + strconv.Itoa(n), m[1]})
		}
	})

	golang := map[string]string{}
	for _, r := range refs {
		if !digestRE.MatchString(r.image) {
			t.Errorf("%s: base image %s has no digest; name it as <image>:<tag>@sha256:<digest> (docker buildx imagetools inspect <image>:<tag>)", r.where, r.image)
		}
		switch {
		case strings.HasPrefix(r.image, "gcr.io/distroless/static:") && r.image != simBase:
			t.Errorf("%s: base image %s differs from DISTROLESS_STATIC=%s in scripts/simcluster/versions.env", r.where, r.image, simBase)
		case strings.HasPrefix(r.image, "golang:"):
			golang[r.image] = r.where
		}
	}
	if len(golang) > 1 {
		t.Errorf("the golang builder differs between files: %v", golang)
	}
}
