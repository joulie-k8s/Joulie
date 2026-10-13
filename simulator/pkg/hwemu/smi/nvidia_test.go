package smi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The corpus capture is what a real nvidia-smi printed for the agent's query
// on a T4. The same values as NVML state must print the same bytes, or the
// agent would parse emulated nodes differently from real ones.
func TestS01AgentQueryMatchesCorpusT4Capture(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "cmd", "agent", "testdata", "hardware", "vm-no-rapl", "nvidia-smi.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != "0, 60.00, 70.00, 70.00, 9.97, Tesla T4\n" {
		t.Fatalf("corpus capture changed to %q; update this test's state to match it", want)
	}
	n := newTestNode(t, map[string]string{"nvidia-smi": "default"})
	n.nvmlGPU(0, map[string]string{
		"name":                    "Tesla T4",
		"uuid":                    "GPU-00000000-0000-0000-0000-000000000000",
		"pci_bus_id":              "00000000:00:1e.0",
		"power_min_limit_mw":      "60000",
		"power_max_limit_mw":      "70000",
		"power_default_limit_mw":  "70000",
		"power_limit_mw":          "70000",
		"enforced_power_limit_mw": "70000",
		"power_draw_mw":           "9970",
		"utilization_gpu_pct":     "0",
	})
	out, err := n.run(agentNvidiaQuery...)
	if err != nil {
		t.Fatalf("query: %v (%q)", err, out)
	}
	if string(out) != string(want) {
		t.Fatalf("query output\n got %q\nwant %q", out, want)
	}
}

// -pl writes the requested limit in mW for the selected GPU only, refuses a
// value outside [min, max] without writing anything, and reports an unknown
// index with exit 6 (nvidia-smi RETURN VALUE).
func TestS02PowerLimitWritesInRangeOnlyAndExitCodes(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	out, err := n.run(agentNvidiaSet(3, 300)...)
	if err != nil {
		t.Fatalf("-i 3 -pl 300: %v (%q)", err, out)
	}
	for i := 0; i < 8; i++ {
		want := "400000"
		if i == 3 {
			want = "300000"
		}
		if got := n.read(nvmlRel(i, "power_limit_mw")); got != want {
			t.Errorf("gpu%d power_limit_mw = %s, want %s", i, got, want)
		}
	}

	out, err = n.run("nvidia-smi", "-pl", "150")
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("-pl 150 on 200 to 400 W GPUs: exit %d, want 2 (%q)", code, out)
	}
	if !strings.Contains(string(out), "should be between 200.00 W and 400.00 W") {
		t.Errorf("-pl 150 output %q, want the valid range", out)
	}
	for i := 0; i < 8; i++ {
		want := "400000"
		if i == 3 {
			want = "300000"
		}
		if got := n.read(nvmlRel(i, "power_limit_mw")); got != want {
			t.Errorf("after rejected -pl 150: gpu%d power_limit_mw = %s, want %s", i, got, want)
		}
	}

	out, err = n.run(agentNvidiaSet(9, 300)...)
	if code := exitCode(t, err); code != 6 {
		t.Fatalf("-i 9 on 8 GPUs: exit %d, want 6 (%q)", code, out)
	}
	if !strings.Contains(string(out), "No devices were found") {
		t.Errorf("-i 9 output %q, want No devices were found", out)
	}
}

// An option nvidia-smi does not have is an invalid argument, exit 2, with
// nothing on stdout for the agent to mistake for data.
func TestS03UnknownNvidiaOptionExits2(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	code, stdout, stderr := n.main("", "nvidia-smi", "--showpowercap")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "Invalid combination of input arguments") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 2, empty, invalid combination", code, stdout, stderr)
	}
	if _, err := n.run("nvidia-smi", "-x"); exitCode(t, err) != 2 {
		t.Fatalf("-x through Runner: %v, want exit status 2", err)
	}
}

