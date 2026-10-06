package hwemu

import (
	"errors"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// nodeOneEvent returns the single event of the last step on rel.
func nodeOneEvent(t *testing.T, n *Node, rel string) Event {
	t.Helper()
	ev := nodeEvents(n, rel)
	if len(ev) != 1 {
		t.Fatalf("events on %s = %+v, want exactly one", rel, ev)
	}
	return ev[0]
}

// H07: a RAPL limit reads back floored to the power unit and is never clamped
// to max_power_uw, while the package enforces the maximum.
func TestH07RAPLLimitQuantizedNotClamped(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)

	nodeWrite(t, tr, pl1, "120000001")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, pl1); got != "120000000\n" {
		t.Errorf("120000001 reads back %q, want 120000000", got)
	}
	if e := nodeOneEvent(t, n, pl1); e.Kind != "quantized" || e.Written != "120000001" || e.Effective != "120000000" {
		t.Errorf("event = %+v, want quantized 120000001 to 120000000", e)
	}

	nodeWrite(t, tr, pl1, "200000001")
	nodeStep(t, n, 10*time.Second)
	if got := nodeRead(t, tr, pl1); got != "200000000\n" {
		t.Errorf("a write above max reads back %q, want 200000000, quantized and unclamped", got)
	}
	if e := nodeOneEvent(t, n, pl1); e.Kind != "clamped" || e.Effective != "165000000" {
		t.Errorf("event = %+v, want clamped, enforced at 165000000", e)
	}
	pk := n.State().Packages[0]
	if pk.RequestedCapW != 200 || math.Abs(pk.CapW-165) > 0.01 {
		t.Errorf("requested %v W, cap %v W; want 200 W requested, enforced at the 165 W maximum", pk.RequestedCapW, pk.CapW)
	}

	nodeWrite(t, tr, pl1, "150000000")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, pl1); got != "150000000\n" {
		t.Errorf("an exact write reads back %q, want it with the newline sysfs prints", got)
	}
	if e := nodeOneEvent(t, n, pl1); e.Kind != "accepted" {
		t.Errorf("event = %+v, want accepted", e)
	}
}

// H08: acpi-cpufreq resolves scaling_max_freq to its table and clamps it to
// cpuinfo_max_freq; the running frequency follows, and under performance it
// stays at the resolved maximum at zero load.
func TestH08ACPIMaxFreqResolvesToTheTable(t *testing.T) {
	t.Parallel()
	p := nodeEPYCProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{CPUUtil: []float64{1}})
	maxf, minf, cur := nodePolicyFile(0, "scaling_max_freq"), nodePolicyFile(0, "scaling_min_freq"), nodePolicyFile(0, "scaling_cur_freq")

	nodeWrite(t, tr, maxf, "1700000")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, maxf); got != "1500000\n" {
		t.Errorf("1700000 reads back %q, want 1500000, the highest entry at or below", got)
	}
	if e := nodeOneEvent(t, n, maxf); e.Kind != "resolved" {
		t.Errorf("event = %+v, want resolved", e)
	}
	nodeStep(t, n, 10*time.Second)
	if got := nodeRead(t, tr, cur); got != "1500000\n" {
		t.Errorf("scaling_cur_freq = %q, want 1500000", got)
	}

	nodeWrite(t, tr, maxf, "99999999")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, maxf); got != "2400000\n" {
		t.Errorf("99999999 reads back %q, want 2400000", got)
	}
	if e := nodeOneEvent(t, n, maxf); e.Kind != "clamped" {
		t.Errorf("event = %+v, want clamped", e)
	}
	n.SetLoad(Load{})
	nodeStep(t, n, 10*time.Second)
	if got := nodeRead(t, tr, cur); got != "2400000\n" {
		t.Errorf("performance at zero load: scaling_cur_freq = %q, want the resolved maximum 2400000", got)
	}

	// A maximum below the minimum lowers the minimum's readback too.
	nodeWrite(t, tr, minf, "1900000")
	nodeStep(t, n, 0)
	nodeWrite(t, tr, maxf, "1600000")
	nodeStep(t, n, 0)
	if gotMax, gotMin := nodeRead(t, tr, maxf), nodeRead(t, tr, minf); gotMax != "1500000\n" || gotMin != "1500000\n" {
		t.Errorf("max %q, min %q; want both 1500000", gotMax, gotMin)
	}
}

