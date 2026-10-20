package hwemu

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"sort"
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
// to, which its events carry. stamp is what the file showed when the
// readback last read it, by which the next readback skips a file nobody
// changed.
type nodeAFile struct {
	rel        string
	path       string
	component  string
	held       string
	emptySeen  bool
	emptyMTime time.Time
	stamp      nodeFileStamp
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
	if b, err := n.readFile(abs); err == nil && len(b) > 0 {
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
// nodeRenderMTime. A file of a laid-out tree (Tree.rendered) that still
// holds what the render wrote (nodeRenderedAsIs) is not read; any other is
// read, and what it holds is the content to keep. It runs after every model
// is built, so each file gets the component of its directory.
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
		r := &nodeRFile{rel: f.Rel, path: abs, component: components[path.Dir(f.Rel)], mode: fi.Mode().Perm()}
		n.rfiles = append(n.rfiles, r)
		if c, ok := n.tree.rendered[f.Rel]; ok && nodeRenderedAsIs(fi, c) {
			r.content = []byte(c)
			continue
		}
		if r.content, err = os.ReadFile(abs); err != nil {
			return fmt.Errorf("hwemu: render-owned %s: %w", f.Rel, err)
		}
		if os.Chtimes(abs, nodeRenderMTime, nodeRenderMTime) != nil {
			r.compare = true
		}
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

// readback is step 1 of a Step: every A file changed since its last read
// is read (readAFile), and those whose content changed are handled in the
// order they were written, by ascending modification time and, for equal
// times, in the order the Node registered them (scaling_governor before
// energy_performance_preference), because the kernel checks each write
// against the state the writes before it left.
// Then every file whose content is not the model's readback is rewritten,
// which normalizes a write and moves a value another write changed, as a
// scaling_max_freq below scaling_min_freq moves the minimum
// (cpufreq.c:2668-2681). With checkRender, changed R files are restored.
// The power in effect is then recomputed from the new inputs.
//
// A rewrite happens only while the file still holds what the Node handled
// (nodeRewriteInPlace). A write the agent made since stays, and the next
// readback handles it, even when its bytes equal the readback: the readback
// of a clamped request is not that request. What an A file shows changes
// only through a handler, so the files are compared with the model only
// after one ran (afilesDirty), and again while a rewrite is still due.
//
// Fidelity gap: a write lands in the file and takes effect at the next
// readback, so until then the file reads back the value written even when
// the driver would refuse it, and two writes to one file between readbacks
// count as the last. CheckWrite answers each write as the kernel would, for
// a writer that can call it, and a Live node's view answers every write so
// (LiveWrite): it reads back no file and checks no R file, since every write
// reached it through the view.
func (n *Node) readback(checkRender bool) error {
	var errs []error
	changed := n.changedBuf[:0]
	readAt := time.Now()
	for i, f := range n.afiles {
		if n.unobserved || n.live {
			break // nobody wrote the tree since the last step (StepUnobserved, Live)
		}
		w, err := n.readAFile(f, readAt)
		if err != nil {
			errs = append(errs, err)
		}
		if w.content != "" {
			w.order = i
			changed = append(changed, w)
		}
	}
	if len(changed) > 1 {
		sort.Slice(changed, func(i, j int) bool {
			if a, b := changed[i].mtime, changed[j].mtime; !a.Equal(b) {
				return a.Before(b)
			}
			return changed[i].order < changed[j].order
		})
	}
	for _, w := range changed {
		f := w.f
		kind, eff := f.handle(strings.TrimSuffix(w.content, "\n"))
		n.event(f.rel, f.component, kind, w.content, eff)
		f.held = w.content
	}
	if len(changed) > 0 {
		n.afilesDirty = true
	}
	clear(changed)
	n.changedBuf = changed[:0]
	if n.afilesDirty {
		errs = append(errs, n.normalizeAFiles())
	}
	if checkRender && !n.unobserved && !n.live {
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

// normalizeAFiles rewrites every A file whose content is not the model's
// readback, while the file still holds what the Node handled
// (nodeRewriteInPlace). A file it cannot rewrite yet leaves afilesDirty set,
// for the next readback.
func (n *Node) normalizeAFiles() error {
	n.afilesDirty = false
	var errs []error
	for _, f := range n.afiles {
		if c := f.current(); c != f.held && !f.emptySeen {
			ok, err := nodeRewriteInPlace(f.path, []byte(f.held), []byte(c), time.Time{}, n.leases)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				f.held = c
			} else {
				n.afilesDirty = true
			}
		}
	}
	return errors.Join(errs...)
}

// nodeAWrite is new content readback found in an A file: the file, what it
// holds, its modification time and the file's place in Node.afiles.
type nodeAWrite struct {
	f       *nodeAFile
	content string
	mtime   time.Time
	order   int
}

// readAFile reads one A file and returns new content for readback to hand
// to its handler, or a zero nodeAWrite; content equal to what the Node last
// handled or wrote is not new. readback then rewrites the file with the
// normalized value. A file whose stamp shows no change since the last read
// (nodeFileStamp, readAt taken before this readback's reads) is not read.
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
// A Live node's view has exact write semantics (LiveWrite).
func (n *Node) readAFile(f *nodeAFile, readAt time.Time) (nodeAWrite, error) {
	if !f.emptySeen && f.stamp.unchanged(f.path) {
		return nodeAWrite{}, nil
	}
	b, mtime, err := nodeReadA(f.path, &n.readBuf, &f.stamp, readAt)
	if errors.Is(err, fs.ErrNotExist) {
		return nodeAWrite{}, nil
	}
	if err != nil {
		return nodeAWrite{}, err
	}
	if string(b) == f.held {
		f.emptySeen = false
		return nodeAWrite{}, nil
	}
	content := string(b)
	if content == "" {
		if !f.emptySeen || !mtime.Equal(f.emptyMTime) {
			f.emptySeen, f.emptyMTime = true, mtime
			_, eff := f.handle("")
			n.event(f.rel, f.component, EventParseError, "", eff)
		}
		if time.Since(mtime) < nodeEmptyRestoreAge {
			return nodeAWrite{}, nil
		}
		// Skipped when the file changed or is open: the next readback sees.
		c := f.current()
		ok, err := nodeRewriteInPlace(f.path, nil, []byte(c), mtime, n.leases)
		if ok {
			f.held, f.emptySeen = c, false
		}
		return nodeAWrite{}, err
	}
	f.emptySeen = false
	return nodeAWrite{f: f, content: content, mtime: mtime}, nil
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

// nodeSscanfU parses v as the kernel's sscanf("%u") does (lib/vsprintf.c,
// vsscanf): spaces, then decimal digits up to the first other character,
// which ends the number. No digit is a parse error; so is a value above 32
// bits, which the kernel would truncate.
func nodeSscanfU(v string) (uint64, bool) {
	v = strings.TrimLeft(v, " \t\n\v\f\r")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	u, err := strconv.ParseUint(v[:end], 10, 32)
	return u, err == nil
}

// nodeKstrtobool parses v as the kernel's kstrtobool does
// (lib/kstrtox.c): the first character decides, y, Y, t, T, e, E or 1 for
// true and n, N, f, F, d, D or 0 for false, and o or O needs n or f after
// it. It returns 1 or 0.
func nodeKstrtobool(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	switch v[0] {
	case 'y', 'Y', 't', 'T', 'e', 'E', '1':
		return 1, true
	case 'n', 'N', 'f', 'F', 'd', 'D', '0':
		return 0, true
	case 'o', 'O':
		if len(v) > 1 {
			switch v[1] {
			case 'n', 'N':
				return 1, true
			case 'f', 'F':
				return 0, true
			}
		}
	}
	return 0, false
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

// nodeFlagHandler is an attribute that takes any unsigned number and keeps
// whether it is non-zero, such as intel_pstate/hwp_dynamic_boost
// (kstrtouint, then !!input; intel_pstate.c:1677-1690 in v6.12).
func nodeFlagHandler(dst *int) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 32)
		if !ok {
			return EventParseError, strconv.Itoa(*dst)
		}
		*dst = 0
		if u != 0 {
			*dst = 1
		}
		if u > 1 {
			return EventClamped, strconv.Itoa(*dst)
		}
		return EventAccepted, strconv.Itoa(*dst)
	}
	return handle, func() string { return strconv.Itoa(*dst) + "\n" }
}

// nodeEnumHandler is the attribute at abs that takes one word from a list,
// such as scaling_governor or energy_performance_preference. The first word
// is read, as the kernel's sscanf("%15s") does (cpufreq.c:802); an unlisted
// word is refused with EINVAL (store_scaling_governor, cpufreq.c:796-830;
// store_energy_performance_preference, intel_pstate.c:826-870). An empty
// list takes any word.
func (n *Node) nodeEnumHandler(abs string, dst *string, allowed []string) (func(string) (string, string), func() string) {
	rel := n.relPath(abs)
	handle := func(v string) (string, string) {
		words := strings.Fields(v)
		if len(words) == 0 {
			return EventParseError, *dst
		}
		if len(allowed) > 0 && !slices.Contains(allowed, words[0]) {
			return n.rejectWrite(rel, ErrnoEINVAL), *dst
		}
		*dst = words[0]
		return EventAccepted, words[0]
	}
	return handle, func() string { return *dst + "\n" }
}

// limitHandler is constraint_N_power_limit_uw at abs, parsed as kstrtoull
// (powercap_sys.c, store_constraint_power_limit_uw). The kernel converts
// the value to whole power units of unitUW, shifts it into the register
// field and masks it there (intel_rapl_common.c, rapl_write_data_raw), so
// the readback is the raw value times the unit: floored to the unit, and a
// value past the field wraps, 4100 W reading about 4 W in a 15-bit field of
// 1/8 W. It does not clamp, so the readback may sit above max_power_uw; the
// package enforces min(limit, max) (assumed: hardware clamp). A
// constraint that rejects writes refuses any number with ENODATA, the
// errno the store returns when the op fails, and keeps its limit.
func (n *Node) limitHandler(c *nodeConstraint, abs string, unitUW int64) (func(string) (string, string), func() string) {
	rel := n.relPath(abs)
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 64)
		if !ok {
			return EventParseError, nodeItoa(c.enforcedUW())
		}
		if c.rejectWrites != "" {
			if n.seeding {
				c.limitUW = int64(min(u, math.MaxInt64))
				return EventAccepted, nodeItoa(c.limitUW)
			}
			return n.rejectWrite(rel, ErrnoENODATA), nodeItoa(c.enforcedUW())
		}
		c.limitUW = nodeRAPLLimitUW(u, unitUW, c.mask)
		kind := EventAccepted
		if uint64(c.limitUW) != u {
			kind = EventQuantized
		}
		if c.enforcedUW() != c.limitUW {
			kind = EventClamped
		}
		return kind, nodeItoa(c.enforcedUW())
	}
	return handle, func() string { return nodeItoa(c.limitUW) + "\n" }
}

