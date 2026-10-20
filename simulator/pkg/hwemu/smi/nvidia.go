package smi

import (
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// nvidia-smi exit codes, from the RETURN VALUE section of the nvidia-smi
// documentation.
const (
	nvExitInvalidArgument = 2
	nvExitNotAvailable    = 3
	nvExitNoPermission    = 4
	nvExitNotFound        = 6
	nvExitDriver          = 9
	nvExitOther           = 255
)

const nvVariantDefault = "default"

// nvValue says how a query field prints.
type nvValue int

const (
	nvIndex   nvValue = iota // %d of the GPU index
	nvText                   // the stored text verbatim
	nvWatts                  // a value in mW, printed as W with two decimals
	nvPercent                // an integer percentage
	nvMHz                    // an integer clock in MHz
)

// nvField is one --query-gpu field and the NVML state file it reads.
type nvField struct {
	file string
	kind nvValue
}

// nvFields are the query fields fakesmi answers: the agent's six
// (listNvidiaDevices in cmd/agent) plus the limits, identity, use,
// persistence mode and clocks that NVML state holds. Field names are the
// nvidia-smi documentation's; the clock fields and their aliases are those
// nvidia-smi --help-query-gpu lists at driver 590.48.01 (a public capture:
// github.com/utkuozdemir/nvidia_gpu_exporter at f6977bb, file
// internal/captures/linux-x86_64__nvidia-h100-80gb-hbm3__590.48.01.txt,
// lines 328 to 1064). A field the device does not support
// (gpus.fieldSupport) holds its token in this file. No field reads a clock
// lock: NVML has no getter for one, so a lock shows only in the clocks it
// holds.
var nvFields = map[string]nvField{
	"index":                {kind: nvIndex},
	"name":                 {file: "name", kind: nvText},
	"uuid":                 {file: "uuid", kind: nvText},
	"pci.bus_id":           {file: "pci_bus_id", kind: nvText},
	"power.draw":           {file: "power_draw_mw", kind: nvWatts},
	"power.limit":          {file: "power_limit_mw", kind: nvWatts},
	"enforced.power.limit": {file: "enforced_power_limit_mw", kind: nvWatts},
	"power.default_limit":  {file: "power_default_limit_mw", kind: nvWatts},
	"power.min_limit":      {file: "power_min_limit_mw", kind: nvWatts},
	"power.max_limit":      {file: "power_max_limit_mw", kind: nvWatts},
	"utilization.gpu":      {file: "utilization_gpu_pct", kind: nvPercent},
	"persistence_mode":     {file: layout.NVMLPersistenceModeFile, kind: nvText},

	"clocks.sm":                      {file: nvClockSMFile, kind: nvMHz},
	"clocks.current.sm":              {file: nvClockSMFile, kind: nvMHz},
	"clocks.mem":                     {file: nvClockMemFile, kind: nvMHz},
	"clocks.current.memory":          {file: nvClockMemFile, kind: nvMHz},
	"clocks.max.sm":                  {file: nvClockMaxSMFile, kind: nvMHz},
	"clocks.max.mem":                 {file: nvClockMaxMemFile, kind: nvMHz},
	"clocks.max.memory":              {file: nvClockMaxMemFile, kind: nvMHz},
	"pstate":                         {file: "pstate", kind: nvText},
	"clocks_event_reasons.active":    {file: "clocks_event_reasons", kind: nvText},
	"clocks_throttle_reasons.active": {file: "clocks_event_reasons", kind: nvText},
}

// The clock files of a GPU's NVML state, as package hwemu renders them:
// the current and maximum clocks in MHz, and supported_clocks, one line per
// domain with its clocks in descending order ("memory 1500 800" and
// "graphics 1500 1485 ..."). The locks are in layout.NVMLLockedGPUClocksFile
// and layout.NVMLLockedMemClocksFile: none, or "<min>,<max>" as -lgc and
// -lmc were told them, or the token [N/A] where the GPU has no such lock.
const (
	nvClockSMFile        = "clocks_sm_mhz"
	nvClockMemFile       = "clocks_mem_mhz"
	nvClockMaxSMFile     = "clocks_max_sm_mhz"
	nvClockMaxMemFile    = "clocks_max_mem_mhz"
	nvClockSupportedFile = "supported_clocks"
	nvLockNone           = "none"
	nvTokenNA            = "[N/A]"
)

// nvPersistenceWords are the persistence_mode contents -pm writes, by the
// value it takes: 0 or DISABLED disables persistence mode, 1 or ENABLED
// enables it (nvidia-smi -h: "-pm ... 0/DISABLED, 1/ENABLED").
var nvPersistenceWords = map[string]string{"0": "Disabled", "1": "Enabled", "DISABLED": "Disabled", "ENABLED": "Enabled"}

// runNvidiaSMI emulates nvidia-smi -L, --query-gpu, -q -d SUPPORTED_CLOCKS,
// -pl, -pm, the clock locks -lgc (with --mode), -rgc, -lmc and -rmc, and
// the deprecated -ac and -rac, optionally restricted by -i, with
// --error-on-warning (-eow) making a warning of -pl or a clock lock an
// error. An option it
// does not have exits 255 (nvUnknownOption); a known option without its
// value, or a bad combination, is an invalid combination, exit 2. The
// option syntax is nvidia-smi -h's at driver 590.48.01 (the capture of
// nvFields, lines 164 to 194).
func runNvidiaSMI(c *call) int {
	if c.variant != nvVariantDefault {
		return c.unknownVariant()
	}
	var (
		help, list, rgc, rmc, rac, q, errorOnWarning                        bool
		hasIDs, hasQuery, hasFormat, hasLimit, hasPM, hasLGC, hasLMC, hasAC bool
		hasMode, hasDisplay                                                 bool
		ids, query, format, limit, pm, lgc, lmc, ac, mode, display          string
	)
	for i := 0; i < len(c.args); i++ {
		arg := c.args[i]
		name, val, hasVal := arg, "", false
		if strings.HasPrefix(arg, "--") {
			name, val, hasVal = strings.Cut(arg, "=")
		}
		takeValue := func(dst *string, seen *bool) bool {
			if !hasVal {
				if i+1 >= len(c.args) {
					return false
				}
				i++
				val = c.args[i]
			}
			*dst, *seen = val, true
			return true
		}
		flag := func(dst *bool) bool {
			*dst = true
			return !hasVal
		}
		ok := true
		switch name {
		case "-h", "--help":
			ok = flag(&help)
		case "-L", "--list-gpus":
			ok = flag(&list)
		case "-i", "--id":
			ok = takeValue(&ids, &hasIDs)
		case "--query-gpu":
			ok = takeValue(&query, &hasQuery)
		case "--format":
			ok = takeValue(&format, &hasFormat)
		case "-q", "--query":
			ok = flag(&q)
		case "-d", "--display":
			ok = takeValue(&display, &hasDisplay)
		case "-pl", "--power-limit":
			ok = takeValue(&limit, &hasLimit)
		case "-pm", "--persistence-mode":
			ok = takeValue(&pm, &hasPM)
		case "-lgc", "--lock-gpu-clocks":
			ok = takeValue(&lgc, &hasLGC)
		case "-m", "--mode":
			ok = takeValue(&mode, &hasMode)
		case "-rgc", "--reset-gpu-clocks":
			ok = flag(&rgc)
		case "-lmc", "--lock-memory-clocks":
			ok = takeValue(&lmc, &hasLMC)
		case "-rmc", "--reset-memory-clocks":
			ok = flag(&rmc)
		case "-ac", "--applications-clocks":
			ok = takeValue(&ac, &hasAC)
		case "-rac", "--reset-applications-clocks":
			ok = flag(&rac)
		case "-eow", "--error-on-warning":
			ok = flag(&errorOnWarning)
		default:
			return nvUnknownOption(c)
		}
		if !ok {
			return nvInvalidArguments(c)
		}
	}
	if help {
		fmt.Fprint(c.stdout, "nvidia-smi emulated by fakesmi. Options: -L, --query-gpu=FIELDS --format=csv[,noheader][,nounits], -q -d SUPPORTED_CLOCKS, -pl WATTS, -pm 0|1, -lgc MIN[,MAX] [-m 0|1], -rgc, -lmc MIN[,MAX], -rmc, -ac MEM,GRAPHICS, -rac, -i ID, -eow\n")
		return 0
	}
	actions := 0
	for _, a := range []bool{list, hasQuery, q, hasLimit, hasPM, hasLGC, rgc, hasLMC, rmc, hasAC, rac} {
		if a {
			actions++
		}
	}
	if actions == 0 && !hasFormat && !hasMode && !hasDisplay {
		return c.notEmulated("without -L, --query-gpu, -q, -pl, -pm, -lgc, -rgc, -lmc, -rmc, -ac or -rac")
	}
	if actions != 1 || hasQuery != hasFormat || hasMode && !hasLGC || hasDisplay && !q {
		return nvInvalidArguments(c)
	}
	if hasMode && mode != "0" && mode != "1" {
		return nvInvalidArguments(c)
	}
	if q {
		// -q alone prints the whole log on a real nvidia-smi; fakesmi
		// emulates only the SUPPORTED_CLOCKS section.
		if !hasDisplay {
			return c.notEmulated("-q without -d SUPPORTED_CLOCKS")
		}
		for _, d := range strings.Split(display, ",") {
			if strings.TrimSpace(d) != "SUPPORTED_CLOCKS" {
				return c.notEmulated("-q -d " + display)
			}
		}
	}

	var fields []string
	header, units := true, true
	if hasQuery {
		csv := false
		for _, f := range strings.Split(format, ",") {
			switch strings.TrimSpace(f) {
			case "csv":
				csv = true
			case "noheader":
				header = false
			case "nounits":
				units = false
			default:
				return nvInvalidArguments(c)
			}
		}
		if !csv {
			return nvInvalidArguments(c)
		}
		for _, f := range strings.Split(query, ",") {
			f = strings.TrimSpace(f)
			if _, ok := nvFields[f]; !ok {
				fmt.Fprintf(c.stdout, "Field \"%s\" is not a valid field to query.\n", f) // text assumed
				return nvExitInvalidArgument
			}
			fields = append(fields, f)
		}
	}
	var watts float64
	if hasLimit {
		w, err := strconv.ParseFloat(strings.TrimSpace(limit), 64)
		if err != nil || math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return nvInvalidArguments(c)
		}
		watts = w
	}
	pmWord, pmOK := nvPersistenceWords[pm]
	if hasPM && !pmOK {
		return nvInvalidArguments(c)
	}
	var lock nvLockCmd
	switch {
	case hasLGC, hasLMC:
		v := lgc
		lock = nvLockCmd{file: layout.NVMLLockedGPUClocksFile, action: "Setting locked GPU clocks", done: "GPU clocks set to \"(gpuClkMin %d, gpuClkMax %d)\" for GPU %s"}
		if hasLMC {
			v = lmc
			lock = nvLockCmd{file: layout.NVMLLockedMemClocksFile, action: "Setting locked memory clocks", done: "Memory clocks set to \"(memClkMin %d, memClkMax %d)\" for GPU %s"}
		}
		lo, hi, ok := nvParseClocks(v)
		if !ok {
			return nvInvalidArguments(c)
		}
		lock.lo, lock.hi = lo, hi
	case rgc:
		lock = nvLockCmd{file: layout.NVMLLockedGPUClocksFile, action: "Resetting locked GPU clocks", done: "GPU clocks reset to default for GPU %s"}
	case rmc:
		lock = nvLockCmd{file: layout.NVMLLockedMemClocksFile, action: "Resetting locked memory clocks", done: "Memory clocks reset to default for GPU %s"}
	case hasAC:
		// -ac takes <memory,graphics>, such as 2000,800 (nvidia-smi -h).
		m, g, pair := strings.Cut(ac, ",")
		if _, _, ok := nvParseClocks(m); !ok || !pair {
			return nvInvalidArguments(c)
		}
		if _, _, ok := nvParseClocks(g); !ok {
			return nvInvalidArguments(c)
		}
	}

	all, err := nvmlGPUs(c.env)
	if errors.Is(err, errNoNVML) {
		// text assumed: the message nvidia-smi gives without a loaded driver
		fmt.Fprintln(c.stdout, "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver. Make sure that the latest NVIDIA driver is installed and running.")
		return nvExitDriver
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "fakesmi: %v\n", err)
		return nvExitOther
	}
	gpus := all
	if hasIDs {
		if gpus, err = nvSelect(all, ids); err != nil {
			return nvStateError(c, err)
		}
	}
	if len(gpus) == 0 {
		fmt.Fprintln(c.stdout, "No devices were found") // text assumed
		return nvExitNotFound
	}

	switch {
	case list:
		var out strings.Builder
		for _, g := range gpus {
			name, err := g.field("name")
			if err != nil {
				return nvStateError(c, err)
			}
			uuid, err := g.field("uuid")
			if err != nil {
				return nvStateError(c, err)
			}
			fmt.Fprintf(&out, "GPU %d: %s (UUID: %s)\n", g.Index, name, uuid) // format assumed
		}
		io.WriteString(c.stdout, out.String())
		return 0
	case hasQuery:
		return nvQuery(c, gpus, fields, header, units)
	case q:
		return nvSupportedClocks(c, len(all), gpus)
	case hasPM:
		return nvSetPersistenceMode(c, gpus, pmWord)
	case hasLGC, hasLMC, rgc, rmc:
		return nvSetLock(c, gpus, lock, errorOnWarning)
	case hasAC, rac:
		return nvApplicationsClocks(c, gpus, rac)
	}
	return nvSetPowerLimit(c, gpus, watts, errorOnWarning)
}

