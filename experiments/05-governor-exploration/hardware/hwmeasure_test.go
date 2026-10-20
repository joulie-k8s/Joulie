package hardware

// Tests of hwmeasure.sh and probe.sh. They are hermetic: a fake kubectl
// answers the node and pod queries from files and, for "create -f", runs
// probe.sh as a local process over a synthetic host (a sysfs tree and a
// cpuinfo in a temporary directory), with fake stress-ng and nvidia-smi
// first on PATH; "exec" runs probe.sh's other subcommands against the same
// output directory, and "delete pod" sends the probe TERM, as the kubelet
// does. Nothing reaches a cluster, and nothing reads the machine's own
// sysfs. They need bash, jq, setsid and GNU coreutils, and skip without
// them. Every value is neutral.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

const (
	hmNode = "node-a"
	hmCtx  = "ctx-a"
	hmBDF  = "0000:c1:00.0"
	hmBus  = "0000:3b:00.0"
)

// hmFakeKubectl is the fake kubectl. FAKE_STATE holds its files: node.json
// and pods.json (the answers), kubectl.log (one line per call), cm/ (the
// ConfigMap's files), pod.env and pod.log (the running probe's environment
// and output) and probe.pid.
const hmFakeKubectl = `#!/usr/bin/env bash
set -u
S=$FAKE_STATE
printf '%s\n' "$*" >>"$S/kubectl.log"
args=()
while (($#)); do
  case $1 in
    --kubeconfig | --context) shift 2 ;;
    --kubeconfig=* | --context=* | --request-timeout=*) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
podenv() {
  local kv
  envs=(HW_OUT="$S/out" HW_SYS="$FAKE_ROOT/sys" HW_HOST="$FAKE_ROOT/host" HW_TRACEFS=none HW_PROBE_DIR="$S/cm" FAKE_NV="$S/nv")
  while IFS= read -r kv; do envs+=("$kv"); done <"$S/pod.env"
}
case "$1 $2" in
  "config get-contexts") printf '%s\n' ${FAKE_CONTEXTS:-ctx-a} ;;
  "get node") cat "$S/node.json" ;;
  "get pods") cat "$S/pods.json" ;;
  "get pod")
    # FAKE_POD_PENDING_S: the API reports Pending that long after the
    # create, although the probe already runs (a status that lags).
    if [[ ${FAKE_POD_START:-ok} != ok ]]; then
      echo "Pending  ErrImagePull"
    elif (( $(date +%s) - $(stat -c %Y "$S/pod.yaml") < ${FAKE_POD_PENDING_S:-0} )); then
      echo "Pending  ContainerCreating"
    else
      echo "Running  "
    fi
    ;;
  "get events") ;;
  "create configmap")
    mkdir -p "$S/cm"
    for a in "$@"; do
      case $a in --from-file=*) kv=${a#--from-file=}; cp "${kv#*=}" "$S/cm/${kv%%=*}" ;; esac
    done
    ;;
  "label configmap" | "delete configmap") ;;
  "create -f")
    cp "$3" "$S/pod.yaml"
    [[ ${FAKE_POD_START:-ok} == ok ]] || exit 0
    pargs=$(sed -n 's/^    args: //p' "$3")
    sed -n 's/^    env: //p' "$3" | jq -r '.[] | "\(.name)=\(.value)"' >"$S/pod.env"
    mapfile -t a < <(jq -r '.[]' <<<"$pargs")
    rm -rf "$S/out" "$S/pod.deleted"
    mkdir -p "$S/out"
    podenv
    env "${envs[@]}" setsid bash "$S/cm/probe.sh" run "${a[@]}" </dev/null >"$S/pod.log" 2>&1 &
    echo $! >"$S/probe.pid"
    ;;
  "logs -f") exec tail --pid="$(cat "$S/probe.pid")" -n +1 -f "$S/pod.log" ;;
  "exec -n")
    # As kubectl exec, it fails unless the container runs.
    if [[ ! -f $S/probe.pid ]] || ! kill -0 "$(cat "$S/probe.pid")" 2>/dev/null; then
      echo "error: unable to upgrade connection: container not found (\"probe\")" >&2
      exit 1
    fi
    while [[ $1 != -- ]]; do shift; done
    shift 3
    podenv
    exec env "${envs[@]}" bash "$S/cm/probe.sh" "$@"
    ;;
  "delete pod")
    p=$(cat "$S/probe.pid")
    kill -TERM "$p" 2>/dev/null
    for i in $(seq 1 100); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$p" 2>/dev/null
    echo deleted >"$S/pod.deleted"
    ;;
  *) echo "fake kubectl: unexpected call: $*" >&2; exit 9 ;;
esac
`

// hmFakeStress sleeps for its -t seconds; FAKE_STRESS_FAIL=1 makes it fail
// at once, as a stress-ng refused by the kernel would.
const hmFakeStress = `#!/bin/sh
[ "${FAKE_STRESS_FAIL:-0}" = 1 ] && { echo "stress-ng: fake failure" >&2; exit 3; }
t=1
while [ $# -gt 0 ]; do [ "$1" = -t ] && t=$2; shift; done
sleep "$t"
`

