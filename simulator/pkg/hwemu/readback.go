package hwemu

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// nodeRenderMTime is the modification time of every render-owned file the
// Node has checked. Any write moves the time to the present, so one lstat per
// step finds a changed file, and a coarse file system clock cannot hide a
// write made in the same tick as the check.
var nodeRenderMTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// nodeEmptyRestoreAge is how long an A file must have stayed empty since its
// last truncation before the readback puts the value back. A writer goes from
// truncate to write in microseconds; the margin covers file systems that keep
// modification times in whole seconds, so a fresh truncation is never taken
// for an old one.
const nodeEmptyRestoreAge = 2 * time.Second

// nodeAFile is one agent-writable file (A). handle parses what was written,
// with the one trailing newline the kernel accepts removed, applies it to the
// model and reports the event kind and the value in effect; content it rejects
// leaves the model unchanged. current is what the file shows for the model's
// state, the normalized readback. held is the content the file had when the
// Node last handled or wrote it, so any other content is a new write.
// emptyMTime is the modification time of the truncation already reported,
// when emptySeen. component is the hw.id of the component the file belongs
// to, which its events carry.
type nodeAFile struct {
	rel        string
	path       string
	component  string
	held       string
	emptySeen  bool
	emptyMTime time.Time
	handle     func(v string) (kind, effective string)
	current    func() string
}

// nodeRFile is one render-owned file (R) and the content it must keep.
// component is the hw.id its restored events carry (rfileComponents).
type nodeRFile struct {
	rel       string
	path      string
	component string
	mode      fs.FileMode
	content   []byte
	compare   bool // no sentinel time could be set, so compare content every step
}

// addAFile registers the agent-writable file at abs, which belongs to
// component (a hw.id such as cpu1 or gpu3, or "" for the whole node), and
// seeds the model from it: valid content takes effect as if it had just been
// written, so a Node started over a live tree keeps the limits in it.
// Content handle rejects leaves the profile value, and the first Step
// reverts the file with an event. It returns nil when the tree has no
// agent-writable file at abs.
func (n *Node) addAFile(abs, component string, handle func(string) (string, string), current func() string) *nodeAFile {
	if !n.agentWritable(abs) {
		return nil
	}
	if b, err := os.ReadFile(abs); err == nil && len(b) > 0 {
		handle(strings.TrimSuffix(string(b), "\n"))
	}
	f := &nodeAFile{rel: n.relPath(abs), path: abs, component: component, held: current(), handle: handle, current: current}
	n.afiles = append(n.afiles, f)
	return f
}

// agentWritable reports whether abs is an A file of the tree. Without a file
// list, as for a tree assembled by hand, any regular file qualifies.
func (n *Node) agentWritable(abs string) bool {
	if f, ok := n.owners[n.relPath(abs)]; ok {
		return f.Kind == layout.KindFile && f.Owner == layout.OwnerAgent
	}
	if len(n.owners) > 0 {
		return false
	}
	fi, err := os.Lstat(abs)
	return err == nil && fi.Mode().IsRegular()
}

// snapshotRenderFiles records the content of every R file and marks it with
// nodeRenderMTime. It runs after every model is built, so each file gets the
// component of its directory.
func (n *Node) snapshotRenderFiles() error {
	components := n.rfileComponents()
	for _, f := range n.tree.Files {
		if f.Kind != layout.KindFile || f.Owner != layout.OwnerRender {
			continue
		}
		abs := n.rootPath(f.Rel)
		fi, err := os.Lstat(abs)
		if err != nil {
			return fmt.Errorf("hwemu: render-owned %s: %w", f.Rel, err)
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("hwemu: render-owned %s: %w", f.Rel, err)
		}
		r := &nodeRFile{rel: f.Rel, path: abs, component: components[path.Dir(f.Rel)], mode: fi.Mode().Perm(), content: b}
		if os.Chtimes(abs, nodeRenderMTime, nodeRenderMTime) != nil {
			r.compare = true
		}
		n.rfiles = append(n.rfiles, r)
	}
	return nil
}

// rfileComponents maps a directory of the tree to the hw.id its files belong
// to, as addAFile names them: the directory of every A file (a policy, an
// HSMP or amdgpu hwmon device, a GPU's NVML state, and a control type with
// ""), every powercap zone, which has no A file when no constraint takes
// writes, and the PCI device of a GPU that has one. A file in any other
// directory, such as cpuinfo, is missing from the map, which gives "".
func (n *Node) rfileComponents() map[string]string {
	dirs := map[string]string{}
	for _, f := range n.afiles {
		dirs[path.Dir(f.rel)] = f.component
	}
	for _, z := range n.zones {
		dirs[n.relPath(n.sysPath(z.rel))] = z.component
	}
	for _, g := range n.gpus {
		if g.sysfsPath != "" {
			dirs[n.relPath(n.sysPath(g.sysfsPath))] = g.id()
		}
	}
	return dirs
}

