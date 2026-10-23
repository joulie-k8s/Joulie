package compat

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// kepler07 reproduces the node energy families of Kepler v0.7.12:
// kepler_node_{package,core,uncore,dram,other,gpu,platform}_joules_total
// with the labels package (the device id), instance (the node) and mode
// (dynamic or idle), and the constant label source.
//
// Each socket has package, core, uncore and dram series. The snapshot has
// no core or uncore zone, and a DRAM zone without an energy counter is
// absent, so those read 0. Package and dram follow the component ledgers,
// as upstream follows energy_uj. GPU series are NVIDIA only, with the
// energy upstream derives from the NVML power reading times the sample
// period (pkg/sensors/accelerator/device/sources/nvml.go:187), here times
// the time since the last snapshot.
//
// Platform is the host energy. assumed: upstream estimates it with its
// node platform power model when no ACPI meter is present, which is meant
// to approximate the host. Other is platform minus package, dram and gpu,
// per mode and interval, and adds nothing when the difference is negative
// (pkg/collector/stats/node_stats.go:108-131).
//
// Idle and dynamic follow k07Split. Every series reports its dynamic
// energy from the first snapshot and its idle energy once it has moved;
// other reports both from the start, as upstream creates both at zero.
type kepler07 struct {
	compatPublished
	descs  map[string]*prometheus.Desc
	excess *prometheus.Desc
	nodes  map[string]*k07Node
}

// Kepler07IdleExcessJoules is the simulator's own counter beside the Kepler
// 0.7 families, never a Kepler series: per node (label instance, as the
// families have it) and family (label Kepler07IdleExcessFamily), the idle
// energy the families reported beyond the energy their series measured,
// which upstream reports while a cap holds a series below the idle power it
// recorded at a lower load (k07Split). A check of the kepler-legacy preset
// adds its rate to the truth, so a reading that Kepler 0.7 itself inflates
// is told apart from one Joulie got wrong.
const (
	Kepler07IdleExcessJoules = "joulie_sim_kepler07_idle_excess_joules_total"
	Kepler07IdleExcessFamily = "family"
)

func newKepler07() *kepler07 {
	k := &kepler07{
		descs: map[string]*prometheus.Desc{},
		excess: prometheus.NewDesc(Kepler07IdleExcessJoules,
			"Idle energy the emulated Kepler 0.7 reported beyond the energy its series measured (simulator only, not a Kepler series).",
			[]string{k07LabelInstance, Kepler07IdleExcessFamily}, nil),
		nodes: map[string]*k07Node{},
	}
	for _, family := range k07Families {
		source := k07SourceRAPL
		if family == k07GPU {
			source = k07SourceNVML
		}
		k.descs[family] = prometheus.NewDesc(
			prometheus.BuildFQName(k07Namespace, k07Context, family+k07Suffix),
			fmt.Sprintf(k07HelpFormat, family, source),
			[]string{k07LabelPackage, k07LabelInstance, k07LabelMode},
			prometheus.Labels{k07LabelSource: source},
		)
	}
	return k
}

func (k *kepler07) observe(snap *emulate.Snapshot) {
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
			st = &k07Node{
				name:      n.Name,
				series:    map[k07Key]*k07Series{},
				otherDyn:  []string{k07OtherID, n.Name, k07ModeDynamic},
				otherIdle: []string{k07OtherID, n.Name, k07ModeIdle},
			}
			k.nodes[n.Name] = st
		}
		st.update(n, snap.Time)
		out = st.appendPoints(out, k.descs, k.excess)
	}
	for name := range k.nodes {
		if !live[name] {
			delete(k.nodes, name)
		}
	}
	k.publish(out)
}

// k07Key names one series of a node: its family and device id.
type k07Key struct{ family, id string }

// k07Series is one series of a node: the counter it follows, its split and
// its label values per mode.
type k07Series struct {
	key        k07Key
	counter    compatCounter
	split      k07Split
	dynLabels  []string
	idleLabels []string
}

// k07Node is one node's state: when it was last observed, the series it
// reports, in order, and the energy of other.
type k07Node struct {
	name       string
	seen       bool
	at         time.Time
	order      []*k07Series
	series     map[k07Key]*k07Series
	otherDynJ  float64
	otherIdleJ float64
	otherDyn   []string
	otherIdle  []string
}

