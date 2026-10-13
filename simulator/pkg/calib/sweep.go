package calib

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The on-node sweep collector, for hwcalib sweep. It holds the node at each
// step of a sweep, a CPU load and optionally a frequency ceiling and a
// package power limit, and records every sample as one MetricsData line in
// the names hwcalib fit reads: the RAPL energy of each package and DRAM
// zone and the HSMP socket power, cpufreq speed and limits, the package
// power limit, the utilization of every logical CPU from /proc/stat with
// cpu.logical_number, and the setpoints it held. It reads only what the
// kernel shows under the sys and proc roots, so a whole-host meter (a BMC,
// a PDU) is not read; a converter can add hw.host.* to the file.
//
// Reading energy_uj needs root: the file is mode 0400 since Linux commit
// 949dd0104c49 ("powercap: restrict energy meter to root access"). A sweep
// writes scaling_max_freq and power limits only when SweepOptions.WriteLimits
// is set, and puts back what it found when it returns.

const (
	// SweepServiceName is service.name of the collector's resource.
	SweepServiceName = "hwcalib"
	// SweepScopeName is the instrumentation scope of the collector's metrics.
	SweepScopeName = "github.com/matbun/joulie/simulator/pkg/calib"
)

// sweepServiceNamespace is service.namespace, the simulator's.
const sweepServiceNamespace = "joulie"

// SweepStep is one setpoint of a sweep: the busy fraction of every logical
// CPU, 0 to 1, and, when positive, the scaling_max_freq written to every
// cpufreq policy, in Hz, and the power limit written to every package, in W.
// Zero leaves the frequency ceiling or the limit as the step before left it.
type SweepStep struct {
	CPUUtilization float64
	FrequencyMaxHz float64
	PowerLimitW    float64
}

// SweepGrid is the full-factorial sweep of the three knobs: power limits
// outermost, then frequency ceilings, then utilizations. An empty list of
// limits or frequencies holds that knob where it is.
func SweepGrid(utils, freqsHz, limitsW []float64) []SweepStep {
	if len(freqsHz) == 0 {
		freqsHz = []float64{0}
	}
	if len(limitsW) == 0 {
		limitsW = []float64{0}
	}
	var out []SweepStep
	for _, l := range limitsW {
		for _, f := range freqsHz {
			for _, u := range utils {
				out = append(out, SweepStep{CPUUtilization: u, FrequencyMaxHz: f, PowerLimitW: l})
			}
		}
	}
	return out
}

// LoadDriver runs the CPU load of a sweep. SetUtilization keeps every CPU
// busy for the share u of the time from now on; Stop ends the load.
type LoadDriver interface {
	SetUtilization(u float64) error
	Stop()
}

// Clock is the time of a sweep. Sleep returns after d, or with the
// context's error when it ends first.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// WallClock is the host's clock.
type WallClock struct{}

// Now is time.Now.
func (WallClock) Now() time.Time { return time.Now() }

