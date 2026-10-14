package hwemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderSubstep is the substep recorded in state/hwemu.json: the Options
// default, since Render does not know the Options a Node will run with.
const renderSubstep = "100ms"

// renderMeta is state/hwemu.json. ProfileDigest is ensureProfileDigest of
// the profile, which Ensure compares before it resumes the tree.
type renderMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	Profile       string `json:"profile"`
	Node          string `json:"node"`
	Substep       string `json:"substep"`
	ProfileDigest string `json:"profileDigest"`
}

// Render writes the tree of one node of profile p under root: sys/ (mounted
// at /host-sys), cpuinfo (over /proc/cpuinfo), state/ (at /emu/state), bin/
// (at /emu/bin) and, on NVIDIA profiles, dev/nvidiactl. root must be absent
// or empty, so Render can never write into a live tree.
//
// o.NodeName is required. o.EnergyStartUJ sets the starting energy_uj of a
// powercap zone by its class name, such as "intel-rapl:0" or
// "intel-rapl:0:0". A zone not listed starts at a value derived from o.Seed
// and its name, or at 0 when o.Seed is 0; a corpus zone starts at its
// captured value.
func Render(p *Profile, root string, o RenderOptions) (*Tree, error) {
	b, abs, err := renderPlan(p, root, o)
	if err != nil {
		return nil, err
	}
	if err := renderCheckRoot(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	umask, err := renderUmask(abs)
	if err != nil {
		return nil, err
	}
	for _, f := range b.files {
		if err := renderCreate(abs, f, b.content[f.Rel], umask); err != nil {
			return nil, fmt.Errorf("render %s: %w", p.Name, err)
		}
	}
	return b.tree(abs, o), nil
}

// renderPlan lays out the tree of node o.NodeName of profile p under root
// without writing anything: every entry, sorted by path, so a directory
// comes before what it holds, with the content of every file. It returns
// root made absolute.
func renderPlan(p *Profile, root string, o RenderOptions) (*renderBuilder, string, error) {
	if p == nil {
		return nil, "", errors.New("render: nil profile")
	}
	if err := p.Validate(); err != nil {
		return nil, "", fmt.Errorf("render: %w", err)
	}
	if o.NodeName == "" {
		return nil, "", errors.New("render: RenderOptions.NodeName is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	b := &renderBuilder{kinds: map[string]layout.Kind{}, content: map[string]string{}}
	for _, step := range []func(*renderBuilder, *Profile, RenderOptions) error{
		renderBase,
		renderPowercap,
		renderCPUFreq,
		renderCPUGovernor,
		renderHSMP,
		renderPCIDevices,
		renderGPUDPM,
		renderNVML,
		renderNVClock,
		renderTools,
	} {
		if err := step(b, p, o); err != nil {
			return nil, "", fmt.Errorf("render %s: %w", p.Name, err)
		}
	}
	sort.Slice(b.files, func(i, j int) bool { return b.files[i].Rel < b.files[j].Rel })
	return b, abs, nil
}

// tree is the Tree of the plan b rendered at root, with the content of
// every file, which NewNode takes without reading a file back while it
// still holds what the render wrote (nodeRenderedAsIs).
func (b *renderBuilder) tree(root string, o RenderOptions) *Tree {
	rendered := make(map[string]string, len(b.content))
	for rel, c := range b.content {
		rendered[rel] = c
	}
	return &Tree{
		Root:        root,
		NodeName:    o.NodeName,
		SysRoot:     filepath.Join(root, layout.SysDir),
		CPUInfoPath: filepath.Join(root, layout.CPUInfoFile),
		StateRoot:   filepath.Join(root, layout.StateDir),
		Files:       b.files,
		rendered:    rendered,
	}
}

// renderUmask is the process's file mode creation mask: the Umask line of
// /proc/self/status where the kernel has one (Linux 4.7 and later), else
// learnt from a file created with every permission bit in dir and removed,
// the bits the kernel took away. A created entry whose mode has one of them
// needs a chmod; any other is right as created.
func renderUmask(dir string) (fs.FileMode, error) {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Umask:"); ok {
				if m, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err == nil {
					return fs.FileMode(m) & fs.ModePerm, nil
				}
			}
		}
	}
	probe := filepath.Join(dir, ".hwemu-umask-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL|nodeNoFollow, 0o777)
	if err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	_ = f.Close()
	_ = os.Remove(probe)
	if err != nil {
		return 0, err
	}
	return 0o777 &^ fi.Mode().Perm(), nil
}

// renderCreate creates entry f of a plan under root: a directory, a file
// with content, created exclusively and never through a link, or a link.
// The mode is set exactly, with one chmod only where umask took a bit of
// it, and a file gets nodeRenderMTime, the mark NewNode gives render-owned
// files, so a Node over a fresh tree reads none of them back
// (nodeRenderedAsIs). A path that exists already is an error that matches
// fs.ErrExist.
func renderCreate(root string, f layout.FileSpec, content string, umask fs.FileMode) error {
	abs := filepath.Join(root, filepath.FromSlash(f.Rel))
	switch f.Kind {
	case layout.KindDir:
		if err := os.Mkdir(abs, 0o755); err != nil {
			return err
		}
		if umask&0o755 != 0 {
			return os.Chmod(abs, 0o755)
		}
		return nil
	case layout.KindSymlink:
		return os.Symlink(f.Target, abs)
	}
	w, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL|nodeNoFollow, f.Mode)
	if err != nil {
		return err
	}
	_, werr := w.WriteString(content)
	if werr == nil && umask&f.Mode != 0 {
		werr = w.Chmod(f.Mode)
	}
	if err := errors.Join(werr, w.Close()); err != nil {
		return err
	}
	return os.Chtimes(abs, nodeRenderMTime, nodeRenderMTime)
}

