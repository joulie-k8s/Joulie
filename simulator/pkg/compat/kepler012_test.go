package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// CP04: kepler-0.12 serves the v0.12.0 node families with the labels zone
// and path (cpu) or gpu, gpu_uuid, gpu_name and vendor (gpu), plus
// node_name; a zone over two sockets has path aggregated-<zone> and a single
// zone its powercap directory; there are no AMD GPU series; and the
// exposition for an Intel and an NVL host matches the golden.
func TestCP04Kepler012FamiliesLabelsPathsAndGolden(t *testing.T) {
	set, _ := compatRun(t, []string{Kepler012}, Options{}, compatIntel(), compatNVL(), compatMI300X(), compatSingle())
	samples := compatGather(t, set, Kepler012)

	cpu := map[string]bool{k012CPUJoules: true, k012CPUWatts: true, k012CPUActiveJoules: true, k012CPUIdleJoules: true, k012CPUActiveWatts: true, k012CPUIdleWatts: true}
	gpu := map[string]bool{k012GPUJoules: true, k012GPUActiveJoules: true, k012GPUIdleJoules: true, k012GPUWatts: true, k012GPUIdleWatts: true, k012GPUActiveWatts: true}
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.family] = true
		labels := strings.Join(compatLabelNames(s), ",")
		switch {
		case cpu[s.family]:
			if labels != "node_name,path,zone" {
				t.Errorf("%s labels %s, want node_name,path,zone", s.family, labels)
			}
		case gpu[s.family]:
			if labels != "gpu,gpu_name,gpu_uuid,node_name,vendor" {
				t.Errorf("%s labels %s, want gpu,gpu_name,gpu_uuid,node_name,vendor", s.family, labels)
			}
			if s.labels[k012LabelVendor] != k012VendorNVIDIA || s.labels[k012LabelNodeName] != "nvl-0" {
				t.Errorf("%s%v: GPU series of an NVIDIA GPU only", s.family, s.labels)
			}
		default:
			t.Errorf("unexpected family %s", s.family)
		}
	}
	for f := range cpu {
		if !seen[f] {
			t.Errorf("family %s missing", f)
		}
	}
	for f := range gpu {
		if !seen[f] {
			t.Errorf("family %s missing", f)
		}
	}

	for _, c := range []struct{ node, zone, path string }{
		{"intel-0", k012ZonePackage, "aggregated-package"},
		{"intel-0", k012ZoneDRAM, "aggregated-dram"},
		{"mi300x-0", k012ZonePackage, "aggregated-package"},
		{"single-0", k012ZonePackage, "/host/sys/class/powercap/intel-rapl:0"},
		{"single-0", k012ZoneDRAM, "/host/sys/class/powercap/intel-rapl:0:0"},
	} {
		compatOne(t, samples, k012CPUJoules, map[string]string{k012LabelNodeName: c.node, k012LabelZone: c.zone, k012LabelPath: c.path})
	}
	if got := compatFind(samples, k012GPUWatts, map[string]string{k012LabelNodeName: "nvl-0"}); len(got) != 8 {
		t.Errorf("nvl-0 has %d GPU power series, want 8", len(got))
	}
	if got := compatFind(samples, k012GPUWatts, map[string]string{k012LabelNodeName: "mi300x-0"}); len(got) != 0 {
		t.Errorf("mi300x-0 has AMD GPU series %v", got)
	}

	golden, _ := compatRun(t, []string{Kepler012}, Options{}, compatIntel(), compatNVL())
	code, body := compatScrape(t, golden, Path(Kepler012))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path(Kepler012), code)
	}
	compatGolden(t, "kepler-0.12.prom", body)
}

