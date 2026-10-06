package smi

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// readAttr returns a sysfs or state file without its trailing newline. Sysfs
// values end in a newline; the agent writes without one.
func readAttr(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// readInt reads a decimal attribute. ok is false when the file is absent or
// holds anything but an integer, which a tool reports as a failed query.
func readInt(p string) (int64, bool) {
	s, err := readAttr(p)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v, err == nil
}

// writeAttr writes an agent-owned attribute in place: open without create,
// truncate, write, as a library writing to sysfs does. It never renames, so
// the emulator's readback keeps seeing the same inode.
func writeAttr(p, value string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// errNoNVML is an absent state/nvml: the emulated node has no NVIDIA driver.
var errNoNVML = errors.New("no NVML state")

// nvGPU is one GPU of the NVML state, state/nvml/gpu<Index>.
type nvGPU struct {
	Index int
	Dir   string
}

// nvmlGPUs lists the GPUs of the NVML state by index. Only directories named
// exactly as layout.NVMLGPUDir names them count, so gpu03 or gpu-x is ignored.
func nvmlGPUs(env Env) ([]nvGPU, error) {
	root := filepath.Join(env.StateRoot, layout.NVMLDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoNVML
	}
	if err != nil {
		return nil, err
	}
	var gpus []nvGPU
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "gpu")
		i, err := strconv.Atoi(n)
		if !ok || err != nil || i < 0 || !e.IsDir() || layout.NVMLGPUDir(i) != path.Join(layout.NVMLDir, e.Name()) {
			continue
		}
		gpus = append(gpus, nvGPU{Index: i, Dir: filepath.Join(env.StateRoot, filepath.FromSlash(layout.NVMLGPUDir(i)))})
	}
	sort.Slice(gpus, func(a, b int) bool { return gpus[a].Index < gpus[b].Index })
	return gpus, nil
}

// field returns one NVML value as stored: a number, or the token the device
// reports instead of one, such as [N/A]. A missing file is an error, not a
// token: every field nvFields names has a file on a rendered node, so a
// missing one is state nobody wrote, and printing a token would hide it.
func (g nvGPU) field(file string) (string, error) {
	return readAttr(filepath.Join(g.Dir, file))
}

// number parses an NVML value. ok is false for a token such as [N/A]; err is
// set when the file is missing.
func (g nvGPU) number(file string) (v float64, ok bool, err error) {
	s, err := g.field(file)
	if err != nil {
		return 0, false, err
	}
	v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, perr == nil, nil
}

// amdVendorID is PCI vendor 0x1002. rocm_smi_lib monitors amdgpu devices
// only; listing DRM cards and filtering on the vendor is assumed to give the
// same list.
const amdVendorID = 0x1002

// The per-card values a text tree has no attribute for live in
// state/amdgpu/card<N>/ (package doc), one file each.
const (
	amdgpuStateDir        = "amdgpu"
	amdgpuEnergyCountFile = "energy_count" // the energy accumulator; 0 marks a secondary die
	amdgpuGFXVersionFile  = "gfx_version"  // the KFD target, such as gfx942
)

// amdGPU is one AMD GPU as rocm-smi and amd-smi number it.
type amdGPU struct {
	Index int    // position in the tool's list: -d N, -g N, card<N> in rocm-smi JSON
	Card  int    // DRM card number, sys/class/drm/card<Card>
	Dev   string // the card's PCI device directory, reached through the class link
	Hwmon string // the device's hwmon directory, empty when it has none
	State string // state/amdgpu/card<Card>
}

// amdGPUs lists sys/class/drm/card<N> (not renderD<N>, not connectors such as
// card0-DP-1) in numeric order and keeps the AMD ones, so the tool index is
// the position in that list: on a board whose BMC VGA is card0, -d 0 is
// card1. A node without class/drm has no GPU.
func amdGPUs(env Env) ([]amdGPU, error) {
	dir := filepath.Join(env.SysRoot, "class", "drm")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cards []int
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "card")
		c, err := strconv.Atoi(n)
		if ok && err == nil && c >= 0 && "card"+strconv.Itoa(c) == e.Name() {
			cards = append(cards, c)
		}
	}
	sort.Ints(cards)
	var gpus []amdGPU
	for _, c := range cards {
		name := "card" + strconv.Itoa(c)
		dev := filepath.Join(dir, name, "device")
		v, err := readAttr(filepath.Join(dev, "vendor"))
		if err != nil {
			continue
		}
		if id, err := strconv.ParseUint(strings.TrimSpace(v), 0, 16); err != nil || id != amdVendorID {
			continue
		}
		g := amdGPU{Index: len(gpus), Card: c, Dev: dev, State: filepath.Join(env.StateRoot, amdgpuStateDir, name)}
		if hw, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*")); len(hw) > 0 {
			sort.Strings(hw)
			g.Hwmon = hw[0]
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

// hwmonInt reads one hwmon attribute of the device, such as power1_cap in uW.
func (g amdGPU) hwmonInt(name string) (int64, bool) {
	if g.Hwmon == "" {
		return 0, false
	}
	return readInt(filepath.Join(g.Hwmon, name))
}

// hwmonPath is the path of one hwmon attribute; empty when the device has no
// hwmon, so a write to it fails.
func (g amdGPU) hwmonPath(name string) string {
	if g.Hwmon == "" {
		return ""
	}
	return filepath.Join(g.Hwmon, name)
}

// devAttr reads one attribute of the PCI device, such as vbios_version.
func (g amdGPU) devAttr(name string) (string, bool) {
	s, err := readAttr(filepath.Join(g.Dev, name))
	return s, err == nil
}

// secondary reports whether the device is the secondary die of a multi-die
// package. rocm-smi decides by an energy counter that reads 0
// (rocm_smi.py:1013-1025); a counter that cannot be read means primary. A
// primary whose counter reads 0 is therefore secondary too, as on hardware.
func (g amdGPU) secondary() bool {
	v, ok := readInt(filepath.Join(g.State, amdgpuEnergyCountFile))
	return ok && v == 0
}

// sensorUW is the device's own power reading in uW and whether it is the
// current socket power (power1_input) rather than the average
// (power1_average). A product exposes one or the other (amdgpu_pm.c:3827-3834);
// when both exist, rocm_smi_lib's choice is not read, and power1_input first is
// assumed. The tools never add dies together: rocm-smi prints each device's
// reading and only notes that the primary's covers the package
// (rocm_smi.py:2690-2711), and amd-smi prints the library's reading
// (cmds 7.2:1932-1982). The package power of a multi-die package is therefore
// the emulator's to write on the primary, and a secondary die has no reading.
func (g amdGPU) sensorUW() (uw int64, current, ok bool) {
	if v, ok := g.hwmonInt("power1_input"); ok {
		return v, true, true
	}
	if v, ok := g.hwmonInt("power1_average"); ok {
		return v, false, true
	}
	return 0, false, false
}
