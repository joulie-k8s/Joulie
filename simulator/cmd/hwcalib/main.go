// Command hwcalib records a calibration sweep on a node and fits a hardware
// profile to it (package calib):
//
//	hwcalib sweep [-node NODE] [-util LIST] [-freq-max-mhz LIST]
//	              [-power-limit-w LIST] [-write-limits] [-hold DURATION]
//	              [-period DURATION] [-duty-period DURATION] [-sys-root DIR]
//	              [-proc-root DIR] [-out FILE]
//	hwcalib fit -profile NAME [-node NODE] [-format otlp|csv] [-out-dir DIR]
//	            [-name NAME] [-substep DURATION] FILE
//
// sweep runs on the node itself, as root, since energy_uj is mode 0400: it
// holds the CPUs at each load of -util (and, with -write-limits, at each
// scaling_max_freq ceiling of -freq-max-mhz and each package power limit of
// -power-limit-w, in a full grid) for -hold, sampling every -period, and
// writes the sweep as OTLP JSON Lines to -out ("-", the default, for
// standard output; an existing file is never replaced). The load is one
// thread per CPU, pinned to it, busy for the load's share of every
// -duty-period. Every file it wrote is put back when it ends, also on
// SIGINT, SIGTERM or SIGHUP (under nohup a hangup stays ignored and the
// sweep goes on). -sys-root and -proc-root point it at another tree,
// such as an emulated node's, for the files; the load still runs on this
// machine's CPUs.
//
// fit reads the sweep in FILE ("-" for standard input): OTLP JSON Lines,
// one MetricsData per line, or with -format csv the CSV form. -node picks
// the resource whose k8s.node.name or host.name it names; a file of one node
// needs none. -profile is the profile the node is described by, a builtin
// or a local one in SIM_PROFILES_DIR: it gives the topology and settling
// times the fit needs and the values it keeps where the sweep says nothing.
// -substep is the fixed physics substep of a simulated source, such as
// 100ms, and zero for hardware.
//
// The fit is printed, and the fitted facts are written as a local profile
// NAME.yaml (default <profile>-measured) that extends the base profile,
// into -out-dir or else SIM_PROFILES_DIR. A directory inside the repository
// is refused, with no override, since a measured profile may name the
// machine it was measured on; an existing file is never replaced. The new
// profile is loaded back with the others of its directory before hwcalib
// reports success, and removed when it does not load.
//
// The exit status is 0 on success, 1 when the sweep cannot be read or
// fitted or the profile cannot be written, or when sweep cannot read the
// host, write a file or finish (the lines written stay whole; a file with
// no sample is removed), and 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/matbun/joulie/simulator/pkg/calib"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

const usage = `usage: hwcalib fit -profile NAME [-node NODE] [-format otlp|csv] [-out-dir DIR] [-name NAME] [-substep DURATION] FILE
       hwcalib sweep [-node NODE] [-util LIST] [-freq-max-mhz LIST] [-power-limit-w LIST] [-write-limits] [-hold DURATION] [-period DURATION] [-duty-period DURATION] [-sys-root DIR] [-proc-root DIR] [-out FILE]`