// nvUnknownOption is nvidia-smi's answer to an option it does not have: the
// invalid-combination line on stdout and exit 255, as driver R525 answers
// it (text assumed).
func nvUnknownOption(c *call) int {
	fmt.Fprintln(c.stdout, "Invalid combination of input arguments. Please run 'nvidia-smi -h' for help.")
	return nvExitOther
}

// nvInvalidArguments is nvidia-smi's answer to a known option without its
// value or a bad combination (text assumed).
func nvInvalidArguments(c *call) int {
	fmt.Fprintln(c.stderr, "Invalid combination of input arguments. Please run 'nvidia-smi -h' for help.")
	return nvExitInvalidArgument
}

// nvStateError ends a call whose NVML state lacks a file nvidia-smi reads.
// No device answers a query with neither a value nor a token, so this is an
// emulated node rendered without that file, reported as an internal error
// (exit 255) with nothing on stdout for the agent to parse.
func nvStateError(c *call, err error) int {
	fmt.Fprintf(c.stderr, "fakesmi: incomplete NVML state: %v\n", err)
	return nvExitOther
}

// nvSelect keeps the GPUs -i names: a comma-separated list of indices, UUIDs
// or PCI bus IDs. An ID that matches no GPU selects nothing, so the caller
// reports that no device was found. An integer is an index only, since UUIDs
// and bus IDs never are integers.
func nvSelect(gpus []nvGPU, ids string) ([]nvGPU, error) {
	var out []nvGPU
	for _, id := range strings.Split(ids, ",") {
		id = strings.TrimSpace(id)
		n, nerr := strconv.Atoi(id)
		found := false
		for _, g := range gpus {
			match := nerr == nil && n == g.Index
			if nerr != nil {
				uuid, err := g.field("uuid")
				if err != nil {
					return nil, err
				}
				bus, err := g.field("pci_bus_id")
				if err != nil {
					return nil, err
				}
				match = strings.EqualFold(id, uuid) || strings.EqualFold(id, bus)
			}
			if match {
				out, found = append(out, g), true
				break
			}
		}
		if !found {
			return nil, nil
		}
	}
	return out, nil
}