// A field the device does not support prints its token literally in the
// agent's query line, which the agent then parses as 0 (D16).
func TestS14UnsupportedFieldPrintsItsToken(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	n.nvmlGPU(0, map[string]string{"power_min_limit_mw": "[N/A]"})
	out, err := n.run(agentNvidiaQuery...)
	if err != nil {
		t.Fatalf("query: %v (%q)", err, out)
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	if want := "0, [N/A], 400.00, 400.00, 50.00, NVIDIA H100 NVL"; first != want {
		t.Fatalf("gpu0 line %q, want %q", first, want)
	}
	if !strings.Contains(string(out), "\n1, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL\n") {
		t.Fatalf("gpu1 line missing or wrong in %q", out)
	}
}

// A limit that holds a token is not a number: -pl on that GPU is refused as
// not supported (exit 3) and writes nothing, where reading the token as 0, as
// the agent does (D16), would accept any value up to the maximum.
func TestS14TokenLimitRefusesPowerLimit(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	n.nvmlGPU(0, map[string]string{"power_min_limit_mw": "[N/A]"})
	out, err := n.run(agentNvidiaSet(0, 100)...)
	if code := exitCode(t, err); code != 3 || !strings.Contains(string(out), "not supported for GPU: 00000000:10:00.0") {
		t.Fatalf("-i 0 -pl 100 with a [N/A] minimum: exit %d, output %q; want 3, not supported", code, out)
	}
	if got := n.read(nvmlRel(0, "power_limit_mw")); got != "400000" {
		t.Fatalf("gpu0 power_limit_mw = %s after a refused -pl, want 400000", got)
	}
}

// Every field has a file holding a value or a token, so a missing file is
// state nobody rendered. It fails the call with exit 255 and nothing on
// stdout, instead of printing [N/A] the device never reported, which would
// let a renderer that drops a file pass A16 unnoticed.
func TestS14MissingStateFileFailsTheCall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, file string
		argv       []string
	}{
		{"query", "power_min_limit_mw", agentNvidiaQuery},
		{"list", "uuid", agentNvidiaList},
		{"power limit", "power_max_limit_mw", agentNvidiaSet(0, 300)},
		{"select by bus", "uuid", []string{"nvidia-smi", "-i", "00000000:11:00.0", "-pl", "300"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := newNVLNode(t)
			n.remove(nvmlRel(0, c.file))
			code, stdout, stderr := n.main("", c.argv...)
			if code != 255 || stdout != "" || !strings.Contains(stderr, filepath.Join("gpu0", c.file)) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 255, empty, the missing file", code, stdout, stderr)
			}
			for i := 0; i < 8; i++ {
				if got := n.read(nvmlRel(i, "power_limit_mw")); got != "400000" {
					t.Fatalf("gpu%d power_limit_mw = %s after a failed call, want 400000", i, got)
				}
			}
		})
	}
}

// S16: -pm 0 writes Disabled to persistence_mode of every GPU, in place, and
// prints one line per GPU and All done, exit 0; -i limits it to the GPUs
// named; a value other than 0 or 1 is an invalid argument, exit 2, with
// nothing written; the persistence_mode query field prints what is stored.
func TestS16PersistenceModeSetAndQuery(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	for i := 0; i < 8; i++ {
		n.write(nvmlRel(i, "persistence_mode"), "Enabled", 0o644)
	}
	before, err := os.Stat(filepath.Join(n.root, nvmlRel(5, "persistence_mode")))
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := n.main("", "nvidia-smi", "-pm", "0")
	var want strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&want, "Disabled persistence mode for GPU 00000000:%02x:00.0.\n", 0x10+i)
	}
	want.WriteString("All done.\n")
	if code != 0 || stdout != want.String() || stderr != "" {
		t.Fatalf("-pm 0: exit %d, stdout %q, stderr %q; want 0, %q, nothing", code, stdout, stderr, want.String())
	}
	for i := 0; i < 8; i++ {
		if got := n.read(nvmlRel(i, "persistence_mode")); got != "Disabled" {
			t.Errorf("gpu%d persistence_mode = %q, want Disabled", i, got)
		}
	}
	after, err := os.Stat(filepath.Join(n.root, nvmlRel(5, "persistence_mode")))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("-pm replaced persistence_mode; the emulator reads agent-owned files back in place")
	}

	out, err := n.run("nvidia-smi", "-i", "2", "--persistence-mode=1")
	if err != nil || string(out) != "Enabled persistence mode for GPU 00000000:12:00.0.\nAll done.\n" {
		t.Fatalf("-i 2 --persistence-mode=1: %v, %q", err, out)
	}
	out, err = n.run("nvidia-smi", "--query-gpu=index,persistence_mode", "--format=csv,noheader")
	if err != nil {
		t.Fatalf("query: %v (%q)", err, out)
	}
	if want := "0, Disabled\n1, Disabled\n2, Enabled\n3, Disabled\n4, Disabled\n5, Disabled\n6, Disabled\n7, Disabled\n"; string(out) != want {
		t.Errorf("query output %q, want %q", out, want)
	}

	for _, v := range []string{"2", "on", ""} {
		code, stdout, _ := n.main("", "nvidia-smi", "-pm", v)
		if code != 2 || stdout != "" {
			t.Errorf("-pm %q: exit %d, stdout %q; want 2 and nothing", v, code, stdout)
		}
	}
	if got := n.read(nvmlRel(2, "persistence_mode")); got != "Enabled" {
		t.Errorf("gpu2 persistence_mode = %q after rejected calls, want Enabled", got)
	}

	n.remove(nvmlRel(7, "persistence_mode"))
	code, stdout, _ = n.main("", "nvidia-smi", "-pm", "0")
	if code != 255 || stdout != "" || n.read(nvmlRel(2, "persistence_mode")) != "Enabled" {
		t.Errorf("-pm 0 with gpu7's file missing: exit %d, stdout %q, gpu2 %q; want 255, nothing printed or written", code, stdout, n.read(nvmlRel(2, "persistence_mode")))
	}
}

