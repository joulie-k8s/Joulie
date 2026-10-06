package smi

import (
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// rocm-smi as python_smi_tools/rocm_smi.py of rocm_smi_lib behaves at commit
// 323ab1105dce, the head of its develop branch. The flags and the exit logic
// used here hold from rocm-5.4.0 on. Line numbers below are that file's at
// that commit.

const (
	rocmVariantCurrent = "current"
	rocmVariantLegacy  = "legacy-agent-contract"
)

// rocmOptionSpec is every option of rocm_smi.py in registration order
// (3821-3964), so prefix abbreviation resolves and collides as argparse
// makes it (3804-3806). rocmOptions adds the legacy --showpowercap.
const rocmOptionSpec = "-h/--help -V/--version -d/--device --alldevices --showhw -a/--showallinfo " +
	"-i/--showid -v/--showvbios -e/--showevents --showdriverversion --showtempgraph --showfwinfo " +
	"--showmclkrange --showmemvendor --showsclkrange --showproductname --showserial --showuniqueid " +
	"--showvoltagerange --showbus --showpagesinfo --showpendingpages --showretiredpages " +
	"--showunreservablepages -f/--showfan -P/--showpower -t/--showtemp -u/--showuse --showmemuse " +
	"--showvoltage -b/--showbw -c/--showclocks -g/--showgpuclocks -l/--showprofile -M/--showmaxpower " +
	"-m/--showmemoverdrive -o/--showoverdrive -p/--showperflevel -S/--showclkvolt -s/--showclkfrq " +
	"--showmeminfo --showpids --showpidgpus --showreplaycount --showrasinfo --showvc --showxgmierr " +
	"--showtopo --showtopoaccess --showtopoweight --showtopohops --showtopotype --showtoponuma " +
	"--showenergycounter --shownodesbw --showcomputepartition --showmemorypartition -r/--resetclocks " +
	"--resetfans --resetprofile --resetpoweroverdrive --resetxgmierr --resetperfdeterminism " +
	"--resetcomputepartition --resetmemorypartition --setclock --setsclk --setmclk --setpcie " +
	"--setslevel --setmlevel --setvc --setsrange --setextremum --setmrange --setfan --setperflevel " +
	"--setoverdrive --setmemoverdrive --setpoweroverdrive --setprofile --setperfdeterminism " +
	"--setcomputepartition --setmemorypartition --rasenable --rasdisable --rasinject --gpureset " +
	"--load --save --autorespond --loglevel --json --csv"

// rocmModelled are the options fakesmi models: the agent's and their
// neighbours on the same code paths.
var rocmModelled = map[string]pyOpt{
	"--help":                {},
	"--device":              {nargs: pyOneOrMore, metavar: []string{"DEVICE"}, isInt: true},
	"--showproductname":     {},
	"--showpower":           {},
	"--showmaxpower":        {},
	"--showpowercap":        {},
	"--setpoweroverdrive":   {nargs: 1, metavar: []string{"WATTS"}},
	"--resetpoweroverdrive": {},
	"--autorespond":         {nargs: 1, metavar: []string{"RESPONSE"}},
	"--loglevel":            {nargs: 1, metavar: []string{"LEVEL"}},
	"--json":                {},
}

// rocmOptions is the parser of a variant. The legacy variant registers
// --showpowercap after --showmaxpower (assumed: no release has the flag).
func rocmOptions(legacy bool) []pyOpt {
	spec := rocmOptionSpec
	if legacy {
		spec = strings.Replace(spec, "-M/--showmaxpower ", "-M/--showmaxpower --showpowercap ", 1)
	}
	return pyOpts(spec, rocmModelled)
}

const (
	rocmAppWidth   = 90                                   // appWidth, 55
	rocmHeader     = " ROCm System Management Interface " // headerString, 52
	rocmFooter     = " End of ROCm SMI Log "              // footerString, 53
	rocmNoDevice   = -1                                   // printLog's device None
	rocmAMDVendor  = "Advanced Micro Devices, Inc. [AMD/ATI]"
	rocmSecondary  = "N/A (Secondary die)"
	rocmPrimaryMsg = "\n\t\tPrimary die (usually one above or below the secondary) shows total (primary + secondary) socket power information"
)

// rocmOutOfSpecWarning is the text confirmOutOfSpecWarning prints (3545-3560).
const rocmOutOfSpecWarning = "\n          ******WARNING******\n\n" +
	"          Operating your AMD GPU outside of official AMD specifications or outside of\n" +
	"          factory settings, including but not limited to the conducting of overclocking,\n" +
	"          over-volting or under-volting (including use of this interface software,\n" +
	"          even if such software has been directly or indirectly provided by AMD or otherwise\n" +
	"          affiliated in any way with AMD), may cause damage to your AMD GPU, system components\n" +
	"          and/or result in system failure, as well as cause other problems.\n" +
	"          DAMAGES CAUSED BY USE OF YOUR AMD GPU OUTSIDE OF OFFICIAL AMD SPECIFICATIONS OR\n" +
	"          OUTSIDE OF FACTORY SETTINGS ARE NOT COVERED UNDER ANY AMD PRODUCT WARRANTY AND\n" +
	"          MAY NOT BE COVERED BY YOUR BOARD OR SYSTEM MANUFACTURER'S WARRANTY.\n" +
	"          Please use this utility with caution.\n          "

// Python logging levels.
const (
	pyDebug   = 10
	pyInfo    = 20
	pyWarning = 30
	pyError   = 40
)

var pyLevelNames = map[int]string{0: "NOTSET", pyDebug: "DEBUG", pyInfo: "INFO", pyWarning: "WARNING", pyError: "ERROR", 50: "CRITICAL"}

// pyLogLevel is getattr(logging, name.upper(), logging.WARNING) for the names
// that are levels (3981-3983).
func pyLogLevel(name string) int {
	switch strings.ToUpper(name) {
	case "NOTSET":
		return 0
	case "DEBUG":
		return pyDebug
	case "INFO":
		return pyInfo
	case "ERROR":
		return pyError
	case "CRITICAL", "FATAL":
		return 50
	}
	return pyWarning
}

// rocmRun is one rocm-smi run: the global state rocm_smi.py keeps in
// PRINT_JSON, JSON_DATA and RETCODE, and its logging level.
type rocmRun struct {
	*call
	gpus    []amdGPU
	json    bool
	level   int
	retcode int
	cards   []int           // JSON_DATA keys in insertion order
	data    map[int]*pyDict // JSON_DATA['card<i>']
}

// runRocmSMI follows rocm_smi.py's main (3803-4288) for the modelled options.
func runRocmSMI(c *call) int {
	legacy := false
	switch c.variant {
	case rocmVariantCurrent:
	case rocmVariantLegacy:
		legacy = true
	default:
		return c.unknownVariant()
	}
	p := &pyParser{prog: toolRocmSMI, opts: rocmOptions(legacy)}
	a, err := p.parse(c.args)
	if err != nil {
		return p.fail(c.stderr, err.Error())
	}
	if a.help {
		return p.help(c.stdout)
	}
	if a.unmodelled != "" {
		return c.notEmulated("option " + a.unmodelled)
	}
	if len(a.extras) > 0 {
		// parse_args (3966) on an unknown flag, such as --showpowercap on
		// every real release: usage on stderr, empty stdout, exit 2 (D1).
		return p.fail(c.stderr, "unrecognized arguments: "+strings.Join(a.extras, " "))
	}
	r := &rocmRun{call: c, json: a.has("--json"), level: pyWarning, data: map[int]*pyDict{}}

	gpus, gerr := amdGPUs(c.env)
	if gerr != nil {
		fmt.Fprintf(c.stderr, "fakesmi: %v\n", gerr)
		return 1
	}
	if len(gpus) == 0 {
		// initializeRsmi (3585-3600) without amdgpu. logging is not
		// configured yet, so Python's default format applies; exit(0).
		fmt.Fprintln(c.stderr, "ERROR:root:Driver not initialized (amdgpu not found in modules)")
		return 0
	}
	r.gpus = gpus
	logLevel, hasLevel := a.value("--loglevel")
	if hasLevel {
		r.level = pyLogLevel(logLevel)
	}

	var devs []int
	if ds, ok := a.values["--device"]; ok {
		for _, s := range ds {
			d, _ := pyInt(s)
			if d < 0 || d >= int64(len(gpus)) {
				// 3997-4000: a warning and sys.exit(), which exits 0.
				r.log(pyWarning, fmt.Sprintf("No such device card%d", d))
				return 0
			}
			if !slices.Contains(devs, int(d)) {
				devs = append(devs, int(d))
			}
		}
	} else {
		for i := range gpus {
			devs = append(devs, i)
		}
	}
	if len(c.args) == 0 || (len(c.args) == 1 && r.json) {
		return c.notEmulated("concise view (no option, or --json alone)")
	}

	if r.json {
		for _, d := range devs {
			r.cards = append(r.cards, d)
			r.data[d] = newPyDict()
		}
	} else {
		fmt.Fprint(c.stdout, "\n\n") // print('\n')
	}
	r.spacer(rocmHeader)
	if a.has("--showmaxpower") {
		r.showMaxPower(devs)
	}
	if legacy && a.has("--showpowercap") {
		r.showPowerCap(devs)
	}
	if a.has("--showpower") {
		r.showPower(devs)
	}
	if a.has("--showproductname") {
		r.showProduct(devs)
	}
	auto, _ := a.value("--autorespond")
	if v, _ := a.value("--setpoweroverdrive"); v != "" {
		if code, exited := r.setPowerOverDrive(devs, v, auto); exited {
			return code
		}
	}
	if a.has("--resetpoweroverdrive") {
		if code, exited := r.setPowerOverDrive(devs, "0", auto); exited {
			return code
		}
	}

	if r.retcode != 0 && !r.json {
		r.log(pyDebug, " \t\t One or more commands failed.")
	}
	// 4250-4255: every RETCODE=1 path exits 0 unless --loglevel names
	// another level than warning (D2).
	if !hasLevel || pyLogLevel(logLevel) == pyWarning {
		r.retcode = 0
	}
	if r.json {
		out := newPyDict()
		for _, d := range r.cards {
			if len(r.data[d].keys) > 0 {
				out.set("card"+strconv.Itoa(d), r.data[d])
			}
		}
		if len(out.keys) == 0 {
			r.log(pyWarning, "No JSON data to report")
			return r.retcode
		}
		fmt.Fprintln(c.stdout, pyJSON(out, 0))
	}
	r.spacer(rocmFooter)
	return r.retcode
}

// log is Python logging under basicConfig(format='%(levelname)s: %(message)s')
// (3979), on stderr.
func (r *rocmRun) log(level int, msg string) {
	if level >= r.level {
		fmt.Fprintf(r.stderr, "%s: %s\n", pyLevelNames[level], msg)
	}
}

// printLog is printLog (839-880): "GPU[i]\t\t: metric: value" on stdout, or
// under --json an entry of card<i> parsed back from that text by formatJson.
// A missing value is Python's None.
func (r *rocmRun) printLog(dev int, metric string, value ...string) {
	line := metric
	if len(value) > 0 {
		line = metric + ": " + value[0]
	}
	if r.json {
		if dev != rocmNoDevice {
			r.formatJSON(dev, line)
		}
		return
	}
	if dev == rocmNoDevice {
		// 'GPU[None]\t\t: ' + line, cut after the first colon: line itself.
		fmt.Fprintln(r.stdout, line)
		return
	}
	fmt.Fprintf(r.stdout, "GPU[%d]\t\t: %s\n", dev, line)
}

// formatJSON is formatJson (84-99): each line with a colon becomes a key and
// a stripped string value, split at ": ". A later value for a key replaces the
// earlier one in place.
func (r *rocmRun) formatJSON(dev int, log string) {
	d := r.data[dev]
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.Split(line, ": ")
		if len(parts) < 2 {
			continue // rocm_smi.py raises IndexError here
		}
		d.set(parts[0], strings.TrimSpace(parts[1]))
	}
}

