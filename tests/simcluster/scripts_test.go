// Tests of the simulated cluster scripts (scripts/simcluster). They are
// hermetic: each test copies the scripts into a temporary repository with
// stub manifests, and runs them against fake docker, kubectl, helm, stat,
// curl, kind and go commands, plus fake simcluster, clusterspec and
// simscenario host tools, that record their arguments and keep a little
// state. Nothing reaches Docker, a cluster or the network. They need bash
// and jq and skip without them.

package simcluster_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// scRepo is the repository root, seen from this package's directory.
const scRepo = "../.."

// scCommon is sourced by every fake: it appends the call to $FAKE_LOG, one
// line per call with the arguments separated by 0x1f.
const scCommon = `fake_record() {
  local line=$1 a
  shift
  for a in "$@"; do line+=$'\x1f'"${a//$'\n'/\\n}"; done
  printf '%s\n' "$line" >>"$FAKE_LOG"
}
`

const scFakeDocker = `#!/usr/bin/env bash
# Fake docker: containers, volumes, local images and the cluster's containerd
# images are files under $FAKE_STATE/docker; the container's file system, as
# far as exec sh and exec tar reach it, is $FAKE_STATE/rootfs.
set -u
source "$FAKE_BIN/fake-common.sh"
fake_record docker "$@"
S=$FAKE_STATE/docker
mkdir -p "$S"
key() { printf '%s' "$1" | tr '/:@' '___'; }
norm() {
  local ref=$1 first=${1%%/*}
  if [[ $ref != */* ]]; then printf 'docker.io/library/%s\n' "$ref"
  elif [[ $first == *.* || $first == *:* || $first == localhost ]]; then printf '%s\n' "$ref"
  else printf 'docker.io/%s\n' "$ref"; fi
}
last=${!#}
sub=$1
shift
case $sub in
inspect)
  [[ -f $S/container-$last ]] || { echo "Error: No such object: $last" >&2; exit 1; }
  if [[ $1 == -f ]]; then cat "$S/container-$last"; else echo '[{}]'; fi
  ;;
run)
  name= images=
  args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
    --name) name=${args[i+1]} ;;
    -v)
      v=${args[i+1]}
      if [[ $v == *:/var/lib/rancher/k3s/agent/images ]]; then images=${v%%:*}; fi
      if [[ $v != /* ]]; then touch "$S/volume-${v%%:*}"; fi
      ;;
    esac
  done
  echo true >"$S/container-$name"
  printf 'docker run saw in the images directory: %s\n' "$(ls -1 "$images" 2>/dev/null | tr '\n' ' ')" >>"$FAKE_STATE/events"
  echo 0123456789abcdef
  ;;
start) echo true >"$S/container-$last" ;;
port) echo 127.0.0.1:41234 ;;
exec)
  while [[ $1 == -* ]]; do shift; done
  shift
  case $1 in
  cat) printf 'apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\n' ;;
  ctr)
    if [[ " $* " == *" ls "* ]]; then
      ref=${last#name==}
      if grep -qxF "$ref" "$S/ctr-images" 2>/dev/null; then echo "$ref"; fi
      exit 0
    fi
    while read -r line; do
      if [[ $line == "fake-image "* ]]; then norm "${line#fake-image }" >>"$S/ctr-images"; fi
    done
    ;;
  crictl)
    id=$(printf '%s' "$last" | sha256sum | cut -c1-64)
    printf '{"status":{"id":"sha256:%s","repoDigests":["%s@sha256:%s"]}}\n' "$id" "${last%:*}" "$id"
    ;;
  sh)
    # sh -c <script> <name> <arg>...: every absolute path argument moves
    # under the root file system.
    script=$3 zero=$4
    shift 4
    args=()
    for a in "$@"; do
      if [[ $a == /* ]]; then a=$FAKE_STATE/rootfs$a; fi
      args+=("$a")
    done
    sh -c "$script" "$zero" ${args[@]+"${args[@]}"}
    ;;
  tar)
    # tar -C <dir> -cf - <path>...: an empty archive when <dir> is missing.
    if [[ -d $FAKE_STATE/rootfs$3 ]]; then
      tar -C "$FAKE_STATE/rootfs$3" -cf - "${@:6}"
    else
      tar -cf - -T /dev/null
    fi
    ;;
  esac
  ;;
pull) echo "$last" >>"$S/local-images" ;;
tag)
  echo "$2" >>"$S/local-images"
  if [[ $1 == *@* ]]; then echo "$1" >"$S/repodigests-$(key "$2")"; fi
  ;;
image)
  grep -qxF "$last" "$S/local-images" 2>/dev/null || exit 1
  if [[ $2 == --format ]]; then cat "$S/repodigests-$(key "$last")" 2>/dev/null; fi
  ;;
build)
  args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ ${args[i]} == -t ]]; then echo "${args[i+1]}" >>"$S/local-images"; fi
  done
  ;;
save)
  if [[ $1 == --help ]]; then echo '      --platform strings   Save only the given platform(s)'; exit 0; fi
  echo "fake-image $last"
  ;;
rm) rm -f "$S/container-$last" ;;
volume)
  case $1 in
  inspect) [[ -f $S/volume-$last ]] || exit 1 ;;
  rm) for v in "${@:2}"; do rm -f "$S/volume-$v"; done ;;
  esac
  ;;
logs) echo "fake k3s log" ;;
esac
exit 0
`

const scFakeKubectl = `#!/usr/bin/env bash
# Fake kubectl: get answers from $FAKE_STATE/kube/get-<resource>[-<name>][-<namespace>].json
# (several resources merged), get --raw answers readiness probes and
# Prometheus queries ($FAKE_STATE/kube/prom.json), apply -o name prints
# $FAKE_STATE/kube/apply-names-<file>; everything else succeeds. A
# <file>.later becomes <file> on its second read, for the waits.
set -u
source "$FAKE_BIN/fake-common.sh"
fake_record kubectl "$@"
printf '%s %s\n' "${KUBECONFIG:-}" "${KUBECACHEDIR:-}" >>"$FAKE_STATE/tool-env"
# Like the real tool, which caches discovery under ~/.kube/cache unless
# KUBECACHEDIR says otherwise.
[[ -n ${KUBECACHEDIR:-} ]] || mkdir -p "$HOME/.kube/cache"
K=$FAKE_STATE/kube
later() {
  [[ -f $1.later ]] || return 0
  if [[ -f $1.seen ]]; then mv "$1.later" "$1"; else : >"$1.seen"; fi
}
ns= out= raw= file= pos=()
while (($#)); do
  case $1 in
  -n | --namespace) ns=$2; shift 2 ;;
  -o) out=$2; shift 2 ;;
  -l | -p) shift 2 ;;
  -f) file=$2; shift 2 ;;
  -A | --all-namespaces) ns=all; shift ;;
  --raw) raw=$2; shift 2 ;;
  -*) shift ;;
  *) pos+=("$1"); shift ;;
  esac
done
if [[ $file == - ]]; then cat >/dev/null; fi
case ${pos[0]:-} in
get)
  if [[ -n $raw ]]; then
    case $raw in
    */readyz | */-/ready) echo ok ;;
    */proxy/api/v1/query\?query=*)
      later "$K/prom.json"
      cat "$K/prom.json" 2>/dev/null || echo '{"status":"success","data":{"resultType":"vector","result":[]}}' ;;
    *) exit 1 ;;
    esac
    exit 0
  fi
  res=${pos[1]:-} name=${pos[2]:-} sfx=${ns:+-$ns}
  if [[ -n $name ]]; then
    f=$K/get-$res-$name$sfx.json
    if [[ $out == json ]]; then
      [[ -f $f ]] || exit 1
      cat "$f"
    fi
    exit 0
  fi
  items='[]'
  IFS=, read -ra kinds <<<"$res"
  for r in "${kinds[@]}"; do
    f=$K/get-$r$sfx.json
    later "$f"
    if [[ -f $f ]]; then items=$(jq -c --argjson a "$items" '$a + .items' "$f"); fi
  done
  list=$(jq -nc --argjson i "$items" '{apiVersion: "v1", kind: "List", items: $i}')
  if [[ $out == name ]]; then
    jq -r '.items[] | "\(.kind | ascii_downcase)/\(.metadata.name)"' <<<"$list"
  else
    echo "$list"
  fi
  ;;
apply)
  if [[ $out == name ]]; then cat "$K/apply-names-$(basename "$file")" 2>/dev/null; fi
  ;;
create) printf 'apiVersion: v1\nkind: Fake\n' ;;
esac
exit 0
`

const scFakeHelm = `#!/usr/bin/env bash
# Fake helm: template prints $FAKE_STATE/helm-template.yaml, get values prints
# $FAKE_STATE/helm-values-<release>.json; everything else succeeds.
set -u
source "$FAKE_BIN/fake-common.sh"
fake_record helm "$@"
printf '%s %s\n' "${KUBECONFIG:-}" "${KUBECACHEDIR:-}" >>"$FAKE_STATE/tool-env"
[[ -n ${KUBECACHEDIR:-} ]] || mkdir -p "$HOME/.kube/cache"
case ${1:-} in
template) cat "$FAKE_STATE/helm-template.yaml" 2>/dev/null || printf 'kind: DaemonSet\nmetadata: {name: joulie-agent}\n' ;;
get)
  if [[ " $* " == *" json "* ]]; then cat "$FAKE_STATE/helm-values-$3.json" 2>/dev/null || echo '{}'; else echo '{}'; fi
  ;;
esac
exit 0
`

const scFakeStat = `#!/usr/bin/env bash
# Fake stat: the cgroup file system type is $FAKE_CGROUP_FS.
source "$FAKE_BIN/fake-common.sh"
fake_record stat "$@"
if [[ "$*" == "-fc %T /sys/fs/cgroup" ]]; then
  echo "${FAKE_CGROUP_FS:-cgroup2fs}"
  exit 0
fi
command -p stat "$@"
`

