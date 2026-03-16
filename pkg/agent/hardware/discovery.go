// Package hardware provides hardware discovery utilities for the Joulie agent.
// It discovers CPU, GPU, memory, and network capabilities and maps them to
// the NodeHardware API type.
package hardware

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/hwinv"
)

// DiscoverOptions controls discovery behavior.
type DiscoverOptions struct {
	// CatalogPath optionally overrides the default embedded hardware catalog.
	CatalogPath string
	// SimulateMode skips real hardware access (for testing).
	SimulateMode bool
	// NodeName is the node name to publish as.
	NodeName string
}

// Discover performs hardware discovery and returns a NodeHardware.
func Discover(opts DiscoverOptions) (joulie.NodeHardware, error) {
	info := joulie.NodeHardware{
		NodeName: opts.NodeName,
	}

	// CPU discovery
	cpuInfo, err := discoverCPU(opts)
	if err != nil {
		return info, fmt.Errorf("CPU discovery: %w", err)
	}
	info.CPU = cpuInfo

	// GPU discovery
	gpuInfo, err := discoverGPU(opts)
	if err != nil {
		// GPU errors are non-fatal; mark as not present
		info.GPU = joulie.NodeHardwareGPU{Present: false}
	} else {
		info.GPU = gpuInfo
	}

	// Memory discovery
	info.Memory = discoverMemory(opts)

	// Network discovery
	info.Network = discoverNetwork(opts)

	// Inventory resolution
	info.Inventory = resolveInventory(info, opts)

	return info, nil
}

func discoverCPU(opts DiscoverOptions) (joulie.NodeHardwareCPU, error) {
	cpu := joulie.NodeHardwareCPU{}

	if opts.SimulateMode {
		cpu.Vendor = "simulator"
		cpu.RawModel = "Simulated CPU"
		cpu.Model = "SIMULATED"
		cpu.Sockets = 2
		cpu.CoresPerSocket = 32
		cpu.TotalCores = 64
		cpu.DriverFamily = "simulated"
		cpu.CapRange = joulie.CPUCapRange{Type: "package", MinWattsPerSocket: 30, MaxWattsPerSocket: 250}
		return cpu, nil
	}

	// Read /proc/cpuinfo for model name
	rawModel, sockets, coresPerSocket, totalCores := parseProcCPUInfo()
	cpu.RawModel = rawModel
	cpu.Sockets = sockets
	cpu.CoresPerSocket = coresPerSocket
	cpu.TotalCores = totalCores

	// Determine vendor
	rawLower := strings.ToLower(rawModel)
	if strings.Contains(rawLower, "amd") {
		cpu.Vendor = "amd"
	} else if strings.Contains(rawLower, "intel") {
		cpu.Vendor = "intel"
	} else {
		cpu.Vendor = "unknown"
	}

	// Driver family from cpufreq
	cpu.DriverFamily = detectCPUDriverFamily()

	// Frequency landmarks from cpufreq
	cpu.Landmarks = detectFrequencyLandmarks()

	// RAPL cap range
	cpu.CapRange = detectRAPLCapRange(sockets)

	return cpu, nil
}

func parseProcCPUInfo() (model string, sockets, coresPerSocket, totalCores int) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown", 1, 1, 1
	}

	socketIDs := make(map[string]bool)
	physCoreIDs := make(map[string]map[string]bool)
	var lastModel string

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				lastModel = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "physical id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				sid := strings.TrimSpace(parts[1])
				socketIDs[sid] = true
				if physCoreIDs[sid] == nil {
					physCoreIDs[sid] = make(map[string]bool)
				}
			}
		}
		// core id within a socket
	}

	sockets = len(socketIDs)
	if sockets == 0 {
		sockets = 1
	}

	// Count total logical processors
	logicalCount := strings.Count(string(data), "processor\t:")
	if logicalCount == 0 {
		logicalCount = 1
	}
	totalCores = logicalCount
	coresPerSocket = totalCores / sockets
	if coresPerSocket == 0 {
		coresPerSocket = 1
	}
	return lastModel, sockets, coresPerSocket, totalCores
}

