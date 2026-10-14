package hwemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// renderCorpusMachine is the part of a capture's machine.yaml FromCorpus
// reads. The corpus validator in cmd/agent checks the file strictly.
type renderCorpusMachine struct {
	Description string `json:"description"`
	Allocatable struct {
		CPU    string            `json:"cpu"`
		Memory string            `json:"memory"`
		GPU    map[string]string `json:"gpu"`
	} `json:"allocatable"`
	WriteRejectingZones []string `json:"writeRejectingZones"`
}

// renderCorpusSource is the source id every fact of a corpus profile cites.
const renderCorpusSource = "corpus"

// FromCorpus builds a profile from one machine of the hardware corpus
// (cmd/agent/testdata/hardware/<machine>), so the rendered tree replays the
// capture through the same paths a real node has:
//
//   - the flat powercap/intel-rapl:N[:M] capture becomes the nested tree with
//     its class links. Every captured file is kept, as extraFiles, and the
//     captured energy_uj is the start value. A zone in writeRejectingZones
//     gets a directory for constraint_0_power_limit_uw;
//   - cpuinfo is used verbatim, and node-labels.json becomes label group
//     corpus;
//   - machine.yaml allocatable becomes capacity, with no reservation, as the
//     corpus test builds its Node;
//   - cpufreq-driver and the governor state under governor/ become cpufreq
//     with the captured governor, preference, boost, frequency range and
//     driver mode (renderCorpusCPUFreq); cpufreq-driver alone, a capture from
//     before the script copied governor state, stays a minimal cpufreq
//     (scaling_driver only);
//   - nvidia-smi.txt becomes NVML state with the captured limits, product
//     and power draw, plus dev/nvidiactl. All GPUs must report the same
//     limits and product; the draw of GPU 0 stands for all of them;
//   - amdgpu-hwmon/gpuN/ becomes amdgpu hwmon devices with the captured cap,
//     minimum, maximum and default (renderCorpusAMDGPU). All GPUs must
//     report the same values.
//
// A directory whose name starts with _ is not a machine. Choosing the corpus
// root, JOULIE_HARDWARE_CORPUS included, is the caller's job.
func FromCorpus(machineDir string) (*Profile, error) {
	name := filepath.Base(filepath.Clean(machineDir))
	if strings.HasPrefix(name, "_") {
		return nil, fmt.Errorf("corpus %s: a directory starting with _ is not a machine", machineDir)
	}
	read := func(file string) (string, bool, error) {
		b, err := os.ReadFile(filepath.Join(machineDir, file))
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return string(b), err == nil, err
	}
	fail := func(err error) (*Profile, error) { return nil, fmt.Errorf("corpus %s: %w", name, err) }

	metaText, ok, err := read("machine.yaml")
	if err != nil || !ok {
		return fail(fmt.Errorf("machine.yaml: %v", renderMissing(err)))
	}
	var meta renderCorpusMachine
	if err := yaml.Unmarshal([]byte(metaText), &meta); err != nil {
		return fail(fmt.Errorf("machine.yaml: %w", err))
	}
	cpuinfo, ok, err := read("cpuinfo")
	if err != nil || !ok {
		return fail(fmt.Errorf("cpuinfo: %v", renderMissing(err)))
	}
	labelsText, ok, err := read("node-labels.json")
	if err != nil || !ok {
		return fail(fmt.Errorf("node-labels.json: %v", renderMissing(err)))
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(labelsText), &labels); err != nil {
		return fail(fmt.Errorf("node-labels.json: %w", err))
	}

	src := func(file string) string { return renderCorpusSource + ":" + renderCorpusSource + "#" + file }
	p := &Profile{
		SchemaVersion: 1,
		Name:          name,
		Description:   strings.TrimSpace(meta.Description),
		Sources: map[string]Source{renderCorpusSource: {
			Kind: renderKindCorpus,
			Ref:  filepath.ToSlash(machineDir),
			Note: "hardware corpus capture, mapped by FromCorpus",
		}},
	}

	// Node: the captured labels, and allocatable as capacity.
	p.Node.NFD = "absent"
	for k := range labels {
		if strings.HasPrefix(k, renderNFDPrefix) {
			p.Node.NFD = "present"
		}
	}
	if len(labels) > 0 {
		p.Node.Labels = map[string]LabelGroup{"corpus": {Src: src("node-labels.json"), Values: labels}}
	}
	cpuQ, err := resource.ParseQuantity(meta.Allocatable.CPU)
	if err != nil {
		return fail(fmt.Errorf("machine.yaml allocatable.cpu %q: %w", meta.Allocatable.CPU, err))
	}
	memQ, err := resource.ParseQuantity(meta.Allocatable.Memory)
	if err != nil {
		return fail(fmt.Errorf("machine.yaml allocatable.memory %q: %w", meta.Allocatable.Memory, err))
	}
	p.Node.Capacity.CPU = Fact[int64]{V: cpuQ.Value(), Src: src("machine.yaml allocatable.cpu")}
	p.Node.Capacity.MemoryKi = Fact[int64]{V: memQ.Value() / 1024, Src: src("machine.yaml allocatable.memory")}

	if err := renderCorpusCPU(p, cpuinfo, src("cpuinfo")); err != nil {
		return fail(err)
	}
	pc, maxPkgW, err := renderCorpusPowercap(filepath.Join(machineDir, "powercap"), meta.WriteRejectingZones, src)
	if err != nil {
		return fail(err)
	}
	p.Powercap = pc
	p.Physics.CPU.Model = "analytic"
	p.Physics.CPU.MaxPkgW = maxPkgW

	if driver, ok, err := read("cpufreq-driver"); err != nil {
		return fail(err)
	} else if err := renderCorpusCPUFreq(p, machineDir, driver, ok, src); err != nil {
		return fail(err)
	}

	nvidia, hasNvidia, err := read("nvidia-smi.txt")
	if err != nil {
		return fail(err)
	}
	amdgpu, err := renderCorpusAMDGPUDevices(machineDir)
	if err != nil {
		return fail(err)
	}
	switch {
	case hasNvidia && len(amdgpu) > 0:
		return fail(errors.New("both nvidia-smi.txt and " + renderCorpusAMDGPUDir + "/; a profile has one GPU vendor"))
	case hasNvidia:
		err = renderCorpusNvidia(p, nvidia, labels, meta.Allocatable.GPU, src)
	case len(amdgpu) > 0:
		err = renderCorpusAMDGPU(p, amdgpu, labels, meta.Allocatable.GPU, src)
	}
	if err != nil {
		return fail(err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func renderMissing(err error) error {
	if err == nil {
		return errors.New("missing")
	}
	return err
}

// renderCorpusCPU fills cpu from the first cpuinfo block and the count of
// distinct physical ids, and keeps the file verbatim.
func renderCorpusCPU(p *Profile, cpuinfo, src string) error {
	blocks := strings.Split(strings.TrimSpace(cpuinfo), "\n\n")
	first := renderCPUInfoFields(blocks[0])
	atoi := func(key string, def int) (int, error) {
		v, ok := first[key]
		if !ok {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("cpuinfo %s %q: %w", key, v, err)
		}
		return n, nil
	}
	family, err := atoi("cpu family", 0)
	if err != nil {
		return err
	}
	model, err := atoi("model", 0)
	if err != nil {
		return err
	}
	stepping, err := atoi("stepping", 0)
	if err != nil {
		return err
	}
	cores, err := atoi("cpu cores", 1)
	if err != nil {
		return err
	}
	siblings, err := atoi("siblings", cores)
	if err != nil {
		return err
	}
	mhz, _ := strconv.ParseFloat(first["cpu MHz"], 64)
	cpus := renderCPUsFromCPUInfo(cpuinfo)
	sockets := map[int]bool{}
	for _, c := range cpus {
		sockets[c.Socket] = true
	}
	threads := 1
	if cores > 0 && siblings >= cores {
		threads = siblings / cores
	}
	flags := first["flags"]

	c := &p.CPU
	c.VendorID = Fact[string]{V: first["vendor_id"], Src: src}
	c.ModelName = Fact[string]{V: first["model name"], Src: src}
	c.Family = Fact[int]{V: family, Src: src}
	c.Model = Fact[int]{V: model, Src: src}
	c.Stepping = Fact[int]{V: stepping, Src: src}
	c.Sockets = Fact[int]{V: len(sockets), Src: src}
	c.CoresPerSocket = Fact[int]{V: cores, Src: src}
	c.ThreadsPerCore = Fact[int]{V: threads, Src: src}
	c.CPUMHz = Fact[float64]{V: mhz, Src: src}
	c.Flags = flags
	c.Hypervisor = renderContains(strings.Fields(flags), "hypervisor")
	c.OneSocketPerVCPU = len(sockets) > 1 && len(sockets) == len(cpus) && cores == 1 && threads == 1
	c.CPUInfoVerbatim = cpuinfo
	return nil
}

// renderCorpusPowercap maps the flat capture to control types and zones. The
// zones of a control type must be numbered 0..N-1, as the kernel numbers
// them, because Render indexes zones by position. It also returns the
// package power from the first package zone's constraint_0_max_power_uw.
func renderCorpusPowercap(dir string, rejecting []string, src func(string) string) (*PowercapSpec, Fact[float64], error) {
	var maxPkgW Fact[float64]
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if len(rejecting) > 0 {
			return nil, maxPkgW, errors.New("machine.yaml lists writeRejectingZones but there is no powercap tree")
		}
		return nil, maxPkgW, nil
	}
	if err != nil {
		return nil, maxPkgW, err
	}
	type zoneDir struct {
		ct  string
		idx []int
	}
	byCT := map[string][]zoneDir{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		parts := strings.Split(e.Name(), ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, maxPkgW, fmt.Errorf("powercap/%s is not <control type>:<zone>[:<subzone>]", e.Name())
		}
		zd := zoneDir{ct: parts[0]}
		for _, s := range parts[1:] {
			n, err := strconv.ParseInt(s, 16, 32)
			if err != nil {
				return nil, maxPkgW, fmt.Errorf("powercap/%s: zone index %q is not hex", e.Name(), s)
			}
			zd.idx = append(zd.idx, int(n))
		}
		byCT[zd.ct] = append(byCT[zd.ct], zd)
	}
	rejects := map[string]bool{}
	for _, z := range rejecting {
		rejects[z] = true
	}
	used := map[string]bool{}

	readZone := func(ct string, idx []int, top bool) (ZoneSpec, error) {
		key := ct
		for _, i := range idx {
			key += ":" + strconv.FormatInt(int64(i), 16)
		}
		used[key] = true
		zsrc := func(file string) string { return src("powercap/" + key + "/" + file) }
		files := map[string]string{}
		ents, err := os.ReadDir(filepath.Join(dir, key))
		if err != nil {
			return ZoneSpec{}, err
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, key, e.Name()))
			if err != nil {
				return ZoneSpec{}, err
			}
			files[e.Name()] = string(b)
		}
		name, ok := files["name"]
		if !ok {
			return ZoneSpec{}, fmt.Errorf("powercap/%s has no name file", key)
		}
		trim := func(f string) string { return strings.TrimSpace(files[f]) }
		num := func(f string) (int64, bool, error) {
			if _, ok := files[f]; !ok {
				return 0, false, nil
			}
			n, err := strconv.ParseInt(trim(f), 10, 64)
			if err != nil {
				return 0, false, fmt.Errorf("powercap/%s/%s: %w", key, f, err)
			}
			return n, true, nil
		}
		z := ZoneSpec{NameFormat: strings.ReplaceAll(strings.TrimSpace(name), "%", "%%"), ExtraFiles: files}
		if top {
			z.Repeat = "once"
		}
		if v, ok, err := num("enabled"); err != nil {
			return z, err
		} else if ok {
			z.Enabled = Fact[int]{V: int(v), Src: zsrc("enabled")}
		}
		if v, ok, err := num("max_energy_range_uj"); err != nil {
			return z, err
		} else if ok {
			z.MaxEnergyRangeUJ = &Fact[int64]{V: v, Src: zsrc("max_energy_range_uj")}
			// The unit that gives this range by the kernel's formula, when
			// there is one (intel_rapl_common.c:741).
			const span = 1<<32 - 1
			if u := (v*1000 + span - 1) / span; MaxEnergyRangeUJ(u) == v {
				z.EnergyUnitNJ = Fact[int64]{V: u, Src: "derived:" + renderCorpusSource + "#powercap/" + key + "/max_energy_range_uj by floor((2^32-1)*unit/1000)"}
			}
		}
		for i := 0; ; i++ {
			prefix := fmt.Sprintf("constraint_%d_", i)
			cname, ok := files[prefix+"name"]
			if !ok {
				break
			}
			c := ConstraintSpec{Name: strings.TrimSpace(cname)}
			for _, f := range []struct {
				file string
				dst  *Fact[int64]
			}{{prefix + "power_limit_uw", &c.PowerLimitUW}, {prefix + "max_power_uw", &c.MaxPowerUW}, {prefix + "time_window_us", &c.TimeWindowUS}} {
				if v, ok, err := num(f.file); err != nil {
					return z, err
				} else if ok {
					*f.dst = Fact[int64]{V: v, Src: zsrc(f.file)}
				}
			}
			if i == 0 && rejects[key] {
				c.RejectWrites = Fact[string]{V: "disabled", Src: src("machine.yaml writeRejectingZones")}
				delete(files, prefix+"power_limit_uw")
			}
			z.Constraints = append(z.Constraints, c)
		}
		if len(z.Constraints) > 0 {
			z.PowerUnitUW = Fact[int64]{V: 125000, Src: "assumed:PU=3 as in intel-xeon-2s-rapl; a capture does not record the power unit"}
		}
		return z, nil
	}

	pc := &PowercapSpec{}
	cts := make([]string, 0, len(byCT))
	for ct := range byCT {
		cts = append(cts, ct)
	}
	sort.Strings(cts)
	for _, ct := range cts {
		zones := byCT[ct]
		var tops []int
		children := map[int][]int{}
		for _, zd := range zones {
			if len(zd.idx) == 1 {
				tops = append(tops, zd.idx[0])
			} else {
				children[zd.idx[0]] = append(children[zd.idx[0]], zd.idx[1])
			}
		}
		sort.Ints(tops)
		spec := ControlTypeSpec{Name: ct}
		for want, i := range tops {
			if i != want {
				return nil, maxPkgW, fmt.Errorf("powercap: %s zones are not numbered 0..%d", ct, len(tops)-1)
			}
			z, err := readZone(ct, []int{i}, true)
			if err != nil {
				return nil, maxPkgW, err
			}
			kids := children[i]
			sort.Ints(kids)
			for wantKid, j := range kids {
				if j != wantKid {
					return nil, maxPkgW, fmt.Errorf("powercap: %s:%x sub-zones are not numbered 0..%d", ct, i, len(kids)-1)
				}
				c, err := readZone(ct, []int{i, j}, false)
				if err != nil {
					return nil, maxPkgW, err
				}
				z.Children = append(z.Children, c)
			}
			delete(children, i)
			if maxPkgW.Src == "" && strings.HasPrefix(z.NameFormat, "package-") && len(z.Constraints) > 0 && z.Constraints[0].MaxPowerUW.Src != "" {
				maxPkgW = Fact[float64]{V: float64(z.Constraints[0].MaxPowerUW.V) / 1e6, Src: "derived:" + renderCorpusSource + "#powercap/" + ct + ":" + strconv.FormatInt(int64(i), 16) + "/constraint_0_max_power_uw"}
			}
			spec.Zones = append(spec.Zones, z)
		}
		if len(children) > 0 {
			return nil, maxPkgW, fmt.Errorf("powercap: %s has sub-zones without a parent zone", ct)
		}
		pc.ControlTypes = append(pc.ControlTypes, spec)
	}
	for _, z := range rejecting {
		if !used[z] {
			return nil, maxPkgW, fmt.Errorf("machine.yaml writeRejectingZones names %q, which the powercap tree does not have", z)
		}
	}
	if len(pc.ControlTypes) == 0 {
		return nil, maxPkgW, nil
	}
	return pc, maxPkgW, nil
}