// nvQuery prints one CSV line per GPU: the fields joined by ", ", power in W
// with two decimals, the index as %d, clocks in MHz, the name, pstate and
// event reasons verbatim. A value the device does not support prints its
// token, such as [N/A]. The header and the unit suffixes (" W", " %",
// " MHz", and " [W]", " [%]", " [MHz]" in the header) are those of the
// capture of nvFields (lines 1740 to 1741); the header names each field by
// its canonical name, whichever alias the call asked for (nvCanonical).
// Output is printed only once every value has been read, so a missing file
// leaves stdout empty.
func nvQuery(c *call, gpus []nvGPU, fields []string, header, units bool) int {
	var out strings.Builder
	if header {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = f
			if canonical, ok := nvCanonical[f]; ok {
				names[i] = canonical
			}
			if units {
				switch nvFields[f].kind {
				case nvWatts:
					names[i] += " [W]"
				case nvPercent:
					names[i] += " [%]"
				case nvMHz:
					names[i] += " [MHz]"
				}
			}
		}
		fmt.Fprintln(&out, strings.Join(names, ", "))
	}
	for _, g := range gpus {
		vals := make([]string, len(fields))
		for i, f := range fields {
			spec := nvFields[f]
			if spec.kind == nvIndex {
				vals[i] = strconv.Itoa(g.Index)
				continue
			}
			s, err := g.field(spec.file)
			if err != nil {
				return nvStateError(c, err)
			}
			vals[i] = s
			v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
			switch {
			case perr != nil:
				// a token such as [N/A], or text, prints as stored
			case spec.kind == nvWatts:
				vals[i] = fmt.Sprintf("%.2f", v/1000)
				if units {
					vals[i] += " W"
				}
			case spec.kind == nvPercent:
				vals[i] = strconv.FormatInt(int64(v), 10)
				if units {
					vals[i] += " %"
				}
			case spec.kind == nvMHz:
				vals[i] = strconv.FormatInt(int64(v), 10)
				if units {
					vals[i] += " MHz"
				}
			}
		}
		fmt.Fprintln(&out, strings.Join(vals, ", "))
	}
	io.WriteString(c.stdout, out.String())
	return 0
}

