// Command simcluster computes, for the simulated cluster scripts
// (scripts/simcluster), what they cannot compute in shell. It reads a
// cluster spec (package clusterspec) with the builtin hardware profiles
// plus the local ones in the directory --profiles-dir names,
// SIM_PROFILES_DIR by default, as the simulator and the clusterspec command
// read them:
//
//	simcluster validate   --spec FILE [--profiles-dir DIR]
//	simcluster agents     --spec FILE --daemonset FILE --image-arch ARCH --root DIR [--profiles-dir DIR]
//	simcluster telemetry  --part PART [--mode MODE] [--scope SCOPE] [--timing NAME] [--spec FILE] [--profiles-dir DIR]
//	simcluster scale-spec --nodes N [--profile NAME] [--profiles-dir DIR]
//
// validate checks the spec's harness section against its pools. agents
// prints a Joulie values file setting agent.emulated.nodes, one entry per
// spec Node the agent DaemonSet in --daemonset (the output of helm template
// --show-only templates/agent-daemonset.yaml) would reach; the Nodes it
// would not reach go to stderr, one "<node>: <reason>" line each.
// telemetry prints one part of a telemetry mode (otel), power scope
// (components) and timing (dev): prom-config, otelcol-config, sim-values,
// joulie-values, checks or wait; wait needs --spec. scale-spec prints a
// cluster spec of N Nodes of one profile (intel-xeon-2s-rapl).
//
// A timing name selects scripts/simcluster/timing/<name>.env in the working
// directory or the nearest parent that has one. Package simcluster
// documents every result. The exit status is 0 on success, 1 on a failure,
// and 2 on a usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
)

// envProfilesDir is the simulator's variable of the local profiles
// directory, the default of --profiles-dir.
const envProfilesDir = "SIM_PROFILES_DIR"

const usage = `usage:
  simcluster validate   --spec FILE [--profiles-dir DIR]
  simcluster agents     --spec FILE --daemonset FILE --image-arch ARCH --root DIR [--profiles-dir DIR]
  simcluster telemetry  --part PART [--mode MODE] [--scope SCOPE] [--timing NAME] [--spec FILE] [--profiles-dir DIR]
  simcluster scale-spec --nodes N [--profile NAME] [--profiles-dir DIR]`

// defaultScaleProfile is the profile of a scale spec without --profile, the
// scripts' SIMCLUSTER_SCALE_PROFILE default.
const defaultScaleProfile = "intel-xeon-2s-rapl"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command with its arguments after the program name; it returns
// the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	var err error
	switch cmd {
	case "validate":
		err = runValidate(args, stdout, stderr)
	case "agents":
		err = runAgents(args, stdout, stderr)
	case "telemetry":
		err = runTelemetry(args, stdout, stderr)
	case "scale-spec":
		err = runScaleSpec(args, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "simcluster: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		if ue.msg != "" {
			fmt.Fprintf(stderr, "simcluster %s: %s\n", cmd, ue.msg)
		}
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fmt.Fprintf(stderr, "simcluster %s: %v\n", cmd, err)
	return 1
}

// usageError is a usage error, exit status 2. An empty msg is a flag error
// the flag package has printed already.
type usageError struct{ msg string }

func (e usageError) Error() string { return "usage: " + e.msg }

func usageErr(format string, a ...any) error {
	return usageError{msg: fmt.Sprintf(format, a...)}
}

// parse parses args into flags; a flag error or a positional argument is a
// usage error.
func parse(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{}
	}
	if flags.NArg() > 0 {
		return usageErr("unexpected argument %q", flags.Arg(0))
	}
	return nil
}