// renderCorpusGPUPCI is the GPU's PCI identity as far as the capture shows
// it: the vendor, and the class when an NFD label names one, so the profile
// generates no label the capture lacks.
func renderCorpusGPUPCI(labels map[string]string, vendor4 string, src func(string) string) PCIDevice {
	d := PCIDevice{Vendor: Fact[string]{V: "0x" + vendor4, Src: "assumed:PCI vendor id of the GPU vendor named by the capture's tool output"}}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rest, ok := strings.CutPrefix(k, renderNFDPrefix+"pci-")
		if !ok {
			continue
		}
		class4, vendor, ok := strings.Cut(strings.TrimSuffix(rest, ".present"), "_")
		if ok && vendor == vendor4 && len(class4) == 4 {
			d.Class = Fact[string]{V: "0x" + class4 + "00", Src: src("node-labels.json " + k + "; prog-if byte assumed 00")}
			break
		}
	}
	return d
}

// renderCorpusExposure maps machine.yaml allocatable.gpu to an exposure: the
// vendor's resource with one per GPU is whole; no GPU resource at all is
// dra, the mode that advertises none.
func renderCorpusExposure(gpu map[string]string, resourceName string, count int, src func(string) string) (Fact[string], error) {
	if len(gpu) == 0 {
		return Fact[string]{V: "dra", Src: "assumed:allocatable.gpu is empty, and dra is the exposure that advertises no GPU resource"}, nil
	}
	if q, ok := gpu[resourceName]; ok && len(gpu) == 1 {
		if n, err := resource.ParseQuantity(q); err == nil && n.Value() == int64(count) {
			return Fact[string]{V: "whole", Src: src("machine.yaml allocatable.gpu")}, nil
		}
	}
	return Fact[string]{}, fmt.Errorf("machine.yaml allocatable.gpu %v does not match %d whole GPUs as %s", gpu, count, resourceName)
}

