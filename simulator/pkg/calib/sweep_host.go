package calib

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The host of the sweep collector: what it finds under the sys and proc
// roots (topology, RAPL and HSMP files, cpufreq policies), the files it
// writes and puts back, and the readings of one sample.

// sweepCounter is one energy_uj file and the energy it counted since the
// sweep started, wraps included.
type sweepCounter struct {
	file    string
	rangeUJ int64
	last    int64
	joules  float64
}

// sweepComponent is a CPU package or DRAM zone in the identity of the name
// table: its energy counters (a package zone, or its die zones when it has
// none), its HSMP power sensor, and its package power limit.
type sweepComponent struct {
	id, typ, name, parent string
	socket                int
	counters              []*sweepCounter
	powerFile             string // HSMP power1_input, uW
	limitFile             string // RAPL long_term power_limit_uw or HSMP power1_cap, uW
	enabledFile           string // the RAPL zone's enabled, "" for HSMP

	powerW      float64
	hasPower    bool
	limitW      float64
	hasLimit    bool
	requestedW  float64
	wroteLimits bool
}

// sweepPolicy is one cpufreq policy and its package.
type sweepPolicy struct {
	dir    string
	socket int
	maxKHz int64
	curKHz int64
	limKHz int64
	ok     bool
}

// sweepCPUTimes is a CPU's line of /proc/stat: its idle time and the sum
// of its times, in USER_HZ.
type sweepCPUTimes struct{ idle, total uint64 }

// sweepSaved is the content a file had before the sweep wrote it.
type sweepSaved struct {
	file    string
	content []byte
}

// sweepHost is the host as the collector found it, and its last readings.
type sweepHost struct {
	sys, proc     string
	cpus          []int
	socketOf      map[int]int
	vendor, model string
	comps         []*sweepComponent
	policies      []*sweepPolicy
	stat          map[int]sweepCPUTimes
	idle          map[int]float64
	saved         []sweepSaved
	savedFiles    map[string]bool
}

// sweepHSMPName is the name of an HSMP hwmon device (hsmp/hwmon.c).
const sweepHSMPName = "amd_hsmp_hwmon"

// sweepDiscover reads the topology, the energy and power files, the power
// limits and the cpufreq policies of the host, and fails when no CPU
// package can be measured.
func sweepDiscover(sys, proc string) (*sweepHost, error) {
	h := &sweepHost{sys: sys, proc: proc, socketOf: map[int]int{}, savedFiles: map[string]bool{}}
	stat, err := sweepReadStat(filepath.Join(proc, "stat"))
	if err != nil {
		return nil, err
	}
	for c := range stat {
		h.cpus = append(h.cpus, c)
	}
	sort.Ints(h.cpus)
	if len(h.cpus) == 0 {
		return nil, fmt.Errorf("%s lists no CPU", filepath.Join(proc, "stat"))
	}
	if err := h.discoverTopology(); err != nil {
		return nil, err
	}
	if err := h.discoverRAPL(); err != nil {
		return nil, err
	}
	if err := h.discoverHSMP(); err != nil {
		return nil, err
	}
	if err := h.discoverCPUFreq(); err != nil {
		return nil, err
	}
	measured := false
	for _, c := range h.comps {
		if c.typ == semconv.HwTypeCPU && (len(c.counters) > 0 || c.powerFile != "") {
			measured = true
		}
	}
	if !measured {
		return nil, fmt.Errorf("no CPU package energy (powercap intel-rapl package zones) or power (%s hwmon) under %s", sweepHSMPName, sys)
	}
	sort.Slice(h.comps, func(i, j int) bool {
		a, b := h.comps[i], h.comps[j]
		if a.typ != b.typ {
			return a.typ == semconv.HwTypeCPU
		}
		return a.socket < b.socket
	})
	return h, nil
}