// nvCanonical is the name the CSV header prints for a field asked for by
// an alias: nvidia-smi --help-query-gpu lists each field as "<name>" or
// "<alias>", and the header carries the name (the capture of nvFields,
// lines 328 to 1064).
var nvCanonical = map[string]string{
	"clocks.sm":                      "clocks.current.sm",
	"clocks.mem":                     "clocks.current.memory",
	"clocks.max.mem":                 "clocks.max.memory",
	"clocks_throttle_reasons.active": "clocks_event_reasons.active",
}

// nvSetPowerLimit is -pl: it writes power_limit_mw = round(W*1000) for every
// selected GPU whose limits are numbers. The value is checked against each
// such GPU's range first, and nothing is written unless every one accepts it
// and the caller is root. A GPU whose limits hold a token has no settable
// limit, and nvidia-smi treats it as a warning: "Changing power management
// limit is not supported for GPU: <bus id>.", then "Treating as warning and
// moving on.", nothing written, and "All done." after the other GPUs
// (NVIDIA/open-gpu-kernel-modules#483 at driver 530.41.03, and the
// adnanpri/legion-wmi README with -i 0 at drivers 460 to 495). It exits 0,
// as the clock locks do after the same warning at driver 580.178.04; for
// -pl that exit code is assumed. With errorOnWarning (--error-on-warning)
// the warning fails the call instead, with nothing written and exit 3, the
// code of NVML_ERROR_NOT_SUPPORTED; that code and the line after the warning
// are assumed. The limit does not persist across driver reloads on
// hardware: with persistence mode Disabled, the emulator puts it back to the
// default while no client holds the GPU (package hwemu, persistReset). The
// other message text is assumed.
func nvSetPowerLimit(c *call, gpus []nvGPU, watts float64, errorOnWarning bool) int {
	mw := math.Round(watts * 1000)
	limits := make([]nvLimits, len(gpus))
	settable := -1
	for i, g := range gpus {
		l, err := g.limits()
		if err != nil {
			return nvStateError(c, err)
		}
		limits[i] = l
		if !l.supported {
			if errorOnWarning {
				fmt.Fprintf(c.stdout, nvPowerLimitNotSupported+"\n", l.bus)
				fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
				return nvExitNotAvailable
			}
			continue
		}
		if mw < l.lo || mw > l.hi {
			fmt.Fprintf(c.stdout, "Provided power limit %.2f W is not a valid power limit which should be between %.2f W and %.2f W for GPU %s\n", mw/1000, l.lo/1000, l.hi/1000, l.bus)
			fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
			return nvExitInvalidArgument
		}
		if settable < 0 {
			settable = i
		}
	}
	if settable >= 0 && c.env.Unprivileged {
		return nvNoPermission(c, "Changing power management limit", limits[settable].bus)
	}
	for i, g := range gpus {
		if !limits[i].supported {
			fmt.Fprintf(c.stdout, nvPowerLimitNotSupported+"\n", limits[i].bus)
			fmt.Fprintln(c.stdout, "Treating as warning and moving on.")
			continue
		}
		if err := writeAttr(filepath.Join(g.Dir, "power_limit_mw"), strconv.FormatInt(int64(mw), 10)+"\n"); err != nil {
			fmt.Fprintf(c.stderr, "fakesmi: set power limit for GPU %s: %v\n", limits[i].bus, err)
			return nvExitOther
		}
		fmt.Fprintf(c.stdout, "Power limit for GPU %s was set to %.2f W from %.2f W.\n", limits[i].bus, mw/1000, limits[i].old/1000)
	}
	fmt.Fprintln(c.stdout, "All done.")
	return 0
}