// nvClockGPU writes the clock state of GPU i of a test node, with neutral
// clocks: graphics 300 to 1500 MHz and memory 300 to 1200 MHz, idle at the
// minimum, unlocked. memLockable false gives the memory lock file the [N/A]
// token, render-owned, as a GPU without a run-time memory lock has.
func (n *testNode) nvClockGPU(i int, memLockable bool) {
	files := map[string]string{
		"clocks_sm_mhz":        "300",
		"clocks_mem_mhz":       "300",
		"clocks_max_sm_mhz":    "1500",
		"clocks_max_mem_mhz":   "1200",
		"pstate":               "P8",
		"clocks_event_reasons": "0x0000000000000001",
		"supported_clocks":     "memory 1200 600 300\ngraphics 1500 1200 900 600 300",
	}
	for f, v := range files {
		n.write(nvmlRel(i, f), v, 0o444)
	}
	n.write(nvmlRel(i, layout.NVMLLockedGPUClocksFile), "none", 0o644)
	if memLockable {
		n.write(nvmlRel(i, layout.NVMLLockedMemClocksFile), "none", 0o644)
	} else {
		n.write(nvmlRel(i, layout.NVMLLockedMemClocksFile), "[N/A]", 0o444)
	}
}

// newNVClockNode is newNVLNode with the clock state of nvClockGPU on every
// GPU.
func newNVClockNode(t *testing.T, memLockable bool) *testNode {
	n := newNVLNode(t)
	for i := 0; i < 8; i++ {
		n.nvClockGPU(i, memLockable)
	}
	return n
}

// nvLocks returns the graphics lock file of every GPU, in index order.
func (n *testNode) nvLocks(file string) []string {
	out := make([]string, 8)
	for i := range out {
		out[i] = n.read(nvmlRel(i, file))
	}
	return out
}