// discoverTopology maps each logical CPU to its package, from
// topology/physical_package_id or else the physical id of /proc/cpuinfo,
// and reads the vendor and model of the CPUs from /proc/cpuinfo.
func (h *sweepHost) discoverTopology() error {
	info, err := os.ReadFile(filepath.Join(h.proc, "cpuinfo"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fromInfo := map[int]int{}
	cpu := -1
	for _, line := range strings.Split(string(info), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			cpu, _ = strconv.Atoi(v)
		case "physical id":
			if n, err := strconv.Atoi(v); err == nil && cpu >= 0 {
				fromInfo[cpu] = n
			}
		case "vendor_id":
			if h.vendor == "" {
				h.vendor = sweepVendor(v)
			}
		case "model name":
			if h.model == "" {
				h.model = v
			}
		}
	}
	for _, c := range h.cpus {
		f := filepath.Join(h.sys, "devices", "system", "cpu", "cpu"+strconv.Itoa(c), "topology", "physical_package_id")
		if n, err := sweepReadInt(f); err == nil {
			h.socketOf[c] = int(n)
		} else if s, ok := fromInfo[c]; ok {
			h.socketOf[c] = s
		} else {
			h.socketOf[c] = 0
		}
	}
	return nil
}

// sweepVendor is hw.vendor of a cpuinfo vendor_id, as the simulator names it.
func sweepVendor(vendorID string) string {
	switch vendorID {
	case "GenuineIntel":
		return "Intel"
	case "AuthenticAMD":
		return "AMD"
	}
	return vendorID
}

// component returns the component of typ on socket, adding it.
func (h *sweepHost) component(typ string, socket int) *sweepComponent {
	for _, c := range h.comps {
		if c.typ == typ && c.socket == socket {
			return c
		}
	}
	c := &sweepComponent{id: typ + strconv.Itoa(socket), typ: typ, socket: socket}
	switch typ {
	case semconv.HwTypeCPU:
		c.name = "package-" + strconv.Itoa(socket)
	case semconv.HwTypeMemory:
		c.name, c.parent = "dram", semconv.HwTypeCPU+strconv.Itoa(socket)
	}
	h.comps = append(h.comps, c)
	return c
}

// discoverRAPL reads the intel-rapl powercap zones (powercap.rst): a
// package-N zone is CPU package N, its die zones count for it only when it
// has no package zone, and a dram child is its DRAM. Core, uncore, psys and
// any other zone are never read: none is a package or DRAM (section 1.5 of
// the telemetry contract). The long_term constraint of a package zone is its
// power limit.
func (h *sweepHost) discoverRAPL() error {
	ct := filepath.Join(h.sys, "devices", "virtual", "powercap", "intel-rapl")
	tops, err := filepath.Glob(filepath.Join(ct, "intel-rapl:*"))
	if err != nil {
		return err
	}
	sort.Strings(tops)
	pkgZones, dieZones := map[int]string{}, map[int][]string{}
	for _, z := range tops {
		name, err := sweepReadString(filepath.Join(z, "name"))
		if err != nil {
			continue
		}
		s, die, ok := layout.RAPLPackageZone(name)
		switch {
		case !ok:
		case die < 0:
			if _, dup := pkgZones[s]; !dup {
				pkgZones[s] = z
			}
		default:
			dieZones[s] = append(dieZones[s], z)
		}
	}
	sockets := map[int]bool{}
	for s := range pkgZones {
		sockets[s] = true
	}
	for s := range dieZones {
		sockets[s] = true
	}
	for s := range sockets {
		zones := dieZones[s]
		if z, ok := pkgZones[s]; ok {
			zones = []string{z}
		}
		cpu := h.component(semconv.HwTypeCPU, s)
		for _, z := range zones {
			c, err := sweepOpenCounter(filepath.Join(z, "energy_uj"))
			if err != nil {
				return err
			}
			if c != nil {
				cpu.counters = append(cpu.counters, c)
			}
			children, err := filepath.Glob(filepath.Join(z, filepath.Base(z)+":*"))
			if err != nil {
				return err
			}
			sort.Strings(children)
			for _, ch := range children {
				if name, err := sweepReadString(filepath.Join(ch, "name")); err != nil || name != "dram" {
					continue
				}
				c, err := sweepOpenCounter(filepath.Join(ch, "energy_uj"))
				if err != nil {
					return err
				}
				if c != nil {
					mem := h.component(semconv.HwTypeMemory, s)
					mem.counters = append(mem.counters, c)
				}
			}
		}
		if z, ok := pkgZones[s]; ok {
			if f := sweepLongTerm(z); f != "" {
				cpu.limitFile, cpu.enabledFile = f, filepath.Join(z, "enabled")
			}
		}
	}
	return nil
}

// sweepLongTerm is the power_limit_uw file of the long_term constraint of
// zone z, or "" when it has none. A limit the firmware locked is a file
// like any other, and a step that writes it fails with the kernel's errno
// (ENODATA).
func sweepLongTerm(z string) string {
	names, _ := filepath.Glob(filepath.Join(z, "constraint_*_name"))
	sort.Strings(names)
	for _, n := range names {
		if v, err := sweepReadString(n); err != nil || v != "long_term" {
			continue
		}
		f := strings.TrimSuffix(n, "_name") + "_power_limit_uw"
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			return f
		}
	}
	return ""
}