// nvPowerLimitNotSupported is the warning -pl prints for a GPU without a
// settable power limit, a format taking its bus id.
const nvPowerLimitNotSupported = "Changing power management limit is not supported for GPU: %s."

// nvLimits is what -pl reads of one GPU before it writes anything. supported
// is false when any of the three limits holds a token instead of a number.
type nvLimits struct {
	bus         string
	old, lo, hi float64
	supported   bool
}

func (g nvGPU) limits() (nvLimits, error) {
	l := nvLimits{supported: true}
	var err error
	if l.bus, err = g.field("pci_bus_id"); err != nil {
		return l, err
	}
	for _, f := range []struct {
		dst  *float64
		file string
	}{{&l.old, "power_limit_mw"}, {&l.lo, "power_min_limit_mw"}, {&l.hi, "power_max_limit_mw"}} {
		v, ok, err := g.number(f.file)
		if err != nil {
			return l, err
		}
		*f.dst, l.supported = v, l.supported && ok
	}
	return l, nil
}

// nvSetPersistenceMode is -pm: it writes word, Enabled or Disabled, to
// persistence_mode of every selected GPU, in place, and prints one line per
// GPU and All done. Every GPU's state is read before anything is written, so
// a GPU without persistence_mode fails the call with nothing changed, as
// does a caller without root. Message text is assumed.
func nvSetPersistenceMode(c *call, gpus []nvGPU, word string) int {
	buses := make([]string, len(gpus))
	for i, g := range gpus {
		bus, err := g.field("pci_bus_id")
		if err != nil {
			return nvStateError(c, err)
		}
		if _, err := g.field(layout.NVMLPersistenceModeFile); err != nil {
			return nvStateError(c, err)
		}
		buses[i] = bus
	}
	if c.env.Unprivileged {
		return nvNoPermission(c, "Changing persistence mode", buses[0])
	}
	var out strings.Builder
	for i, g := range gpus {
		if err := writeAttr(filepath.Join(g.Dir, layout.NVMLPersistenceModeFile), word+"\n"); err != nil {
			fmt.Fprintf(c.stderr, "fakesmi: set persistence mode for GPU %s: %v\n", buses[i], err)
			return nvExitOther
		}
		fmt.Fprintf(&out, "%s persistence mode for GPU %s.\n", word, buses[i])
	}
	fmt.Fprintln(&out, "All done.")
	io.WriteString(c.stdout, out.String())
	return 0
}