// renderCorpusNvidia maps the agent's nvidia-smi query output, one line per
// GPU: index, min limit, max limit, limit, draw, name (listNvidiaDevices in
// cmd/agent).
func renderCorpusNvidia(p *Profile, text string, labels, gpu map[string]string, src func(string) string) error {
	type line struct {
		min, max, limit, draw float64
		name                  string
	}
	var lines []line
	for _, raw := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.Split(raw, ",")
		if len(parts) < 6 {
			return fmt.Errorf("nvidia-smi.txt line %q has fewer than 6 fields", raw)
		}
		var l line
		for i, dst := range []*float64{&l.min, &l.max, &l.limit, &l.draw} {
			v, err := strconv.ParseFloat(strings.TrimSpace(parts[i+1]), 64)
			if err != nil {
				return fmt.Errorf("nvidia-smi.txt line %q field %d: %w", raw, i+1, err)
			}
			*dst = v
		}
		l.name = strings.TrimSpace(strings.Join(parts[5:], ","))
		lines = append(lines, l)
	}
	first := lines[0]
	for _, l := range lines[1:] {
		if l.min != first.min || l.max != first.max || l.limit != first.limit || l.name != first.name {
			return errors.New("nvidia-smi.txt: GPUs differ in limits or product; a profile holds one value for all GPUs")
		}
	}
	exposure, err := renderCorpusExposure(gpu, "nvidia.com/gpu", len(lines), src)
	if err != nil {
		return err
	}
	file := src("nvidia-smi.txt")
	g := &GPUSpec{
		Vendor:   "nvidia",
		Count:    Fact[int]{V: len(lines), Src: file},
		Product:  Fact[string]{V: first.name, Src: file},
		PCI:      renderCorpusGPUPCI(labels, "10de", src),
		Exposure: exposure,
		Labeller: Fact[string]{V: "listed", Src: src("node-labels.json")},
	}
	g.Limits.MinW = Fact[float64]{V: first.min, Src: file}
	g.Limits.MaxW = Fact[float64]{V: first.max, Src: file}
	g.Limits.DefaultW = Fact[float64]{V: first.limit, Src: "assumed:the capture has no default limit; the current power.limit stands for it"}
	g.Tools = append(g.Tools, GPUTool{Name: "nvidia-smi", Variant: "default"})
	p.GPUs = g
	p.Physics.GPU.IdleW = Fact[float64]{V: first.draw, Src: src("nvidia-smi.txt power.draw of GPU 0")}
	return nil
}