// Sleep waits d or until ctx ends.
func (WallClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SweepOptions configure Sweep. SysRoot and ProcRoot default to /sys and
// /proc. Node names the resource (service.instance.id and host.name). Each
// step lasts Hold and is sampled every Period, the first sample one Period
// into it. Load runs the CPU load, and Sweep stops it when it returns; nil
// is a DutyCycleLoad with DutyPeriod on every CPU /proc/stat lists. Class
// and MemoryIntensity describe the load in the setpoint record; a
// DutyCycleLoad spins on registers, which no class (the models' default
// weights) and memory intensity 0 describe. Clock
// defaults to WallClock. Out receives one MetricsData per line; Log, when
// set, a line per step and any warning.
type SweepOptions struct {
	SysRoot         string
	ProcRoot        string
	Node            string
	Steps           []SweepStep
	Hold            time.Duration
	Period          time.Duration
	WriteLimits     bool
	Load            LoadDriver
	DutyPeriod      time.Duration
	Class           string
	MemoryIntensity float64
	Clock           Clock
	Out             io.Writer
	Log             io.Writer
}

// Sweep runs the sweep o describes and returns the number of samples it
// wrote. It reads the host before it starts and fails when no package
// energy or power can be read, naming root when a file is not readable. A
// step that sets a frequency ceiling or a power limit needs o.WriteLimits.
// Every file it wrote is put back as it was found when it returns, also
// when ctx ends or a step fails; the lines written so far stay valid.
func Sweep(ctx context.Context, o SweepOptions) (samples int, err error) {
	// The load stops first, then every file written is put back.
	var h *sweepHost
	defer func() {
		if o.Load != nil {
			o.Load.Stop()
		}
		if h != nil {
			if rerr := h.restore(); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err := o.Validate(); err != nil {
		return 0, err
	}
	if o.Out == nil {
		return 0, errors.New("sweep: no output")
	}
	if o.SysRoot == "" {
		o.SysRoot = "/sys"
	}
	if o.ProcRoot == "" {
		o.ProcRoot = "/proc"
	}
	if o.Clock == nil {
		o.Clock = WallClock{}
	}
	// The load's goroutines may warn while the sweep logs a step.
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		if o.Log != nil {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	h, err = sweepDiscover(o.SysRoot, o.ProcRoot)
	if err != nil {
		return 0, err
	}
	if err := h.checkKnobs(o.Steps); err != nil {
		return 0, err
	}
	if o.Load == nil {
		o.Load = NewDutyCycleLoad(h.cpus, o.DutyPeriod, func(err error) {
			logf("warning: the load runs unpinned: %v", err)
		})
	}

	res := otlpjson.Resource{Attributes: []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrServiceName, SweepServiceName),
		otlpjson.String(semconv.AttrServiceNamespace, sweepServiceNamespace),
		otlpjson.String(semconv.AttrServiceInstanceID, o.Node),
		otlpjson.String(semconv.AttrHostName, o.Node),
	}}
	scope := otlpjson.Scope{Name: SweepScopeName}
	enc := json.NewEncoder(o.Out)
	enc.SetEscapeHTML(false)

	start := o.Clock.Now()
	if err := h.read(start); err != nil {
		return 0, err
	}
	n := int(o.Hold / o.Period)
	var held SweepStep
	for i, st := range o.Steps {
		if st.FrequencyMaxHz > 0 && st.FrequencyMaxHz != held.FrequencyMaxHz {
			if err := h.writeFrequencyMax(st.FrequencyMaxHz); err != nil {
				return samples, err
			}
			held.FrequencyMaxHz = st.FrequencyMaxHz
		}
		if st.PowerLimitW > 0 && st.PowerLimitW != held.PowerLimitW {
			if err := h.writePowerLimit(st.PowerLimitW); err != nil {
				return samples, err
			}
			held.PowerLimitW = st.PowerLimitW
		}
		held.CPUUtilization = st.CPUUtilization
		if err := o.Load.SetUtilization(st.CPUUtilization); err != nil {
			return samples, fmt.Errorf("step %d: %w", i+1, err)
		}
		logf("step %d of %d: %s for %v", i+1, len(o.Steps), sweepDescribe(held), o.Hold)
		stepStart := o.Clock.Now()
		for k := 1; k <= n; k++ {
			if err := o.Clock.Sleep(ctx, stepStart.Add(time.Duration(k)*o.Period).Sub(o.Clock.Now())); err != nil {
				return samples, err
			}
			at := o.Clock.Now()
			if err := h.read(at); err != nil {
				return samples, err
			}
			md := otlpjson.NewMetricsData(res, scope, h.metrics(start, at, held, o.Class, o.MemoryIntensity)...)
			if err := enc.Encode(md); err != nil {
				return samples, fmt.Errorf("write sample: %w", err)
			}
			samples++
		}
	}
	return samples, nil
}

// Validate checks the plan of o (steps, timing, node, load description)
// before anything is read or written.
func (o SweepOptions) Validate() error {
	switch {
	case len(o.Steps) == 0:
		return errors.New("no steps")
	case o.Period <= 0:
		return fmt.Errorf("sample period %v is not positive", o.Period)
	case o.Hold < o.Period:
		return fmt.Errorf("hold %v is shorter than the sample period %v", o.Hold, o.Period)
	case o.Node == "":
		return errors.New("no node name")
	case o.MemoryIntensity < 0 || o.MemoryIntensity > 1:
		return fmt.Errorf("memory intensity %v is outside 0..1", o.MemoryIntensity)
	}
	for i, st := range o.Steps {
		switch {
		case !(st.CPUUtilization >= 0 && st.CPUUtilization <= 1):
			return fmt.Errorf("step %d: utilization %v is outside 0..1", i+1, st.CPUUtilization)
		case !(st.FrequencyMaxHz >= 0) || math.IsInf(st.FrequencyMaxHz, 0):
			return fmt.Errorf("step %d: frequency ceiling %v Hz is not a positive number", i+1, st.FrequencyMaxHz)
		case !(st.PowerLimitW >= 0) || math.IsInf(st.PowerLimitW, 0):
			return fmt.Errorf("step %d: power limit %v W is not a positive number", i+1, st.PowerLimitW)
		case !o.WriteLimits && (st.FrequencyMaxHz > 0 || st.PowerLimitW > 0):
			return fmt.Errorf("step %d sets a frequency ceiling or a power limit, which needs write-limits", i+1)
		}
	}
	return nil
}

// sweepDescribe is a step as the log shows it.
func sweepDescribe(st SweepStep) string {
	s := fmt.Sprintf("utilization %g", st.CPUUtilization)
	if st.FrequencyMaxHz > 0 {
		s += fmt.Sprintf(", scaling_max_freq %g MHz", st.FrequencyMaxHz/1e6)
	}
	if st.PowerLimitW > 0 {
		s += fmt.Sprintf(", package limit %g W", st.PowerLimitW)
	}
	return s
}

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

var (
	sweepPackageName    = regexp.MustCompile(`^package-(\d+)$`)
	sweepPackageDieName = regexp.MustCompile(`^package-(\d+)-die-(\d+)$`)
)

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
		if m := sweepPackageName.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[1])
			if _, dup := pkgZones[s]; !dup {
				pkgZones[s] = z
			}
		} else if m := sweepPackageDieName.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[1])
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
// zone z, or "" when it has none that takes writes (a constraint that
// rejects them shows a directory in its place).
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