// hmFakeNvidiaSMI has two GPUs, 00000000:3B:00.0 and 00000000:5E:00.0. A
// graphics lock set with -lgc is kept in $FAKE_NV/lock.<bus id> and moves
// clocks.sm and power.draw, -rgc removes it; -lmc and -rmc do the same with
// mlock.<bus id> and clocks.mem; every call is appended to $FAKE_NV/calls.
const hmFakeNvidiaSMI = `#!/usr/bin/env bash
set -u
S=$FAKE_NV
mkdir -p "$S"
echo "$*" >>"$S/calls"
all=(00000000:3B:00.0 00000000:5E:00.0)
q= lms= fmt= id=
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case ${args[i]} in
    -i) id=${args[i+1]} ;;
    --query-gpu=*) q=${args[i]#--query-gpu=} ;;
    --format=*) fmt=${args[i]#--format=} ;;
    -lms) lms=${args[i+1]} ;;
  esac
done
sel=("${all[@]}")
if [[ -n $id ]]; then
  sel=()
  IFS=, read -r -a ids <<<"${id^^}"
  for x in "${ids[@]}"; do
    [[ " ${all[*]} " == *" $x "* ]] || { echo "No devices were found" >&2; exit 6; }
    sel+=("$x")
  done
fi
for ((i = 0; i < ${#args[@]}; i++)); do
  case ${args[i]} in
    --query-compute-apps=*) exit 0 ;;
    -lgc | -lmc)
      f=lock; [[ ${args[i]} == -lmc ]] && f=mlock
      for b in "${sel[@]}"; do echo "${args[i+1]}" >"$S/$f.$b"; echo "Clocks set to \"(${args[i+1]})\" for GPU $b"; done
      echo "All done."; exit 0 ;;
    -rgc | -rmc)
      f=lock; [[ ${args[i]} == -rmc ]] && f=mlock
      for b in "${sel[@]}"; do rm -f "$S/$f.$b"; done
      echo "All done."; exit 0 ;;
    -q)
      for b in "${sel[@]}"; do
        printf 'GPU %s\n    Supported Clocks\n        Memory                            : 9001 MHz\n            Graphics                      : 1980 MHz\n            Graphics                      : 1500 MHz\n            Graphics                      : 345 MHz\n        Memory                            : 405 MHz\n            Graphics                      : 345 MHz\n' "$b"
      done
      exit 0
      ;;
  esac
done
value() {
  local lock mlock
  lock=$(cat "$S/lock.$2" 2>/dev/null || echo none)
  mlock=$(cat "$S/mlock.$2" 2>/dev/null || echo none)
  case $1 in
    pci.bus_id) echo "$2" ;;
    timestamp) date '+%Y/%m/%d %H:%M:%S.%3N' ;;
    pstate) echo P0 ;;
    clocks.sm) if [[ $lock != none ]]; then echo "${lock%,*}"; else echo 345; fi ;;
    clocks.mem) if [[ $mlock != none ]]; then echo "${mlock%,*}"; else echo 405; fi ;;
    power.draw) if [[ $lock$mlock != nonenone ]]; then echo 90.50; else echo 70.25; fi ;;
    clocks_event_reasons.active)
      if [[ ${FAKE_NV_OLD:-0} == 1 ]]; then echo "Field \"$1\" is not a valid field to query." >&2; return 2; fi
      echo 0x0000000000000001
      ;;
    clocks_throttle_reasons.active) echo 0x0000000000000001 ;;
    power.limit) echo 700.00 ;;
    persistence_mode) echo Enabled ;;
    compute_mode) echo Default ;;
    mig.mode.current) echo Disabled ;;
    *) echo "Field \"$1\" is not a valid field to query." >&2; return 2 ;;
  esac
}
emit() {
  local line f v fs b
  IFS=, read -r -a fs <<<"$q"
  for b in "${sel[@]}"; do
    line=
    for f in "${fs[@]}"; do v=$(value "$f" "$b") || exit 2; line+="${line:+, }$v"; done
    echo "$line"
  done
}
[[ -n $q ]] || exit 0
[[ $fmt == *noheader* ]] || echo "${q//,/, }"
if [[ -n $lms ]]; then
  while :; do emit; sleep "$(awk -v m="$lms" 'BEGIN { print m / 1000 }')"; done
fi
emit
`

// hmFakePeercopy answers list, prints 12 copies at a rate that is lower
// while the destination holds no lock and runs no load, and sleeps for
// stream.
const hmFakePeercopy = `#!/usr/bin/env bash
case $1 in
  list) echo 'device index=0 bus=0000:3B:00.0 name="Neutral GPU"'; echo 'device index=1 bus=0000:5E:00.0 name="Neutral GPU"' ;;
  stream) shift; while (($#)); do [[ $1 == --seconds ]] && sleep "$2"; shift; done ;;
  copy)
    load=none dst=
    while (($#)); do case $1 in --receiver-load) load=$2 ;; --dst) dst=$2 ;; esac; shift; done
    b=$(tr '[:lower:]' '[:upper:]' <<<"0000${dst}")
    r=15.5
    if [[ $load == stream || -e $FAKE_NV/lock.$b || -e $FAKE_NV/mlock.$b ]]; then r=28.0; fi
    for i in $(seq 0 11); do echo "copy iter=$i bytes=67108864 ms=1.0 gbps=$r"; done
    echo "summary median_gbps=$r min_gbps=$r max_gbps=$r peer_access=1"
    ;;
esac
`

func hmNeed(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "setsid", "tac", "tail", "awk", "mountpoint"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
}

