package hwemu

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// ensureMetaFile is state/hwemu.json, relative to the node root.
var ensureMetaFile = path.Join(layout.StateDir, "hwemu.json")

// ensureScratchPrefix names a directory under state/.tmp that held a whole
// fresh render, which a resume removes: a resume creates the entries the
// root lacks from the layout and needs no such directory.
const ensureScratchPrefix = "ensure-"

// Ensure makes root hold the tree of node o.NodeName of profile p, and says
// what it did, so a simulator restart keeps the tree an agent has mounted.
//
// An absent or empty root is rendered as Render renders it, with
// EnsureReport.Fresh set. Any other root is resumed:
//
//   - state/hwemu.json must name p, o.NodeName and the digest of p's values
//     (ensureProfileDigest), or Ensure returns ErrProfileChanged naming what
//     differs. A tree without a digest predates it and gets the same error.
//   - A root that is not a directory, or that has no readable
//     state/hwemu.json, is not a tree hwemu rendered: ErrTreeConflict.
//   - The profile is laid out afresh without writing (renderPlan), and every
//     entry of that layout the root lacks is created, a directory before
//     what it holds, and listed in EnsureReport.Added. Nothing the root has
//     is replaced, rewritten or touched: limits the agent wrote, counters
//     the emulator wrote and entries of its own, such as an energy ledger,
//     stay as they are.
//   - An entry of another kind than the render's (a directory for a file, a
//     file for a link, a link to another target) is ErrTreeConflict, found
//     before anything is created.
//
// The caller holds the root: no other Ensure, and no Node, runs on it
// meanwhile. With no agent mounting the node, removing its root and calling
// Ensure again recovers from either error.
func Ensure(p *Profile, root string, o RenderOptions) (*Tree, EnsureReport, error) {
	if p == nil {
		return nil, EnsureReport{}, errors.New("ensure: nil profile")
	}
	if o.NodeName == "" {
		return nil, EnsureReport{}, errors.New("ensure: RenderOptions.NodeName is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, EnsureReport{}, err
	}
	fresh, err := ensureIsFresh(abs)
	if err != nil {
		return nil, EnsureReport{}, err
	}
	if fresh {
		tree, err := Render(p, abs, o)
		if err != nil {
			return nil, EnsureReport{}, err
		}
		return tree, EnsureReport{Fresh: true}, nil
	}
	if err := p.Validate(); err != nil {
		return nil, EnsureReport{}, fmt.Errorf("ensure: %w", err)
	}
	if err := ensureCheckMeta(p, abs, o.NodeName); err != nil {
		return nil, EnsureReport{}, err
	}
	return ensureResume(p, abs, o)
}

// ensureIsFresh reports whether root is absent or an empty directory, which
// Render fills. Anything else but a non-empty directory is a conflict.
func ensureIsFresh(root string) (bool, error) {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%w: root %s is not a directory", ErrTreeConflict, root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// ensureCheckMeta compares state/hwemu.json with what Render would write for
// p and node.
func ensureCheckMeta(p *Profile, root, node string) error {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ensureMetaFile)))
	if err != nil {
		return fmt.Errorf("%w: %s has no readable %s, so hwemu did not render it: %v", ErrTreeConflict, root, ensureMetaFile, err)
	}
	var meta renderMeta
	if err := json.Unmarshal(b, &meta); err != nil || meta.SchemaVersion != 1 {
		return fmt.Errorf("%w: %s/%s is not a schemaVersion 1 render record: %q", ErrTreeConflict, root, ensureMetaFile, bytes.TrimSpace(b))
	}
	digest, err := ensureProfileDigest(p)
	if err != nil {
		return err
	}
	var diffs []string
	if meta.Profile != p.Name {
		diffs = append(diffs, fmt.Sprintf("profile %q, want %q", meta.Profile, p.Name))
	}
	if meta.Node != node {
		diffs = append(diffs, fmt.Sprintf("node %q, want %q", meta.Node, node))
	}
	if meta.ProfileDigest != digest {
		diffs = append(diffs, fmt.Sprintf("profileDigest %q, want %q", meta.ProfileDigest, digest))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("%w: %s/%s has %s", ErrProfileChanged, root, ensureMetaFile, strings.Join(diffs, "; "))
	}
	return nil
}