// renderCheckRoot refuses a root that exists and is not an empty directory.
func renderCheckRoot(root string) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("render: root %s exists and is not a directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("render: root %s is not empty; Render never writes into an existing tree", root)
	}
	return nil
}

// renderBase writes what every family has: the top-level directories,
// cpuinfo, state/hwemu.json and, on NVIDIA profiles, dev/nvidiactl.
func renderBase(b *renderBuilder, p *Profile, o RenderOptions) error {
	for _, d := range []string{layout.SysDir, layout.StateDir, path.Join(layout.StateDir, layout.TmpDir), layout.BinDir} {
		if err := b.mkdir(d); err != nil {
			return err
		}
	}
	if err := b.file(layout.CPUInfoFile, renderCPUInfo(p), layout.OwnerRender, 0o444); err != nil {
		return err
	}
	digest, err := ensureProfileDigest(p)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(renderMeta{SchemaVersion: 1, Profile: p.Name, Node: o.NodeName, Substep: renderSubstep, ProfileDigest: digest})
	if err != nil {
		return err
	}
	if err := b.file(path.Join(layout.StateDir, "hwemu.json"), string(meta)+"\n", layout.OwnerRender, 0o444); err != nil {
		return err
	}
	if p.GPUs != nil && p.GPUs.Vendor == "nvidia" {
		// The agent only stats it (detectGPUVendor in cmd/agent); a
		// privileged pod on a real NVIDIA node has the device.
		if err := b.file(layout.DevNvidiactl, "", layout.OwnerRender, 0o666); err != nil {
			return err
		}
	}
	return nil
}

// renderSys is rel, a path relative to sys/ as layout returns it, relative to
// the node root.
func renderSys(rel string) string {
	return path.Join(layout.SysDir, rel)
}

// renderBuilder lays out a tree: every entry the render steps ask for, in
// files, with the content of each file in content; renderCreate writes
// them. claim refuses an entry rendered twice.
type renderBuilder struct {
	files   []layout.FileSpec
	kinds   map[string]layout.Kind
	content map[string]string
}

func (b *renderBuilder) claim(rel string, k layout.Kind) error {
	if old, ok := b.kinds[rel]; ok {
		if old == layout.KindDir && k == layout.KindDir {
			return nil
		}
		return fmt.Errorf("%s is rendered twice", rel)
	}
	b.kinds[rel] = k
	return nil
}

// mkdir lays out rel and every missing parent, recording each one.
func (b *renderBuilder) mkdir(rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	if k, ok := b.kinds[rel]; ok {
		if k != layout.KindDir {
			return fmt.Errorf("%s is rendered as a directory and as something else", rel)
		}
		return nil
	}
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindDir); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindDir, Owner: layout.OwnerRender, Mode: 0o755})
	return nil
}

// file lays out rel with content and mode, which renderCreate sets exactly,
// whatever the umask.
func (b *renderBuilder) file(rel, content string, owner layout.Owner, mode fs.FileMode) error {
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindFile); err != nil {
		return err
	}
	b.content[rel] = content
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindFile, Owner: owner, Mode: mode})
	return nil
}

// link lays out rel as a symlink to target.
func (b *renderBuilder) link(rel, target string) error {
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindSymlink); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindSymlink, Owner: layout.OwnerRender, Mode: 0o777, Target: target})
	return nil
}

// renderEntry is one attribute of a directory before it is written.
type renderEntry struct {
	name    string
	content string
	owner   layout.Owner
	mode    fs.FileMode
}

// writeEntries writes entries into dir in order.
func (b *renderBuilder) writeEntries(dir string, entries []renderEntry) error {
	if err := b.mkdir(dir); err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.file(path.Join(dir, e.name), e.content, e.owner, e.mode); err != nil {
			return err
		}
	}
	return nil
}

// renderUint formats a sysfs integer: the value and a newline.
func renderUint[T ~int | ~int64](v T) string {
	return strconv.FormatInt(int64(v), 10) + "\n"
}

// renderSeedEnergy is the starting energy_uj of a zone that RenderOptions
// does not set: 0 for seed 0, otherwise a whole number of energy units below
// the 32-bit counter span, derived from the seed and the zone name, so the
// value the counter starts from is one the hardware could hold.
func renderSeedEnergy(seed uint64, zone string, unitNJ int64) int64 {
	if seed == 0 || unitNJ <= 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(zone))
	x := h.Sum64() ^ seed
	// splitmix64 finalizer
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	raw := int64(x & 0xffffffff)
	return raw * unitNJ / 1000
}