// printErrLog is printErrLog (777-790): an ERROR log line per line of err,
// demoted to DEBUG under --json.
func (r *rocmRun) printErrLog(dev int, err string) {
	for _, line := range strings.Split(err, "\n") {
		s := fmt.Sprintf("GPU[%d]\t: %s", dev, line)
		if r.json {
			r.log(pyDebug, s)
		} else {
			r.log(pyError, s)
		}
	}
}

// spacer is printLogSpacer (912-941): the title centred in '=' over appWidth,
// or a full line of '=' without a title. Nothing under --json.
func (r *rocmRun) spacer(title string) {
	if r.json {
		return
	}
	if title == "" {
		fmt.Fprintln(r.stdout, strings.Repeat("=", rocmAppWidth))
		return
	}
	if len(title)%2 == 1 {
		title += "="
	}
	pad := strings.Repeat("=", max(0, (rocmAppWidth-len(title))/2))
	fmt.Fprintln(r.stdout, pad+title+pad)
}

// showMaxPower is -M (2462-2475). It reports the current cap, floored to
// whole watts as a float (getMaxPower, 414-425), not the top of the range
// (D4): 300500000 uW prints "300.0".
func (r *rocmRun) showMaxPower(devs []int) {
	r.spacer(" Power Cap ")
	for _, d := range devs {
		if uw, ok := r.gpus[d].hwmonInt("power1_cap"); ok {
			r.printLog(d, "Max Graphics Package Power (W)", pyFloat(math.Floor(float64(uw)/1e6)))
		} else {
			r.printLog(d, "Max Graphics Package Power Unsupported")
		}
	}
	r.spacer("")
}