// envProfilesDir is the simulator's directory of local profiles: where the
// base profile may live and where the fit goes without -out-dir.
const envProfilesDir = "SIM_PROFILES_DIR"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is the command with its arguments after the program name; it returns
// the exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "fit":
	case "sweep":
		return runSweep(args, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "hwcalib: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	flags := flag.NewFlagSet("hwcalib fit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "base hardware profile of the node (required)")
	node := flags.String("node", "", "node whose resource to read, by k8s.node.name or host.name")
	format := flags.String("format", "otlp", "input format: otlp (JSON Lines of MetricsData) or csv")
	outDir := flags.String("out-dir", "", "directory to write the fitted profile into (default $"+envProfilesDir+")")
	name := flags.String("name", "", "name of the fitted profile (default <profile>-measured)")
	substep := flags.Duration("substep", 0, "fixed physics substep of a simulated source, such as 100ms; 0 for hardware")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "hwcalib fit: "+format+"\n%s\n", append(a, usage)...)
		return 2
	}
	switch {
	case flags.NArg() != 1:
		return usageErr("want one input file, got %d arguments", flags.NArg())
	case *profile == "":
		return usageErr("-profile is required")
	case *format != "otlp" && *format != "csv":
		return usageErr("-format %q is neither otlp nor csv", *format)
	case *substep < 0:
		return usageErr("-substep %v is negative", *substep)
	}
	profilesDir := strings.TrimSpace(getenv(envProfilesDir))
	dir := *outDir
	if dir == "" {
		dir = profilesDir
	}
	if dir == "" {
		return usageErr("no -out-dir, and %s is unset", envProfilesDir)
	}
	if *name == "" {
		*name = *profile + "-measured"
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "hwcalib: %v\n", err)
		return 1
	}
	// Refuse a repository path before any work, so a long fit is never
	// thrown away at the end.
	if root, in, err := calib.InRepository(dir); err != nil {
		return fail(err)
	} else if in {
		return fail(fmt.Errorf("refusing to write into %s: it is inside the repository at %s, and a measured profile stays outside it (%s)", dir, root, envProfilesDir))
	}

	profiles, _, err := hwemu.LoadProfileSet(profilesDir)
	if err != nil {
		return fail(fmt.Errorf("%s=%q: %w", envProfilesDir, profilesDir, err))
	}
	base, ok := profiles[*profile]
	if !ok {
		names := make([]string, 0, len(profiles))
		for n := range profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return fail(fmt.Errorf("no profile %q; have %s", *profile, strings.Join(names, ", ")))
	}

	input := flags.Arg(0)
	d, err := readSweep(input, *format, *node, stdin)
	if err != nil {
		return fail(fmt.Errorf("%s: %w", input, err))
	}
	r, err := calib.Fit(d, base, calib.Options{Substep: *substep})
	if r != nil {
		report(stdout, r)
	}
	if err != nil {
		return fail(err)
	}

	ref := "hwcalib fit of " + filepath.Base(input)
	if input == "-" {
		ref = "hwcalib fit of standard input"
	}
	if d.Node != "" {
		ref += ", node " + d.Node
	}
	note := fmt.Sprintf("%d setpoint segments, %d with a steady window", r.Segments, r.Steady)
	if *substep > 0 {
		note += fmt.Sprintf("; settling corrected for a %v substep", *substep)
	}
	b, err := calib.LocalProfile(r, *name, ref, note)
	if err != nil {
		return fail(err)
	}
	path, err := calib.WriteProfile(dir, *name, b)
	if err != nil {
		return fail(err)
	}
	if loaded, _, err := hwemu.LoadProfileSet(dir); err != nil || loaded[*name] == nil {
		os.Remove(path)
		if err == nil {
			err = fmt.Errorf("profile %s did not load", *name)
		}
		return fail(fmt.Errorf("the fitted profile does not load beside the others in %s, so it was removed (a local base profile must be in the same directory): %w", dir, err))
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return 0
}

// readSweep reads the sweep in path, or standard input for "-".
func readSweep(path, format, node string, stdin io.Reader) (*calib.Data, error) {
	in := stdin
	if path != "-" {
		fh, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer fh.Close()
		in = fh
	}
	if format == "csv" {
		d, err := calib.ReadCSV(in)
		if err != nil {
			return nil, err
		}
		d.Node = node
		return d, nil
	}
	return calib.ReadOTLP(in, node)
}

