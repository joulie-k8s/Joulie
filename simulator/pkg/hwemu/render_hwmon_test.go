package hwemu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The HSMP and amdgpu hwmon devices and the PCI and DRM entries hold the
// values of their profiles: HSMP caps in microwatts per socket; card0 the BMC
// VGA at bus 0x02, then per GPU i bus 0x10+i, card i+1 and hwmon i+2 with the
// amdgpu limits in microwatts.
func TestH02HwmonAndPCIFileContents(t *testing.T) {
	t.Parallel()
	hsmp := renderTestTree(t, renderBuiltin(t, "amd-epyc-2s-hsmp"))
	for s := 0; s < 2; s++ {
		dir := "sys/" + layout.HSMPHwmonDir(s) + "/"
		for file, want := range map[string]string{
			"name":           "amd_hsmp_hwmon\n",
			"power1_cap":     "400000000\n",
			"power1_cap_max": "400000000\n",
			"power1_input":   "45000000\n",
		} {
			if got := renderReadFile(t, hsmp, dir+file); got != want {
				t.Errorf("hsmp socket %d %s = %q, want %q", s, file, got, want)
			}
		}
	}

	mi := renderTestTree(t, renderBuiltin(t, "amd-instinct-mi300x-8gpu"))
	sysReal, err := filepath.EvalSymlinks(mi.SysRoot)
	if err != nil {
		t.Fatal(err)
	}
	card0 := "sys/" + layout.PCIDevDir(0x02) + "/"
	if renderReadFile(t, mi, card0+"vendor") != "0x1a03\n" || renderReadFile(t, mi, card0+"class") != "0x030000\n" {
		t.Errorf("card0 is not the BMC VGA")
	}
	if got, _ := filepath.EvalSymlinks(filepath.Join(mi.SysRoot, "class/drm/card0/device")); got != filepath.Join(sysReal, layout.PCIDevDir(0x02)) {
		t.Errorf("class/drm/card0/device resolves to %s", got)
	}
	for i := 0; i < 8; i++ {
		bus := 0x10 + i
		dev := "sys/" + layout.PCIDevDir(bus) + "/"
		for file, want := range map[string]string{
			"vendor":        "0x1002\n",
			"device":        "0x74a1\n",
			"class":         "0x120000\n",
			"product_name":  "AMD Instinct MI300X\n",
			"vbios_version": "113-X0000000-102\n",
		} {
			if got := renderReadFile(t, mi, dev+file); got != want {
				t.Errorf("gpu %d %s = %q, want %q", i, file, got, want)
			}
		}
		card := "card" + strconv.Itoa(i+1)
		if got, _ := filepath.EvalSymlinks(filepath.Join(mi.SysRoot, "class/drm", card, "device")); got != filepath.Join(sysReal, layout.PCIDevDir(bus)) {
			t.Errorf("class/drm/%s/device resolves to %s", card, got)
		}
		link, _ := layout.HwmonClassLink(i+2, layout.PCIDevDir(bus))
		hw := filepath.Join(mi.SysRoot, link)
		for file, want := range map[string]string{
			"name":               "amdgpu\n",
			"power1_cap":         "750000000\n",
			"power1_cap_min":     "0\n",
			"power1_cap_max":     "750000000\n",
			"power1_cap_default": "750000000\n",
			"power1_input":       "120000000\n",
		} {
			b, err := os.ReadFile(filepath.Join(hw, file))
			if err != nil || string(b) != want {
				t.Errorf("gpu %d hwmon %s = %q (%v), want %q", i, file, b, err, want)
			}
		}
		for _, absent := range []string{"power1_average", "power2_cap"} {
			if _, err := os.Stat(filepath.Join(hw, absent)); err == nil {
				t.Errorf("gpu %d has %s; the profile has powerSensor input and no ppt1", i, absent)
			}
		}
	}
}

// H29: what the AMD tools read from KFD or gpu_metrics is rendered from the
// profile, not left to a test helper: every MI300X card has
// state/amdgpu/card<N>/gfx_version holding the profile's gfxVersion, and a
// device listed in secondaryDies, and only such a device, has energy_count 0,
// the zero accumulator by which rocm-smi and amd-smi tell a secondary die.
// Both are render-owned.
func TestH29AMDGPUStateCarriesGFXVersionAndSecondaryDies(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "amd-instinct-mi300x-8gpu")
	p.GPUs.SecondaryDies = []int{1, 3}
	tree := renderTestTree(t, p)
	owners := map[string]layout.Owner{}
	for _, f := range tree.Files {
		owners[f.Rel] = f.Owner
	}
	for i := 0; i < p.GPUs.Count.V; i++ {
		// card0 is the BMC VGA, so GPU i is card i+1.
		dir := "state/amdgpu/card" + strconv.Itoa(i+1) + "/"
		if got := renderReadFile(t, tree, dir+"gfx_version"); got != p.GPUs.GFXVersion.V+"\n" {
			t.Errorf("gpu %d gfx_version = %q, want %q", i, got, p.GPUs.GFXVersion.V+"\n")
		}
		if owner, ok := owners[dir+"gfx_version"]; !ok || owner != layout.OwnerRender {
			t.Errorf("gpu %d gfx_version is not a render-owned entry of Tree.Files", i)
		}
		energy := filepath.Join(tree.Root, filepath.FromSlash(dir+"energy_count"))
		_, err := os.Stat(energy)
		switch secondary := i == 1 || i == 3; {
		case secondary && err != nil:
			t.Errorf("secondary die %d has no energy_count: %v", i, err)
		case secondary:
			if got := renderReadFile(t, tree, dir+"energy_count"); got != "0\n" {
				t.Errorf("secondary die %d energy_count = %q, want \"0\\n\"", i, got)
			}
			if owners[dir+"energy_count"] != layout.OwnerRender {
				t.Errorf("secondary die %d energy_count is not render-owned", i)
			}
		case err == nil:
			t.Errorf("primary %d has an energy_count; the tools would read its value, not tell a secondary die", i)
		}
	}
}
