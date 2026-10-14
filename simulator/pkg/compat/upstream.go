package compat

import "github.com/matbun/joulie/pkg/controller/inputs"

// The names, labels and constant labels each exporter reproduces, pinned to
// one upstream release. Every value cites the file and line it was read
// from at that release's tag, so a version bump is a diff of this file.

// Kepler v0.7.12, github.com/sustainable-computing-io/kepler at tag
// v0.7.12. The node energy schema is the same through v0.9.0.
const (
	// pkg/metrics/consts/consts.go:24-25 and
	// pkg/metrics/metricfactory/metric_factory.go:125: every family is
	// kepler_node_<name>_joules_total.
	k07Namespace = "kepler"
	k07Context   = "node"
	k07Suffix    = "_joules_total"

	// pkg/config/types.go:20-26, in the order of EnergyMetricNames
	// (pkg/metrics/consts/consts.go:43-51).
	k07Package  = "package"
	k07Core     = "core"
	k07Uncore   = "uncore"
	k07DRAM     = "dram"
	k07Other    = "other"
	k07GPU      = "gpu"
	k07Platform = "platform"

	// pkg/metrics/consts/consts.go:34, NodeEnergyLabels; the values are
	// the device id, the node name and the mode
	// (pkg/metrics/utils/utils.go:153).
	k07LabelPackage  = "package"
	k07LabelInstance = "instance"
	k07LabelMode     = "mode"

	// pkg/metrics/metricfactory/metric_factory.go:128, the constant label
	// of every family.
	k07LabelSource = "source"

	// pkg/metrics/utils/utils.go:42,45. The idle mode is exposed whenever
	// RAPL is readable (cmd/exporter/exporter.go:128).
	k07ModeDynamic = "dynamic"
	k07ModeIdle    = "idle"

	// The source of each family (metric_factory.go:36-47). GPU families
	// name the accelerator, NVML
	// (pkg/sensors/accelerator/device/device.go:32,
	// pkg/sensors/accelerator/device/sources/nvml.go:75-77). Every other
	// family, platform included when no ACPI or Redfish meter is present,
	// names the RAPL reader
	// (pkg/sensors/components/source/rapl_sysfs.go:123-124).
	k07SourceRAPL = "rapl-sysfs"
	k07SourceNVML = "NVML"

	// The help text is "Aggregated value in <name> value from <source>"
	// (metric_factory.go:126).
	k07HelpFormat = "Aggregated value in %s value from %s"

	// Device ids. Packages, cores, uncores and DRAM are keyed by socket
	// with strconv.Itoa (pkg/collector/energy/node_energy_collector.go:53)
	// and GPUs by index with "%d" (node_energy_collector.go:73). Other is
	// keyed GenericSocketID (pkg/utils/utils.go:68,
	// pkg/collector/stats/node_stats.go:118,130). Without an ACPI meter
	// the platform energy comes from the node platform power model, keyed
	// "estimator" plus the index of its single output
	// (pkg/model/node_platform_energy.go:31,89).
	k07OtherID    = "socket0"
	k07PlatformID = "estimator0"
)

// k07Families is every node energy family, in EnergyMetricNames order.
var k07Families = []string{k07Package, k07Core, k07Uncore, k07DRAM, k07Other, k07GPU, k07Platform}

