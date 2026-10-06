package smi

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// amd-smi as amdsmi_cli of rocm-7.1.0 and rocm-7.2.0 behaves. "parser 7.2:N"
// is a line of 7.2's amdsmi_parser.py, "cmds 7.1:N" one of 7.1's
// amdsmi_commands.py, "logger 7.2:N" one of 7.2's amdsmi_logger.py;
// amdsmi_cli.py and amdsmi_cli_exceptions.py are the same in both. The agent
// never runs amd-smi; it is here for emulated cluster nodes and for people in
// the pod.

const (
	amdsmiVariant71 = "7.1"
	amdsmiVariant72 = "7.2"
)

// amdsmiCommands are the subcommands amdsmi_cli.py accepts as its first
// argument (parser 7.2:124-127; amdsmi_cli.py:181-214).
var amdsmiCommands = []string{"version", "list", "static", "firmware", "ucode", "bad-pages",
	"metric", "process", "profile", "event", "topology", "set", "reset", "monitor", "dmon",
	"xgmi", "partition", "ras", "node", "default"}

// Option strings of the subcommands fakesmi models, in registration order
// (parser 7.2:846-1450, 707-760), with options that depend on the platform
// all listed. 7.2's set adds -S and -F.
const (
	amdsmiStaticSpec = "-a/--asic -b/--bus -V/--vbios -I/--ifwi -d/--driver -v/--vram -c/--cache " +
		"-B/--board -R/--process-isolation -r/--ras -C/--clock -p/--partition -l/--limit " +
		"-P/--soc-pstate -x/--xgmi-plpd -u/--numa -d/--dfc-ucode -f/--fb-info -n/--num-vf -s/--smu " +
		"-i/--interface-ver"
	amdsmiMetricSpec = "-m/--mem-usage -u/--usage -p/--power -c/--clock -t/--temperature -P/--pcie " +
		"-e/--ecc -k/--ecc-blocks -V/--voltage -b/--base-board -G/--gpu-board -f/--fan " +
		"-C/--voltage-curve -o/--overdrive -l/--perf-level -x/--xgmi-err -E/--energy -v/--violation " +
		"-T/--throttle -s/--schedule -G/--guard -u/--guest-data -f/--fb-usage -m/--xgmi " +
		"--cpu-power-metrics --cpu-prochot --cpu-freq-metrics --cpu-c0-res --cpu-lclk-dpm-level " +
		"--cpu-pwr-svi-telemetry-rails --cpu-io-bandwidth --cpu-xgmi-bandwidth --cpu-metrics-ver " +
		"--cpu-metrics-table --cpu-socket-energy --cpu-ddr-bandwidth --cpu-temp " +
		"--cpu-dimm-temp-range-rate --cpu-dimm-pow-consumption --cpu-dimm-thermal-sensor " +
		"--core-boost-limit --core-curr-active-freq-core-limit --core-energy"
	amdsmiSetSpec = "-f/--fan -l/--perf-level -P/--profile -d/--perf-determinism -C/--compute-partition " +
		"-M/--memory-partition -o/--power-cap -p/--soc-pstate -x/--xgmi-plpd -c/--clk-level " +
		"-L/--clk-limit -R/--process-isolation --cpu-pwr-limit --cpu-xgmi-link-width " +
		"--cpu-lclk-dpm-level --cpu-pwr-eff-mode --cpu-gmi3-link-width --cpu-pcie-link-rate " +
		"--cpu-df-pstate-range --cpu-enable-apb --cpu-disable-apb --soc-boost-limit --core-boost-limit"
	amdsmiResetSpec = "-G/--gpureset -c/--clocks -f/--fans -p/--profile -x/--xgmierr " +
		"-d/--perf-determinism -o/--power-cap -r/--reload-driver -l/--clean-local-data"
	amdsmiDeviceSpec   = "-g/--gpu -U/--cpu -O/--core -v/--vf"
	amdsmiModifierSpec = "--json --csv --file --loglevel"
)

// amdsmiTempLimits are the temperature keys of static --limit. Rendered trees
// have no temperature attributes, so the library calls fail and print N/A.
var amdsmiTempLimits = []string{"slowdown_edge_temperature", "slowdown_hotspot_temperature",
	"slowdown_vram_temperature", "shutdown_edge_temperature", "shutdown_hotspot_temperature",
	"shutdown_vram_temperature"}

