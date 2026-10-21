package contracts_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Joulie images are built by the Dockerfiles (the release workflow builds
// Dockerfile and simulator/Dockerfile) and by the Dagger module of CI. The golang image sets GOTOOLCHAIN=local and builds with its
// own release whatever go.mod asks for, so its tag must be the go.mod
// toolchain: otherwise a toolchain bump that fixes a vulnerability never
// reaches the shipped binaries. Every base image is named by digest, so a
// moved tag cannot change a shipped image without a diff in this repository.

// daggerLiterals holds every image the Dagger module of CI runs.
const daggerLiterals = "ci/src/joulie_ci/literals.py"

var (
	golangTagRE = regexp.MustCompile(`golang:([0-9][0-9.]*[0-9])`)
	digestRE    = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	fromRE      = regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	// daggerImageRE is an image constant of daggerLiterals, NAME_IMAGE = "...".
	daggerImageRE = regexp.MustCompile(`^([A-Z0-9_]+_IMAGE)\s*=\s*"([^"]+)"`)
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

// imageRef is a base image reference and where the repository names it.
type imageRef struct{ where, image string }

// simDistrolessBase returns DISTROLESS_STATIC of scripts/simcluster/versions.env:
// the simulated cluster builds its images on this base, at the paths the
// release images use, so it must be the release base too.
func simDistrolessBase(t *testing.T, root string) string {
	t.Helper()
	var simBase string
	lines(t, root, "scripts/simcluster/versions.env", func(_ int, line string) {
		if v, ok := strings.CutPrefix(line, "DISTROLESS_STATIC="); ok {
			simBase = v
		}
	})
	if simBase == "" {
		t.Fatal("scripts/simcluster/versions.env sets no DISTROLESS_STATIC")
	}
	return simBase
}

// baseImageRefs returns every base image of the Dockerfiles (stage names
// left out) and of the Dagger module.
func baseImageRefs(t *testing.T, root string) []imageRef {
	t.Helper()
	var refs []imageRef
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
				refs = append(refs, imageRef{rel + ":" + strconv.Itoa(n), m[1]})
			}
			if m[2] != "" {
				stages[strings.ToLower(m[2])] = true
			}
		})
	}
	if dockerfiles < 2 {
		t.Fatalf("found %d Dockerfiles, want at least Dockerfile and simulator/Dockerfile", dockerfiles)
	}
	// Every image the Dagger module runs is pinned by digest too, except
	// K3S_IMAGE: that one is versions.env's K3S_IMAGE, which the simulated
	// cluster pins by tag (TestM03K3sImageMatchesDagger in tests/simcluster).
	daggerImages := 0
	lines(t, root, daggerLiterals, func(n int, line string) {
		m := daggerImageRE.FindStringSubmatch(line)
		if m == nil {
			return
		}
		daggerImages++
		if m[1] != "K3S_IMAGE" {
			refs = append(refs, imageRef{daggerLiterals + ":" + strconv.Itoa(n), m[2]})
		}
	})
	if daggerImages == 0 {
		t.Fatalf("%s names no NAME_IMAGE constant; if the module's images moved, update daggerLiterals", daggerLiterals)
	}
	return refs
}

func TestImageBasesArePinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	simBase := simDistrolessBase(t, root)
	golang := map[string]string{}
	for _, r := range baseImageRefs(t, root) {
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

// imageIndexDir holds the raw multi-arch index of every pinned base image
// digest, as the registry serves it, named sha256-<hex>.json. The file's
// sha256 is the digest, so the index the build pulls is the one checked
// here, with no registry access.
const imageIndexDir = "tests/contracts/testdata/image-indexes"

// releasePlatforms are the platforms of every image the release workflow
// publishes; each base image must provide all of them.
var releasePlatforms = []string{"linux/amd64", "linux/arm64"}

// TestImageBasesAreMultiArchIndexes checks that every pinned base image of
// the release (the Dockerfiles and DISTROLESS_STATIC) names a multi-arch
// index, not the manifest of one platform, and that the index
// lists every release platform: a single-platform digest would build an
// image of another architecture, or fail, for the other platforms.
func TestImageBasesAreMultiArchIndexes(t *testing.T) {
	root := repoRoot(t)
	refs := append(baseImageRefs(t, root), imageRef{"scripts/simcluster/versions.env DISTROLESS_STATIC", simDistrolessBase(t, root)})
	checked := map[string]bool{}
	for _, r := range refs {
		// The Dagger module's own images run the checks on the runner's
		// platform and are no release base; its golang image is the
		// Dockerfiles' one, which TestImageBasesArePinnedByDigest holds equal.
		if strings.HasPrefix(r.where, daggerLiterals+":") {
			continue
		}
		image, digest, ok := strings.Cut(r.image, "@sha256:")
		if !ok || checked[digest] {
			continue // a missing digest is TestImageBasesArePinnedByDigest's to report
		}
		checked[digest] = true
		save := fmt.Sprintf("docker buildx imagetools inspect --raw %s@sha256:%s > %s/sha256-%s.json", image, digest, imageIndexDir, digest)
		raw, err := os.ReadFile(filepath.Join(root, imageIndexDir, "sha256-"+digest+".json"))
		if err != nil {
			t.Errorf("%s: no saved index for %s; save it with %s", r.where, r.image, save)
			continue
		}
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != digest {
			t.Errorf("%s: %s/sha256-%s.json hashes to sha256:%x, not its name; save it again, unmodified, with %s", r.where, imageIndexDir, digest, sum, save)
			continue
		}
		var index struct {
			MediaType string `json:"mediaType"`
			Manifests []struct {
				Platform struct {
					OS           string `json:"os"`
					Architecture string `json:"architecture"`
				} `json:"platform"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(raw, &index); err != nil {
			t.Errorf("%s: the saved index of %s: %v", r.where, r.image, err)
			continue
		}
		if index.MediaType != "application/vnd.oci.image.index.v1+json" && index.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
			t.Errorf("%s: %s is a %q, not a multi-arch index; pin the digest of the tag's index (docker buildx imagetools inspect %s)", r.where, r.image, index.MediaType, image)
			continue
		}
		have := map[string]bool{}
		for _, m := range index.Manifests {
			have[m.Platform.OS+"/"+m.Platform.Architecture] = true
		}
		for _, p := range releasePlatforms {
			if !have[p] {
				t.Errorf("%s: the index %s has no %s image", r.where, r.image, p)
			}
		}
	}
	if len(checked) < 2 {
		t.Fatalf("checked %d base image digests, want at least the golang builder and the distroless base", len(checked))
	}
}