// Kepler v0.12.0, github.com/sustainable-computing-io/kepler at tag
// v0.12.0. Paths are under internal/ unless they say otherwise. The names
// the kepler preset reads are package inputs' constants, so the reader and
// this writer share one spelling; the comment on each cites where upstream
// spells it.
const (
	// exporter/prometheus/collector/build_info.go:12.
	k012Namespace = "kepler"

	// exporter/prometheus/collector/power_collector.go:17: the constant
	// label of every family.
	k012LabelNodeName = inputs.KeplerNodeLabel

	// power_collector.go:118,129-136: the labels of the node cpu families.
	k012LabelZone = inputs.KeplerZoneLabel
	k012LabelPath = "path"

	// power_collector.go:164-178: the labels of the node gpu families.
	k012LabelGPU     = "gpu"
	k012LabelGPUUUID = "gpu_uuid"
	k012LabelGPUName = "gpu_name"
	k012LabelVendor  = "vendor"

	// device/energy_zone.go:90-93: the path of a zone aggregated over the
	// zones that share its name. A single zone's path is its class/powercap
	// entry under the host sysfs mount (prometheus/procfs v0.19.2
	// sysfs/class_powercap.go, GetRaplZones), which the upstream chart
	// mounts at /host/sys (manifests/helm/kepler/values.yaml:94).
	k012AggregatedPrefix = "aggregated-"
	k012PowercapDir      = "/host/sys/class/powercap/"

	// procfs sysfs/class_powercap.go, GetRaplZones: the directory it lists,
	// under the sysfs root, and the files it reads in each entry.
	k012ClassPowercap = "class/powercap"
	k012NameFile      = "name"
	k012EnergyFile    = "energy_uj"
	k012MaxEnergyFile = "max_energy_range_uj"

	// device/rapl_sysfs_power_meter.go:230-233: a zone whose path holds
	// this is standard; it replaces a zone of the same name and index read
	// through another control type, such as intel-rapl-mmio.
	k012StandardPath = "/intel-rapl:"

	// device/gpu/types.go:12, returned by the NVIDIA collector
	// (device/gpu/nvidia/collector.go:178-179).
	k012VendorNVIDIA = "nvidia"
)

// The Kepler v0.12.0 node families and their help texts
// (power_collector.go:78-104 build the names and texts; 129-136 and
// 164-178 instantiate them). GPU series need the experimental GPU feature,
// which is off by default (config/config.go:505,
// manifests/helm/kepler/values.yaml:149).
const (
	k012CPUJoules       = inputs.KeplerCPUJoules
	k012CPUWatts        = inputs.KeplerCPUWatts
	k012CPUActiveJoules = inputs.KeplerCPUActiveJoules
	k012CPUIdleJoules   = inputs.KeplerCPUIdleJoules
	k012CPUActiveWatts  = "kepler_node_cpu_active_watts"
	k012CPUIdleWatts    = "kepler_node_cpu_idle_watts"
	k012GPUJoules       = inputs.KeplerGPUJoules
	k012GPUActiveJoules = "kepler_node_gpu_active_joules_total"
	k012GPUIdleJoules   = "kepler_node_gpu_idle_joules_total"
	k012GPUWatts        = "kepler_node_gpu_watts"
	k012GPUIdleWatts    = "kepler_node_gpu_idle_watts"
	k012GPUActiveWatts  = "kepler_node_gpu_active_watts"

	k012CPUJoulesHelp       = "Energy consumption of cpu at node level in joules"
	k012CPUWattsHelp        = "Power consumption of cpu at node level in watts"
	k012CPUActiveJoulesHelp = "Energy consumption of cpu in active state at node level in joules"
	k012CPUIdleJoulesHelp   = "Energy consumption of cpu in idle state at node level in joules"
	k012CPUActiveWattsHelp  = "Power consumption of cpu in active state at node level in watts"
	k012CPUIdleWattsHelp    = "Power consumption of cpu in idle state at node level in watts"
	k012GPUJoulesHelp       = "Energy consumption of gpu at node level in joules"
	k012GPUActiveJoulesHelp = "Energy consumption of gpu in active state at node level in joules"
	k012GPUIdleJoulesHelp   = "Energy consumption of gpu in idle state at node level in joules"
	k012GPUWattsHelp        = "Total GPU power consumption in watts"
	k012GPUIdleWattsHelp    = "GPU idle power (auto-detected minimum) in watts"
	k012GPUActiveWattsHelp  = "GPU active power (total - idle) in watts"
)