// amdsmiRun is one amd-smi run.
type amdsmiRun struct {
	*call
	v72    bool
	cmd    string
	format string // human, json or csv, from argv as get_output_format reads it
	gpus   []amdGPU
}

// runAMDSMI emulates static --limit and metric --power under --json, and set
// and reset of the power cap.
func runAMDSMI(c *call) int {
	r := &amdsmiRun{call: c}
	switch c.variant {
	case amdsmiVariant71:
	case amdsmiVariant72:
		r.v72 = true
	default:
		return c.unknownVariant()
	}
	argv := amdsmiLowercase(c.args)
	r.format = amdsmiFormat(argv)
	if len(argv) == 0 {
		return c.notEmulated("without a command (the default view)")
	}
	r.cmd = argv[0]
	if r.cmd == "-h" || r.cmd == "--help" {
		fmt.Fprintln(c.stdout, "usage: amd-smi [-h] {static,metric,set,reset} ...  (emulated by fakesmi)")
		return 0
	}
	if !slices.Contains(amdsmiCommands, r.cmd) {
		// amdsmi_cli.py:214 passes the destination as the format, so the
		// message is always the plain one.
		r.format = "human"
		return r.raise("AmdSmiInvalidSubcommandException", -10,
			fmt.Sprintf("AMD-SMI Command '%s' is invalid. Must receive valid AMD-SMI Command first. Run 'amd-smi -h' for more info.", r.cmd))
	}

	modelled := map[string]pyOpt{
		"--help":     {},
		"--gpu":      {nargs: pyOneOrMore, metavar: []string{"GPU"}},
		"--json":     {},
		"--loglevel": {nargs: 1, metavar: []string{"LEVEL"}},
	}
	var spec string
	switch r.cmd {
	case "static":
		spec, modelled["--limit"] = amdsmiStaticSpec, pyOpt{}
	case "metric":
		spec, modelled["--power"] = amdsmiMetricSpec, pyOpt{}
	case "set":
		spec = amdsmiSetSpec
		if r.v72 {
			// parser 7.2:1362: -o PWR_TYPE WATTS, and -S and -F after -c.
			spec = strings.Replace(spec, "-c/--clk-level ", "-c/--clk-level -S/--ptl-status -F/--ptl-format ", 1)
			modelled["--power-cap"] = pyOpt{nargs: 2, metavar: []string{"PWR_TYPE", "WATTS"}}
		} else {
			// parser 7.1:1283: -o WATTS.
			modelled["--power-cap"] = pyOpt{nargs: 1, metavar: []string{"WATTS"}}
		}
	case "reset":
		spec, modelled["--power-cap"] = amdsmiResetSpec, pyOpt{}
	default:
		return c.notEmulated("command " + r.cmd)
	}
	// Subcommand parsers are plain argparse.ArgumentParser (parser 7.2:119):
	// their errors print usage and exit 2.
	p := &pyParser{prog: toolAMDSMI + " " + r.cmd, opts: pyOpts("-h/--help "+spec+" "+amdsmiDeviceSpec+" "+amdsmiModifierSpec, modelled)}
	a, err := p.parse(argv[1:])
	if err != nil {
		return p.fail(c.stderr, err.Error())
	}
	if a.help {
		return p.help(c.stdout)
	}
	if a.unmodelled != "" {
		return c.notEmulated("option " + a.unmodelled)
	}
	gpus, gerr := amdGPUs(c.env)
	if gerr != nil {
		fmt.Fprintf(c.stderr, "fakesmi: %v\n", gerr)
		return 1
	}
	if len(gpus) == 0 {
		return c.notEmulated("on a node without an AMD GPU")
	}
	r.gpus = gpus

	// argparse runs type checks and actions while it parses, in argument
	// order; the exceptions they raise are not caught (amdsmi_cli.py:207-214).
	sel := gpus
	var pwrType string
	var watts int64
	for _, key := range a.order {
		vals := a.values[key]
		switch key {
		case "--gpu":
			var code int
			if sel, code = r.selectGPUs(vals); sel == nil {
				return code
			}
		case "--power-cap":
			if r.cmd != "set" {
				continue
			}
			w := vals[len(vals)-1]
			if r.v72 {
				// _power_cap_options, parser 7.2:296-315.
				if pwrType = vals[0]; pwrType != "ppt0" && pwrType != "ppt1" {
					return r.invalidParameter(pwrType)
				}
				if !pyIsDigit(w) {
					return r.invalidValue(w)
				}
			} else if v, ok := pyInt(w); !pyIsDigit(w) || !ok || v <= 0 {
				// _positive_int, parser 7.1:194-204.
				return r.invalidValue(w)
			}
			watts, _ = pyInt(w)
		}
	}
	if len(a.extras) > 0 {
		// AMDSMIParser.error turns "unrecognized arguments" into an exception
		// (parser 7.2:1651-1667).
		return r.invalidParameter(strings.Join(a.extras, " "))
	}

	switch r.cmd {
	case "static", "metric":
		want := map[string]string{"static": "--limit", "metric": "--power"}[r.cmd]
		if !a.has(want) || r.format != "json" {
			return c.notEmulated(r.cmd + " other than " + want + " --json")
		}
		var data []any
		for _, g := range sel {
			if r.cmd == "static" {
				data = append(data, r.staticLimit(g))
			} else {
				data = append(data, r.metricPower(g))
			}
		}
		// combine_arrays_to_json (logger 7.2:600-624): indent 4.
		fmt.Fprintln(c.stdout, pyJSON(newPyDict("gpu_data", data), 4))
		return 0
	}
	if !a.has("--power-cap") || r.format == "csv" {
		return c.notEmulated(r.cmd + " other than -o")
	}
	for _, g := range sel {
		var out any
		var code int
		switch {
		case r.cmd == "set" && r.v72:
			out, code = r.setPowerCap(g, map[string]int{"ppt0": 0, "ppt1": 1}[pwrType], strings.ToUpper(pwrType)+" ", watts)
		case r.cmd == "set":
			out, code = r.setPowerCap(g, 0, "", watts)
		case r.v72:
			out, code = r.resetPowerCap72(g)
		default:
			out, code = r.resetPowerCap71(g)
		}
		if out == nil {
			return code
		}
		r.printOutput(g, "powercap", out)
	}
	return 0
}

