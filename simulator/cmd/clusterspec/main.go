// Command clusterspec checks and expands a simulated cluster spec (package
// clusterspec) against the builtin hardware profiles plus the local ones in
// the directory -profiles-dir names, SIM_PROFILES_DIR by default, as the
// simulator and the simcluster command read them:
//
//	clusterspec validate     -spec FILE [-profiles-dir DIR]
//	clusterspec render-nodes -spec FILE [-profiles-dir DIR]
//	clusterspec list         -spec FILE [-profiles-dir DIR]
//
// validate checks the spec and prints its size. render-nodes prints the
// spec's Nodes, with status, as one YAML v1 List to pipe into kubectl; it is
// the only renderer of fake Nodes. list prints one line per Node: name,
// pool, profile, rack and cooling zone, "-" for an empty one.
//
// The exit status is 0 on success, 1 when the spec does not load, validate
// or expand, and 2 on a usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

const usage = `usage: clusterspec validate|render-nodes|list -spec FILE [-profiles-dir DIR]`

// envProfilesDir is the simulator's variable of the local profiles
// directory, the default of -profiles-dir.
const envProfilesDir = "SIM_PROFILES_DIR"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// nodeList is a v1 List of Nodes, the output of render-nodes.
type nodeList struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Items      []*corev1.Node `json:"items"`
}

// run is the command with its arguments after the program name; it returns
// the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "validate", "render-nodes", "list":
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "clusterspec: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	flags := flag.NewFlagSet("clusterspec "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "", "cluster spec file (required)")
	profilesDir := flags.String("profiles-dir", os.Getenv(envProfilesDir), "directory of local hardware profiles (*.yaml), added to the builtin ones; empty for the builtins only (default "+envProfilesDir+")")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "clusterspec %s: unexpected argument %q\n%s\n", cmd, flags.Arg(0), usage)
		return 2
	}
	if *specPath == "" {
		fmt.Fprintf(stderr, "clusterspec %s: -spec is required\n%s\n", cmd, usage)
		return 2
	}

	spec, err := clusterspec.LoadFile(*specPath)
	if err != nil {
		fmt.Fprintf(stderr, "clusterspec: %v\n", err)
		return 1
	}
	members, err := spec.Expand(clusterspec.Profiles(*profilesDir))
	if err != nil {
		fmt.Fprintf(stderr, "clusterspec: %s is not a valid cluster spec:\n", *specPath)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  %s\n", line)
		}
		return 1
	}

	switch cmd {
	case "validate":
		fmt.Fprintf(stdout, "%s: cluster spec %s is valid: %d pools, %d Nodes\n", *specPath, spec.Metadata.Name, len(spec.Spec.Pools), len(members))
	case "render-nodes":
		list := nodeList{APIVersion: "v1", Kind: "List", Items: make([]*corev1.Node, 0, len(members))}
		for _, m := range members {
			list.Items = append(list.Items, m.Node)
		}
		b, err := yaml.Marshal(list)
		if err != nil {
			fmt.Fprintf(stderr, "clusterspec: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(b); err != nil {
			fmt.Fprintf(stderr, "clusterspec: %v\n", err)
			return 1
		}
	case "list":
		w := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		for _, m := range members {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.Pool, m.Profile.Name, orDash(m.Rack), orDash(m.CoolingZone))
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintf(stderr, "clusterspec: %v\n", err)
			return 1
		}
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
