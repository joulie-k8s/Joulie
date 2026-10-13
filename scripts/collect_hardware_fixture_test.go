package scripts

// Tests of collect-hardware-fixture.sh, run against a synthetic host: a sysfs
// and a procfs tree in a temporary directory, and fake nvidia-smi, rocm-smi
// and kubectl first on PATH, so the run never reads the machine it runs on
// beyond its host name and never reaches a cluster. Every value is neutral.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const collectScript = "collect-hardware-fixture.sh"

// collectNvidiaClocks is what the fake nvidia-smi prints for
// -q -d CLOCK,SUPPORTED_CLOCKS,PERFORMANCE: two GPUs, each headed by its bus
// id, and a serial number line the redaction must catch as well.
const collectNvidiaClocks = `
==============NVSMI LOG==============

Timestamp                                 : Fri Oct 10 09:00:00 2026
Driver Version                            : 550.54.15
CUDA Version                              : 12.4

Attached GPUs                             : 2
GPU 00000000:3B:00.0
    Serial Number                         : 1650123456789
    Performance State                     : P8
    Clocks
        Graphics                          : 300 MHz
        Memory                            : 405 MHz
    Supported Clocks
        Memory                            : 1500 MHz
            Graphics                      : 1500 MHz
            Graphics                      : 300 MHz

GPU 00000000:5E:00.0
    Serial Number                         : 1650123456790
    Performance State                     : P8
    Clocks
        Graphics                          : 300 MHz
        Memory                            : 405 MHz
`

// collectFiles writes files, relative to root.
func collectFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// collectHostFiles is the synthetic host, relative to its root: sys/ and
// proc/. Two cpufreq policies with the governor files and two that must not
// be copied (scaling_max_freq, scaling_setspeed), both pstate status files,
// CPU 0's idle states, two amdgpu devices out of bus id order on disk and an
// NVIDIA device without a level file.
func collectHostFiles() map[string]string {
	files := map[string]string{
		"proc/cpuinfo": "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD EPYC 9654 96-Core Processor\nphysical id\t: 0\n\n" +
			"processor\t: 1\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD EPYC 9654 96-Core Processor\nphysical id\t: 0\n\n",
		"proc/meminfo":                                                       "MemTotal:       16384 kB\n",
		"sys/devices/system/cpu/cpufreq/boost":                               "1\n",
		"sys/devices/system/cpu/intel_pstate/status":                         "passive\n",
		"sys/devices/system/cpu/intel_pstate/no_turbo":                       "0\n",
		"sys/devices/system/cpu/amd_pstate/status":                           "active\n",
		"sys/devices/system/cpu/cpu0/cpuidle/state0/name":                    "POLL\n",
		"sys/devices/system/cpu/cpu0/cpuidle/state1/name":                    "C1\n",
		"sys/devices/system/cpu/cpu0/cpuidle/state1/latency":                 "1\n",
		"sys/bus/pci/devices/0000:c1:00.0/vendor":                            "0x1002\n",
		"sys/bus/pci/devices/0000:c1:00.0/device":                            "0x74a1\n",
		"sys/bus/pci/devices/0000:c1:00.0/power_dpm_force_performance_level": "manual\n",
		"sys/bus/pci/devices/0000:c1:00.0/pp_dpm_sclk":                       "0: 500Mhz\n1: 1500Mhz *\n",
		"sys/bus/pci/devices/0000:03:00.0/vendor":                            "0x1002\n",
		"sys/bus/pci/devices/0000:03:00.0/device":                            "0x7448\n",
		"sys/bus/pci/devices/0000:03:00.0/power_dpm_force_performance_level": "auto\n",
		"sys/bus/pci/devices/0000:03:00.0/pp_dpm_sclk":                       "0: 500Mhz *\n1: 1200Mhz\n",
		"sys/bus/pci/devices/0000:03:00.0/pp_dpm_mclk":                       "0: 400Mhz *\n1: 1000Mhz\n",
		"sys/bus/pci/devices/0000:03:00.0/pp_dpm_fclk":                       "0: 600Mhz *\n1: 1400Mhz\n",
		"sys/bus/pci/devices/0000:03:00.0/pp_dpm_socclk":                     "0: 600Mhz *\n1: 1000Mhz\n",
		"sys/bus/pci/devices/0000:03:00.0/pp_dpm_pcie":                       "0: 2.5GT/s, x1 619Mhz\n1: 16.0GT/s, x16 619Mhz *\n",
		"sys/bus/pci/devices/0000:5e:00.0/vendor":                            "0x10de\n",
		"sys/bus/pci/devices/0000:5e:00.0/device":                            "0x2330\n",
	}
	for _, pol := range []string{"policy0", "policy1"} {
		dir := "sys/devices/system/cpu/cpufreq/" + pol + "/"
		files[dir+"scaling_driver"] = "amd-pstate-epp\n"
		files[dir+"scaling_governor"] = "powersave\n"
		files[dir+"scaling_available_governors"] = "performance powersave\n"
		files[dir+"energy_performance_preference"] = "balance_performance\n"
		files[dir+"energy_performance_available_preferences"] = "default performance balance_performance balance_power power \n"
		files[dir+"boost"] = "1\n"
		files[dir+"cpuinfo_min_freq"] = "400000\n"
		files[dir+"cpuinfo_max_freq"] = "1500000\n"
		files[dir+"scaling_max_freq"] = "1500000\n"
		files[dir+"scaling_setspeed"] = "<unsupported>\n"
	}
	return files
}

