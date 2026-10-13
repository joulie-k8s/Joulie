package compat

import (
	"fmt"
	"math"
	"path"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// kepler012 reproduces the node families of Kepler v0.12.0:
// kepler_node_cpu_{joules_total,watts} and
// kepler_node_cpu_{active,idle}_{joules_total,watts} per zone, and for
// NVIDIA GPUs kepler_node_gpu_{,active_,idle_}{joules_total,watts}, all
// with the constant label node_name. node_name is a variable label here;
// the exposition is the same.
//
// Zones are package, and dram when a DRAM zone has an energy counter. A
// zone over several sockets is one series with path aggregated-<zone>
// (internal/device/energy_zone.go:90-93); a single zone has its powercap
// directory as path. joules_total is the zone's energy counter: the sum of
// the component ledgers, which upstream reads from energy_uj. assumed: the
// ledger never wraps where energy_uj does. Power is the energy delta over
// the time between snapshots, and the active part is that delta times the
// node CPU usage ratio, here the mean utilisation of the packages
// (internal/monitor/node.go:131-141). The first snapshot of a node reports
// its counters with zero power.
//
// A GPU reports the device energy counter (total_energy_mj) and its power.
// Its idle power is the lowest power seen while it ran no compute process,
// and 0 until it has been seen so (device/gpu/nvidia/collector.go:234-267);
// assumed: utilisation 0 stands for no compute process. The energy splits
// by active power over total power (internal/monitor/process.go:264-299).
type kepler012 struct {
	compatPublished
	descs map[string]*prometheus.Desc
	nodes map[string]*k012Node
}

func newKepler012() *kepler012 {
	k := &kepler012{descs: map[string]*prometheus.Desc{}, nodes: map[string]*k012Node{}}
	cpu := []string{k012LabelZone, k012LabelPath, k012LabelNodeName}
	gpu := []string{k012LabelGPU, k012LabelGPUUUID, k012LabelGPUName, k012LabelVendor, k012LabelNodeName}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{k012CPUJoules, k012CPUJoulesHelp, cpu},
		{k012CPUWatts, k012CPUWattsHelp, cpu},
		{k012CPUActiveJoules, k012CPUActiveJoulesHelp, cpu},
		{k012CPUIdleJoules, k012CPUIdleJoulesHelp, cpu},
		{k012CPUActiveWatts, k012CPUActiveWattsHelp, cpu},
		{k012CPUIdleWatts, k012CPUIdleWattsHelp, cpu},
		{k012GPUJoules, k012GPUJoulesHelp, gpu},
		{k012GPUActiveJoules, k012GPUActiveJoulesHelp, gpu},
		{k012GPUIdleJoules, k012GPUIdleJoulesHelp, gpu},
		{k012GPUWatts, k012GPUWattsHelp, gpu},
		{k012GPUIdleWatts, k012GPUIdleWattsHelp, gpu},
		{k012GPUActiveWatts, k012GPUActiveWattsHelp, gpu},
	} {
		k.descs[d.name] = prometheus.NewDesc(d.name, d.help, d.labels, nil)
	}
	return k
}

func (k *kepler012) observe(snap *emulate.Snapshot) {
	out := make([]compatPoint, 0, len(k.points))
	live := make(map[string]bool, len(snap.Nodes))
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		if !compatHasPackageEnergy(n) {
			continue
		}
		live[n.Name] = true
		st := k.nodes[n.Name]
		if st == nil {
			st = &k012Node{name: n.Name, zoneByName: map[string]*k012Zone{}, gpuByID: map[string]*k012GPU{}}
			k.nodes[n.Name] = st
		}
		st.update(n, snap.Time)
		out = st.appendPoints(out, k.descs)
	}
	for name := range k.nodes {
		if !live[name] {
			delete(k.nodes, name)
		}
	}
	k.publish(out)
}

// k012Node is one node's state: when it was last observed and the zones
// and GPUs it reports, in snapshot order.
type k012Node struct {
	name       string
	seen       bool
	at         time.Time
	zones      []*k012Zone
	gpus       []*k012GPU
	zoneByName map[string]*k012Zone
	gpuByID    map[string]*k012GPU
}

// k012Zone is one cpu zone.
type k012Zone struct {
	labels  []string // zone, path, node_name
	energy  compatCounter
	split   compatActiveIdle
	totalJ  float64
	powerW  float64
	activeW float64
	idleW   float64
}

// k012GPU is one NVIDIA GPU.
type k012GPU struct {
	labels       []string // gpu, gpu_uuid, gpu_name, vendor, node_name
	energy       compatCounter
	split        compatActiveIdle
	totalJ       float64
	powerW       float64
	activeW      float64
	idleW        float64
	idleObserved bool
	minIdleW     float64
}