func hmWrite(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func hmRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hmHost writes the synthetic host under root: two acpi-cpufreq policies
// at schedutil on a 3-entry table, a RAPL package zone and its DRAM
// subzone, an energy_perf_bias, a cpuinfo with hwp flags, and one amdgpu
// device at auto with its hwmon cap files.
func hmHost(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"host/proc/cpuinfo": "processor\t: 0\nmodel name\t: Neutral CPU\nflags\t\t: fpu hwp hwp_epp\n\n" +
			"processor\t: 1\nmodel name\t: Neutral CPU\nflags\t\t: fpu hwp hwp_epp\n",
		"sys/class/powercap/intel-rapl:0/name":                                "package-0\n",
		"sys/class/powercap/intel-rapl:0/energy_uj":                           "1000000\n",
		"sys/class/powercap/intel-rapl:0/max_energy_range_uj":                 "262143328850\n",
		"sys/class/powercap/intel-rapl:0:0/name":                              "dram\n",
		"sys/class/powercap/intel-rapl:0:0/energy_uj":                         "5\n",
		"sys/bus/pci/devices/" + hmBDF + "/vendor":                            "0x1002\n",
		"sys/bus/pci/devices/" + hmBDF + "/device":                            "0x74a1\n",
		"sys/bus/pci/devices/" + hmBDF + "/power_dpm_force_performance_level": "auto\n",
		"sys/bus/pci/devices/" + hmBDF + "/pp_dpm_fclk":                       "0: 600Mhz *\n1: 1400Mhz\n",
		"sys/bus/pci/devices/" + hmBDF + "/pp_dpm_sclk":                       "0: 500Mhz *\n1: 2100Mhz\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/name":                 "amdgpu\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/power1_cap":           "750000000\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/power1_cap_min":       "0\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/power1_cap_max":       "750000000\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/power1_cap_default":   "750000000\n",
		"sys/bus/pci/devices/" + hmBDF + "/hwmon/hwmon3/power1_input":         "120000000\n",
	}
	for _, c := range []string{"0", "1"} {
		d := "sys/devices/system/cpu/cpufreq/policy" + c + "/"
		files[d+"scaling_governor"] = "schedutil\n"
		files[d+"scaling_available_governors"] = "conservative ondemand userspace powersave performance schedutil\n"
		files[d+"scaling_driver"] = "acpi-cpufreq\n"
		files[d+"scaling_cur_freq"] = "1500000\n"
		files[d+"scaling_min_freq"] = "1500000\n"
		files[d+"scaling_max_freq"] = "2400000\n"
		files[d+"cpuinfo_max_freq"] = "3700000\n"
		files[d+"scaling_available_frequencies"] = "2400000 1900000 1500000\n"
		files["sys/devices/system/cpu/cpu"+c+"/power/energy_perf_bias"] = "6\n"
	}
	hmWrite(t, root, files)
	for _, c := range []string{"0", "1"} {
		if err := os.Symlink("../cpufreq/policy"+c, filepath.Join(root, "sys/devices/system/cpu/cpu"+c+"/cpufreq")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../bus/pci/drivers/amdgpu", filepath.Join(root, "sys/bus/pci/devices", hmBDF, "driver")); err != nil {
		t.Fatal(err)
	}
}

const hmNodeJSON = `{"metadata": {"name": "node-a"}, "spec": {%s},
 "status": {"allocatable": {"cpu": "4", "amd.com/gpu": "1"}, "nodeInfo": {"osImage": "Neutral OS", "kernelVersion": "6.8.0"}}}`

func hmPodsJSON(pods ...string) string {
	return `{"items": [` + strings.Join(pods, ",") + `]}`
}

func hmDaemonPod(ns, name string) string {
	return fmt.Sprintf(`{"metadata": {"namespace": %q, "name": %q, "ownerReferences": [{"kind": "DaemonSet"}]}, "status": {"phase": "Running"}}`, ns, name)
}

func hmPlainPod(ns, name, phase string) string {
	return fmt.Sprintf(`{"metadata": {"namespace": %q, "name": %q, "ownerReferences": [{"kind": "ReplicaSet"}]}, "status": {"phase": %q}}`, ns, name, phase)
}

// hmEnv is one test's world: the host root, the fake state, the tools and
// a kubeconfig whose name is neutral.
type hmEnv struct {
	root, state, bin, out, kubeconfig string
	extraEnv                          []string
}

func hmSetup(t *testing.T, nodeSpec string, pods string) *hmEnv {
	t.Helper()
	hmNeed(t)
	e := &hmEnv{root: t.TempDir(), state: t.TempDir(), bin: t.TempDir(), out: t.TempDir()}
	hmHost(t, e.root)
	hmWrite(t, e.state, map[string]string{
		"node.json": fmt.Sprintf(hmNodeJSON, nodeSpec),
		"pods.json": pods,
	})
	for name, body := range map[string]string{"kubectl": hmFakeKubectl, "stress-ng": hmFakeStress, "nvidia-smi": hmFakeNvidiaSMI, "peercopy": hmFakePeercopy} {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.kubeconfig = filepath.Join(t.TempDir(), "lab-a.yaml")
	hmWrite(t, filepath.Dir(e.kubeconfig), map[string]string{"lab-a.yaml": "apiVersion: v1\nkind: Config\n"})
	t.Cleanup(func() { hmKillProbe(e) })
	return e
}

// hmKillProbe ends a probe a failed test left running.
func hmKillProbe(e *hmEnv) {
	b, err := os.ReadFile(filepath.Join(e.state, "probe.pid"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err == nil && pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (e *hmEnv) cmd(stdin string, args ...string) *exec.Cmd {
	c := exec.Command("bash", append([]string{"hwmeasure.sh"}, args...)...)
	env := []string{
		"PATH=" + e.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_STATE=" + e.state, "FAKE_ROOT=" + e.root,
		"XDG_CACHE_HOME=" + filepath.Join(e.out, "cache"), "HOME=" + e.out,
		"KUBECONFIG=/nonexistent/should-never-be-read",
	}
	env = append(env, e.extraEnv...)
	c.Env = env
	c.Stdin = strings.NewReader(stdin)
	return c
}

// run runs hwmeasure.sh to its end and returns its output and exit status.
func (e *hmEnv) run(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	c := e.cmd(stdin, args...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &exit):
		return buf.String(), exit.ExitCode()
	}
	t.Fatalf("run hwmeasure.sh: %v\n%s", err, buf.String())
	return "", 0
}

// base are the required arguments for MEASUREMENT on DEVICE.
func (e *hmEnv) base(m, device string) []string {
	return []string{m, "--kubeconfig", e.kubeconfig, "--context", hmCtx, "--node", hmNode, "--device", device, "--out", e.out}
}

func (e *hmEnv) kubectlLog(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(e.state, "kubectl.log"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *hmEnv) gov(t *testing.T, c string) string {
	return strings.TrimSpace(hmRead(t, filepath.Join(e.root, "sys/devices/system/cpu/cpufreq/policy"+c+"/scaling_governor")))
}

// runDir is the single run directory under the output root.
func (e *hmEnv) runDir(t *testing.T) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(e.out, "*Z-*"))
	if len(dirs) != 1 {
		t.Fatalf("want one run directory under %s, have %v", e.out, dirs)
	}
	return dirs[0]
}

const hmSchedulable = `"unschedulable": false`

// HM01: every required argument is checked before kubectl runs, and an
// option the measurement does not take, an unknown measurement and a
// device of the wrong kind are refused, with exit status 2.
func TestHM01ArgumentChecks(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	full := e.base("conservative-steps", "cpu1")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no kubeconfig", without(full, "--kubeconfig"), "--kubeconfig is required"},
		{"no context", without(full, "--context"), "--context is required"},
		{"no node", without(full, "--node"), "--node is required"},
		{"no device", without(full, "--device"), "--device is required"},
		{"wrong device kind", replace(full, "cpu1", hmBDF), "takes cpuN"},
		{"unknown measurement", append([]string{"no-such-measurement"}, full[1:]...), "unknown measurement"},
		{"option of another measurement", append(append([]string{}, full...), "--watts", "100"), "takes no --watts"},
		{"bad option value", append(append([]string{}, full...), "--loads", "30,170"), "--loads 30,170 does not match"},
		{"peer on a CPU measurement", append(append([]string{}, full...), "--peer", hmBDF), "takes no --peer"},
		{"receiver test without peer", e.base("hopper-receiver-gate", hmBus), "--peer is required"},
		{"image without digest", append(append([]string{}, full...), "--image", "example.com/tools:1.0"), "has no @sha256: digest"},
	}
	for _, c := range cases {
		out, code := e.run(t, "", c.args...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want 2 and %q in:\n%s", c.name, code, c.want, out)
		}
	}
	if log := e.kubectlLog(t); log != "" {
		t.Errorf("kubectl ran although every case was refused before it:\n%s", log)
	}
}

func without(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func replace(args []string, from, to string) []string {
	out := append([]string{}, args...)
	for i := range out {
		if out[i] == from {
			out[i] = to
		}
	}
	return out
}

// HM02: a kubeconfig whose path contains "prod" in any case, or that
// resolves to such a file through a link, is refused before kubectl runs,
// and KUBECONFIG is never consulted.
func TestHM02KubeconfigGuard(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	dir := t.TempDir()
	hmWrite(t, dir, map[string]string{"cluster-prod.yaml": "x\n", "PROD/cfg.yaml": "x\n", "real-Prod-b.yaml": "x\n"})
	link := filepath.Join(dir, "neutral.yaml")
	if err := os.Symlink(filepath.Join(dir, "real-Prod-b.yaml"), link); err != nil {
		t.Fatal(err)
	}
	for _, kc := range []string{filepath.Join(dir, "cluster-prod.yaml"), filepath.Join(dir, "PROD/cfg.yaml"), link} {
		args := replace(e.base("hwp-epb", "all-cpus"), e.kubeconfig, kc)
		out, code := e.run(t, "", append(args, "--yes")...)
		if code != 2 || !strings.Contains(out, `contains "prod"`) {
			t.Errorf("%s: exit %d, want 2 and the prod refusal:\n%s", kc, code, out)
		}
	}
	if log := e.kubectlLog(t); log != "" {
		t.Errorf("kubectl ran with a prod kubeconfig:\n%s", log)
	}
	// The neutral kubeconfig of the same run gets past the check and every
	// kubectl call names it and the context.
	if out, code := e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--dry-run")...); code != 0 {
		t.Fatalf("dry run with a neutral kubeconfig: exit %d\n%s", code, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(e.kubectlLog(t)), "\n") {
		if !strings.Contains(line, "--kubeconfig "+e.kubeconfig) {
			t.Errorf("a kubectl call does not name the kubeconfig: %s", line)
		}
		if !strings.HasPrefix(line, "--kubeconfig "+e.kubeconfig+" config") && !strings.Contains(line, "--context "+hmCtx) {
			t.Errorf("a kubectl call does not name the context: %s", line)
		}
	}
}

// HM03: a cordoned node (spec.unschedulable or the unschedulable taint) and
// a node with a NoSchedule taint are refused before any pod exists; the
// explicit flags let them through, and an allowed taint gets a toleration.
func TestHM03RefusesACordonedOrTaintedNode(t *testing.T) {
	cases := []struct {
		name, spec, want string
		allow            []string
	}{
		{"unschedulable", `"unschedulable": true`, "is cordoned", []string{"--allow-cordoned"}},
		{"cordon taint", `"taints": [{"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule"}]`, "is cordoned", []string{"--allow-cordoned"}},
		{"NoSchedule taint", `"taints": [{"key": "example.com/dedicated", "value": "batch", "effect": "NoSchedule"}]`, "has the taint example.com/dedicated=batch:NoSchedule", []string{"--allow-taint", "example.com/dedicated"}},
		{"taint without a value", `"taints": [{"key": "example.com/gpu", "effect": "NoSchedule"}]`, "has the taint example.com/gpu:NoSchedule", []string{"--allow-taint", "example.com/gpu"}},
	}
	for _, c := range cases {
		e := hmSetup(t, c.spec, hmPodsJSON())
		out, code := e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--yes")...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want 2 and %q:\n%s", c.name, code, c.want, out)
		}
		if strings.Contains(e.kubectlLog(t), "create") {
			t.Errorf("%s: a refused run created something:\n%s", c.name, e.kubectlLog(t))
		}
		out, code = e.run(t, "", append(e.base("hwp-epb", "all-cpus"), append(c.allow, "--dry-run")...)...)
		if code != 0 {
			t.Errorf("%s with %v: exit %d, want 0:\n%s", c.name, c.allow, code, out)
		}
		if c.name == "NoSchedule taint" && !strings.Contains(out, `"key":"example.com/dedicated","operator":"Exists"`) {
			t.Errorf("the allowed taint has no toleration in the pod:\n%s", out)
		}
	}
}

// HM04: a pod no DaemonSet owns refuses the node unless an --expect-pod
// pattern matches its namespace/name; DaemonSet pods and finished pods
// never do.
func TestHM04RefusesUnexpectedPods(t *testing.T) {
	pods := hmPodsJSON(hmDaemonPod("kube-system", "cni-x1"), hmPlainPod("team-a", "train-7", "Running"), hmPlainPod("team-b", "done-1", "Succeeded"))
	e := hmSetup(t, hmSchedulable, pods)
	out, code := e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--yes")...)
	if code != 2 || !strings.Contains(out, "runs pods it was not told to expect: team-a/train-7") || strings.Contains(out, "cni-x1 (") || strings.Contains(out, "done-1") {
		t.Errorf("exit %d, want 2 naming team-a/train-7 only:\n%s", code, out)
	}
	out, code = e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--expect-pod", "team-a/train-.*", "--dry-run")...)
	if code != 0 || !strings.Contains(out, "2 expected") {
		t.Errorf("with --expect-pod: exit %d, want 0 and 2 expected pods:\n%s", code, out)
	}
	out, code = e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--expect-pod", "train-7", "--dry-run")...)
	if code != 2 {
		t.Errorf("a pattern that matches the name without its namespace must not count: exit %d\n%s", code, out)
	}
	if out, code = e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--allow-other-pods", "--dry-run")...); code != 0 {
		t.Errorf("with --allow-other-pods: exit %d, want 0:\n%s", code, out)
	}
}