const scFakeCurl = `#!/usr/bin/env bash
# Fake curl: writes $FAKE_AIRGAP_CONTENT to the -o file.
source "$FAKE_BIN/fake-common.sh"
fake_record curl "$@"
out=
while (($#)); do
  case $1 in
  -o) out=$2; shift 2 ;;
  *) shift ;;
  esac
done
printf '%s' "${FAKE_AIRGAP_CONTENT:-fake k3s air-gap bundle}" >"$out"
`

const scFakeKind = `#!/usr/bin/env bash
source "$FAKE_BIN/fake-common.sh"
fake_record kind "$@"
exit 1
`

// scFakeGo builds a fake binary per package: the fake host tool when there
// is one ($FAKE_BIN/fake-<name>), otherwise a file whose content changes
// with $FAKE_BUILD_SALT.
const scFakeGo = `#!/usr/bin/env bash
source "$FAKE_BIN/fake-common.sh"
fake_record go "$@"
case $1 in
build)
  shift
  out= pkgs=()
  while (($#)); do
    case $1 in
    -o) out=$2; shift 2 ;;
    -*) shift ;;
    *) pkgs+=("$1"); shift ;;
    esac
  done
  mkdir -p "$out"
  for p in "${pkgs[@]}"; do
    n=$(basename "$p")
    if [[ -f $FAKE_BIN/fake-$n ]]; then
      cp "$FAKE_BIN/fake-$n" "$out/$n"
    else
      printf 'fake binary %s %s\n' "$n" "${FAKE_BUILD_SALT:-0}" >"$out/$n"
    fi
    chmod +x "$out/$n"
  done
  printf 'GOOS=%s GOARCH=%s CGO_ENABLED=%s GOTOOLCHAIN=%s\n' "${GOOS:-}" "${GOARCH:-}" "${CGO_ENABLED:-}" "${GOTOOLCHAIN:-}" >>"$FAKE_STATE/go-build-env"
  ;;
env) echo go1.22.9 ;;
test)
  printf '%s\n' "${SIMCLUSTER_RUN_DIR:-}" >>"$FAKE_STATE/go-test-run-dir"
  exit "${FAKE_GO_TEST_RC:-0}"
  ;;
esac
exit 0
`

const scFakeSimcluster = `#!/usr/bin/env bash
# Fake simcluster: agents prints $FAKE_STATE/agents.yaml (one agent by
# default), telemetry prints $FAKE_STATE/telemetry-<part>.
source "$FAKE_BIN/fake-common.sh"
fake_record simcluster "$@"
sub=$1
shift
part=
while (($#)); do
  if [[ $1 == --part ]]; then part=$2; fi
  shift
done
case $sub in
agents)
  cat "$FAKE_STATE/agents.yaml" 2>/dev/null || printf 'agent:\n  emulated:\n    nodes:\n    - name: intel-0\n      profile: intel-xeon-2s-rapl\n'
  if [[ -f $FAKE_STATE/exclusions ]]; then cat "$FAKE_STATE/exclusions" >&2; fi
  ;;
telemetry)
  if [[ -f $FAKE_STATE/telemetry-$part ]]; then
    cat "$FAKE_STATE/telemetry-$part"
  elif [[ $part == wait ]]; then
    echo '{"query":"count by (instance) (hw_energy_joules_total{hw_type=\"cpu\"})","label":"instance","nodes":["intel-0"]}'
  else
    echo "# fake $part"
  fi
  ;;
scale-spec) printf 'apiVersion: sim.joulie.io/v1alpha1\nkind: ClusterSpec\nmetadata: {name: scale}\n' ;;
esac
exit 0
`

const scFakeClusterspec = `#!/usr/bin/env bash
source "$FAKE_BIN/fake-common.sh"
fake_record clusterspec "$@"
# With FAKE_LOCAL_PROFILE the spec needs that profile from -profiles.
if [[ $1 == validate && -n ${FAKE_LOCAL_PROFILE:-} && " $* " != *" -profiles "* ]]; then
  echo "unknown profile $FAKE_LOCAL_PROFILE" >&2
  exit 1
fi
if [[ $1 == render-nodes ]]; then
  printf 'apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Node\n  metadata: {name: intel-0}\n'
fi
exit 0
`

// scFakeSimscenario plans one unnamed variant, with the smoke scenario's
// workload namespace and no Joulie or simulator --set values. run stands in
// for the simulator: it needs the trace scenario.sh copied into the run's
// directory in the container and writes the run's OTLP file there. evaluate
// and summarize write a summary.json; summarize exits $FAKE_SCENARIO_RC.
const scFakeSimscenario = `#!/usr/bin/env bash
source "$FAKE_BIN/fake-common.sh"
fake_record simscenario "$@"
sub=$1
shift
case $sub in
validate) [[ -s $1 ]] || { echo "simscenario validate: no scenario $1" >&2; exit 2; } ;;
plan)
  out=
  while (($#)); do
    if [[ $1 == -out ]]; then out=$2; fi
    shift
  done
  mkdir -p "$out"
  printf 'name: smoke\nworkload:\n  namespace: sim-workload\n' >"$out/scenario.effective.yaml"
  printf '{"type":"job","jobId":"cpu-performance-0","namespace":"sim-workload"}\n' >"$out/trace.jsonl"
  printf '%s\n' "SCENARIO_NAME=smoke" "SCENARIO_VARIANT=''" "SCENARIO_RUN_ID=smoke-1" \
    "SIMCLUSTER_SPEC=tests/simcluster/specs/smoke.yaml" "SIMCLUSTER_TIMING=dev" "SIMCLUSTER_TELEMETRY=otel" \
    "SIMCLUSTER_POWER_SCOPE=components" "SCENARIO_ISOLATION=reuse" "JOULIE_ENABLED=true" \
    "JOULIE_SET=()" "JOULIE_VALUES=()" "SIM_SET=()" >"$out/variant.env"
  echo "run directory $out: 1 jobs due within 4m0s"
  echo "variant (unnamed): run id smoke-1, joulie enabled true, telemetry otel"
  ;;
run)
  dir=$FAKE_STATE/rootfs/var/lib/joulie-hwemu/_runs/smoke-1
  [[ -s $dir/trace.jsonl ]] || { echo "simscenario run: no trace in $dir" >&2; exit 2; }
  mkdir -p "$dir/otlp"
  echo '{"resourceMetrics":[]}' >"$dir/otlp/simulator.metrics.jsonl"
  ;;
evaluate) echo '{"status":"passed"}' >"$1/summary.json" ;;
summarize)
  echo '{"status":"passed"}' >"$1/summary.json"
  exit "${FAKE_SCENARIO_RC:-0}"
  ;;
esac
exit 0
`

// scEnv is one temporary repository with fake tools.
type scEnv struct {
	t     *testing.T
	repo  string
	bin   string
	state string
	home  string
	dir   string
	vars  map[string]string
}

// scCall is one recorded call: the tool, then its arguments.
type scCall []string

func (c scCall) tool() string { return c[0] }
func (c scCall) args() []string {
	return c[1:]
}
func (c scCall) String() string { return strings.Join(c, " ") }