// showPowerCap is --showpowercap of the legacy-agent-contract variant
// (assumed, no release has it): the keys the agent reads (listAmdDevices in
// cmd/agent), in W as float strings. Max is the top of the range here; when
// -M also ran, this later value replaces its floored cap.
func (r *rocmRun) showPowerCap(devs []int) {
	r.spacer(" Power Cap ")
	for _, d := range devs {
		g := r.gpus[d]
		for _, f := range [][2]string{
			{"Max Graphics Package Power (W)", "power1_cap_max"},
			{"Min Graphics Package Power (W)", "power1_cap_min"},
			{"Current Power Cap (W)", "power1_cap"},
		} {
			if uw, ok := g.hwmonInt(f[1]); ok {
				r.printLog(d, f[0], pyFloat(float64(uw)/1e6))
			} else {
				r.printLog(d, f[0], "N/A")
			}
		}
		switch uw, _, ok := g.sensorUW(); {
		case ok:
			r.printLog(d, "Average Graphics Package Power (W)", pyFloat(float64(uw)/1e6))
		case g.secondary():
			r.printLog(d, "Average Graphics Package Power (W)", rocmSecondary)
		default:
			r.printLog(d, "Average Graphics Package Power (W)", "N/A")
		}
	}
	r.spacer("")
}

