package smi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