// report prints the fit: one row per parameter, then the notes.
func report(w io.Writer, r *calib.Result) {
	node := r.Node
	if node == "" {
		node = "(unnamed)"
	}
	fmt.Fprintf(w, "node %s, base profile %s: %d segments, %d with a steady window\n", node, r.Base, r.Segments, r.Steady)
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PARAMETER\tBASE\tFITTED\tUNIT\tFIT\tN\tNOTE")
	for _, p := range r.Params {
		base, fitted, fit, n, note := num(p.Base), "-", "-", "-", ""
		if p.Fitted {
			fitted, fit, n = num(p.Value), p.Fit, strconv.Itoa(p.N)
			if !p.Written {
				note = "reported only"
			}
		} else {
			note = "not fitted: " + p.Reason
		}
		if !p.Written && p.Base == 0 {
			base = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Path, base, fitted, p.Unit, fit, n, note)
	}
	tw.Flush()
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

// num prints a value with at most four digits after the point.
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// runSweep is hwcalib sweep with its arguments after the command; it
// returns the exit status.
func runSweep(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hwcalib sweep", flag.ContinueOnError)
	flags.SetOutput(stderr)
	node := flags.String("node", "", "node name, for host.name and service.instance.id (default the host name)")
	utils := flags.String("util", "0,0.2,0.4,0.6,0.8,1", "comma-separated CPU loads, 0 to 1")
	freqs := flags.String("freq-max-mhz", "", "comma-separated scaling_max_freq ceilings in MHz (needs -write-limits)")
	limits := flags.String("power-limit-w", "", "comma-separated package power limits in W (needs -write-limits)")
	writeLimits := flags.Bool("write-limits", false, "write scaling_max_freq and package power limits; they are put back at the end")
	hold := flags.Duration("hold", 30*time.Second, "time each setpoint is held")
	period := flags.Duration("period", 2*time.Second, "sample period")
	duty := flags.Duration("duty-period", 100*time.Millisecond, "period of the duty-cycled load")
	sysRoot := flags.String("sys-root", "/sys", "sysfs root to read and write")
	procRoot := flags.String("proc-root", "/proc", "procfs root, for stat and cpuinfo")
	out := flags.String("out", "-", "file to write the sweep to, - for standard output; an existing file is never replaced")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "hwcalib sweep: "+format+"\n%s\n", append(a, usage)...)
		return 2
	}
	if flags.NArg() != 0 {
		return usageErr("unexpected arguments %q", flags.Args())
	}
	us, err := floatList(*utils, 1)
	if err != nil {
		return usageErr("-util %q: %v", *utils, err)
	}
	if len(us) == 0 {
		return usageErr("-util is empty: want a comma-separated list of loads")
	}
	fs, err := floatList(*freqs, 1e6)
	if err != nil {
		return usageErr("-freq-max-mhz %q: %v", *freqs, err)
	}
	ls, err := floatList(*limits, 1)
	if err != nil {
		return usageErr("-power-limit-w %q: %v", *limits, err)
	}
	if *duty <= 0 {
		return usageErr("-duty-period %v is not positive", *duty)
	}
	if *node == "" {
		h, err := os.Hostname()
		if err != nil {
			return usageErr("no -node, and the host name is unknown: %v", err)
		}
		*node = h
	}
	o := calib.SweepOptions{
		SysRoot: *sysRoot, ProcRoot: *procRoot, Node: *node,
		Steps: calib.SweepGrid(us, fs, ls), Hold: *hold, Period: *period,
		WriteLimits: *writeLimits, DutyPeriod: *duty, Log: stderr,
	}
	if err := o.Validate(); err != nil {
		return usageErr("%v", err)
	}

	name := "standard output"
	o.Out = stdout
	if *out != "-" {
		fh, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "hwcalib sweep: %v\n", err)
			return 1
		}
		defer fh.Close()
		o.Out, name = fh, *out
	}
	ctx, stop := signal.NotifyContext(context.Background(), sweepSignals()...)
	defer stop()
	fmt.Fprintf(stderr, "hwcalib sweep: %d setpoints of %v, about %v\n", len(o.Steps), *hold, time.Duration(len(o.Steps))*(*hold))
	n, err := calib.Sweep(ctx, o)
	if err != nil {
		if n == 0 && *out != "-" {
			os.Remove(*out)
			fmt.Fprintf(stderr, "hwcalib sweep: %v (no sample written; %s removed)\n", err, *out)
			return 1
		}
		fmt.Fprintf(stderr, "hwcalib sweep: %v (%d samples written to %s)\n", err, n, name)
		return 1
	}
	fmt.Fprintf(stderr, "hwcalib sweep: wrote %d samples to %s\n", n, name)
	return 0
}

// sweepSignals are the signals that end a sweep early, so that it puts back
// what it wrote rather than die with the limits still in place: SIGINT,
// SIGTERM and SIGHUP, which a dropped SSH session sends. Under nohup, where
// SIGHUP is ignored, it stays ignored and the sweep goes on.
func sweepSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// floatList parses a comma-separated list of numbers, each times scale.
// An empty string is an empty list.
func floatList(s string, scale float64) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v*scale)
	}
	return out, nil
}