// amdsmiLowercase is amdsmi_cli.py:176-205: long options and plain values are
// lowercased, short options keep their case, and the value of --gpu, --file
// and a few others keeps its case.
func amdsmiLowercase(args []string) []string {
	caseSensitive := []string{"--folder", "--file", "--gpu", "--cpu", "--core", "--profile", "--cper-file"}
	out := make([]string, 0, len(args))
	keep := false
	for _, arg := range args {
		switch {
		case keep:
			out, keep = append(out, arg), false
		case slices.Contains(caseSensitive, arg):
			out, keep = append(out, strings.ToLower(arg)), true
		default:
			name, val, eq := strings.Cut(arg, "=")
			switch {
			case eq && slices.Contains(caseSensitive, name):
				out = append(out, strings.ToLower(name)+"="+val)
			case strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-"):
				out = append(out, strings.ToLower(arg))
			default:
				out = append(out, arg)
			}
		}
	}
	return out
}

// amdsmiFormat is AMDSMIHelpers.get_output_format: json or csv when argv
// names it anywhere, human otherwise.
func amdsmiFormat(argv []string) string {
	switch {
	case slices.Contains(argv, "--json") || slices.Contains(argv, "--j"):
		return "json"
	case slices.Contains(argv, "--csv") || slices.Contains(argv, "--c"):
		return "csv"
	}
	return "human"
}

// raise prints an AmdSmiException nothing catches: Python prints the
// qualified class name and str(e), which follows the output format, on stderr
// without a traceback (sys.tracebacklimit = -1), and exits 1.
func (r *amdsmiRun) raise(class string, code int, msg string) int {
	var s string
	switch r.format {
	case "json":
		s = pyJSON(newPyDict("error", msg, "code", code), 0)
	case "csv":
		s = fmt.Sprintf("error,code\n%s, %d", msg, code)
	default:
		s = fmt.Sprintf("%s Error code: %d", msg, code)
	}
	fmt.Fprintf(r.stderr, "amdsmi_cli_exceptions.%s: %s\n", class, s)
	return 1
}

func (r *amdsmiRun) invalidParameter(arg string) int {
	return r.raise("AmdSmiInvalidParameterException", -2,
		fmt.Sprintf("Parameter '%s' is invalid. Run 'amd-smi %s -h' for more info.", arg, r.cmd))
}

