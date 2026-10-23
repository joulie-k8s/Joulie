package compat

import (
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// compatKepler012GPU is a Set option with the kepler-0.12 GPU series on.
var compatKepler012GPU = Options{Kepler012GPU: true}

// CP04: kepler-0.12 serves the v0.12.0 node families with the labels zone
// and path (cpu) or gpu, gpu_uuid, gpu_name and vendor (gpu), plus
// node_name; zones of one name over two sockets are one zone with path
// aggregated-<zone>, and a single zone has its class/powercap entry as path;
// there are no AMD GPU series; and the exposition for an Intel and an NVL
// host, with the GPU series on, matches the golden that example 04 reads.
func TestCP04Kepler012FamiliesLabelsPathsAndGolden(t *testing.T) {
	set, _ := compatRun(t, []string{Kepler012}, compatKepler012GPU, compatIntel(), compatNVL(), compatMI300X(), compatSingle())
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
	for _, families := range []map[string]bool{cpu, gpu} {
		for f := range families {
			if !seen[f] {
				t.Errorf("family %s missing", f)
			}
		}
	}

	for _, c := range []struct{ node, zone, path string }{
		{"intel-0", "package", "aggregated-package"},
		{"intel-0", "dram", "aggregated-dram"},
		{"mi300x-0", "package", "aggregated-package"},
		{"single-0", "package", "/host/sys/class/powercap/intel-rapl:0"},
		{"single-0", "dram", "/host/sys/class/powercap/intel-rapl:0:0"},
	} {
		compatOne(t, samples, k012CPUJoules, map[string]string{k012LabelNodeName: c.node, k012LabelZone: c.zone, k012LabelPath: c.path})
	}
	if got := compatFind(samples, k012GPUWatts, map[string]string{k012LabelNodeName: "nvl-0"}); len(got) != 8 {
		t.Errorf("nvl-0 has %d GPU power series, want 8", len(got))
	}
	if got := compatFind(samples, k012GPUWatts, map[string]string{k012LabelNodeName: "mi300x-0"}); len(got) != 0 {
		t.Errorf("mi300x-0 has AMD GPU series %v", got)
	}

	golden, _ := compatRun(t, []string{Kepler012}, compatKepler012GPU, compatIntel(), compatNVL())
	code, body := compatScrape(t, golden, Path(Kepler012))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path(Kepler012), code)
	}
	compatGolden(t, "kepler-0.12.prom", body)
}

// CP14: the kepler-0.12 GPU series need Options.Kepler012GPU, as Kepler's
// GPU series need its experimental GPU feature, which is off by default;
// the cpu series are the same either way.
func TestCP14Kepler012GPUSeriesOnlyWithTheExperimentalFeature(t *testing.T) {
	off, _ := compatRun(t, []string{Kepler012}, Options{}, compatNVL())
	on, _ := compatRun(t, []string{Kepler012}, compatKepler012GPU, compatNVL())
	offSamples, onSamples := compatGather(t, off, Kepler012), compatGather(t, on, Kepler012)
	for _, s := range offSamples {
		if strings.HasPrefix(s.family, "kepler_node_gpu_") {
			t.Errorf("GPU series %s%v with the GPU feature off", s.family, s.labels)
		}
	}
	if got := compatFind(onSamples, k012GPUJoules, map[string]string{k012LabelNodeName: "nvl-0"}); len(got) != 8 {
		t.Errorf("nvl-0 has %d GPU energy series with the GPU feature on, want 8", len(got))
	}
	var cpuOn []compatSample
	for _, s := range onSamples {
		if strings.HasPrefix(s.family, "kepler_node_cpu_") {
			cpuOn = append(cpuOn, s)
		}
	}
	compatSame(t, "turning the GPU feature on", cpuOn, offSamples)
}

// k012ZoneJ is the energy of the measured components of hwType in a node
// snapshot, which the fake's zones of that type count.
func k012ZoneJ(n *emulate.NodeSnapshot, hwType string) float64 {
	var j float64
	for _, c := range n.Components {
		if c.Type == hwType && c.Measured {
			j += c.EnergyJ
		}
	}
	return j
}

var k012Zones = []struct{ name, hwType string }{{"package", semconv.HwTypeCPU}, {"dram", semconv.HwTypeMemory}}