// NV01: -i <bus id> -lgc 1200,1200 writes the lock of that GPU only, in
// place, prints one confirmation line and All done, exit 0, with nothing on
// stderr. A single clock locks both bounds, and --mode 0 or 1 is taken.
func TestNV01LockGPUClocksChangesOnlyTheNamedGPU(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	before, err := os.Stat(filepath.Join(n.root, nvmlRel(2, layout.NVMLLockedGPUClocksFile)))
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := n.main("", "nvidia-smi", "-i", "00000000:12:00.0", "-lgc", "1200,1200")
	want := "GPU clocks set to \"(gpuClkMin 1200, gpuClkMax 1200)\" for GPU 00000000:12:00.0\nAll done.\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("-lgc 1200,1200: exit %d, stdout %q, stderr %q; want 0, %q, nothing", code, stdout, stderr, want)
	}
	for i, got := range n.nvLocks(layout.NVMLLockedGPUClocksFile) {
		want := "none"
		if i == 2 {
			want = "1200,1200"
		}
		if got != want {
			t.Errorf("gpu%d %s = %q, want %q", i, layout.NVMLLockedGPUClocksFile, got, want)
		}
	}
	after, err := os.Stat(filepath.Join(n.root, nvmlRel(2, layout.NVMLLockedGPUClocksFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("-lgc replaced the lock file; the emulator reads agent-owned files back in place")
	}
	if got := n.read(nvmlRel(2, layout.NVMLLockedMemClocksFile)); got != "none" {
		t.Errorf("-lgc changed the memory lock to %q", got)
	}

	out, err := n.run("nvidia-smi", "-i", "3", "-lgc", "900", "--mode", "1")
	if err != nil || !strings.HasSuffix(string(out), "All done.\n") {
		t.Fatalf("-i 3 -lgc 900 --mode 1: %v, %q", err, out)
	}
	if got := n.read(nvmlRel(3, layout.NVMLLockedGPUClocksFile)); got != "900,900" {
		t.Errorf("a single clock locks %q, want 900,900", got)
	}
	if _, err := n.run("nvidia-smi", "-i", "4", "--lock-gpu-clocks=300,600", "-m", "0"); err != nil {
		t.Fatalf("--lock-gpu-clocks=300,600 -m 0: %v", err)
	}
	if got := n.read(nvmlRel(4, layout.NVMLLockedGPUClocksFile)); got != "300,600" {
		t.Errorf("gpu4 lock %q, want 300,600", got)
	}

	for _, argv := range [][]string{
		{"nvidia-smi", "-i", "0", "-lgc", "1500,1200"},
		{"nvidia-smi", "-i", "0", "-lgc", "0,1200"},
		{"nvidia-smi", "-i", "0", "-lgc", "fast"},
		{"nvidia-smi", "-i", "0", "-lgc", "300,600", "-m", "2"},
		{"nvidia-smi", "-i", "0", "-m", "1"},
		{"nvidia-smi", "-i", "0", "-lgc", "300,600", "-lmc", "300,600"},
	} {
		if code, stdout, _ := n.main("", argv...); code != 2 || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want 2 and nothing", argv[1:], code, stdout)
		}
	}
	if got := n.read(nvmlRel(0, layout.NVMLLockedGPUClocksFile)); got != "none" {
		t.Errorf("a refused -lgc wrote %q", got)
	}
}

// NV02: no query field prints a lock, since NVML has no getter for one: no
// field reads a lock file, and a query of every field on GPUs whose locks
// hold values found nowhere else prints neither value.
func TestNV02NoQueryFieldPrintsTheLock(t *testing.T) {
	t.Parallel()
	for f, spec := range nvFields {
		if spec.file == layout.NVMLLockedGPUClocksFile || spec.file == layout.NVMLLockedMemClocksFile {
			t.Errorf("query field %s reads the lock file %s", f, spec.file)
		}
	}
	n := newNVClockNode(t, true)
	for i := 0; i < 8; i++ {
		n.write(nvmlRel(i, "persistence_mode"), "Enabled", 0o644)
		n.write(nvmlRel(i, layout.NVMLLockedGPUClocksFile), "1234,1357", 0o644)
		n.write(nvmlRel(i, layout.NVMLLockedMemClocksFile), "1111,1113", 0o644)
	}
	fields := make([]string, 0, len(nvFields))
	for f := range nvFields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, format := range []string{"csv", "csv,noheader,nounits"} {
		out, err := n.run("nvidia-smi", "--query-gpu="+strings.Join(fields, ","), "--format="+format)
		if err != nil {
			t.Fatalf("query of every field: %v (%q)", err, out)
		}
		for _, v := range []string{"1234", "1357", "1111", "1113"} {
			if strings.Contains(string(out), v) {
				t.Errorf("--format=%s prints the lock value %s:\n%s", format, v, out)
			}
		}
	}
}

