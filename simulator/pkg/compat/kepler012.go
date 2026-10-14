package compat

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// kepler012 reproduces the node families of Kepler v0.12.0:
// kepler_node_cpu_{joules_total,watts} and
// kepler_node_cpu_{active,idle}_{joules_total,watts} per RAPL zone, and,
// with the experimental GPU feature, for NVIDIA GPUs
// kepler_node_gpu_{,active_,idle_}{joules_total,watts}, all with the
// constant label node_name. node_name is a variable label here; the
// exposition is the same.
//
// The cpu zones are read from the node's rendered sysfs tree, as Kepler
// reads the host's (k012Discover): every class/powercap entry with a name
// file, named as procfs names it, with zones of one name summed into an
// aggregated zone. joules_total is the zone's energy_uj as the kernel shows
// it, wrapping at max_energy_range_uj, or for an aggregated zone the sum of
// its zones' increments wrapping at the sum of their ranges
// (internal/device/energy_zone.go:97-148; internal/monitor/node.go:53,160).
// Power is the increment over the time between snapshots, across a wrap
// (node.go:99-103,176-187), and the active part is that increment times the
// node CPU usage ratio, here the mean utilisation of the packages, the idle
// part the rest (node.go:131-141). The first snapshot of a node reports
// zero power and puts the whole counter in idle, since Kepler reads its
// zones before it first measures the CPU usage (monitor.go:381-387,
// node.go:189-256).
//
// A GPU reports the device energy counter (total_energy_mj) and its power.
// Its idle power is the lowest power seen while it ran no compute process,
// and 0 until it has been seen so (device/gpu/nvidia/collector.go:234-267);
// assumed: utilisation 0 stands for no compute process. The energy splits
// by active power over total power (internal/monitor/process.go:264-299).
type kepler012 struct {
	compatPublished
	gpu   bool
	descs map[string]*prometheus.Desc
	nodes map[string]*k012Node
}

func newKepler012(gpu bool) *kepler012 {
	k := &kepler012{gpu: gpu, descs: map[string]*prometheus.Desc{}, nodes: map[string]*k012Node{}}
	cpu := []string{k012LabelZone, k012LabelPath, k012LabelNodeName}
	gpuLabels := []string{k012LabelGPU, k012LabelGPUUUID, k012LabelGPUName, k012LabelVendor, k012LabelNodeName}
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
		{k012GPUJoules, k012GPUJoulesHelp, gpuLabels},
		{k012GPUActiveJoules, k012GPUActiveJoulesHelp, gpuLabels},
		{k012GPUIdleJoules, k012GPUIdleJoulesHelp, gpuLabels},
		{k012GPUWatts, k012GPUWattsHelp, gpuLabels},
		{k012GPUIdleWatts, k012GPUIdleWattsHelp, gpuLabels},
		{k012GPUActiveWatts, k012GPUActiveWattsHelp, gpuLabels},
	} {
		k.descs[d.name] = prometheus.NewDesc(d.name, d.help, d.labels, nil)
	}
	return k
}

// observe reads every node's zones. A node whose tree has no RAPL zone gets
// no series, GPUs included: Kepler v0.12.0 finds no CPU power meter and
// does not start.
func (k *kepler012) observe(snap *emulate.Snapshot) {
	out := make([]compatPoint, 0, len(k.points))
	live := make(map[string]bool, len(snap.Nodes))
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		st := k.nodes[n.Name]
		if st == nil || st.root != n.Root {
			st = &k012Node{name: n.Name, root: n.Root, gpuByID: map[string]*k012GPU{}}
			k.nodes[n.Name] = st
		}
		if len(st.zones) == 0 {
			st.zones = k012Discover(filepath.Join(n.Root, layout.SysDir))
			if len(st.zones) == 0 {
				continue
			}
		}
		live[n.Name] = true
		st.update(n, snap.Time, k.gpu)
		out = st.appendPoints(out, k.descs)
	}
	for name := range k.nodes {
		if !live[name] {
			delete(k.nodes, name)
		}
	}
	k.publish(out)
}

