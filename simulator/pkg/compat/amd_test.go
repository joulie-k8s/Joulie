package compat

import (
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// amdFamilies are the families amd-dme-1.5.2 serves.
var amdFamilies = []string{amdPackagePower, amdPowerUsage, amdEnergyConsumed, amdGFXActivity}

// amdWantLabels is the label set of an AMD GPU series of node, GPU index.
func amdWantLabels(node string, index int, product string) map[string]string {
	return map[string]string{
		amdLabelGPUID: strconv.Itoa(index), amdLabelCardModel: product, amdLabelGPUPartitionID: "0",
		amdLabelGPUComputePartitionType: "spx", amdLabelGPUMemoryPartitionType: "nps1",
		amdLabelDeploymentMode: "baremetal", amdLabelSerialNumber: amdSerial(node, index),
		amdLabelPod: "", amdLabelNamespace: "", amdLabelContainer: "",
		amdLabelJobID: "", amdLabelJobUser: "", amdLabelJobPartition: "", amdLabelHostname: node,
	}
}

// CP09: amd-dme-1.5.2 serves gpu_package_power and gpu_power_usage in whole
// W, gpu_energy_consumed in uJ and gpu_gfx_activity in percent per AMD GPU,
// all gauges, with the fourteen mandatory labels and hostname the node;
// NVIDIA GPUs and CPU hosts get nothing; and the exposition matches the
// golden.
func TestCP09AMDFamiliesLabelsAndValues(t *testing.T) {
	set, snaps := compatRun(t, []string{AMDDME152}, Options{}, compatMI300X(), compatNVL(), compatIntel())
	samples := compatGather(t, set, AMDDME152)
	n := &snaps[len(snaps)-1].Nodes[0]

	for _, family := range amdFamilies {
		got := compatFind(samples, family, nil)
		if len(got) != 8 {
			t.Errorf("%s: %d series, want 8 (the MI300X GPUs only)", family, len(got))
		}
		for _, s := range got {
			if s.labels[amdLabelHostname] != "mi300x-0" {
				t.Errorf("%s%v is not on mi300x-0", family, s.labels)
			}
		}
	}
	for i := range n.Components {
		c := &n.Components[i]
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		match := map[string]string{amdLabelGPUID: strconv.Itoa(c.Index)}
		for _, family := range amdFamilies {
			for _, s := range compatFind(samples, family, match) {
				if want := amdWantLabels("mi300x-0", c.Index, "AMD Instinct MI300X"); fmt.Sprint(s.labels) != fmt.Sprint(want) {
					t.Errorf("%s gpu%d labels %v, want %v", family, c.Index, s.labels, want)
				}
			}
		}
		compatNear(t, c.ID+" package power W", compatOne(t, samples, amdPackagePower, match), math.Round(c.PowerW))
		compatNear(t, c.ID+" power usage W", compatOne(t, samples, amdPowerUsage, match), math.Round(c.PowerW))
		compatNear(t, c.ID+" energy uJ", compatOne(t, samples, amdEnergyConsumed, match), math.Round(c.EnergyJ*1e6))
		compatNear(t, c.ID+" gfx activity %", compatOne(t, samples, amdGFXActivity, match), 100)
	}

	golden, _ := compatRun(t, []string{AMDDME152}, Options{}, compatMI300X())
	code, body := compatScrape(t, golden, Path(AMDDME152))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path(AMDDME152), code)
	}
	for _, family := range amdFamilies {
		if want := "# TYPE " + family + " gauge\n"; !strings.Contains(body, want) {
			t.Errorf("exposition has no %q", want)
		}
	}
	compatGolden(t, "amd-dme-1.5.2.prom", body)
}

