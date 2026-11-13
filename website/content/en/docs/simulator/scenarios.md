---
title: "Workload Scenarios"
weight: 12
---

A workload scenario runs a workload through the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) and checks what happened: whether the jobs completed, where they were placed, how long they took, what energy they used, and how Joulie's settings compare with each other or with no Joulie at all. The jobs are real Kubernetes Jobs on KWOK Nodes, driven by the simulator's v1 engine on the emulated hardware, so a cap the agent writes slows the jobs on that Node.

A scenario is one file in `tests/simcluster/scenarios/`. `scripts/simcluster/scenario.sh` runs it and `simscenario` (`simulator/cmd/simscenario`) validates, plans and evaluates it.

## Quick start

```bash
make simcluster-scenario SCENARIO=tests/simcluster/scenarios/smoke.yaml
jq '{status: ."test.suite.run.status", failures, jobs}' ~/.cache/joulie/simcluster/joulie-sim/runs/latest/summary.json
make simcluster-down
```

The first command creates or reuses the cluster, deploys the simulator and Joulie from the working tree, runs the jobs and evaluates them. It exits 0 when every assertion of severity `fail` passed, 1 when one failed, and 2 when the scenario is invalid or a variant could not be run. The cluster is kept, so a second scenario on the same spec starts in seconds.

To check a scenario without a cluster:

```bash
go run ./simulator/cmd/simscenario validate tests/simcluster/scenarios/nightly-mixed.yaml
go build -o /tmp/bin/ ./simulator/cmd/workloadgen
go run ./simulator/cmd/simscenario plan tests/simcluster/scenarios/nightly-mixed.yaml -out /tmp/plan -workloadgen /tmp/bin/workloadgen
```

`plan` writes the trace and prints the offered load per resource; it refuses a load above 0.9, or a run whose work cannot end well before its deadline.

## The library

| Scenario | Cluster spec, timing | Checks |
|---|---|---|
| `smoke` | `smoke`, dev; about 7 minutes | 9 jobs on CPU, NVIDIA and AMD Nodes all complete, with no stall and no stray pod |
| `perf-isolation` | `policy`, dev | no performance pod is bound to an eco Node |
| `cap-cost` | `policy`, dev | an eco CPU cap slows CPU jobs by more than 5 %, and GPU jobs with no CPU feed by at most 2 % |
| `deadlock-visible` | `smoke`, dev | a gang that cannot fit is reported stalled and never completes |
| `demo` | `demo`, dev; 40 minutes of arrivals, 10 to drain | 112 jobs on 10 Nodes in five hardware families, the cluster of the `kubectl joulie` recording; at least 90 % complete |
| `nightly-mixed` | `nightly`, production; a 60 minute run, 20 to drain | a workloadgen trace at 0.3 to 0.8 offered load; at least 95 % complete, no performance pod on eco, twin power within 15 % (warning) |
| `weekly-compare` | `nightly`, production | the nightly workload with `joulie-off`, `queue-aware` and `kepler-compat` variants; Joulie spends no more energy than `joulie-off` (warning) |

The specs `policy.yaml` and `nightly.yaml` in `tests/simcluster/specs/` carry no `baseLoad`, so a Node idles until a run's jobs load it.

## Scenario files

A scenario is decoded strictly: an unknown key anywhere is an error. Durations are Go durations such as `90s` or `4m`.

| Key | Meaning |
|---|---|
| `schemaVersion` | `1` |
| `name` | the file stem, a DNS label |
| `cluster.spec`, `cluster.timing` | a cluster spec, relative to the repository root; `dev` (default) or `production` |
| `seed` | seeds the plan and workloadgen |
| `duration`, `drain` | the submission window (at most 6 h) and the extra time for submitted jobs to finish |
| `abortOnRunStalled` | end the run once `joulie.sim.run.stalled` has lasted this long |
| `isolation` | `cluster` (each variant on a fresh cluster, the default with several variants) or `reuse` (one variant only) |
| `workload` | the run's settings and exactly one source: `jobs`, `generate` or `trace` |
| `joulie` | `enabled` (default true), `inputPreset` (`otel`, `kepler` or `kepler-legacy`), `set` (Helm `--set` pairs) and `values` (values files) |
| `simulator.set` | Helm `--set` pairs for the simulator release |
| `variants` | up to 4, each a `name`, a `description` and its own `joulie` and `simulator`, merged over the top level |
| `capture` | `interval` of the NodeTwin captures (default 15s) and `prometheus` range queries recorded as `joulie.capture.*` gauges |
| `assertions`, `compare` | below |

A scenario may not set `NODE_POWER_*` or `FACILITY_*`: it reaches Joulie's inputs only through `joulie.inputPreset`, so it always tests a preset Joulie ships.

### The workload

`workload.namespace`, `owner`, `intent`, `completion`, `jobTTL`, `stallReport`, `baseSpeedPerCore` and `placement` become the run's options (the `SIM_WORKLOAD_*` defaults of [the configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) apply to what is unset). The source is one of:

| Source | What it is |
|---|---|
| `jobs` | inline job kinds: `name`, `class`, `count` groups `every` apart, `pods` (a gang with `gang: true`), `requests`, `gpus`, `runtime`, `workClass`, `profile` (with `peerTransferIntensity`, see [Peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})), `sensitivity`, `affinity`, `nodeSelector`, `tolerations` |
| `generate.workloadgen` | workloadgen flags without the dash, from an allowlist |
| `trace` | a v3 trace file, relative to the repository root |

A GPU job needs its GPU vendor's toleration (`nvidia.com/gpu`, `amd.com/gpu`) in `tolerations` or in `placement.tolerations`: k3s adds none.

### Assertions

Every assertion has a `name`, a `type`, a `severity` (`fail`, the default, or `warn`) and an `expect` comparator such as `">= 0.95"`, `"== 0"`, `"< 90s"` or `"> 0 J"`. One with nothing to evaluate fails with "no data".

| Type | Fields | Observed |
|---|---|---|
| `jobs` | a job filter (`class`, `workload`, `nodeProfile`); `expect` per count: `completedFraction`, `completed`, `failed`, `incomplete` | the jobs due before the duration |
| `latency` | a job filter, `metric` (`duration`, `queueWait`, `runTime`, `slowdown`), `quantile` (0.5 to 1, or `max`), optional `reference` filter | the quantile over completed jobs, or its ratio to the reference's |
| `stall` | `expect` per reason: `gangIncomplete`, `noProgress`, `runStalled` | stall events |
| `placement` | `class`, `forbidden` schedulable classes, `graceSeconds` | binds of that class to a Node that held a forbidden class for at least the grace |
| `energy` | `scope` (`total`, `class:<class>`, `node:<node>`), `hwType` (`cpu`, `gpu`, `all`), `perJob` | `joulie.sim.workload.energy` at the end |
| `events` | `event`, `where` attribute values | matching log records |
| `objects` | `expect` per count: `jobsNotComplete`, `strayPodsOnFakeNodes`, `nodeHardwares`, `controlBlocked` | the objects at the end |
| `twinPower` | `warmup`, `tolerance`, `quantile`, `nodes` | the relative error between each NodeTwin's measured power and its Node's energy rate |
| `promql` | `query`, `at: end` | one instant query at the end of the run |

`slowdown` is run time over ideal run time, so queueing does not count.

### Comparisons

`compare` entries take a `metric` of a `variant` over the same metric of a `reference` variant, with an `expect` on the ratio and a `severity`. Metrics: `energy.total`, `class.<class>.energy`, `class.<class>.throughput`, and `class.<class>.<duration|queueWait|slowdown>.<p50|p95|max>`.

## Results

A run directory, `$SIMCLUSTER_DIR/runs/<UTC>-<scenario>-<sha12>[-dirty]/`, holds `scenario.effective.yaml`, `scenario.log`, a `summary.json` and one directory per variant (a scenario without variants writes these at the top):

| File | Contents |
|---|---|
| `summary.json` | schema `joulie.simcluster.summary/v1`, as the smoke test's, plus `jobs`, `classes` (throughput, energy, slowdown and duration quantiles per class), `energy` and `comparisons` |
| `otlp/*.jsonl` | the simulator's and the runner's records as OTLP JSON Lines, with the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md#workload-run-and-scenario-names" >}}) |
| `trace.jsonl`, `variant.env`, `k8s/`, `logs/`, ... | the inputs, the cluster snapshots and the component logs |

## Settings

`scenario.sh` reads these besides the settings of `up.sh` and `deploy.sh` ([Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md#settings" >}})).

| Variable | Default | Effect |
|---|---|---|
| `SCENARIO` | (none) | The scenario file, for `make simcluster-scenario` |
| `SCENARIO_VARIANTS` | (empty: all) | Run only these variants, space separated; the run is then not summarized, so `simscenario summarize` can compare variants run on separate machines |
| `SCENARIO_RUN_DIR` | a new `$SIMCLUSTER_DIR/runs/<UTC>-<scenario>-<sha12>[-dirty]` | The run directory; `runs/latest` is left alone when it is set |
| `SIMCLUSTER_VALIDATE_OTLP` | (unset) | `1` posts every OTLP line of the run to an OpenTelemetry Collector and fails the variant if any is rejected |
| `SIMCLUSTER_SCENARIOS` | (unset) | `1` makes `make simcluster-smoke` also run the `smoke` scenario after the end-to-end test |

## simscenario

`simscenario validate|plan|run|evaluate|summarize` (exit codes 0 success, 1 a failed assertion, 2 an invalid scenario or run).

## CI

CI runs `nightly-mixed` and `deadlock-visible` Monday to Saturday and `weekly-compare` on Sundays ([`tests/simcluster/README.md`](https://github.com/joulie-k8s/Joulie/blob/main/tests/simcluster/README.md#ci)).

How a run works step by step, what `plan` checks, the library's workloads in full, and the detail of every field and result file: [Scenario runs](https://github.com/joulie-k8s/Joulie/wiki/Scenario-Runs) on the wiki.