// readback is step 1 of a Step: every A file is read and handled, then every
// file whose content is not the model's readback is rewritten, which
// normalizes a write and moves a value another write changed, as a
// scaling_max_freq below scaling_min_freq moves the minimum
// (cpufreq.c:2668-2681). With checkRender, changed R files are restored.
// The power in effect is then recomputed from the new inputs.
//
// A rewrite happens only while the file still holds what the Node handled
// (nodeRewriteInPlace). A write the agent made since stays, and the next
// readback handles it, even when its bytes equal the readback: the readback
// of a clamped request is not that request.
func (n *Node) readback(checkRender bool) error {
	var errs []error
	for _, f := range n.afiles {
		if err := n.readAFile(f); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range n.afiles {
		if c := f.current(); c != f.held && !f.emptySeen {
			ok, err := nodeRewriteInPlace(f.path, []byte(f.held), []byte(c), time.Time{}, n.leases)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				f.held = c
			}
		}
	}
	if checkRender {
		for _, r := range n.rfiles {
			if err := n.checkRFile(r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	n.derive()
	n.adopt(n.now)
	return errors.Join(errs...)
}

// readAFile reads one A file and hands new content to its handler; content
// equal to what the Node last handled or wrote is not new. readback then
// rewrites the file with the normalized value.
//
// Empty content is a writer caught between truncate and write (os.WriteFile,
// as applyRAPLPackageCap in cmd/agent writes): it keeps the previous value,
// is reported once per truncation, by its modification time, and is left
// alone. The value is put back only once the truncation is
// nodeEmptyRestoreAge old and nobody holds the file open: its writer gave up,
// and sysfs never shows an empty attribute.
//
// Fidelity gaps: the check and the rewrite run under a write lease
// (nodeLease) where the tree's file system has them, so an open that comes
// meanwhile waits for the rewrite and no write is lost. Where no lease can be
// had, they are separate system calls, and a write landing entirely between
// them, a complete open, truncate and write within about a microsecond, is
// still lost, where sysfs applies every write. A file emptied by a writer that
// never writes reads empty until the restore, where sysfs keeps showing the
// value. A writer that puts back the exact bytes the file holds is not seen.
// Exact write semantics need FUSE.
func (n *Node) readAFile(f *nodeAFile) error {
	b, mtime, err := nodeReadWithMTime(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	content := string(b)
	if content == f.held {
		f.emptySeen = false
		return nil
	}
	if content == "" {
		if !f.emptySeen || !mtime.Equal(f.emptyMTime) {
			f.emptySeen, f.emptyMTime = true, mtime
			_, eff := f.handle("")
			n.event(f.rel, f.component, EventParseError, "", eff)
		}
		if time.Since(mtime) < nodeEmptyRestoreAge {
			return nil
		}
		// Skipped when the file changed or is open: the next readback sees.
		c := f.current()
		ok, err := nodeRewriteInPlace(f.path, nil, []byte(c), mtime, n.leases)
		if ok {
			f.held, f.emptySeen = c, false
		}
		return err
	}
	f.emptySeen = false
	kind, eff := f.handle(strings.TrimSuffix(content, "\n"))
	n.event(f.rel, f.component, kind, content, eff)
	f.held = content
	return nil
}

// checkRFile restores an R file whose content changed, in place, with a
// restored event.
func (n *Node) checkRFile(r *nodeRFile) error {
	if !r.compare {
		fi, err := os.Lstat(r.path)
		if err == nil && fi.Mode().IsRegular() && fi.Size() == int64(len(r.content)) && fi.ModTime().Equal(nodeRenderMTime) {
			return nil
		}
	}
	b, err := os.ReadFile(r.path)
	if err == nil && bytes.Equal(b, r.content) {
		if !r.compare {
			_ = os.Chtimes(r.path, nodeRenderMTime, nodeRenderMTime)
		}
		return nil
	}
	if err := nodeWriteInPlace(r.path, r.content, r.mode); err != nil {
		return fmt.Errorf("hwemu: restore %s: %w", r.rel, err)
	}
	if !r.compare {
		_ = os.Chtimes(r.path, nodeRenderMTime, nodeRenderMTime)
	}
	n.event(r.rel, r.component, EventRestored, string(b), string(r.content))
	return nil
}

func (n *Node) event(file, component, kind, written, effective string) {
	n.events = append(n.events, Event{Time: n.now, File: file, Kind: kind, Written: nodeEventValue(written), Effective: nodeEventValue(effective), Component: component})
}

// nodeEventValue trims a file value for an event and shortens long ones, such
// as a restored cpuinfo.
func nodeEventValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 128 {
		return s[:128] + "..."
	}
	return s
}

// nodeKstrtou parses v as the kernel's kstrtou* functions do (lib/kstrtox.c):
// an optional '+', then digits of the base and nothing else. The caller has
// removed the one trailing newline the kernel accepts. Base 0 means 16 after
// "0x", 8 after a leading 0, and 10 otherwise.
func nodeKstrtou(v string, base, bits int) (uint64, bool) {
	v = strings.TrimPrefix(v, "+")
	if base == 0 {
		switch {
		case len(v) > 2 && v[0] == '0' && (v[1] == 'x' || v[1] == 'X'):
			base, v = 16, v[2:]
		case len(v) > 1 && v[0] == '0':
			base = 8
		default:
			base = 10
		}
	}
	// With an explicit base, ParseUint takes neither a sign nor underscores.
	u, err := strconv.ParseUint(v, base, bits)
	return u, err == nil
}

// nodeParseInt parses a signed decimal, as kstrtol with base 10 does.
func nodeParseInt(v string) (int64, bool) {
	x, err := strconv.ParseInt(v, 10, 64)
	return x, err == nil
}

func nodeItoa(v int64) string { return strconv.FormatInt(v, 10) }

// nodeBoolHandler is an attribute that takes 0 or 1, such as a powercap
// zone's enabled (sysfs-class-powercap ABI) or cpufreq boost. Another number
// is reverted, anything else is a parse error.
func nodeBoolHandler(dst *int) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		if v == "0" || v == "1" {
			*dst = int(v[0] - '0')
			return EventAccepted, v
		}
		if _, ok := nodeParseInt(v); ok {
			return EventRevertedOutOfRange, strconv.Itoa(*dst)
		}
		return EventParseError, strconv.Itoa(*dst)
	}
	return handle, func() string { return strconv.Itoa(*dst) + "\n" }
}

// nodeEnumHandler is an attribute that takes one word from a list, such as
// scaling_governor or energy_performance_preference. The first word is read,
// as the kernel's sscanf("%15s") does (cpufreq.c:802); an unlisted word is
// reverted. An empty list takes any word.
func nodeEnumHandler(dst *string, allowed []string) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		words := strings.Fields(v)
		if len(words) == 0 {
			return EventParseError, *dst
		}
		if len(allowed) > 0 && !slices.Contains(allowed, words[0]) {
			return EventRevertedOutOfRange, *dst
		}
		*dst = words[0]
		return EventAccepted, words[0]
	}
	return handle, func() string { return *dst + "\n" }
}

