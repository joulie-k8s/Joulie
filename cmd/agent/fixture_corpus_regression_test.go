package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/matbun/joulie/pkg/agent/dvfs"
)

// TestCorpusReplayIgnoresTheHostSysfs: the corpus replay reads the capture
// and never the /sys of the machine the test runs on. Governor discovery
// reads the host's /sys through dvfs.HostSysRoot, which pointAgentAtFixture
// left alone while it redirected every other host path, so on a machine with
// a /host-sys tree (an agent image, a runner that mounts the host) every
// golden picked up that host's cpufreq governor and frequency range. A
// populated tree stands in for /host-sys here, and every machine with a
// golden must still publish exactly its golden.
func TestCorpusReplayIgnoresTheHostSysfs(t *testing.T) {
	t.Setenv("HARDWARE_CATALOG_PATH", "")

	host := t.TempDir()
	policy := filepath.Join(host, "devices", "system", "cpu", "cpufreq", "policy0")
	if err := os.MkdirAll(policy, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"scaling_driver":              "acpi-cpufreq\n",
		"scaling_governor":            "userspace\n",
		"scaling_available_governors": "userspace performance \n",
		"cpuinfo_min_freq":            "300000\n",
		"cpuinfo_max_freq":            "1500000\n",
	} {
		if err := os.WriteFile(filepath.Join(policy, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(host, "devices", "system", "cpu", "cpufreq", "boost"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHostSys := dvfs.HostSysRoot
	dvfs.HostSysRoot = host
	t.Cleanup(func() { dvfs.HostSysRoot = oldHostSys })

	for _, machine := range corpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(corpusRoot(), machine)
			want, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if errors.Is(err, os.ErrNotExist) {
				t.Skipf("%s has no expected.json", machine)
			}
			if err != nil {
				t.Fatal(err)
			}
			meta := readCorpusMachine(t, dir)
			hideHostGPUProbe(t, dir)
			pointAgentAtFixture(t, dir, copyCorpusPowercap(t, dir))
			node := corpusNode(t, machine, dir, meta)

			got := publishedNodeHardwareStatus(t, node.Name, discoverHardware(context.Background(), node))
			if string(got) != string(want) {
				t.Fatalf("with a /host-sys tree on the test machine, %s publishes a status that differs from its golden: "+
					"the replay read the host.\ngot:\n%s\nwant:\n%s", machine, got, want)
			}
		})
	}
}