// k012Node is one node's state: its tree, its zones as Kepler caches them
// after the first read that finds any, when it was last observed and the
// GPUs it reports, in snapshot order.
type k012Node struct {
	name    string
	root    string
	zones   []*k012Zone
	seen    bool
	at      time.Time
	gpus    []*k012GPU
	gpuByID map[string]*k012GPU
}

// k012Part is one RAPL zone as procfs lists it: its class/powercap entry,
// its name with a trailing index stripped, that index, and its counter range
// in uJ.
type k012Part struct {
	dir   string
	name  string
	index int
	maxUJ uint64
}

// k012Zone is one zone Kepler reports: a RAPL zone, or the zones that share
// a name summed. The counters are in uJ and the powers in uW, as upstream
// keeps them.
type k012Zone struct {
	name, path string
	parts      []k012Part
	maxUJ      uint64

	// The aggregated counter: the last reading of each part and the sum of
	// their increments, wrapped at maxUJ.
	lastUJ    []uint64
	lastSeen  []bool
	currentUJ uint64

	// The node usage of the last snapshot. has is false before the zone's
	// first reading and after a reading that failed.
	has                       bool
	energyUJ                  uint64
	activeUJ, idleUJ          uint64
	powerUW, activeUW, idleUW float64
	labels                    []string
}

// k012Discover lists the RAPL zones under sysRoot as Kepler v0.12.0 does:
// procfs GetRaplZones reads every class/powercap entry with a name file and
// its max_energy_range_uj, and names it with a trailing index stripped, so
// package-0 is zone "package" index 0 while package-0-die-1 keeps its name;
// a zone read through a standard intel-rapl path replaces another of the
// same name and index; zones that share a name become one aggregated zone
// with path aggregated-<name> (rapl_sysfs_power_meter.go:119-182). A name
// file whose range cannot be read fails the whole listing, as in procfs, and
// no zone is returned. The zones are sorted by name.
func k012Discover(sysRoot string) []*k012Zone {
	dir := filepath.Join(sysRoot, filepath.FromSlash(k012ClassPowercap))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type key struct {
		name  string
		index int
	}
	used := map[string]int{}
	std := map[key]k012Part{}
	var order []key
	for _, e := range entries {
		entry := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(filepath.Join(entry, k012NameFile))
		if err != nil {
			continue
		}
		index, name := k012IndexAndName(used, strings.TrimSpace(string(raw)))
		maxUJ, err := k012ReadUint(filepath.Join(entry, k012MaxEnergyFile))
		if err != nil {
			return nil
		}
		used[name] = index + 1
		p := k012Part{dir: entry, name: name, index: index, maxUJ: maxUJ}
		k := key{name, index}
		old, exists := std[k]
		if exists && strings.Contains(k012PowercapDir+filepath.Base(old.dir), k012StandardPath) {
			continue
		}
		if !exists {
			order = append(order, k)
		}
		std[k] = p
	}
	groups := map[string][]k012Part{}
	var names []string
	for _, k := range order {
		if groups[k.name] == nil {
			names = append(names, k.name)
		}
		groups[k.name] = append(groups[k.name], std[k])
	}
	sort.Strings(names)
	zones := make([]*k012Zone, 0, len(names))
	for _, name := range names {
		parts := groups[name]
		z := &k012Zone{name: name, parts: parts}
		if len(parts) == 1 {
			z.path = k012PowercapDir + filepath.Base(parts[0].dir)
			z.maxUJ = parts[0].maxUJ
		} else {
			z.path = k012AggregatedPrefix + name
			for _, p := range parts {
				if z.maxUJ > 0 && p.maxUJ > math.MaxUint64-z.maxUJ {
					z.maxUJ = math.MaxUint64
					break
				}
				z.maxUJ += p.maxUJ
			}
			z.lastUJ = make([]uint64, len(parts))
			z.lastSeen = make([]bool, len(parts))
		}
		zones = append(zones, z)
	}
	return zones
}

// k012IndexAndName is procfs getIndexAndName: a name of exactly two
// dash-separated parts whose second is a number is that name and index;
// any other name keeps its spelling and takes as index the count of its
// earlier uses.
func k012IndexAndName(used map[string]int, name string) (int, string) {
	if s := strings.Split(name, "-"); len(s) == 2 {
		if i, err := strconv.Atoi(s[1]); err == nil {
			return i, s[0]
		}
	}
	return used[name], name
}

