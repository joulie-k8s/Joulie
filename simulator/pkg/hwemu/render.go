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

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderSubstep is the substep recorded in state/hwemu.json: the Options
// default, since Render does not know the Options a Node will run with.
const renderSubstep = "100ms"

// renderMeta is state/hwemu.json.
type renderMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	Profile       string `json:"profile"`
	Node          string `json:"node"`
	Substep       string `json:"substep"`
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
	if p == nil {
		return nil, errors.New("render: nil profile")
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	if o.NodeName == "" {
		return nil, errors.New("render: RenderOptions.NodeName is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := renderCheckRoot(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	b := &renderBuilder{root: abs, kinds: map[string]layout.Kind{}}
	for _, step := range []func(*renderBuilder, *Profile, RenderOptions) error{
		renderBase,
		renderPowercap,
		renderCPUFreq,
		renderHSMP,
		renderPCIDevices,
		renderNVML,
		renderTools,
	} {
		if err := step(b, p, o); err != nil {
			return nil, fmt.Errorf("render %s: %w", p.Name, err)
		}
	}
	sort.Slice(b.files, func(i, j int) bool { return b.files[i].Rel < b.files[j].Rel })
	return &Tree{
		Root:        abs,
		SysRoot:     filepath.Join(abs, layout.SysDir),
		CPUInfoPath: filepath.Join(abs, layout.CPUInfoFile),
		StateRoot:   filepath.Join(abs, layout.StateDir),
		Files:       b.files,
	}, nil
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
	meta, err := json.Marshal(renderMeta{SchemaVersion: 1, Profile: p.Name, Node: o.NodeName, Substep: renderSubstep})
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

// renderBuilder creates a tree and records every entry it creates.
type renderBuilder struct {
	root  string
	files []layout.FileSpec
	kinds map[string]layout.Kind
}

func (b *renderBuilder) abs(rel string) string {
	return filepath.Join(b.root, filepath.FromSlash(rel))
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

// mkdir creates rel and every missing parent, recording each one.
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
	if err := os.Mkdir(b.abs(rel), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(b.abs(rel), 0o755); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindDir, Owner: layout.OwnerRender, Mode: 0o755})
	return nil
}

// file writes rel with content and sets its mode exactly, whatever the umask.
func (b *renderBuilder) file(rel, content string, owner layout.Owner, mode fs.FileMode) error {
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindFile); err != nil {
		return err
	}
	if err := os.WriteFile(b.abs(rel), []byte(content), mode); err != nil {
		return err
	}
	if err := os.Chmod(b.abs(rel), mode); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindFile, Owner: owner, Mode: mode})
	return nil
}

// link creates rel as a symlink to target.
func (b *renderBuilder) link(rel, target string) error {
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindSymlink); err != nil {
		return err
	}
	if err := os.Symlink(target, b.abs(rel)); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindSymlink, Owner: layout.OwnerRender, Mode: 0o777, Target: target})
	return nil
}

// rejectingDir creates rel as a directory in place of an attribute whose
// writes fail: os.WriteFile on it returns EISDIR.
func (b *renderBuilder) rejectingDir(rel string) error {
	if err := b.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	if err := b.claim(rel, layout.KindRejectingDir); err != nil {
		return err
	}
	if err := os.Mkdir(b.abs(rel), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(b.abs(rel), 0o755); err != nil {
		return err
	}
	b.files = append(b.files, layout.FileSpec{Rel: rel, Kind: layout.KindRejectingDir, Owner: layout.OwnerRender, Mode: 0o755})
	return nil
}

// renderEntry is one attribute of a directory before it is written.
type renderEntry struct {
	name      string
	content   string
	owner     layout.Owner
	mode      fs.FileMode
	rejecting bool
}

// writeEntries writes entries into dir in order.
func (b *renderBuilder) writeEntries(dir string, entries []renderEntry) error {
	if err := b.mkdir(dir); err != nil {
		return err
	}
	for _, e := range entries {
		rel := path.Join(dir, e.name)
		var err error
		if e.rejecting {
			err = b.rejectingDir(rel)
		} else {
			err = b.file(rel, e.content, e.owner, e.mode)
		}
		if err != nil {
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