// The clock fields print the NVML state with the units of the capture
// nvFields cites: " [MHz]" in the header and " MHz" after a clock; pstate
// and the event reasons as stored.
func TestNVClockFieldsPrintWithUnits(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	n.write(nvmlRel(1, "clocks_sm_mhz"), "1410", 0o444)
	out, err := n.run("nvidia-smi", "-i", "1", "--query-gpu=index,clocks.sm,clocks.current.memory,clocks.max.sm,clocks.max.mem,pstate,clocks_event_reasons.active", "--format=csv")
	want := "index, clocks.sm [MHz], clocks.current.memory [MHz], clocks.max.sm [MHz], clocks.max.mem [MHz], pstate, clocks_event_reasons.active\n" +
		"1, 1410 MHz, 300 MHz, 1500 MHz, 1200 MHz, P8, 0x0000000000000001\n"
	if err != nil || string(out) != want {
		t.Fatalf("query: %v\n got %q\nwant %q", err, out, want)
	}
	out, err = n.run("nvidia-smi", "-i", "1", "--query-gpu=clocks.mem,clocks_throttle_reasons.active", "--format=csv,noheader,nounits")
	if err != nil || string(out) != "300, 0x0000000000000001\n" {
		t.Fatalf("nounits query: %v, %q", err, out)
	}
}

// NV04: -rgc clears the graphics lock and -rmc the memory lock, each on the
// GPUs selected, with one line per GPU and All done.
func TestNV04ResetClearsTheLock(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	for _, argv := range [][]string{
		{"nvidia-smi", "-i", "1", "-lgc", "600,900"},
		{"nvidia-smi", "-i", "2", "-lgc", "600,900"},
		{"nvidia-smi", "-i", "1", "-lmc", "1200,1200"},
	} {
		if out, err := n.run(argv...); err != nil {
			t.Fatalf("%v: %v (%q)", argv[1:], err, out)
		}
	}
	code, stdout, stderr := n.main("", "nvidia-smi", "-i", "1", "-rgc")
	if code != 0 || stdout != "GPU clocks reset to default for GPU 00000000:11:00.0\nAll done.\n" || stderr != "" {
		t.Fatalf("-i 1 -rgc: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := n.read(nvmlRel(1, layout.NVMLLockedGPUClocksFile)); got != "none" {
		t.Errorf("gpu1 graphics lock %q after -rgc, want none", got)
	}
	if got := n.read(nvmlRel(2, layout.NVMLLockedGPUClocksFile)); got != "600,900" {
		t.Errorf("-i 1 -rgc cleared gpu2's lock: %q", got)
	}
	if got := n.read(nvmlRel(1, layout.NVMLLockedMemClocksFile)); got != "1200,1200" {
		t.Errorf("-rgc cleared the memory lock: %q", got)
	}
	if out, err := n.run("nvidia-smi", "--reset-memory-clocks"); err != nil || !strings.HasPrefix(string(out), "Memory clocks reset to default for GPU 00000000:10:00.0\n") {
		t.Fatalf("--reset-memory-clocks: %v, %q", err, out)
	}
	if got := n.read(nvmlRel(1, layout.NVMLLockedMemClocksFile)); got != "none" {
		t.Errorf("gpu1 memory lock %q after -rmc, want none", got)
	}
	if _, err := n.run("nvidia-smi", "-rgc"); err != nil {
		t.Fatal(err)
	}
	for i, got := range n.nvLocks(layout.NVMLLockedGPUClocksFile) {
		if got != "none" {
			t.Errorf("gpu%d graphics lock %q after -rgc on every GPU", i, got)
		}
	}
}

// NV05: on a GPU whose memory clock has no run-time lock (Hopper), -lmc and
// -rmc fail with exit 3, say so, and change nothing; where the lock exists
// -lmc <max>,<max> stores it.
func TestNV05MemoryLockWithoutRuntimeLockFails(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, false)
	for _, argv := range [][]string{
		{"nvidia-smi", "-i", "0", "-lmc", "1200,1200"},
		{"nvidia-smi", "-lmc", "1200"},
		{"nvidia-smi", "-i", "0", "-rmc"},
	} {
		code, stdout, stderr := n.main("", argv...)
		if code != 3 || !strings.Contains(stdout, "is not supported for GPU 00000000:10:00.0") || stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 3 and not supported", argv[1:], code, stdout, stderr)
		}
	}
	for i, got := range n.nvLocks(layout.NVMLLockedMemClocksFile) {
		if got != "[N/A]" {
			t.Errorf("gpu%d memory lock %q after a refused -lmc, want [N/A]", i, got)
		}
	}

	m := newNVClockNode(t, true)
	out, err := m.run("nvidia-smi", "-i", "0", "-lmc", "1200,1200")
	if err != nil || string(out) != "Memory clocks set to \"(memClkMin 1200, memClkMax 1200)\" for GPU 00000000:10:00.0\nAll done.\n" {
		t.Fatalf("-lmc 1200,1200: %v, %q", err, out)
	}
	if got := m.read(nvmlRel(0, layout.NVMLLockedMemClocksFile)); got != "1200,1200" {
		t.Errorf("memory lock %q, want 1200,1200", got)
	}
}

