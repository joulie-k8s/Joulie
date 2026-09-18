package dvfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUIndexFromPathCPU(t *testing.T) {
	idx, ok := CPUIndexFromPath("/host-sys/devices/system/cpu/cpu3/cpufreq")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if idx != 3 {
		t.Errorf("expected idx=3, got %d", idx)
	}
}

func TestCPUIndexFromPathPolicy(t *testing.T) {
	idx, ok := CPUIndexFromPath("/host-sys/devices/system/cpu/cpufreq/policy7")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if idx != 7 {
		t.Errorf("expected idx=7, got %d", idx)
	}
}

func TestCPUIndexFromPathInvalid(t *testing.T) {
	_, ok := CPUIndexFromPath("/tmp/random/path")
	if ok {
		t.Error("expected ok=false for invalid path")
	}
}

func TestCPUIndexFromPathNonNumeric(t *testing.T) {
	_, ok := CPUIndexFromPath("/host-sys/devices/system/cpu/cpuXYZ/cpufreq")
	if ok {
		t.Error("expected ok=false for non-numeric CPU index")
	}
}

func TestIsPackageEnergyFileValid(t *testing.T) {
	// Package-level: exactly one colon (e.g. intel-rapl:0)
	if !IsPackageEnergyFile("/host-sys/class/powercap/intel-rapl:0/energy_uj") {
		t.Error("expected true for package-level energy file")
	}
}

func TestIsPackageEnergyFileSubdomain(t *testing.T) {
	// Sub-domain: two colons (e.g. intel-rapl:0:1) - should be rejected
	if IsPackageEnergyFile("/host-sys/class/powercap/intel-rapl:0:1/energy_uj") {
		t.Error("expected false for sub-domain energy file")
	}
}

func TestIsPackageEnergyFileNoColon(t *testing.T) {
	if IsPackageEnergyFile("/host-sys/class/powercap/intel-rapl/energy_uj") {
		t.Error("expected false for top-level (no colon) energy file")
	}
}

func TestControllerActive(t *testing.T) {
	c := &Controller{ThrottlePct: 0}
	if c.Active() {
		t.Error("expected Active()=false when ThrottlePct=0")
	}
	c.ThrottlePct = 10
	if !c.Active() {
		t.Error("expected Active()=true when ThrottlePct=10")
	}
}

func TestControllerHasHostControl(t *testing.T) {
	c := &Controller{Cpus: nil}
	if c.HasHostControl() {
		t.Error("expected HasHostControl()=false with no CPUs")
	}
	c.Cpus = []CPU{{Index: 0}}
	if !c.HasHostControl() {
		t.Error("expected HasHostControl()=true with CPUs")
	}
}

func TestControllerSetThrottlePct(t *testing.T) {
	c := &Controller{}
	c.SetThrottlePct(42)
	if c.ThrottlePct != 42 {
		t.Errorf("expected ThrottlePct=42, got %d", c.ThrottlePct)
	}
}

func TestApplyThrottlePctInvalidRange(t *testing.T) {
	c := &Controller{Cpus: []CPU{{Index: 0, MinKHz: 1000, MaxKHz: 3000}}}
	_, err := c.applyThrottlePct(-1, nil, 0)
	if err == nil {
		t.Error("expected error for negative throttle pct")
	}
	_, err = c.applyThrottlePct(101, nil, 0)
	if err == nil {
		t.Error("expected error for throttle pct > 100")
	}
}

func TestApplyThrottlePctNoCPUs(t *testing.T) {
	c := &Controller{Cpus: []CPU{}}
	_, err := c.applyThrottlePct(50, nil, 0)
	if err == nil {
		t.Error("expected error when no CPUs available")
	}
}

type mockPowerReader struct {
	watts float64
	ok    bool
	err   error
}

func (m *mockPowerReader) ReadPowerWatts() (float64, bool, error) {
	return m.watts, m.ok, m.err
}

func TestReadPowerWattsWithReader(t *testing.T) {
	c := &Controller{
		PowerReader: &mockPowerReader{watts: 150.0, ok: true, err: nil},
	}
	watts, ok, err := c.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if watts != 150.0 {
		t.Errorf("expected 150W, got %f", watts)
	}
}

func TestReadPowerWattsReaderNotOK(t *testing.T) {
	c := &Controller{
		PowerReader: &mockPowerReader{watts: 0, ok: false, err: nil},
	}
	_, ok, err := c.ReadPowerWatts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false")
	}
}

// writeRAPLFixture builds a powercap tree: zones maps a zone directory name to
// the contents of its `name` file ("" means the zone has no name file), and
// every zone gets the usual constraint files.
func writeRAPLFixture(t *testing.T, zones map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for zone, name := range zones {
		dir := filepath.Join(root, zone)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if name != "" {
			if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range []string{"constraint_0_power_limit_uw", "constraint_0_max_power_uw", "constraint_0_min_power_uw"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("1000000"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func withPowercapRoot(t *testing.T, root string) {
	t.Helper()
	old := PowercapRoot
	PowercapRoot = root
	t.Cleanup(func() { PowercapRoot = old })
}

func TestRAPLPackageZonesSkipsSubZonesAndPsys(t *testing.T) {
	root := writeRAPLFixture(t, map[string]string{
		"intel-rapl:0":   "package-0",
		"intel-rapl:0:0": "dram",
		"intel-rapl:1":   "package-1",
		"intel-rapl:1:0": "dram",
		"intel-rapl:2":   "psys",
	})
	withPowercapRoot(t, root)

	zones, err := RAPLPackageZones()
	if err != nil {
		t.Fatalf("RAPLPackageZones: %v", err)
	}
	want := []string{filepath.Join(root, "intel-rapl:0"), filepath.Join(root, "intel-rapl:1")}
	if len(zones) != len(want) {
		t.Fatalf("zones=%v want=%v", zones, want)
	}
	for i := range want {
		if zones[i] != want[i] {
			t.Fatalf("zones=%v want=%v", zones, want)
		}
	}
}

func TestRAPLPackageZonesFallsBackToColonCountWithoutNameFiles(t *testing.T) {
	root := writeRAPLFixture(t, map[string]string{
		"intel-rapl:0":   "",
		"intel-rapl:0:0": "",
		"intel-rapl:1":   "",
	})
	withPowercapRoot(t, root)

	zones, err := RAPLPackageZones()
	if err != nil {
		t.Fatalf("RAPLPackageZones: %v", err)
	}
	if len(zones) != 2 {
		t.Fatalf("zones=%v want 2 package zones", zones)
	}
}

func TestRAPLCapFilesTargetsPackageZonesOnly(t *testing.T) {
	root := writeRAPLFixture(t, map[string]string{
		"intel-rapl:0":   "package-0",
		"intel-rapl:0:0": "dram",
		"intel-rapl:1":   "package-1",
		"intel-rapl:1:0": "dram",
	})
	withPowercapRoot(t, root)

	files, err := RAPLCapFiles()
	if err != nil {
		t.Fatalf("RAPLCapFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%v want only the two package limit files", files)
	}
	for _, f := range files {
		if strings.Contains(filepath.Base(filepath.Dir(f)), ":0:") {
			t.Fatalf("cap file targets a sub-zone: %s", f)
		}
	}
}