// metrics are the readings of the last sample in the name table's names,
// with the setpoints held.
func (h *sweepHost) metrics(start, at time.Time, held SweepStep, class string, mem float64) []otlpjson.Metric {
	var energy, power, limits []otlpjson.NumberDataPoint
	for _, c := range h.comps {
		attrs := []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrHwID, c.id),
			otlpjson.String(semconv.AttrHwType, c.typ),
			otlpjson.String(semconv.AttrHwName, c.name),
		}
		if c.parent != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwParent, c.parent))
		}
		if len(c.counters) > 0 {
			j := 0.0
			for _, k := range c.counters {
				j += k.joules
			}
			energy = append(energy, otlpjson.DoublePoint(start, at, j, attrs...))
		}
		if c.hasPower {
			power = append(power, otlpjson.DoublePoint(time.Time{}, at, c.powerW, attrs...))
		}
		lim := func(state string, w float64) otlpjson.NumberDataPoint {
			return otlpjson.DoublePoint(time.Time{}, at, w,
				otlpjson.String(semconv.AttrHwID, c.id),
				otlpjson.String(semconv.AttrHwType, c.typ),
				otlpjson.String(semconv.AttrJoulieLimitState, state))
		}
		if c.wroteLimits {
			limits = append(limits, lim(semconv.LimitStateRequested, c.requestedW))
		}
		if c.hasLimit {
			limits = append(limits, lim(semconv.LimitStateEnforced, c.limitW))
		}
	}

	var speed, speedLimit []otlpjson.NumberDataPoint
	for _, c := range h.comps {
		if c.typ != semconv.HwTypeCPU {
			continue
		}
		var cur, lim float64
		var maxKHz int64
		k := 0
		for _, p := range h.policies {
			if p.socket == c.socket && p.ok {
				cur += float64(p.curKHz)
				lim += float64(p.limKHz)
				maxKHz = max(maxKHz, p.maxKHz)
				k++
			}
		}
		if k == 0 {
			continue
		}
		attrs := []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, c.id), otlpjson.String(semconv.AttrHwName, c.name)}
		if h.vendor != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwVendor, h.vendor))
		}
		if h.model != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwModel, h.model))
		}
		speed = append(speed, otlpjson.DoublePoint(time.Time{}, at, cur/float64(k)*1000, attrs...))
		speedLimit = append(speedLimit,
			otlpjson.DoublePoint(time.Time{}, at, float64(maxKHz)*1000, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeMax))...),
			otlpjson.DoublePoint(time.Time{}, at, lim/float64(k)*1000, append(attrs, otlpjson.String(semconv.AttrHwLimitType, semconv.LimitTypeThrottled))...))
	}

	idle := otlpjson.String(semconv.AttrCPUMode, semconv.CPUModeIdle)
	var util []otlpjson.NumberDataPoint
	for _, c := range h.cpus {
		if v, ok := h.idle[c]; ok {
			util = append(util, otlpjson.DoublePoint(time.Time{}, at, v, idle, otlpjson.Int(semconv.AttrCPULogicalNumber, int64(c))))
		}
	}

	knob := func(k string, v float64, extra ...otlpjson.KeyValue) otlpjson.NumberDataPoint {
		return otlpjson.DoublePoint(time.Time{}, at, v, append([]otlpjson.KeyValue{otlpjson.String(semconv.AttrCalibrationKnob, k)}, extra...)...)
	}
	cpuLoad := []otlpjson.KeyValue{otlpjson.Double(semconv.AttrCalibrationMemoryIntensity, mem)}
	if class != "" {
		cpuLoad = append([]otlpjson.KeyValue{otlpjson.String(semconv.AttrCalibrationClass, class)}, cpuLoad...)
	}
	setpoints := []otlpjson.NumberDataPoint{knob(semconv.KnobCPUUtilization, held.CPUUtilization, cpuLoad...)}
	if held.FrequencyMaxHz > 0 {
		setpoints = append(setpoints, knob(semconv.KnobCPUFrequencyMax, held.FrequencyMaxHz))
	}
	if held.PowerLimitW > 0 {
		setpoints = append(setpoints, knob(semconv.KnobCPUPowerLimit, held.PowerLimitW))
	}

	var out []otlpjson.Metric
	if len(energy) > 0 {
		out = append(out, otlpjson.SumMetric(semconv.HwEnergy, energy...))
	}
	if len(power) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.HwPower, power...))
	}
	if len(speed) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.HwCPUSpeed, speed...), otlpjson.GaugeMetric(semconv.HwCPUSpeedLimit, speedLimit...))
	}
	if len(limits) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.JoulieHwPowerLimit, limits...))
	}
	if len(util) > 0 {
		out = append(out, otlpjson.GaugeMetric(semconv.SystemCPUUtilization, util...))
	}
	return append(out, otlpjson.GaugeMetric(semconv.JoulieCalibrationSetpoint, setpoints...))
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