// collectTools writes the fake tools and returns their directory: nvidia-smi
// answers the agent's inventory query and the clock query, rocm-smi and
// kubectl fail as on a node without them.
func collectTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	clocks := filepath.Join(bin, "clocks.txt")
	if err := os.WriteFile(clocks, []byte(collectNvidiaClocks), 0o644); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"nvidia-smi": "#!/bin/sh\ncase \"$*\" in\n" +
			"\"-q -d CLOCK,SUPPORTED_CLOCKS,PERFORMANCE\") cat '" + clocks + "' ;;\n" +
			"--query-gpu=*) printf '0, 100.00, 300.00, 300.00, 50.00, Neutral GPU\\n' ;;\n" +
			"*) exit 9 ;;\nesac\n",
		"rocm-smi": "#!/bin/sh\nexit 1\n",
		"kubectl":  "#!/bin/sh\nexit 1\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// runCollect runs the script with the fake tools first on PATH and NODE_NAME
// unset, and returns its combined output and exit status.
func runCollect(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{collectScript}, args...)...)
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "NODE_NAME=") && !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("run %s: %v\n%s", collectScript, err, out)
	return "", 0
}

// collectCapture runs a capture of a synthetic host and returns the capture
// directory, the run's output and its exit status.
func collectCapture(t *testing.T, host map[string]string, args ...string) (dir, out string, code int) {
	t.Helper()
	root := t.TempDir()
	collectFiles(t, root, host)
	outDir := filepath.Join(t.TempDir(), "out")
	args = append([]string{"--name", "synthetic", "--out", outDir,
		"--sysfs", filepath.Join(root, "sys"), "--procfs", filepath.Join(root, "proc")}, args...)
	out, code = runCollect(t, collectTools(t), args...)
	return filepath.Join(outDir, "synthetic"), out, code
}

func collectRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// GF01: on a synthetic host the script copies the governor state: every
// policy's governor, preference pair, boost and frequency range, the global
// boost, both pstate status files, CPU 0's idle state names, the level and
// clock tables of each amdgpu device under its position in bus id order, and
// the NVIDIA clock query with every bus id and serial redacted. It copies
// nothing else from those directories and writes the tar.
func TestGF01CaptureCopiesTheGovernorState(t *testing.T) {
	host := collectHostFiles()
	dir, out, code := collectCapture(t, host)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	want := map[string]string{
		"cpufreq/boost":         "sys/devices/system/cpu/cpufreq/boost",
		"intel_pstate/status":   "sys/devices/system/cpu/intel_pstate/status",
		"intel_pstate/no_turbo": "sys/devices/system/cpu/intel_pstate/no_turbo",
		"amd_pstate/status":     "sys/devices/system/cpu/amd_pstate/status",
		"cpuidle/state0/name":   "sys/devices/system/cpu/cpu0/cpuidle/state0/name",
		"cpuidle/state1/name":   "sys/devices/system/cpu/cpu0/cpuidle/state1/name",
	}
	for _, pol := range []string{"policy0", "policy1"} {
		for _, f := range []string{"scaling_governor", "scaling_available_governors", "energy_performance_preference",
			"energy_performance_available_preferences", "boost", "cpuinfo_min_freq", "cpuinfo_max_freq"} {
			want["cpufreq/"+pol+"/"+f] = "sys/devices/system/cpu/cpufreq/" + pol + "/" + f
		}
	}
	// 0000:03:00.0 sorts before 0000:c1:00.0, so it is gpu0.
	for gpu, bus := range map[string]string{"gpu0": "0000:03:00.0", "gpu1": "0000:c1:00.0"} {
		for _, f := range []string{"device", "power_dpm_force_performance_level", "pp_dpm_sclk", "pp_dpm_mclk", "pp_dpm_fclk", "pp_dpm_socclk", "pp_dpm_pcie"} {
			if _, ok := host["sys/bus/pci/devices/"+bus+"/"+f]; ok {
				want["amdgpu/"+gpu+"/"+f] = "sys/bus/pci/devices/" + bus + "/" + f
			}
		}
	}
	for rel, src := range want {
		if got := collectRead(t, filepath.Join(dir, "governor", rel)); got != host[src] {
			t.Errorf("governor/%s = %q, want the host's %q", rel, got, host[src])
		}
	}

	var got []string
	_ = filepath.WalkDir(filepath.Join(dir, "governor"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(dir, "governor"), p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	for _, rel := range got {
		if _, ok := want[rel]; !ok && rel != "nvidia-smi-clocks.txt" {
			t.Errorf("governor/%s was copied; the capture holds only the governor files", rel)
		}
	}

	clocks := collectRead(t, filepath.Join(dir, "governor", "nvidia-smi-clocks.txt"))
	for _, leak := range []string{"3B:00.0", "5E:00.0", "1650123456789"} {
		if strings.Contains(clocks, leak) {
			t.Errorf("nvidia-smi-clocks.txt still carries %q:\n%s", leak, clocks)
		}
	}
	for _, kept := range []string{"GPU BUSID-REDACTED", "Graphics                          : 300 MHz", "Performance State                     : P8"} {
		if !strings.Contains(clocks, kept) {
			t.Errorf("nvidia-smi-clocks.txt lost %q:\n%s", kept, clocks)
		}
	}
	if _, err := os.Stat(dir + ".tar"); err != nil {
		t.Errorf("no tar written: %v", err)
	}
}

// GF01: a host without cpufreq, pstate, cpuidle or amdgpu, as a VM is, gets
// no governor directory at all: with an nvidia-smi whose queries fail, which
// the fake on PATH makes hermetic on any host, and with no nvidia-smi, which
// runs only where none is installed.
func TestGF01CaptureWithoutGovernorStateWritesNoDirectory(t *testing.T) {
	run := func(t *testing.T, bin string) {
		t.Helper()
		root := t.TempDir()
		collectFiles(t, root, map[string]string{"proc/cpuinfo": "processor\t: 0\nmodel name\t: Neutral CPU\n\n", "sys/class/.keep": ""})
		outDir := filepath.Join(t.TempDir(), "out")
		out, code := runCollect(t, bin, "--name", "vm", "--out", outDir, "--sysfs", filepath.Join(root, "sys"), "--procfs", filepath.Join(root, "proc"))
		if code != 0 {
			t.Fatalf("exit %d, want 0:\n%s", code, out)
		}
		if _, err := os.Stat(filepath.Join(outDir, "vm", "governor")); !os.IsNotExist(err) {
			t.Errorf("governor/ exists on a host with no governor state (err %v)", err)
		}
	}
	t.Run("nvidia-smi fails", func(t *testing.T) {
		bin := collectTools(t)
		if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, bin)
	})
	t.Run("no nvidia-smi", func(t *testing.T) {
		bin := collectTools(t)
		if err := os.Remove(filepath.Join(bin, "nvidia-smi")); err != nil {
			t.Fatal(err)
		}
		if path, err := exec.LookPath("nvidia-smi"); err == nil {
			t.Skipf("%s is on PATH, so the run would query it", path)
		}
		run(t, bin)
	})
}

// GF03: the identifier check rejects a PCI bus id that reached a governor
// file: the capture exits 1, names the file and writes no tar.
func TestGF03CaptureRejectsABusIDInAGovernorFile(t *testing.T) {
	host := collectHostFiles()
	host["sys/bus/pci/devices/0000:03:00.0/pp_dpm_sclk"] = "0: 500Mhz *\n1: 1200Mhz 0000:03:00.0\n"
	dir, out, code := collectCapture(t, host)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "governor/amdgpu/gpu0/pp_dpm_sclk:2: contains a PCI bus id") {
		t.Errorf("the output does not name the file, the line and the bus id:\n%s", out)
	}
	if _, err := os.Stat(dir + ".tar"); !os.IsNotExist(err) {
		t.Errorf("a tar was written for a capture the check rejected (err %v)", err)
	}
}