// nodeMI300XCap is power1_cap of GPU i, card i+1 behind the BMC's card0.
func nodeMI300XCap(i int, name string) string {
	return path.Join(layout.SysDir, layout.PCIDevDir(0x10+i), "hwmon", "hwmon"+strconv.Itoa(i+2), name)
}

// H09: amdgpu power1_cap truncates to whole watts and refuses a value outside
// [power1_cap_min, power1_cap_max] (amdgpu_pm.c:3469-3496).
func TestH09AmdgpuCapTruncatesAndRejectsOutOfRange(t *testing.T) {
	t.Parallel()
	p := nodeMI300XProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	capf := nodeMI300XCap(0, "power1_cap")

	nodeWrite(t, tr, capf, "300500000")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, capf); got != "300000000\n" {
		t.Errorf("300500000 reads back %q, want 300000000", got)
	}
	nodeWrite(t, tr, capf, "800000000")
	nodeStep(t, n, 5*time.Second)
	if got := nodeRead(t, tr, capf); got != "300000000\n" {
		t.Errorf("an out-of-range write reads back %q, want the previous 300000000", got)
	}
	if e := nodeOneEvent(t, n, capf); e.Kind != "reverted-out-of-range" || e.Written != "800000000" || e.Effective != "300000000" {
		t.Errorf("event = %+v, want reverted-out-of-range keeping 300000000", e)
	}
	g := n.State().GPUs
	if g[0].PowerW > 303 || g[1].PowerW < 600 {
		t.Errorf("GPU power = %.1f W and %.1f W, want GPU 0 (card1) capped at 300 W and GPU 1 uncapped", g[0].PowerW, g[1].PowerW)
	}
	if got := nodeReadInt64(t, tr, nodeMI300XCap(0, "power1_input")); got > 303000000 {
		t.Errorf("power1_input = %d uW, want at most 303 W", got)
	}
}

// H10: HSMP power1_cap reads back in mW times 1000, and package power, as
// power1_input shows it, settles to the cap.
func TestH10HSMPCapTruncatesToMilliwattsAndSettles(t *testing.T) {
	t.Parallel()
	p := nodeHSMPProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{CPUUtil: []float64{1}})
	dir := path.Join(layout.SysDir, layout.HSMPHwmonDir(0))
	capf, input := path.Join(dir, "power1_cap"), path.Join(dir, "power1_input")

	nodeStep(t, n, 5*time.Second)
	if got := nodeReadInt64(t, tr, input); got < 399000000 {
		t.Fatalf("uncapped power1_input = %d uW, want about 400 W", got)
	}
	nodeWrite(t, tr, capf, "250000123")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, capf); got != "250000000\n" {
		t.Errorf("250000123 reads back %q, want 250000000", got)
	}
	nodeStep(t, n, 10*time.Second)
	if got := nodeReadInt64(t, tr, input); got > 252500000 || got < 247500000 {
		t.Errorf("power1_input after settling = %d uW, want 250 W within 1%%", got)
	}
	if got := nodeReadInt64(t, tr, path.Join(layout.SysDir, layout.HSMPHwmonDir(1), "power1_input")); got < 399000000 {
		t.Errorf("socket 1 power1_input = %d uW, want unchanged at about 400 W", got)
	}

	nodeWrite(t, tr, capf, "500000000")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, capf); got != "400000000\n" {
		t.Errorf("a cap above power1_cap_max reads back %q, want 400000000", got)
	}
}