// sweepOpenCounter reads energy_uj and its range once. A missing file is
// no counter; one that cannot be read for its mode needs root.
func sweepOpenCounter(file string) (*sweepCounter, error) {
	v, err := sweepReadInt(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("reading %s needs root: energy_uj is mode 0400 since Linux commit 949dd0104c49: %w", file, err)
	case err != nil:
		return nil, err
	}
	rng, _ := sweepReadInt(filepath.Join(filepath.Dir(file), "max_energy_range_uj"))
	return &sweepCounter{file: file, rangeUJ: rng, last: v}, nil
}

// discoverHSMP finds the HSMP hwmon devices, one per socket in the order of
// their hwmon numbers (hsmp/plat.c registers them socket by socket): the
// socket power in power1_input and the socket limit in power1_cap, in uW. A
// package with an HSMP limit takes it rather than RAPL's.
func (h *sweepHost) discoverHSMP() error {
	devs, err := filepath.Glob(filepath.Join(h.sys, "class", "hwmon", "hwmon*"))
	if err != nil {
		return err
	}
	num := func(p string) int {
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "hwmon"))
		return n
	}
	sort.Slice(devs, func(i, j int) bool { return num(devs[i]) < num(devs[j]) })
	socket := 0
	for _, d := range devs {
		if name, err := sweepReadString(filepath.Join(d, "name")); err != nil || name != sweepHSMPName {
			continue
		}
		cpu := h.component(semconv.HwTypeCPU, socket)
		socket++
		if f := filepath.Join(d, "power1_input"); sweepExists(f) {
			cpu.powerFile = f
		}
		if f := filepath.Join(d, "power1_cap"); sweepExists(f) {
			cpu.limitFile, cpu.enabledFile = f, ""
		}
	}
	return nil
}

// discoverCPUFreq finds the cpufreq policies and the package of each, the
// package of the first CPU it covers.
func (h *sweepHost) discoverCPUFreq() error {
	dirs, err := filepath.Glob(filepath.Join(h.sys, "devices", "system", "cpu", "cpufreq", "policy*"))
	if err != nil {
		return err
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		first := -1
		for _, f := range []string{"related_cpus", "affected_cpus"} {
			if v, err := sweepReadString(filepath.Join(d, f)); err == nil {
				if ids := strings.Fields(v); len(ids) > 0 {
					first, _ = strconv.Atoi(ids[0])
					break
				}
			}
		}
		if first < 0 {
			first, _ = strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "policy"))
		}
		maxKHz, err := sweepReadInt(filepath.Join(d, "cpuinfo_max_freq"))
		if err != nil {
			continue
		}
		h.policies = append(h.policies, &sweepPolicy{dir: d, socket: h.socketOf[first], maxKHz: maxKHz})
	}
	return nil
}

// checkKnobs fails before anything is written when a step sets a knob the
// host has no file for.
func (h *sweepHost) checkKnobs(steps []SweepStep) error {
	freq, limit := false, false
	for _, st := range steps {
		freq = freq || st.FrequencyMaxHz > 0
		limit = limit || st.PowerLimitW > 0
	}
	if freq && len(h.policies) == 0 {
		return fmt.Errorf("the sweep sets a frequency ceiling, and %s has no cpufreq policy", h.sys)
	}
	if limit {
		for _, c := range h.comps {
			if c.typ == semconv.HwTypeCPU && c.limitFile == "" {
				return fmt.Errorf("the sweep sets a package power limit, and %s has none for %s (neither a RAPL long_term constraint nor an HSMP power1_cap)", h.sys, c.name)
			}
		}
	}
	return nil
}