// GF03: --check on an existing capture rejects a host name, a bus id without
// its domain and a serial number in a governor file, and accepts the
// placeholders the redaction writes.
func TestGF03CheckRejectsIdentifiersInGovernorFiles(t *testing.T) {
	bin := collectTools(t)
	cases := []struct {
		name, rel, body, want string
	}{
		{"host name", "governor/cpufreq/policy0/scaling_governor", "schedutil fixture-node-17\n", "contains the host name fixture-node-17"},
		{"host name in another case", "governor/amd_pstate/status", "FIXTURE-NODE-17\n", "contains the host name fixture-node-17"},
		{"bus id without a domain", "governor/amdgpu/gpu0/pp_dpm_pcie", "0: 16.0GT/s, x16 619Mhz c1:00.0\n", "contains a PCI bus id"},
		{"serial number", "governor/nvidia-smi-clocks.txt", "    Serial Number : 1650123456789\n", "contains a serial number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			collectFiles(t, dir, map[string]string{tc.rel: tc.body})
			out, code := runCollect(t, bin, "--check", dir, "--node", "fixture-node-17")
			if code != 1 {
				t.Fatalf("exit %d, want 1:\n%s", code, out)
			}
			if !strings.Contains(out, tc.rel+":1: "+tc.want) {
				t.Errorf("the output does not say %q for %s:\n%s", tc.want, tc.rel, out)
			}
		})
	}

	t.Run("placeholders and governor words pass", func(t *testing.T) {
		dir := t.TempDir()
		collectFiles(t, dir, map[string]string{
			"governor/nvidia-smi-clocks.txt":                         "GPU BUSID-REDACTED\n    Serial Number : SERIAL-REDACTED\n    Graphics : 300 MHz\n",
			"governor/cpufreq/policy0/scaling_governor":              "schedutil\n",
			"governor/amdgpu/gpu0/pp_dpm_pcie":                       "0: 2.5GT/s, x1 619Mhz\n1: 16.0GT/s, x16 619Mhz *\n",
			"governor/cpufreq/policy0/cpuinfo_max_freq":              "1500000\n",
			"governor/amdgpu/gpu0/power_dpm_force_performance_level": "profile_peak\n",
		})
		out, code := runCollect(t, bin, "--check", dir, "--node", "fixture-node-17")
		if code != 0 {
			t.Fatalf("exit %d, want 0:\n%s", code, out)
		}
	})
}
