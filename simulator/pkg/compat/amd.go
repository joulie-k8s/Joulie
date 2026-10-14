package compat

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// amdDME reproduces the ROCm device-metrics-exporter v1.5.2 families Joulie
// can use, per AMD GPU: gpu_package_power and gpu_power_usage in W,
// gpu_energy_consumed in uJ and gpu_gfx_activity in percent, all gauges as
// upstream types them. Each series carries the fourteen mandatory labels of
// upstream.go. Upstream has no power cap family at v1.5.2: its GPUMetricField
// enum (exporter/proto/exporterconfig.proto:63-292) has none, and the GPU
// power cap is only GPU agent configuration, the GPUPowerCap of GPUSpec
// (amdgpu/proto/gpu.proto:168-184, 195-196). Paths are as in upstream.go.
//
// Label values:
//   - gpu_id is the GPU index (amdgpu/gpuagent/gpuagent_gpu_metrics.go:1834-1839,
//     2014-2017).
//   - hostname is the node name: upstream reads DS_NODE_NAME or NODE_NAME
//     (exporter/utils/utils.go:42-50, 95-107), which its chart sets from
//     spec.nodeName (helm-charts/templates/daemonsets.yaml:113-116).
//   - pod, namespace, container, job_id, job_user and job_partition are
//     empty, as upstream writes them for a GPU no workload holds
//     (gpuagent_gpu_metrics.go:2060-2087). assumed: the snapshot does not
//     say which pod holds a GPU, so they stay empty under load too.
//   - card_model is the product name. assumed: upstream reports the card
//     model amd-smi reads (gpuagent_gpu_metrics.go:2026-2042), a board part
//     number on MI3xx boards, which the profiles do not carry.
//   - serial_number is a synthetic serial derived from the node and GPU
//     index (amdSerial). assumed: the profiles carry no serial.
//
// Values:
//   - Both power families report the GPU's power in whole watts. On MI3xx
//     both read the current socket power (internal/metricsmap.md:43,84);
//     the MI2xx family gpu_average_package_power is not reproduced.
//     assumed: rounded to the nearest watt.
//   - gpu_energy_consumed is the GPU's energy ledger in whole uJ, which
//     continues across a restart as the hardware accumulator does. assumed:
//     upstream's value is the accumulator times its resolution.
//   - gpu_gfx_activity is the utilisation in whole percent.
//
// A power or energy reading of 0, or one of a GPU no collector could read,
// is left out: upstream marks such a field unsupported for the GPU and
// exports nothing for it (gpuagent_gpu_metrics.go:2206-2210, 2392-2396,
// 2427-2430; gpuagent_utils.go:71-100). assumed: upstream decides once, on
// its first poll after it starts (docs/releasenotes.md:145-151), where the
// exporter decides per snapshot. A value that is not finite is left out.
//
// A node whose snapshot is not later than its last one keeps the series it
// has, so the energy never goes back, which a rate() over the gauge would
// read as a drop. A node that leaves the snapshot loses its series.
type amdDME struct {
	compatPublished
	packagePower, powerUsage, energy, activity *prometheus.Desc
	nodes                                      map[string]*amdNode
}

// amdNode is one node's state: when it was last observed, the series it
// reports and their label values by GPU id.
type amdNode struct {
	at     time.Time
	points []compatPoint
	labels map[string][]string
}

// amdLabels is the label order of every series; the exposition sorts them.
var amdLabels = []string{
	amdLabelGPUID, amdLabelCardModel, amdLabelGPUPartitionID, amdLabelGPUComputePartitionType,
	amdLabelGPUMemoryPartitionType, amdLabelDeploymentMode, amdLabelSerialNumber, amdLabelPod,
	amdLabelNamespace, amdLabelContainer, amdLabelJobID, amdLabelJobUser, amdLabelJobPartition,
	amdLabelHostname,
}

func newAMDDME() *amdDME {
	return &amdDME{
		packagePower: prometheus.NewDesc(amdPackagePower, amdPackagePowerHelp, amdLabels, nil),
		powerUsage:   prometheus.NewDesc(amdPowerUsage, amdPowerUsageHelp, amdLabels, nil),
		energy:       prometheus.NewDesc(amdEnergyConsumed, amdEnergyHelp, amdLabels, nil),
		activity:     prometheus.NewDesc(amdGFXActivity, amdGFXActivityHelp, amdLabels, nil),
		nodes:        map[string]*amdNode{},
	}
}

func (d *amdDME) observe(snap *emulate.Snapshot) {
	out := make([]compatPoint, 0, len(d.points))
	live := make(map[string]bool, len(snap.Nodes))
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		live[n.Name] = true
		st := d.nodes[n.Name]
		if st == nil {
			st = &amdNode{}
			d.nodes[n.Name] = st
		} else if !snap.Time.After(st.at) {
			out = append(out, st.points...)
			continue
		}
		st.at = snap.Time
		start := len(out)
		out = d.appendNode(out, st, n)
		// A published view is never modified, so the node can keep its
		// part of this one.
		st.points = out[start:len(out):len(out)]
	}
	for name := range d.nodes {
		if !live[name] {
			delete(d.nodes, name)
		}
	}
	d.publish(out)
}

// appendNode appends the series of n's AMD GPUs to out.
func (d *amdDME) appendNode(out []compatPoint, st *amdNode, n *emulate.NodeSnapshot) []compatPoint {
	labelsByID := make(map[string][]string, len(st.labels))
	for j := range n.Components {
		c := &n.Components[j]
		if c.Type != semconv.HwTypeGPU || !compatIsAMD(c) {
			continue
		}
		index := strconv.Itoa(c.Index)
		labels := compatLabelValues(st.labels[c.ID], index, compatGPUProduct(c), amdPartitionID, amdComputePartition,
			amdMemoryPartition, amdDeploymentBaremetal, amdSerial(n.Name, c.Index), "", "", "", "", "", "", n.Name)
		labelsByID[c.ID] = labels
		powerW := math.Round(c.PowerW)
		energyUJ := math.Round(c.EnergyJ * 1e6)
		for _, v := range []struct {
			desc     *prometheus.Desc
			value    float64
			measured bool // left out at 0 or when no collector reads it
		}{
			{d.packagePower, powerW, true},
			{d.powerUsage, powerW, true},
			{d.energy, energyUJ, true},
			{d.activity, math.Round(compatClamp01(c.Utilization) * 100), false},
		} {
			if math.IsNaN(v.value) || math.IsInf(v.value, 0) || (v.measured && (!c.Measured || v.value == 0)) {
				continue
			}
			out = append(out, compatPoint{v.desc, prometheus.GaugeValue, v.value, labels})
		}
	}
	st.labels = labelsByID
	return out
}

// compatIsAMD reports whether a GPU component is an AMD device, by its
// vendor name or PCI vendor id.
func compatIsAMD(c *emulate.ComponentReading) bool {
	v := strings.ToLower(c.Vendor)
	return strings.Contains(v, "amd") || v == "0x1002"
}

// amdSerial is a GPU's synthetic serial number: the first 16 hex digits of
// sha256 over the node name and GPU index, upper case, so it is stable
// across restarts and unique across nodes.
func amdSerial(node string, index int) string {
	sum := sha256.Sum256([]byte(node + "/gpu" + strconv.Itoa(index)))
	return strings.ToUpper(hex.EncodeToString(sum[:8]))
}