func (r *amdsmiRun) invalidValue(arg string) int {
	return r.raise("AmdSmiInvalidParameterValueException", -5,
		fmt.Sprintf("Value '%s' is not of valid type or format. Run 'amd-smi %s -h' for more info.", arg, r.cmd))
}

// selectGPUs is the -g action (parser 7.2:420-447;
// get_device_handles_from_gpu_selections): all, or GPU indices. A number
// that is no GPU is a device not found; anything else is an invalid value.
// BDF and UUID selections are not modelled. It returns nil and the exit code
// on an error.
func (r *amdsmiRun) selectGPUs(vals []string) ([]amdGPU, int) {
	if slices.Contains(vals, "all") {
		return r.gpus, 0
	}
	var sel []amdGPU
	for _, v := range vals {
		i, err := strconv.Atoi(v)
		switch {
		case !pyIsDigit(v):
			return nil, r.invalidValue(v)
		case err != nil || i >= len(r.gpus):
			return nil, r.raise("AmdSmiDeviceNotFoundException", -3, fmt.Sprintf("Can not find a device: GPU '%s'", v))
		}
		sel = append(sel, r.gpus[i])
	}
	return sel, 0
}

// amdsmiCap is amdsmi_get_power_cap_info in whole watts, as convert_SI_unit
// gives them for integer microwatts: int(float(uW) * 1e-6).
type amdsmiCap struct{ cur, lo, hi, def, defUW int64 }

// capInfo reads power<sensor+1>_cap, _cap_min, _cap_max and _cap_default.
// Sensor 0 is PPT0, sensor 1 is PPT1 (power2_*), present only on the GPUs
// that have it (amdgpu_pm.c:3670-3674).
func capInfo(g amdGPU, sensor int) (amdsmiCap, bool) {
	p := "power" + strconv.Itoa(sensor+1) + "_cap"
	cur, ok1 := g.hwmonInt(p)
	lo, ok2 := g.hwmonInt(p + "_min")
	hi, ok3 := g.hwmonInt(p + "_max")
	def, ok4 := g.hwmonInt(p + "_default")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return amdsmiCap{}, false
	}
	return amdsmiCap{cur: amdsmiW(cur), lo: amdsmiW(lo), hi: amdsmiW(hi), def: amdsmiW(def), defUW: def}, true
}

// amdsmiW is convert_SI_unit(uW, MICRO): the float product truncated, so the
// result matches Python's to the last bit.
func amdsmiW(uw int64) int64 { return int64(float64(uw) * 0.000001) }

// amdsmiUW is convert_SI_unit(W, BASE, MICRO).
func amdsmiUW(w int64) int64 { return int64(float64(w) / 0.000001) }

// supportedSensors is amdsmi_get_supported_power_cap: PPT0 and, when the GPU
// has power2_cap, PPT1.
func supportedSensors(g amdGPU) []int {
	var s []int
	for i := 0; i < 2; i++ {
		if _, ok := g.hwmonInt("power" + strconv.Itoa(i+1) + "_cap"); ok {
			s = append(s, i)
		}
	}
	return s
}

// wattsJSON is unit_format in JSON: {"value": W, "unit": "W"}.
func wattsJSON(w int64) *pyDict { return newPyDict("value", w, "unit", "W") }

// staticLimit is one GPU of static --limit: 7.1 has max_power, min_power and
// socket_power (cmds 7.1:577-723); 7.2 has them per PPT, with PPT1 all N/A
// when the GPU has none (cmds 7.2:615-650, 784-799).
func (r *amdsmiRun) staticLimit(g amdGPU) *pyDict {
	limit := newPyDict()
	if r.v72 {
		ppts := []*pyDict{
			newPyDict("max_power_limit", "N/A", "min_power_limit", "N/A", "socket_power_limit", "N/A"),
			newPyDict("max_power_limit", "N/A", "min_power_limit", "N/A", "socket_power_limit", "N/A"),
		}
		for _, s := range supportedSensors(g) {
			info, ok := capInfo(g, s)
			if !ok {
				break
			}
			ppts[s] = newPyDict("max_power_limit", wattsJSON(info.hi), "min_power_limit", wattsJSON(info.lo), "socket_power_limit", wattsJSON(info.cur))
		}
		limit.set("ppt0", ppts[0])
		limit.set("ppt1", ppts[1])
	} else if info, ok := capInfo(g, 0); ok {
		limit.set("max_power", wattsJSON(info.hi))
		limit.set("min_power", wattsJSON(info.lo))
		limit.set("socket_power", wattsJSON(info.cur))
	} else {
		limit.set("max_power", "N/A")
		limit.set("min_power", "N/A")
		limit.set("socket_power", "N/A")
	}
	for _, k := range amdsmiTempLimits {
		limit.set(k, "N/A")
	}
	if r.v72 {
		limit.set("ptl_state", "N/A")
		limit.set("ptl_format", "N/A")
	}
	return newPyDict("gpu", g.Index, "limit", limit)
}