func detectCPUDriverFamily() string {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver")
	if err != nil {
		return "unknown"
	}
	driver := strings.TrimSpace(string(data))
	switch {
	case strings.HasPrefix(driver, "amd-pstate"):
		return "amd-pstate"
	case strings.HasPrefix(driver, "intel_pstate"):
		return "intel-pstate"
	case driver == "acpi-cpufreq":
		return "acpi-cpufreq"
	default:
		return driver
	}
}

func detectFrequencyLandmarks() joulie.CPULandmarks {
	landmarks := joulie.CPULandmarks{}

	// Min freq
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_min_freq"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			landmarks.MinFreqMHz = v / 1000.0 // kHz -> MHz
		}
	}

	// Max freq
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			landmarks.MaxBoostMHz = v / 1000.0
		}
	}

	// Base freq (nominal)
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/base_frequency"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			landmarks.NominalFreqMHz = v / 1000.0
		}
	}

	// Lowest nonlinear: heuristic as 50% of nominal
	if landmarks.NominalFreqMHz > 0 {
		landmarks.LowestNonlinearFreqMHz = landmarks.NominalFreqMHz * 0.5
	}

	return landmarks
}

func detectRAPLCapRange(sockets int) joulie.CPUCapRange {
	cr := joulie.CPUCapRange{Type: "package"}

	// Try to read RAPL constraint_0 max power for socket 0
	for s := 0; s < sockets && s < 2; s++ {
		raplPath := fmt.Sprintf("/sys/class/powercap/intel-rapl/intel-rapl:%d/constraint_0_max_power_uw", s)
		data, err := os.ReadFile(raplPath)
		if err != nil {
			// Try AMD RAPL path (amd_rapl uses the same sysfs layout)
			raplPath = fmt.Sprintf("/sys/class/powercap/amd-rapl/amd-rapl:%d/constraint_0_max_power_uw", s)
			data, err = os.ReadFile(raplPath)
		}
		if err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				maxW := v / 1e6 // uW -> W
				if maxW > cr.MaxWattsPerSocket {
					cr.MaxWattsPerSocket = maxW
				}
				cr.MinWattsPerSocket = maxW * 0.1 // ~10% as practical minimum
			}
		}
	}

	// Fallback: if RAPL not readable, leave zeros (operator will use catalog)
	return cr
}

func discoverGPU(opts DiscoverOptions) (joulie.NodeHardwareGPU, error) {
	if opts.SimulateMode {
		return joulie.NodeHardwareGPU{
			Present:  true,
			Vendor:   "nvidia",
			RawModel: "Simulated NVIDIA GPU",
			Model:    "NVIDIA H100",
			Count:    2,
			CapRange: joulie.GPUCapRange{MinWatts: 100, MaxWatts: 400, DefaultWatts: 350},
			Slicing:  joulie.GPUSlicing{Supported: true, Modes: []string{"dra-timeslice"}},
		}, nil
	}

	// Try nvidia-smi
	gpu, err := discoverNvidiaGPU()
	if err == nil {
		return gpu, nil
	}

	// Try AMD SMI
	gpu, err = discoverAMDGPU()
	if err == nil {
		return gpu, nil
	}

	return joulie.NodeHardwareGPU{Present: false}, fmt.Errorf("no GPU found")
}

func discoverNvidiaGPU() (joulie.NodeHardwareGPU, error) {
	ctx := context.Background()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,power.limit,power.min_limit,power.max_limit",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return joulie.NodeHardwareGPU{}, fmt.Errorf("nvidia-smi not available: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return joulie.NodeHardwareGPU{}, fmt.Errorf("no GPU output")
	}

	gpu := joulie.NodeHardwareGPU{Present: true, Vendor: "nvidia", Count: len(lines)}
	parts := strings.Split(lines[0], ",")
	if len(parts) >= 4 {
		gpu.RawModel = strings.TrimSpace(parts[0])
		gpu.Model = sanitizeGPUModel(gpu.RawModel)
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
			gpu.CapRange.MinWatts = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64); err == nil {
			gpu.CapRange.MaxWatts = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			gpu.CapRange.DefaultWatts = v
		}
	}

	// Check MIG support
	migOut, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=mig.mode.current", "--format=csv,noheader").Output()
	if err == nil && strings.Contains(strings.ToLower(string(migOut)), "enabled") {
		gpu.Slicing = joulie.GPUSlicing{Supported: true, Modes: []string{"mig", "dra-timeslice"}}
	} else {
		gpu.Slicing = joulie.GPUSlicing{Supported: true, Modes: []string{"dra-timeslice"}}
	}

	return gpu, nil
}