// limitHandler is constraint_N_power_limit_uw. The kernel stores whole power
// units and does not clamp (intel_rapl_common.c:558-563, 744), so the
// readback is QuantizeUW of the value and may sit above max_power_uw; the
// package enforces min(limit, max) (assumed: hardware clamp).
func (c *nodeConstraint) limitHandler(unitUW int64) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 63)
		if !ok {
			return EventParseError, nodeItoa(c.enforcedUW())
		}
		written := int64(u)
		c.limitUW = QuantizeUW(written, unitUW)
		kind := EventAccepted
		if c.limitUW != written {
			kind = EventQuantized
		}
		if c.enforcedUW() != c.limitUW {
			kind = EventClamped
		}
		return kind, nodeItoa(c.enforcedUW())
	}
	return handle, func() string { return nodeItoa(c.limitUW) + "\n" }
}

// windowHandler is constraint_N_time_window_us: any positive value is kept, and
// it is the settling time constant of PL1. The kernel's Y/F encoding of the
// window is not modelled (powercap.rst).
func (c *nodeConstraint) windowHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 63)
		if !ok {
			return EventParseError, nodeItoa(c.windowUS)
		}
		if u == 0 {
			return EventRevertedOutOfRange, nodeItoa(c.windowUS)
		}
		c.windowUS = int64(u)
		return EventAccepted, nodeItoa(c.windowUS)
	}
	return handle, func() string { return nodeItoa(c.windowUS) + "\n" }
}

// freqHandler is scaling_max_freq (high) or scaling_min_freq. The kernel parses
// with kstrtoul base 0 (cpufreq.c:741), clamps to the cpuinfo range, resolves
// a table driver to the highest entry at or below the maximum and the lowest
// at or above the minimum, then lowers the minimum to the maximum
// (cpufreq.c:735-750, 2668-2681, 474-490). Both files show the result.
func (p *nodePolicy) freqHandler(high bool) (func(string) (string, string), func() string) {
	eff := func() int64 {
		if high {
			return p.effMaxKHz()
		}
		return p.effMinKHz()
	}
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 0, 64)
		if !ok {
			return EventParseError, nodeItoa(eff())
		}
		written := int64(min(u, uint64(1<<63-1)))
		if high {
			p.reqMaxKHz = written
		} else {
			p.reqMinKHz = written
		}
		clamped := p.clampKHz(written)
		resolved := p.resolveKHz(clamped, high)
		got := eff()
		switch {
		case got == written:
			return EventAccepted, nodeItoa(got)
		case got == resolved && resolved != clamped:
			return EventResolved, nodeItoa(got)
		default:
			return EventClamped, nodeItoa(got)
		}
	}
	return handle, func() string { return nodeItoa(eff()) + "\n" }
}

