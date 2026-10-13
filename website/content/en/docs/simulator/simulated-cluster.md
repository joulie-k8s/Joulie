---
title: "Simulated Cluster"
weight: 12
---

The simulated cluster runs the real Joulie components on emulated bare-metal nodes, on one machine with Docker. The agent discovers and caps emulated hardware through the same files and tools it uses on a real node, the controller manager reads node power from the simulator's OpenTelemetry metrics in Prometheus, and the scheduler extender scores real pods. The scripts in `scripts/simcluster` drive it, `tests/simcluster` checks it end to end, and CI runs it on every pull request that touches what it builds.

Use it to see a change to the agent, the controller manager, the extender, the chart or the simulator work across components. A change to discovery or capping alone is checked faster by the hwemu tests in `cmd/agent` (`go test ./cmd/agent/ -run Hwemu`).

## Architecture

```text
host: Docker
  container $SIMCLUSTER_NAME (--privileged): one k3s server
    Node <name>-infra: the only real kubelet (joulie.io/infra=true, joulie.io/hwemu-host=true)
      monitoring     Prometheus with its OTLP receiver; the Collector in mode otel-collector
      joulie-sim     the simulator in emulate mode, writing /var/lib/joulie-hwemu/<node>/
      joulie-system  the controller manager; the scheduler extender on the host network
      joulie-system  joulie-agent-emu-<i>: one agent per eligible fake Node, mounting its tree
    KWOK            keeps the fake Nodes Ready
    fake Nodes      rendered from the cluster spec
```