// CP04: a zone's joules_total is its counter, here the sum of its ledgers;
// its power is the increment over the interval; the active part is that
// increment times the node CPU usage ratio and the idle part the rest. The
// first snapshot reports no power and puts the whole counter in idle. A GPU
// reports its device counter and power; its idle power is the lowest power
// seen at zero utilisation, 0 before that, and its energy splits by active
// over total power.
func TestCP04Kepler012ActiveSplit(t *testing.T) {
	set, snaps := compatRun(t, []string{Kepler012}, compatKepler012GPU, compatNVL())
	samples := compatGather(t, set, Kepler012)
	last, prev := snaps[len(snaps)-1], snaps[len(snaps)-2]
	n, p := &last.Nodes[0], &prev.Nodes[0]
	dt := last.Time.Sub(prev.Time).Seconds()
	ratio := compatScript[len(compatScript)-1].cpu

	for _, zone := range k012Zones {
		nowJ, wasJ := k012ZoneJ(n, zone.hwType), k012ZoneJ(p, zone.hwType)
		match := map[string]string{k012LabelNodeName: "nvl-0", k012LabelZone: zone.name}
		compatNear(t, zone.name+" joules", compatOne(t, samples, k012CPUJoules, match), nowJ)
		watts := (nowJ - wasJ) / dt
		compatNear(t, zone.name+" watts", compatOne(t, samples, k012CPUWatts, match), watts)
		compatNear(t, zone.name+" active watts", compatOne(t, samples, k012CPUActiveWatts, match), watts*ratio)
		compatNear(t, zone.name+" idle watts", compatOne(t, samples, k012CPUIdleWatts, match), watts*(1-ratio))

		// Active and idle totals over the script: the first snapshot's
		// counter is idle, then each interval splits by its own ratio.
		active, idle := 0.0, k012ZoneJ(&snaps[0].Nodes[0], zone.hwType)
		for k := 1; k < len(snaps); k++ {
			deltaJ := k012ZoneJ(&snaps[k].Nodes[0], zone.hwType) - k012ZoneJ(&snaps[k-1].Nodes[0], zone.hwType)
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
	set, err := NewSet([]string{Kepler012}, compatKepler012GPU)
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatNVL())
	for _, dt := range []float64{0, 10, 10} {
		c.observe(set, c.snap(dt, 0.5, 0.7))
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

// CP04: a snapshot that repeats the last one's time, or comes before it,
// changes nothing, watts included. The next snapshot in order is measured
// from the last one in order, so the active and idle energy still add up to
// the zone and device counters, and its power is that of its own interval.
func TestCP04Kepler012RepeatedAndOutOfOrderSnapshotsChangeNothing(t *testing.T) {
	set, err := NewSet([]string{Kepler012}, compatKepler012GPU)
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatIntel(), compatNVL())
	first := c.snap(0, 0.5, 0.5)
	late := c.snap(5, 0.5, 0.5)
	mid := c.snap(5, 0.5, 0.5)
	last := c.snap(10, 0.5, 0.5)

	c.observe(set, first)
	c.observe(set, mid)
	before := compatGather(t, set, Kepler012)
	c.observe(set, mid)
	compatSame(t, "a repeated snapshot", before, compatGather(t, set, Kepler012))
	c.observe(set, late)
	compatSame(t, "a snapshot 5 s before the last one", before, compatGather(t, set, Kepler012))

	c.observe(set, last)
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
				k012ZoneJ(n, zone.hwType))
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

// CP02: joules_total is the RAPL counter as the kernel shows it, which
// wraps at max_energy_range_uj: a single zone reports energy_uj, and the
// zones of one name summed wrap at the sum of their ranges. The power and
// the active and idle counters follow the energy across the wrap, so
// active plus idle keeps growing with the ledgers.
func TestCP02Kepler012CounterWrapsAtTheRAPLRange(t *testing.T) {
	const maxUJ = 2_000_000_000 // 2000 J: the ledgers start at 1000 J
	single, intel := compatSingle(), compatIntel()
	single.maxUJ, intel.maxUJ = maxUJ, maxUJ
	set, err := NewSet([]string{Kepler012}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, single, intel)
	var snaps []*emulate.Snapshot
	for _, s := range compatScript {
		snap := c.snap(s.dt, s.cpu, s.gpu)
		c.observe(set, snap)
		snaps = append(snaps, snap)
	}
	samples := compatGather(t, set, Kepler012)
	last, prev := snaps[len(snaps)-1], snaps[len(snaps)-2]
	dt := last.Time.Sub(prev.Time).Seconds()
	wrapped := false
	for i, node := range []struct {
		name  string
		zones uint64 // zones summed into the series
	}{{"single-0", 1}, {"intel-0", 2}} {
		n, p := &last.Nodes[i], &prev.Nodes[i]
		for _, zone := range k012Zones {
			match := map[string]string{k012LabelNodeName: node.name, k012LabelZone: zone.name}
			what := node.name + " " + zone.name
			ledgerJ := k012ZoneJ(n, zone.hwType)
			rangeJ := float64(node.zones*maxUJ) / 1e6
			if ledgerJ >= rangeJ {
				wrapped = true
			}
			compatNear(t, what+" joules, the wrapped counter", compatOne(t, samples, k012CPUJoules, match), math.Mod(ledgerJ, rangeJ))
			compatNear(t, what+" watts across the wrap", compatOne(t, samples, k012CPUWatts, match), (ledgerJ-k012ZoneJ(p, zone.hwType))/dt)
			compatNear(t, what+" active+idle joules",
				compatOne(t, samples, k012CPUActiveJoules, match)+compatOne(t, samples, k012CPUIdleJoules, match), ledgerJ)
		}
	}
	if !wrapped {
		t.Fatal("no counter wrapped; the test needs a ledger past the range")
	}
}

// compatEmulated steps builtin profiles through the emulator, the way the
// simulator's emulate mode does, observing each snapshot with set, and
// returns the snapshots.
func compatEmulated(t *testing.T, set *Set, ticks int, nodes map[string]string) []*emulate.Snapshot {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	var members []clusterspec.Member
	for _, name := range names {
		builtin := profiles[nodes[name]]
		if builtin == nil {
			t.Fatalf("no %s profile", nodes[name])
		}
		// One core per package: the RAPL zones follow the sockets and dies,
		// and the per-CPU files would only slow the render down.
		p := *builtin
		p.CPU.CoresPerSocket.V, p.CPU.ThreadsPerCore.V = 1, 1
		p.Node.Capacity.CPU.V = int64(p.CPU.Sockets.V)
		cpu := 0.5
		members = append(members, clusterspec.Member{Name: name, Pool: nodes[name], Profile: &p, BaseLoad: clusterspec.Load{CPUUtil: &cpu}})
	}
	e, err := emulate.New(emulate.Config{
		Root: filepath.Join(t.TempDir(), "hwemu"), Cluster: "compat-test", Members: members,
		Start: compatStart, Substep: 10 * time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	var snaps []*emulate.Snapshot
	for i := 1; i <= ticks; i++ {
		snap, err := e.Tick(compatStart.Add(time.Duration(i)*10*time.Second), nil, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		set.Observe(snap)
		snaps = append(snaps, snap)
	}
	return snaps
}

// CP03: on emulated nodes kepler-0.12 reports every RAPL zone the rendered
// tree has, under the name Kepler gives it: a die zone keeps its name
// package-<socket>-die-<die> and is a zone of its own, psys and core are
// zones too, and zones of one name over sockets are one aggregated zone.
// The kepler preset's zone matcher selects exactly the package and die
// zones, and their power is the power of the CPU packages.
func TestCP03Kepler012ReportsEveryZoneOfTheTree(t *testing.T) {
	set, err := NewSet([]string{Kepler012}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	snaps := compatEmulated(t, set, 2, map[string]string{
		"multidie-0": "intel-xeon-2s-multidie",
		"psys-0":     "intel-xeon-2s-psys",
		"epyc-0":     "amd-epyc-2s-energy-only",
	})
	samples := compatGather(t, set, Kepler012)
	want := map[string]map[string]string{ // node, zone, path
		"multidie-0": {
			"package-0-die-0": "/host/sys/class/powercap/intel-rapl:0",
			"package-0-die-1": "/host/sys/class/powercap/intel-rapl:1",
			"package-1-die-0": "/host/sys/class/powercap/intel-rapl:2",
			"package-1-die-1": "/host/sys/class/powercap/intel-rapl:3",
			"dram":            "aggregated-dram",
		},
		"psys-0": {
			"package": "aggregated-package",
			"dram":    "aggregated-dram",
			"psys":    "/host/sys/class/powercap/intel-rapl:1",
		},
		"epyc-0": {
			"package": "aggregated-package",
			"core":    "aggregated-core",
		},
	}
	got := map[string]map[string]string{}
	for _, s := range compatFind(samples, k012CPUJoules, nil) {
		node := s.labels[k012LabelNodeName]
		if got[node] == nil {
			got[node] = map[string]string{}
		}
		got[node][s.labels[k012LabelZone]] = s.labels[k012LabelPath]
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("zones and paths by node:\n got  %v\n want %v", got, want)
	}

	packageZones := regexp.MustCompile(`^(?:` + inputs.KeplerPackageZones + `)$`)
	last, prev := snaps[len(snaps)-1], snaps[len(snaps)-2]
	dt := last.Time.Sub(prev.Time).Seconds()
	for i, n := range last.Nodes {
		var pkgW float64
		for _, s := range compatFind(samples, k012CPUWatts, map[string]string{k012LabelNodeName: n.Name}) {
			if packageZones.MatchString(s.labels[k012LabelZone]) {
				pkgW += s.value
			}
		}
		wantW := (k012ZoneJ(&n, semconv.HwTypeCPU) - k012ZoneJ(&prev.Nodes[i], semconv.HwTypeCPU)) / dt
		if !(wantW > 0) || math.Abs(pkgW-wantW) > 1e-3*wantW {
			t.Errorf("%s: package zones the preset matches draw %.4f W, the CPU packages %.4f W", n.Name, pkgW, wantW)
		}
	}
}

// CP10: the names the kepler and kepler-legacy presets read are the names
// the exporters write. kepler-0.12 serves every family and label the kepler
// preset selects, and only the package and die zones match its zone
// matcher; kepler-0.7 builds its family names from the upstream namespace
// and its node label is instance, which Prometheus stores as the
// exported_instance the kepler-legacy preset groups by.
func TestCP10KeplerNamesAreThePresetsNames(t *testing.T) {
	set, _ := compatRun(t, []string{Kepler07, Kepler012}, compatKepler012GPU, compatNVL())
	k012 := compatGather(t, set, Kepler012)
	for _, family := range []string{inputs.KeplerCPUJoules, inputs.KeplerCPUActiveJoules, inputs.KeplerCPUIdleJoules, inputs.KeplerCPUWatts, inputs.KeplerGPUJoules} {
		if len(compatFind(k012, family, map[string]string{inputs.KeplerNodeLabel: "nvl-0"})) == 0 {
			t.Errorf("kepler-0.12 serves no %s with %s", family, inputs.KeplerNodeLabel)
		}
	}
	packageZones := regexp.MustCompile(`^(?:` + inputs.KeplerPackageZones + `)$`)
	for _, s := range compatFind(k012, inputs.KeplerCPUActiveJoules, nil) {
		zone, ok := s.labels[inputs.KeplerZoneLabel]
		if !ok {
			t.Errorf("%s%v has no %s label", s.family, s.labels, inputs.KeplerZoneLabel)
		}
		if matched := packageZones.MatchString(zone); matched != (zone == "package") {
			t.Errorf("zone %q matched by the preset: %t", zone, matched)
		}
	}

	k07 := compatGather(t, set, Kepler07)
	for _, family := range []string{inputs.KeplerLegacyPackageJoules, inputs.KeplerLegacyPlatformJoules, inputs.KeplerGPUJoules} {
		if len(compatFind(k07, family, map[string]string{k07LabelInstance: "nvl-0"})) == 0 {
			t.Errorf("kepler-0.7 serves no %s with %s", family, k07LabelInstance)
		}
	}
	if got := "exported_" + k07LabelInstance; got != inputs.KeplerLegacyNodeLabel {
		t.Errorf("Prometheus stores kepler-0.7's node label as %s, the kepler-legacy preset groups by %s", got, inputs.KeplerLegacyNodeLabel)
	}
}