// nvNoPermission ends a call that needs root, run without it
// (Env.Unprivileged): one line naming the first GPU and the NVML error
// string, then exit 4, "The current user does not have permission to access
// this device or perform this operation" (nvidia-smi RETURN VALUE). The
// documentation marks -pl, -pm, -lgc and -lmc as needing root; the line's
// wording is assumed.
func nvNoPermission(c *call, action, bus string) int {
	fmt.Fprintf(c.stdout, "%s for GPU %s failed: Insufficient Permissions\n", action, bus)
	fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
	return nvExitNoPermission
}

// nvLockCmd is one clock lock command: the lock file it writes, the action
// its messages name, and the line it prints per GPU, a format taking lo, hi
// and the bus id for -lgc and -lmc, and the bus id alone for -rgc and -rmc.
// lo and hi are the clocks of a lock, zero for a reset.
type nvLockCmd struct {
	file, action, done string
	lo, hi             int64
}

// nvParseClocks parses the value of -lgc or -lmc: "<min>,<max>", or a
// single clock that is both, in whole MHz with 0 < min <= max.
func nvParseClocks(v string) (lo, hi int64, ok bool) {
	a, b, pair := strings.Cut(v, ",")
	if !pair {
		b = a
	}
	lo, errA := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	hi, errB := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if errA != nil || errB != nil || lo <= 0 || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// nvSetLock is -lgc, -rgc, -lmc and -rmc: it writes the lock, "<min>,<max>"
// as told or none for a reset, to the lock file of every selected GPU, in
// place, and prints one line per GPU and All done. A lock is stored as told;
// the emulator clamps the clocks it holds (package hwemu). A GPU whose lock
// file holds [N/A] has no such lock, as Hopper has no run-time memory lock
// (the documentation: -lmc "does not work on GPUs based on NVIDIA Hopper
// architectures"), and nvidia-smi treats it as a warning: "<action> is not
// supported for GPU <bus id>.", then "Treating as warning and moving on.",
// nothing written there, the other GPUs still set, and exit 0. That is what
// -lgc, -lmc and -ac print and return on a GPU that cannot lock its clocks
// (LucaSct/rime docs/adr/0050 at 6ddfab6, measured at driver 580.178.04;
// the -ac line as printed in ailzhang/EfficientPyTorch); the nvidia-smi
// change log of v3.295 treats N/A errors as warnings when a command runs on
// several GPUs at once, and these were measured with one GPU. The action
// words of each command, the warning for -rgc and -rmc and the All done that
// follows the warning are assumed. With errorOnWarning (--error-on-warning,
// documented as "Return a non-zero error for warnings") the warning fails
// the call instead, with nothing written and exit 3, the code of
// NVML_ERROR_NOT_SUPPORTED; that code and the line after the warning are
// assumed, as for -pl. Every GPU is checked before anything is written; then
// the caller must be root ("Requires root") when any GPU has the lock. Other
// message text is assumed.
func nvSetLock(c *call, gpus []nvGPU, cmd nvLockCmd, errorOnWarning bool) int {
	buses := make([]string, len(gpus))
	lockable := make([]bool, len(gpus))
	settable := -1
	for i, g := range gpus {
		bus, err := g.field("pci_bus_id")
		if err != nil {
			return nvStateError(c, err)
		}
		cur, err := g.field(cmd.file)
		if err != nil {
			return nvStateError(c, err)
		}
		buses[i] = bus
		if strings.TrimSpace(cur) == nvTokenNA {
			if errorOnWarning {
				fmt.Fprintf(c.stdout, nvLockNotSupported+"\n", cmd.action, bus)
				fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
				return nvExitNotAvailable
			}
			continue
		}
		lockable[i] = true
		if settable < 0 {
			settable = i
		}
	}
	if settable >= 0 && c.env.Unprivileged {
		return nvNoPermission(c, cmd.action, buses[settable])
	}
	value := nvLockNone
	if cmd.hi > 0 {
		value = strconv.FormatInt(cmd.lo, 10) + "," + strconv.FormatInt(cmd.hi, 10)
	}
	var out strings.Builder
	for i, g := range gpus {
		if !lockable[i] {
			fmt.Fprintf(&out, nvLockNotSupported+"\n", cmd.action, buses[i])
			fmt.Fprintln(&out, "Treating as warning and moving on.")
			continue
		}
		if err := writeAttr(filepath.Join(g.Dir, cmd.file), value+"\n"); err != nil {
			fmt.Fprintf(c.stderr, "fakesmi: %s for GPU %s: %v\n", cmd.action, buses[i], err)
			return nvExitOther
		}
		if cmd.hi > 0 {
			fmt.Fprintf(&out, cmd.done+"\n", cmd.lo, cmd.hi, buses[i])
		} else {
			fmt.Fprintf(&out, cmd.done+"\n", buses[i])
		}
	}
	fmt.Fprintln(&out, "All done.")
	io.WriteString(c.stdout, out.String())
	return 0
}

// nvLockNotSupported is the warning a clock lock command prints for a GPU
// without that lock, a format taking the action and the bus id.
const nvLockNotSupported = "%s is not supported for GPU %s."

// nvApplicationsClocks is -ac and -rac: application clocks are deprecated
// (nvml.h marks them DEPRECATED(13.0)), so both fail on the first selected
// GPU with the NVML error string nvidia-smi prints for them, "Requested
// functionality has been deprecated" (the capture of nvFields, -q), and
// exit 3. Nothing is written. The rest of the line is assumed.
func nvApplicationsClocks(c *call, gpus []nvGPU, reset bool) int {
	bus, err := gpus[0].field("pci_bus_id")
	if err != nil {
		return nvStateError(c, err)
	}
	action := "Setting applications clocks"
	if reset {
		action = "Resetting applications clocks"
	}
	fmt.Fprintf(c.stdout, "%s is not supported for GPU %s: Requested functionality has been deprecated\n", action, bus)
	fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
	return nvExitNotAvailable
}

// nvSupportedClocks is -q -d SUPPORTED_CLOCKS: the -q header, then per
// selected GPU each supported memory clock with the graphics clocks it
// offers under it, both in descending order: every one, or those up to the
// memory clock's ceiling in supported_clocks, as a lower memory clock
// offers only the lower graphics clocks. The header lines, their value
// column and the GPU line are those of nvidia-smi -q at driver 590.48.01
// (the capture of nvFields, lines 1479 to 1487), without the CUDA Version
// line, which the NVML state does not hold; attached counts every GPU, as
// -i does not change it. The Supported Clocks section itself is assumed
// (capture nvidia-smi -q -d SUPPORTED_CLOCKS to check it). Every GPU's
// state is read before anything is printed.
func nvSupportedClocks(c *call, attached int, gpus []nvGPU) int {
	driver, err := readAttr(filepath.Join(c.env.StateRoot, layout.NVMLDir, "driver_version"))
	if err != nil {
		return nvStateError(c, err)
	}
	var out strings.Builder
	out.WriteString("\n==============NVSMI LOG==============\n\n")
	nvLogLine(&out, 0, "Timestamp", time.Now().Format("Mon Jan _2 15:04:05 2006"))
	nvLogLine(&out, 0, "Driver Version", driver)
	out.WriteString("\n")
	nvLogLine(&out, 0, "Attached GPUs", strconv.Itoa(attached))
	for _, g := range gpus {
		bus, err := g.field("pci_bus_id")
		if err != nil {
			return nvStateError(c, err)
		}
		s, err := g.field(nvClockSupportedFile)
		if err != nil {
			return nvStateError(c, err)
		}
		mem, graphics, ceil, err := nvParseSupported(s)
		if err != nil {
			return nvStateError(c, fmt.Errorf("%s: %w", filepath.Join(g.Dir, nvClockSupportedFile), err))
		}
		fmt.Fprintf(&out, "GPU %s\n    Supported Clocks\n", bus)
		for _, m := range mem {
			nvLogLine(&out, 8, "Memory", strconv.FormatInt(m, 10)+" MHz")
			top, capped := ceil[m]
			for _, gr := range graphics {
				if !capped || gr <= top {
					nvLogLine(&out, 12, "Graphics", strconv.FormatInt(gr, 10)+" MHz")
				}
			}
		}
		out.WriteString("\n")
	}
	io.WriteString(c.stdout, out.String())
	return 0
}

// nvLogLine writes one line of -q output: the key, indented by indent
// spaces, padded so the colon sits in column 55, then the value.
func nvLogLine(b *strings.Builder, indent int, key, value string) {
	fmt.Fprintf(b, "%-55s: %s\n", strings.Repeat(" ", indent)+key, value)
}

// nvParseSupported reads supported_clocks: a memory line and a graphics
// line, each listing whole MHz, and a graphics-max line, "graphics-max
// <memory MHz> <graphics MHz>", for each memory clock that offers only the
// graphics clocks up to a ceiling, which ceil maps.
func nvParseSupported(s string) (mem, graphics []int64, ceil map[int64]int64, err error) {
	ceil = map[int64]int64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		vals := make([]int64, 0, len(f)-1)
		for _, v := range f[1:] {
			x, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				return nil, nil, nil, fmt.Errorf("clock %q is not whole MHz", v)
			}
			vals = append(vals, x)
		}
		switch f[0] {
		case "memory":
			mem = vals
		case "graphics":
			graphics = vals
		case "graphics-max":
			if len(vals) != 2 {
				return nil, nil, nil, fmt.Errorf("line %q is not graphics-max <memory MHz> <graphics MHz>", line)
			}
			ceil[vals[0]] = vals[1]
		default:
			return nil, nil, nil, fmt.Errorf("line %q is neither memory, graphics nor graphics-max", line)
		}
	}
	if len(mem) == 0 || len(graphics) == 0 {
		return nil, nil, nil, errors.New("no memory or no graphics clocks")
	}
	return mem, graphics, ceil, nil
}