// H25: D entries stay directories nobody writes, and a written R file is put
// back in place at the next Step with a restored event.
func TestH25RejectingEntriesStayAndRenderFilesAreRestored(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	var rejecting []string
	for _, f := range tr.Files {
		if f.Kind == layout.KindRejectingDir {
			rejecting = append(rejecting, f.Rel)
		}
	}
	if len(rejecting) != 2 {
		t.Fatalf("fixture has %d D entries, want the two DRAM limits", len(rejecting))
	}

	window := nodeZoneFile("constraint_0_time_window_us", 0, 0)
	enabled := nodeZoneFile("enabled", 0, 0)
	before, err := os.Stat(filepath.Join(tr.Root, window))
	if err != nil {
		t.Fatal(err)
	}
	nodeWrite(t, tr, window, "5000")
	nodeWrite(t, tr, enabled, "1")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, window); got != "976\n" {
		t.Errorf("render-owned time window after a write = %q, want 976 restored", got)
	}
	if got := nodeRead(t, tr, enabled); got != "0\n" {
		t.Errorf("render-owned DRAM enabled after a write = %q, want 0 restored", got)
	}
	if e := nodeOneEvent(t, n, window); e.Kind != "restored" || e.Written != "5000" || e.Effective != "976" {
		t.Errorf("event = %+v, want restored 5000 to 976", e)
	}
	after, err := os.Stat(filepath.Join(tr.Root, window))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the restore replaced the file; a file bind mount would keep the old one")
	}
	nodeStep(t, n, time.Second)
	if ev := nodeEvents(n, window); len(ev) != 0 {
		t.Errorf("a restored file is restored again: %+v", ev)
	}

	for _, rel := range rejecting {
		abs := filepath.Join(tr.Root, rel)
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			t.Errorf("%s is no longer a directory: %v", rel, err)
			continue
		}
		if entries, _ := os.ReadDir(abs); len(entries) != 0 {
			t.Errorf("%s gained entries: %v", rel, entries)
		}
		if err := os.WriteFile(abs, []byte("1"), 0); !errors.Is(err, syscall.EISDIR) {
			t.Errorf("writing %s: %v, want EISDIR", rel, err)
		}
	}
}

// nodeOpenTruncated opens rel as os.WriteFile does, truncating it, and leaves
// the write to nodeFinishWrite: a writer caught between truncate and write.
func nodeOpenTruncated(t *testing.T, tr *Tree, rel string) *os.File {
	t.Helper()
	w, err := os.OpenFile(filepath.Join(tr.Root, filepath.FromSlash(rel)), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// nodeFinishWrite completes the write nodeOpenTruncated started.
func nodeFinishWrite(t *testing.T, w *os.File, v string) {
	t.Helper()
	if _, err := w.WriteString(v); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// nodeAgeFile dates rel's last change an hour back, as a truncation whose
// writer never wrote.
func nodeAgeFile(t *testing.T, tr *Tree, rel string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(tr.Root, filepath.FromSlash(rel)), old, old); err != nil {
		t.Fatal(err)
	}
}

// H26: empty or unparsable content in an A file keeps the previous effective
// value with a parse-error event. Empty content is a writer between truncate
// and write, so the file is left for it to finish.
func TestH26UnparsableWritesKeepThePreviousValue(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)

	nodeWrite(t, tr, pl1, "12ab")
	nodeStep(t, n, 3*time.Second)
	if e := nodeOneEvent(t, n, pl1); e.Kind != "parse-error" || e.Written != "12ab" || e.Effective != "165000000" {
		t.Errorf("event = %+v, want parse-error keeping 165000000", e)
	}
	if got := nodeRead(t, tr, pl1); got != "165000000\n" {
		t.Errorf("12ab reads back %q, want the previous 165000000", got)
	}
	if pk := n.State().Packages[0]; pk.RequestedCapW != 165 || math.Abs(pk.PowerW-165) > 0.01 {
		t.Errorf("after 12ab: requested %v W, power %.2f W; want 165 W and 165 W", pk.RequestedCapW, pk.PowerW)
	}

	nodeWrite(t, tr, pl1, "")
	nodeStep(t, n, 3*time.Second)
	if e := nodeOneEvent(t, n, pl1); e.Kind != "parse-error" || e.Written != "" || e.Effective != "165000000" {
		t.Errorf("event = %+v, want parse-error for empty content", e)
	}
	if got := nodeRead(t, tr, pl1); got != "" {
		t.Errorf("an empty file was rewritten to %q while its writer may still be writing", got)
	}
	if pk := n.State().Packages[0]; pk.RequestedCapW != 165 || math.Abs(pk.PowerW-165) > 0.01 {
		t.Errorf("after empty content: requested %v W, power %.2f W; want 165 W and 165 W", pk.RequestedCapW, pk.PowerW)
	}
	nodeWrite(t, tr, pl1, "120000000")
	nodeStep(t, n, 0)
	if e := nodeOneEvent(t, n, pl1); e.Kind != "accepted" {
		t.Errorf("the completed write: event %+v, want accepted", e)
	}

	// A writer still between truncate and write is left alone however many
	// steps pass; one whose truncation is old gave up, and the value its
	// open truncated away is put back, as sysfs never shows an empty limit.
	nodeWrite(t, tr, pl1, "")
	nodeStep(t, n, 0)
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, pl1); got != "" {
		t.Errorf("a fresh truncation was rewritten to %q while its writer may still be writing", got)
	}
	nodeAgeFile(t, tr, pl1)
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, pl1); got != "120000000\n" {
		t.Errorf("a file left empty by an old truncation = %q, want 120000000 restored", got)
	}

	maxf := nodePolicyFile(0, "scaling_max_freq")
	nodeWrite(t, tr, maxf, "12ab")
	nodeStep(t, n, 0)
	if e := nodeOneEvent(t, n, maxf); e.Kind != "parse-error" || e.Effective != "3900000" {
		t.Errorf("scaling_max_freq event = %+v, want parse-error keeping 3900000", e)
	}
}

