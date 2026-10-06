package dvfs

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func withHostSysRoot(t *testing.T, root string) {
	t.Helper()
	old := HostSysRoot
	HostSysRoot = root
	t.Cleanup(func() { HostSysRoot = old })
}

// writeSysFile writes one sysfs attribute under root, value plus newline as
// the kernel prints it, creating its directory.
func writeSysFile(t *testing.T, root, rel, value string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkSys creates the relative symlink rel -> target under root, the way sysfs
// links cpuN/cpufreq to its policy and class/powercap entries to devices.
func linkSys(t *testing.T, root, rel, target string) {
	t.Helper()
	link := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestD01HostSysRootDefaultsToHostSys pins the production paths: with the
// default roots, every glob and message is the literal the agent used before
// HostSysRoot existed.
func TestD01HostSysRootDefaultsToHostSys(t *testing.T) {
	if HostSysRoot != "/host-sys" {
		t.Fatalf("HostSysRoot=%q want /host-sys", HostSysRoot)
	}
	if PowercapRoot != "/host-sys/class/powercap" {
		t.Fatalf("PowercapRoot=%q want /host-sys/class/powercap", PowercapRoot)
	}

	wantCPUFreq := []string{
		"/host-sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq",
		"/host-sys/devices/system/cpu/cpufreq/policy*/scaling_max_freq",
	}
	if got := cpuFreqGlobs(); !reflect.DeepEqual(got, wantCPUFreq) {
		t.Fatalf("cpuFreqGlobs=%q want %q", got, wantCPUFreq)
	}
	wantEnergy := []string{
		"/host-sys/class/powercap/*/energy_uj",
		"/host-sys/class/powercap/*:*/energy_uj",
		"/host-sys/class/powercap/*:*:*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/*/energy_uj",
	}
	if got := energyFileGlobs(); !reflect.DeepEqual(got, wantEnergy) {
		t.Fatalf("energyFileGlobs=%q want %q", got, wantEnergy)
	}

	_, err := (&Controller{}).applyThrottlePct(50, nil, 0)
	wantErr := "no cpufreq scaling_max_freq files found under /host-sys/devices/system/cpu"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("applyThrottlePct error=%v want %q", err, wantErr)
	}

	t.Run("messages", func(t *testing.T) {
		// New lists the real host's cpufreq files under the default root, so
		// the wording is checked around an empty tree; the root is pinned above.
		// A temp root also tells a HostSysRoot-built message from the literal.
		root := t.TempDir()
		withHostSysRoot(t, root)

		_, err := (&Controller{}).applyThrottlePct(50, nil, 0)
		wantErr := "no cpufreq scaling_max_freq files found under " + root + "/devices/system/cpu"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("applyThrottlePct error=%v want %q", err, wantErr)
		}

		var buf bytes.Buffer
		prevOut, prevFlags := log.Writer(), log.Flags()
		log.SetOutput(&buf)
		log.SetFlags(0)
		t.Cleanup(func() {
			log.SetOutput(prevOut)
			log.SetFlags(prevFlags)
		})

		if _, err := New(Config{}, nil); err != nil {
			t.Fatalf("New: %v", err)
		}
		want := "warning: no cpufreq files found under " + root + "/devices/system/cpu; host DVFS writes disabled, HTTP control can still be used\n"
		if buf.String() != want {
			t.Fatalf("log=%q want %q", buf.String(), want)
		}
	})
}