- **One k3s server in Docker.** The container `$SIMCLUSTER_NAME` (default `joulie-sim`) runs the k3s image pinned in `scripts/simcluster/versions.env`. Its own kubelet is the only real Node, `<name>-infra`, and runs every pod that really runs. The k3s state and the hwemu trees live in the named volumes `<name>-k3s` and `<name>-hwemu`, so they outlive a restart of the container: the trees, the limits agents wrote and the energy ledgers that keep exported counters from going back. The trees need no tmpfs: 200 emulated nodes take about 0.2 s per tick on the volume ([Tick cost]({{< relref "/docs/simulator/simulator.md#tick-cost" >}})).
- **Fake Nodes.** `clusterspec render-nodes` turns the [cluster spec]({{< relref "/docs/simulator/simulator.md#the-cluster-spec" >}}) into Node objects with the labels, taints, capacity and architecture of their hardware profiles, and KWOK v0.8.0 keeps them Ready. Every fake Node carries `sim.joulie.io/cluster=<spec>` and `sim.joulie.io/pool=<pool>`.
- **Emulated hardware.** The simulator runs in [emulate mode]({{< relref "/docs/simulator/simulator.md#emulate-mode" >}}) with the same spec: one tree per fake Node under `/var/lib/joulie-hwemu` on the infra Node, all stepped on one tick.
- **Emulated agents.** The agent binary is the one a release ships, unmodified. With `agent.mode: emulated` the chart renders one Deployment `joulie-agent-emu-<i>` per entry of `agent.emulated.nodes`, on the infra Node, mounting its Node's tree where the agent reads `/sys`, `/proc/cpuinfo` and the GPU tools ([Agent chart values]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-chart-values" >}})). Which Nodes get one is decided as in a real install: `simcluster agents` renders the chart's agent DaemonSet with the same values files as the install and keeps the Nodes it would reach. The node selector must match, every `NoSchedule` and `NoExecute` taint must be tolerated (counting the tolerations the DaemonSet controller adds), and the architecture must match the image. Every other Node is printed with its reason.
- **Scheduler.** The k3s scheduler calls the extender through `scripts/simcluster/manifests/scheduler-config.yaml`, the same file the Dagger integration job uses. The extender runs on the infra Node's host network.
- **Telemetry.** The simulator pushes OTLP straight into Prometheus, and the controller manager reads node power and the facility from there with its default settings, or with the settings of the chosen [telemetry mode](#telemetry-modes).
- **Access.** Every cluster HTTP call goes through the API server's service proxy. The only published port is the API server's, on `127.0.0.1`, at a port Docker picks. The kubeconfig is `$SIMCLUSTER_DIR/kubeconfig`; `~/.kube` is never read or written.
- **Images.** One `go build` (linux/amd64, CGO off) builds the agent, controller manager, extender, simulator and `fakesmi` from the working tree. Each binary is wrapped in the distroless image its release uses, tagged `joulie.local/joulie-<component>:dev-<first 12 hex of the binary's sha256>` (for the simulator image, which also carries `fakesmi`, a hash of both binaries) and imported into the cluster's containerd. Pods pull with `IfNotPresent`, and the deploy checks each pod's image ID against `images.env`.

## Requirements

- **Host.** amd64 Linux with Docker allowed to run a `--privileged` container: k3s runs its kubelet and containerd inside it. The images are built for linux/amd64 only.
- **Tools.** `docker`, `kubectl`, `helm`, `jq`, `sha256sum` and Go. The scripts build with the toolchain `go.mod` names (they set `GOTOOLCHAIN`), so the images match what CI builds. CI installs the Helm release that `HELM_VERSION` in `versions.env` pins.
- **k3s 1.34 on a cgroup v1 host.** `stat -fc %T /sys/fs/cgroup` prints `tmpfs` on cgroup v1. k3s 1.35 and later refuse cgroup v1 unless the kubelet runs with `fail-cgroupv1=false`; `up.sh` adds that flag on such a host for such a k3s, but only the pinned 1.34 is tested there.
- **`fs.inotify.max_user_instances`.** Every pod of the cluster runs on the one infra kubelet, so all their containers and file watches share the host's per-user inotify limit with the host's own programs. 128 is a common default. The smoke spec fits in it, but a scale run with many emulated agents may exhaust it. The scripts never change host settings: check it with `sysctl fs.inotify.max_user_instances` and raise it by hand before a large run.
- **Pod capacity.** Every emulated agent is a pod on the infra Node. `up.sh` sizes its kubelet for the agents plus 50, at least 110 pods: up to 250 pods with the default /24 per Node, up to 1000 with a /22 per Node and the cluster CIDR `10.64.0.0/12`. Beyond 1000 it fails with "split across k3s agents". A running container created for fewer pods is never resized: `up.sh` asks for a recreate.
- **Network on the first run.** The k3s system images, KWOK, Prometheus, the Collector and the distroless base are pulled once; third-party images are pulled by the digest `versions.env` pins. With `SIMCLUSTER_AIRGAP=1` the k3s system images come from the release's air-gap bundle, kept in `$SIMCLUSTER_DIR/images` for later runs.

## Quick start

```bash
make simcluster-smoke SIMCLUSTER_KEEP=1
D=~/.cache/joulie/simcluster/joulie-sim-smoke
jq '{status: ."test.suite.run.status", failures, knownDefects}' $D/runs/latest/summary.json
export KUBECONFIG=$D/kubeconfig
kubectl get nodes -L sim.joulie.io/pool
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,SRC:.status.powerMeasurement.source,W:.status.powerMeasurement.measuredNodePowerW
make simcluster-down SIMCLUSTER_NAME=joulie-sim-smoke
```

`make simcluster-smoke` creates the cluster `joulie-sim-smoke` from `tests/simcluster/specs/smoke.yaml`, deploys with `tests/simcluster/smoke-values.yaml`, runs `TestSimclusterE2E`, collects the results and deletes the cluster unless `SIMCLUSTER_KEEP=1`. The smoke spec has eight Nodes: an Intel Xeon with RAPL, an AMD EPYC with energy counters only, an AMD EPYC with HSMP, an H100 NVL node, an H100 SXM node with persistence mode off, an MI300X node, a virtual machine, and a managed Intel node whose taint the agent does not tolerate. The smoke values put every node in performance with CPU and GPU caps at 80 %; the test lowers them to 50 % halfway.

To keep a cluster and work on it step by step:

```bash
make simcluster-up       # k3s, KWOK, Prometheus, the fake Nodes
make simcluster-deploy   # images, simulator, Joulie; waits until every component reports
make simcluster-status
```

## Targets

Each target runs one script of `scripts/simcluster` and nothing else, so `make -n` shows what will run. The scripts also run directly, and each starts with its usage.

| Target | Script | What it does |
|---|---|---|
| `simcluster-up` | `up.sh` | Creates the k3s container, or reuses an existing one, starting it when stopped, if it was created for at least as many pods and as large pod CIDRs, with the same scheduler configuration and k3s image; otherwise it says to recreate. Installs KWOK, Prometheus and, in mode `otel-collector`, the Collector. Applies the spec's Nodes and deletes the fake Nodes the spec no longer names. |
| `simcluster-images` | `images.sh` | Builds and imports the Joulie images, and the pinned third-party ones, and writes `images.env`. An unchanged binary keeps its tag and is not imported again. |
| `simcluster-deploy` | `deploy.sh` | Images, CRDs, the simulator, the agent placement, Joulie. Waits for the rollouts, one NodeHardware per emulated agent, the series of every measured Node and a NodeTwin per managed Node, then checks every pod's image. `-f <values>` adds a Joulie values file when run directly. |
| `simcluster-reload C=<component>` | `reload.sh` | Rebuilds and rolls out `agent`, `controller-manager`, `scheduler`, `simulator` or `all`; see [the dev loop](#the-dev-loop). |
| `simcluster-status` | `status.sh` | Prints every Node with its pool, profile and NodeTwin power. Exits 1 when a Node is NotReady, a pod without `joulie.io/workload-class` is bound to a fake Node, a pod runs another image than `images.env` names, an emulated agent's Node has no NodeHardware, or a measured Node has no series. |
| `simcluster-tree [NODE=<node>]` | `tree.sh` | Without a Node, lists the Nodes that have a tree; with one, prints every file of its tree with its first line: the energy counters, limits, frequencies and GPU state its agent sees. |
| `simcluster-logs C=<component> [NODE=<node>]` | `logs.sh` | The last 200 lines of `agent` (one Node's, or every agent's), `controller-manager`, `scheduler`, `simulator`, `prometheus`, `otelcol`, `kwok` or `k3s`. Run directly, it passes further flags such as `-f` to `kubectl logs`. |
| `simcluster-smoke` | `smoke.sh` | See [Quick start](#quick-start). |
| `simcluster-down` | `down.sh` | Removes the container with its anonymous volumes and the named volumes `<name>-k3s` and `<name>-hwemu`. Keeps `images/`, `runs/` and `bin/` of the state directory. |

## Settings

The scripts read these variables from the environment; `make` passes on the ones that are set.

| Variable | Default | Effect |
|---|---|---|
| `SIMCLUSTER_NAME` | `joulie-sim`; `joulie-sim-smoke` for the smoke run | Container name and prefix of its volumes. It must match `^joulie-sim(-[a-z0-9]+)*$`: the scripts touch no container or volume named otherwise. |
| `SIMCLUSTER_DIR` | `${XDG_CACHE_HOME:-~/.cache}/joulie/simcluster/<name>` | State directory, outside the repository (below). |
| `SIMCLUSTER_SPEC` | `tests/simcluster/specs/smoke.yaml` | The cluster spec. `tests/simcluster/specs/medium.yaml` has 50 Nodes, 46 of them reached by the agent, with GPU sharing, AMD GPUs with a pinned power cap, Nodes without NFD labels or GPU driver tools, and an unmanaged pool. |
| `SIMCLUSTER_NODES` | (empty) | Instead of a spec, this many Nodes of one profile, for scale runs. |
| `SIMCLUSTER_SCALE_PROFILE` | `intel-xeon-2s-rapl` | The profile of `SIMCLUSTER_NODES`. |
| `SIMCLUSTER_TELEMETRY` | `otel` | The [telemetry mode](#telemetry-modes). |
| `SIMCLUSTER_POWER_SCOPE` | `components` | The node power scope Joulie reads: `components` (CPU packages and GPUs) or `host` (`hw.host.*`, otel modes only). |
| `SIMCLUSTER_TIMING` | `dev` | The [timing profile](#timing), a file `scripts/simcluster/timing/<name>.env`. |
| `SIMCLUSTER_PROFILES_DIR` | (empty) | Local hardware profiles, read by the host tools only. The simulator in the cluster reads its builtin profiles, so `up.sh` refuses a spec that needs a local one. |
| `SIMCLUSTER_EXTRA_VALUES` | (empty) | Joulie values files, space separated, installed after the generated ones. They also decide which Nodes get an emulated agent. |
| `SIMCLUSTER_KEEP` | `0` | `1` keeps the smoke cluster after the run. |
| `SIMCLUSTER_AIRGAP` | `0` | `1` loads the k3s system images from the release's air-gap bundle. |
| `SIMCLUSTER_FORCE_RELOAD` | `0` | `1` makes every reload a `reload.sh --force`. |
| `COMPONENT` (or `C`), `NODE` | (empty) | The component and the Node of `simcluster-reload`, `simcluster-logs` and `simcluster-tree`. |

The state directory holds `kubeconfig`; `cluster.env` (how the container was created); `telemetry.env`, `spec.yaml`, `images.env`, `deploy.env` and `crds.sha` (what is installed); `values/` and `render/` (generated values files, renders and configurations); `bin/` (the binaries, with the host tools in `bin/host`); `images/` (image tarballs); and `runs/` (the [run results](#run-results)).

## Telemetry modes

A mode sets how the emulated Nodes' power reaches Prometheus and which Joulie input settings read it. Every mode is checked against the emulator's truth, read from the simulator's `/debug/emulate`.

| `SIMCLUSTER_TELEMETRY` | Path to Prometheus | Joulie settings beyond the defaults | Checked against |
|---|---|---|---|
| `otel` (default) | the simulator pushes OTLP; Prometheus promotes `k8s.node.name` and `host.name` | none | CPU packages plus GPUs |
| `otel-unpromoted` | as `otel`, without promotion | none: `auto` falls back to joining `target_info` | CPU packages plus GPUs |
| `otel-labels-unpromoted` | as `otel-unpromoted` | `PROMETHEUS_RESOURCE_ATTRIBUTES=labels` | no data, with the promotion hint in the log |
| `otel-utf8` | as `otel`, names translated with `NoUTF8EscapingWithSuffixes` | `PROMETHEUS_NAME_TRANSLATION=NoUTF8EscapingWithSuffixes` | CPU packages plus GPUs |
| `otel-collector` | the simulator pushes to an OpenTelemetry Collector, whose `prometheus` exporter Prometheus scrapes with `honor_labels: true` | none | CPU packages plus GPUs |
| `kepler` | the simulator also serves Kepler 0.12 series | `NODE_POWER_PRESET=kepler` | CPU packages plus NVIDIA GPUs |
| `kepler-legacy` | the simulator also serves Kepler 0.7 series, scraped with `honor_labels: false` | `NODE_POWER_PRESET=kepler-legacy` | CPU packages plus NVIDIA GPUs |

With `SIMCLUSTER_POWER_SCOPE=host`, an otel mode also sets `NODE_POWER_SCOPE=host` and is checked against whole-host power. Each check passes within 3 % of the truth, and never needs to be closer than 1 W. `simcluster telemetry` prints what a mode generates, for example `go run ./simulator/cmd/simcluster telemetry --part prom-config --mode otel-utf8`; the parts are `prom-config`, `otelcol-config`, `sim-values`, `joulie-values`, `checks` and `wait`.

### Timing

Timing names map onto the real settings, so a nightly run in `production` timing exercises the values a real install uses.

| Timing setting | `dev` | `production` | Becomes |
|---|---|---|---|
| `CONTROLLER_RECONCILE_INTERVAL` | `10s` | `1m` | controller manager `RECONCILE_INTERVAL` |
| `AGENT_RECONCILE_INTERVAL` | `5s` | `20s` | agent `RECONCILE_INTERVAL` |
| `AGENT_SPEC_READ_TIMEOUT` | `60s` | `5m` | agent `SPEC_READ_TIMEOUT` |
| `PROM_SCRAPE_INTERVAL` | `5s` | `1m` | Prometheus `scrape_interval` |
| `OTLP_EXPORT_INTERVAL_MS` | `5000` | `60000` | simulator `otel.exportIntervalMillis` |
| `RATE_WINDOW` | `30s` | `5m` | controller manager `PROMETHEUS_RATE_WINDOW` |
| `FACILITY_POLL_INTERVAL` | `10s` | `30s` | controller manager `FACILITY_POLL_INTERVAL` |

A rate window shorter than four export intervals reads too few samples.

### What a real Prometheus needs

The harness configures its Prometheus the way a production one must be configured for Joulie's OpenTelemetry inputs:

```yaml
global: {scrape_interval: 5s}            # 1m in production timing
storage: {tsdb: {out_of_order_time_window: 30m}}
otlp:
  promote_resource_attributes: [k8s.node.name, host.name]
  translation_strategy: UnderscoreEscapingWithSuffixes
```

Prometheus runs with `--web.enable-otlp-receiver`, which serves `/api/v1/otlp/v1/metrics`. On a real cluster:

- Without the receiver flag (`enableOTLPReceiver` with the Prometheus Operator), Prometheus has no OTLP endpoint to push to.
- Prometheus promotes no resource attribute by default. Without the promotion, Joulie's `auto` mode joins `target_info` for the node name (mode `otel-unpromoted`); a join must group `target_info` with `max by (job, instance, <id>)`, or a restarted producer's second `target_info` series fails it.
- `otlp.translation_strategy` must match Joulie's `PROMETHEUS_NAME_TRANSLATION`, or no series is selected (mode `otel-utf8`).
- OTLP samples carry the producer's timestamps, hence the out-of-order window.
- `rate()` under-reads a series younger than its window: after a producer starts, wait one window before trusting the reading.
- A scraped Kepler 0.7 keeps its own `instance` label only as `exported_instance` (`honor_labels: false`); a Collector's `prometheus` exporter is scraped with `honor_labels: true`.

The kube-prometheus-stack values (`values/prometheus-grafana.yaml`) and the full list are under [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}) and [Pitfalls]({{< relref "/docs/architecture/telemetry.md#pitfalls" >}}).

## The dev loop

```bash
make simcluster-up && make simcluster-deploy       # once
# edit the agent
make simcluster-reload C=agent                     # rebuild, import, roll out; prints the seconds
make simcluster-logs C=agent NODE=intel-0
make simcluster-tree NODE=intel-0
make simcluster-status
```

- **A reload follows the binary, not the sources.** The image tag is the binary's sha256, so `reload.sh` imports and upgrades only when the binary changed; an unchanged binary changes nothing, with no import, no upgrade and no restart. Touching a source file is therefore a no-op. To restart a component anyway, use `reload.sh <component> --force` (through `make`, `SIMCLUSTER_FORCE_RELOAD=1`), which sets the pod template annotation `sim.joulie.io/reload` to the current time.
- **A reload rolls out one component.** The others keep their images and pods even when shared code changed their binaries too; reload them, or `all`.
- **A reload re-reads the charts and values files**, reruns the release's `helm upgrade` with only the component's tag changed, and re-applies the CRDs when they changed. The values files are those of the last deploy, as `deploy.env` records them, a scenario's `simulator.set` (`SIMCLUSTER_SIM_EXTRA_VALUES`) included.
- **Another spec or mode.** `up.sh` with another `SIMCLUSTER_SPEC` applies its Nodes and deletes the ones it no longer names; `deploy.sh` sets Prometheus up again when the mode or the timing changed.
- **Reading Prometheus and the truth**, through the API server proxy:

```bash
export KUBECONFIG=~/.cache/joulie/simcluster/joulie-sim/kubeconfig
P=/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy/api/v1/query
pq() { kubectl get --raw "$P?query=$(jq -rn --arg q "$1" '$q|@uri')" | jq -c '[.data.result[] | [.metric.k8s_node_name, .value[1]]]'; }
pq 'sum by (k8s_node_name) (rate(hw_host_energy_joules_total[30s]))'
kubectl get --raw /api/v1/namespaces/joulie-sim/services/joulie-telemetry-sim:80/proxy/debug/emulate | jq '[.nodes[] | {name, hostW: .host.powerW}]'
```

## Deviations from a real cluster

- **KWOK taint.** Every fake Node carries the KWOK annotation and the taint `kwok.x-k8s.io/node=fake:NoSchedule`, which keeps infrastructure pods off it. A pod meant for a fake Node must tolerate it. Placement ignores this taint, since a real Node does not have it.
- **The `sim.joulie.io/cluster` selector.** The infra Node runs a real kubelet, so a workload pod could land there. The simulator's workload pods select `sim.joulie.io/cluster=<spec>` by default, and nothing without `joulie.io/workload-class` may be bound to a fake Node (E02, `status.sh`).
- **Agents are Deployments, unprivileged.** On a real node the agent is a DaemonSet pod with host network and privileges. An emulated agent is a Deployment on the infra Node with `privileged: false` and no host network, and its `NODE_NAME` is a literal. A privileged pod would see the infra host's real devices on every fake Node, and only one host-network agent fits on a Node.
- **`/dev/nvidiactl` is a file.** On NVIDIA Nodes the tree has an empty regular file mounted at `/dev/nvidiactl`, where a privileged agent on a real node sees the driver's character device. The agent only checks that it exists.
- **Tool stubs.** `nvidia-smi`, `rocm-smi` and `amd-smi` are links to `fakesmi`, first on the agent's `PATH`. `fakesmi` checks what the real tool needs before it runs (the ELF loader, the NVML library, a script's interpreter; see [Simulator hardware emulation]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-hardware-emulation" >}})). Under `toolDelivery: mounted`, the default, the harness mounts one stub file at the first path of each requirement, standing in for a node where the chart's hooks deliver the driver; the stub is never run. A pool listed with `toolDelivery: none` under `spec.harness.pools` gets nothing, so its tools fail as on a node without driver tools (MD03). Only a profile whose tools list requirements can fail this way: a tool without requirements always runs.
- **Locked limits fail with EISDIR.** A limit the firmware locks, or a disabled one, is a directory in place of the attribute, so a write fails with EISDIR instead of the kernel's error for that attribute. This stays until the emulation mounts such limits read-only.
- **Fake Nodes have no kubelet.** KWOK moves a pod bound to a fake Node through its phases (its stages, plus `scripts/simcluster/manifests/stage-*.yaml`); no container runs, and the Node's power is the simulator's model of its load.

## Known defects

A defect the run reproduces today, and that waits for a fix or a decision, is registered in `tests/simcluster/known_defects_test.go`. Its check asserts the defect's observation, so the run stays green while the defect lasts, and `summary.json` lists it under `knownDefects`. The day it no longer reproduces, the run fails with "remove it from tests/simcluster/known_defects_test.go": the fix has landed, and the check must assert the fixed behaviour instead.

| ID | Check | Observation | Changes with |
|---|---|---|---|
| D1 | E11 | The MI300X agent's GPU inventory fails: no `rocm-smi` release has `--showpowercap`, so the GPU status is `none`/`error` with "unrecognized arguments: --showpowercap". | the agent reading the MI300X cap without that flag |
| D15 | E12 | A managed Node the agent DaemonSet does not reach (here an untolerated taint) gets a NodeTwin with a cap, but no NodeHardware and no `controlStatus`, and its hardware keeps the uncapped limit: the cap is planned, never enforced. The chart now tolerates the GPU vendor taints by default (`agent.tolerations`), so GPU Nodes are reached. | tolerating every taint, or the controller manager planning only Nodes that run an agent |
| D18 | MD01 (medium) | MIG-mixed and renamed time-slicing GPU Nodes get no GPU intent (`none`/`none`/"no GPU cap intent") and a CPU percent cap, as if they had no GPU: the controller manager counts only `*/gpu` resources (`discoverGPUCount`), not `nvidia.com/mig-*` or `nvidia.com/gpu.shared`. | the controller manager counting those resources |

D17 is fixed: a scheduler extender on the host network could not roll out on a single Node, because a rolling update starts the new pod before it stops the old one and both need the host port. The chart now deploys it with strategy `Recreate` when `schedulerExtender.hostNetwork` is true (case C07 of `scripts/verify-chart-renders.sh`; E14 rolls it out). `Recreate` leaves a moment with no extender, which is safe here because the scheduler configuration marks it `ignorable: true`; for a configuration that does not, see [Scheduler extender]({{< relref "/docs/getting-started/05-configuration-reference.md#scheduler-extender" >}}).

## End-to-end checks

`TestSimclusterE2E` (`tests/simcluster`, build tag `simcluster`) runs against the deployed cluster. `smoke.sh` runs it with `SIMCLUSTER_DIR` and `SIMCLUSTER_RUN_DIR` set; without `SIMCLUSTER_DIR` it skips. It changes the cluster as it goes (the caps go to 50 %, the extender is reloaded), so it expects a fresh deploy. The caps go to 50 % only when `E09.50pct` or `E10.50pct` runs, so a `-run` filter that leaves both out keeps a cluster at its deployed caps.

| ID | Checks |
|---|---|
| E01 | every spec Node is Ready, with the labels, taints, capacity and architecture `clusterspec` renders |
| E02 | no pod without `joulie.io/workload-class` is bound to a fake Node |
| E03 | every container runs the image just built and imported |
| E04 | one emulated agent exists for exactly the Nodes the agent DaemonSet would reach |
| E05 | each NodeHardware equals the in-process golden of its profile (`cmd/agent/testdata/hwemu`) |
| E06 | Prometheus holds series for every Node with a measured CPU package |
| E07 | every check query of the mode equals the emulated truth (E07c: the Kepler 0.7 data shape) |
| E08 | each NodeTwin's measured power is the truth, or `prometheus-no-data` where nothing measures |
| E09, E10 | lowering the CPU and GPU caps from 80 % to 50 % lowers the enforced limits and the NodeTwins' measured power (RAPL, DVFS, NVIDIA) |
| E11 | the control status of each hardware family |
| E12 | D15, above |
| E13 | the extender filters and scores a real pod |
| E14 | a forced extender reload rolls out; an agent reload with an unchanged binary does nothing |
| E15 | `status.sh` exits 0 |
| E16 | the facility inputs reach the NodeTwin gauges and the cooling stress |
| MD01 to MD04 | medium spec only: D18; no NFD labels caps through DVFS; no driver tools leaves the GPUs alone; an unmanaged pool has series and nothing else |
| GS01 | governor control on only: each agent's `NodeHardware` lists the governor presets its profile accepts, with the result each gives |
| GS02 | governor control on only: each `NodeTwin` asks for the preset its profile and node state (performance or eco) call for, and the agent's result for it is the one its profile predicts |
| GS03 | governor control on only: the emulated files hold the native words of every applied or equivalent result |
| GS04 | governor control on only, and last: after one `helm upgrade` to `GOVERNOR_CONTROL=off`, every governor result is restored or gone, and every file (governor, EPP, boost, AMD level and overdrive table, NVIDIA locks) is back at the baseline `NodeHardware` recorded |

GS01 to GS04 skip unless the deployed controller manager runs with `GOVERNOR_CONTROL` not `off`. `experiments/05-governor-exploration/scripts/run_tier_b.sh` deploys `tests/simcluster/specs/governor.yaml`, one pool per CPU frequency driver and per GPU family, with governor control on, and runs them (see [Governor exploration]({{< relref "/docs/experiments/governor-exploration.md" >}})). GS04 changes the release, so the cluster needs a fresh deploy afterwards.

## Run results

Each smoke run writes `$SIMCLUSTER_DIR/runs/<UTC>-<spec>-<mode>-<sha12>[-dirty]/`, and `runs/latest` points at the newest. `down.sh` keeps them.

- `summary.json`, schema `joulie.simcluster.summary/v1`: `test.suite.name`, `test.suite.run.status` (`success`, `failure`, `timed_out` or `aborted`), `exitCode`, `failures`, `warnings`, `knownDefects`, one entry per check under `assertions` (`test.case.name`, `test.case.result.status`, observed and expected), the spec, mode, scope and timing, the git commit and whether the tree was dirty, the images, and the semantic conventions schema URL.
- `otlp/harness.metrics.jsonl` and `otlp/harness.logs.jsonl`: one OTLP JSON object per line, with a `joulie.scenario.assertion.evaluated` log record and `joulie.scenario.assertion.value` points per check.
- `k8s/` (the Nodes, NodeHardware, NodeTwins, pods and events at the end), `logs/` (every pod in `joulie-system`, `joulie-sim` and `monitoring`, and k3s), `values.effective.yaml` and `values.simulator.effective.yaml`, `images.env`, `deploy.env`, `cluster.env`, and `smoke.log`, everything the run printed.

The scenario runner writes the same layout and schema, so one reader serves both.

## CI

`.github/workflows/simcluster.yml` has two jobs; the Dagger integration job (`integration-tests.yml`) stays as it is.

- **`smoke`**, on pull requests and pushes to `main` that touch `cmd/`, `pkg/`, `api/`, `charts/`, `config/`, `simulator/`, `ci/`, `scripts/simcluster/`, `tests/simcluster/`, `values/simcluster*.yaml`, the `Makefile`, `go.mod`, `go.sum` or the workflow, and on demand. It vets the test, runs the harness's own tests (`TestE2E*`), then `make simcluster-smoke`, with a 30 minute timeout around the test's 20. The air-gap bundle is cached by the hash of `versions.env`.
- **`nightly`**, daily at 03:23 UTC and on demand: the medium spec in `production` timing, and the smoke spec once for each of `otel-unpromoted`, `otel-labels-unpromoted`, `otel-utf8`, `otel-collector`, `kepler`, `kepler-legacy`, and `otel` with host scope. One failing variant does not cancel the others.

Every job uploads its `runs/` directory, also when it fails.