// renderCorpusAMDGPUDir holds the power cap attributes of the amdgpu hwmon
// device of every AMD GPU of a capture, as scripts/collect-hardware-fixture.sh
// copies them: gpuN/ for the Nth amdgpu device in bus id order, with
// power1_cap, power1_cap_min, power1_cap_max and power1_cap_default in
// microwatts, each one present when it read on the machine.
const renderCorpusAMDGPUDir = "amdgpu-hwmon"

// renderCorpusAMDGPUFiles are the attributes of one device the capture holds.
var renderCorpusAMDGPUFiles = []string{"power1_cap", "power1_cap_min", "power1_cap_max", "power1_cap_default"}

// renderCorpusAMDGPUDevices reads the attributes of every captured amdgpu
// device, by file name and without the newline; nil when the capture has no
// amdgpu-hwmon/. The devices must be gpu0 to gpuN-1.
func renderCorpusAMDGPUDevices(machineDir string) ([]map[string]string, error) {
	root := filepath.Join(machineDir, renderCorpusAMDGPUDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var devs []map[string]string
	for i := range entries {
		dir := filepath.Join(root, fmt.Sprintf("gpu%d", i))
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s: want gpu0 to gpu%d, one directory per device and nothing else", renderCorpusAMDGPUDir, len(entries)-1)
		}
		attrs := map[string]string{}
		for _, f := range renderCorpusAMDGPUFiles {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			attrs[f] = strings.TrimSpace(string(b))
		}
		devs = append(devs, attrs)
	}
	return devs, nil
}