// CP09: a GPU no collector could read gets no power or energy series but
// keeps its activity, and a power or energy reading of 0 is left out, as
// upstream exports nothing for a field it reads as 0.
func TestCP09AMDUnmeasuredAndZeroReadingsAreLeftOut(t *testing.T) {
	set, err := NewSet([]string{AMDDME152}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatMI300X())
	snap := c.snap(10, 0.5, 0.5)
	gpus := map[int]*emulate.ComponentReading{}
	for i := range snap.Nodes[0].Components {
		if comp := &snap.Nodes[0].Components[i]; comp.Type == semconv.HwTypeGPU {
			gpus[comp.Index] = comp
		}
	}
	gpus[0].Measured = false
	gpus[1].PowerW = 0.4 // rounds to 0 W
	gpus[2].EnergyJ = 0
	gpus[3].PowerW = math.NaN()
	set.Observe(snap)
	samples := compatGather(t, set, AMDDME152)
	for _, tc := range []struct {
		gpu                   int
		power, energy, active int
	}{
		{0, 0, 0, 1}, {1, 0, 1, 1}, {2, 1, 0, 1}, {3, 0, 1, 1}, {4, 1, 1, 1},
	} {
		match := map[string]string{amdLabelGPUID: strconv.Itoa(tc.gpu)}
		for _, f := range []struct {
			family string
			want   int
		}{
			{amdPackagePower, tc.power}, {amdPowerUsage, tc.power}, {amdEnergyConsumed, tc.energy}, {amdGFXActivity, tc.active},
		} {
			if got := len(compatFind(samples, f.family, match)); got != f.want {
				t.Errorf("gpu%d %s: %d series, want %d", tc.gpu, f.family, got, f.want)
			}
		}
	}
}

// CP09: a node keeps its series while its snapshot time does not advance,
// so a repeated or earlier snapshot never takes the energy back. A node that
// leaves the snapshot loses its series and comes back with its current ones.
func TestCP09AMDRepeatedAndOutOfOrderSnapshotsChangeNothing(t *testing.T) {
	set, err := NewSet([]string{AMDDME152}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatMI300X(), compatNVL())
	first := c.snap(0, 0.5, 0.5)
	late := c.snap(5, 0.5, 1)
	mid := c.snap(5, 0.5, 0.25)

	set.Observe(first)
	set.Observe(mid)
	before := compatGather(t, set, AMDDME152)
	set.Observe(mid)
	compatSame(t, "a repeated snapshot", before, compatGather(t, set, AMDDME152))
	set.Observe(late)
	compatSame(t, "a snapshot 5 s before the last one", before, compatGather(t, set, AMDDME152))

	last := c.snap(10, 0.5, 0.5)
	set.Observe(last)
	got := compatGather(t, set, AMDDME152)
	for _, comp := range last.Nodes[0].Components {
		if comp.Type != semconv.HwTypeGPU {
			continue
		}
		match := map[string]string{amdLabelHostname: "mi300x-0", amdLabelGPUID: strconv.Itoa(comp.Index)}
		compatNear(t, comp.ID+" energy uJ", compatOne(t, got, amdEnergyConsumed, match), math.Round(comp.EnergyJ*1e6))
		compatNear(t, comp.ID+" power W", compatOne(t, got, amdPowerUsage, match), math.Round(comp.PowerW))
	}

	gone := c.snap(10, 0.5, 0.5)
	gone.Nodes = gone.Nodes[1:]
	set.Observe(gone)
	if got := compatFind(compatGather(t, set, AMDDME152), amdEnergyConsumed, nil); len(got) != 0 {
		t.Errorf("mi300x-0 left the snapshot and still has series %v", got)
	}
	back := c.snap(10, 0.5, 0.5)
	set.Observe(back)
	var gpu0 *emulate.ComponentReading
	for i := range back.Nodes[0].Components {
		if comp := &back.Nodes[0].Components[i]; comp.ID == "gpu0" {
			gpu0 = comp
		}
	}
	if gpu0 == nil {
		t.Fatal("mi300x-0 has no gpu0")
	}
	compatNear(t, "mi300x-0 gpu0 energy uJ when back",
		compatOne(t, compatGather(t, set, AMDDME152), amdEnergyConsumed, map[string]string{amdLabelHostname: "mi300x-0", amdLabelGPUID: "0"}),
		math.Round(gpu0.EnergyJ*1e6))
}