// update reads the node's zones and GPUs. A snapshot that is not later
// than the last one changes nothing.
func (st *k012Node) update(n *emulate.NodeSnapshot, now time.Time) {
	dt := 0.0
	if st.seen {
		if !now.After(st.at) {
			return
		}
		dt = now.Sub(st.at).Seconds()
	}
	st.seen, st.at = true, now

	ratio, cpus := 0.0, 0
	for i := range n.Components {
		if c := &n.Components[i]; c.Type == semconv.HwTypeCPU {
			ratio += compatClamp01(c.Utilization)
			cpus++
		}
	}
	if cpus > 0 {
		ratio /= float64(cpus)
	}

	st.zones = st.zones[:0]
	for _, zone := range []struct{ name, hwType string }{
		{k012ZonePackage, semconv.HwTypeCPU},
		{k012ZoneDRAM, semconv.HwTypeMemory},
	} {
		var parts []*emulate.ComponentReading
		totalJ := 0.0
		for i := range n.Components {
			if c := &n.Components[i]; c.Type == zone.hwType && c.Measured {
				parts = append(parts, c)
				totalJ += c.EnergyJ
			}
		}
		if len(parts) == 0 {
			continue
		}
		z := st.zoneByName[zone.name]
		if z == nil {
			z = &k012Zone{}
			st.zoneByName[zone.name] = z
		}
		z.labels = compatLabelValues(z.labels, zone.name, k012ZonePath(zone.name, parts), st.name)
		deltaJ := z.energy.delta(totalJ)
		z.totalJ = totalJ
		z.powerW, z.activeW, z.idleW = 0, 0, 0
		if dt > 0 {
			z.powerW = deltaJ / dt
			z.activeW = float64(z.powerW * ratio)
			z.idleW = z.powerW - z.activeW
		}
		z.split.add(deltaJ, ratio)
		st.zones = append(st.zones, z)
	}

	st.gpus = st.gpus[:0]
	for i := range n.Components {
		c := &n.Components[i]
		if c.Type != semconv.HwTypeGPU || !compatIsNVIDIA(c) {
			continue
		}
		id := c.UUID
		if id == "" {
			id = c.ID
		}
		g := st.gpuByID[id]
		if g == nil {
			g = &k012GPU{}
			st.gpuByID[id] = g
		}
		g.labels = compatLabelValues(g.labels, fmt.Sprintf("%d", c.Index), c.UUID, compatGPUProduct(c), k012VendorNVIDIA, st.name)
		p := c.PowerW
		if !(p > 0) || math.IsInf(p, 0) {
			p = 0
		}
		if !(c.Utilization > 0) && (!g.idleObserved || p < g.minIdleW) {
			g.minIdleW, g.idleObserved = p, true
		}
		g.idleW = 0
		if g.idleObserved {
			g.idleW = g.minIdleW
		}
		g.powerW, g.activeW = p, math.Max(0, p-g.idleW)
		activeRatio := 0.0
		if p > 0 {
			activeRatio = g.activeW / p
		}
		g.split.add(g.energy.delta(c.DeviceEnergyJ), activeRatio)
		g.totalJ = c.DeviceEnergyJ
		st.gpus = append(st.gpus, g)
	}
}

// appendPoints appends the node's series to out.
func (st *k012Node) appendPoints(out []compatPoint, descs map[string]*prometheus.Desc) []compatPoint {
	for _, z := range st.zones {
		out = append(out,
			compatPoint{descs[k012CPUJoules], prometheus.CounterValue, z.totalJ, z.labels},
			compatPoint{descs[k012CPUActiveJoules], prometheus.CounterValue, z.split.activeJ, z.labels},
			compatPoint{descs[k012CPUIdleJoules], prometheus.CounterValue, z.split.idleJ, z.labels},
			compatPoint{descs[k012CPUWatts], prometheus.GaugeValue, z.powerW, z.labels},
			compatPoint{descs[k012CPUActiveWatts], prometheus.GaugeValue, z.activeW, z.labels},
			compatPoint{descs[k012CPUIdleWatts], prometheus.GaugeValue, z.idleW, z.labels},
		)
	}
	for _, g := range st.gpus {
		out = append(out,
			compatPoint{descs[k012GPUWatts], prometheus.GaugeValue, g.powerW, g.labels},
			compatPoint{descs[k012GPUIdleWatts], prometheus.GaugeValue, g.idleW, g.labels},
			compatPoint{descs[k012GPUActiveWatts], prometheus.GaugeValue, g.activeW, g.labels},
			compatPoint{descs[k012GPUJoules], prometheus.CounterValue, g.totalJ, g.labels},
			compatPoint{descs[k012GPUActiveJoules], prometheus.CounterValue, g.split.activeJ, g.labels},
			compatPoint{descs[k012GPUIdleJoules], prometheus.CounterValue, g.split.idleJ, g.labels},
		)
	}
	return out
}

// k012ZonePath is a zone's path label: aggregated-<zone> over several
// sockets, else the zone's class/powercap directory under the host sysfs
// mount. assumed: a component without a sysfs path is the first package,
// or its first subzone for DRAM, of its socket.
func k012ZonePath(zone string, parts []*emulate.ComponentReading) string {
	if len(parts) > 1 {
		return k012AggregatedPrefix + zone
	}
	c := parts[0]
	if c.SysfsPath != "" {
		return k012PowercapDir + path.Base(c.SysfsPath)
	}
	dir := "intel-rapl:" + strconv.Itoa(c.Index)
	if zone == k012ZoneDRAM {
		dir += ":0"
	}
	return k012PowercapDir + dir
}