// showPower is -P (2690-2711): each device's own average or current socket
// power in W as a float string. A device without a reading prints N/A
// (Secondary die) when it is a secondary die, whose primary's reading covers
// the package; it is not added to the primary here.
func (r *rocmRun) showPower(devs []int) {
	secondaryPresent := false
	r.spacer(" Power Consumption ")
	for _, d := range devs {
		uw, current, ok := r.gpus[d].sensorUW()
		switch {
		case ok:
			kind := "Average"
			if current {
				kind = "Current Socket"
			}
			r.printLog(d, kind+" Graphics Package Power (W)", pyFloat(float64(uw)/1e6))
		case r.gpus[d].secondary():
			r.printLog(d, "Average Graphics Package Power (W)", rocmSecondary)
			secondaryPresent = true
		default:
			r.printErrLog(d, "Unable to get Average or Current Socket Graphics Package Power Consumption")
		}
	}
	if secondaryPresent {
		r.printLog(rocmNoDevice, rocmPrimaryMsg)
	}
	r.spacer("")
}

// showProduct is --showproductname (showProduct, 2749-2770), whose keys and
// order the JSON keeps. Card SKU is the middle segment of vbios_version when
// it has exactly two dashes; the agent reads it as the product (D4).
func (r *rocmRun) showProduct(devs []int) {
	r.spacer(" Product Info ")
	for _, d := range devs {
		g := r.gpus[d]
		vbios, ok := g.devAttr("vbios_version")
		if !ok || vbios == "" {
			vbios = "N/A"
		}
		sku := "N/A"
		if parts := strings.Split(vbios, "-"); len(parts) == 3 && len(parts[1]) > 1 {
			sku = parts[1]
		}
		r.printLog(d, "Card Series", "\t\t"+rocmSeries(g))
		r.printLog(d, "Card Model", "\t\t"+rocmHexAttr(g, "device", 0))
		r.printLog(d, "Card Vendor", "\t\t"+rocmAMDVendor)
		r.printLog(d, "Card SKU", "\t\t"+sku)
		r.printLog(d, "Subsystem ID", "\t"+rocmHexAttr(g, "subsystem_device", 4))
		r.printLog(d, "Device Rev", "\t\t"+rocmHexAttr(g, "revision", 2))
		r.printLog(d, "Node ID", "\t\t"+strconv.Itoa(g.Index+1))
		r.printLog(d, "GUID", "\t\t"+rocmGUID(g))
		r.printLog(d, "GFX Version", "\t\t"+rocmGFXVersion(g))
	}
	r.spacer("")
}

