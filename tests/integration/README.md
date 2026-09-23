# Integration tests

Two things live under the name "integration" and they run in different places.

1. **The Dagger suite.** `ci/tests/integration_runner.py` drives a real
   2 node k3s cluster with `kubectl`, `helm` and `curl`. This is what CI runs,
   and it is where almost all of the coverage is.
2. **The Go tests in this directory**, behind the `integration` build tag. They
   talk to a cluster through `KUBECONFIG` and are run by the second Dagger
   function, `integration-new-components`, which CI does not currently call.

## The cluster

Built in `ci/src/joulie_ci/__init__.py`, from `rancher/k3s:v1.34.1-k3s1`:

- **Two nodes**: a k3s server named `k3s-server` and a k3s agent named
  `k3s-worker-0`. Two are required because the controller manager enforces a
  per hardware family performance floor: with a single managed node,
  `STATIC_HP_FRAC=0` cannot move that node to eco.
- Both nodes are labelled `joulie.io/managed=true` by the runner.
- k3s runs with `--snapshotter native` (overlayfs on overlayfs fails inside
  Dagger containers on the RHEL 8 kernel) and with traefik, metrics-server and
  the default registry endpoint disabled.
- The kube-scheduler is started with a `KubeSchedulerConfiguration` that
  registers the Joulie extender at `http://127.0.0.1:9876`, `weight: 5`,
  `ignorable: true`. The loopback address works because the extender pod runs
  with `hostNetwork: true`, pinned to `k3s-server`.
- The three images (`agent`, `controller-manager`, `scheduler`) are built from
  the working tree with `golang:1.23` onto `distroless/static:nonroot`, pushed
  to a `registry:2` sidecar with `crane`, and pulled by k3s through a
  `registries.yaml` mirror for `joulie-registry.local:5000`. Tags must start
  with `dev`; `latest` is never used.

## What runs, in order

`ci/scripts/run-integration.sh` first waits for the API server, for both nodes
to register and for both to be Ready, then runs the Python runner. Every phase
below logs its ID with the `[integration]` prefix.

Always, whatever the scope:

| ID | What it asserts |
|----|-----------------|
| `IT-BOOT-01` / `IT-HELM-01` | Both expected nodes are Ready and labelled; `helm upgrade --install joulie ./charts/joulie -n joulie-system` succeeds with `agent.mode=pool`, one shard, `RECONCILE_INTERVAL=5s`, `POLICY_TYPE=static_partition`; both CRDs are registered; flipping `STATIC_HP_FRAC` between 0 and 1 splits the two nodes into one `performance` and one `eco`, which the rest of the suite relies on |
| `IT-SCHED-EXT-01` | The scheduler extender Deployment is available, its Service exposes port 9876, the pod runs the image under test and its `EXTENDER_ADDR` ends in `:9876` |
| `IT-TP-01` | Telemetry and control over HTTP: the agent is pointed at the in-cluster mock with `TELEMETRY_CPU_SOURCE=http`, `TELEMETRY_CPU_CONTROL=http` (mode `dvfs`) and `TELEMETRY_GPU_CONTROL=http` (mode `powercap`), and the mock's counters must show both reads and control POSTs. Patching the NodeTwin GPU cap must then produce a GPU POST on a GPU node, or a `none`/`blocked`/`error` `controlStatus.gpu.result` on a node without one |
| `IT-TWIN-STATUS-01` | Both nodes get a non-empty `schedulableClass`, the three predicted scores and a `lastUpdated`, and the controller manager's twin status coexists with the agent's `controlStatus` |
| `IT-HW-01` | `NodeHardware` is published for both nodes with a non-zero `status.cpu.totalCores`, a `capabilities.cpuTelemetry` key and a non-empty `quality.overall` |

Then, only when the scope is `all` or `full` (the CI default):

