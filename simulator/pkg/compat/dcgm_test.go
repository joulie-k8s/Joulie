package compat

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// CP05: dcgm-4.8.4 serves power, total energy and utilisation per NVIDIA
// GPU with the labels gpu, UUID, pci_bus_id, device, modelName, the node
// name label and DCGM_FI_DRIVER_VERSION; energy is the device counter in mJ;
// the node name label is hostname, or Hostname with the option; AMD GPUs
// get nothing; and the exposition matches the golden.
func TestCP05DCGMFamiliesLabelsAndValues(t *testing.T) {
	set, snaps := compatRun(t, []string{DCGM484}, Options{}, compatNVL(), compatMI300X(), compatIntel())
	samples := compatGather(t, set, DCGM484)
	n := &snaps[len(snaps)-1].Nodes[0]

	for _, family := range []string{dcgmPowerUsage, dcgmTotalEnergy, dcgmGPUUtil} {
		if got := compatFind(samples, family, nil); len(got) != 8 {
			t.Errorf("%s: %d series, want 8 (the NVL GPUs only)", family, len(got))
		}
	}
	for i := range n.Components {
		c := &n.Components[i]
		if c.Type != semconv.HwTypeGPU {
			continue
		}
		match := map[string]string{dcgmLabelGPU: strconv.Itoa(c.Index)}
		for _, s := range compatFind(samples, dcgmTotalEnergy, match) {
			want := map[string]string{
				dcgmLabelGPU: strconv.Itoa(c.Index), dcgmLabelUUID: c.UUID, dcgmLabelPCIBusID: c.PCIBusID,
				dcgmLabelDevice: fmt.Sprintf("nvidia%d", c.Index), dcgmLabelModelName: "NVIDIA H100 NVL",
				dcgmLabelHostname: "nvl-0", dcgmLabelDriverVersion: "550.54.15",
			}
			if fmt.Sprint(s.labels) != fmt.Sprint(want) {
				t.Errorf("gpu%d labels %v, want %v", c.Index, s.labels, want)
			}
		}
		compatNear(t, c.ID+" energy mJ", compatOne(t, samples, dcgmTotalEnergy, match), math.Round(c.DeviceEnergyJ*1000))
		compatNear(t, c.ID+" power W", compatOne(t, samples, dcgmPowerUsage, match), c.PowerW)
		compatNear(t, c.ID+" utilisation %", compatOne(t, samples, dcgmGPUUtil, match), 100)
	}

	legacy, _ := compatRun(t, []string{DCGM484}, Options{DCGMHostnameLabel: dcgmLabelHostnameLegacy}, compatNVL())
	for _, s := range compatGather(t, legacy, DCGM484) {
		if s.labels[dcgmLabelHostnameLegacy] != "nvl-0" || s.labels[dcgmLabelHostname] != "" {
			t.Errorf("with Hostname: %s%v, want Hostname=nvl-0 and no hostname", s.family, s.labels)
		}
	}

	golden, _ := compatRun(t, []string{DCGM484}, Options{}, compatNVL())
	code, body := compatScrape(t, golden, Path(DCGM484))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path(DCGM484), code)
	}
	compatGolden(t, "dcgm-4.8.4.prom", body)
}