// CP09: an emulated MI300X node, stepped by hwemu from the builtin profile
// next to an NVL node, appears in amd-dme-1.5.2 under its own node name,
// every GPU with its index, power, activity and growing ledger energy, while
// dcgm-4.8.4 serves only the NVL node's GPUs and amd-dme-1.5.2 none of them.
func TestCP09AMDEmulatedMI300XNodeCarriesItsNodeName(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var members []clusterspec.Member
	for _, m := range []struct{ name, profile string }{
		{"mi300x-a", "amd-instinct-mi300x-8gpu"},
		{"nvl-a", "nvidia-h100-nvl-8gpu"},
	} {
		p := profiles[m.profile]
		if p == nil {
			t.Fatalf("no %s profile", m.profile)
		}
		gpu := 0.5
		members = append(members, clusterspec.Member{Name: m.name, Pool: m.profile, Profile: p, BaseLoad: clusterspec.Load{GPUUtil: &gpu}})
	}
	e, err := emulate.New(emulate.Config{
		Root: filepath.Join(t.TempDir(), "hwemu"), Cluster: "compat-test", Members: members,
		Start: compatStart, Substep: time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	set, err := NewSet([]string{DCGM484, AMDDME152}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var snap *emulate.Snapshot
	var prev []compatSample
	for i := 1; i <= 2; i++ {
		if snap, err = e.Tick(compatStart.Add(time.Duration(i)*time.Second), nil, true, nil); err != nil {
			t.Fatal(err)
		}
		set.Observe(snap)
		got := compatGather(t, set, AMDDME152)
		for _, s := range compatFind(prev, amdEnergyConsumed, nil) {
			if now := compatOne(t, got, amdEnergyConsumed, s.labels); !(now > s.value) {
				t.Errorf("tick %d: %v energy %g uJ, was %g", i, s.labels, now, s.value)
			}
		}
		prev = got
	}

	for _, n := range snap.Nodes {
		amdGPUs := 0
		for _, c := range n.Components {
			if c.Type != semconv.HwTypeGPU || !compatIsAMD(&c) {
				continue
			}
			amdGPUs++
			match := map[string]string{amdLabelHostname: n.Name, amdLabelGPUID: strconv.Itoa(c.Index)}
			if got := compatFind(prev, amdEnergyConsumed, match); len(got) == 1 {
				if want := amdWantLabels(n.Name, c.Index, "AMD Instinct MI300X"); fmt.Sprint(got[0].labels) != fmt.Sprint(want) {
					t.Errorf("%s gpu%d labels %v, want %v", n.Name, c.Index, got[0].labels, want)
				}
			}
			compatNear(t, n.Name+" "+c.ID+" energy uJ", compatOne(t, prev, amdEnergyConsumed, match), math.Round(c.EnergyJ*1e6))
			compatNear(t, n.Name+" "+c.ID+" package power W", compatOne(t, prev, amdPackagePower, match), math.Round(c.PowerW))
			compatNear(t, n.Name+" "+c.ID+" gfx activity %", compatOne(t, prev, amdGFXActivity, match), math.Round(c.Utilization*100))
		}
		wantGPUs := map[string]int{"mi300x-a": 8, "nvl-a": 0}[n.Name]
		if amdGPUs != wantGPUs {
			t.Errorf("%s has %d AMD GPUs in the snapshot, want %d", n.Name, amdGPUs, wantGPUs)
		}
		for _, family := range amdFamilies {
			if got := len(compatFind(prev, family, map[string]string{amdLabelHostname: n.Name})); got != wantGPUs {
				t.Errorf("%s %s: %d series, want %d", n.Name, family, got, wantGPUs)
			}
		}
	}
	dcgmByNode := map[string]int{}
	for _, s := range compatFind(compatGather(t, set, DCGM484), dcgmTotalEnergy, nil) {
		dcgmByNode[s.labels[dcgmLabelHostname]]++
	}
	if fmt.Sprint(dcgmByNode) != fmt.Sprint(map[string]int{"nvl-a": 8}) {
		t.Errorf("dcgm-4.8.4 energy series by node %v, want nvl-a 8 only", dcgmByNode)
	}
}