// nodeRAPLLimitUW is the limit a RAPL constraint holds after u uW is
// written, with a power unit of unitUW and a limit field of mask raw units
// (rapl_unit_xlate, rapl_write_data_raw): floor(u / unit), masked, times
// the unit. A unit of zero or less keeps u, and a mask of zero is no mask.
func nodeRAPLLimitUW(u uint64, unitUW, mask int64) int64 {
	if unitUW <= 0 {
		return int64(min(u, math.MaxInt64))
	}
	raw := u / uint64(unitUW)
	if mask > 0 {
		raw &= uint64(mask)
	}
	return int64(min(raw, uint64(math.MaxInt64/unitUW))) * unitUW
}

// windowHandler is constraint_N_time_window_us at abs: any positive value is
// kept, and it is the settling time constant of PL1. The kernel's Y/F
// encoding of the window is not modelled (powercap.rst). A constraint that
// rejects writes refuses any number with ENODATA, as limitHandler does.
func (n *Node) windowHandler(c *nodeConstraint, abs string) (func(string) (string, string), func() string) {
	rel := n.relPath(abs)
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 63)
		if !ok {
			return EventParseError, nodeItoa(c.windowUS)
		}
		if c.rejectWrites != "" && !n.seeding {
			return n.rejectWrite(rel, ErrnoENODATA), nodeItoa(c.windowUS)
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
// With the governor facts, nodeGovPolicy.setspeedHandler takes over and
// accepts a frequency under userspace.
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

// amdgpuCapHandler is amdgpu power1_cap at abs: decimal uW parsed with
// kstrtou32, truncated to whole watts, and refused outside
// [power1_cap_min, power1_cap_max] with EINVAL (amdgpu_pm.c:3469-3496; the
// errno is assumed).
func (n *Node) amdgpuCapHandler(g *nodeGPU, abs string) (func(string) (string, string), func() string) {
	rel := n.relPath(abs)
	handle := func(v string) (string, string) {
		u, ok := nodeKstrtou(v, 10, 32)
		if !ok {
			return EventParseError, nodeItoa(g.limitRaw)
		}
		w := int64(u / 1000000)
		if float64(w) < g.minW || float64(w) > g.maxW {
			return n.rejectWrite(rel, ErrnoEINVAL), nodeItoa(g.limitRaw)
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
// tears the GPU down with the last client gone (assumed: a load of 0 stands
// for no client). It runs after the readback, so a limit written to
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