func newFlags(cmd string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("simcluster "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

// expand loads and expands the spec at path.
func expand(path, profilesDir string) (*clusterspec.Spec, []clusterspec.Member, error) {
	s, err := clusterspec.LoadFile(path)
	if err != nil {
		return nil, nil, err
	}
	members, err := s.Expand(clusterspec.Profiles(profilesDir))
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not a valid cluster spec:\n%w", path, err)
	}
	return s, members, nil
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	flags := newFlags("validate", stderr)
	spec := flags.String("spec", "", "cluster spec file (required)")
	profilesDir := profilesDirFlag(flags)
	if err := parse(flags, args); err != nil {
		return err
	}
	if *spec == "" {
		return usageErr("--spec is required")
	}
	s, members, err := expand(*spec, *profilesDir)
	if err != nil {
		return err
	}
	h, err := simcluster.SpecHarness(s)
	if err != nil {
		return fmt.Errorf("%s: %w", *spec, err)
	}
	var set []string
	for _, p := range s.Spec.Pools {
		if d := h.ToolDelivery(p.Name, ""); d != "" {
			set = append(set, p.Name+": "+d)
		}
	}
	fmt.Fprintf(stdout, "%s: harness section of cluster spec %s is valid: %d Nodes, %d with measured CPU packages, tool delivery set for pools [%s], the install's for the others\n",
		*spec, s.Metadata.Name, len(members), len(simcluster.WaitNodes(members)), strings.Join(set, ", "))
	return nil
}

func runAgents(args []string, stdout, stderr io.Writer) error {
	flags := newFlags("agents", stderr)
	spec := flags.String("spec", "", "cluster spec file (required)")
	profilesDir := profilesDirFlag(flags)
	daemonset := flags.String("daemonset", "", "the agent DaemonSet render, helm template --show-only templates/agent-daemonset.yaml (required)")
	imageArch := flags.String("image-arch", "", "architecture the agent image is built for, such as amd64 (required)")
	root := flags.String("root", "", "hwemu root on the infra Node, an absolute path (required)")
	if err := parse(flags, args); err != nil {
		return err
	}
	for _, f := range []struct{ name, v string }{{"spec", *spec}, {"daemonset", *daemonset}, {"image-arch", *imageArch}, {"root", *root}} {
		if f.v == "" {
			return usageErr("--%s is required", f.name)
		}
	}
	s, members, err := expand(*spec, *profilesDir)
	if err != nil {
		return err
	}
	h, err := simcluster.SpecHarness(s)
	if err != nil {
		return fmt.Errorf("%s: %w", *spec, err)
	}
	render, err := os.ReadFile(*daemonset)
	if err != nil {
		return err
	}
	placement, err := simcluster.PlacementFromRender(render, *imageArch)
	if err != nil {
		return fmt.Errorf("%s: %w", *daemonset, err)
	}
	agents, excluded, err := simcluster.Agents(members, placement, h, *root)
	if err != nil {
		return err
	}
	out, err := simcluster.AgentsValues(s.Metadata.Name, agents)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(out); err != nil {
		return err
	}
	for _, e := range excluded {
		fmt.Fprintf(stderr, "%s: %s\n", e.Node, e.Reason)
	}
	return nil
}

func runTelemetry(args []string, stdout, stderr io.Writer) error {
	flags := newFlags("telemetry", stderr)
	part := flags.String("part", "", "part to print: "+strings.Join(simcluster.Parts(), ", ")+" (required)")
	mode := flags.String("mode", string(simcluster.ModeOTel), "telemetry mode")
	scope := flags.String("scope", string(simcluster.ScopeComponents), "power scope")
	timing := flags.String("timing", simcluster.TimingDev, "timing, scripts/simcluster/timing/<name>.env")
	spec := flags.String("spec", "", "cluster spec file (required for --part wait)")
	profilesDir := profilesDirFlag(flags)
	if err := parse(flags, args); err != nil {
		return err
	}
	if *part == "" {
		return usageErr("--part is required")
	}
	known := false
	for _, p := range simcluster.Parts() {
		known = known || p == *part
	}
	if !known {
		return usageErr("unknown part %q; parts are %s", *part, strings.Join(simcluster.Parts(), ", "))
	}
	m, err := simcluster.ParseMode(*mode)
	if err != nil {
		return usageErr("%v", err)
	}
	sc, err := simcluster.ParseScope(*scope)
	if err != nil {
		return usageErr("%v", err)
	}
	if *part == simcluster.PartWait && *spec == "" {
		return usageErr("--part %s needs --spec", simcluster.PartWait)
	}
	t, err := findTiming(*timing)
	if err != nil {
		return err
	}
	parts, err := simcluster.Telemetry(m, sc, t)
	if err != nil {
		return err
	}
	if *part == simcluster.PartWait {
		_, members, err := expand(*spec, *profilesDir)
		if err != nil {
			return err
		}
		parts.Wait.Nodes = simcluster.WaitNodes(members)
	}
	out, err := parts.Part(*part)
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
}

// findTiming reads timing name from the working directory or the nearest
// parent directory that has scripts/simcluster/timing/<name>.env.
func findTiming(name string) (simcluster.Timing, error) {
	wd, err := os.Getwd()
	if err != nil {
		return simcluster.Timing{}, err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(simcluster.TimingFile(dir, name)); err == nil {
			return simcluster.ReadTiming(dir, name)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return simcluster.Timing{}, fmt.Errorf("timing %q: no %s in %s or a parent directory", name, filepath.Join("scripts", "simcluster", "timing", name+".env"), wd)
}

func runScaleSpec(args []string, stdout, stderr io.Writer) error {
	flags := newFlags("scale-spec", stderr)
	nodes := flags.Int("nodes", 0, "number of Nodes, at least 1 (required)")
	profile := flags.String("profile", defaultScaleProfile, "hardware profile of every Node")
	profilesDir := profilesDirFlag(flags)
	if err := parse(flags, args); err != nil {
		return err
	}
	if *nodes < 1 {
		return usageErr("--nodes %d: want at least 1", *nodes)
	}
	b, err := simcluster.ScaleSpec(*nodes, *profile)
	if err != nil {
		return err
	}
	s, err := clusterspec.Parse(b)
	if err != nil {
		return err
	}
	if _, err := s.Expand(clusterspec.Profiles(*profilesDir)); err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

// profilesDirFlag defines --profiles-dir, the local profiles directory,
// SIM_PROFILES_DIR by default; an empty value means the builtins only.
func profilesDirFlag(flags *flag.FlagSet) *string {
	return flags.String("profiles-dir", os.Getenv(envProfilesDir),
		"directory of local hardware profiles (*.yaml), added to the builtin ones; empty for the builtins only (default "+envProfilesDir+")")
}
