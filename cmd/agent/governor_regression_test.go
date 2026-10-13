package main

// Regression test of the governor discovery: the live agent never wrote
// NodeHardware.status.cpu.landmarks, although the field existed and its
// documentation says the agent publishes frequency landmarks.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	corev1 "k8s.io/api/core/v1"
)

// sysfsFiles writes files, paths relative to root and slash separated, with
// their content.
func sysfsFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pointAgentAtSysfs makes root the agent's host /sys and hides every other
// host path and tool, so discovery reads only the synthetic tree (and the
// cpuinfo given, when not empty) and never the machine running the test.
// The seams are process globals: a test that uses it never calls t.Parallel.
func pointAgentAtSysfs(t *testing.T, root, cpuinfo string, runner CommandRunner) {
	t.Helper()
	absent := t.TempDir()

	oldHostSys := dvfs.HostSysRoot
	dvfs.HostSysRoot = root
	t.Cleanup(func() { dvfs.HostSysRoot = oldHostSys })

	oldPowercap := dvfs.PowercapRoot
	dvfs.PowercapRoot = filepath.Join(absent, "powercap")
	t.Cleanup(func() { dvfs.PowercapRoot = oldPowercap })

	oldCPUInfo := procCPUInfoPath
	procCPUInfoPath = filepath.Join(absent, "cpuinfo")
	if cpuinfo != "" {
		procCPUInfoPath = filepath.Join(root, "cpuinfo")
		if err := os.WriteFile(procCPUInfoPath, []byte(cpuinfo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { procCPUInfoPath = oldCPUInfo })

	oldDriver := cpufreqDriverPath
	cpufreqDriverPath = filepath.Join(root, "devices", "system", "cpu", "cpufreq", "policy0", "scaling_driver")
	t.Cleanup(func() { cpufreqDriverPath = oldDriver })

	oldNvidiactl := nvidiaControlDevicePath
	nvidiaControlDevicePath = filepath.Join(absent, "nvidiactl")
	t.Cleanup(func() { nvidiaControlDevicePath = oldNvidiactl })

	if runner == nil {
		runner = fakeCommandRunner{}
	}
	oldRunner := commandRunner
	commandRunner = runner
	t.Cleanup(func() { commandRunner = oldRunner })
}

// publishedStatus runs discovery and the real status writer for a node with
// labels and returns the stored status, decoded into the typed struct.
func publishedStatus(t *testing.T, nodeName string, labels map[string]string) v1alpha1.NodeHardwareStatus {
	t.Helper()
	node := &corev1.Node{}
	node.Name = nodeName
	node.Labels = labels
	b := publishedNodeHardwareStatus(t, nodeName, discoverHardware(context.Background(), node))
	var status v1alpha1.NodeHardwareStatus
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatalf("decode the published status: %v\n%s", err, b)
	}
	return status
}

// GA13: the agent writes cpu.landmarks from cpufreq: the lowest
// cpuinfo_min_freq, the highest cpuinfo_max_freq and base_frequency, in MHz.
func TestGA13LandmarksAreWrittenFromCPUFreq(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for _, p := range []string{"policy0", "policy1"} {
		dir := "devices/system/cpu/cpufreq/" + p + "/"
		files[dir+"scaling_driver"] = "intel_pstate\n"
		files[dir+"cpuinfo_min_freq"] = "300000\n"
		files[dir+"cpuinfo_max_freq"] = "1500000\n"
		files[dir+"base_frequency"] = "1000000\n"
	}
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)

	status := publishedStatus(t, "ga13-landmarks", nil)
	if status.CPU == nil || status.CPU.Landmarks == nil {
		t.Fatalf("NodeHardware.status.cpu.landmarks is not written, although cpufreq has cpuinfo_min_freq, cpuinfo_max_freq and base_frequency; cpu status: %+v", status.CPU)
	}
	want := v1alpha1.CPULandmarks{MinFreqMHz: 300, NominalFreqMHz: 1000, MaxBoostMHz: 1500}
	if got := *status.CPU.Landmarks; got != want {
		t.Fatalf("cpu.landmarks = %+v, want %+v", got, want)
	}
}