// has reports whether the call has every one of args, in any position.
func (c scCall) has(args ...string) bool {
	for _, want := range args {
		found := false
		for _, a := range c.args() {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// after returns the arguments that follow each occurrence of flag.
func (c scCall) after(flag string) []string {
	var out []string
	a := c.args()
	for i := 0; i+1 < len(a); i++ {
		if a[i] == flag {
			out = append(out, a[i+1])
		}
	}
	return out
}

func scNew(t *testing.T) *scEnv {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the script tests need it", tool)
		}
	}
	root := t.TempDir()
	e := &scEnv{
		t:     t,
		repo:  filepath.Join(root, "repo"),
		bin:   filepath.Join(root, "fakebin"),
		state: filepath.Join(root, "state"),
		home:  filepath.Join(root, "home"),
		dir:   filepath.Join(root, "cache", "simcluster"),
	}
	for _, d := range []string{e.repo, e.bin, e.state, e.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scripts, err := filepath.Glob(filepath.Join(scRepo, "scripts/simcluster/*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no scripts under scripts/simcluster: %v", err)
	}
	for _, s := range scripts {
		e.copyIn(s, "scripts/simcluster/"+filepath.Base(s))
	}
	for _, f := range []string{
		"scripts/simcluster/versions.env",
		"scripts/simcluster/timing/dev.env",
		"scripts/simcluster/timing/production.env",
		"go.mod",
		"values/simcluster.yaml",
		"values/simcluster-simulator.yaml",
	} {
		e.copyIn(filepath.Join(scRepo, f), f)
	}
	// Stubs for files other packages own: the scripts only pass their paths
	// to the fakes, and hash the scheduler configuration.
	for f, content := range map[string]string{
		"scripts/simcluster/manifests/prometheus.yaml":             "# stub\n",
		"scripts/simcluster/manifests/otelcol.yaml":                "# stub\n",
		"scripts/simcluster/manifests/scheduler-config.yaml":       "ignorable: true\n",
		"scripts/simcluster/manifests/kwok-v0.8.0/kwok.yaml":       "# stub\n",
		"scripts/simcluster/manifests/kwok-v0.8.0/stage-fast.yaml": "# stub\n",
		"charts/joulie/crds/joulie.io_nodetwins.yaml":              "# stub\n",
		"charts/joulie/Chart.yaml":                                 "name: joulie\n",
		"charts/joulie-simulator/Chart.yaml":                       "name: joulie-simulator\n",
		"tests/simcluster/specs/smoke.yaml":                        "metadata: {name: smoke}\n",
		"tests/simcluster/smoke-values.yaml":                       "{}\n",
	} {
		e.write(filepath.Join(e.repo, f), content)
	}
	for name, body := range map[string]string{
		"fake-common.sh":   scCommon,
		"docker":           scFakeDocker,
		"kubectl":          scFakeKubectl,
		"helm":             scFakeHelm,
		"stat":             scFakeStat,
		"curl":             scFakeCurl,
		"kind":             scFakeKind,
		"go":               scFakeGo,
		"fake-simcluster":  scFakeSimcluster,
		"fake-clusterspec": scFakeClusterspec,
		"fake-simscenario": scFakeSimscenario,
	} {
		e.write(filepath.Join(e.bin, name), body)
		if err := os.Chmod(filepath.Join(e.bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.vars = map[string]string{
		// The fakes come first, so no real docker, kubectl or helm runs.
		"PATH":           e.bin + ":" + os.Getenv("PATH"),
		"HOME":           e.home,
		"LANG":           "C",
		"SIMCLUSTER_DIR": e.dir,
		"FAKE_LOG":       filepath.Join(e.state, "calls.log"),
		"FAKE_STATE":     e.state,
		"FAKE_BIN":       e.bin,
	}
	e.seedCluster()
	return e
}

func (e *scEnv) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *scEnv) copyIn(src, rel string) {
	e.t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		e.t.Fatal(err)
	}
	e.write(filepath.Join(e.repo, rel), string(b))
}

// seedCluster writes a cluster of one fake Node, intel-0, that is Ready, has
// an emulated agent, a NodeHardware, a written NodeTwin and series in
// Prometheus.
func (e *scEnv) seedCluster() {
	k := filepath.Join(e.state, "kube")
	files := map[string]string{
		"get-nodes.json": `{"items":[{"kind":"Node","metadata":{"name":"intel-0",
			"labels":{"joulie.io/managed":"true","sim.joulie.io/cluster":"smoke","sim.joulie.io/pool":"intel"},
			"annotations":{"kwok.x-k8s.io/node":"fake","sim.joulie.io/profile":"intel-xeon-2s-rapl"}},
			"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`,
		"get-nodehardwares.json": `{"items":[{"kind":"NodeHardware","metadata":{"name":"intel-0"},"spec":{"nodeName":"intel-0"}}]}`,
		"get-nodetwins.json": `{"items":[{"kind":"NodeTwin","metadata":{"name":"intel-0"},"spec":{"nodeName":"intel-0"},
			"status":{"lastUpdated":"2026-10-05T00:00:00Z","powerMeasurement":{"source":"prometheus","measuredNodePowerW":420}}}]}`,
		"get-deployments-joulie-system.json": `{"items":[{"kind":"Deployment","metadata":{"name":"joulie-agent-emu-0",
			"annotations":{"sim.joulie.io/node":"intel-0"}},
			"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/name":"joulie-agent"}}}}}]}`,
		"prom.json":                   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"intel-0"},"value":[0,"1"]}]}}`,
		"apply-names-prometheus.yaml": "namespace/monitoring\ndeployment.apps/prometheus\nservice/prometheus-operated\n",
		"apply-names-otelcol.yaml":    "deployment.apps/otelcol\n",
		"apply-names-nodes.yaml":      "node/intel-0\n",
	}
	for name, content := range files {
		e.write(filepath.Join(k, name), content)
	}
}

// run runs a script of the temporary repository with extra environment and
// returns its combined output.
func (e *scEnv) run(script string, extra map[string]string, args ...string) (string, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(e.repo, "scripts/simcluster", script)}, args...)...)
	cmd.Dir = e.repo
	vars := map[string]string{}
	for k, v := range e.vars {
		vars[k] = v
	}
	for k, v := range extra {
		vars[k] = v
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		e.t.Fatalf("%s timed out:\n%s", script, out)
	}
	return string(out), err
}

// mustRun runs a script and fails the test when it fails.
func (e *scEnv) mustRun(script string, extra map[string]string, args ...string) string {
	e.t.Helper()
	out, err := e.run(script, extra, args...)
	if err != nil {
		e.t.Fatalf("%s %v: %v\n%s", script, args, err, out)
	}
	return out
}

// mustFail runs a script, fails the test when it succeeds, and returns its
// output.
func (e *scEnv) mustFail(script string, extra map[string]string, args ...string) string {
	e.t.Helper()
	out, err := e.run(script, extra, args...)
	if err == nil {
		e.t.Fatalf("%s %v succeeded; want a failure:\n%s", script, args, out)
	}
	return out
}

// mustExit runs a script and fails the test unless it exits with code.
func (e *scEnv) mustExit(code int, script string, extra map[string]string, args ...string) string {
	e.t.Helper()
	out, err := e.run(script, extra, args...)
	var ee *exec.ExitError
	switch {
	case err == nil && code != 0:
		e.t.Fatalf("%s %v succeeded; want exit status %d:\n%s", script, args, code, out)
	case err != nil && (!errors.As(err, &ee) || ee.ExitCode() != code):
		e.t.Fatalf("%s %v: %v; want exit status %d:\n%s", script, args, err, code, out)
	}
	return out
}

func (e *scEnv) calls() []scCall {
	e.t.Helper()
	b, err := os.ReadFile(e.vars["FAKE_LOG"])
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []scCall
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.Split(line, "\x1f"))
		}
	}
	return out
}

// callsOf returns the calls of tool whose first arguments are prefix.
func (e *scEnv) callsOf(tool string, prefix ...string) []scCall {
	var out []scCall
	for _, c := range e.calls() {
		if c.tool() != tool || len(c.args()) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if c.args()[i] != p {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func (e *scEnv) resetCalls() {
	e.t.Helper()
	if err := os.Remove(e.vars["FAKE_LOG"]); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
}

// setVersion replaces one key of the temporary repository's versions.env.
func (e *scEnv) setVersion(key, value string) {
	e.t.Helper()
	path := filepath.Join(e.repo, "scripts/simcluster/versions.env")
	b, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
	if !re.Match(b) {
		e.t.Fatalf("versions.env has no %s", key)
	}
	e.write(path, re.ReplaceAllString(string(b), key+"="+value))
}

// setAgents makes the fake simcluster list n emulated agents.
func (e *scEnv) setAgents(n int) {
	var b strings.Builder
	b.WriteString("agent:\n  emulated:\n    nodes:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "    - name: intel-%d\n      profile: intel-xeon-2s-rapl\n", i)
	}
	e.write(filepath.Join(e.state, "agents.yaml"), b.String())
}

// container makes the fake docker report a container (and its volumes).
func (e *scEnv) container(name string) {
	d := filepath.Join(e.state, "docker")
	e.write(filepath.Join(d, "container-"+name), "true\n")
	e.write(filepath.Join(d, "volume-"+name+"-k3s"), "")
	e.write(filepath.Join(d, "volume-"+name+"-hwemu"), "")
}

func scExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// scEnvFile reads a KEY=VALUE file.
func scEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: line %q is not KEY=VALUE", path, line)
		}
		out[k] = v
	}
	return out
}

// scSha12 is the tag hash of a fake binary built with salt.
func scSha12(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:12]
}

func scFakeBinary(name, salt string) string {
	return fmt.Sprintf("fake binary %s %s\n", name, salt)
}

func TestSC01DownRefusesNamesOutsideTheGuard(t *testing.T) {
	e := scNew(t)
	for _, name := range []string{"kind-x", "hwemu-k3s", "joulie-simx", "joulie-sim-", "joulie-sim_a", "Joulie-sim"} {
		t.Run(name, func(t *testing.T) {
			e.resetCalls()
			e.container(name)
			out := e.mustFail("down.sh", map[string]string{"SIMCLUSTER_NAME": name})
			if !strings.Contains(out, "does not match") {
				t.Errorf("down.sh did not name the guard:\n%s", out)
			}
			if calls := e.callsOf("docker"); len(calls) != 0 {
				t.Errorf("down.sh ran docker for %s: %v", name, calls)
			}
			if !scExists(filepath.Join(e.state, "docker", "container-"+name)) || !scExists(filepath.Join(e.state, "docker", "volume-"+name+"-k3s")) {
				t.Errorf("the container or a volume of %s was removed", name)
			}
		})
	}
}

func TestSC02SecondUpReusesTheContainer(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	e.mustRun("up.sh", nil)
	if runs := e.callsOf("docker", "run"); len(runs) != 1 {
		t.Fatalf("two up.sh runs issued %d docker run calls, want 1: %v", len(runs), runs)
	}
	kc, err := os.ReadFile(filepath.Join(e.dir, "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kc), "https://127.0.0.1:41234") {
		t.Errorf("the kubeconfig does not point at the published port:\n%s", kc)
	}
}

func TestSC03PodCapacityFlagsAndRecreate(t *testing.T) {
	flags := func(c scCall) []string {
		var out []string
		for _, a := range c.args() {
			if strings.HasPrefix(a, "--kubelet-arg=max-pods=") || strings.HasPrefix(a, "--kube-controller-manager-arg=node-cidr-mask-size=") || strings.HasPrefix(a, "--cluster-cidr=") {
				out = append(out, a)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		agents int
		nodes  string
		want   []string
	}{
		{"smoke", 7, "", nil},
		{"scale-200", 200, "200", []string{"--kubelet-arg=max-pods=250", "--kube-controller-manager-arg=node-cidr-mask-size=24"}},
		{"scale-600", 600, "600", []string{"--kubelet-arg=max-pods=650", "--kube-controller-manager-arg=node-cidr-mask-size=22", "--cluster-cidr=10.64.0.0/12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := scNew(t)
			e.setAgents(tc.agents)
			extra := map[string]string{}
			if tc.nodes != "" {
				extra["SIMCLUSTER_NODES"] = tc.nodes
			}
			e.mustRun("up.sh", extra)
			runs := e.callsOf("docker", "run")
			if len(runs) != 1 {
				t.Fatalf("got %d docker run calls, want 1", len(runs))
			}
			if got := flags(runs[0]); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("pod flags = %v, want %v", got, tc.want)
			}
			if tc.nodes != "" && len(e.callsOf("simcluster", "scale-spec", "--nodes", tc.nodes)) != 1 {
				t.Errorf("up.sh did not ask simcluster for a scale spec of %s Nodes", tc.nodes)
			}
			// cluster.env records what the container was created for, so the
			// same spec reuses it.
			got := scEnvFile(t, filepath.Join(e.dir, "cluster.env"))
			want := map[string]string{"MAX_PODS": "110", "NODE_CIDR_MASK": "24", "CLUSTER_CIDR": "10.42.0.0/16"}
			for _, f := range tc.want {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "--kubelet-arg":
					want["MAX_PODS"] = strings.TrimPrefix(v, "max-pods=")
				case "--kube-controller-manager-arg":
					want["NODE_CIDR_MASK"] = strings.TrimPrefix(v, "node-cidr-mask-size=")
				case "--cluster-cidr":
					want["CLUSTER_CIDR"] = v
				}
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("cluster.env %s = %q, want %q (what docker run was given)", k, got[k], v)
				}
			}
			e.mustRun("up.sh", extra)
			if runs := e.callsOf("docker", "run"); len(runs) != 1 {
				t.Errorf("rerunning up.sh with the same spec issued %d docker run calls, want 1", len(runs))
			}
		})
	}

	t.Run("too-many", func(t *testing.T) {
		e := scNew(t)
		e.setAgents(1000)
		out := e.mustFail("up.sh", map[string]string{"SIMCLUSTER_NODES": "1000"})
		if !strings.Contains(out, "split across k3s agents") {
			t.Errorf("want the split message:\n%s", out)
		}
		if runs := e.callsOf("docker", "run"); len(runs) != 0 {
			t.Errorf("docker run was issued: %v", runs)
		}
	})

	t.Run("too-small-cluster", func(t *testing.T) {
		e := scNew(t)
		e.mustRun("up.sh", nil)
		e.setAgents(200)
		out := e.mustFail("up.sh", nil)
		if !strings.Contains(out, "recreate with simcluster-down") || !strings.Contains(out, "max-pods 110, now 250") {
			t.Errorf("want the recreate message naming max-pods:\n%s", out)
		}
		if runs := e.callsOf("docker", "run"); len(runs) != 1 {
			t.Errorf("got %d docker run calls, want only the first", len(runs))
		}
	})

	t.Run("scheduler-config-changed", func(t *testing.T) {
		e := scNew(t)
		e.mustRun("up.sh", nil)
		e.write(filepath.Join(e.repo, "scripts/simcluster/manifests/scheduler-config.yaml"), "ignorable: false\n")
		out := e.mustFail("up.sh", nil)
		if !strings.Contains(out, "recreate with simcluster-down") || !strings.Contains(out, "scheduler-config.yaml changed") {
			t.Errorf("want the recreate message naming the scheduler configuration:\n%s", out)
		}
	})

	t.Run("k3s-image-changed", func(t *testing.T) {
		e := scNew(t)
		e.mustRun("up.sh", nil)
		e.setVersion("K3S_IMAGE", "rancher/k3s:v1.34.2-k3s1")
		out := e.mustFail("up.sh", nil)
		if !strings.Contains(out, "recreate with simcluster-down") {
			t.Errorf("want the recreate message:\n%s", out)
		}
	})
}

func TestSC04CgroupV1FlagOnlyForK3s135OnTmpfs(t *testing.T) {
	for _, tc := range []struct {
		fs, image string
		want      bool
	}{
		{"tmpfs", "rancher/k3s:v1.35.0-k3s1", true},
		{"tmpfs", "rancher/k3s:v1.36.2-k3s1", true},
		{"tmpfs", "rancher/k3s:v1.34.1-k3s1", false},
		{"cgroup2fs", "rancher/k3s:v1.35.0-k3s1", false},
		{"cgroup2fs", "rancher/k3s:v1.34.1-k3s1", false},
	} {
		t.Run(tc.fs+"-"+tc.image, func(t *testing.T) {
			e := scNew(t)
			e.setVersion("K3S_IMAGE", tc.image)
			e.mustRun("up.sh", map[string]string{"FAKE_CGROUP_FS": tc.fs})
			runs := e.callsOf("docker", "run")
			if len(runs) != 1 {
				t.Fatalf("got %d docker run calls, want 1", len(runs))
			}
			if got := runs[0].has("--kubelet-arg=fail-cgroupv1=false"); got != tc.want {
				t.Errorf("fail-cgroupv1=false present = %v, want %v: %v", got, tc.want, runs[0])
			}
			if !runs[0].has(tc.image) {
				t.Errorf("docker run does not use %s: %v", tc.image, runs[0])
			}
		})
	}
}

func TestSC05ImageTagsFollowTheBinaryAndListedImagesAreNotImported(t *testing.T) {
	e := scNew(t)
	e.container("joulie-sim")
	agentRef := "joulie.local/joulie-agent:dev-" + scSha12(scFakeBinary("agent", "0"))
	// The cluster's containerd lists the agent image already.
	e.write(filepath.Join(e.state, "docker", "ctr-images"), agentRef+"\n")
	e.mustRun("images.sh", nil, "--joulie")

	tag := regexp.MustCompile(`^joulie\.local/joulie-(agent|controller-manager|scheduler|simulator):dev-[0-9a-f]{12}$`)
	env := scEnvFile(t, filepath.Join(e.dir, "images.env"))
	for _, c := range []string{"agent", "controller-manager", "scheduler"} {
		key := "IMAGE_" + strings.ToUpper(strings.ReplaceAll(c, "-", "_"))
		want := "joulie.local/joulie-" + c + ":dev-" + scSha12(scFakeBinary(c, "0"))
		if env[key] != want {
			t.Errorf("%s = %q, want %q (the first 12 hex of the binary's sha256)", key, env[key], want)
		}
	}
	sim := scSha12(scSha12(scFakeBinary("simulator", "0")) + " " + scSha12(scFakeBinary("fakesmi", "0")) + "\n")
	if want := "joulie.local/joulie-simulator:dev-" + sim; env["IMAGE_SIMULATOR"] != want {
		t.Errorf("IMAGE_SIMULATOR = %q, want %q (simulator and fakesmi hashed together)", env["IMAGE_SIMULATOR"], want)
	}
	for k, v := range env {
		if strings.HasPrefix(k, "IMAGE_IDS_") {
			if v == "" || !strings.HasPrefix(v, "sha256:") {
				t.Errorf("%s = %q, want the image ID first", k, v)
			}
			continue
		}
		if strings.HasPrefix(k, "IMAGE_") && !tag.MatchString(v) {
			t.Errorf("%s = %q is not joulie.local/joulie-<c>:dev-<12 hex>", k, v)
		}
	}
	if env["GOTOOLCHAIN"] == "" || env["GO_VERSION"] != "go1.22.9" {
		t.Errorf("images.env records GOTOOLCHAIN %q and GO_VERSION %q", env["GOTOOLCHAIN"], env["GO_VERSION"])
	}

	saved := map[string]bool{}
	for _, c := range e.callsOf("docker", "save") {
		if !c.has("--help") {
			saved[c.args()[len(c.args())-1]] = true
		}
	}
	if saved[agentRef] {
		t.Errorf("the listed agent image was imported again")
	}
	for _, k := range []string{"IMAGE_CONTROLLER_MANAGER", "IMAGE_SCHEDULER", "IMAGE_SIMULATOR"} {
		if !saved[env[k]] {
			t.Errorf("%s (%s) was not imported", k, env[k])
		}
	}

	// The build: linux/amd64 without cgo, with the go.mod toolchain.
	b, err := os.ReadFile(filepath.Join(e.state, "go-build-env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOTOOLCHAIN=go1.22.9") {
		t.Errorf("the image binaries were not built for linux/amd64 without cgo with go1.22.9:\n%s", b)
	}
	// Without a VCS stamp, so a tag follows the code and not the git state.
	for _, c := range e.callsOf("go", "build") {
		if !c.has("-buildvcs=false") {
			t.Errorf("the image binaries were built with a VCS stamp: %v", c)
		}
	}

	// A second run with nothing changed builds and imports nothing.
	e.resetCalls()
	e.mustRun("images.sh", nil, "--joulie")
	if c := e.callsOf("docker", "build"); len(c) != 0 {
		t.Errorf("unchanged binaries were rebuilt: %v", c)
	}
	for _, c := range e.callsOf("docker", "save") {
		if !c.has("--help") {
			t.Errorf("an unchanged image was imported again: %v", c)
		}
	}
}

func TestSC06CRDsBeforeTheFirstHelmUpgrade(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	e.resetCalls()
	e.mustRun("deploy.sh", nil)
	crds, upgrade := -1, -1
	for i, c := range e.calls() {
		if crds < 0 && c.tool() == "kubectl" && len(c.args()) > 0 && c.args()[0] == "apply" && c.has("--server-side") {
			for _, f := range c.after("-f") {
				if strings.HasSuffix(f, "charts/joulie/crds/") {
					crds = i
				}
			}
		}
		if upgrade < 0 && c.tool() == "helm" && len(c.args()) > 0 && c.args()[0] == "upgrade" {
			upgrade = i
		}
	}
	if crds < 0 || upgrade < 0 {
		t.Fatalf("deploy.sh applied the CRDs at call %d and ran helm upgrade at call %d", crds, upgrade)
	}
	if crds > upgrade {
		t.Errorf("the CRDs were applied (call %d) after the first helm upgrade (call %d)", crds, upgrade)
	}
	if !scExists(filepath.Join(e.dir, "crds.sha")) || !scExists(filepath.Join(e.dir, "deploy.env")) {
		t.Errorf("deploy.sh wrote no crds.sha or deploy.env")
	}
}

// scPods writes the Joulie namespace's pods: one Running pod per image, each
// reporting imageID.
func (e *scEnv) scPods(images map[string]string) {
	var items []string
	i := 0
	for image, id := range images {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"pod-%d","namespace":"joulie-system"},
			"spec":{"containers":[{"name":"main","image":%q}]},
			"status":{"phase":"Running","containerStatuses":[{"name":"main","imageID":%q}]}}`, i, image, id))
		i++
	}
	e.write(filepath.Join(e.state, "kube", "get-pods-joulie-system.json"), `{"items":[`+strings.Join(items, ",")+`]}`)
}

