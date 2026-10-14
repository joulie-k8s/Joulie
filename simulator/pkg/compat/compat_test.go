package compat

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// compatUpdateGolden rewrites testdata/*.prom from the exporters:
//
//	go test ./simulator/pkg/compat/ -run 'CP01|CP04|CP05|CP09' -update
var compatUpdateGolden = flag.Bool("update", false, "rewrite simulator/pkg/compat/testdata/*.prom from the exporters")

// compatStart is the time of the first test snapshot.
var compatStart = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// compatFake is an emulated node the tests step by hand. Each component's
// power follows the load, and its energy grows by that power times the step,
// so the energy of an interval is exactly the PowerW the snapshot reports at
// its end times its length. Ledgers start above zero, as after an earlier
// run. A node with RAPL also has a sysfs tree under root, written at every
// step as Kepler v0.12.0 reads it (compatFake.writeZones).
type compatFake struct {
	name      string
	sockets   int
	rapl      bool   // packages have energy counters
	dram      bool   // DRAM zones have energy counters
	gpus      int    // GPU count
	gpuVendor string // NVIDIA or AMD
	energy    map[string]float64
	device    map[string]float64
	hostJ     float64

	t     *testing.T
	root  string // the node's tree, set by compatNewCluster
	maxUJ uint64 // max_energy_range_uj of every zone; 0 is compatMaxUJ
}

// compatMaxUJ is the counter range of a RAPL zone with a 61035 nJ energy
// unit, the most common one.
var compatMaxUJ = uint64(hwemu.MaxEnergyRangeUJ(61035))

func compatNewFake(name string, sockets int, rapl, dram bool, gpus int, gpuVendor string) *compatFake {
	return &compatFake{
		name: name, sockets: sockets, rapl: rapl, dram: dram, gpus: gpus, gpuVendor: gpuVendor,
		energy: map[string]float64{}, device: map[string]float64{}, hostJ: 50000,
	}
}