// HM05: the plan names the measurement, the node, the device, the image by
// digest, what changes and how it is restored, the duration and the
// results directory; a dry run prints the pod and creates nothing.
func TestHM05PlanAndDryRun(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON(hmDaemonPod("kube-system", "cni-x1")))
	out, code := e.run(t, "", append(e.base("cpu-governor-energy", "all-cpus"), "--loads", "0,50", "--dry-run")...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Measurement   cpu-governor-energy", "settles     physics.cpu.idleGovernorDeltaW",
		"Node          node-a (unschedulable: false; taints: none)", "Device        all-cpus",
		"Image         ghcr.io/colinianking/stress-ng:", "@sha256:",
		"Changes       scaling_governor of every cpufreq policy", "Restore       each policy's scaling_governor read before",
		"Options       --loads 0,50", "Duration      about 8 min", "Results       " + e.out + "/",
		"nodeName: \"node-a\"", "privileged: true", `{"name":"HW_OPT_LOADS","value":"0,50"}`,
		`args: ["cpu-governor-energy","all-cpus"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan or the pod lacks %q:\n%s", want, out)
		}
	}
	if regexp.MustCompile(`@[A-Z_]+@`).MatchString(out) {
		t.Errorf("a placeholder is left in the pod:\n%s", out)
	}
	if log := e.kubectlLog(t); strings.Contains(log, "create") || strings.Contains(log, "exec") {
		t.Errorf("the dry run created something:\n%s", log)
	}
	if dirs, _ := filepath.Glob(filepath.Join(e.out, "*Z-*")); len(dirs) != 0 {
		t.Errorf("the dry run wrote a run directory: %v", dirs)
	}
	if strings.Contains(out, "HW_TRACEFS") {
		t.Errorf("the host's own root must keep the kernel's tracefs:\n%s", out)
	}
	// Another host root (an emulated node's tree) turns the kernel trace off.
	out, _ = e.run(t, "", append(e.base("schedutil-steps", "cpu1"), "--host-root", "/var/lib/joulie-hwemu/n-0", "--dry-run")...)
	if !strings.Contains(out, `{"name":"HW_TRACEFS","value":"none"}`) || !strings.Contains(out, `path: "/var/lib/joulie-hwemu/n-0/sys"`) {
		t.Errorf("--host-root does not mount the tree's sys or does not turn the trace off:\n%s", out)
	}
	// A GPU measurement requests every allocatable GPU of its vendor.
	out, _ = e.run(t, "", append(e.base("amdgpu-cap-range", "all"), "--dry-run")...)
	if !strings.Contains(out, `resources: {"limits":{"amd.com/gpu":"1"}}`) {
		t.Errorf("amdgpu-cap-range does not request the node's amd.com/gpu:\n%s", out)
	}
}

// HM06: without --yes the run starts only when the node name is typed back.
func TestHM06ConfirmationIsTheNodeName(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, code := e.run(t, "yes\n", e.base("hwp-epb", "all-cpus")...)
	if code != 2 || !strings.Contains(out, "not confirmed") || strings.Contains(e.kubectlLog(t), "create") {
		t.Fatalf("answer yes: exit %d, want 2 and nothing created:\n%s\n%s", code, out, e.kubectlLog(t))
	}
	out, code = e.run(t, hmNode+"\n", e.base("hwp-epb", "all-cpus")...)
	if code != 0 {
		t.Fatalf("answer %s: exit %d, want 0:\n%s", hmNode, code, out)
	}
	if !strings.Contains(e.kubectlLog(t), "create -f") {
		t.Errorf("the confirmed run created no pod:\n%s", e.kubectlLog(t))
	}
}

// HM07: a measurement that fails after its first change restores it in
// the probe's trap: the governor is back, the restore and the second
// verification say so, the pod and its ConfigMap are deleted, the journal
// line is in the pod log, and the exit status is 1.
func TestHM07RestoreOnFailure(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	e.extraEnv = []string{"FAKE_STRESS_FAIL=1"}
	out, code := e.run(t, "", append(e.base("conservative-steps", "cpu1"), "--yes", "--measure-s", "1")...)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if g := e.gov(t, "1"); g != "schedutil" {
		t.Errorf("policy1 governor %q after the failure, want schedutil restored", g)
	}
	rd := e.runDir(t)
	for file, want := range map[string]string{
		"raw/STATUS":       "rc=1 restore=ok verify=ok",
		"raw/restore.txt":  "ok\t1\tsysfs\t" + e.root + "/sys/devices/system/cpu/cpufreq/policy1/scaling_governor\tschedutil",
		"verify-after.txt": "ok\t" + e.root + "/sys/devices/system/cpu/cpufreq/policy1/scaling_governor\tschedutil",
		"probe.log":        "journal 1: sysfs " + e.root + "/sys/devices/system/cpu/cpufreq/policy1/scaling_governor (restore to: schedutil)",
	} {
		if got := hmRead(t, filepath.Join(rd, file)); !strings.Contains(got, want) {
			t.Errorf("%s lacks %q:\n%s", file, want, got)
		}
	}
	log := e.kubectlLog(t)
	for _, want := range []string{"delete pod", "delete configmap", "exec -n default joulie-hw-conservative-steps-"} {
		if !strings.Contains(log, want) {
			t.Errorf("kubectl was not asked to %q:\n%s", want, log)
		}
	}
}

// HM08: Ctrl-C during the measurement stops the probe through its TERM
// trap, which restores the governor; the runner copies the results,
// deletes the pod and exits 130.
func TestHM08RestoreOnInterrupt(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	c := e.cmd("", append(e.base("conservative-steps", "cpu1"), "--yes", "--measure-s", "30")...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for e.gov(t, "1") != "conservative" {
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatalf("the probe never set conservative:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := c.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := c.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("exit %v, want 130:\n%s", err, buf.String())
	}
	if g := e.gov(t, "1"); g != "schedutil" {
		t.Errorf("policy1 governor %q after Ctrl-C, want schedutil restored", g)
	}
	rd := e.runDir(t)
	if got := hmRead(t, filepath.Join(rd, "raw/STATUS")); !strings.Contains(got, "rc=143 restore=ok verify=ok") {
		t.Errorf("STATUS %q, want the TERM exit with the restore verified", got)
	}
	log := e.kubectlLog(t)
	if !strings.Contains(log, "probe.sh stop") || !strings.Contains(log, "delete pod") {
		t.Errorf("the interrupt did not stop the probe and delete the pod:\n%s", log)
	}
}

// HM09: a restore the kernel refuses (here the governor file became a
// directory, as a sysfs attribute that rejects writes) is reported, never
// hidden: the run exits 3 and names the files to check.
func TestHM09FailedRestoreIsReported(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	c := e.cmd("", append(e.base("conservative-steps", "cpu1"), "--yes", "--measure-s", "4")...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	gov := filepath.Join(e.root, "sys/devices/system/cpu/cpufreq/policy1/scaling_governor")
	deadline := time.Now().Add(30 * time.Second)
	for e.gov(t, "1") != "conservative" {
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatalf("the probe never set conservative:\n%s", buf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := os.Remove(gov); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(gov, 0o755); err != nil {
		t.Fatal(err)
	}
	err := c.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit %v, want 3:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "RESTORE NOT VERIFIED") {
		t.Errorf("the failed restore is not reported:\n%s", buf.String())
	}
	if got := hmRead(t, filepath.Join(e.runDir(t), "raw/restore.txt")); !strings.Contains(got, "FAILED\t1\tsysfs") {
		t.Errorf("restore.txt does not name the failed entry:\n%s", got)
	}
}

// HM10: the read-only measurements write nothing to the host, and their
// summaries carry the facts they read: the hwp flags and EPB, and the
// amdgpu cap range with the profile line for minW.
func TestHM10ReadOnlyMeasurementsChangeNothing(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	before := hmTreeDigest(t, e.root)
	out, code := e.run(t, "", append(e.base("hwp-epb", "all-cpus"), "--yes")...)
	if code != 0 {
		t.Fatalf("hwp-epb: exit %d\n%s", code, out)
	}
	sum := hmRead(t, filepath.Join(e.runDir(t), "summary.md"))
	for _, want := range []string{"| CPUs in cpuinfo, with a flags line | 2, 2 |", "| CPUs with the hwp flag | 2 of 2 |", "| energy_perf_bias (value x CPUs read) | 6 x2 |", "cpufreq.hwp: {v: true"} {
		if !strings.Contains(sum, want) {
			t.Errorf("hwp-epb summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, hmNode) {
		t.Errorf("the summary names the node:\n%s", sum)
	}
	if err := os.RemoveAll(e.out); err != nil {
		t.Fatal(err)
	}
	out, code = e.run(t, "", append(e.base("amdgpu-cap-range", "all"), "--yes")...)
	if code != 0 {
		t.Fatalf("amdgpu-cap-range: exit %d\n%s", code, out)
	}
	sum = hmRead(t, filepath.Join(e.runDir(t), "summary.md"))
	for _, want := range []string{"| gpu0 | 0x74a1 | 0.0 | 750.0 | 750.0 | 750.0 |", "gpus.limits.minW: {v: 0, src: \"measured:<id>#power1_cap_min"} {
		if !strings.Contains(sum, want) {
			t.Errorf("amdgpu-cap-range summary lacks %q:\n%s", want, sum)
		}
	}
	if after := hmTreeDigest(t, e.root); after != before {
		t.Errorf("a read-only measurement changed the host tree")
	}
}

// hmTreeDigest is every path, mode and content of the host tree.
func hmTreeDigest(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v\n", strings.TrimPrefix(p, root), fi.Mode())
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// hmProbe runs probe.sh directly with the host of e and returns its
// output directory and the started command.
func hmProbe(t *testing.T, e *hmEnv, env []string, args ...string) (string, *exec.Cmd, *bytes.Buffer) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	c := exec.Command("bash", append([]string{"probe.sh", "run"}, args...)...)
	c.Env = append([]string{
		"PATH=" + e.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HW_OUT=" + out, "HW_SYS=" + filepath.Join(e.root, "sys"), "HW_HOST=" + filepath.Join(e.root, "host"),
		"HW_TRACEFS=none", "HW_LINGER_S=0", "HW_NVSMI=nvidia-smi", "FAKE_NV=" + filepath.Join(e.state, "nv"),
	}, env...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) })
	return out, c, &buf
}

// HM11: an NVIDIA clock lock is journaled before it is set, and TERM in the
// middle of the locked period resets it (-rgc) before the probe exits; a
// lock has no getter, so the reset is the restore.
func TestHM11NvidiaLockIsResetOnTERM(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_OPT_SAMPLE_S=1"}, "nvidia-lock-floor-idle", hmBus)
	lock := filepath.Join(e.state, "nv/lock.00000000:3B:00.0")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(lock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe never locked the clocks:\n%s", buf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := strings.TrimSpace(hmRead(t, lock)); got != "1500,1980" {
		t.Errorf("lock %q, want the supported clock nearest 75 %% of max and the max, 1500,1980", got)
	}
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	if _, err := os.Stat(lock); err == nil {
		t.Errorf("the lock is still set after TERM:\n%s", buf.String())
	}
	if got := hmRead(t, filepath.Join(out, "STATUS")); !strings.Contains(got, "rc=143 restore=ok verify=ok") {
		t.Errorf("STATUS %q:\n%s", got, buf.String())
	}
	calls := hmRead(t, filepath.Join(e.state, "nv/calls"))
	if !regexp.MustCompile(`(?s)-lgc 1500,1980.*-i 00000000:3B:00.0 -rgc`).MatchString(calls) {
		t.Errorf("no -rgc after the lock:\n%s", calls)
	}
}

// HM12: a full nvidia-lock-floor-idle run resets the lock it set as a step
// of the measurement, and its summary compares the arms.
func TestHM12NvidiaLockFloorIdleRun(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_OPT_SAMPLE_S=2", "HW_OPT_SETTLE_S=0"}, "nvidia-lock-floor-idle", hmBus)
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	for _, want := range []string{"| before | 70.250 |", "| locked | 90.500 |", "| after | 70.250 |", "lockFloorAtIdle", "the reset took effect"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if _, err := os.Stat(filepath.Join(e.state, "nv/lock.00000000:3B:00.0")); err == nil {
		t.Errorf("the lock is left set")
	}
}

// HM13: cpu-governor-energy restores every policy's governor after its
// cells and reads only the RAPL package zone, not its DRAM subzone.
func TestHM13CPUGovernorEnergyRestoresEveryPolicy(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_OPT_GOVERNORS=performance,conservative", "HW_OPT_LOADS=0,30", "HW_OPT_SETTLE_S=0", "HW_OPT_MEASURE_S=1"}, "cpu-governor-energy", "all-cpus")
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	for _, p := range []string{"0", "1"} {
		if g := e.gov(t, p); g != "schedutil" {
			t.Errorf("policy%s governor %q, want schedutil restored", p, g)
		}
	}
	csv := hmRead(t, filepath.Join(out, "energy.csv"))
	if strings.Contains(csv, "dram") || strings.Count(csv, "package-0") != 4 {
		t.Errorf("energy.csv must hold the package zone once per cell (4 cells) and no DRAM zone:\n%s", csv)
	}
	if got := hmRead(t, filepath.Join(out, "STATUS")); got != "rc=0 restore=ok verify=ok\n" {
		t.Errorf("STATUS %q", got)
	}
}

// HM14: every image the measurements pull is named by tag and digest.
func TestHM14ImagesArePinnedByDigest(t *testing.T) {
	env := hmRead(t, "images.env")
	re := regexp.MustCompile(`(?m)^HW_IMAGE_[A-Z]+=(.*)$`)
	m := re.FindAllStringSubmatch(env, -1)
	if len(m) != 3 {
		t.Fatalf("want 3 images in images.env, have %d", len(m))
	}
	for _, x := range m {
		if !regexp.MustCompile(`^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`).MatchString(x[1]) {
			t.Errorf("image %q is not named by tag and digest", x[1])
		}
	}
}

// HM15: the README has a section for every measurement the runner lists,
// with the command that runs it.
func TestHM15ReadmeCoversEveryMeasurement(t *testing.T) {
	hmNeed(t)
	out, err := exec.Command("bash", "hwmeasure.sh", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("hwmeasure.sh list: %v\n%s", err, out)
	}
	readme := hmRead(t, "README.md")
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")[1:]
	if len(lines) < 15 {
		t.Fatalf("hwmeasure.sh list printed %d measurements", len(lines))
	}
	for _, l := range lines {
		m := strings.Fields(l)[0]
		if !strings.Contains(readme, "### `"+m+"`") {
			t.Errorf("README.md has no section ### `%s`", m)
		}
		if !strings.Contains(readme, "hwmeasure.sh "+m+" ") {
			t.Errorf("README.md has no command line for %s", m)
		}
	}
}

// HM16: the scripts parse under the bash that runs the tests.
func TestHM16ScriptsParse(t *testing.T) {
	hmNeed(t)
	for _, s := range []string{"hwmeasure.sh", "probe.sh"} {
		if out, err := exec.Command("bash", "-n", s).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", s, err, out)
		}
	}
}

// HM17: w7900-level-power without a load forces each level of --levels on
// every device and restores auto after each; its summary turns the idle
// deltas into dataPathHoldW (high) and gatingOffIdleW (profile_peak minus
// high).
func TestHM17W7900LevelsAreRestored(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_OPT_LOAD=none", "HW_OPT_LEVELS=high,profile_peak", "HW_OPT_SAMPLE_S=1", "HW_OPT_SETTLE_S=0"}, "w7900-level-power", "all")
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	if got := strings.TrimSpace(hmRead(t, filepath.Join(e.root, "sys/bus/pci/devices", hmBDF, "power_dpm_force_performance_level"))); got != "auto" {
		t.Errorf("level %q after the run, want auto", got)
	}
	journal := hmRead(t, filepath.Join(out, "journal"))
	if strings.Count(journal, "power_dpm_force_performance_level\tauto") != 2 {
		t.Errorf("want the level journaled once per forced level, each time with auto:\n%s", journal)
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	for _, want := range []string{"| gpu0 | high | 120.000 | 120.000 | 0.00 |", "| gpu0 | profile_peak |", "dataPathHoldW: {v: 0.00", "gatingOffIdleW: {v: 0.00"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if got := hmRead(t, filepath.Join(out, "STATUS")); got != "rc=0 restore=ok verify=ok\n" {
		t.Errorf("STATUS %q", got)
	}
}

// HM18: nvidia-lock-floor-idle with --clock memory locks the memory clock
// at its maximum, as sustained does on a GDDR part, resets it with -rmc, and
// reports the idle cost as dataPathHoldW.
func TestHM18NvidiaMemoryLockIdleCost(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_OPT_CLOCK=memory", "HW_OPT_SAMPLE_S=2", "HW_OPT_SETTLE_S=0"}, "nvidia-lock-floor-idle", hmBus)
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	calls := hmRead(t, filepath.Join(e.state, "nv/calls"))
	if !regexp.MustCompile(`(?s)-lmc 9001,9001.*-i 00000000:3B:00.0 -rmc`).MatchString(calls) {
		t.Errorf("want -lmc 9001,9001 then -rmc:\n%s", calls)
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	for _, want := range []string{"| locked | 90.500 |", "dataPathHoldW: {v: 20.25", "the reset took effect"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
}

// HM19: a knob changed twice, once per arm, is journaled again after each
// undo, so every lock is reset: before the fix the second lock of a
// receiver test was never journaled and stayed set after the run, where no
// readback could see it. The summary gives the idle receiver against the
// best arm.
func TestHM19EveryArmLockIsReset(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"HW_PEERCOPY=" + filepath.Join(e.bin, "peercopy"), "HW_OPT_COLD_S=0"}, "hopper-receiver-gate", hmBus, "0000:5e:00.0")
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	calls := hmRead(t, filepath.Join(e.state, "nv/calls"))
	if !regexp.MustCompile(`(?s)-i 00000000:3B:00.0 -lgc 1980,1980.*-i 00000000:3B:00.0 -rgc.*-i 00000000:3B:00.0 -lgc 345,345.*-i 00000000:3B:00.0 -rgc`).MatchString(calls) {
		t.Errorf("want each lock arm followed by its own -rgc:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(e.state, "nv/lock.00000000:3B:00.0")); err == nil {
		t.Errorf("a lock is left set on the receiver")
	}
	if j := hmRead(t, filepath.Join(out, "journal")); strings.Count(j, "nvrgc\t00000000:3B:00.0") != 2 {
		t.Errorf("want the receiver's lock journaled once per lock arm:\n%s", j)
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	for _, want := range []string{"| default | 12 | 15.500 |", "| warm | 12 | 28.000 |", "Idle receiver over the best arm: 0.55"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
}

// HM20: a pod that never runs (an image it cannot pull) ends the run with
// exit 1 after --start-timeout; nothing was changed, and the pod and its
// ConfigMap are deleted.
func TestHM20PodThatNeverRunsIsCleanedUp(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	e.extraEnv = []string{"FAKE_POD_START=fail"}
	before := hmTreeDigest(t, e.root)
	out, code := e.run(t, "", append(e.base("conservative-steps", "cpu1"), "--yes", "--start-timeout", "6")...)
	if code != 1 || !strings.Contains(out, "pod: Pending  ErrImagePull") || !strings.Contains(out, "did not run within 6 s") {
		t.Fatalf("exit %d, want 1 with the pod's waiting reason:\n%s", code, out)
	}
	log := e.kubectlLog(t)
	if !strings.Contains(log, "delete pod") || !strings.Contains(log, "delete configmap") {
		t.Errorf("the pod or the ConfigMap was not deleted:\n%s", log)
	}
	if hmTreeDigest(t, e.root) != before {
		t.Errorf("the host tree changed although the probe never ran")
	}
}

// HM21: on a driver that knows only the clocks_throttle_reasons names
// (R525 and older), the samplers keep the reasons column under that name
// instead of dropping it.
func TestHM21ThrottleReasonsOnOldDrivers(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	out, c, buf := hmProbe(t, e, []string{"FAKE_NV_OLD=1", "HW_OPT_SAMPLE_S=1", "HW_OPT_SETTLE_S=0"}, "nvidia-lock-floor-idle", hmBus)
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	head := strings.SplitN(hmRead(t, filepath.Join(out, "idle-before.csv")), "\n", 2)[0]
	if !strings.Contains(head, "clocks_throttle_reasons.active") {
		t.Errorf("the sampler header %q has no reasons column", head)
	}
}

// HM22: a cpuinfo without a flags line (an emulated node's, or a stripped
// one) says nothing about HWP: hwp-epb reports it unknown and proposes no
// profile change, where it used to read "0 of 0" and propose hwp false.
func TestHM22NoFlagsLineMeansUnknown(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	hmWrite(t, e.root, map[string]string{"host/proc/cpuinfo": "processor\t: 0\nmodel name\t: Neutral CPU\n\nprocessor\t: 1\nmodel name\t: Neutral CPU\n"})
	out, c, buf := hmProbe(t, e, nil, "hwp-epb", "all-cpus")
	if err := c.Wait(); err != nil {
		t.Fatalf("probe: %v\n%s", err, buf.String())
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	if !strings.Contains(sum, "| CPUs in cpuinfo, with a flags line | 2, 0 |") || !strings.Contains(sum, "HWP is unknown") || strings.Contains(sum, "cpufreq.hwp:") {
		t.Errorf("want HWP unknown and no profile line:\n%s", sum)
	}
}

// HM23: a TERM while the runner still waits for the pod to report Running,
// although the probe already runs and changed the governor, stops the probe
// and collects its results: before the fix the runner only deleted the pod,
// so the restore report was lost and the run ended with restore unknown.
func TestHM23InterruptBeforeThePodReportsRunning(t *testing.T) {
	e := hmSetup(t, hmSchedulable, hmPodsJSON())
	e.extraEnv = []string{"FAKE_POD_PENDING_S=120"}
	c := e.cmd("", append(e.base("conservative-steps", "cpu1"), "--yes", "--measure-s", "30")...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for e.gov(t, "1") != "conservative" {
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatalf("the probe never set conservative:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := c.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("exit %v, want 130:\n%s", err, buf.String())
	}
	if g := e.gov(t, "1"); g != "schedutil" {
		t.Errorf("policy1 governor %q, want schedutil restored", g)
	}
	if got := hmRead(t, filepath.Join(e.runDir(t), "raw/STATUS")); !strings.Contains(got, "restore=ok verify=ok") {
		t.Errorf("STATUS %q, want the restore collected", got)
	}
	if !strings.Contains(e.kubectlLog(t), "probe.sh stop") {
		t.Errorf("the probe was not stopped through its trap:\n%s", e.kubectlLog(t))
	}
}

// HM24: hwp-epb on the tree hwemu renders for an HWP Xeon, its sysfs and
// its generated cpuinfo, finds the hwp flag on every CPU and proposes
// hwp true; before hwemu rendered flags from the facts it read no flags
// line there and could only say unknown.
func TestHM24HWPEPBOnAnEmulatedHWPXeon(t *testing.T) {
	hmNeed(t)
	_, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", nil)
	n := p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * p.CPU.ThreadsPerCore.V
	out := filepath.Join(t.TempDir(), "out")
	c := exec.Command("bash", "probe.sh", "run", "hwp-epb", "all-cpus")
	c.Env = append(os.Environ(), "HW_OUT="+out, "HW_SYS="+tree.SysRoot, "HW_HOST="+filepath.Dir(tree.CPUInfoPath), "HW_LINGER_S=0")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v\n%s", err, b)
	}
	sum := hmRead(t, filepath.Join(out, "summary.md"))
	for _, want := range []string{fmt.Sprintf("| CPUs with the hwp flag | %d of %d |", n, n), fmt.Sprintf("| CPUs with the hwp_epp flag | %d of %d |", n, n), "cpufreq.hwp: {v: true"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
}