func TestSC07MismatchedImageIDFails(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	e.mustRun("deploy.sh", nil)
	env := scEnvFile(t, filepath.Join(e.dir, "images.env"))
	ids := strings.Split(env["IMAGE_IDS_AGENT"], ",")
	if len(ids) == 0 || ids[0] == "" {
		t.Fatalf("images.env has no IMAGE_IDS_AGENT")
	}

	// Every recorded ID of an image is accepted.
	for _, id := range ids {
		e.scPods(map[string]string{env["IMAGE_AGENT"]: id})
		if out, err := e.run("status.sh", nil); err != nil {
			t.Errorf("status.sh failed with the recorded imageID %s: %v\n%s", id, err, out)
		}
	}

	for name, pods := range map[string]map[string]string{
		"other-id":    {env["IMAGE_AGENT"]: "sha256:" + strings.Repeat("0", 64)},
		"other-image": {"registry.example.com/joulie-agent:v0.2.0": ids[0]},
	} {
		e.scPods(pods)
		for script, args := range map[string][]string{"status.sh": nil, "deploy.sh": {"--skip-images"}} {
			out := e.mustExit(1, script, nil, args...)
			if !strings.Contains(out, "joulie-system/pod-0 main:") {
				t.Errorf("%s: %s did not name the container:\n%s", name, script, out)
			}
		}
	}
}