// renderCorpusAMDGPU maps the captured amdgpu hwmon attributes to AMD GPUs
// whose hwmon devices hold the same values, so the agent reads the same cap
// range from the emulated node as from the capture. power1_cap_default is
// the limit the GPUs start at; the maximum stands for a missing or empty
// one. The capture names no product; the labeller's label does when the node
// has one. A device without a cap of its own (power1_cap absent or
// 0) is not modelled.
func renderCorpusAMDGPU(p *Profile, devs []map[string]string, labels, gpu map[string]string, src func(string) string) error {
	first := devs[0]
	for i, d := range devs[1:] {
		for _, f := range renderCorpusAMDGPUFiles {
			if d[f] != first[f] {
				return fmt.Errorf("%s: gpu%d and gpu0 differ in %s; a profile holds one value for all GPUs", renderCorpusAMDGPUDir, i+1, f)
			}
		}
	}
	watts := func(f string) (float64, bool, error) {
		v, ok := first[f]
		if !ok || v == "" {
			return 0, false, nil
		}
		uw, err := strconv.ParseInt(v, 10, 64)
		if err != nil || uw < 0 {
			return 0, false, fmt.Errorf("%s/gpu0/%s %q is not a number of microwatts", renderCorpusAMDGPUDir, f, v)
		}
		return float64(uw) / 1e6, true, nil
	}
	var w [4]float64
	var has [4]bool
	for i, f := range renderCorpusAMDGPUFiles {
		var err error
		if w[i], has[i], err = watts(f); err != nil {
			return err
		}
	}
	capW, minW, maxW, defW := w[0], w[1], w[2], w[3]
	if capW <= 0 || maxW <= 0 {
		return fmt.Errorf("%s/gpu0 has no power1_cap above 0 W and power1_cap_max: hwemu models only a device with a cap of its own", renderCorpusAMDGPUDir)
	}
	exposure, err := renderCorpusExposure(gpu, "amd.com/gpu", len(devs), src)
	if err != nil {
		return err
	}
	file := func(f string) string { return src(renderCorpusAMDGPUDir + "/gpu0/" + f) }
	g := &GPUSpec{
		Vendor:      "amd",
		Count:       Fact[int]{V: len(devs), Src: src(renderCorpusAMDGPUDir)},
		PCI:         renderCorpusGPUPCI(labels, "1002", src),
		Exposure:    exposure,
		Labeller:    Fact[string]{V: "listed", Src: src("node-labels.json")},
		PowerSensor: Fact[string]{V: "input", Src: "assumed:the capture holds no power reading, and the agent reads none"},
		Product:     Fact[string]{V: "AMD GPU", Src: "assumed:the capture names no product"},
	}
	for _, k := range []string{"amd.com/gpu.product-name", "beta.amd.com/gpu.product-name", "amd.com/gpu.product"} {
		if v := labels[k]; v != "" {
			g.Product = Fact[string]{V: v, Src: src("node-labels.json " + k)}
			break
		}
	}
	g.Limits.MinW = Fact[float64]{V: minW, Src: file("power1_cap_min")}
	if !has[1] {
		g.Limits.MinW.Src = "assumed:the capture has no power1_cap_min, which the agent reads as 0 W"
	}
	g.Limits.MaxW = Fact[float64]{V: maxW, Src: file("power1_cap_max")}
	g.Limits.DefaultW = Fact[float64]{V: defW, Src: file("power1_cap_default")}
	if defW <= 0 {
		g.Limits.DefaultW = Fact[float64]{V: maxW, Src: "assumed:the capture has no power1_cap_default; the maximum stands for it"}
	}
	if capW != g.Limits.DefaultW.V {
		g.Limits.CurrentW = Fact[float64]{V: capW, Src: file("power1_cap")}
	}
	p.GPUs = g
	return nil
}

