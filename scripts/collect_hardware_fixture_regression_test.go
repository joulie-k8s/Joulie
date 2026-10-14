package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The identifier check split the output of find on white space, so under a
// capture directory whose path holds a space every governor file became two
// paths that do not exist, sed read nothing, and the check passed: a bus id
// left in a governor file reached the tar. These tests run the check and a
// whole capture under such a path.

// GF03: --check on a capture whose path holds a space still finds the bus
// id, and exits 1.
func TestGF03CheckFindsABusIDUnderAPathWithASpace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture with space")
	collectFiles(t, dir, map[string]string{"governor/amdgpu/gpu0/pp_dpm_sclk": "0: 500Mhz 0000:03:00.0\n"})
	out, code := runCollect(t, collectTools(t), "--check", dir, "--node", "fixture-node-17")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "governor/amdgpu/gpu0/pp_dpm_sclk:1: contains a PCI bus id") {
		t.Errorf("the output does not name the file and the bus id:\n%s", out)
	}
}

// GF03: a capture written under --out with a space in it rejects a bus id in
// a governor file and writes no tar.
func TestGF03CaptureRejectsABusIDUnderAnOutDirWithASpace(t *testing.T) {
	host := collectHostFiles()
	host["sys/bus/pci/devices/0000:03:00.0/pp_dpm_sclk"] = "0: 500Mhz *\n1: 1200Mhz 0000:03:00.0\n"
	root := t.TempDir()
	collectFiles(t, root, host)
	outDir := filepath.Join(t.TempDir(), "out dir")
	out, code := runCollect(t, collectTools(t), "--name", "synthetic", "--out", outDir,
		"--sysfs", filepath.Join(root, "sys"), "--procfs", filepath.Join(root, "proc"))
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "governor/amdgpu/gpu0/pp_dpm_sclk:2: contains a PCI bus id") {
		t.Errorf("the output does not name the file, the line and the bus id:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(outDir, "synthetic.tar")); !os.IsNotExist(err) {
		t.Errorf("a tar was written for a capture the check rejected (err %v)", err)
	}
}