// save keeps the content of file before its first write.
func (h *sweepHost) save(file string) error {
	if h.savedFiles[file] {
		return nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	h.saved = append(h.saved, sweepSaved{file: file, content: b})
	h.savedFiles[file] = true
	return nil
}

// write writes v to file after saving it, naming root when it may not.
func (h *sweepHost) write(file string, v int64) error {
	if err := h.save(file); err != nil {
		return err
	}
	err := os.WriteFile(file, []byte(strconv.FormatInt(v, 10)+"\n"), 0o644)
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("writing %s needs root: %w", file, err)
	}
	return err
}

// writeFrequencyMax writes scaling_max_freq of every policy, in kHz.
func (h *sweepHost) writeFrequencyMax(hz float64) error {
	for _, p := range h.policies {
		if err := h.write(filepath.Join(p.dir, "scaling_max_freq"), int64(math.Round(hz/1000))); err != nil {
			return err
		}
	}
	return nil
}

// writePowerLimit writes the limit of every package, in uW.
func (h *sweepHost) writePowerLimit(w float64) error {
	for _, c := range h.comps {
		if c.typ != semconv.HwTypeCPU || c.limitFile == "" {
			continue
		}
		if err := h.write(c.limitFile, int64(math.Round(w*1e6))); err != nil {
			return err
		}
		c.requestedW, c.wroteLimits = w, true
	}
	return nil
}

// restore puts back every file the sweep wrote, the last written first.
func (h *sweepHost) restore() error {
	var errs []error
	for i := len(h.saved) - 1; i >= 0; i-- {
		s := h.saved[i]
		if err := os.WriteFile(s.file, s.content, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", s.file, err))
		}
	}
	h.saved, h.savedFiles = nil, map[string]bool{}
	return errors.Join(errs...)
}

// read takes one sample: the CPU times, every energy counter, the HSMP
// power, the limits and the cpufreq policies.
func (h *sweepHost) read(at time.Time) error {
	stat, err := sweepReadStat(filepath.Join(h.proc, "stat"))
	if err != nil {
		return err
	}
	h.idle = map[int]float64{}
	for c, now := range stat {
		if prev, ok := h.stat[c]; ok && now.total > prev.total && now.idle >= prev.idle {
			h.idle[c] = float64(now.idle-prev.idle) / float64(now.total-prev.total)
		}
	}
	h.stat = stat
	for _, c := range h.comps {
		for _, k := range c.counters {
			v, err := sweepReadInt(k.file)
			if err != nil {
				return err
			}
			d := v - k.last
			if d < 0 && k.rangeUJ > 0 {
				d += k.rangeUJ
			}
			if d > 0 {
				k.joules += float64(d) / 1e6
			}
			k.last = v
		}
		c.hasPower, c.hasLimit = false, false
		if c.powerFile != "" {
			if v, err := sweepReadInt(c.powerFile); err == nil {
				c.powerW, c.hasPower = float64(v)/1e6, true
			}
		}
		if c.limitFile != "" {
			enabled := true
			if c.enabledFile != "" {
				if v, err := sweepReadInt(c.enabledFile); err == nil {
					enabled = v != 0
				}
			}
			if v, err := sweepReadInt(c.limitFile); err == nil && enabled && v > 0 {
				c.limitW, c.hasLimit = float64(v)/1e6, true
			}
		}
	}
	for _, p := range h.policies {
		cur, errC := sweepReadInt(filepath.Join(p.dir, "scaling_cur_freq"))
		lim, errL := sweepReadInt(filepath.Join(p.dir, "scaling_max_freq"))
		p.curKHz, p.limKHz, p.ok = cur, lim, errC == nil && errL == nil
	}
	return nil
}

// sweepReadStat reads the per-CPU lines of /proc/stat (proc_stat(5)): user,
// nice, system, idle, iowait, irq, softirq and steal, whose sum is the
// CPU's time; guest time is already in user and nice.
func sweepReadStat(file string) (map[int]sweepCPUTimes, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	out := map[int]sweepCPUTimes{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
			continue
		}
		c, err := strconv.Atoi(strings.TrimPrefix(f[0], "cpu"))
		if err != nil {
			continue
		}
		var t sweepCPUTimes
		for i, s := range f[1:min(len(f), 9)] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", file, f[0], err)
			}
			t.total += v
			if i == 3 {
				t.idle = v
			}
		}
		out[c] = t
	}
	return out, sc.Err()
}

func sweepReadString(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func sweepReadInt(file string) (int64, error) {
	s, err := sweepReadString(file)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", file, err)
	}
	return v, nil
}

func sweepExists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}