func TestSC08MakeTargetsOnlyRunTheScripts(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	probe := exec.Command("make", "-n", "-C", scRepo, "simcluster-up")
	if out, err := probe.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "No rule to make target") {
			t.Skip("the Makefile has no simcluster targets yet (S3-WP6 adds them)")
		}
		t.Fatalf("make -n simcluster-up: %v\n%s", err, out)
	}
	for _, x := range []string{"up", "images", "deploy", "reload", "status", "tree", "logs", "smoke", "down"} {
		cmd := exec.Command("make", "-s", "-n", "-C", scRepo, "simcluster-"+x)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("make -n simcluster-%s: %v\n%s", x, err, out)
			continue
		}
		want := regexp.MustCompile(`^bash scripts/simcluster/` + regexp.QuoteMeta(x) + `\.sh( |$)`)
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Errorf("make -n simcluster-%s prints nothing", x)
		}
		for _, l := range lines {
			if !want.MatchString(strings.TrimSpace(l)) {
				t.Errorf("make -n simcluster-%s prints %q, want only bash scripts/simcluster/%s.sh ...", x, l, x)
			}
		}
	}
}

func TestSC09ScriptsParseAndFailFast(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	scripts, err := filepath.Glob(filepath.Join(scRepo, "scripts/simcluster/*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lib.sh", "up.sh", "down.sh", "images.sh", "deploy.sh", "reload.sh", "status.sh", "tree.sh", "logs.sh", "smoke.sh"}
	have := map[string]bool{}
	for _, s := range scripts {
		have[filepath.Base(s)] = true
		if out, err := exec.Command("bash", "-n", s).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", s, err, out)
		}
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`(?m)^set -euo pipefail$`).Match(b) {
			t.Errorf("%s does not set -euo pipefail", s)
		}
		if !bytes.HasPrefix(b, []byte("#!/usr/bin/env bash\n")) {
			t.Errorf("%s does not start with #!/usr/bin/env bash", s)
		}
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("scripts/simcluster/%s is missing", w)
		}
	}
	shellcheck, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Log("shellcheck is not installed; only bash -n ran")
		return
	}
	args := append([]string{"-x", "-P", filepath.Join(scRepo, "scripts/simcluster")}, scripts...)
	if out, err := exec.Command(shellcheck, args...).CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

func TestSC10DefaultDirIsOutsideTheRepository(t *testing.T) {
	e := scNew(t)
	repo, err := filepath.Abs(scRepo)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(e.repo, "scripts/simcluster/probe.sh")
	e.write(probe, "#!/usr/bin/env bash\nset -euo pipefail\nsource \"$(dirname \"${BASH_SOURCE[0]}\")/lib.sh\"\nsim_init\nprintf '%s\\n' \"$SIMCLUSTER_DIR\"\n")
	xdg := filepath.Join(t.TempDir(), "xdg")
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{"home", map[string]string{"SIMCLUSTER_DIR": ""}, filepath.Join(e.home, ".cache/joulie/simcluster/joulie-sim")},
		{"xdg", map[string]string{"SIMCLUSTER_DIR": "", "XDG_CACHE_HOME": xdg}, filepath.Join(xdg, "joulie/simcluster/joulie-sim")},
		{"named", map[string]string{"SIMCLUSTER_DIR": "", "SIMCLUSTER_NAME": "joulie-sim-smoke"}, filepath.Join(e.home, ".cache/joulie/simcluster/joulie-sim-smoke")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := strings.TrimSpace(e.mustRun("probe.sh", tc.extra))
			if out != tc.want {
				t.Errorf("SIMCLUSTER_DIR = %q, want %q", out, tc.want)
			}
			for _, r := range []string{repo, e.repo} {
				if rel, err := filepath.Rel(r, out); err == nil && !strings.HasPrefix(rel, "..") {
					t.Errorf("SIMCLUSTER_DIR %s is inside the repository %s", out, r)
				}
			}
		})
	}
	// The default holds in the scripts themselves, not only in this probe.
	lib, err := os.ReadFile(filepath.Join(scRepo, "scripts/simcluster/lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lib), `SIMCLUSTER_DIR=${SIMCLUSTER_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/joulie/simcluster/$SIMCLUSTER_NAME}`) {
		t.Errorf("lib.sh does not default SIMCLUSTER_DIR to the user's cache directory")
	}
}

// scFullRun brings a fake cluster up, deploys, checks its status and
// deletes it.
func scFullRun(t *testing.T, e *scEnv) {
	t.Helper()
	e.mustRun("up.sh", nil)
	e.mustRun("deploy.sh", nil, "-f", "tests/simcluster/smoke-values.yaml")
	e.mustRun("status.sh", nil)
	e.mustRun("reload.sh", nil, "all", "--force")
	e.mustRun("down.sh", nil)
}

func TestSC11NoKindAndNoKubeHome(t *testing.T) {
	e := scNew(t)
	scFullRun(t, e)
	if c := e.callsOf("kind"); len(c) != 0 {
		t.Errorf("the scripts ran kind: %v", c)
	}
	if scExists(filepath.Join(e.home, ".kube")) {
		t.Errorf("the scripts created %s", filepath.Join(e.home, ".kube"))
	}
	b, err := os.ReadFile(filepath.Join(e.state, "tool-env"))
	if err != nil {
		t.Fatal(err)
	}
	// Each line is the KUBECONFIG and KUBECACHEDIR of one call; without
	// KUBECACHEDIR both tools cache under ~/.kube/cache.
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != filepath.Join(e.dir, "kubeconfig") || !strings.HasPrefix(f[1], e.dir+"/") {
			t.Errorf("kubectl or helm ran with KUBECONFIG and KUBECACHEDIR %q, want %s and a directory under %s",
				line, filepath.Join(e.dir, "kubeconfig"), e.dir)
		}
	}
	// No script calls kind, or names ~/.kube.
	kind := regexp.MustCompile(`(^|[\s;&|(])kind\s`)
	scripts, _ := filepath.Glob(filepath.Join(scRepo, "scripts/simcluster/*.sh"))
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") {
				continue
			}
			if kind.MatchString(code) {
				t.Errorf("%s:%d calls kind: %s", s, i+1, code)
			}
			if strings.Contains(code, "/.kube") {
				t.Errorf("%s:%d names ~/.kube: %s", s, i+1, code)
			}
		}
	}
}

func TestSC12PrometheusOnlyThroughTheProxy(t *testing.T) {
	e := scNew(t)
	scFullRun(t, e)
	proxied := 0
	for _, c := range e.callsOf("kubectl") {
		for _, raw := range c.after("--raw") {
			if strings.Contains(raw, "prometheus") {
				if !strings.HasPrefix(raw, "/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy/") {
					t.Errorf("Prometheus reached outside the service proxy: %s", raw)
				}
				proxied++
			}
		}
		if c.has("port-forward") || c.has("proxy") {
			t.Errorf("kubectl %v opens a local port", c.args())
		}
	}
	if proxied == 0 {
		t.Errorf("no Prometheus request went through the service proxy")
	}
	runs := e.callsOf("docker", "run")
	if len(runs) != 1 {
		t.Fatalf("got %d docker run calls, want 1", len(runs))
	}
	if p := runs[0].after("-p"); len(p) != 1 || p[0] != "127.0.0.1::6443" {
		t.Errorf("docker run publishes %v, want only 127.0.0.1::6443", p)
	}
	for _, a := range runs[0].args() {
		if a == "-P" || strings.HasPrefix(a, "--publish") || strings.HasPrefix(a, "--network") || strings.HasPrefix(a, "--net=") {
			t.Errorf("docker run passes %s", a)
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(scRepo, "scripts/simcluster/*.sh"))
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("port-forward")) {
			t.Errorf("%s uses port-forward", s)
		}
	}
}

