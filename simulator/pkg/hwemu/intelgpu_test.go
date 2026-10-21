package hwemu

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	corev1 "k8s.io/api/core/v1"
)

// intelProfile is the Flex 170 profile: four GPUs under i915, the limit
// starting at the 150 W TDP, the hardware clamping any limit to 40 to
// 150 W (assumed), in units of 1/8 W.
const intelProfile = "intel-flex-170-4gpu"

// intelHwmon is attribute name of the hwmon device of Intel GPU i, as
// renderIntelGPUs lays it out: on bus 0x10+i, hwmon i+2.
func intelHwmon(i int, name string) string {
	return path.Join(layout.SysDir, layout.PCIDevDir(0x10+i), "hwmon", "hwmon"+strconv.Itoa(i+2), name)
}

// H47: each Flex 170 is a PCI function with vendor 0x8086, device 0x56c0
// and class 0x038000, bound to i915 (driver link and the driver's link
// back), listed under bus/pci/devices, with one hwmon device named i915:
// power1_max at the TDP (A, mode 0664), power1_rated_max at the TDP and
// power1_max_interval (R), and energy1_input from 0 (S). i915 has no power
// sensor and no power1_cap, and DG2 no per-tile hwmon device. The Node
// carries NFD's pci-0380_8086, the Intel NFD rule labels and 4
// gpu.intel.com/i915.
func TestH47IntelGPUTreeAndNode(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, intelProfile)
	tr := renderTestTree(t, p)
	owners := map[string]layout.FileSpec{}
	for _, f := range tr.Files {
		owners[f.Rel] = f
	}
	for i := 0; i < p.GPUs.Count.V; i++ {
		dev := path.Join(layout.SysDir, layout.PCIDevDir(0x10+i))
		for name, want := range map[string]string{"vendor": "0x8086\n", "device": "0x56c0\n", "class": "0x038000\n"} {
			if got := nodeRead(t, tr, path.Join(dev, name)); got != want {
				t.Errorf("gpu %d %s = %q, want %q", i, name, got, want)
			}
		}
		drv, err := filepath.EvalSymlinks(filepath.Join(tr.Root, dev, "driver"))
		if err != nil || drv != filepath.Join(tr.SysRoot, "bus", "pci", "drivers", "i915") {
			t.Errorf("gpu %d driver resolves to %q (%v), want sys/bus/pci/drivers/i915", i, drv, err)
		}
		back, err := filepath.EvalSymlinks(filepath.Join(tr.SysRoot, "bus", "pci", "drivers", "i915", path.Base(dev)))
		if err != nil || back != filepath.Join(tr.Root, dev) {
			t.Errorf("gpu %d: the driver's entry resolves to %q (%v), want the device", i, back, err)
		}
		listed, err := filepath.EvalSymlinks(filepath.Join(tr.SysRoot, "bus", "pci", "devices", path.Base(dev)))
		if err != nil || listed != filepath.Join(tr.Root, dev) {
			t.Errorf("gpu %d: bus/pci/devices resolves to %q (%v), want the device", i, listed, err)
		}
		for _, c := range []struct {
			name, content string
			owner         layout.Owner
			mode          fs.FileMode
		}{
			{"name", "i915\n", layout.OwnerRender, 0o444},
			{"power1_max", "150000000\n", layout.OwnerAgent, 0o664},
			{"power1_rated_max", "150000000\n", layout.OwnerRender, 0o444},
			{"power1_max_interval", "1000\n", layout.OwnerRender, 0o664},
			{"energy1_input", "0\n", layout.OwnerEmulator, 0o444},
		} {
			rel := intelHwmon(i, c.name)
			if got := nodeRead(t, tr, rel); got != c.content {
				t.Errorf("%s = %q, want %q", rel, got, c.content)
			}
			if f := owners[rel]; f.Owner != c.owner || f.Mode != c.mode {
				t.Errorf("%s owner %v mode %o, want %v %o", rel, f.Owner, f.Mode, c.owner, c.mode)
			}
		}
		for _, absent := range []string{"power1_input", "power1_average", "power1_cap", "power2_max"} {
			if _, err := os.Lstat(filepath.Join(tr.Root, intelHwmon(i, absent))); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("gpu %d has %s (%v); i915 has none", i, absent, err)
			}
		}
		hw, err := filepath.Glob(filepath.Join(tr.Root, dev, "hwmon", "hwmon*"))
		if err != nil || len(hw) != 1 {
			t.Errorf("gpu %d has hwmon devices %v (%v), want one", i, hw, err)
		}
	}

	node := p.NodeObject("flex-0")
	for k, v := range map[string]string{
		"feature.node.kubernetes.io/pci-0380_8086.present": "true",
		"intel.feature.node.kubernetes.io/gpu":             "true",
		"gpu.intel.com/device-id.0380-56c0.present":        "true",
		"gpu.intel.com/device-id.0380-56c0.count":          "4",
		"gpu.intel.com/family":                             "Flex_Series",
		"gpu.intel.com/product":                            "Flex_170",
		"gpu.intel.com/device.count":                       "4",
	} {
		if got := node.Labels[k]; got != v {
			t.Errorf("label %s = %q, want %q", k, got, v)
		}
	}
	for _, res := range []corev1.ResourceList{node.Status.Capacity, node.Status.Allocatable} {
		if q, ok := res["gpu.intel.com/i915"]; !ok || q.Value() != 4 {
			t.Errorf("gpu.intel.com/i915 = %v (%v), want 4", q.String(), ok)
		}
		for name := range res {
			if strings.HasSuffix(string(name), "/gpu") {
				t.Errorf("the Node advertises %s; the Intel plugin advertises gpu.intel.com/i915", name)
			}
		}
	}
}