// NV06: application clocks are deprecated: -ac and -rac print the NVML
// error string, exit non-zero and change nothing; a malformed -ac value is
// an invalid argument.
func TestNV06ApplicationsClocksAreDeprecated(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	for _, argv := range [][]string{
		{"nvidia-smi", "-ac", "1200,1500"},
		{"nvidia-smi", "-i", "0", "--applications-clocks=1200,300"},
		{"nvidia-smi", "-rac"},
	} {
		code, stdout, stderr := n.main("", argv...)
		if code == 0 || !strings.Contains(stdout, "Requested functionality has been deprecated") || stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want non-zero and the deprecation message", argv[1:], code, stdout, stderr)
		}
	}
	for _, v := range []string{"1200", "1200,x", "a,b"} {
		if code, _, _ := n.main("", "nvidia-smi", "-ac", v); code != 2 {
			t.Errorf("-ac %s: exit %d, want 2", v, code)
		}
	}
	for i := 0; i < 8; i++ {
		for _, f := range []string{layout.NVMLLockedGPUClocksFile, layout.NVMLLockedMemClocksFile} {
			if got := n.read(nvmlRel(i, f)); got != "none" {
				t.Errorf("gpu%d %s = %q after -ac and -rac", i, f, got)
			}
		}
	}
}

// NV07: -i takes an index, a PCI bus id in either case or a UUID; one that
// names no GPU exits 6 with nothing written.
func TestNV07SelectByBusIDOrIndex(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	for id, gpu := range map[string]int{
		"5":                5,
		"00000000:16:00.0": 6,
		"00000000:1A:00.0": -1,
		"00000000:17:00.0": 7,
		"GPU-00000000-0000-0000-0000-000000000001": 1,
	} {
		code, stdout, _ := n.main("", "nvidia-smi", "-i", id, "-lgc", "600,600")
		if gpu < 0 {
			if code != 6 || !strings.Contains(stdout, "No devices were found") {
				t.Errorf("-i %s: exit %d, stdout %q; want 6, no devices", id, code, stdout)
			}
			continue
		}
		if code != 0 {
			t.Errorf("-i %s: exit %d (%q)", id, code, stdout)
		}
		if got := n.read(nvmlRel(gpu, layout.NVMLLockedGPUClocksFile)); got != "600,600" {
			t.Errorf("-i %s: gpu%d lock %q, want 600,600", id, gpu, got)
		}
	}
	n.write(nvmlRel(4, "pci_bus_id"), "00000000:1D:00.0", 0o444)
	if code, _, _ := n.main("", "nvidia-smi", "-i", "00000000:1d:00.0", "-lgc", "900,900"); code != 0 {
		t.Errorf("-i in lower case: exit %d", code)
	}
	if got := n.read(nvmlRel(4, layout.NVMLLockedGPUClocksFile)); got != "900,900" {
		t.Errorf("gpu4 lock %q, want 900,900", got)
	}
	for _, i := range []int{0, 2, 3} {
		if got := n.read(nvmlRel(i, layout.NVMLLockedGPUClocksFile)); got != "none" {
			t.Errorf("gpu%d lock %q, want none", i, got)
		}
	}
	if code, _, _ := n.main("", "nvidia-smi", "-i", "9", "-rgc"); code != 6 {
		t.Errorf("-i 9 -rgc: exit %d, want 6", code)
	}
}