// nodeSetspeedHandler is scaling_setspeed without the userspace governor: the
// write fails (cpufreq.c:901-925), modelled as a revert to <unsupported>.
func nodeSetspeedHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		if v == "" {
			return EventParseError, "<unsupported>"
		}
		return EventRevertedOutOfRange, "<unsupported>"
	}
	return handle, func() string { return "<unsupported>\n" }
}

// hsmpCapHandler is the HSMP hwmon power1_cap: uW, sent to the firmware in mW
// (hsmp/hwmon.c:34-38) and read back in mW times 1000 (hwmon.c:74). A value
// above power1_cap_max becomes the maximum (assumed: firmware clamp); a
// negative one is rejected.
func (h *nodeHSMP) capHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		x, ok := nodeParseInt(v)
		if !ok {
			return EventParseError, nodeItoa(h.capUW)
		}
		if x < 0 {
			return EventRevertedOutOfRange, nodeItoa(h.capUW)
		}
		uw := x / 1000 * 1000
		kind := EventAccepted
		if uw != x {
			kind = EventQuantized
		}
		if h.capMaxUW > 0 && uw > h.capMaxUW {
			uw, kind = h.capMaxUW, EventClamped
		}
		h.capUW = uw
		return kind, nodeItoa(uw)
	}
	return handle, func() string { return nodeItoa(h.capUW) + "\n" }
}

// amdgpuCapHandler is amdgpu power1_cap: decimal uW parsed with kstrtou32,
// truncated to whole watts, and refused outside [power1_cap_min,
// power1_cap_max] (amdgpu_pm.c:3469-3496; the refusal is EINVAL, assumed, and
// modelled as a revert).
func (g *nodeGPU) amdgpuCapHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 32)
		if !ok {
			return EventParseError, nodeItoa(g.limitRaw)
		}
		w := int64(u / 1000000)
		if float64(w) < g.minW || float64(w) > g.maxW {
			return EventRevertedOutOfRange, nodeItoa(g.limitRaw)
		}
		g.limitRaw = w * 1000000
		if g.limitRaw != int64(u) {
			return EventQuantized, nodeItoa(g.limitRaw)
		}
		return EventAccepted, nodeItoa(g.limitRaw)
	}
	return handle, func() string { return nodeItoa(g.limitRaw) + "\n" }
}

// nvmlLimitHandler is NVML state power_limit_mw, written by fakesmi after its
// own range check. The value is kept as written and enforced within
// [min, max], a defensive clamp.
func (g *nodeGPU) nvmlLimitHandler() (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		x, ok := nodeParseInt(v)
		if !ok {
			return EventParseError, nodeItoa(g.targetMW())
		}
		g.limitRaw = x
		if g.targetMW() != x {
			return EventClamped, nodeItoa(g.targetMW())
		}
		return EventAccepted, nodeItoa(x)
	}
	return handle, func() string { return nodeItoa(g.limitRaw) + "\n" }
}

// persistReset puts the limit of every NVIDIA GPU that has persistence mode
// Disabled and no client back to its default, as the driver does when it
// tears the GPU down with the last client gone (DR-34; assumed: a load of 0
// stands for no client). It runs after the readback, so a limit written to
// an idle GPU is accepted and then reset within the same step. The file is
// rewritten in place, as the readback rewrites it, and the reset is a
// reset-persistence-off event with the old limit as written and the default
// as effective.
func (n *Node) persistReset() error {
	var errs []error
	for _, g := range n.gpus {
		if !g.nvml || g.persistence != persistTextDisabled || n.gpuUtil(g.index) != 0 || g.limitRaw == g.defaultMW {
			continue
		}
		old := g.limitRaw
		g.limitRaw = g.defaultMW
		rel := path.Join(layout.StateDir, layout.NVMLGPUDir(g.index), "power_limit_mw")
		if f := g.limitFile; f != nil {
			rel = f.rel
			if c := f.current(); c != f.held && !f.emptySeen {
				ok, err := nodeRewriteInPlace(f.path, []byte(f.held), []byte(c), time.Time{}, n.leases)
				if err != nil {
					errs = append(errs, err)
				}
				if ok {
					f.held = c
				}
			}
		}
		n.event(rel, g.id(), EventResetPersistenceOff, nodeItoa(old), nodeItoa(g.limitRaw))
	}
	return errors.Join(errs...)
}