// CP04: a zone's joules_total is the sum of its ledgers; its power is the
// energy over the interval; the active part is that energy times the node
// CPU usage ratio and the idle part the rest. A GPU reports its device
// counter and power; its idle power is the lowest power seen at zero
// utilisation, 0 before that, and its energy splits by active over total
// power.
func TestCP04Kepler012ActiveSplit(t *testing.T) {
	set, snaps := compatRun(t, []string{Kepler012}, Options{}, compatNVL())
	samples := compatGather(t, set, Kepler012)
	last, prev := snaps[len(snaps)-1], snaps[len(snaps)-2]
	n, p := &last.Nodes[0], &prev.Nodes[0]
	dt := last.Time.Sub(prev.Time).Seconds()
	ratio := compatScript[len(compatScript)-1].cpu

	for _, zone := range []struct{ name, hwType string }{{k012ZonePackage, semconv.HwTypeCPU}, {k012ZoneDRAM, semconv.HwTypeMemory}} {
		var nowJ, wasJ float64
		for i := range n.Components {
			if n.Components[i].Type == zone.hwType {
				nowJ += n.Components[i].EnergyJ
				wasJ += p.Components[i].EnergyJ
			}
		}
		match := map[string]string{k012LabelNodeName: "nvl-0", k012LabelZone: zone.name}
		compatNear(t, zone.name+" joules", compatOne(t, samples, k012CPUJoules, match), nowJ)
		watts := (nowJ - wasJ) / dt
		compatNear(t, zone.name+" watts", compatOne(t, samples, k012CPUWatts, match), watts)
		compatNear(t, zone.name+" active watts", compatOne(t, samples, k012CPUActiveWatts, match), watts*ratio)
		compatNear(t, zone.name+" idle watts", compatOne(t, samples, k012CPUIdleWatts, match), watts*(1-ratio))

		// Active and idle totals over the script: the first snapshot adds
		// nothing, then each interval splits by its own ratio.
		var active, idle float64
		for k := 1; k < len(snaps); k++ {
			var deltaJ float64
			for i, c := range snaps[k].Nodes[0].Components {
				if c.Type == zone.hwType {
					deltaJ += c.EnergyJ - snaps[k-1].Nodes[0].Components[i].EnergyJ
				}
			}
			active += deltaJ * compatScript[k].cpu
			idle += deltaJ * (1 - compatScript[k].cpu)
		}
		compatNear(t, zone.name+" active joules", compatOne(t, samples, k012CPUActiveJoules, match), active)
		compatNear(t, zone.name+" idle joules", compatOne(t, samples, k012CPUIdleJoules, match), idle)
	}

	// GPU 2: idle at 0 % in the first two snapshots, at 60 + 2 W; then
	// 50 % and 100 %.
	match := map[string]string{k012LabelNodeName: "nvl-0", k012LabelGPU: "2"}
	g := n.Components[len(n.Components)-8+2]
	if g.ID != "gpu2" {
		t.Fatalf("component %s, want gpu2", g.ID)
	}
	compatNear(t, "gpu2 joules", compatOne(t, samples, k012GPUJoules, match), g.DeviceEnergyJ)
	compatNear(t, "gpu2 watts", compatOne(t, samples, k012GPUWatts, match), g.PowerW)
	compatNear(t, "gpu2 idle watts", compatOne(t, samples, k012GPUIdleWatts, match), 62)
	compatNear(t, "gpu2 active watts", compatOne(t, samples, k012GPUActiveWatts, match), g.PowerW-62)
	var active, idle float64
	for k := 1; k < len(snaps); k++ {
		c := snaps[k].Nodes[0].Components[len(n.Components)-8+2]
		deltaJ := c.DeviceEnergyJ - snaps[k-1].Nodes[0].Components[len(n.Components)-8+2].DeviceEnergyJ
		share := (c.PowerW - 62) / c.PowerW
		active += deltaJ * share
		idle += deltaJ * (1 - share)
	}
	compatNear(t, "gpu2 active joules", compatOne(t, samples, k012GPUActiveJoules, match), active)
	compatNear(t, "gpu2 idle joules", compatOne(t, samples, k012GPUIdleJoules, match), idle)
	if got := compatOne(t, samples, k012GPUJoules, match); got != g.DeviceEnergyJ {
		t.Errorf("gpu2 joules %g, want the device counter %g", got, g.DeviceEnergyJ)
	}
	if uuid := compatFind(samples, k012GPUJoules, match)[0].labels[k012LabelGPUUUID]; uuid != g.UUID {
		t.Errorf("gpu2 uuid %s, want %s", uuid, g.UUID)
	}
}

// CP04: a GPU that has not been seen at zero utilisation has no idle
// power, so all its power and energy are active.
func TestCP04Kepler012GPUIdleUnknownUntilSeenIdle(t *testing.T) {
	set, err := NewSet([]string{Kepler012}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatNVL())
	for _, dt := range []float64{0, 10, 10} {
		set.Observe(c.snap(dt, 0.5, 0.7))
	}
	samples := compatGather(t, set, Kepler012)
	for i := 0; i < 8; i++ {
		match := map[string]string{k012LabelGPU: fmt.Sprint(i)}
		if v := compatOne(t, samples, k012GPUIdleWatts, match); v != 0 {
			t.Errorf("gpu%d idle watts %g before an idle reading, want 0", i, v)
		}
		if v := compatOne(t, samples, k012GPUIdleJoules, match); v != 0 {
			t.Errorf("gpu%d idle joules %g before an idle reading, want 0", i, v)
		}
		compatNear(t, fmt.Sprintf("gpu%d active watts", i), compatOne(t, samples, k012GPUActiveWatts, match), compatOne(t, samples, k012GPUWatts, match))
	}
}

// k012ZoneJ is the energy counter of a kepler-0.12 cpu zone in a node
// snapshot: the sum of its measured ledgers.
func k012ZoneJ(n *emulate.NodeSnapshot, hwType string) float64 {
	var j float64
	for _, c := range n.Components {
		if c.Type == hwType && c.Measured {
			j += c.EnergyJ
		}
	}
	return j
}

