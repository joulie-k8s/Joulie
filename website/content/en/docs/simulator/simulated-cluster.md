---
title: "Simulated Cluster"
weight: 10
---

The simulated cluster runs the real Joulie components on emulated bare-metal nodes, on one machine with Docker. The agent discovers and caps emulated hardware through the same files and tools it uses on a real node, the controller manager reads node power from Prometheus, and the scheduler extender scores real pods.

Use it to see a change to the agent, the controller manager, the extender, the chart or the simulator work across components. A change to discovery or capping alone is checked faster by the hwemu tests in `cmd/agent` (`go test ./cmd/agent/ -run Hwemu`).

It is one k3s container in Docker: its own kubelet runs every real pod (Prometheus, the simulator in [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}), the controller manager, the extender and one unmodified agent per fake Node), and KWOK keeps the fake Nodes of the [cluster spec]({{< relref "/docs/simulator/emulate-mode.md#the-cluster-spec" >}}) Ready. The agents reach their Node's files through the simulator's [FUSE views]({{< relref "/docs/simulator/emulate-mode.md#fuse-mode" >}}) by default. 200 emulated nodes take about 0.1 s per tick ([Tick cost]({{< relref "/docs/simulator/emulate-mode.md#tick-cost" >}})).

## Requirements

- **Host**: amd64 Linux with Docker allowed to run a `--privileged` container.
- **FUSE**: `/dev/fuse` on the host for `SIMCLUSTER_HWEMU=fuse`, the default; `files` needs none.
- **Tools**: `docker`, `kubectl`, `helm`, `jq`, `sha256sum` and Go.
- **cgroup v1 hosts**: only the pinned k3s 1.34 is tested there (`stat -fc %T /sys/fs/cgroup` prints `tmpfs` on cgroup v1).
- **inotify**: a large run may exhaust `fs.inotify.max_user_instances` (often 128); check it with `sysctl` and raise it by hand, since the scripts never change host settings.
- **Pod capacity**: every emulated agent is a pod on the one infra Node; `up.sh` sizes it for the agents plus 50, up to 1000 pods.
- **Network on the first run**: images are pulled once; `SIMCLUSTER_AIRGAP=1` takes the k3s system images from the release's air-gap bundle.

## Quick start

```bash
make simcluster-smoke SIMCLUSTER_KEEP=1
D=~/.cache/joulie/simcluster/joulie-sim-smoke
jq '{status: ."test.suite.run.status", failures}' $D/runs/latest/summary.json
export KUBECONFIG=$D/kubeconfig
kubectl get nodes -L sim.joulie.io/pool
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,SRC:.status.powerMeasurement.source,W:.status.powerMeasurement.measuredNodePowerW
make simcluster-down SIMCLUSTER_NAME=joulie-sim-smoke
```

`make simcluster-smoke` creates the cluster `joulie-sim-smoke` from `tests/simcluster/specs/smoke.yaml` (nine Nodes: Intel, AMD, NVIDIA and AMD GPU nodes, a VM, a tainted node and an arm64 node), runs `TestSimclusterE2E`, collects the results and deletes the cluster unless `SIMCLUSTER_KEEP=1`. The kubeconfig is `$SIMCLUSTER_DIR/kubeconfig`; `~/.kube` is never read or written.

To keep a cluster and work on it step by step:

```bash
make simcluster-up       # k3s, KWOK, Prometheus, the fake Nodes
make simcluster-deploy   # images, simulator, Joulie; waits until every component reports
make simcluster-status
```

## Targets

Each target runs one script of `scripts/simcluster`; `make -n` shows what will run, and each script starts with its usage.

