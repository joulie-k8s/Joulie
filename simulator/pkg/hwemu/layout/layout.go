// Package layout names the entries of an emulated node's tree: where sysfs
// puts powercap zones, cpufreq policies, hwmon and PCI devices, and where the
// emulator keeps the state the fake GPU tools read. Paths come from the
// kernel's documentation and source, never from the agent's constants, so a
// tree the agent reads correctly is one a real kernel could have produced.
//
// Every helper returns a slash-separated path relative to the sys/ directory
// of the node root, except NVMLGPUDir, which is relative to StateDir. Link
// targets are relative to the link's own directory and never leave sys/, so
// they resolve the same way at /host-sys inside a pod. Kernel citations
// (file.c:N, file.rst:N) are against Linux v7.3-rc5.
package layout

import (
	"fmt"
	"io/fs"
	"path"
	"strconv"
)

// Owner says who writes a file after Render. doc.go of package hwemu holds the
// full legend.
type Owner int

const (
	// OwnerRender files are written once by Render; a later write is reverted.
	OwnerRender Owner = iota
	// OwnerEmulator files are rewritten by temp file and rename.
	OwnerEmulator
	// OwnerAgent files are written in place by the agent or a tool and read
	// back on every step.
	OwnerAgent
)

// Kind is what an entry is on disk.
type Kind int

const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	// KindRejectingDir is a directory in place of an attribute that rejects
	// writes, so os.WriteFile on it fails with EISDIR.
	KindRejectingDir
)

// FileSpec is one entry of a rendered tree. Rel is relative to the node root;
// Target is set only for KindSymlink.
type FileSpec struct {
	Rel    string
	Kind   Kind
	Owner  Owner
	Mode   fs.FileMode
	Target string
}

// Entries of a node root, and their mount points in an emulated agent pod:
// sys at /host-sys, cpuinfo over /proc/cpuinfo, state at /emu/state. ToolsFile,
// NVMLDir and TmpDir live under StateDir.
const (
	SysDir, CPUInfoFile, StateDir, ToolsFile, NVMLDir, TmpDir = "sys", "cpuinfo", "state", "tools.json", "nvml", ".tmp"
)

const (
	DevNvidiactl       = "dev/nvidiactl"    // empty file the agent stats to detect NVIDIA (detectGPUVendor in cmd/agent)
	BinDir             = "bin"              // per-node tool links, mounted at /emu/bin
	FakeSMIInContainer = "/emu/lib/fakesmi" // absolute target of bin/<tool>
)

const (
	powercapDevicesDir = "devices/virtual/powercap"
	powercapClassDir   = "class/powercap" // class name "powercap", powercap_sys.c:476
	hwmonClassDir      = "class/hwmon"
	cpuDir             = "devices/system/cpu"
	hsmpPlatformDir    = "devices/platform/amd_hsmp" // DRIVER_NAME, hsmp/plat.c:30
)

// PowercapControlDir is the directory of control type ct, such as
// "devices/virtual/powercap/intel-rapl" (powercap.rst:34-35).
func PowercapControlDir(ct string) string {
	return path.Join(powercapDevicesDir, ct)
}

// PowercapZoneDir is the directory of the zone at idx under control type ct.
// A zone nests in its parent and is named after it plus ":" and its index in
// hex, so ("intel-rapl", 0, 1) is ".../intel-rapl/intel-rapl:0/intel-rapl:0:1"
// (powercap.rst:34-100; powercap_sys.c:553). No idx gives PowercapControlDir.
func PowercapZoneDir(ct string, idx ...int) string {
	dir := PowercapControlDir(ct)
	name := ct
	for _, i := range idx {
		name += ":" + strconv.FormatInt(int64(i), 16)
		dir = path.Join(dir, name)
	}
	return dir
}

// PowercapClassLink is the flat class entry of the same zone and its target:
// ("intel-rapl", 0) is "class/powercap/intel-rapl:0" pointing at
// "../../devices/virtual/powercap/intel-rapl/intel-rapl:0". Class directories
// hold only links into devices (sysfs-rules.rst:108-112).
func PowercapClassLink(ct string, idx ...int) (link, target string) {
	dir := PowercapZoneDir(ct, idx...)
	return path.Join(powercapClassDir, path.Base(dir)), path.Join("..", "..", dir)
}

// PolicyDir is the directory of cpufreq policy p (cpufreq.rst:202-210).
func PolicyDir(policy int) string {
	return path.Join(cpuDir, "cpufreq", "policy"+strconv.Itoa(policy))
}

// CPUFreqLink is the cpufreq link of a CPU and its target: (5, 4) is
// "devices/system/cpu/cpu5/cpufreq" pointing at "../cpufreq/policy4". Every
// CPU of a policy has one (cpufreq.rst:202-210).
func CPUFreqLink(cpu, policy int) (link, target string) {
	link = path.Join(cpuDir, "cpu"+strconv.Itoa(cpu), "cpufreq")
	return link, path.Join("..", "cpufreq", "policy"+strconv.Itoa(policy))
}

// HSMPHwmonDir is hwmon device h of the HSMP platform layout. The amd_hsmp
// platform device registers one hwmon device per socket (hsmp/plat.c:198), so
// its class link is HwmonClassLink(h, "devices/platform/amd_hsmp").
func HSMPHwmonDir(h int) string {
	return hwmonDir(hsmpPlatformDir, h)
}

// HwmonClassLink is the class entry of hwmon device h registered by the device
// at devDir, such as PCIDevDir(bus): "class/hwmon/hwmon<h>" pointing at
// "../../<devDir>/hwmon/hwmon<h>".
func HwmonClassLink(h int, devDir string) (link, target string) {
	return path.Join(hwmonClassDir, "hwmon"+strconv.Itoa(h)), path.Join("..", "..", hwmonDir(devDir, h))
}

// PCIDevDir is function 0 of device 0 on root bus bus, domain 0, with no
// bridge in between: "devices/pci0000:<bus>/0000:<bus>:00.0" with the bus in
// two hex digits. Real servers put bridges in the path; the flat form is
// assumed.
func PCIDevDir(bus int) string {
	return fmt.Sprintf("devices/pci0000:%02x/0000:%02x:00.0", bus, bus)
}

// DRMCardDir is DRM card card of the PCI device on bus bus.
func DRMCardDir(bus, card int) string {
	return path.Join(PCIDevDir(bus), "drm", "card"+strconv.Itoa(card))
}

// NVMLGPUDir is the NVML state directory of GPU i, relative to StateDir.
func NVMLGPUDir(i int) string {
	return path.Join(NVMLDir, "gpu"+strconv.Itoa(i))
}

// NVMLPersistenceModeFile is the persistence mode of a GPU, in its
// NVMLGPUDir: Enabled or Disabled, as nvidia-smi prints it, set by the fake
// nvidia-smi -pm.
const NVMLPersistenceModeFile = "persistence_mode"

// ToolsManifest is state/tools.json: the GPU tools on the node's PATH and the
// variant each one emulates. Every family writes the same envelope, with an
// empty Tools map when it has no tool.
type ToolsManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Tools         map[string]ToolEntry `json:"tools"`
}

// ToolEntry is one tool of a ToolsManifest.
type ToolEntry struct {
	Variant string `json:"variant"`
}

func hwmonDir(devDir string, h int) string {
	return path.Join(devDir, "hwmon", "hwmon"+strconv.Itoa(h))
}