// NV08: without root (HWEMU_UNPRIVILEGED) every command that needs root,
// the locks, -pl and -pm, fails with the permission exit code 4, says why,
// and writes nothing.
func TestNV08UnprivilegedSetsFailWithoutChange(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	for i := 0; i < 8; i++ {
		n.write(nvmlRel(i, "persistence_mode"), "Enabled", 0o644)
	}
	n.env.Unprivileged = true
	for _, argv := range [][]string{
		{"nvidia-smi", "-i", "0", "-lgc", "600,600"},
		{"nvidia-smi", "-rgc"},
		{"nvidia-smi", "-i", "0", "-lmc", "1200,1200"},
		{"nvidia-smi", "-rmc"},
		agentNvidiaSet(0, 300),
		{"nvidia-smi", "-pm", "0"},
	} {
		code, stdout, stderr := n.main("", argv...)
		if code != 4 || !strings.Contains(stdout, "Insufficient Permissions") || stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 4 and Insufficient Permissions", argv[1:], code, stdout, stderr)
		}
	}
	for i := 0; i < 8; i++ {
		for f, want := range map[string]string{
			layout.NVMLLockedGPUClocksFile: "none",
			layout.NVMLLockedMemClocksFile: "none",
			"power_limit_mw":               "400000",
			"persistence_mode":             "Enabled",
		} {
			if got := n.read(nvmlRel(i, f)); got != want {
				t.Errorf("gpu%d %s = %q after unprivileged calls, want %q", i, f, got, want)
			}
		}
	}
	if out, err := n.run(agentNvidiaQuery...); err != nil {
		t.Errorf("an unprivileged query fails: %v (%q)", err, out)
	}
}

// NV09: -q -d SUPPORTED_CLOCKS prints the -q header and, for each selected
// GPU, every memory clock with every graphics clock under it, in the
// descending order supported_clocks holds them, with nothing on stderr.
func TestNV09SupportedClocksListsTheState(t *testing.T) {
	t.Parallel()
	n := newNVClockNode(t, true)
	code, stdout, stderr := n.main("", "nvidia-smi", "-q", "-d", "SUPPORTED_CLOCKS", "-i", "00000000:11:00.0")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := strings.Split(stdout, "\n")
	if len(lines) < 6 || lines[0] != "" || lines[1] != "==============NVSMI LOG==============" || !strings.HasPrefix(lines[3], "Timestamp ") {
		t.Fatalf("header of %q is not the -q header", stdout)
	}
	pad := func(indent int, key, value string) string {
		return fmt.Sprintf("%-55s: %s", strings.Repeat(" ", indent)+key, value)
	}
	want := []string{
		pad(0, "Driver Version", "550.54.15"),
		"",
		pad(0, "Attached GPUs", "8"),
		"GPU 00000000:11:00.0",
		"    Supported Clocks",
	}
	for _, m := range []string{"1200", "600", "300"} {
		want = append(want, pad(8, "Memory", m+" MHz"))
		for _, g := range []string{"1500", "1200", "900", "600", "300"} {
			want = append(want, pad(12, "Graphics", g+" MHz"))
		}
	}
	want = append(want, "", "")
	if got := lines[4:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after the timestamp\n got %q\nwant %q", got, want)
	}

	if code, _, stderr := n.main("", "nvidia-smi", "-q"); code != 2 || !strings.Contains(stderr, "is not emulated") {
		t.Errorf("-q without -d: exit %d, stderr %q; want 2, not emulated (a real nvidia-smi prints the whole log)", code, stderr)
	}
	if code, _, _ := n.main("", "nvidia-smi", "-q", "-d", "CLOCK"); code != 2 {
		t.Errorf("-q -d CLOCK: exit %d, want 2 (not emulated)", code)
	}
	if code, _, _ := n.main("", "nvidia-smi", "-d", "SUPPORTED_CLOCKS"); code != 2 {
		t.Errorf("-d without -q: exit %d, want 2", code)
	}
	n.remove(nvmlRel(3, "supported_clocks"))
	if code, stdout, _ := n.main("", "nvidia-smi", "-q", "-d", "SUPPORTED_CLOCKS"); code != 255 || stdout != "" {
		t.Errorf("gpu3 without supported_clocks: exit %d, stdout %q; want 255 and nothing", code, stdout)
	}
}