// k012ReadUint reads a file holding one unsigned integer.
func k012ReadUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// energy is the zone's counter in uJ: a single zone's energy_uj, or the
// aggregated counter, which adds each part's increment since its last
// reading, across a wrap at the part's range, and on a part's first reading
// the reading itself. A read that fails is returned at once, as upstream
// does, leaving the parts read before it updated.
func (z *k012Zone) energy() (uint64, error) {
	if z.lastUJ == nil {
		return k012ReadUint(filepath.Join(z.parts[0].dir, k012EnergyFile))
	}
	var total uint64
	for i, p := range z.parts {
		v, err := k012ReadUint(filepath.Join(p.dir, k012EnergyFile))
		if err != nil {
			return 0, err
		}
		if z.lastSeen[i] {
			total += k012Delta(v, z.lastUJ[i], p.maxUJ)
		} else {
			total += v
		}
		z.lastUJ[i], z.lastSeen[i] = v, true
	}
	z.currentUJ += total
	if z.maxUJ > 0 {
		z.currentUJ %= z.maxUJ
	}
	return z.currentUJ, nil
}

// k012Delta is calculateEnergyDelta (node.go:176-187): the increment from
// previous to current, across one wrap at maxUJ; a fall without a range is
// no increment.
func k012Delta(current, previous, maxUJ uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	if maxUJ > 0 {
		return (maxUJ - previous) + current
	}
	return 0
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

// update reads the node's zones and, with gpu, its GPUs. A snapshot that is
// not later than the last one changes nothing.
func (st *k012Node) update(n *emulate.NodeSnapshot, now time.Time, gpu bool) {
	first := !st.seen
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

	for _, z := range st.zones {
		z.labels = compatLabelValues(z.labels, z.name, z.path, st.name)
		abs, err := z.energy()
		if err != nil || !(z.maxUJ > 0 || abs > 0) {
			z.has = false
			continue
		}
		switch {
		case first:
			// The CPU usage is not measured yet: all of it is idle.
			z.activeUJ, z.idleUJ = 0, abs
			z.powerUW, z.activeUW, z.idleUW = 0, 0, 0
		case !z.has:
			z.activeUJ, z.idleUJ = 0, 0
			z.powerUW, z.activeUW, z.idleUW = 0, 0, 0
		default:
			delta := k012Delta(abs, z.energyUJ, z.maxUJ)
			active := uint64(float64(delta) * ratio)
			z.activeUJ += active
			z.idleUJ += delta - active
			z.powerUW = float64(delta) / dt
			z.activeUW = float64(z.powerUW * ratio)
			z.idleUW = z.powerUW - z.activeUW
		}
		z.energyUJ, z.has = abs, true
	}

	st.gpus = st.gpus[:0]
	if !gpu {
		return
	}
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

// appendPoints appends the node's series to out: a zone whose last reading
// failed has none, as upstream drops it from that snapshot.
func (st *k012Node) appendPoints(out []compatPoint, descs map[string]*prometheus.Desc) []compatPoint {
	for _, z := range st.zones {
		if !z.has {
			continue
		}
		out = append(out,
			compatPoint{descs[k012CPUJoules], prometheus.CounterValue, k012Joules(z.energyUJ), z.labels},
			compatPoint{descs[k012CPUActiveJoules], prometheus.CounterValue, k012Joules(z.activeUJ), z.labels},
			compatPoint{descs[k012CPUIdleJoules], prometheus.CounterValue, k012Joules(z.idleUJ), z.labels},
			compatPoint{descs[k012CPUWatts], prometheus.GaugeValue, z.powerUW / 1e6, z.labels},
			compatPoint{descs[k012CPUActiveWatts], prometheus.GaugeValue, z.activeUW / 1e6, z.labels},
			compatPoint{descs[k012CPUIdleWatts], prometheus.GaugeValue, z.idleUW / 1e6, z.labels},
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

// k012Joules is Energy.Joules: uJ over 1e6.
func k012Joules(uj uint64) float64 { return float64(uj) / 1e6 }