var k012Zones = []struct{ name, hwType string }{{k012ZonePackage, semconv.HwTypeCPU}, {k012ZoneDRAM, semconv.HwTypeMemory}}

// CP04: a snapshot that repeats the last one's time, or comes before it,
// changes nothing, watts included. The next snapshot in order is measured
// from the last one in order, so the active and idle energy over the window
// still sum to the growth of the zone and device counters, and its power is
// that of its own interval.
func TestCP04Kepler012RepeatedAndOutOfOrderSnapshotsChangeNothing(t *testing.T) {
	set, err := NewSet([]string{Kepler012}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(compatIntel(), compatNVL())
	first := c.snap(0, 0.5, 0.5)
	late := c.snap(5, 0.5, 0.5)
	mid := c.snap(5, 0.5, 0.5)
	last := c.snap(10, 0.5, 0.5)

	set.Observe(first)
	set.Observe(mid)
	before := compatGather(t, set, Kepler012)
	set.Observe(mid)
	compatSame(t, "a repeated snapshot", before, compatGather(t, set, Kepler012))
	set.Observe(late)
	compatSame(t, "a snapshot 5 s before the last one", before, compatGather(t, set, Kepler012))

	set.Observe(last)
	end := compatGather(t, set, Kepler012)
	dt := last.Time.Sub(mid.Time).Seconds()
	for i := range last.Nodes {
		n, m, f := &last.Nodes[i], &mid.Nodes[i], &first.Nodes[i]
		for _, zone := range k012Zones {
			match := map[string]string{k012LabelNodeName: n.Name, k012LabelZone: zone.name}
			what := n.Name + " " + zone.name
			compatNear(t, what+" watts", compatOne(t, end, k012CPUWatts, match), (k012ZoneJ(n, zone.hwType)-k012ZoneJ(m, zone.hwType))/dt)
			compatNear(t, what+" active+idle joules",
				compatOne(t, end, k012CPUActiveJoules, match)+compatOne(t, end, k012CPUIdleJoules, match),
				k012ZoneJ(n, zone.hwType)-k012ZoneJ(f, zone.hwType))
		}
		for j, g := range n.Components {
			if g.Type != semconv.HwTypeGPU {
				continue
			}
			match := map[string]string{k012LabelNodeName: n.Name, k012LabelGPU: fmt.Sprint(g.Index)}
			compatNear(t, n.Name+" "+g.ID+" active+idle joules",
				compatOne(t, end, k012GPUActiveJoules, match)+compatOne(t, end, k012GPUIdleJoules, match),
				g.DeviceEnergyJ-f.Components[j].DeviceEnergyJ)
		}
	}
}

// CP04: a zone counter that falls, which a ledger never does, adds no energy
// and reports no power, never a negative one; the zone reports the lower
// reading, and the next interval is measured from it.
func TestCP04Kepler012FallingCounterAddsNothing(t *testing.T) {
	set, err := NewSet([]string{Kepler012}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	intel := compatIntel()
	c := compatNewCluster(intel)
	set.Observe(c.snap(0, 0.5, 0.5))
	set.Observe(c.snap(10, 0.5, 0.5))
	before := compatGather(t, set, Kepler012)
	for id := range intel.energy {
		intel.energy[id] = 0 // a ledger restarted from zero
	}
	fell := c.snap(10, 0.5, 0.5)
	set.Observe(fell)
	after := compatGather(t, set, Kepler012)
	next := c.snap(10, 0.5, 0.5)
	set.Observe(next)
	end := compatGather(t, set, Kepler012)

	f, x := &fell.Nodes[0], &next.Nodes[0]
	for _, zone := range k012Zones {
		match := map[string]string{k012LabelNodeName: "intel-0", k012LabelZone: zone.name}
		if was, now := compatOne(t, before, k012CPUJoules, match), k012ZoneJ(f, zone.hwType); !(now < was) {
			t.Fatalf("%s counter %g after %g; the test needs it to fall", zone.name, now, was)
		}
		compatNear(t, zone.name+" joules after the fall", compatOne(t, after, k012CPUJoules, match), k012ZoneJ(f, zone.hwType))
		for _, family := range []string{k012CPUWatts, k012CPUActiveWatts, k012CPUIdleWatts} {
			if v := compatOne(t, after, family, match); v != 0 {
				t.Errorf("%s %s = %g after the counter fell, want 0", zone.name, family, v)
			}
		}
		for _, family := range []string{k012CPUActiveJoules, k012CPUIdleJoules} {
			if was, now := compatOne(t, before, family, match), compatOne(t, after, family, match); now != was {
				t.Errorf("%s %s went from %g to %g when the counter fell", zone.name, family, was, now)
			}
		}
		compatNear(t, zone.name+" watts after the fall", compatOne(t, end, k012CPUWatts, match), (k012ZoneJ(x, zone.hwType)-k012ZoneJ(f, zone.hwType))/10)
	}
}