// DutyCycleLoad is the CPU load of a sweep on real hardware: one goroutine
// per CPU, each locked to its own OS thread and, on Linux, pinned to its
// CPU, spinning for the share u of every period and sleeping for the rest.
// The spin touches no memory: its memory intensity is 0.
type DutyCycleLoad struct {
	period time.Duration
	share  atomic.Uint64 // math.Float64bits of u
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// sweepPin pins the calling OS thread to cpu. sweep_linux.go sets it.
var sweepPin = func(cpu int) error {
	return errors.New("pinning a thread to a CPU is not supported on " + runtime.GOOS)
}

// sweepSink keeps the spin's result alive, so the compiler keeps the loop.
var sweepSink atomic.Uint64

// NewDutyCycleLoad starts an idle load on cpus with a duty period (100 ms
// when zero). warn, when set, is called once when a thread cannot be
// pinned; the load then runs where the scheduler puts it. GOMAXPROCS is
// raised to one more than the CPUs, so the sampler always has a thread.
func NewDutyCycleLoad(cpus []int, period time.Duration, warn func(error)) *DutyCycleLoad {
	if period <= 0 {
		period = 100 * time.Millisecond
	}
	if n := len(cpus) + 1; runtime.GOMAXPROCS(0) < n {
		runtime.GOMAXPROCS(n)
	}
	l := &DutyCycleLoad{period: period, stop: make(chan struct{})}
	var warnOnce sync.Once
	for _, c := range cpus {
		l.wg.Add(1)
		go l.run(c, func(err error) {
			if warn != nil {
				warnOnce.Do(func() { warn(err) })
			}
		})
	}
	return l
}

// SetUtilization sets the busy share of every CPU, 0 to 1.
func (l *DutyCycleLoad) SetUtilization(u float64) error {
	if !(u >= 0 && u <= 1) {
		return fmt.Errorf("utilization %v is outside 0..1", u)
	}
	l.share.Store(math.Float64bits(u))
	return nil
}

// Stop ends the load and waits for every goroutine.
func (l *DutyCycleLoad) Stop() {
	l.once.Do(func() { close(l.stop) })
	l.wg.Wait()
}

// run is the load of one CPU. The goroutine keeps its thread locked when it
// returns, so the runtime ends the thread rather than reuse one pinned to a
// single CPU.
func (l *DutyCycleLoad) run(cpu int, warn func(error)) {
	defer l.wg.Done()
	runtime.LockOSThread()
	if err := sweepPin(cpu); err != nil {
		warn(fmt.Errorf("CPU %d: %w", cpu, err))
	}
	x := uint64(cpu) + 1
	for {
		begin := time.Now()
		busy := time.Duration(math.Float64frombits(l.share.Load()) * float64(l.period))
		for end := begin.Add(busy); time.Now().Before(end); {
			for i := 0; i < 4096; i++ {
				x = x*6364136223846793005 + 1442695040888963407
			}
		}
		rest := l.period - time.Since(begin)
		if rest <= 0 {
			select {
			case <-l.stop:
				sweepSink.Store(x)
				return
			default:
				continue
			}
		}
		t := time.NewTimer(rest)
		select {
		case <-l.stop:
			t.Stop()
			sweepSink.Store(x)
			return
		case <-t.C:
		}
	}
}
