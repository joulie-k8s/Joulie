package hwemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
//   - cpufreq-driver becomes a minimal cpufreq (scaling_driver only);
//   - nvidia-smi.txt becomes NVML state with the captured limits, product
//     and power draw, plus dev/nvidiactl. All GPUs must report the same
//     limits and product; the draw of GPU 0 stands for all of them;
//   - rocm-smi.txt becomes amdgpu hwmon devices and the
//     legacy-agent-contract rocm-smi.
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
	} else if ok {
		p.CPUFreq = &CPUFreqSpec{
			Driver:         Fact[string]{V: strings.TrimSpace(driver), Src: src("cpufreq-driver")},
			PolicyGrouping: Fact[string]{V: "per-cpu", Src: "assumed:the capture records the scaling driver only"},
			Minimal:        true,
		}
	}

	nvidia, hasNvidia, err := read("nvidia-smi.txt")
	if err != nil {
		return fail(err)
	}
	rocm, hasRocm, err := read("rocm-smi.txt")
	if err != nil {
		return fail(err)
	}
	switch {
	case hasNvidia && hasRocm:
		return fail(errors.New("both nvidia-smi.txt and rocm-smi.txt; a profile has one GPU vendor"))
	case hasNvidia:
		err = renderCorpusNvidia(p, nvidia, labels, meta.Allocatable.GPU, src)
	case hasRocm:
		err = renderCorpusRocm(p, rocm, labels, meta.Allocatable.GPU, src)
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
	g.Tools = append(g.Tools, struct {
		Name    string `json:"name"`
		Variant string `json:"variant"`
	}{Name: "nvidia-smi", Variant: "default"})
	p.GPUs = g
	p.Physics.GPU.IdleW = Fact[float64]{V: first.draw, Src: src("nvidia-smi.txt power.draw of GPU 0")}
	return nil
}

// renderCorpusRocm maps the output of rocm-smi --showpowercap
// --showproductname --json, as the agent runs it (listAmdDevices in cmd/agent):
// card<i> keys with string values.
func renderCorpusRocm(p *Profile, text string, labels, gpu map[string]string, src func(string) string) error {
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return fmt.Errorf("rocm-smi.txt: %w", err)
	}
	var cards []map[string]any
	for i := 0; ; i++ {
		c, ok := raw[fmt.Sprintf("card%d", i)]
		if !ok {
			break
		}
		cards = append(cards, c)
	}
	if len(cards) == 0 || len(cards) != len(raw) {
		return errors.New("rocm-smi.txt: want keys card0..cardN-1")
	}
	str := func(c map[string]any, key string) string {
		if v, ok := c[key]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	watts := func(c map[string]any, key string) (float64, error) {
		v, err := strconv.ParseFloat(strings.TrimSuffix(str(c, key), "W"), 64)
		if err != nil {
			return 0, fmt.Errorf("rocm-smi.txt %s: %w", key, err)
		}
		return v, nil
	}
	const (
		maxKey = "Max Graphics Package Power (W)"
		minKey = "Min Graphics Package Power (W)"
		capKey = "Current Power Cap (W)"
		avgKey = "Average Graphics Package Power (W)"
	)
	first := cards[0]
	for _, c := range cards[1:] {
		for _, k := range []string{maxKey, minKey, capKey, "Card Series", "Card SKU"} {
			if str(c, k) != str(first, k) {
				return fmt.Errorf("rocm-smi.txt: cards differ in %q; a profile holds one value for all GPUs", k)
			}
		}
	}
	maxW, err := watts(first, maxKey)
	if err != nil {
		return err
	}
	minW, err := watts(first, minKey)
	if err != nil {
		return err
	}
	capW, err := watts(first, capKey)
	if err != nil {
		return err
	}
	exposure, err := renderCorpusExposure(gpu, "amd.com/gpu", len(cards), src)
	if err != nil {
		return err
	}
	file := src("rocm-smi.txt")
	g := &GPUSpec{
		Vendor:      "amd",
		Count:       Fact[int]{V: len(cards), Src: file},
		PCI:         renderCorpusGPUPCI(labels, "1002", src),
		Exposure:    exposure,
		Labeller:    Fact[string]{V: "listed", Src: src("node-labels.json")},
		PowerSensor: Fact[string]{V: "average", Src: "assumed:the agent reads Average Graphics Package Power"},
	}
	product := str(first, "Card Series")
	if product == "" {
		product = str(first, "Card SKU")
	}
	g.Product = Fact[string]{V: product, Src: file}
	if sku := str(first, "Card SKU"); sku != "" && !strings.Contains(sku, "-") {
		// rocm-smi prints the middle segment of a two-dash vbios_version as
		// Card SKU (rocm_smi.py showProduct 2756-2761).
		g.VBIOSVersion = Fact[string]{V: "113-" + sku + "-000", Src: "assumed:a vbios_version whose middle segment is the captured Card SKU"}
	}
	g.Limits.MinW = Fact[float64]{V: minW, Src: file}
	g.Limits.MaxW = Fact[float64]{V: maxW, Src: file}
	g.Limits.DefaultW = Fact[float64]{V: capW, Src: "assumed:the capture has no default cap; the current cap stands for it"}
	g.Tools = append(g.Tools, struct {
		Name    string `json:"name"`
		Variant string `json:"variant"`
	}{Name: "rocm-smi", Variant: "legacy-agent-contract"})
	p.GPUs = g
	if avg, err := watts(first, avgKey); err == nil {
		p.Physics.GPU.IdleW = Fact[float64]{V: avg, Src: src("rocm-smi.txt " + avgKey + " of card0")}
	}
	return nil
}
