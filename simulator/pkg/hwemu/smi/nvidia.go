package smi

import (
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// nvidia-smi exit codes, from the RETURN VALUE section of the nvidia-smi
// documentation.
const (
	nvExitInvalidArgument = 2
	nvExitNotAvailable    = 3
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
)

// nvField is one --query-gpu field and the NVML state file it reads.
type nvField struct {
	file string
	kind nvValue
}

// nvFields are the query fields fakesmi answers: the agent's six
// (listNvidiaDevices in cmd/agent) plus the limits, identity and use that
// NVML state holds. Field names are the nvidia-smi documentation's. A field
// the device does not support (gpus.fieldSupport) holds its token in this
// file.
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
}

// runNvidiaSMI emulates nvidia-smi -L, --query-gpu and -pl, optionally
// restricted by -i. Anything else is an invalid combination, exit 2.
func runNvidiaSMI(c *call) int {
	if c.variant != nvVariantDefault {
		return c.unknownVariant()
	}
	var (
		help, list, hasIDs, hasQuery, hasFormat, hasLimit bool
		ids, query, format, limit                         string
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
		ok := true
		switch name {
		case "-h", "--help":
			help, ok = true, !hasVal
		case "-L", "--list-gpus":
			list, ok = true, !hasVal
		case "-i", "--id":
			ok = takeValue(&ids, &hasIDs)
		case "--query-gpu":
			ok = takeValue(&query, &hasQuery)
		case "--format":
			ok = takeValue(&format, &hasFormat)
		case "-pl", "--power-limit":
			ok = takeValue(&limit, &hasLimit)
		default:
			ok = false
		}
		if !ok {
			return nvInvalidArguments(c)
		}
	}
	if help {
		fmt.Fprint(c.stdout, "nvidia-smi emulated by fakesmi. Options: -L, --query-gpu=FIELDS --format=csv[,noheader][,nounits], -pl WATTS, -i ID\n")
		return 0
	}
	actions := 0
	for _, a := range []bool{list, hasQuery, hasLimit} {
		if a {
			actions++
		}
	}
	if actions == 0 && !hasFormat {
		return c.notEmulated("without -L, --query-gpu or -pl")
	}
	if actions != 1 || hasQuery != hasFormat {
		return nvInvalidArguments(c)
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

	gpus, err := nvmlGPUs(c.env)
	if errors.Is(err, errNoNVML) {
		// text assumed: the message nvidia-smi gives without a loaded driver
		fmt.Fprintln(c.stdout, "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver. Make sure that the latest NVIDIA driver is installed and running.")
		return nvExitDriver
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "fakesmi: %v\n", err)
		return nvExitOther
	}
	if hasIDs {
		if gpus, err = nvSelect(gpus, ids); err != nil {
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
	}
	return nvSetPowerLimit(c, gpus, watts)
}

// nvInvalidArguments is nvidia-smi's answer to an unknown option or a bad
// combination (text assumed).
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
// with two decimals, the index as %d, the name verbatim. A value the device
// does not support prints its token, such as [N/A]. The header and the unit
// suffixes (" W", " %", and " [W]" in the header) are assumed. Output is
// printed only once every value has been read, so a missing file leaves
// stdout empty.
func nvQuery(c *call, gpus []nvGPU, fields []string, header, units bool) int {
	var out strings.Builder
	if header {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = f
			if units {
				switch nvFields[f].kind {
				case nvWatts:
					names[i] += " [W]"
				case nvPercent:
					names[i] += " [%]"
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
			}
		}
		fmt.Fprintln(&out, strings.Join(vals, ", "))
	}
	io.WriteString(c.stdout, out.String())
	return 0
}

// nvSetPowerLimit is -pl: it writes power_limit_mw = round(W*1000) for every
// selected GPU. The value is checked against each GPU's range first, and
// nothing is written unless every GPU accepts it. The limit does not persist
// across driver reloads on hardware, and persistence mode is not modelled.
// Message text is assumed.
func nvSetPowerLimit(c *call, gpus []nvGPU, watts float64) int {
	mw := math.Round(watts * 1000)
	limits := make([]nvLimits, len(gpus))
	for i, g := range gpus {
		l, err := g.limits()
		if err != nil {
			return nvStateError(c, err)
		}
		if !l.supported {
			fmt.Fprintf(c.stdout, "Changing power management limit is not supported for GPU: %s.\n", l.bus)
			return nvExitNotAvailable
		}
		if mw < l.lo || mw > l.hi {
			fmt.Fprintf(c.stdout, "Provided power limit %.2f W is not a valid power limit which should be between %.2f W and %.2f W for GPU %s\n", mw/1000, l.lo/1000, l.hi/1000, l.bus)
			fmt.Fprintln(c.stdout, "Terminating early due to previous errors.")
			return nvExitInvalidArgument
		}
		limits[i] = l
	}
	for i, g := range gpus {
		if err := writeAttr(filepath.Join(g.Dir, "power_limit_mw"), strconv.FormatInt(int64(mw), 10)+"\n"); err != nil {
			fmt.Fprintf(c.stderr, "fakesmi: set power limit for GPU %s: %v\n", limits[i].bus, err)
			return nvExitOther
		}
		fmt.Fprintf(c.stdout, "Power limit for GPU %s was set to %.2f W from %.2f W.\n", limits[i].bus, mw/1000, limits[i].old/1000)
	}
	fmt.Fprintln(c.stdout, "All done.")
	return 0
}

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