| Target | Script | What it does |
|---|---|---|
| `simcluster-up` | `up.sh` | Validates the spec, creates or reuses the k3s container, installs KWOK and Prometheus (and the Collector where the mode needs it), applies the spec's Nodes |
| `simcluster-images` | `images.sh` | Builds and imports the Joulie images and writes `images.env`; an unchanged binary is not imported again |
| `simcluster-deploy` | `deploy.sh` | Images, CRDs, the simulator, the agents, Joulie; waits until every component reports, then checks every pod's image |
| `simcluster-reload C=<component>` | `reload.sh` | Rebuilds and rolls out `agent`, `controller-manager`, `scheduler`, `simulator` or `all`; see [the dev loop](#the-dev-loop) |
| `simcluster-status` | `status.sh` | Prints every Node with its pool, profile and NodeTwin power; exits 1 when something is not ready or not reporting |
| `simcluster-tree [NODE=<node>]` | `tree.sh` | Lists the Nodes that have a tree, or prints every file of one Node's tree with its first line |
| `simcluster-logs C=<component> [NODE=<node>]` | `logs.sh` | The last 200 lines of `agent`, `controller-manager`, `scheduler`, `simulator`, `prometheus`, `otelcol`, `kwok` or `k3s` |
| `simcluster-smoke` | `smoke.sh` | See [Quick start](#quick-start) |
| `simcluster-down` | `down.sh` | Removes the container and its volumes; keeps `images/`, `runs/` and `bin/` of the state directory |

## Settings

The scripts read these variables from the environment; `make` passes on the ones that are set.

| Variable | Default | Effect |
|---|---|---|
| `SIMCLUSTER_NAME` | `joulie-sim`; `joulie-sim-smoke` for the smoke run | Container name and prefix of its volumes; must match `^joulie-sim(-[a-z0-9]+)*$` |
| `SIMCLUSTER_DIR` | `${XDG_CACHE_HOME:-~/.cache}/joulie/simcluster/<name>` | State directory, outside the repository |
| `SIMCLUSTER_SPEC` | `tests/simcluster/specs/smoke.yaml` | The cluster spec; `medium.yaml` (50 Nodes), `profiles.yaml` (one Node per builtin profile) and `demo.yaml` (10 Nodes) are in the same directory |
| `SIMCLUSTER_NODES` | (empty) | Instead of a spec, this many Nodes of one profile, for scale runs |
| `SIMCLUSTER_SCALE_PROFILE` | `intel-xeon-2s-rapl` | The profile of `SIMCLUSTER_NODES` |
| `SIMCLUSTER_TELEMETRY` | `otel` | The [telemetry mode](#telemetry-modes) |
| `SIMCLUSTER_POWER_SCOPE` | `components` | The node power scope Joulie reads: `components` (CPU packages and GPUs) or `host` |
| `SIMCLUSTER_TIMING` | `dev` | The [timing profile](#timing): `dev` or `production` |
| `SIMCLUSTER_HWEMU` | `fuse` | How the emulated agents reach their Node's files: `fuse` (the simulator's FUSE views) or `files` (the trees' own files) |
| `SIMCLUSTER_EXTENDER` | `hostnet` | How the k3s scheduler reaches the extender: `hostnet` or `service`; switching recreates the container |
| `SIMCLUSTER_PROFILES_DIR` | (empty) | Local hardware profiles, for the host tools only; the simulator in the cluster reads its builtin ones |
| `SIMCLUSTER_EXTRA_VALUES` | (empty) | Joulie values files, space separated, installed after the generated ones |
| `SIMCLUSTER_SIM_EXTRA_VALUES` | (empty) | Simulator values files, space separated; later reloads install with them too |
| `SIMCLUSTER_KEEP` | `0` | `1` keeps the smoke cluster after the run |
| `SIMCLUSTER_TEST_TIMEOUT` | ten `RATE_WINDOW`s of the timing, at least `20m` (`20m` in `dev`, `50m` in `production`) | Timeout of `TestSimclusterE2E` in the smoke run |
| `SIMCLUSTER_AIRGAP` | `0` | `1` loads the k3s system images from the release's air-gap bundle |
| `SIMCLUSTER_FORCE_RELOAD` | `0` | `1` makes every reload a `reload.sh --force` |
| `COMPONENT` (or `C`), `NODE` | (empty) | The component and the Node of `simcluster-reload`, `simcluster-logs` and `simcluster-tree` |

## Telemetry modes

A mode sets how the emulated Nodes' power reaches Prometheus and which Joulie settings read it; every mode is checked against the simulator's truth (`/debug/emulate`), within 3 %.

| `SIMCLUSTER_TELEMETRY` | Path to Prometheus |
|---|---|
| `otel` (default) | the simulator pushes OTLP to Prometheus, which promotes `k8s.node.name` and `host.name` |
| `otel-unpromoted`, `otel-labels-unpromoted`, `otel-utf8` | as `otel`, without the promotion or with another name translation, to check what Joulie reads then |
| `otel-collector`, `otel-bridge` | through an OpenTelemetry Collector: OTLP, or the [compatibility exporters]({{< relref "/docs/simulator/metrics.md#compatibility-exporters-emulate-mode" >}}) bridged to `hw.*` |
| `kepler`, `kepler-legacy` | Kepler 0.12 or 0.7 series, read with `NODE_POWER_PRESET=kepler` or `kepler-legacy` |

### Timing

`SIMCLUSTER_TIMING=dev` uses short intervals (a 10 s controller reconcile, a 30 s rate window); `production` uses the values a real install uses (1 min and 5 min).

## The dev loop

```bash
make simcluster-up && make simcluster-deploy       # once
# edit the agent
make simcluster-reload C=agent                     # rebuild, import, roll out; prints the seconds
make simcluster-logs C=agent NODE=intel-0
make simcluster-tree NODE=intel-0
make simcluster-status
```

- A reload rolls out one component, and only when its binary changed; `reload.sh <component> --force` (or `SIMCLUSTER_FORCE_RELOAD=1`) restarts it anyway.
- Another `SIMCLUSTER_SPEC` or mode applies with `make simcluster-up` and `make simcluster-deploy` again.
- To read Prometheus and the truth, through the API server proxy:

```bash
export KUBECONFIG=~/.cache/joulie/simcluster/joulie-sim/kubeconfig
P=/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy/api/v1/query
pq() { kubectl get --raw "$P?query=$(jq -rn --arg q "$1" '$q|@uri')" | jq -c '[.data.result[] | [.metric.k8s_node_name, .value[1]]]'; }
pq 'sum by (k8s_node_name) (rate(hw_host_energy_joules_total[30s]))'
kubectl get --raw /api/v1/namespaces/joulie-sim/services/joulie-telemetry-sim:80/proxy/debug/emulate | jq '[.nodes[] | {name, hostW: .host.powerW}]'
```

## Dashboard

The simulator chart ships the Grafana dashboard **Joulie - Simulated Cluster** (`charts/joulie-simulator/dashboards/joulie-simulated-cluster.json`): one cell per Node, coloured by its inlet temperature and labelled with the power profile Joulie applied, and below it each Node's profile and temperature over time, the outside air, IT and cooling power, PUE, host power, the power limits in force and rack power.

{{< img src="images/simulated-cluster-dashboard.png" alt="Joulie - Simulated Cluster on the demo spec under the workloads of tutorial 02, with the outside air raised from 22 to 30 °C halfway" >}}

The simulated cluster runs no Grafana, so run one beside it with the dashboard file and a port-forward to its Prometheus:

```bash
export KUBECONFIG=~/.cache/joulie/simcluster/joulie-sim/kubeconfig
kubectl -n monitoring port-forward svc/prometheus-operated 19090:9090 &
G=$(mktemp -d) && mkdir $G/datasources $G/dashboards && chmod -R a+rX $G
cat >$G/datasources/prometheus.yaml <<'EOF'
apiVersion: 1
datasources: [{name: Prometheus, uid: prometheus, type: prometheus, access: proxy, url: "http://127.0.0.1:19090", isDefault: true}]
EOF
cat >$G/dashboards/joulie.yaml <<'EOF'
apiVersion: 1
providers: [{name: joulie, type: file, options: {path: /var/lib/grafana/dashboards}}]
EOF
docker run -d --name joulie-grafana --network host -e GF_SERVER_HTTP_ADDR=127.0.0.1 \
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Admin -e GF_AUTH_DISABLE_LOGIN_FORM=true \
  -v $G/datasources:/etc/grafana/provisioning/datasources:ro -v $G/dashboards:/etc/grafana/provisioning/dashboards:ro \
  -v $PWD/charts/joulie-simulator/dashboards:/var/lib/grafana/dashboards:ro grafana/grafana:13.2.4
```

Open <http://127.0.0.1:3000/d/joulie-simulated-cluster>. Grafana listens on `127.0.0.1` only, with anonymous admin access; `docker rm -f joulie-grafana` and `kill %1` stop both. The panels fill in the modes `otel`, `otel-collector`, `kepler` and `kepler-legacy`. To change the outside air, edit the spec's `facility` block and run `make simcluster-up` and `make simcluster-deploy`. On a cluster whose Grafana loads dashboards from ConfigMaps, `grafanaDashboard.enabled=true` in the simulator chart installs it.

## Deviations from a real cluster

- **KWOK taint**: every fake Node carries `kwok.x-k8s.io/node=fake:NoSchedule`; a pod meant for one must tolerate it.
- **The `sim.joulie.io/cluster` selector**: workload pods select it, so none lands on the infra Node.
- **Agents are unprivileged Deployments** on the infra Node, not privileged host-network DaemonSet pods.
- **`/dev/nvidiactl` is an empty file**, not the driver's character device.
- **GPU tools are `fakesmi` stubs**; a pool's `toolDelivery` sets whether the driver files they need are present.
- **With `SIMCLUSTER_HWEMU=files`, writes and counters move once per tick**: a refused write reads back until the next tick, and energy changes once per `SIM_EMULATE_TICK`.
- **A simulator restart in FUSE mode** makes the agents' reads fail with `ENOTCONN` for seconds; they recover without a restart.
- **A root with directory limits is not resumed**: run `make simcluster-down` and deploy again.
- **Fake Nodes have no kubelet**: no container runs, and a Node's power is the simulator's model of its load.

## Checks

`make simcluster-smoke` runs `TestSimclusterE2E` (`tests/simcluster`, build tag `simcluster`) against a fresh deploy; each check is one entry under `assertions` in `summary.json`. The list of checks is in [`tests/simcluster/README.md`](https://github.com/joulie-k8s/Joulie/blob/main/tests/simcluster/README.md).

## Run results

Each smoke run writes `$SIMCLUSTER_DIR/runs/<UTC>-<spec>-<mode>-<sha12>[-dirty]/`, and `runs/latest` points at the newest; `down.sh` keeps them. The scenario runner writes the same layout.

| File | Contents |
|---|---|
| `summary.json` | schema `joulie.simcluster.summary/v1`: the run status, failures, warnings, known defects, one entry per check, and what was run |
| `otlp/harness.metrics.jsonl`, `otlp/harness.logs.jsonl` | the checks as OTLP JSON, one object per line |
| `k8s/`, `logs/` | the cluster objects at the end and every pod's log, k3s and containerd included |
| `values.effective.yaml`, `values.simulator.effective.yaml`, `images.env`, `deploy.env`, `cluster.env`, `smoke.log` | what was installed, and everything the run printed |

Architecture, the requirements in detail, every target's and setting's full effect, the timing table, what a real Prometheus needs, the dashboard's series and why each deviation exists: [Simulated cluster](https://github.com/joulie-k8s/Joulie/wiki/Simulated-Cluster) on the wiki.