// H26: the restore of an emptied file never lands under a pending write,
// with or without a lease. Two empty readbacks of two different truncations
// are no abandoned writer: the agent wrote in between and is writing again.
// Nor is an old truncation that a new one replaces between the readback's
// read and its restore. With a lease, nor is an old truncation whose writer
// still holds the file open. Restoring would put the old value under the
// agent's write, which then lands over it: 50000000 over 120000000\n reads
// 500000000.
func TestH26RestoreNeverLandsUnderAPendingWrite(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
		check := func(t *testing.T, when string) {
			t.Helper()
			if got, gotW := nodeRead(t, tr, pl1), n.State().Packages[0].RequestedCapW; got != "50000000\n" || gotW != 50 {
				t.Errorf("%s: PL1 reads back %q, requested %v W; want 50000000 and 50 W", when, got, gotW)
			}
		}

		// Two truncations: the agent wrote 60000000 in between and is
		// writing again.
		nodeWrite(t, tr, pl1, "120000000")
		nodeStep(t, n, 0)
		nodeWrite(t, tr, pl1, "")
		nodeStep(t, n, 0)
		nodeWrite(t, tr, pl1, "60000000")
		w := nodeOpenTruncated(t, tr, pl1)
		nodeStep(t, n, 0)
		nodeFinishWrite(t, w, "50000000")
		nodeStep(t, n, 0)
		check(t, "after a second truncation and its write")

		// An old truncation, and a new one landing between its read and its
		// restore: the handler of the old one's readback opens the file.
		nodeWrite(t, tr, pl1, "120000000")
		nodeStep(t, n, 0)
		nodeWrite(t, tr, pl1, "")
		nodeAgeFile(t, tr, pl1)
		f := nodeAFileOf(t, n, pl1)
		handle := f.handle
		f.handle = func(v string) (string, string) {
			if w == nil {
				w = nodeOpenTruncated(t, tr, pl1)
			}
			return handle(v)
		}
		w = nil
		nodeStep(t, n, 0)
		f.handle = handle
		nodeFinishWrite(t, w, "50000000")
		nodeStep(t, n, 0)
		check(t, "after a truncation between the read and the restore")

		if !leases {
			return
		}
		t.Run("held open", func(t *testing.T) {
			nodeNeedLeases(t)
			nodeWrite(t, tr, pl1, "120000000")
			nodeStep(t, n, 0)
			w := nodeOpenTruncated(t, tr, pl1)
			nodeAgeFile(t, tr, pl1)
			nodeStep(t, n, 0)
			nodeFinishWrite(t, w, "50000000")
			nodeStep(t, n, 0)
			check(t, "after a writer held an old truncation open")
		})
	})
}