func discoverAMDGPU() (joulie.NodeHardwareGPU, error) {
	ctx := context.Background()
	out, err := exec.CommandContext(ctx, "rocm-smi", "--showproductname", "--json").Output()
	if err != nil {
		return joulie.NodeHardwareGPU{}, fmt.Errorf("rocm-smi not available: %w", err)
	}

	gpu := joulie.NodeHardwareGPU{
		Present: true,
		Vendor:  "amd",
		Count:   strings.Count(string(out), "Card series"),
	}
	if gpu.Count == 0 {
		gpu.Count = 1
	}
	gpu.RawModel = "AMD GPU"
	gpu.Slicing = joulie.GPUSlicing{Supported: false, Modes: []string{}}

	return gpu, nil
}

func discoverMemory(opts DiscoverOptions) joulie.NodeHardwareMemory {
	if opts.SimulateMode {
		return joulie.NodeHardwareMemory{TotalBytes: 256 * 1024 * 1024 * 1024} // 256 GiB
	}

	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return joulie.NodeHardwareMemory{}
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					return joulie.NodeHardwareMemory{TotalBytes: v * 1024} // kB -> bytes
				}
			}
		}
	}
	return joulie.NodeHardwareMemory{}
}

func discoverNetwork(opts DiscoverOptions) joulie.NodeHardwareNetwork {
	if opts.SimulateMode {
		return joulie.NodeHardwareNetwork{LinkClass: "simulated"}
	}

	// Check primary network interface speed via ethtool or /sys
	ctx := context.Background()
	out, err := exec.CommandContext(ctx, "ethtool", "eth0").Output()
	if err != nil {
		// Try ip link
		return joulie.NodeHardwareNetwork{LinkClass: "unknown"}
	}

	speed := "unknown"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Speed:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				speed = classifyLinkSpeed(parts[1])
			}
		}
	}
	return joulie.NodeHardwareNetwork{LinkClass: speed}
}

func classifyLinkSpeed(speedStr string) string {
	speedStr = strings.ToUpper(speedStr)
	switch {
	case strings.HasPrefix(speedStr, "400000"):
		return "400G"
	case strings.HasPrefix(speedStr, "200000"):
		return "200G"
	case strings.HasPrefix(speedStr, "100000"):
		return "100G"
	case strings.HasPrefix(speedStr, "25000"):
		return "25G"
	case strings.HasPrefix(speedStr, "10000"):
		return "10G"
	default:
		return "unknown"
	}
}

// resolveInventory uses the hwinv catalog to match CPU and GPU models.
// Since hwinv.MatchResult does not carry an exactness field, we derive
// exactness heuristically: "exact" when the catalog key is found, "generic" otherwise.
func resolveInventory(info joulie.NodeHardware, opts DiscoverOptions) joulie.InventoryResolution {
	res := joulie.InventoryResolution{}

	var cat *hwinv.Catalog
	var err error
	if opts.CatalogPath != "" {
		cat, err = hwinv.LoadCatalog(opts.CatalogPath)
	} else {
		cat, err = hwinv.LoadDefaultCatalog()
	}
	if err != nil {
		res.Exactness = joulie.CatalogExactness{CPU: "generic", GPU: "generic"}
		return res
	}

	// Build NodeDescriptor using the actual hwinv field names.
	nd := hwinv.NodeDescriptor{
		CPUModelRaw: info.CPU.RawModel,
		GPUModelRaw: info.GPU.RawModel,
		CPUSockets:  info.CPU.Sockets,
		CPUCores:    info.CPU.TotalCores,
		GPUCount:    info.GPU.Count,
	}

	match := cat.MatchNode(nd)

	if match.CPUKey != "" {
		res.HardwareCatalogKey.CPU = match.CPUKey
		res.Exactness.CPU = "exact"
	} else {
		res.Exactness.CPU = "generic"
	}

	if match.GPUKey != "" {
		res.HardwareCatalogKey.GPU = match.GPUKey
		res.Exactness.GPU = "exact"
	} else {
		res.Exactness.GPU = "generic"
	}

	return res
}

func sanitizeGPUModel(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}
