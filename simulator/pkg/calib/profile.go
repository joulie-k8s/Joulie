package calib

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// SourceID is the id of the measured source every fitted fact cites, as
// measured:sweep#<fit>.
const SourceID = "sweep"

// repoModule is the module path of this repository's go.mod. A directory
// whose go.mod declares it, or a module below it, is inside the repository.
const repoModule = "github.com/matbun/joulie"

var profileName = regexp.MustCompile(`^[a-z0-9-]+$`)

// LocalProfile renders a fit as a local hardware profile named name that
// extends the base profile. It holds only what the fit measured: a source
// sweep of kind measured, whose ref and note are ref and note, and each
// fitted fact with the provenance measured:sweep#<fit>. Everything else is
// inherited, so the profile loads only beside its base: a builtin, or a
// local profile in the same directory. Powers are rounded to 0.01 W,
// exponents to 0.001 and settling times to whole ms.
func LocalProfile(r *Result, name, ref, note string) ([]byte, error) {
	if !profileName.MatchString(name) {
		return nil, fmt.Errorf("profile name %q does not match [a-z0-9-]+", name)
	}
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("the sweep source needs a ref")
	}
	if !r.Fitted() {
		return nil, errors.New("the fit has no fitted fact to write")
	}
	physics := map[string]any{}
	for _, p := range r.Params {
		if !p.Fitted || !p.Written {
			continue
		}
		var v any
		switch p.Unit {
		case "ms":
			v = int(math.Round(p.Value))
		case "W":
			v = math.Round(p.Value*100) / 100
		default:
			v = math.Round(p.Value*1000) / 1000
		}
		keys := strings.Split(strings.TrimPrefix(p.Path, "physics."), ".")
		m := physics
		for _, k := range keys[:len(keys)-1] {
			next, ok := m[k].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[k] = next
			}
			m = next
		}
		m[keys[len(keys)-1]] = map[string]any{"v": v, "src": "measured:" + SourceID + "#" + p.Fit}
	}
	if r.cpuModel != "" {
		cpu, ok := physics["cpu"].(map[string]any)
		if !ok {
			cpu = map[string]any{}
			physics["cpu"] = cpu
		}
		cpu["model"] = r.cpuModel
	}
	src := map[string]any{"kind": "measured", "ref": ref}
	if note != "" {
		src["note"] = note
	}
	doc := map[string]any{
		"schemaVersion": 1,
		"name":          name,
		"description":   fmt.Sprintf("%s with physics fitted by hwcalib to a measured sweep", r.Base),
		"extends":       r.Base,
		"sources":       map[string]any{SourceID: src},
		"physics":       physics,
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	head := "# Written by hwcalib fit. A measured profile stays local, in SIM_PROFILES_DIR\n" +
		"# outside the repository: it may name the machine it was measured on.\n"
	return append([]byte(head), b...), nil
}

// WriteProfile writes profile b as dir/name.yaml and returns its path. It
// refuses a dir inside the repository, by its own path or by the one its
// links resolve to, and never replaces an existing file. dir is created
// when missing.
func WriteProfile(dir, name string, b []byte) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("no output directory")
	}
	if !profileName.MatchString(name) {
		return "", fmt.Errorf("profile name %q does not match [a-z0-9-]+", name)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if root, in, err := InRepository(abs); err != nil {
		return "", err
	} else if in {
		return "", fmt.Errorf("refusing to write into %s: it is inside the repository at %s, and a measured profile stays outside it (SIM_PROFILES_DIR)", abs, root)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(abs, name+".yaml")
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s exists; remove it or pick another name", path)
		}
		return "", err
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		os.Remove(path)
		return "", err
	}
	if err := fh.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// InRepository reports whether path is inside this repository: whether it,
// or the path its existing part resolves to through links, has an ancestor
// whose go.mod declares the repository's module. root is that ancestor. The
// path need not exist.
func InRepository(path string) (root string, in bool, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	candidates := []string{filepath.Clean(abs)}
	existing := abs
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	if real, err := filepath.EvalSymlinks(existing); err == nil {
		candidates = append(candidates, filepath.Join(append([]string{real}, rest...)...))
	}
	for _, c := range candidates {
		for dir := c; ; dir = filepath.Dir(dir) {
			if repoGoMod(filepath.Join(dir, "go.mod")) {
				return dir, true, nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return "", false, nil
}

// repoGoMod reports whether the go.mod at path declares the repository's
// module or one below it.
func repoGoMod(path string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if mod, ok := strings.CutPrefix(line, "module"); ok && (mod == "" || mod[0] == ' ' || mod[0] == '\t') {
			mod = strings.Trim(strings.TrimSpace(mod), `"`)
			return mod == repoModule || strings.HasPrefix(mod, repoModule+"/")
		}
	}
	return false
}