// CP05: a node keeps its series while its snapshot time does not advance,
// as the Kepler exporters do, so a repeated or earlier snapshot never takes
// the energy counter back, which Prometheus would read as a reset. A node
// that leaves the snapshot loses its series and comes back with its current
// ones.
func TestCP05DCGMRepeatedAndOutOfOrderSnapshotsChangeNothing(t *testing.T) {
	set, err := NewSet([]string{DCGM484}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := compatNewCluster(t, compatNVL(), compatNoRAPL())
	first := c.snap(0, 0.5, 0.5)
	late := c.snap(5, 0.5, 1)
	mid := c.snap(5, 0.5, 0.25)

	set.Observe(first)
	set.Observe(mid)
	before := compatGather(t, set, DCGM484)
	set.Observe(mid)
	compatSame(t, "a repeated snapshot", before, compatGather(t, set, DCGM484))
	set.Observe(late)
	compatSame(t, "a snapshot 5 s before the last one", before, compatGather(t, set, DCGM484))

	last := c.snap(10, 0.5, 0.5)
	set.Observe(last)
	got := compatGather(t, set, DCGM484)
	for _, n := range last.Nodes {
		for _, comp := range n.Components {
			if comp.Type != semconv.HwTypeGPU {
				continue
			}
			match := map[string]string{dcgmLabelHostname: n.Name, dcgmLabelGPU: strconv.Itoa(comp.Index)}
			compatNear(t, n.Name+" "+comp.ID+" energy mJ", compatOne(t, got, dcgmTotalEnergy, match), math.Round(comp.DeviceEnergyJ*1000))
			compatNear(t, n.Name+" "+comp.ID+" power W", compatOne(t, got, dcgmPowerUsage, match), comp.PowerW)
		}
	}

	gone := c.snap(10, 0.5, 0.5)
	gone.Nodes = gone.Nodes[1:]
	set.Observe(gone)
	if got := compatFind(compatGather(t, set, DCGM484), dcgmTotalEnergy, map[string]string{dcgmLabelHostname: "nvl-0"}); len(got) != 0 {
		t.Errorf("nvl-0 left the snapshot and still has series %v", got)
	}
	back := c.snap(10, 0.5, 0.5)
	set.Observe(back)
	compatNear(t, "nvl-0 gpu0 energy mJ when back", compatOne(t, compatGather(t, set, DCGM484), dcgmTotalEnergy, map[string]string{dcgmLabelHostname: "nvl-0", dcgmLabelGPU: "0"}),
		math.Round(back.Nodes[0].Components[4].DeviceEnergyJ*1000))
}

// CP05: GPU readings built from the NVML files of an NVL tree stepped by
// hwemu come out of DCGM as those files: the mJ rounding recovers
// total_energy_mj exactly, the utilisation is utilization_gpu_pct, and the
// UUID, pci_bus_id, model and driver labels pass through. The readings are
// built here from the files, so this checks the DCGM side alone; how a
// snapshot gets them from hwemu is the emulator's tests' concern.
func TestCP05DCGMMatchesTheNVMLFiles(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["nvidia-h100-nvl-8gpu"]
	if p == nil {
		t.Fatal("no nvidia-h100-nvl-8gpu profile")
	}
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "nvl-0"), hwemu.RenderOptions{NodeName: "nvl-0"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := hwemu.NewNode(p, tree, hwemu.Options{Start: compatStart, Substep: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	node.SetLoad(hwemu.Load{CPUUtil: []float64{0.4}, GPUUtil: []float64{0.25, 0.5, 0.75, 1, 0, 0.1, 0.9, 0.6}})
	if err := node.AdvanceTo(compatStart.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	stateDir := tree.StateRoot
	if stateDir == "" {
		stateDir = filepath.Join(tree.Root, layout.StateDir)
	}
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	n := emulate.NodeSnapshot{Name: "nvl-0", DriverVersion: read(layout.NVMLDir + "/driver_version")}
	files := map[int]map[string]string{}
	for i := 0; i < p.GPUs.Count.V; i++ {
		dir := layout.NVMLGPUDir(i)
		f := map[string]string{}
		for _, name := range []string{"name", "uuid", "pci_bus_id", "total_energy_mj", "power_draw_mw", "utilization_gpu_pct"} {
			f[name] = read(dir + "/" + name)
		}
		files[i] = f
		mj, err := strconv.ParseInt(f["total_energy_mj"], 10, 64)
		if err != nil || mj <= 0 {
			t.Fatalf("gpu%d total_energy_mj %q after 10 s at load", i, f["total_energy_mj"])
		}
		mw, _ := strconv.ParseFloat(f["power_draw_mw"], 64)
		pct, _ := strconv.ParseFloat(f["utilization_gpu_pct"], 64)
		n.Components = append(n.Components, emulate.ComponentReading{
			ID: fmt.Sprintf("gpu%d", i), Type: semconv.HwTypeGPU, Name: f["name"], Vendor: "NVIDIA", Model: f["name"],
			UUID: f["uuid"], PCIBusID: f["pci_bus_id"], Index: i, Measured: true,
			PowerW: mw / 1000, DeviceEnergyJ: float64(mj) / 1000, Utilization: pct / 100,
		})
	}
	set, err := NewSet([]string{DCGM484}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	set.Observe(&emulate.Snapshot{Seq: 1, Time: compatStart.Add(10 * time.Second), Nodes: []emulate.NodeSnapshot{n}})
	samples := compatGather(t, set, DCGM484)
	for i, f := range files {
		match := map[string]string{
			dcgmLabelGPU: strconv.Itoa(i), dcgmLabelUUID: f["uuid"], dcgmLabelPCIBusID: f["pci_bus_id"],
			dcgmLabelModelName: f["name"], dcgmLabelDriverVersion: n.DriverVersion,
		}
		if got := strconv.FormatFloat(compatOne(t, samples, dcgmTotalEnergy, match), 'f', -1, 64); got != f["total_energy_mj"] {
			t.Errorf("gpu%d DCGM energy %s mJ, total_energy_mj %s", i, got, f["total_energy_mj"])
		}
		if got, want := compatOne(t, samples, dcgmGPUUtil, match), f["utilization_gpu_pct"]; strconv.FormatFloat(got, 'f', -1, 64) != want {
			t.Errorf("gpu%d DCGM utilisation %g, utilization_gpu_pct %s", i, got, want)
		}
	}
}