// rocmSeries is Card Series: the marketing name rocm_smi_lib looks up,
// assumed equal to the device's product_name attribute.
func rocmSeries(g amdGPU) string {
	if s, ok := g.devAttr("product_name"); ok && s != "" {
		return s
	}
	return "N/A"
}

// rocmHexAttr is a PCI ID attribute as rocm-smi prints it: hex() of a C
// short, zero-padded to width digits by padHexValue (3665-3676) when width is
// set. A subsystem name lookup in pci.ids is not modelled.
func rocmHexAttr(g amdGPU, name string, width int) string {
	s, ok := g.devAttr(name)
	if !ok {
		return "N/A"
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return "N/A"
	}
	h := pyHex(v)
	if width > 1 && strings.HasPrefix(h, "0x") && len(h)-2 < width {
		h = "0x" + strings.Repeat("0", width-(len(h)-2)) + h[2:]
	}
	return h
}

// rocmGUID is a synthetic KFD gpu_id: a 16-bit hash, as KFD's is, of the
// device's PCI address. The Node ID next to it is synthetic too (index + 1).
// The agent reads neither.
func rocmGUID(g amdGPU) string {
	addr := filepath.Base(g.Dev)
	if real, err := filepath.EvalSymlinks(g.Dev); err == nil {
		addr = filepath.Base(real)
	}
	h := fnv.New32a()
	io.WriteString(h, addr)
	id := h.Sum32() & 0xffff
	if id == 0 {
		id = 1
	}
	return strconv.FormatUint(uint64(id), 10)
}

// rocmGFXVersion is the KFD target from state/amdgpu/card<N>/gfx_version, or
// N/A when the state has none.
func rocmGFXVersion(g amdGPU) string {
	if s, err := readAttr(filepath.Join(g.State, amdgpuGFXVersionFile)); err == nil && s != "" {
		return s
	}
	return "N/A"
}

