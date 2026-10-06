package layout

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func layoutIsCleanRelative(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

// A link that leaves sys/ would resolve against the pod's own filesystem at
// /host-sys, not against the emulated tree. Each link is checked lexically and
// then created on disk and resolved, as the agent's reads resolve it.
func TestL01HelpersReturnCleanRelativePathsAndLinksStayInsideSys(t *testing.T) {
	paths := map[string]string{
		"SysDir":                      SysDir,
		"CPUInfoFile":                 CPUInfoFile,
		"StateDir":                    StateDir,
		"ToolsFile":                   ToolsFile,
		"NVMLDir":                     NVMLDir,
		"TmpDir":                      TmpDir,
		"DevNvidiactl":                DevNvidiactl,
		"BinDir":                      BinDir,
		"PowercapControlDir":          PowercapControlDir("intel-rapl"),
		"PowercapZoneDir()":           PowercapZoneDir("intel-rapl"),
		"PowercapZoneDir(0)":          PowercapZoneDir("intel-rapl", 0),
		"PowercapZoneDir(1,0)":        PowercapZoneDir("intel-rapl", 1, 0),
		"PowercapZoneDir(mmio 0)":     PowercapZoneDir("intel-rapl-mmio", 0),
		"PolicyDir":                   PolicyDir(47),
		"HSMPHwmonDir":                HSMPHwmonDir(1),
		"PCIDevDir":                   PCIDevDir(0x17),
		"DRMCardDir":                  DRMCardDir(0x17, 8),
		"NVMLGPUDir":                  NVMLGPUDir(7),
		"PowercapZoneDir(16 sockets)": PowercapZoneDir("intel-rapl", 15, 0),
	}
	for name, p := range paths {
		if !layoutIsCleanRelative(p) {
			t.Errorf("%s = %q, want a clean relative path", name, p)
		}
	}
	if !path.IsAbs(FakeSMIInContainer) || path.Clean(FakeSMIInContainer) != FakeSMIInContainer {
		t.Errorf("FakeSMIInContainer = %q, want a clean absolute path inside the container", FakeSMIInContainer)
	}

	type linkCase struct{ name, link, target, want string }
	var links []linkCase
	add := func(name, link, target, want string) {
		links = append(links, linkCase{name, link, target, want})
	}
	l, tg := PowercapClassLink("intel-rapl")
	add("class intel-rapl", l, tg, PowercapControlDir("intel-rapl"))
	l, tg = PowercapClassLink("intel-rapl", 0)
	add("class intel-rapl:0", l, tg, PowercapZoneDir("intel-rapl", 0))
	l, tg = PowercapClassLink("intel-rapl", 1, 0)
	add("class intel-rapl:1:0", l, tg, PowercapZoneDir("intel-rapl", 1, 0))
	l, tg = PowercapClassLink("intel-rapl-mmio", 0)
	add("class intel-rapl-mmio:0", l, tg, PowercapZoneDir("intel-rapl-mmio", 0))
	l, tg = CPUFreqLink(0, 0)
	add("cpu0 cpufreq", l, tg, PolicyDir(0))
	l, tg = CPUFreqLink(95, 47)
	add("cpu95 cpufreq", l, tg, PolicyDir(47))
	l, tg = HwmonClassLink(1, "devices/platform/amd_hsmp")
	add("class hwmon1 (HSMP)", l, tg, HSMPHwmonDir(1))
	l, tg = HwmonClassLink(9, PCIDevDir(0x17))
	add("class hwmon9 (PCI)", l, tg, PCIDevDir(0x17)+"/hwmon/hwmon9")

	sys := filepath.Join(t.TempDir(), SysDir)
	for _, c := range links {
		if !layoutIsCleanRelative(c.link) {
			t.Errorf("%s: link %q, want a clean relative path", c.name, c.link)
			continue
		}
		if path.IsAbs(c.target) {
			t.Errorf("%s: target %q is absolute, want relative to the link", c.name, c.target)
			continue
		}
		if got := path.Join(path.Dir(c.link), c.target); got != c.want || !strings.HasPrefix(got, "devices/") {
			t.Errorf("%s: %q -> %q resolves to %q, want %q under devices/", c.name, c.link, c.target, got, c.want)
			continue
		}
		if err := os.MkdirAll(filepath.Join(sys, c.want), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sys, path.Dir(c.link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(c.target, filepath.Join(sys, c.link)); err != nil {
			t.Fatal(err)
		}
		got, err := filepath.EvalSymlinks(filepath.Join(sys, c.link))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want, err := filepath.EvalSymlinks(filepath.Join(sys, c.want))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: resolves on disk to %q, want %q", c.name, got, want)
		}
	}
}

// The agent globs these exact paths, so they come from the kernel docs, not
// from the agent's constants. Zone names carry the index in hex
// (powercap_sys.c:553), which only shows from index 10 on.
func TestL02PathsMatchTheKernelLayout(t *testing.T) {
	dirs := []struct{ name, got, want string }{
		{"control intel-rapl", PowercapControlDir("intel-rapl"), "devices/virtual/powercap/intel-rapl"},
		{"zone intel-rapl 0", PowercapZoneDir("intel-rapl", 0), "devices/virtual/powercap/intel-rapl/intel-rapl:0"},
		{"zone intel-rapl 0 0", PowercapZoneDir("intel-rapl", 0, 0), "devices/virtual/powercap/intel-rapl/intel-rapl:0/intel-rapl:0:0"},
		{"zone intel-rapl-mmio 0", PowercapZoneDir("intel-rapl-mmio", 0), "devices/virtual/powercap/intel-rapl-mmio/intel-rapl-mmio:0"},
		{"zone intel-rapl 10", PowercapZoneDir("intel-rapl", 10), "devices/virtual/powercap/intel-rapl/intel-rapl:a"},
		{"policy 3", PolicyDir(3), "devices/system/cpu/cpufreq/policy3"},
		// hwmon devices sit under their parent device (hsmp/plat.c:198 for HSMP).
		{"hsmp hwmon 1", HSMPHwmonDir(1), "devices/platform/amd_hsmp/hwmon/hwmon1"},
		{"pci bus 0x10", PCIDevDir(0x10), "devices/pci0000:10/0000:10:00.0"},
		{"drm card 1", DRMCardDir(0x10, 1), "devices/pci0000:10/0000:10:00.0/drm/card1"},
		{"nvml gpu 7", NVMLGPUDir(7), "nvml/gpu7"},
	}
	for _, c := range dirs {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	type pair struct{ link, target string }
	links := []struct {
		name string
		got  pair
		want pair
	}{
		{"class intel-rapl", layoutPair(PowercapClassLink("intel-rapl")),
			pair{"class/powercap/intel-rapl", "../../devices/virtual/powercap/intel-rapl"}},
		{"class intel-rapl:0", layoutPair(PowercapClassLink("intel-rapl", 0)),
			pair{"class/powercap/intel-rapl:0", "../../devices/virtual/powercap/intel-rapl/intel-rapl:0"}},
		{"class intel-rapl:0:0", layoutPair(PowercapClassLink("intel-rapl", 0, 0)),
			pair{"class/powercap/intel-rapl:0:0", "../../devices/virtual/powercap/intel-rapl/intel-rapl:0/intel-rapl:0:0"}},
		{"class intel-rapl-mmio:0", layoutPair(PowercapClassLink("intel-rapl-mmio", 0)),
			pair{"class/powercap/intel-rapl-mmio:0", "../../devices/virtual/powercap/intel-rapl-mmio/intel-rapl-mmio:0"}},
		{"cpu5 cpufreq", layoutPair(CPUFreqLink(5, 4)),
			pair{"devices/system/cpu/cpu5/cpufreq", "../cpufreq/policy4"}},
		{"class hwmon2", layoutPair(HwmonClassLink(2, PCIDevDir(0x10))),
			pair{"class/hwmon/hwmon2", "../../devices/pci0000:10/0000:10:00.0/hwmon/hwmon2"}},
	}
	for _, c := range links {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

func layoutPair(link, target string) struct{ link, target string } {
	return struct{ link, target string }{link, target}
}

// fakesmi reads one envelope whatever the family, so a node with no GPU tool
// still gets a parsable file with an empty map.
func TestL03ToolsManifestHasOneEnvelopeForEveryFamily(t *testing.T) {
	cases := []struct {
		name string
		m    ToolsManifest
		want string
	}{
		{"no tools", ToolsManifest{SchemaVersion: 1, Tools: map[string]ToolEntry{}},
			`{"schemaVersion":1,"tools":{}}`},
		{"nvidia", ToolsManifest{SchemaVersion: 1, Tools: map[string]ToolEntry{"nvidia-smi": {Variant: "default"}}},
			`{"schemaVersion":1,"tools":{"nvidia-smi":{"variant":"default"}}}`},
		{"amd", ToolsManifest{SchemaVersion: 1, Tools: map[string]ToolEntry{"rocm-smi": {Variant: "current"}, "amd-smi": {Variant: "7.2"}}},
			`{"schemaVersion":1,"tools":{"amd-smi":{"variant":"7.2"},"rocm-smi":{"variant":"current"}}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.m)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s: marshals to %s, want %s", c.name, b, c.want)
		}
		var back ToolsManifest
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(back, c.m) {
			t.Errorf("%s: round trip gives %+v, want %+v", c.name, back, c.m)
		}
	}
}
