package compat

import (
	"math"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// dcgm reproduces the three dcgm-exporter 4.8.4 default counters Joulie can
// use, per NVIDIA GPU: DCGM_FI_DEV_POWER_USAGE in W,
// DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION in mJ, the device counter the tree
// keeps in total_energy_mj, and DCGM_FI_DEV_GPU_UTIL in percent. Each series
// carries gpu, UUID, pci_bus_id, device, modelName, the node name label and
// DCGM_FI_DRIVER_VERSION. Upstream writes the labels in that order; the
// exposition here sorts them, which Prometheus does not distinguish.
//
// Values are rounded as upstream reads them from DCGM: power to six
// decimals (internal/pkg/collector/gpu_collector.go:1039), energy and
// utilisation to whole numbers (int64 fields). A value that is not finite
// is left out, as upstream refuses it (render_metrics.go, parseMetricValue).
//
// A node whose snapshot is not later than its last one keeps the series it
// has, so the energy counter never goes back, which Prometheus would read
// as a reset. A node that leaves the snapshot loses its series.
type dcgm struct {
	compatPublished
	power, energy, util *prometheus.Desc
	nodes               map[string]*dcgmNode
}

// dcgmNode is one node's state: when it was last observed, the series it
// reports and their label values by GPU id.
type dcgmNode struct {
	at     time.Time
	points []compatPoint
	labels map[string][]string
}

func newDCGM(hostnameLabel string) *dcgm {
	labels := []string{dcgmLabelGPU, dcgmLabelUUID, dcgmLabelPCIBusID, dcgmLabelDevice, dcgmLabelModelName, hostnameLabel, dcgmLabelDriverVersion}
	return &dcgm{
		power:  prometheus.NewDesc(dcgmPowerUsage, dcgmPowerUsageHelp, labels, nil),
		energy: prometheus.NewDesc(dcgmTotalEnergy, dcgmTotalEnergyHelp, labels, nil),
		util:   prometheus.NewDesc(dcgmGPUUtil, dcgmGPUUtilHelp, labels, nil),
		nodes:  map[string]*dcgmNode{},
	}
}

func (d *dcgm) observe(snap *emulate.Snapshot) {
	out := make([]compatPoint, 0, len(d.points))
	live := make(map[string]bool, len(snap.Nodes))
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		live[n.Name] = true
		st := d.nodes[n.Name]
		if st == nil {
			st = &dcgmNode{}
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

// appendNode appends the series of n's NVIDIA GPUs to out.
func (d *dcgm) appendNode(out []compatPoint, st *dcgmNode, n *emulate.NodeSnapshot) []compatPoint {
	labelsByID := make(map[string][]string, len(st.labels))
	for j := range n.Components {
		c := &n.Components[j]
		if c.Type != semconv.HwTypeGPU || !compatIsNVIDIA(c) {
			continue
		}
		index := strconv.Itoa(c.Index)
		labels := compatLabelValues(st.labels[c.ID], index, c.UUID, c.PCIBusID, dcgmDevicePrefix+index, compatGPUProduct(c), n.Name, n.DriverVersion)
		labelsByID[c.ID] = labels
		for _, v := range []struct {
			desc  *prometheus.Desc
			kind  prometheus.ValueType
			value float64
		}{
			{d.power, prometheus.GaugeValue, math.Round(c.PowerW*1e6) / 1e6},
			// DeviceEnergyJ is total_energy_mj / 1000, a whole number
			// of mJ, which rounding recovers exactly.
			{d.energy, prometheus.CounterValue, math.Round(c.DeviceEnergyJ * 1000)},
			{d.util, prometheus.GaugeValue, math.Round(compatClamp01(c.Utilization) * 100)},
		} {
			if math.IsNaN(v.value) || math.IsInf(v.value, 0) {
				continue
			}
			out = append(out, compatPoint{v.desc, v.kind, v.value, labels})
		}
	}
	st.labels = labelsByID
	return out
}