| ID | What it asserts |
|----|-----------------|
| `IT-CLS-01..03` | Classification matrix: a pod annotated `performance` blocks the move to eco (guarded transition, `schedulableClass=draining`); a `standard` pod and an unannotated pod do not |
| `IT-FSM-01..` | Full FSM walk: performance to guarded draining and back to eco ready as the pod is deleted, then back to performance when `STATIC_HP_FRAC` returns to 1 |
| `IT-FSM-07` | A performance pod targeting an eco node stays Pending and the node does not flip |
| `IT-FSM-05` | Idempotency: over several reconcile cycles the power profile label does not flap, and no `joulie.io/draining` label is ever created (draining lives in `NodeTwin.status.schedulableClass`) |
| `IT-SCH-*` | End to end scheduling through the extender: performance pods land on the performance node and stay Pending on the eco node; standard pods schedule anywhere |
| `IT-SCHED-SCORE-01` | `POST /prioritize` on the live extender returns one score per node, each within 0 to 100 |
| `IT-SCHED-FILTER-01` | `POST /filter` puts the eco node in `failedNodes` for a performance pod and passes both nodes for an unannotated one |
| `IT-SCH-DRAIN-01` | A node in the draining state rejects a new performance pod |
| `IT-SCH-STD-01` | An unannotated pod runs on both nodes under eco |
| `IT-POLICY-SWITCH-01` | A rapid 1 to 0 to 1 policy switch converges with both nodes performance and not draining |
| `IT-TWIN-SCORES-01` | Twin scores and `lastUpdated` change when the profile changes |
| `IT-TWIN-STATUS-02` | The agent's `controlStatus` writes never wipe the controller manager's twin status over three agent cycles |
| `IT-FACILITY-01` | No `[facility]` line in the controller manager log, since `ENABLE_FACILITY_METRICS` defaults to false |

`--it-scope gpu-only` runs the first five phases and skips the rest.

On any failure the runner dumps nodes, pods, events, both CR kinds and the
controller manager and agent logs before exiting non-zero.

## The Go tests in this directory

All three files carry `//go:build integration` and read only `KUBECONFIG`:

| ID | Test | File |
|----|------|------|
| `IT-ARCH-01` | `TestIT_ARCH_01_CRDsRegistered` | `it_arch_test.go` |
| `IT-TWIN-01` | `TestIT_TWIN_01_ControllerManagerWritesNodeTwin` | `it_twin_test.go` |
| `IT-SCHED-01` | `TestIT_SCHED_01_FilterEcoDraining` | `it_sched_test.go` |
| `IT-SCHED-02` | `TestIT_SCHED_02_ScoringDeterminism` | `it_sched_test.go` |
| `IT-SCHED-03` | `TestIT_SCHED_03_AdaptivePressureRelief` | `it_sched_test.go` |
| `IT-SCHED-04` | `TestIT_SCHED_04_CPUOnlyGPUPenalty` | `it_sched_test.go` |
| `IT-FSM-01` | `TestIT_FSM_01_ExistingFSMWithNodeTwin` | `it_sched_test.go` |

They are compiled and run by `dagger call integration-new-components`, which
builds a single node cluster and then runs:

```bash
go test -v -tags=integration -timeout=10m ./tests/integration/...
```

Nothing in `.github/workflows/` calls that function today, so these tests do
not gate a pull request. Point `KUBECONFIG` at any cluster with Joulie
installed to run them by hand with the command above.

## Running locally

```bash
# Install the Dagger CLI: https://docs.dagger.io
cd ci && dagger call integration --source=..

# equivalently, from the repository root
dagger -m ./ci call integration --source=.

# the shorter GPU scoped run (first five phases only)
dagger -m ./ci call integration --source=. --it-scope gpu-only
```

`--source` is required. There is no per test selector; `--it-scope` takes
`all`, `full` or `gpu-only`.

CI runs this from `.github/workflows/integration-tests.yml` on pull requests
and pushes to `main` that touch `ci/`, `cmd/`, `charts/`, `config/`,
`simulator/`, `go.mod`, `go.sum` or that workflow.

## What this does NOT cover

- **Real RAPL.** Nothing in the suite reads `/sys/class/powercap`. CPU control
  is redirected to the HTTP mock, so the powercap write path
  (`applyRAPLPackageCap`, zone selection, DRAM sub zone rejection) is proven
  only by the fixture based unit tests in `cmd/agent`.
- **GPUs.** No node has one. The GPU control assertion falls back to checking
  that the agent degrades gracefully.
- **The Prometheus node power source.** `NODE_POWER_SOURCE` is never set, so
  the controller manager runs with the default `static`, meaning 0 W. Note
  that `TELEMETRY_CPU_SOURCE=http` configures the agent's telemetry, which is
  a different road into the twin.
- **Leader election.** The controller manager runs one replica and the chart
  is installed with leader election off. No Lease is ever contended. The chart
  side of it, the Role and RoleBinding, is covered by
  `hack/verify-chart-renders.sh`.
- **Chart upgrades.** `helm upgrade --install` runs exactly once per job, as a
  first install. Every later configuration change is a `kubectl set env`, so no
  upgrade path is exercised.
- **Scale.** Two nodes. Behaviour at hundreds of nodes lives in
  `experiments/` on KWOK, not in CI.