// renderCorpusGovernorDir holds the governor state of a capture, as
// scripts/collect-hardware-fixture.sh copies it: cpufreq/policyN/,
// cpufreq/boost, intel_pstate/, amd_pstate/, cpuidle/stateN/name, amdgpu/gpuN/
// and nvidia-smi-clocks.txt.
const renderCorpusGovernorDir = "governor"

// renderCorpusPolicyFiles are the attributes of one policy the capture holds,
// each of them a fact of CPUFreqSpec.
var renderCorpusPolicyFiles = []string{
	"scaling_governor", "scaling_available_governors",
	"energy_performance_preference", "energy_performance_available_preferences",
	"boost", "cpuinfo_min_freq", "cpuinfo_max_freq",
}

// renderCorpusCPUFreq sets p.CPUFreq from the scaling driver (hasDriver says
// cpufreq-driver exists) and the policies under governor/cpufreq. Without
// policies the cpufreq is minimal, the scaling driver only, as a capture
// without governor state allows. With them it carries the governor, the
// available governors, the frequency range and the energy-performance
// preference of the policies, the driver mode, HWP from the cpuinfo flags on
// intel_pstate, and the boost state where the driver keeps it. A profile
// holds one state for all policies, so policies that differ are an error, as
// are captured policies the cpuinfo does not render per CPU or per core, and
// a capture that lacks a file the governor emulation renders (a driver's
// status, its boost file, the preference pair): the emulated node would
// differ from the capture in what the agent reads.
//
// cpuidle/, amdgpu/ and nvidia-smi-clocks.txt are not mapped: hwemu renders
// no cpuidle tree, and the amdgpu DPM and NVIDIA clock facts (the levels a
// family accepts, the pinned clocks, the lock behaviour) are more than a
// capture shows. They stay in the capture for review and calibration.
func renderCorpusCPUFreq(p *Profile, machineDir, driverText string, hasDriver bool, src func(string) string) error {
	govRoot := filepath.Join(machineDir, renderCorpusGovernorDir)
	policies := map[int]map[string]string{}
	entries, err := os.ReadDir(filepath.Join(govRoot, "cpufreq"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		id, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "policy"))
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "policy") || err != nil {
			continue
		}
		files := map[string]string{}
		for _, name := range renderCorpusPolicyFiles {
			b, err := os.ReadFile(filepath.Join(govRoot, "cpufreq", e.Name(), name))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			files[name] = string(b)
		}
		policies[id] = files
	}
	driver := strings.TrimSpace(driverText)
	if len(policies) == 0 {
		if hasDriver {
			p.CPUFreq = &CPUFreqSpec{
				Driver:         Fact[string]{V: driver, Src: src("cpufreq-driver")},
				PolicyGrouping: Fact[string]{V: "per-cpu", Src: "assumed:the capture records the scaling driver only"},
				Minimal:        true,
			}
		}
		return nil
	}
	if !hasDriver {
		return errors.New("governor/cpufreq has policies but there is no cpufreq-driver")
	}

	ids := make([]int, 0, len(policies))
	for id := range policies {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	first := policies[ids[0]]
	for _, id := range ids[1:] {
		for _, name := range renderCorpusPolicyFiles {
			if policies[id][name] != first[name] {
				return fmt.Errorf("governor/cpufreq/policy%d/%s is %q where policy%d has %q; a profile holds one state for all policies",
					id, name, policies[id][name], ids[0], first[name])
			}
		}
	}
	gsrc := func(file string) string { return src(renderCorpusGovernorDir + "/" + file) }
	psrc := func(name string) string { return gsrc("cpufreq/policy*/" + name) }
	word := func(name string) (string, error) {
		v, ok := first[name]
		if !ok || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("governor/cpufreq/policy%d has no %s", ids[0], name)
		}
		return strings.TrimSpace(v), nil
	}
	khz := func(name string) (Fact[int64], error) {
		v, err := word(name)
		if err != nil {
			return Fact[int64]{}, err
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Fact[int64]{}, fmt.Errorf("governor/cpufreq/policy%d/%s: %w", ids[0], name, err)
		}
		return Fact[int64]{V: n, Src: psrc(name)}, nil
	}
	gov, err := word("scaling_governor")
	if err != nil {
		return err
	}
	available, err := word("scaling_available_governors")
	if err != nil {
		return err
	}
	f := &CPUFreqSpec{
		Driver:             Fact[string]{V: driver, Src: src("cpufreq-driver")},
		Governor:           Fact[string]{V: gov, Src: psrc("scaling_governor")},
		AvailableGovernors: Fact[string]{V: strings.Join(strings.Fields(available), " "), Src: psrc("scaling_available_governors")},
	}
	if f.CPUInfoMinKHz, err = khz("cpuinfo_min_freq"); err != nil {
		return err
	}
	if f.CPUInfoMaxKHz, err = khz("cpuinfo_max_freq"); err != nil {
		return err
	}
	p.CPUFreq = f

	// The captured policies must be the ones Render lays out from the
	// cpuinfo, one per CPU or one per core.
	cpus := renderCPUs(p)
	for _, grouping := range []string{"per-cpu", "per-core"} {
		f.PolicyGrouping = Fact[string]{V: grouping, Src: "derived:" + renderCorpusSource + "#governor/cpufreq policy directories against the cpuinfo CPUs"}
		var rendered []int
		for _, pol := range renderPolicies(p, cpus) {
			rendered = append(rendered, pol.ID)
		}
		if slices.Equal(rendered, ids) {
			break
		}
		if grouping == "per-core" {
			return fmt.Errorf("governor/cpufreq has policies %v, which the cpuinfo renders neither per CPU nor per core", ids)
		}
	}

	readGov := func(file string) (string, bool, error) {
		b, err := os.ReadFile(filepath.Join(govRoot, filepath.FromSlash(file)))
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return strings.TrimSpace(string(b)), err == nil, err
	}
	// The driver mode, from the status file the governor emulation renders
	// for the drivers with modes.
	var status string
	switch {
	case cpuGovIntel(driver):
		status = "intel_pstate/status"
		f.HWP = Fact[bool]{V: slices.Contains(strings.Fields(p.CPU.Flags), "hwp"), Src: src("cpuinfo")}
	case driver == "amd-pstate" || driver == "amd-pstate-epp":
		status = "amd_pstate/status"
	}
	if status != "" {
		mode, ok, err := readGov(status)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s keeps its mode in %s, which governor/ lacks", driver, status)
		}
		f.DriverMode = Fact[string]{V: mode, Src: gsrc(status)}
	}

	// The preference pair. Under performance amd-pstate-epp offers no other
	// preference, so the capture shows the forced state rather than the
	// policy's own; the profile then keeps the default list, which renders
	// the same forced state.
	epp, hasEPP := first["energy_performance_preference"]
	prefs, hasPrefs := first["energy_performance_available_preferences"]
	if hasEPP != hasPrefs {
		return fmt.Errorf("governor/cpufreq/policy%d has one of the energy-performance preference pair only", ids[0])
	}
	if hasEPP && !(driver == "amd-pstate-epp" && gov == "performance") {
		f.EPP = &EPPSpec{
			Available: Fact[string]{V: strings.Join(strings.Fields(prefs), " "), Src: psrc("energy_performance_available_preferences")},
			Default:   Fact[string]{V: strings.TrimSpace(epp), Src: psrc("energy_performance_preference")},
		}
	}
	if !hasEPP && cpuGovHasEPP(f) {
		return fmt.Errorf("%s policies render the energy-performance preference pair, which governor/cpufreq/policy%d lacks", driver, ids[0])
	}

	// Boost, where the driver keeps it: the inverted no_turbo, each policy's
	// boost, or the global cpufreq/boost. The first two are rendered whenever
	// the governor emulation is on, so they must be captured.
	bit := func(v, file string) (int, error) {
		switch v {
		case "0":
			return 0, nil
		case "1":
			return 1, nil
		}
		return 0, fmt.Errorf("governor/%s is %q, want 0 or 1", file, v)
	}
	scope := cpuGovDriverBoostScope(driver)
	var boostFile, boostText string
	var hasBoost bool
	switch scope {
	case cpuGovBoostNoTurbo:
		boostFile = "intel_pstate/no_turbo"
		if boostText, hasBoost, err = readGov(boostFile); err != nil {
			return err
		}
	case cpuGovBoostPerCPU:
		boostFile = "cpufreq/policy*/boost"
		boostText, hasBoost = first["boost"]
		boostText = strings.TrimSpace(boostText)
	default:
		boostFile = "cpufreq/boost"
		if boostText, hasBoost, err = readGov(boostFile); err != nil {
			return err
		}
	}
	switch {
	case hasBoost:
		v, err := bit(boostText, boostFile)
		if err != nil {
			return err
		}
		if scope == cpuGovBoostNoTurbo {
			v = 1 - v
		}
		f.Boost = Fact[int]{V: v, Src: gsrc(boostFile)}
		f.BoostScope = Fact[string]{V: scope, Src: gsrc(boostFile)}
	case scope != cpuGovBoostGlobal:
		return fmt.Errorf("%s keeps boost in %s, which governor/ lacks", driver, boostFile)
	}
	return nil
}