// setPowerOverDrive is setPowerOverDrive (1659-1757), and value "0" is
// --resetpoweroverdrive (1073-1078). It returns exited when the prompt ends
// the program; otherwise failures only set RETCODE, which main then resets
// (D2).
func (r *rocmRun) setPowerOverDrive(devs []int, value, auto string) (code int, exited bool) {
	w, ok := pyInt(value)
	if !ok {
		r.printLog(rocmNoDevice, "Unable to set Power OverDrive")
		r.log(pyError, value+" is not an integer")
		r.retcode = 1
		return 0, false
	}
	if w == 0 {
		r.spacer(" Reset GPU Power OverDrive ")
	} else {
		r.spacer(" Set GPU Power OverDrive ")
	}
	confirmed := false
	for _, d := range devs {
		g := r.gpus[d]
		if g.secondary() {
			// 1687-1689: a secondary die has no power management.
			r.log(pyDebug, "Unavailable for secondary die.")
			continue
		}
		cur, ok := g.hwmonInt("power1_cap")
		if !ok {
			r.log(pyDebug, "Unable to retireive current power cap.") // sic
		}
		def, ok := g.hwmonInt("power1_cap_default")
		if !ok {
			// rocm-smi then resets the cap to learn the default; rendered
			// trees always have power1_cap_default, so that is not modelled.
			r.log(pyDebug, "Unable to retrieve default power cap; retrieving via reset.")
		}
		newCap := w * 1000000
		if w == 0 {
			newCap = def
		}
		lo, okLo := g.hwmonInt("power1_cap_min")
		hi, okHi := g.hwmonInt("power1_cap_max")
		if !okLo || !okHi {
			r.printErrLog(d, "Unable to parse Power OverDrive range")
			r.retcode = 1
			continue
		}
		if float64(w) > float64(hi)/1e6 {
			r.printErrLog(d, "Unable to set Power OverDrive")
			r.log(pyError, fmt.Sprintf("GPU[%d]\t\t: Value cannot be greater than: %dW ", d, int64(float64(hi)/1e6)))
			r.retcode = 1
			continue
		}
		if float64(w) < float64(lo)/1e6 {
			r.printErrLog(d, "Unable to set Power OverDrive")
			r.log(pyError, fmt.Sprintf("GPU[%d]\t\t: Value cannot be less than: %dW ", d, int64(float64(lo)/1e6)))
			r.retcode = 1
			continue
		}
		if newCap == cur {
			r.printLog(d, "Max power was already at: "+pyFloat(float64(newCap)/1e6)+"W")
		}
		cur = max(cur, def)
		if !confirmed && newCap > cur {
			// 1729-1734: above max(current, default) asks first (D3).
			if code, exited := r.confirm(auto); exited {
				return code, true
			}
			confirmed = true
		}
		if err := writeAttr(g.hwmonPath("power1_cap"), strconv.FormatInt(newCap, 10)); err != nil {
			if w == 0 {
				r.printErrLog(d, "Unable to reset Power OverDrive to default")
			} else {
				r.printErrLog(d, "Unable to set Power OverDrive to "+value+"W")
			}
			continue
		}
		if r.json {
			continue
		}
		got, _ := g.hwmonInt("power1_cap")
		switch {
		case w == 0:
			r.printLog(d, fmt.Sprintf("Successfully reset Power OverDrive to: %dW", int64(float64(got)/1e6)))
		case got == newCap:
			r.printLog(d, "Successfully set power to: "+value+"W")
		default:
			r.printErrLog(d, fmt.Sprintf("Unable set power to: %sW, current value is %dW", value, int64(float64(got)/1e6)))
		}
	}
	r.spacer("")
	return 0, false
}

// confirm is confirmOutOfSpecWarning (3545-3570): the warning, then a prompt
// read from stdin unless --autorespond answered it. Only yes, in five
// spellings, goes on. The agent passes no --autorespond and exec gives it an
// empty stdin, so input() raises EOFError and the run exits 1 (D3). The
// traceback is abridged.
func (r *rocmRun) confirm(auto string) (code int, exited bool) {
	fmt.Fprintln(r.stdout, rocmOutOfSpecWarning)
	answer := auto
	if auto == "" {
		fmt.Fprint(r.stdout, "Do you accept these terms? [y/N] ")
		line, ok := pyInput(r.stdin)
		if !ok {
			fmt.Fprint(r.stderr, "Traceback (most recent call last):\n  File \"rocm_smi.py\", in confirmOutOfSpecWarning\nEOFError: EOF when reading a line\n")
			return 1, true
		}
		answer = line
	}
	switch answer {
	case "Yes", "yes", "y", "Y", "YES":
		return 0, false
	}
	fmt.Fprintln(r.stderr, "Confirmation not given. Exiting without setting value") // sys.exit(msg): stderr, exit 1
	return 1, true
}

// pyInput is input() on a pipe: one line without its newline. ok is false at
// EOF before any byte, where input() raises EOFError.
func pyInput(in io.Reader) (string, bool) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return string(line), true
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return string(line), len(line) > 0
		}
	}
}
