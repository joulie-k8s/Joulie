// kubectl-joulie is the kubectl plugin of Joulie: the energy state of a
// cluster in the terminal, as styled tables or as a live dashboard.
//
// Install: kubectl krew install joulie, or go build -o kubectl-joulie ./cmd/kubectl-joulie
// Usage:   kubectl joulie [status|top|node NAME|pods|version]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/pflag"
	"golang.org/x/term"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // cloud and OIDC auth providers
	"k8s.io/client-go/tools/clientcmd"
)

// version is set at build time: -ldflags "-X main.version=v0.3.0".
var version = "dev"

const usage = `kubectl joulie shows the energy state of a cluster that Joulie manages.

Usage:
  kubectl joulie [command] [flags]

Commands:
  status       the cluster summary and one row per managed node (default)
  top          the live dashboard: nodes, power history, node details, pods
  node NAME    one node: hardware, power, twin scores, applied caps, pods
  pods         workload placement: each pod, its class and its node's class
  version      the plugin version

Flags:
  -o, --output string       table (default) or json
  -w, --watch               status and pods: open the live dashboard
      --interval duration   refresh period of the live dashboard (default 2s)
      --no-color            no colors; NO_COLOR=1 does the same
  -h, --help                this help

The kubectl flags --kubeconfig, --context, --cluster, --user, --server,
--token and -n/--namespace (pods only) work as in kubectl.

In the live dashboard: ↑/↓ select, enter details, p pods, n nodes,
s sort, r refresh, q quit.
`

// options are the parsed command line.
type options struct {
	command  string
	args     []string
	output   string
	watch    bool
	interval time.Duration
	noColor  bool
	help     bool

	kubeconfig   string
	overrides    clientcmd.ConfigOverrides
	namespaceSet bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// parseArgs reads the command, its arguments and the flags, which may come
// in any order.
func parseArgs(argv []string, stderr io.Writer) (options, error) {
	var o options
	fs := pflag.NewFlagSet("kubectl joulie", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVarP(&o.output, "output", "o", "table", "")
	fs.BoolVarP(&o.watch, "watch", "w", false, "")
	fs.BoolVarP(&o.watch, "Watch", "W", false, "")
	_ = fs.MarkHidden("Watch")
	fs.DurationVar(&o.interval, "interval", 2*time.Second, "")
	fs.BoolVar(&o.noColor, "no-color", false, "")
	fs.BoolVarP(&o.help, "help", "h", false, "")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "")
	clientcmd.BindOverrideFlags(&o.overrides, fs, clientcmd.RecommendedConfigOverrideFlags(""))
	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	o.namespaceSet = fs.Changed("namespace")
	rest := fs.Args()
	o.command = "status"
	if len(rest) > 0 {
		o.command, o.args = rest[0], rest[1:]
	}
	if o.output != "table" && o.output != "json" {
		return o, fmt.Errorf("--output %q: want table or json", o.output)
	}
	if o.interval < 500*time.Millisecond {
		return o, fmt.Errorf("--interval %s: want at least 500ms", o.interval)
	}
	return o, nil
}

func run(argv []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(argv, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n\n%s", err, usage)
		return 2
	}
	if o.help || o.command == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if o.command == "version" {
		fmt.Fprintf(stdout, "kubectl-joulie %s\n", version)
		return 0
	}

	switch o.command {
	case "status", "top", "node", "pods":
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n%s", o.command, usage)
		return 2
	}
	if o.command == "node" && len(o.args) != 1 {
		fmt.Fprintf(stderr, "error: node takes one node name, as in kubectl joulie node gpu-0\n")
		return 2
	}

	c, contextName, err := connect(o)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if o.command == "top" || (o.watch && o.output == "table") {
		if err := runDashboard(c, contextName, o); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}

	snap, err := readCluster(context.Background(), c, podNamespace(o))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if o.command == "node" {
		if _, ok := findNode(snap, o.args[0]); !ok {
			fmt.Fprintf(stderr, "error: no NodeTwin for node %q\n", o.args[0])
			return 1
		}
	}

	if o.output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(toJSON(snap, o.command, o.args)); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}

	w := colorprofile.NewWriter(stdout, os.Environ())
	if o.noColor {
		w.Profile = colorprofile.NoTTY
	}
	width := terminalWidth(stdout)
	var out string
	switch o.command {
	case "status":
		out = statusView(snap, contextName, width, -1, nil, nil)
	case "pods":
		out = podsView(snap, contextName, width, -1)
	case "node":
		n, _ := findNode(snap, o.args[0])
		out = nodeDetail(snap, n, contextName, width, nil)
	}
	fmt.Fprintln(w, out)
	return 0
}

// podNamespace is the namespace the pods are listed in: the -n of the pods
// command, else every namespace.
func podNamespace(o options) string {
	if o.command == "pods" && o.namespaceSet {
		return o.overrides.Context.Namespace
	}
	return ""
}

// terminalWidth is the width of the terminal behind w, COLUMNS when it is
// not a terminal, and 0, meaning no limit, when neither says.
func terminalWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 {
			return width
		}
	}
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 {
		return v
	}
	return 0
}

type clients struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
}

// connect builds the clients; tests replace it with fakes.
var connect = newClients

// newClients loads the kubeconfig as kubectl does, with the kubectl flags
// applied, and returns the clients and the context name.
func newClients(o options) (clients, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = o.kubeconfig
	overrides := o.overrides
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &overrides)
	rest, err := cfg.ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			return clients{}, "", errors.New("no kubeconfig: set KUBECONFIG or pass --kubeconfig")
		}
		return clients{}, "", fmt.Errorf("loading the kubeconfig: %w", err)
	}
	rest.UserAgent = "kubectl-joulie/" + version
	contextName := overrides.CurrentContext
	if contextName == "" {
		if raw, err := cfg.RawConfig(); err == nil {
			contextName = raw.CurrentContext
		}
	}
	dyn, err := dynamic.NewForConfig(rest)
	if err != nil {
		return clients{}, "", err
	}
	kube, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return clients{}, "", err
	}
	return clients{dyn: dyn, kube: kube}, contextName, nil
}