// update adds the energy since the node's last snapshot. A snapshot that is
// not later than the last one changes nothing.
func (st *k07Node) update(n *emulate.NodeSnapshot, now time.Time) {
	dt := 0.0
	if st.seen {
		if !now.After(st.at) {
			return
		}
		dt = now.Sub(st.at).Seconds()
	}
	st.seen, st.at = true, now
	st.order = st.order[:0]
	cpuLoad, gpuLoad := k07Loads(n)

	// The idle and dynamic increments of package, dram and gpu, which
	// other is computed against.
	var compIdle, compDyn float64
	get := func(family, id string) *k07Series {
		key := k07Key{family, id}
		s := st.series[key]
		if s == nil {
			s = &k07Series{
				key:        key,
				dynLabels:  []string{id, st.name, k07ModeDynamic},
				idleLabels: []string{id, st.name, k07ModeIdle},
			}
			st.series[key] = s
		}
		st.order = append(st.order, s)
		return s
	}
	split := func(s *k07Series, deltaJ, load float64) (idle, dyn float64) {
		idle, dyn = s.split.add(deltaJ, dt, load)
		if f := s.key.family; f == k07Package || f == k07DRAM || f == k07GPU {
			compIdle += idle
			compDyn += dyn
		}
		return idle, dyn
	}

	for i := range n.Components {
		c := &n.Components[i]
		if c.Type != semconv.HwTypeCPU {
			continue
		}
		id := strconv.Itoa(c.Index)
		pkgJ, dramJ := 0.0, 0.0
		if c.Measured {
			pkgJ = c.EnergyJ
		}
		if m := k07DRAMOf(n, c); m != nil && m.Measured {
			dramJ = m.EnergyJ
		}
		pkg := get(k07Package, id)
		split(pkg, pkg.counter.delta(pkgJ), cpuLoad)
		split(get(k07Core, id), 0, cpuLoad)
		split(get(k07Uncore, id), 0, cpuLoad)
		dram := get(k07DRAM, id)
		split(dram, dram.counter.delta(dramJ), cpuLoad)
	}
	for i := range n.Components {
		c := &n.Components[i]
		if c.Type != semconv.HwTypeGPU || !compatIsNVIDIA(c) {
			continue
		}
		deltaJ := 0.0
		if c.PowerW > 0 && !math.IsInf(c.PowerW, 0) {
			deltaJ = float64(c.PowerW * dt)
		}
		split(get(k07GPU, fmt.Sprintf("%d", c.Index)), deltaJ, gpuLoad)
	}
	plat := get(k07Platform, k07PlatformID)
	platIdle, platDyn := split(plat, plat.counter.delta(n.Host.EnergyJ), cpuLoad)
	st.otherDynJ += math.Max(0, platDyn-compDyn)
	st.otherIdleJ += math.Max(0, platIdle-compIdle)
}

// appendPoints appends the node's series to out, then its idle excess per
// family, in k07Families order, for each family the node has series of.
func (st *k07Node) appendPoints(out []compatPoint, descs map[string]*prometheus.Desc, excess *prometheus.Desc) []compatPoint {
	excessJ := map[string]float64{}
	for _, s := range st.order {
		desc := descs[s.key.family]
		out = append(out, compatPoint{desc, prometheus.CounterValue, s.split.dynJ, s.dynLabels})
		if s.split.seen {
			out = append(out, compatPoint{desc, prometheus.CounterValue, s.split.idleJ, s.idleLabels})
		}
		excessJ[s.key.family] += s.split.excessJ
	}
	out = append(out,
		compatPoint{descs[k07Other], prometheus.CounterValue, st.otherDynJ, st.otherDyn},
		compatPoint{descs[k07Other], prometheus.CounterValue, st.otherIdleJ, st.otherIdle},
	)
	for _, family := range k07Families {
		if j, ok := excessJ[family]; ok {
			out = append(out, compatPoint{excess, prometheus.CounterValue, j, []string{st.name, family}})
		}
	}
	return out
}

// k07DRAMOf returns the DRAM zone of a CPU package, or nil.
func k07DRAMOf(n *emulate.NodeSnapshot, cpu *emulate.ComponentReading) *emulate.ComponentReading {
	for i := range n.Components {
		if m := &n.Components[i]; m.Type == semconv.HwTypeMemory && (m.Parent == cpu.ID || (m.Parent == "" && m.Index == cpu.Index)) {
			return m
		}
	}
	return nil
}

// k07Loads returns the node's CPU load, which upstream measures as CPU
// time and compares for every CPU family and platform, and its GPU load,
// the compute utilisation it compares for GPUs. Both are the mean
// utilisation of the components, in 0..1.
func k07Loads(n *emulate.NodeSnapshot) (cpu, gpu float64) {
	var cpuSum, gpuSum float64
	var cpus, gpus int
	for i := range n.Components {
		c := &n.Components[i]
		switch {
		case c.Type == semconv.HwTypeCPU:
			cpuSum += compatClamp01(c.Utilization)
			cpus++
		case c.Type == semconv.HwTypeGPU && compatIsNVIDIA(c):
			gpuSum += compatClamp01(c.Utilization)
			gpus++
		}
	}
	if cpus > 0 {
		cpu = cpuSum / float64(cpus)
	}
	if gpus > 0 {
		gpu = gpuSum / float64(gpus)
	}
	return cpu, gpu
}