// writeZones writes the node's RAPL zones under root as the kernel lays out
// class/powercap: the control type intel-rapl, which has no name file, and
// per socket intel-rapl:<s> (package-<s>) and, with DRAM energy,
// intel-rapl:<s>:0 (dram). energy_uj is the component's ledger in uJ,
// wrapped at max_energy_range_uj, as a RAPL counter wraps.
func (f *compatFake) writeZones(n *emulate.NodeSnapshot) {
	if f.root == "" || !f.rapl {
		return
	}
	maxUJ := f.maxUJ
	if maxUJ == 0 {
		maxUJ = compatMaxUJ
	}
	class := filepath.Join(f.root, layout.SysDir, "class", "powercap")
	write := func(entry, file, content string) {
		dir := filepath.Join(class, entry)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if file == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	write("intel-rapl", "", "")
	zone := func(entry, name string, energyJ float64) {
		write(entry, "name", name)
		write(entry, "max_energy_range_uj", strconv.FormatUint(maxUJ, 10))
		write(entry, "energy_uj", strconv.FormatUint(uint64(math.Floor(energyJ*1e6))%maxUJ, 10))
	}
	for _, c := range n.Components {
		switch {
		case c.Type == semconv.HwTypeCPU && c.Measured:
			zone(fmt.Sprintf("intel-rapl:%d", c.Index), c.Name, c.EnergyJ)
		case c.Type == semconv.HwTypeMemory && c.Measured:
			zone(fmt.Sprintf("intel-rapl:%d:0", c.Index), c.Name, c.EnergyJ)
		}
	}
}

// The test fleet, after the builtin profiles: the Intel and NVL hosts the
// goldens use, an EPYC host whose DRAM has no energy counter, an MI300X host,
// a virtual machine, a single socket host, and an NVIDIA host without RAPL.
func compatIntel() *compatFake  { return compatNewFake("intel-0", 2, true, true, 0, "") }
func compatNVL() *compatFake    { return compatNewFake("nvl-0", 2, true, true, 8, "NVIDIA") }
func compatEPYC() *compatFake   { return compatNewFake("epyc-0", 2, true, false, 0, "") }
func compatMI300X() *compatFake { return compatNewFake("mi300x-0", 2, true, true, 8, "AMD") }
func compatVM() *compatFake     { return compatNewFake("vm-0", 1, false, false, 0, "") }
func compatSingle() *compatFake { return compatNewFake("single-0", 1, true, true, 0, "") }
func compatNoRAPL() *compatFake { return compatNewFake("gpu-norapl-0", 2, false, false, 2, "NVIDIA") }

// step advances the node by dt seconds at the given loads and returns its
// snapshot. Powers are in W: a package draws 90 + 110 x load (plus 5 W per
// socket index), a DRAM zone 12 + 8 x load, an NVIDIA GPU 60 + 290 x load
// (plus 1 W per index), an AMD GPU 150 + 550 x load, and the platform 80.
func (f *compatFake) step(dt, cpuUtil, gpuUtil float64) emulate.NodeSnapshot {
	n := emulate.NodeSnapshot{
		Name: f.name, Pool: f.name, Profile: f.name, Root: f.root,
		StartTime: compatStart.Add(-time.Hour),
		Host:      emulate.HostReading{Measured: f.rapl, OtherW: 80, InletC: 22},
	}
	hostW := 80.0
	add := func(c emulate.ComponentReading) {
		if _, ok := f.energy[c.ID]; !ok {
			f.energy[c.ID] = 1000 // a ledger that started before this run
		}
		f.energy[c.ID] += c.PowerW * dt
		c.EnergyJ = f.energy[c.ID]
		hostW += c.PowerW
		n.Components = append(n.Components, c)
	}
	for s := 0; s < f.sockets; s++ {
		cpu := fmt.Sprintf("cpu%d", s)
		add(emulate.ComponentReading{
			ID: cpu, Type: semconv.HwTypeCPU, Name: fmt.Sprintf("package-%d", s), Vendor: "Intel", Model: "Intel(R) Xeon(R) test CPU",
			SysfsPath: fmt.Sprintf("devices/virtual/powercap/intel-rapl/intel-rapl:%d", s),
			Index:     s, Measured: f.rapl, PowerW: 90 + 110*cpuUtil + 5*float64(s), Utilization: cpuUtil,
			SpeedHz: 2.4e9, SpeedMaxHz: 3.5e9, SpeedLimitHz: 3.5e9,
		})
		add(emulate.ComponentReading{
			ID: fmt.Sprintf("memory%d", s), Type: semconv.HwTypeMemory, Name: "dram", Parent: cpu,
			SysfsPath: fmt.Sprintf("devices/virtual/powercap/intel-rapl/intel-rapl:%d/intel-rapl:%d:0", s, s),
			Index:     s, Measured: f.dram, PowerW: 12 + 8*cpuUtil, Utilization: cpuUtil,
		})
	}
	for i := 0; i < f.gpus; i++ {
		c := emulate.ComponentReading{ID: fmt.Sprintf("gpu%d", i), Type: semconv.HwTypeGPU, Index: i, Measured: true, Utilization: gpuUtil}
		if f.gpuVendor == "NVIDIA" {
			c.Name, c.Vendor, c.Model = "NVIDIA H100 NVL", "NVIDIA", "NVIDIA H100 NVL"
			c.UUID = fmt.Sprintf("GPU-%08x-0000-4000-8000-%012x", i+1, i+1)
			c.PCIBusID = fmt.Sprintf("%08X:%02X:00.0", 0, 0x1B+i)
			c.PowerW = 60 + 290*gpuUtil + float64(i)
			c.LimitW, c.RequestedLimitW = 400, 400
			if _, ok := f.device[c.ID]; !ok {
				f.device[c.ID] = 3600 // the device counter since the driver loaded
			}
			f.device[c.ID] += c.PowerW * dt
			c.DeviceEnergyJ = f.device[c.ID]
			n.DriverVersion = "550.54.15"
		} else {
			c.Name, c.Vendor, c.Model = "AMD Instinct MI300X", "AMD", "AMD Instinct MI300X"
			c.PCIBusID = fmt.Sprintf("0000:%02x:00.0", 0x0C+i)
			c.PowerW = 150 + 550*gpuUtil
		}
		add(c)
	}
	f.hostJ += hostW * dt
	n.Host.PowerW, n.Host.EnergyJ = hostW, f.hostJ
	f.writeZones(&n)
	return n
}

// compatCluster steps a set of nodes together into snapshots.
type compatCluster struct {
	seq   uint64
	now   time.Time
	nodes []*compatFake
}

// compatNewCluster gives every node a tree in a directory of t.
func compatNewCluster(t *testing.T, nodes ...*compatFake) *compatCluster {
	t.Helper()
	dir := t.TempDir()
	for _, f := range nodes {
		f.t, f.root = t, filepath.Join(dir, f.name)
	}
	return &compatCluster{now: compatStart, nodes: nodes}
}

// observe writes each node's tree from snap, as the emulator's tick leaves
// it when the snapshot is published, and observes snap. A test that steps
// several snapshots before it observes them needs it; the tree otherwise
// holds the last step.
func (c *compatCluster) observe(set *Set, snap *emulate.Snapshot) {
	for i, f := range c.nodes {
		f.writeZones(&snap.Nodes[i])
	}
	set.Observe(snap)
}

// snap advances every node by dt seconds and returns the snapshot.
func (c *compatCluster) snap(dt, cpuUtil, gpuUtil float64) *emulate.Snapshot {
	c.seq++
	c.now = c.now.Add(time.Duration(dt * float64(time.Second)))
	s := &emulate.Snapshot{Seq: c.seq, Cluster: "compat-test", Time: c.now}
	for _, f := range c.nodes {
		s.Nodes = append(s.Nodes, f.step(dt, cpuUtil, gpuUtil))
	}
	return s
}

// compatScript is the load the goldens are taken at: a first snapshot, then
// three 10 s ticks at low, high and mixed load. The loads are binary
// fractions, so every power and energy is exact and the goldens do not
// depend on how a platform rounds or fuses floating-point operations.
var compatScript = []struct{ dt, cpu, gpu float64 }{
	{0, 0.25, 0}, {10, 0.125, 0}, {10, 0.875, 0.5}, {10, 0.5, 1},
}

// compatRun observes the script with a Set of the named exporters.
func compatRun(t *testing.T, names []string, o Options, nodes ...*compatFake) (*Set, []*emulate.Snapshot) {
	t.Helper()
	set, err := NewSet(names, o)
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, nodes...)
	var snaps []*emulate.Snapshot
	for _, s := range compatScript {
		snap := c.snap(s.dt, s.cpu, s.gpu)
		c.observe(set, snap)
		snaps = append(snaps, snap)
	}
	return set, snaps
}