// H48: power1_max takes any value, as i915 does. A whole number of watts
// reads back as written; a value between power units is quantized to the
// nearest 1/8 W; a limit below the 40 W floor is accepted and reads back,
// and is enforced, as the floor (clamped); 0 disables the limit, which reads
// 0 and leaves the GPU at PKG_MAX_PWR; a value that is not a number is a
// parse error, EINVAL through CheckWrite, which changes nothing.
func TestH48IntelGPULimitWritesAsTheDriver(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, intelProfile)
	tr := renderTestTree(t, p)
	n := nodeNewNode(t, p, tr)
	f := intelHwmon(1, "power1_max")
	enforced := func() float64 { return n.State().GPUs[1].EnforcedLimitW }
	for _, c := range []struct {
		write, read, kind string
		limitW            float64
	}{
		{"90000000", "90000000", EventAccepted, 90},
		{"120000001", "120000000", EventQuantized, 120},
		{"100062000", "100000000", EventQuantized, 100},
		{"30000000", "40000000", EventClamped, 40},
		{"200000000", "150000000", EventClamped, 150},
		{"0", "0", EventAccepted, 150},
		{"75000000", "75000000", EventAccepted, 75},
	} {
		nodeWrite(t, tr, f, c.write)
		nodeStep(t, n, 2*time.Second)
		if got := nodeRead(t, tr, f); got != c.read+"\n" {
			t.Errorf("a write of %s reads back %q, want %s", c.write, got, c.read)
		}
		if e := nodeOneEvent(t, n, f); e.Kind != c.kind || e.Written != c.write || e.Effective != c.read {
			t.Errorf("a write of %s: event %+v, want %s to %s", c.write, e, c.kind, c.read)
		}
		if got := enforced(); math.Abs(got-c.limitW) > 0.01 {
			t.Errorf("a write of %s: enforced limit %.3f W, want %.0f", c.write, got, c.limitW)
		}
	}
	if got := nodeRead(t, tr, intelHwmon(0, "power1_max")); got != "150000000\n" {
		t.Errorf("gpu 0 power1_max = %q after writes to gpu 1, want it untouched", got)
	}

	if err := n.CheckWrite(f, "lots"); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("CheckWrite(%s, lots) = %v, want EINVAL", f, err)
	}
	// CheckWrite takes a limit below the floor, as the driver does; the
	// caller writes the file, and the next step shows the floor in it.
	if err := n.CheckWrite(f, "30000000"); err != nil {
		t.Errorf("CheckWrite(%s, 30000000) = %v, want it accepted, as the driver accepts any limit", f, err)
	}
	nodeWrite(t, tr, f, "30000000")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, f); got != "40000000\n" {
		t.Errorf("after CheckWrite and a write of 30 W, power1_max reads %q, want the 40 W floor", got)
	}
	nodeStep(t, n, 2*time.Second)
	if got := enforced(); math.Abs(got-40) > 0.01 {
		t.Errorf("after CheckWrite of 30 W the enforced limit is %v W, want the 40 W floor", got)
	}
}