func TestSC13DownRemovesAnonymousVolumes(t *testing.T) {
	e := scNew(t)
	e.container("joulie-sim")
	for _, d := range []string{"images", "runs", "values"} {
		e.write(filepath.Join(e.dir, d, "keep"), "")
	}
	e.write(filepath.Join(e.dir, "kubeconfig"), "x")
	e.mustRun("down.sh", nil)
	rm := e.callsOf("docker", "rm")
	if len(rm) != 1 || !rm[0].has("-f", "-v", "joulie-sim") {
		t.Errorf("docker rm calls = %v, want one docker rm -f -v joulie-sim", rm)
	}
	vols := map[string]bool{}
	for _, c := range e.callsOf("docker", "volume", "rm") {
		for _, v := range c.args()[2:] {
			vols[v] = true
		}
	}
	if !vols["joulie-sim-k3s"] || !vols["joulie-sim-hwemu"] {
		t.Errorf("down.sh removed the volumes %v, want joulie-sim-k3s and joulie-sim-hwemu", vols)
	}
	for _, d := range []string{"images", "runs"} {
		if !scExists(filepath.Join(e.dir, d, "keep")) {
			t.Errorf("down.sh removed %s", d)
		}
	}
	for _, f := range []string{"values", "kubeconfig"} {
		if scExists(filepath.Join(e.dir, f)) {
			t.Errorf("down.sh kept %s", f)
		}
	}
	// A cluster that is gone already is not an error.
	e.mustRun("down.sh", nil)
}