// TestD02CPUFreqListUnderHostSysRootListsCPUAndPolicyEntries replays the sysfs
// cpufreq layout, where each cpuN/cpufreq is a link to its policy directory,
// under a temporary HostSysRoot. Every CPU is listed twice, once per path,
// which is what CPUFreqList does on a real host today.
func TestD02CPUFreqListUnderHostSysRootListsCPUAndPolicyEntries(t *testing.T) {
	root := t.TempDir()
	want := map[string]int{}
	for cpu := 0; cpu < 2; cpu++ {
		policy := fmt.Sprintf("devices/system/cpu/cpufreq/policy%d", cpu)
		writeSysFile(t, root, policy+"/scaling_max_freq", "3000000")
		writeSysFile(t, root, policy+"/scaling_cur_freq", "2000000")
		writeSysFile(t, root, policy+"/cpuinfo_min_freq", "800000")
		writeSysFile(t, root, policy+"/cpuinfo_max_freq", "3000000")
		link := fmt.Sprintf("devices/system/cpu/cpu%d/cpufreq", cpu)
		linkSys(t, root, link, fmt.Sprintf("../cpufreq/policy%d", cpu))
		want[filepath.Join(root, link, "scaling_max_freq")] = cpu
		want[filepath.Join(root, policy, "scaling_max_freq")] = cpu
	}
	withHostSysRoot(t, root)

	cpus, err := CPUFreqList()
	if err != nil {
		t.Fatalf("CPUFreqList: %v", err)
	}
	got := map[string]int{}
	for i, c := range cpus {
		if i > 0 && cpus[i-1].Index > c.Index {
			t.Fatalf("entries not sorted by index: %+v", cpus)
		}
		if c.MinKHz != 800000 || c.MaxKHz != 3000000 {
			t.Fatalf("%s: min=%d max=%d want 800000 3000000", c.MaxFile, c.MinKHz, c.MaxKHz)
		}
		if wantCur := filepath.Join(filepath.Dir(c.MaxFile), "scaling_cur_freq"); c.CurFile != wantCur {
			t.Fatalf("CurFile=%s want %s", c.CurFile, wantCur)
		}
		got[c.MaxFile] = c.Index
	}
	if len(cpus) != len(want) || !reflect.DeepEqual(got, want) {
		t.Fatalf("CPUFreqList listed %d entries %v, want %d %v. Today it lists every CPU through its cpuN/cpufreq link "+
			"and its policy directory (D6 in cmd/agent/hwemu_test.go); F1 dedupes them, so update want with it",
			len(cpus), got, len(want), want)
	}
}

// TestD03EnergyFilesUnderHostSysRootMatchesDevicePowercapGlobs builds the
// sysfs powercap layout, devices/virtual/powercap plus class/powercap links,
// under a temporary HostSysRoot.
func TestD03EnergyFilesUnderHostSysRootMatchesDevicePowercapGlobs(t *testing.T) {
	root := t.TempDir()
	control := "devices/virtual/powercap/intel-rapl"
	linkSys(t, root, "class/powercap/intel-rapl", "../../"+control)
	var devicePaths, classPaths []string
	for pkg := 0; pkg < 2; pkg++ {
		zone := fmt.Sprintf("intel-rapl:%d", pkg)
		dram := fmt.Sprintf("intel-rapl:%d:0", pkg)
		writeSysFile(t, root, control+"/"+zone+"/name", fmt.Sprintf("package-%d", pkg))
		writeSysFile(t, root, control+"/"+zone+"/energy_uj", "1000")
		writeSysFile(t, root, control+"/"+zone+"/"+dram+"/name", "dram")
		writeSysFile(t, root, control+"/"+zone+"/"+dram+"/energy_uj", "1000")
		linkSys(t, root, "class/powercap/"+zone, "../../"+control+"/"+zone)
		linkSys(t, root, "class/powercap/"+dram, "../../"+control+"/"+zone+"/"+dram)
		devicePaths = append(devicePaths, filepath.Join(root, control, zone, "energy_uj"))
		classPaths = append(classPaths, filepath.Join(root, "class/powercap", zone, "energy_uj"))
	}
	withHostSysRoot(t, root)

	t.Run("device globs", func(t *testing.T) {
		// With PowercapRoot pointing nowhere, only the HostSysRoot globs can
		// match, and the dram sub-zones are filtered out.
		withPowercapRoot(t, filepath.Join(t.TempDir(), "absent-powercap"))
		files, err := EnergyFiles()
		if err != nil {
			t.Fatalf("EnergyFiles: %v", err)
		}
		if !reflect.DeepEqual(files, devicePaths) {
			t.Fatalf("EnergyFiles=%q want %q", files, devicePaths)
		}
	})

	t.Run("class links too", func(t *testing.T) {
		// EnergyFiles dedupes by path string, so a package reached through its
		// class link and through its devices path is listed under both.
		withPowercapRoot(t, filepath.Join(root, "class/powercap"))
		files, err := EnergyFiles()
		if err != nil {
			t.Fatalf("EnergyFiles: %v", err)
		}
		want := append(append([]string{}, classPaths...), devicePaths...)
		if !reflect.DeepEqual(files, want) {
			t.Fatalf("EnergyFiles=%q want %q. Today it lists each package through its class link and its devices path "+
				"(D5 in cmd/agent/hwemu_test.go); F1 dedupes them, so update want with it", files, want)
		}
	})
}