// compatScrape GETs path from the Set's handler.
func compatScrape(t *testing.T, set *Set, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// compatSample is one served series.
type compatSample struct {
	family string
	labels map[string]string
	value  float64
}

// compatGather returns every series an exporter serves.
func compatGather(t *testing.T, set *Set, name string) []compatSample {
	t.Helper()
	reg := set.registry[name]
	if reg == nil {
		t.Fatalf("%s is not enabled", name)
	}
	out, err := compatCollect(reg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// compatCollect returns every series a registry serves. It does not stop
// the test, so a goroutine may call it.
func compatCollect(reg *prometheus.Registry) ([]compatSample, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	var out []compatSample
	for _, f := range families {
		for _, m := range f.GetMetric() {
			s := compatSample{family: f.GetName(), labels: map[string]string{}}
			for _, l := range m.GetLabel() {
				s.labels[l.GetName()] = l.GetValue()
			}
			switch {
			case m.GetCounter() != nil:
				s.value = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				s.value = m.GetGauge().GetValue()
			default:
				return nil, fmt.Errorf("%s: neither counter nor gauge", f.GetName())
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// compatSame fails unless got holds the series of want, in order, with the
// same values.
func compatSame(t *testing.T, what string, want, got []compatSample) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s changed the series count from %d to %d", what, len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.family != w.family || fmt.Sprint(g.labels) != fmt.Sprint(w.labels) || g.value != w.value {
			t.Errorf("%s changed %s%v = %g to %s%v = %g", what, w.family, w.labels, w.value, g.family, g.labels, g.value)
		}
	}
}

// compatFind returns the samples of family whose labels include match.
func compatFind(samples []compatSample, family string, match map[string]string) []compatSample {
	var out []compatSample
next:
	for _, s := range samples {
		if s.family != family {
			continue
		}
		for k, v := range match {
			if s.labels[k] != v {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

// compatOne returns the value of the single sample of family matching
// match, failing the test unless there is exactly one.
func compatOne(t *testing.T, samples []compatSample, family string, match map[string]string) float64 {
	t.Helper()
	got := compatFind(samples, family, match)
	if len(got) != 1 {
		t.Fatalf("%s%v: %d series, want 1", family, match, len(got))
	}
	return got[0].value
}

// compatNear fails unless got is within 1e-9 relative (1e-9 absolute near
// zero) of want.
func compatNear(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %.12g, want %.12g", what, got, want)
	}
}

// compatLabelNames returns a sample's label names, sorted.
func compatLabelNames(s compatSample) []string {
	names := make([]string, 0, len(s.labels))
	for k := range s.labels {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// compatGolden compares got with testdata/<file>, or rewrites the file with
// -update.
func compatGolden(t *testing.T, file, got string) {
	t.Helper()
	path := filepath.Join("testdata", file)
	if *compatUpdateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("%s differs at line %d:\n got  %s\n want %s\n(run with -update after checking the change)", path, i+1, g, w)
			}
		}
	}
}

// CP07: the handler serves only the enabled exporters, at their path and no
// other, and NewSet rejects an unknown exporter or DCGM hostname label with
// the valid values. Names are trimmed and repeats are skipped.
func TestCP07HandlerServesOnlyEnabledExporters(t *testing.T) {
	set, err := NewSet([]string{" kepler-0.7", "kepler-0.7 ", "", DCGM484}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(set.Enabled(), ","), "kepler-0.7,dcgm-4.8.4"; got != want {
		t.Fatalf("Enabled() = %s, want %s", got, want)
	}
	set.Observe(compatNewCluster(t, compatNVL()).snap(0, 0.5, 0.5))

	for _, c := range []struct {
		path string
		code int
	}{
		{Path(Kepler07), http.StatusOK},
		{Path(DCGM484), http.StatusOK},
		{Path(Kepler012), http.StatusNotFound},
		{Path(AMDDME152), http.StatusNotFound},
		{Path("kepler-0.9"), http.StatusNotFound},
		{"/exporters/kepler-0.7", http.StatusNotFound},
		{"/exporters/kepler-0.7/metrics/extra", http.StatusNotFound},
		{"/exporters//metrics", http.StatusNotFound},
		{"/metrics", http.StatusNotFound},
	} {
		code, body := compatScrape(t, set, c.path)
		if code != c.code {
			t.Errorf("GET %s = %d, want %d", c.path, code, c.code)
		}
		if code == http.StatusOK && !strings.Contains(body, "# TYPE ") {
			t.Errorf("GET %s served no metric family:\n%s", c.path, body)
		}
	}
	if _, body := compatScrape(t, set, Path(Kepler07)); !strings.Contains(body, "kepler_node_package_joules_total") || strings.Contains(body, "DCGM_") {
		t.Errorf("kepler-0.7 serves another exporter's series:\n%s", body)
	}

	_, err = NewSet([]string{Kepler07, "kepler-current", "dcgm"}, Options{})
	if err == nil {
		t.Fatal("NewSet accepted unknown exporters")
	}
	for _, want := range append(Names(), `"kepler-current"`, `"dcgm"`) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if _, err := NewSet([]string{DCGM484}, Options{DCGMHostnameLabel: "host"}); err == nil ||
		!strings.Contains(err.Error(), "hostname") || !strings.Contains(err.Error(), "Hostname") {
		t.Errorf("NewSet with DCGM hostname label host: %v, want an error naming hostname and Hostname", err)
	}

	empty, err := NewSet(nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	empty.Observe(nil)
	var none *Set
	none.Observe(compatNewCluster(t, compatNVL()).snap(0, 0.5, 0.5))
	for _, name := range Names() {
		if code, _ := compatScrape(t, empty, Path(name)); code != http.StatusNotFound {
			t.Errorf("an empty Set serves %s with %d", name, code)
		}
		if code, _ := compatScrape(t, none, Path(name)); code != http.StatusNotFound {
			t.Errorf("a nil Set serves %s with %d", name, code)
		}
	}
}

// CP07: scrapes may run while snapshots are observed; each scrape sees one
// whole snapshot, which compatWhole checks. Run with -race.
func TestCP07ObserveAndScrapeConcurrently(t *testing.T) {
	set, err := NewSet(Names(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatIntel(), compatNVL())
	set.Observe(c.snap(0, 0.2, 0.2))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, name := range Names() {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path(name), nil))
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d", name, rec.Code)
					return
				}
				samples, err := compatCollect(set.registry[name])
				if err == nil {
					err = compatWhole(samples)
				}
				if err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		}(name)
	}
	for i := 0; i < 200; i++ {
		set.Observe(c.snap(1, float64(i%10)/10, float64(i%5)/5))
	}
	close(stop)
	wg.Wait()
}

// compatWhole returns an error unless samples come from one snapshot of
// intel-0 and nvl-0 stepped together. Both nodes have the same CPU series
// at the same load, so their CPU series are equal; the NVL GPUs share one
// load, so their power differs only by the 1 W per index the fake adds, and
// their utilisation not at all. A view that mixes two snapshots, or holds
// only part of one, breaks one of these.
func compatWhole(samples []compatSample) error {
	k07CPU := map[string]bool{k07Family(k07Package): true, k07Family(k07Core): true, k07Family(k07Uncore): true, k07Family(k07DRAM): true}
	cpu := map[string]map[string]float64{} // node, series without the node, value
	gpu := map[string]float64{}            // family, the value every GPU shares
	for _, s := range samples {
		nodeLabel, gpuLabel := "", ""
		switch {
		case k07CPU[s.family]:
			nodeLabel = k07LabelInstance
		case strings.HasPrefix(s.family, "kepler_node_cpu_"):
			nodeLabel = k012LabelNodeName
		case s.family == k012GPUWatts:
			gpuLabel = k012LabelGPU
		case s.family == dcgmPowerUsage || s.family == dcgmGPUUtil:
			gpuLabel = dcgmLabelGPU
		default:
			continue
		}
		if gpuLabel != "" {
			v := s.value
			if s.family != dcgmGPUUtil {
				index, err := strconv.Atoi(s.labels[gpuLabel])
				if err != nil {
					return fmt.Errorf("%s%v: %v", s.family, s.labels, err)
				}
				v -= float64(index)
			}
			if was, ok := gpu[s.family]; ok && math.Abs(v-was) > 1e-6 {
				return fmt.Errorf("%s%v = %g, not the load of the other GPUs (%g)", s.family, s.labels, s.value, was)
			}
			gpu[s.family] = v
			continue
		}
		node := s.labels[nodeLabel]
		rest := map[string]string{}
		for k, v := range s.labels {
			if k != nodeLabel {
				rest[k] = v
			}
		}
		if cpu[node] == nil {
			cpu[node] = map[string]float64{}
		}
		cpu[node][s.family+fmt.Sprint(rest)] = s.value
	}
	if len(cpu) == 0 {
		return nil
	}
	intel, nvl := cpu["intel-0"], cpu["nvl-0"]
	if len(cpu) != 2 || len(intel) != len(nvl) {
		return fmt.Errorf("CPU series per node intel-0 %d, nvl-0 %d, of %d nodes", len(intel), len(nvl), len(cpu))
	}
	for key, v := range intel {
		if w, ok := nvl[key]; !ok || w != v {
			return fmt.Errorf("%s: intel-0 %g, nvl-0 %g (present %t)", key, v, w, ok)
		}
	}
	return nil
}

// CP08: a node without package energy, a virtual machine or an NVIDIA host
// without RAPL, gets no Kepler series, while its GPUs still reach DCGM. An
// EPYC host whose DRAM has no energy counter reads 0 for kepler-0.7 dram
// and has no kepler-0.12 dram zone.
func TestCP08NodeWithoutPackageEnergyEmitsNoKeplerSeries(t *testing.T) {
	set, _ := compatRun(t, Names(), Options{}, compatVM(), compatNoRAPL(), compatEPYC())
	for _, name := range []string{Kepler07, Kepler012} {
		for _, s := range compatGather(t, set, name) {
			if node := s.labels[k07LabelInstance] + s.labels[k012LabelNodeName]; node != "epyc-0" {
				t.Errorf("%s serves %s%v for %s, which has no package energy", name, s.family, s.labels, node)
			}
		}
	}
	if got := compatFind(compatGather(t, set, DCGM484), dcgmTotalEnergy, map[string]string{dcgmLabelHostname: "gpu-norapl-0"}); len(got) != 2 {
		t.Errorf("dcgm-4.8.4 serves %d energy series for the GPUs of the host without RAPL, want 2", len(got))
	}

	k07 := compatGather(t, set, Kepler07)
	for _, socket := range []string{"0", "1"} {
		match := map[string]string{k07LabelInstance: "epyc-0", k07LabelPackage: socket, k07LabelMode: k07ModeDynamic}
		if v := compatOne(t, k07, "kepler_node_dram_joules_total", match); v != 0 {
			t.Errorf("epyc-0 dram socket %s dynamic = %g, want 0", socket, v)
		}
		if got := compatFind(k07, "kepler_node_dram_joules_total", map[string]string{k07LabelInstance: "epyc-0", k07LabelMode: k07ModeIdle}); len(got) != 0 {
			t.Errorf("epyc-0 dram has idle series %v; a zone that never moved has none", got)
		}
		if v := compatOne(t, k07, "kepler_node_package_joules_total", map[string]string{k07LabelInstance: "epyc-0", k07LabelPackage: socket, k07LabelMode: k07ModeIdle}); !(v > 0) {
			t.Errorf("epyc-0 package socket %s idle = %g, want > 0", socket, v)
		}
	}
	k012 := compatGather(t, set, Kepler012)
	if got := compatFind(k012, k012CPUJoules, map[string]string{k012LabelZone: "dram"}); len(got) != 0 {
		t.Errorf("kepler-0.12 serves a dram zone for a host without DRAM energy: %v", got)
	}
	if got := compatFind(k012, k012CPUJoules, map[string]string{k012LabelZone: "package", k012LabelNodeName: "epyc-0"}); len(got) != 1 {
		t.Errorf("kepler-0.12 serves %d package zones for epyc-0, want 1", len(got))
	}
}