func TestSC14PlacementRendersWithTheInstallValues(t *testing.T) {
	e := scNew(t)
	extra := filepath.Join(e.repo, "extra-values.yaml")
	e.write(extra, "{}\n")
	e.mustRun("up.sh", nil)
	e.resetCalls()
	e.mustRun("deploy.sh", map[string]string{"SIMCLUSTER_EXTRA_VALUES": extra}, "-f", "tests/simcluster/smoke-values.yaml")
	var template, install []string
	for _, c := range e.callsOf("helm") {
		switch {
		case c.args()[0] == "template":
			if !c.has("--set", "agent.mode=daemonset", "--show-only", "templates/agent-daemonset.yaml") {
				t.Errorf("the placement render is not the DaemonSet's: %v", c)
			}
			template = c.after("-f")
		case c.args()[0] == "upgrade" && c.has("joulie") && !c.has("joulie-sim"):
			install = c.after("-f")
		}
	}
	if len(install) == 0 {
		t.Fatalf("deploy.sh did not install Joulie")
	}
	if strings.Join(template, " ") != strings.Join(install, " ") {
		t.Errorf("helm template -f %v\nhelm upgrade  -f %v\nwant the same files in the same order", template, install)
	}
	for _, want := range []string{"values/simcluster.yaml", "agent-nodes.yaml", "joulie-values.yaml", "smoke-values.yaml", "extra-values.yaml"} {
		found := false
		for _, f := range install {
			if strings.HasSuffix(f, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("the install does not use %s: %v", want, install)
		}
	}
}

func TestSC15AirgapBundleBeforeDockerRun(t *testing.T) {
	content := "fake k3s air-gap bundle"
	sum := sha256.Sum256([]byte(content))

	e := scNew(t)
	e.setVersion("K3S_AIRGAP_SHA256", hex.EncodeToString(sum[:]))
	e.mustRun("up.sh", map[string]string{"SIMCLUSTER_AIRGAP": "1", "FAKE_AIRGAP_CONTENT": content})
	events, err := os.ReadFile(filepath.Join(e.state, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "k3s-airgap-images-amd64.tar.zst") {
		t.Errorf("docker run did not find the bundle in the images directory: %s", events)
	}
	curl, run := -1, -1
	for i, c := range e.calls() {
		if curl < 0 && c.tool() == "curl" {
			curl = i
		}
		if run < 0 && c.tool() == "docker" && c.args()[0] == "run" {
			run = i
		}
	}
	if curl < 0 || run < 0 || curl > run {
		t.Errorf("curl at call %d, docker run at call %d: want the download first", curl, run)
	}

	// A bundle whose sha256 differs stops before docker run.
	e2 := scNew(t)
	e2.setVersion("K3S_AIRGAP_SHA256", strings.Repeat("0", 64))
	out := e2.mustFail("up.sh", map[string]string{"SIMCLUSTER_AIRGAP": "1", "FAKE_AIRGAP_CONTENT": content})
	if !strings.Contains(out, "sha256 is not") {
		t.Errorf("want the sha256 message:\n%s", out)
	}
	if runs := e2.callsOf("docker", "run"); len(runs) != 0 {
		t.Errorf("docker run was issued with a bad bundle: %v", runs)
	}
}

func TestSC16ReloadOnlyWhenChangedOrForced(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	e.mustRun("deploy.sh", nil)
	// deploy.sh's helm commands, which a reload reruns.
	var installJoulie, installSim scCall
	for _, c := range e.callsOf("helm", "upgrade") {
		if c.has("joulie") {
			installJoulie = c
		}
		if c.has("joulie-sim") {
			installSim = c
		}
	}
	if installJoulie == nil || installSim == nil {
		t.Fatalf("deploy.sh did not install both releases")
	}
	env := scEnvFile(t, filepath.Join(e.dir, "images.env"))
	tag := func(ref string) string { return ref[strings.LastIndex(ref, ":")+1:] }
	e.write(filepath.Join(e.state, "helm-values-joulie.json"), fmt.Sprintf(
		`{"agent":{"image":{"tag":%q}},"controllerManager":{"image":{"tag":%q}},"schedulerExtender":{"image":{"tag":%q}}}`,
		tag(env["IMAGE_AGENT"]), tag(env["IMAGE_CONTROLLER_MANAGER"]), tag(env["IMAGE_SCHEDULER"])))
	e.write(filepath.Join(e.state, "helm-values-joulie-sim.json"), fmt.Sprintf(`{"image":{"tag":%q}}`, tag(env["IMAGE_SIMULATOR"])))

	reloadAnnotation := func() []scCall {
		var out []scCall
		for _, c := range e.callsOf("kubectl") {
			for _, p := range c.after("-p") {
				if strings.Contains(p, `"sim.joulie.io/reload"`) {
					out = append(out, c)
				}
			}
		}
		return out
	}

	// Unchanged binary, no force: no upgrade, no restart.
	e.resetCalls()
	out := e.mustRun("reload.sh", nil, "agent")
	if c := e.callsOf("helm", "upgrade"); len(c) != 0 {
		t.Errorf("an unchanged agent was upgraded: %v", c)
	}
	if c := reloadAnnotation(); len(c) != 0 {
		t.Errorf("an unchanged agent was restarted: %v", c)
	}
	if !strings.Contains(out, "nothing to reload") {
		t.Errorf("reload.sh did not say there was nothing to do:\n%s", out)
	}

	// --force restarts the agent's workloads through the pod template
	// annotation, still without an upgrade.
	e.resetCalls()
	e.mustRun("reload.sh", nil, "agent", "--force")
	if c := e.callsOf("helm", "upgrade"); len(c) != 0 {
		t.Errorf("a forced reload of an unchanged agent ran helm upgrade: %v", c)
	}
	forced := reloadAnnotation()
	if len(forced) != 1 || !forced[0].has("patch", "deployment/joulie-agent-emu-0") {
		t.Errorf("reload --force patched %v, want deployment/joulie-agent-emu-0", forced)
	}
	// So does SIMCLUSTER_FORCE_RELOAD, with the component from C.
	e.resetCalls()
	e.mustRun("reload.sh", map[string]string{"SIMCLUSTER_FORCE_RELOAD": "1", "C": "scheduler"})
	if forced := reloadAnnotation(); len(forced) != 1 || !forced[0].has("deployment/joulie-scheduler-extender") {
		t.Errorf("SIMCLUSTER_FORCE_RELOAD=1 C=scheduler patched %v", forced)
	}

	// Every binary changed (shared code, say), but only the agent is
	// reloaded: deploy.sh's helm command again, with only the agent's tag
	// changed and no annotation without --force. The other components keep
	// their images.env entries, so their pods, still on the images of the
	// deploy, pass the image check of reload.sh and status.sh.
	newAgent := "joulie.local/joulie-agent:dev-" + scSha12(scFakeBinary("agent", "1"))
	e.scPods(map[string]string{
		newAgent:                        "sha256:" + scSha256(newAgent),
		env["IMAGE_CONTROLLER_MANAGER"]: strings.Split(env["IMAGE_IDS_CONTROLLER_MANAGER"], ",")[0],
		env["IMAGE_SCHEDULER"]:          strings.Split(env["IMAGE_IDS_SCHEDULER"], ",")[0],
	})
	e.resetCalls()
	e.mustRun("reload.sh", map[string]string{"FAKE_BUILD_SALT": "1"}, "agent")
	up := e.callsOf("helm", "upgrade")
	if len(up) != 1 {
		t.Fatalf("helm upgrade calls = %v, want one", up)
	}
	want := strings.Replace(installJoulie.String(), "agent.image.tag="+tag(env["IMAGE_AGENT"]), "agent.image.tag="+tag(newAgent), 1)
	if up[0].String() != want {
		t.Errorf("reload.sh agent ran\n  %v\nwant deploy.sh's command with only the agent's tag changed\n  %s", up[0], want)
	}
	if c := reloadAnnotation(); len(c) != 0 {
		t.Errorf("a reload without --force set the annotation: %v", c)
	}
	after := scEnvFile(t, filepath.Join(e.dir, "images.env"))
	if after["IMAGE_AGENT"] != newAgent {
		t.Errorf("images.env IMAGE_AGENT = %q, want %q", after["IMAGE_AGENT"], newAgent)
	}
	for _, k := range []string{"IMAGE_CONTROLLER_MANAGER", "IMAGE_IDS_CONTROLLER_MANAGER", "IMAGE_SCHEDULER", "IMAGE_SIMULATOR"} {
		if after[k] != env[k] {
			t.Errorf("reload.sh agent changed images.env %s from %q to %q", k, env[k], after[k])
		}
	}
	e.mustRun("status.sh", nil)

	// The simulator's release likewise: deploy.sh's command, values files
	// and spec included, with only the simulator's tag changed.
	e.resetCalls()
	e.mustRun("reload.sh", map[string]string{"FAKE_BUILD_SALT": "1"}, "simulator")
	up = e.callsOf("helm", "upgrade")
	after = scEnvFile(t, filepath.Join(e.dir, "images.env"))
	want = strings.Replace(installSim.String(), "image.tag="+tag(env["IMAGE_SIMULATOR"]), "image.tag="+tag(after["IMAGE_SIMULATOR"]), 1)
	if len(up) != 1 || up[0].String() != want || after["IMAGE_SIMULATOR"] == env["IMAGE_SIMULATOR"] {
		t.Errorf("reload.sh simulator ran %v\nwant one call\n  %s", up, want)
	}
}

// scSha256 is the hex sha256 of s, the image ID the fake crictl reports for
// an image reference.
func scSha256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSCTimingFilesCoverFourExportIntervals(t *testing.T) {
	keys := []string{"CONTROLLER_RECONCILE_INTERVAL", "AGENT_RECONCILE_INTERVAL", "AGENT_SPEC_READ_TIMEOUT",
		"PROM_SCRAPE_INTERVAL", "OTLP_EXPORT_INTERVAL_MS", "RATE_WINDOW", "FACILITY_POLL_INTERVAL"}
	for _, name := range []string{"dev", "production"} {
		env := scEnvFile(t, filepath.Join(scRepo, "scripts/simcluster/timing", name+".env"))
		if len(env) != len(keys) {
			t.Errorf("%s.env has %d keys, want %v", name, len(env), keys)
		}
		for _, k := range keys {
			if k == "OTLP_EXPORT_INTERVAL_MS" {
				if _, err := strconv.Atoi(env[k]); err != nil {
					t.Errorf("%s.env %s=%q is not a number of milliseconds", name, k, env[k])
				}
				continue
			}
			if _, err := time.ParseDuration(env[k]); err != nil {
				t.Errorf("%s.env %s=%q: %v", name, k, env[k], err)
			}
		}
		window, _ := time.ParseDuration(env["RATE_WINDOW"])
		ms, _ := strconv.Atoi(env["OTLP_EXPORT_INTERVAL_MS"])
		if window < 4*time.Duration(ms)*time.Millisecond {
			t.Errorf("%s.env RATE_WINDOW %v covers fewer than 4 export intervals of %d ms", name, window, ms)
		}
		if window < 10*time.Second {
			t.Errorf("%s.env RATE_WINDOW %v is under the controller's 10 s minimum", name, window)
		}
	}
}

func TestSCValuesFilesUseLocalImages(t *testing.T) {
	read := func(path string) map[string]interface{} {
		b, err := os.ReadFile(filepath.Join(scRepo, path))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]interface{}
		if err := yaml.UnmarshalStrict(b, &v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !strings.Contains(string(b), "values/joulie.yaml") && path == "values/simcluster.yaml" {
			t.Errorf("%s does not say it is never combined with values/joulie.yaml", path)
		}
		return v
	}
	get := func(v map[string]interface{}, path string) interface{} {
		var cur interface{} = v
		for _, k := range strings.Split(path, ".") {
			m, ok := cur.(map[string]interface{})
			if !ok {
				return nil
			}
			cur = m[k]
		}
		return cur
	}
	joulie := read("values/simcluster.yaml")
	for path, want := range map[string]interface{}{
		"agent.mode":                         "emulated",
		"agent.image.repository":             "joulie.local/joulie-agent",
		"agent.image.pullPolicy":             "IfNotPresent",
		"controllerManager.image.repository": "joulie.local/joulie-controller-manager",
		"controllerManager.image.pullPolicy": "IfNotPresent",
		"schedulerExtender.enabled":          true,
		"schedulerExtender.hostNetwork":      true,
		"schedulerExtender.image.repository": "joulie.local/joulie-scheduler",
		"schedulerExtender.image.pullPolicy": "IfNotPresent",
	} {
		if got := get(joulie, path); got != want {
			t.Errorf("values/simcluster.yaml %s = %v, want %v", path, got, want)
		}
	}
	for _, path := range []string{"controllerManager.nodeSelector", "schedulerExtender.nodeSelector"} {
		sel, _ := get(joulie, path).(map[string]interface{})
		if sel["joulie.io/infra"] != "true" {
			t.Errorf("values/simcluster.yaml %s = %v, want joulie.io/infra: \"true\"", path, sel)
		}
	}
	sim := read("values/simcluster-simulator.yaml")
	for path, want := range map[string]interface{}{
		"image.repository": "joulie.local/joulie-simulator",
		"image.pullPolicy": "IfNotPresent",
		"emulate.enabled":  true,
		"emulate.root":     "/var/lib/joulie-hwemu",
	} {
		if got := get(sim, path); got != want {
			t.Errorf("values/simcluster-simulator.yaml %s = %v, want %v", path, got, want)
		}
	}
	if sel, _ := get(sim, "nodeSelector").(map[string]interface{}); sel["joulie.io/infra"] != "true" {
		t.Errorf("values/simcluster-simulator.yaml nodeSelector = %v, want joulie.io/infra: \"true\"", sel)
	}
}

func TestSCStageOverridesApplyAfterKWOKInLexicalOrder(t *testing.T) {
	e := scNew(t)
	for _, f := range []string{"stage-pod-complete.yaml", "stage-a.yaml", "not-a-stage.yaml"} {
		e.write(filepath.Join(e.repo, "scripts/simcluster/manifests", f), "# stub\n")
	}
	e.mustRun("up.sh", nil)
	var applied []string
	for _, c := range e.callsOf("kubectl") {
		if len(c.args()) == 0 || c.args()[0] != "apply" {
			continue
		}
		for _, f := range c.after("-f") {
			if strings.Contains(f, "stage") || strings.HasSuffix(f, "kwok.yaml") {
				applied = append(applied, filepath.Base(f))
			}
		}
	}
	want := []string{"kwok.yaml", "stage-fast.yaml", "stage-a.yaml", "stage-pod-complete.yaml"}
	if strings.Join(applied, " ") != strings.Join(want, " ") {
		t.Errorf("applied %v, want %v", applied, want)
	}
}

func TestSCStatusReportsEachProblem(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	e.mustRun("deploy.sh", nil)
	e.mustRun("status.sh", nil)
	k := filepath.Join(e.state, "kube")
	for _, tc := range []struct{ name, file, content, want string }{
		{"not ready", "get-nodes.json", `{"items":[{"kind":"Node","metadata":{"name":"intel-0",
			"labels":{"joulie.io/managed":"true"},"annotations":{"kwok.x-k8s.io/node":"fake"}},
			"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`,
			"Node intel-0 is not Ready"},
		{"stray pod", "get-pods-all.json", `{"items":[
			{"metadata":{"name":"web-0","namespace":"default"},"spec":{"nodeName":"intel-0"}},
			{"metadata":{"name":"job-0","namespace":"default","annotations":{"joulie.io/workload-class":"standard"}},"spec":{"nodeName":"intel-0"}}]}`,
			"pod default/web-0 on intel-0 has no joulie.io/workload-class annotation and runs on a fake Node"},
		{"no NodeHardware", "get-nodehardwares.json", `{"items":[]}`,
			"Node intel-0 has an emulated agent and no NodeHardware"},
		{"no series", "prom.json", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"amd-0"},"value":[0,"1"]}]}}`,
			"Node intel-0 has no series in Prometheus"},
	} {
		path := filepath.Join(k, tc.file)
		old, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		e.write(path, tc.content)
		out := e.mustExit(1, "status.sh", nil)
		if !strings.Contains(out, "1 problem(s):\n  "+tc.want) {
			t.Errorf("%s: status.sh does not report only %q:\n%s", tc.name, tc.want, out)
		}
		if old == nil {
			err = os.Remove(path)
		} else {
			err = os.WriteFile(path, old, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSCDeployWaitsForHardwareSeriesAndTwins(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	// Each object deploy.sh waits for appears on its second read.
	k := filepath.Join(e.state, "kube")
	waits := []string{"get-nodehardwares.json", "prom.json", "get-nodetwins.json"}
	for _, f := range waits {
		if err := os.Rename(filepath.Join(k, f), filepath.Join(k, f+".later")); err != nil {
			t.Fatal(err)
		}
	}
	e.mustRun("deploy.sh", nil)
	for _, f := range waits {
		if scExists(filepath.Join(k, f+".later")) {
			t.Errorf("deploy.sh finished without waiting for %s", f)
		}
	}

	// A wait that times out fails and names what is still missing.
	e.write(filepath.Join(k, "get-nodetwins.json"), `{"items":[]}`)
	e.write(filepath.Join(e.repo, "scripts/simcluster/probe.sh"), "#!/usr/bin/env bash\nset -euo pipefail\n"+
		"source \"$(dirname \"${BASH_SOURCE[0]}\")/lib.sh\"\nsim_init\nsim_wait 0 \"a written NodeTwin\" sim_none sim_missing_twins\n")
	out := e.mustExit(1, "probe.sh", nil)
	if !strings.Contains(out, "timed out after 0s waiting for a written NodeTwin; last attempt:\nintel-0\n") {
		t.Errorf("a timed out wait does not name the Node still missing:\n%s", out)
	}
}

func TestSCUpDeletesNodesOfAnEarlierSpec(t *testing.T) {
	e := scNew(t)
	e.write(filepath.Join(e.state, "kube", "get-nodes.json"), `{"items":[
		{"kind":"Node","metadata":{"name":"intel-0","labels":{"sim.joulie.io/cluster":"smoke"}}},
		{"kind":"Node","metadata":{"name":"old-0","labels":{"sim.joulie.io/cluster":"other"}}}]}`)
	e.mustRun("up.sh", nil)
	if dels := e.callsOf("kubectl", "delete"); len(dels) != 1 || !dels[0].has("node/old-0") || dels[0].has("node/intel-0") {
		t.Errorf("up.sh deleted %v, want only node/old-0, which the spec does not name", dels)
	}
}

func TestSCUpRefusesSpecsThatNeedLocalProfiles(t *testing.T) {
	profiles := t.TempDir()
	// Built-in profiles only: a profiles directory is fine.
	scNew(t).mustRun("up.sh", map[string]string{"SIMCLUSTER_PROFILES_DIR": profiles})

	// A spec that needs a local profile stops before the container exists:
	// the in-cluster simulator could not resolve it.
	e := scNew(t)
	out := e.mustExit(1, "up.sh", map[string]string{"SIMCLUSTER_PROFILES_DIR": profiles, "FAKE_LOCAL_PROFILE": "lab-x"})
	if !strings.Contains(out, "the in-cluster simulator reads only its built-in profiles") {
		t.Errorf("want the local profile message:\n%s", out)
	}
	if runs := e.callsOf("docker", "run"); len(runs) != 0 {
		t.Errorf("docker run was issued for a spec the simulator cannot resolve: %v", runs)
	}
}

func TestSCSmokeRunsTheE2ETestInARunDirectory(t *testing.T) {
	e := scNew(t)
	start := time.Now().UTC().Truncate(time.Second)
	e.mustExit(3, "smoke.sh", map[string]string{"FAKE_GO_TEST_RC": "3"})

	// runs/latest names the run directory, named as scenario.RunDirName
	// names it, with <spec>-<mode> as the scenario.
	runs := filepath.Join(e.dir, "runs")
	name, err := os.Readlink(filepath.Join(runs, "latest"))
	if err != nil {
		t.Fatal(err)
	}
	sha, dirty := "nogit", false
	if b, err := exec.Command("git", "-C", e.repo, "rev-parse", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(b))
		st, _ := exec.Command("git", "-C", e.repo, "status", "--porcelain").Output()
		dirty = len(bytes.TrimSpace(st)) > 0
	}
	var ts time.Time
	if len(name) >= 16 {
		ts, err = time.Parse("20060102T150405Z", name[:16])
	}
	if err != nil || ts.Before(start) || ts.After(time.Now()) {
		t.Fatalf("run directory %q does not start with the run's UTC start time (%v)", name, err)
	}
	if want := scenario.RunDirName(ts, "smoke-otel", sha, dirty); name != want {
		t.Errorf("run directory %q, want %q", name, want)
	}
	runDir := filepath.Join(runs, name)

	// The end-to-end test ran once, writing into the run directory.
	b, err := os.ReadFile(filepath.Join(e.state, "go-test-run-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != runDir {
		t.Errorf("TestSimclusterE2E ran with SIMCLUSTER_RUN_DIR %q, want %q", got, runDir)
	}
	if c := e.callsOf("go", "test"); len(c) != 1 ||
		!c[0].has("-tags", "simcluster", "-count=1", "./tests/simcluster/", "-run", "TestSimclusterE2E") {
		t.Errorf("go test calls = %v, want one run of TestSimclusterE2E with -tags simcluster", c)
	}
	for _, f := range []string{"smoke.log", "images.env", "deploy.env", "cluster.env", "k8s/nodetwins-end.yaml"} {
		if !scExists(filepath.Join(runDir, f)) {
			t.Errorf("the run directory has no %s", f)
		}
	}
	// The cluster is deleted, since SIMCLUSTER_KEEP is unset.
	if rm := e.callsOf("docker", "rm"); len(rm) != 1 || !rm[0].has("-f", "-v", "joulie-sim-smoke") {
		t.Errorf("docker rm calls = %v, want one removing joulie-sim-smoke", rm)
	}
}

// TestSC17ReloadKeepsTheScenarioSimulatorValues: a deploy with
// SIMCLUSTER_SIM_EXTRA_VALUES, a scenario's simulator.set (kepler-compat's
// emulate.exporters, say), records that file in deploy.env, so a later
// reload of the simulator, run without the variable, installs with it again
// instead of dropping it.
func TestSC17ReloadKeepsTheScenarioSimulatorValues(t *testing.T) {
	e := scNew(t)
	e.mustRun("up.sh", nil)
	set := filepath.Join(e.dir, "runs", "r1", "simulator-set.yaml")
	e.write(set, "emulate:\n  exporters:\n  - kepler\n")
	e.mustRun("deploy.sh", map[string]string{"SIMCLUSTER_SIM_EXTRA_VALUES": set})
	var installSim scCall
	for _, c := range e.callsOf("helm", "upgrade") {
		if c.has("joulie-sim") {
			installSim = c
		}
	}
	files := func(c scCall) string { return strings.Join(c.after("-f"), ",") }
	if installSim == nil || !strings.HasSuffix(files(installSim), ","+set) {
		t.Fatalf("deploy.sh installed the simulator with %v, want %s as the last values file", installSim, set)
	}
	if got := scEnvFile(t, filepath.Join(e.dir, "deploy.env"))["SIM_VALUES"]; got != files(installSim) {
		t.Errorf("deploy.env SIM_VALUES = %q, want the values files of the install, %q", got, files(installSim))
	}

	// The fake helm reports no image tag, so the reload upgrades: with
	// exactly deploy.sh's command, the scenario's file included.
	e.resetCalls()
	e.mustRun("reload.sh", nil, "simulator")
	up := e.callsOf("helm", "upgrade")
	if len(up) != 1 || up[0].String() != installSim.String() {
		t.Errorf("reload.sh simulator ran %v\nwant deploy.sh's command\n  %s", up, installSim)
	}
}

// TestSC18SmokeRunsTheScenarioOnTheSameCluster: with SIMCLUSTER_SCENARIOS=1,
// smoke.sh collects the end-to-end run, then runs the smoke scenario on the
// same cluster into the run directory's scenario-smoke/, then deletes the
// cluster. It exits with the test's status, else the scenario's.
func TestSC18SmokeRunsTheScenarioOnTheSameCluster(t *testing.T) {
	newEnv := func() *scEnv {
		e := scNew(t)
		e.copyIn(filepath.Join(scRepo, "tests/simcluster/scenarios/smoke.yaml"), "tests/simcluster/scenarios/smoke.yaml")
		return e
	}
	e := newEnv()
	e.mustExit(1, "smoke.sh", map[string]string{"SIMCLUSTER_SCENARIOS": "1", "FAKE_SCENARIO_RC": "1"})

	// runs/latest still names the smoke run: scenario.sh got its directory.
	runs := filepath.Join(e.dir, "runs")
	name, err := os.Readlink(filepath.Join(runs, "latest"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(name, "-smoke-otel-") {
		t.Fatalf("runs/latest = %q, want the smoke run", name)
	}
	runDir := filepath.Join(runs, name)
	scenarioDir := filepath.Join(runDir, "scenario-smoke")
	scenarioFile := filepath.Join(e.repo, "tests/simcluster/scenarios/smoke.yaml")
	if c := e.callsOf("simscenario", "plan", scenarioFile); len(c) != 1 || !c[0].has("-out", scenarioDir) {
		t.Errorf("simscenario plan calls = %v, want one of %s into %s", c, scenarioFile, scenarioDir)
	}
	// The trace reached the simulator's run directory and the run's OTLP
	// file came back.
	for _, f := range []string{"scenario.log", "variant.env", "trace.jsonl", "summary.json", "otlp/simulator.metrics.jsonl"} {
		if !scExists(filepath.Join(scenarioDir, f)) {
			t.Errorf("scenario-smoke has no %s", f)
		}
	}

	// The scenario deployed again, without the smoke values; the run
	// directory's deploy.env is the end-to-end run's, so it was collected
	// first.
	var joulie []scCall
	for _, c := range e.callsOf("helm", "upgrade") {
		if c.has("joulie") {
			joulie = append(joulie, c)
		}
	}
	smokeValues := filepath.Join(e.repo, "tests/simcluster/smoke-values.yaml")
	if len(joulie) != 2 || !joulie[0].has(smokeValues) || joulie[1].has(smokeValues) {
		t.Errorf("Joulie installs = %v, want the smoke deploy with %s, then the scenario's without it", joulie, smokeValues)
	}
	if v := scEnvFile(t, filepath.Join(runDir, "deploy.env"))["JOULIE_VALUES"]; !strings.Contains(v, smokeValues) {
		t.Errorf("the run directory's deploy.env has JOULIE_VALUES %q, want the end-to-end run's, with %s", v, smokeValues)
	}

	// Order: the end-to-end test, the scenario, then the cluster deleted.
	first := func(tool string, prefix ...string) int {
		for i, c := range e.calls() {
			if c.tool() == tool && len(c.args()) >= len(prefix) && strings.Join(c.args()[:len(prefix)], " ") == strings.Join(prefix, " ") {
				return i
			}
		}
		return -1
	}
	goTest, plan, summarize, rm := first("go", "test"), first("simscenario", "plan"), first("simscenario", "summarize"), first("docker", "rm")
	if goTest < 0 || plan < goTest || summarize < plan || rm < summarize {
		t.Errorf("call order go test %d, simscenario plan %d, simscenario summarize %d, docker rm %d: want increasing", goTest, plan, summarize, rm)
	}

	// A failed end-to-end test sets the status; the scenario still runs.
	e = newEnv()
	e.mustExit(3, "smoke.sh", map[string]string{"SIMCLUSTER_SCENARIOS": "1", "FAKE_SCENARIO_RC": "1", "FAKE_GO_TEST_RC": "3"})
	if c := e.callsOf("simscenario", "summarize"); len(c) != 1 {
		t.Errorf("after a failed end-to-end test, simscenario summarize calls = %v, want one", c)
	}
	// Without SIMCLUSTER_SCENARIOS no scenario runs.
	e = newEnv()
	e.mustRun("smoke.sh", nil)
	if c := e.callsOf("simscenario"); len(c) != 0 {
		t.Errorf("smoke.sh without SIMCLUSTER_SCENARIOS ran %v", c)
	}
}