// metricPower is one GPU of metric --power (cmds 7.2:1932-1982). The socket
// power comes from the hwmon power reading in whole watts (assumed: the
// library reads gpu_metrics, which a text tree has not); the voltages, the
// throttle status and the power management state have no source here and
// print N/A, as when their library calls fail.
func (r *amdsmiRun) metricPower(g amdGPU) *pyDict {
	power := newPyDict("socket_power", "N/A", "gfx_voltage", "N/A", "soc_voltage", "N/A",
		"mem_voltage", "N/A", "throttle_status", "N/A", "power_management", "N/A")
	if uw, _, ok := g.sensorUW(); ok {
		power.set("socket_power", wattsJSON(amdsmiW(uw)))
	}
	return newPyDict("gpu", g.Index, "power", power)
}

// setPowerCap is set -o (cmds 7.2:5055-5102 with typ "PPT0 " or "PPT1 ";
// cmds 7.1:4878-4922 with typ ""). Every outcome is a message and exit 0; only
// an in-range value different from the current cap is written. It returns a
// nil message and the exit code when the run ends instead.
func (r *amdsmiRun) setPowerCap(g amdGPU, sensor int, typ string, watts int64) (any, int) {
	info, ok := capInfo(g, sensor)
	if !ok {
		return fmt.Sprintf("[AMDSMI_STATUS_NOT_SUPPORTED] Unable to set %spower cap to %dW", typ, watts), 0
	}
	inRange := watts >= info.lo && watts <= info.hi
	if r.v72 {
		inRange = inRange && watts > 0
	}
	subject := strings.TrimSpace(typ + "power cap")
	if typ == "" {
		subject = "Power cap"
	}
	switch {
	case watts == info.cur:
		return fmt.Sprintf("%s is already set to %dW", subject, watts), 0
	case info.cur == 0:
		return fmt.Sprintf("Unable to set %spower cap to %dW, current value is %dW", typ, watts, info.cur), 0
	case inRange:
		if err := writeAttr(g.hwmonPath("power"+strconv.Itoa(sensor+1)+"_cap"), strconv.FormatInt(amdsmiUW(watts), 10)); err != nil {
			if code, ok := r.permissionError(err); ok {
				return nil, code
			}
			return fmt.Sprintf("[AMDSMI_STATUS_FILE_ERROR] Unable to set %spower cap to %dW", typ, watts), 0 // status assumed
		}
		return fmt.Sprintf("Successfully set %spower cap to %dW", typ, watts), 0
	}
	// "setting power cap to 0 will return the current power cap so the
	// technical minimum value is 1"
	lo := info.lo
	if lo == 0 {
		lo = 1
	}
	return fmt.Sprintf("Power cap must be between %dW and %dW", lo, info.hi), 0
}

// resetPowerCap71 is reset -o of 7.1 (cmds 7.1:5440-5467): power1_cap back
// to power1_cap_default, unless it is there already.
func (r *amdsmiRun) resetPowerCap71(g amdGPU) (any, int) {
	info, ok := capInfo(g, 0)
	if !ok {
		return "[AMDSMI_STATUS_NOT_SUPPORTED] Unable to reset power cap to default", 0
	}
	if info.cur == info.def {
		return fmt.Sprintf("Power cap is already set to %dW", info.def), 0
	}
	if err := writeAttr(g.hwmonPath("power1_cap"), strconv.FormatInt(amdsmiUW(info.def), 10)); err != nil {
		if code, ok := r.permissionError(err); ok {
			return nil, code
		}
		fmt.Fprintf(r.stderr, "ValueError: Unable to reset power cap to %d on GPU %d\n", info.def, g.Index)
		return nil, 1
	}
	return fmt.Sprintf("Successfully set power cap to %dW", info.def), 0
}

