package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// fakesmiChildEnv marks a process B01 started. A child that does not become
// a tool exits instead of running the tests, which would start B01 again.
const fakesmiChildEnv = "FAKESMI_TEST_CHILD"

// TestMain makes the test binary fakesmi itself when it runs under a tool's
// name, so B01 can exec it through bin/<tool>-style links.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "nvidia-smi", "rocm-smi", "amd-smi", "fakesmi":
		main()
	}
	if os.Getenv(fakesmiChildEnv) != "" {
		fmt.Fprintf(os.Stderr, "fakesmi test binary started as %q did not run as a tool\n", os.Args[0])
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// fakesmiNode builds a node root by hand: two NVIDIA GPUs in NVML state and
// two MI300X behind a BMC VGA card0, as Render lays them out, with tools
// listed in tools.json.
func fakesmiNode(t *testing.T, tools map[string]string) smi.Env {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	link := func(rel, target string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	m := layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{}}
	for name, v := range tools {
		m.Tools[name] = layout.ToolEntry{Variant: v}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write(path.Join(layout.StateDir, layout.ToolsFile), string(b), 0o444)
	for i := 0; i < 2; i++ {
		for f, v := range map[string]string{
			"name": "NVIDIA H100 NVL", "uuid": fmt.Sprintf("GPU-%d", i), "pci_bus_id": fmt.Sprintf("00000000:%02x:00.0", 0x20+i),
			"power_min_limit_mw": "200000", "power_max_limit_mw": "400000", "power_limit_mw": "400000", "power_draw_mw": "50000",
		} {
			write(path.Join(layout.StateDir, layout.NVMLGPUDir(i), f), v, 0o644)
		}
	}
	cards := []map[string]string{{"vendor": "0x1a03", "device": "0x2000", "class": "0x030000"}}
	for i := 0; i < 2; i++ {
		cards = append(cards, map[string]string{"vendor": "0x1002", "device": "0x74a1", "class": "0x120000",
			"product_name": "AMD Instinct MI300X", "vbios_version": "113-X0000000-102"})
	}
	for card, attrs := range cards {
		bus := 0x10 + card
		dev := layout.PCIDevDir(bus)
		for f, v := range attrs {
			write(path.Join(layout.SysDir, dev, f), v, 0o444)
		}
		link(path.Join(layout.SysDir, layout.DRMCardDir(bus, card), "device"), "../../../"+path.Base(dev))
		link(path.Join(layout.SysDir, "class/drm/card"+strconv.Itoa(card)), "../../"+layout.DRMCardDir(bus, card))
		if card == 0 {
			continue
		}
		hl, ht := layout.HwmonClassLink(card+1, dev)
		link(path.Join(layout.SysDir, hl), ht)
		for f, v := range map[string]string{"name": "amdgpu", "power1_cap": "750000000", "power1_cap_min": "0",
			"power1_cap_max": "750000000", "power1_cap_default": "750000000", "power1_input": "300000000"} {
			write(path.Join(layout.SysDir, dev, "hwmon", "hwmon"+strconv.Itoa(card+1), f), v, 0o644)
		}
	}
	return smi.Env{SysRoot: filepath.Join(root, layout.SysDir), StateRoot: filepath.Join(root, layout.StateDir)}
}

// In a pod the agent execs the fakesmi binary through a link named after the
// tool. That path must give the bytes and exit codes the in-process smi.Main
// gives, which the in-process tests check, and a failing exit must read the
// same in the agent's error text either way.
func TestB01ReExecThroughToolLinksMatchesInProcessMain(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"nvidia-smi", "rocm-smi", "amd-smi", "fakesmi"} {
		if err := os.Symlink(exe, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	nvidia := map[string]string{"nvidia-smi": "default"}
	amd := map[string]string{"rocm-smi": "current", "amd-smi": "7.2"}
	cases := []struct {
		tools map[string]string
		argv  []string
		code  int
	}{
		{nvidia, []string{"nvidia-smi", "--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name", "--format=csv,noheader,nounits"}, 0},
		{nvidia, []string{"nvidia-smi", "-i", "1", "-pl", "300"}, 0},
		{nvidia, []string{"nvidia-smi", "-i", "9", "-pl", "300"}, 6},
		{nvidia, []string{"nvidia-smi", "--bogus"}, 2},
		{nvidia, []string{"fakesmi", "nvidia-smi", "-L"}, 0},
		{nvidia, []string{"rocm-smi", "--showproductname"}, 127},
		{amd, []string{"rocm-smi", "--showproductname", "--json"}, 0},
		{amd, []string{"rocm-smi", "--showpowercap", "--showproductname", "--json"}, 2},
		{amd, []string{"rocm-smi", "-d", "0", "--setpoweroverdrive", "800"}, 0},
		{amd, []string{"amd-smi", "set", "-g", "0", "-o", "300"}, 2},
		{amd, []string{"amd-smi", "set", "-g", "0", "-o", "ppt2", "300"}, 1},
		{amd, []string{"amd-smi", "reset", "-g", "1", "-o"}, 0},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.argv, " "), func(t *testing.T) {
			sub := fakesmiNode(t, c.tools)
			cmd := exec.Command(filepath.Join(bin, c.argv[0]), c.argv[1:]...)
			cmd.Env = append(os.Environ(), "HWEMU_SYS_ROOT="+sub.SysRoot, "HWEMU_STATE_ROOT="+sub.StateRoot, "GOCOVERDIR="+t.TempDir(), fakesmiChildEnv+"=1")
			got, err := cmd.CombinedOutput()

			var want bytes.Buffer
			code := smi.Main(c.argv, fakesmiNode(t, c.tools), strings.NewReader(""), &want, &want)
			if code != c.code {
				t.Fatalf("in-process exit %d, want %d (%q)", code, c.code, want.String())
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("binary output\n%q\nin-process output\n%q", got, want.Bytes())
			}
			var ee *exec.ExitError
			switch {
			case code == 0 && err != nil:
				t.Fatalf("binary: %v, want success", err)
			case code != 0 && !errors.As(err, &ee):
				t.Fatalf("binary: %v (%T), want *exec.ExitError", err, err)
			case code != 0 && (ee.ExitCode() != code || ee.Error() != (&smi.ExitError{Code: code}).Error()):
				t.Fatalf("binary exit %d %q, in-process %d %q", ee.ExitCode(), ee.Error(), code, (&smi.ExitError{Code: code}).Error())
			}
		})
	}
}