// ensureResume lays p out without writing, checks every entry the root
// already has against the layout, then creates the ones the root lacks.
func ensureResume(p *Profile, root string, o RenderOptions) (*Tree, EnsureReport, error) {
	plan, _, err := renderPlan(p, root, o)
	if err != nil {
		return nil, EnsureReport{}, err
	}
	var missing []layout.FileSpec
	var conflicts []string
	for _, f := range plan.files {
		live := filepath.Join(root, filepath.FromSlash(f.Rel))
		fi, err := os.Lstat(live)
		if errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, f)
			continue
		}
		if err != nil {
			return nil, EnsureReport{}, err
		}
		if c := ensureKindConflict(f, live, fi); c != "" {
			conflicts = append(conflicts, f.Rel+": "+c)
		}
	}
	if len(conflicts) > 0 {
		return nil, EnsureReport{}, fmt.Errorf("%w: %s: %s", ErrTreeConflict, root, strings.Join(conflicts, "; "))
	}
	// A scratch render a resume left there is no one's.
	tmpDir := filepath.Join(root, layout.StateDir, layout.TmpDir)
	if old, err := filepath.Glob(filepath.Join(tmpDir, ensureScratchPrefix+"*")); err == nil {
		for _, d := range old {
			_ = os.RemoveAll(d)
		}
	}

	var rep EnsureReport
	var umask fs.FileMode
	if len(missing) > 0 {
		if umask, err = renderUmask(root); err != nil {
			return nil, rep, err
		}
	}
	for _, f := range missing {
		if err := renderCreate(root, f, plan.content[f.Rel], umask); errors.Is(err, fs.ErrExist) {
			return nil, rep, fmt.Errorf("ensure %s: %s appeared during the resume; nothing live is replaced", root, f.Rel)
		} else if err != nil {
			return nil, rep, fmt.Errorf("ensure %s: %w", root, err)
		}
		rep.Added = append(rep.Added, f.Rel)
	}
	sort.Strings(rep.Added)
	return plan.tree(root, o), rep, nil
}

// ensureKindConflict says how the live entry at abs differs in kind from f,
// or "" when it is of f's kind.
func ensureKindConflict(f layout.FileSpec, abs string, fi fs.FileInfo) string {
	found := "a " + ensureKindName(fi.Mode())
	switch f.Kind {
	case layout.KindFile:
		if !fi.Mode().IsRegular() {
			return "want a file, found " + found
		}
	case layout.KindDir:
		if !fi.IsDir() {
			return "want a directory, found " + found
		}
	case layout.KindSymlink:
		if fi.Mode()&fs.ModeSymlink == 0 {
			return "want a link, found " + found
		}
		if target, err := os.Readlink(abs); err != nil || target != f.Target {
			return fmt.Sprintf("want a link to %s, found a link to %s", f.Target, target)
		}
	}
	return ""
}

func ensureKindName(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "link"
	}
	return "special file"
}

// ensureProfileDigest is the digest of p's values that state/hwemu.json
// records (ensureDigestJSON of p as JSON).
func ensureProfileDigest(p *Profile) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("profile digest: %w", err)
	}
	return ensureDigestJSON(b)
}

// ensureDigestJSON is sha256 over the profile JSON b with the provenance left
// out (sources, every src, extends) and the description too, since none of
// them changes what is rendered or simulated. Zero values are left out as
// well (ensurePruneZero), so a field added to the profile schema does not
// change the digest of a profile that leaves it unset, and a tree rendered
// before the field existed still resumes. Map keys marshal sorted, so the
// digest does not depend on map order.
func ensureDigestJSON(b []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("profile digest: %w", err)
	}
	delete(v, "sources")
	delete(v, "extends")
	delete(v, "description")
	ensureStripSrc(v)
	ensurePruneZero(v)
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("profile digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ensureStripSrc removes every src key, the provenance of a Fact or a label
// group, from a decoded JSON value.
func ensureStripSrc(x any) {
	switch t := x.(type) {
	case map[string]any:
		delete(t, "src")
		for _, e := range t {
			ensureStripSrc(e)
		}
	case []any:
		for _, e := range t {
			ensureStripSrc(e)
		}
	}
}

// ensurePruneZero removes from a decoded JSON value every object key whose
// value is null, "", 0 or false, or an object or array left empty, and says
// whether x itself is such a zero value. Go decodes an absent field and its
// zero value alike, so the pruned value still differs only where the values
// do. Array elements are kept, so their positions still count.
func ensurePruneZero(x any) bool {
	switch t := x.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case json.Number:
		f, err := t.Float64()
		return err == nil && f == 0
	case map[string]any:
		for k, e := range t {
			if ensurePruneZero(e) {
				delete(t, k)
			}
		}
		return len(t) == 0
	case []any:
		for _, e := range t {
			ensurePruneZero(e)
		}
		return len(t) == 0
	}
	return false
}