// H49: at full load an uncapped Flex 170 draws well above 90 W; with
// power1_max at 90 W it settles at the limit, and energy1_input counts the energy in uJ
// without ever going back, also when a new Node takes over the tree, which
// keeps the limit written. State reports the GPUs as Intel's, measured.
func TestH49IntelGPUPowerFollowsTheLimit(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, intelProfile)
	tr := renderTestTree(t, p)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	nodeStep(t, n, 5*time.Second)
	if w := n.State().GPUs[0].PowerW; w < 100 || w > 150 {
		t.Fatalf("uncapped gpu 0 at full load draws %.1f W, want 100 to 150 W, or the cap below proves nothing", w)
	}
	e0 := nodeReadInt64(t, tr, intelHwmon(0, "energy1_input"))
	if e0 <= 0 {
		t.Fatalf("energy1_input = %d after 5 s at full load, want it counting", e0)
	}
	for i := 0; i < p.GPUs.Count.V; i++ {
		nodeWrite(t, tr, intelHwmon(i, "power1_max"), "90000000")
	}
	nodeStep(t, n, 10*time.Second)
	st := n.State()
	for _, g := range st.GPUs {
		if g.PowerW > 90*1.01 || math.Abs(g.EnforcedLimitW-90) > 0.01 || g.LimitW != 90 {
			t.Errorf("gpu %d: power %.2f W, enforced %.2f W, limit %.2f W; want at most 90.9 W under a 90 W limit", g.Index, g.PowerW, g.EnforcedLimitW, g.LimitW)
		}
	}
	gpus := 0
	for _, c := range st.Components {
		if c.Type != "gpu" {
			continue
		}
		gpus++
		if c.Vendor != "Intel" || !c.Measured || c.PCIBusID == "" || c.SysfsPath == "" || math.Abs(c.LimitW-90) > 0.01 {
			t.Errorf("component %s: vendor %q measured %v bus %q sysfs %q limit %v; want Intel, measured, with its PCI device and the 90 W limit",
				c.ID, c.Vendor, c.Measured, c.PCIBusID, c.SysfsPath, c.LimitW)
		}
	}
	if gpus != p.GPUs.Count.V {
		t.Errorf("%d GPU components, want %d", gpus, p.GPUs.Count.V)
	}
	e1 := nodeReadInt64(t, tr, intelHwmon(0, "energy1_input"))
	if want := e0 + int64(10*90*1e6); e1 <= e0 || math.Abs(float64(e1-want)) > 0.1*10*90*1e6 {
		t.Errorf("energy1_input went from %d to %d uJ over 10 s near 90 W, want about %d", e0, e1, want)
	}

	m := nodeNewNode(t, p, tr)
	m.SetLoad(Load{GPUUtil: []float64{1}})
	nodeStep(t, m, time.Second)
	if got := nodeRead(t, tr, intelHwmon(0, "power1_max")); got != "90000000\n" {
		t.Errorf("a new Node over the tree reads power1_max %q, want the 90 W written", got)
	}
	if e2 := nodeReadInt64(t, tr, intelHwmon(0, "energy1_input")); e2 <= e1 {
		t.Errorf("energy1_input went from %d to %d under a new Node, want it never to go back", e1, e2)
	}
	if g := m.State().GPUs[0]; math.Abs(g.EnforcedLimitW-90) > 0.01 {
		t.Errorf("a new Node enforces %v W, want the 90 W limit in the tree", g.EnforcedLimitW)
	}
}