// resetPowerCap72 is reset -o of 7.2 (cmds 7.2:5620-5650): every supported
// PPT back to its default, written as read, one message per PPT.
func (r *amdsmiRun) resetPowerCap72(g amdGPU) (any, int) {
	const unsupported = "[AMDSMI_STATUS_NOT_SUPPORTED] Unable to reset to default power cap"
	const failed = "[AMDSMI_STATUS_NOT_SUPPORTED] Unable to reset cap to default power cap"
	out := newPyDict("ppt0", unsupported, "ppt1", unsupported)
	sensors := supportedSensors(g)
	if len(sensors) == 0 {
		out.set("ppt0", failed)
	}
	for _, s := range sensors {
		key := "ppt" + strconv.Itoa(s)
		info, ok := capInfo(g, s)
		if !ok {
			out.set(key, failed)
			break
		}
		if err := writeAttr(g.hwmonPath("power"+strconv.Itoa(s+1)+"_cap"), strconv.FormatInt(info.defUW, 10)); err != nil {
			if code, ok := r.permissionError(err); ok {
				return nil, code
			}
			out.set(key, "[AMDSMI_STATUS_FILE_ERROR] Unable to reset cap to default power cap") // status assumed
			break
		}
		out.set(key, fmt.Sprintf("Successfully reset power cap to %dW", info.def))
	}
	return out, 0
}

// permissionError is AMDSMI_STATUS_NO_PERM, which the commands turn into an
// uncaught PermissionError.
func (r *amdsmiRun) permissionError(err error) (int, bool) {
	if !errors.Is(err, fs.ErrPermission) {
		return 0, false
	}
	fmt.Fprintln(r.stderr, "PermissionError: Command requires elevation")
	return 1, true
}

// printOutput is store_output then print_output for one GPU: a one-element
// JSON list with indent 4, or the human form, "GPU: 0" then the key
// capitalized and indented by four (logger 7.2:272-350).
func (r *amdsmiRun) printOutput(g amdGPU, key string, value any) {
	out := newPyDict("gpu", g.Index, key, value)
	if r.format == "json" {
		fmt.Fprintln(r.stdout, pyJSON([]any{out}, 4))
		return
	}
	fmt.Fprintln(r.stdout, amdsmiHuman(out))
}

// amdsmiHuman is _convert_json_to_human_readable for flat and nested string
// values: keys uppercased, everything but GPU moved one level in, dumped as
// YAML-like lines, every pair of spaces in a key's indentation doubled.
func amdsmiHuman(out *pyDict) string {
	caps := amdsmiUpper(out)
	top, tabbed := newPyDict(), newPyDict()
	for _, k := range caps.keys {
		if k == "GPU" || k == "CPU" || k == "CORE" {
			top.set(k, caps.vals[k])
		} else {
			tabbed.set(k, caps.vals[k])
		}
	}
	top.set("AMDSMI_SPACING_REMOVAL", tabbed)
	y := strings.ReplaceAll(amdsmiDump(top, 0), "AMDSMI_SPACING_REMOVAL:\n", "")
	y = strings.ReplaceAll(y, "'", "")
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(y, "\n"), "\n") {
		parts := strings.Split(line, ":")
		parts[0] = strings.ReplaceAll(strings.Replace(parts[0], "-", " ", 1), "  ", "    ")
		b.WriteString(strings.Join(parts, ":") + "\n")
	}
	return b.String()
}

func amdsmiUpper(d *pyDict) *pyDict {
	out := newPyDict()
	for _, k := range d.keys {
		v := d.vals[k]
		if sub, ok := v.(*pyDict); ok {
			v = amdsmiUpper(sub)
		}
		out.set(strings.ToUpper(k), v)
	}
	return out
}

// amdsmiDump is custom_dump (logger 7.2:332-350) for dicts and scalars.
func amdsmiDump(d *pyDict, indent int) string {
	var b strings.Builder
	pad := strings.Repeat("  ", indent)
	for _, k := range d.keys {
		if sub, ok := d.vals[k].(*pyDict); ok {
			b.WriteString(pad + k + ":\n" + amdsmiDump(sub, indent+1))
			continue
		}
		b.WriteString(fmt.Sprintf("%s%s: %v\n", pad, k, d.vals[k]))
	}
	return b.String()
}