// dcgm-exporter 4.8.4, github.com/NVIDIA/dcgm-exporter at tag 4.8.4.
const (
	// etc/default-counters.csv:14,17,23: field, type and help text.
	dcgmPowerUsage      = "DCGM_FI_DEV_POWER_USAGE"
	dcgmPowerUsageHelp  = "Power draw (in W)."
	dcgmTotalEnergy     = "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION"
	dcgmTotalEnergyHelp = "Total energy consumption since the driver was last reloaded (in mJ)."
	dcgmGPUUtil         = "DCGM_FI_DEV_GPU_UTIL"
	dcgmGPUUtilHelp     = "GPU utilization (in %)."

	// etc/default-counters.csv:92: the driver version is a label on every
	// GPU series, named after the field.
	dcgmLabelDriverVersion = "DCGM_FI_DRIVER_VERSION"

	// internal/pkg/rendermetrics/render_metrics.go:339-379: the labels of a
	// GPU series. The UUID label is named "UUID"
	// (internal/pkg/collector/gpu_collector.go:879); gpu is the index and
	// device nvidia<index> (gpu_collector.go:886,889).
	dcgmLabelGPU       = "gpu"
	dcgmLabelUUID      = "UUID"
	dcgmLabelPCIBusID  = "pci_bus_id"
	dcgmLabelDevice    = "device"
	dcgmLabelModelName = "modelName"
	dcgmDevicePrefix   = "nvidia"

	// The node name label: hostname from 4.8.3 on (render_metrics.go:376
	// at 4.8.4, 349 at 4.6.0-4.8.3), Hostname up to 4.8.2 (the template at
	// render_metrics.go:47 of tag 4.5.3-4.8.2). Its value is NODE_NAME,
	// which the upstream chart sets to the node
	// (internal/pkg/hostname/hostname.go:98-100).
	dcgmLabelHostname       = "hostname"
	dcgmLabelHostnameLegacy = "Hostname"
)

// ROCm device-metrics-exporter v1.5.2, github.com/ROCm/device-metrics-exporter
// at tag v1.5.2 (commit f41fc780b879). Go and proto paths are under pkg/;
// docs/, internal/ and helm-charts/ are at the repository root. Every family
// is a GaugeVec, the energy counter included
// (amdgpu/gpuagent/gpuagent_gpu_metrics.go:68,74,85,94), so the exposition
// types them all gauge. Names carry no prefix by default: the
// MetricsFieldPrefix of the config is empty unless set
// (exporter/proto/exporterconfig.proto:421-423).
const (
	// amdgpu/gpuagent/gpuagent_gpu_metrics.go:788-792, 918-922, 873-877
	// and 818-822: name and help text. Power is in whole watts and energy
	// in microjoules, as the GPU agent reports them
	// (amdgpu/proto/gpu.proto:616-617, 740-741: uint64 W; 628-629: double
	// uJ); activity is a whole percentage (gpu.proto:473-474, uint32).
	amdPackagePower     = "gpu_package_power"
	amdPackagePowerHelp = "Current socket power in Watts"
	amdPowerUsage       = "gpu_power_usage"
	amdPowerUsageHelp   = "GPU Power usage in Watts"
	amdEnergyConsumed   = "gpu_energy_consumed"
	amdEnergyHelp       = "Accumulated energy consumed by the GPU in uJ"
	amdGFXActivity      = "gpu_gfx_activity"
	amdGFXActivityHelp  = "Graphics engine usage in Percentage (0-100)"

	// amdgpu/gpuagent/gpuagent_gpu_metrics.go:41-56: the mandatory labels,
	// which every GPU series carries and a config cannot remove
	// (docs/configuration/configmap.md:10). They are the enum names
	// lowercased (gpuagent_gpu_metrics.go:301-308).
	amdLabelGPUID                   = "gpu_id"
	amdLabelCardModel               = "card_model"
	amdLabelGPUPartitionID          = "gpu_partition_id"
	amdLabelGPUComputePartitionType = "gpu_compute_partition_type"
	amdLabelGPUMemoryPartitionType  = "gpu_memory_partition_type"
	amdLabelDeploymentMode          = "deployment_mode"
	amdLabelSerialNumber            = "serial_number"
	amdLabelPod                     = "pod"
	amdLabelNamespace               = "namespace"
	amdLabelContainer               = "container"
	amdLabelJobID                   = "job_id"
	amdLabelJobUser                 = "job_user"
	amdLabelJobPartition            = "job_partition"
	amdLabelHostname                = "hostname"

	// The label values of an unpartitioned GPU on bare metal: partition id
	// 0 and compute partition spx, as the upstream example of an
	// unpartitioned GPU shows them (docs/configuration/metricslist.md:401),
	// the enum names with their prefix trimmed and lowercased
	// (gpuagent_gpu_metrics.go:2102-2127; amdgpu/proto/gpu.proto:129,145);
	// and deployment mode baremetal (gpuagent_gpu_metrics.go:2143-2148,
	// exporter/utils/utils.go:143-156). assumed: memory partition nps1, the
	// mode an unpartitioned GPU runs in; no upstream example shows it.
	amdPartitionID         = "0"
	amdComputePartition    = "spx"
	amdMemoryPartition     = "nps1"
	amdDeploymentBaremetal = "baremetal"
)