// H50: under xe the limit is on the package channel: an hwmon device named
// xe with power2_max, power2_rated_max, power2_max_interval, power2_label
// pkg and energy2_input, and no power1_max; the Node advertises
// gpu.intel.com/xe, and a write of power2_max lowers the enforced limit.
func TestH50IntelGPUUnderXeUsesThePackageChannel(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, intelProfile)
	p.GPUs.Intel.Driver = Fact[string]{V: IntelDriverXe, Src: "assumed:test variant"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	tr := renderTestTree(t, p)
	for name, want := range map[string]string{"name": "xe\n", "power2_max": "150000000\n", "power2_rated_max": "150000000\n", "power2_label": "pkg\n", "energy2_input": "0\n"} {
		if got := nodeRead(t, tr, intelHwmon(0, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(tr.Root, intelHwmon(0, "power1_max"))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("xe renders power1_max (%v); DG2 has no card channel limit", err)
	}
	drv, err := filepath.EvalSymlinks(filepath.Join(tr.Root, layout.SysDir, layout.PCIDevDir(0x10), "driver"))
	if err != nil || filepath.Base(drv) != "xe" {
		t.Errorf("driver resolves to %q (%v), want xe", drv, err)
	}
	if q, ok := p.NodeObject("flex-xe").Status.Capacity["gpu.intel.com/xe"]; !ok || q.Value() != 4 {
		t.Errorf("gpu.intel.com/xe = %v (%v), want 4", q.String(), ok)
	}
	n := nodeNewNode(t, p, tr)
	nodeWrite(t, tr, intelHwmon(0, "power2_max"), "60000000")
	nodeStep(t, n, 2*time.Second)
	if g := n.State().GPUs[0]; math.Abs(g.EnforcedLimitW-60) > 0.01 {
		t.Errorf("enforced limit %v W after power2_max 60 W, want 60", g.EnforcedLimitW)
	}
}

// H51: gpus.intel is required for an Intel GPU, with a driver of i915 or
// xe, a power unit shift of 0 to 15 and a time window above 0, and refused
// for any other vendor; gpu.intel.com/i915 comes from gpus.exposure, never
// from extendedResources.
func TestH51IntelGPUProfileValidation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"no intel block", func(p *Profile) { p.GPUs.Intel = nil }, "gpus.intel is required"},
		{"unknown driver", func(p *Profile) { p.GPUs.Intel.Driver.V = "nouveau" }, `gpus.intel.driver "nouveau"`},
		{"shift out of range", func(p *Profile) { p.GPUs.Intel.PowerUnitShift.V = 16 }, "gpus.intel.powerUnitShift 16"},
		{"no window", func(p *Profile) { p.GPUs.Intel.MaxIntervalMS.V = 0 }, "gpus.intel.maxIntervalMS"},
		{"amdgpu sensor", func(p *Profile) { p.GPUs.PowerSensor = Fact[string]{V: "average", Src: "assumed:test"} }, "apply to amdgpu only"},
		{"resource by hand", func(p *Profile) { p.Node.ExtendedResources = map[string]int64{"gpu.intel.com/i915": 4} }, "GPU resources come from gpus.exposure"},
		{"another vendor", func(p *Profile) { p.GPUs.Vendor = "amd" }, "gpus.intel applies to Intel GPUs only"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := renderBuiltin(t, intelProfile)
			c.mutate(p)
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate() = %v, want an error containing %q", err, c.want)
			}
		})
	}
}
