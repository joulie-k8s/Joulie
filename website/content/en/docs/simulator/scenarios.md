---
title: "Workload Scenarios"
weight: 12
---

A workload scenario runs a workload through the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) and checks what happened: whether the jobs completed, where they were placed, how long they took, what energy they used, and how Joulie's settings compare with each other or with no Joulie at all. The jobs are real Kubernetes Jobs, scheduled by the real scheduler and the Joulie extender onto KWOK Nodes, and the simulator's v1 workload engine drives them on the emulated hardware, so a cap the agent writes slows the jobs on that Node.

A scenario is one file in `tests/simcluster/scenarios/`. `scripts/simcluster/scenario.sh` runs it and `simscenario` (`simulator/cmd/simscenario`) validates, plans and evaluates it.

## Quick start

```bash
make simcluster-scenario SCENARIO=tests/simcluster/scenarios/smoke.yaml
jq '{status: ."test.suite.run.status", failures, jobs}' ~/.cache/joulie/simcluster/joulie-sim/runs/latest/summary.json
make simcluster-down
```

The first command creates the cluster (or reuses a running one), deploys the simulator and Joulie built from the working tree, runs the scenario's jobs and evaluates them. It exits 0 when every assertion of severity `fail` passed, 1 when one failed, and 2 when the scenario is invalid or a variant could not be run. The cluster is kept, so a second scenario on the same spec starts in seconds; `make simcluster-down` deletes it.

To check a scenario without a cluster:

```bash
go run ./simulator/cmd/simscenario validate tests/simcluster/scenarios/nightly-mixed.yaml
go build -o /tmp/bin/ ./simulator/cmd/workloadgen
go run ./simulator/cmd/simscenario plan tests/simcluster/scenarios/nightly-mixed.yaml -out /tmp/plan -workloadgen /tmp/bin/workloadgen
```

`plan` writes the trace and prints, for each resource, the offered load (the sum over the jobs due in the window of request times ideal runtime, over capacity times the window) and when the work can end at the earliest: the latest, over the due times T, of T plus the runtime of a job due then and of T plus the work due from T on over the capacity, against the deadline (duration plus drain). It refuses a load above 0.9, which would queue jobs past any drain, and an earliest end past 0.9 of the deadline, which a burst of late arrivals or a long last job causes at any load. For a variant on `static_partition` (the controller manager's default policy, read from `joulie.set` with its `STATIC_HP_FRAC` and `NODE_SELECTOR`), performance pods run only on the Nodes the policy keeps in performance (`policy.BuildStaticPlan` over the spec's Nodes), so `plan` checks the performance jobs against those Nodes too and prints that table separately. A variant without Joulie, or on `queue_aware_v1`, which grows its performance set with the demand, is checked on every Node.

## The library

| Scenario | Cluster spec, timing | Workload | Checks |
|---|---|---|---|
| `smoke` | `smoke`, dev; about 7 minutes | 9 inline jobs: CPU and GPU, both classes, NVIDIA and AMD | every job completes through KWOK; no stall; no Job left incomplete and no stray pod on a fake Node; 9 `joulie.sim.job.completed` events; energy above 0 J |
| `perf-isolation` | `policy`, dev | performance and standard CPU jobs, and one standard job pinned to the eco Node `intel-2` | no performance pod is bound to a Node that was eco for longer than the placement grace; standard jobs completed on an eco Node and `intel-2` drew CPU energy, so the check is not vacuous |
| `cap-cost` | `policy`, dev | CPU jobs that nearly fill an intel Node; GPU jobs with no CPU feed | `cpu-cap-cost`: the median slowdown of CPU jobs on the eco Node exceeds that on performance Nodes by more than 5 %; `gpu-unaffected-by-cpu-cap`: the median slowdown of the GPU jobs stays within 2 % |
| `deadlock-visible` | `smoke`, dev | one gang of 2 pods of 8 GPUs, both pinned to `nvl-0`, which has 8 | the run reports `joulie.sim.run.stalled` and a `gang_incomplete` stall, and the gang never completes |
| `demo` | `demo`, dev; 40 minutes of arrivals, 10 to drain | 112 inline jobs on 10 Nodes in five hardware families: NVIDIA training and inference, AMD fine-tuning, performance and standard CPU jobs, half the Nodes in eco; the cluster of the `kubectl joulie` recording, `scripts/kubectl-joulie-demo.sh` | no performance pod is bound to an eco Node; at least 90 % of the jobs complete |
| `nightly-mixed` | `nightly`, production; a 60 minute run, 20 to drain | workloadgen seed 7: arrivals at a constant rate over 55 minutes, jobs of at most 15 minutes; offered load 0.3 to 0.8 of the CPUs and both GPU kinds, of the cluster and, for the performance jobs, of the performance Nodes | at least 95 % of the due jobs complete; no performance pod on eco; no run stall; the NodeTwins' measured power within 15 % of the emulated energy rate at p95 (warning) |
| `weekly-compare` | `nightly`, production | the nightly workload, three variants on fresh clusters: `joulie-off`, `queue-aware`, `kepler-compat` | per variant as nightly; Joulie spends no more energy than `joulie-off`, and its performance jobs take at most 10 % longer at p95 (warnings) |

`cap-cost` sets `GPU_ECO_CAP_PCT_OF_MAX=100`, so every GPU is uncapped and the eco CPU cap is the only cap in play. The controller manager gives the CPU of a GPU Node the performance cap whatever its profile, and a GPU job whose CPU feed intensity is 0 would not feel a CPU cap anyway, so `gpu-unaffected-by-cpu-cap` checks that the CPU cap costs such a job nothing. No GPU cap is measured. `perf-isolation` and `cap-cost` pin `static_partition` with `STATIC_HP_FRAC=0.5`, which on the `policy` spec puts `intel-2`, `epyc-1` and `nvl-1` in eco.

The specs the library adds, in `tests/simcluster/specs/`, carry no `baseLoad`, so a Node idles until a run's jobs load it:

- `policy.yaml`: 3 `intel-xeon-2s-rapl`, 2 `amd-epyc-2s-energy-only`, 2 `nvidia-h100-nvl-8gpu`. Every family has more than one Node, so the policy keeps an eco Node in each.
- `nightly.yaml`: 16 `intel-xeon-2s-rapl`, 12 `amd-epyc-2s-energy-only`, 4 `amd-epyc-2s-hsmp`, 8 `nvidia-h100-nvl-8gpu`, 4 `nvidia-h100-sxm-8gpu`, 4 `amd-instinct-mi300x-8gpu`, 2 `vm-no-powercap`.


## How a run works

`scenario.sh <file>`:

1. builds `simscenario` and `workloadgen`, validates the scenario and plans the run directory: `scenario.effective.yaml` (the scenario with its defaults filled in) and, per variant, `trace.jsonl` and `variant.env`;
2. per variant:
   - deletes the cluster first when the scenario isolates its variants (it lists more than one, or sets `isolation: cluster`), then runs `up.sh`, which creates the cluster or reuses a running one and switches it to the variant's spec;
   - runs `deploy.sh` with the variant's spec, timing, telemetry mode and Joulie values files. `joulie.set` and `simulator.set` become values files, rendered by `helm` from the `--set` arguments so they mean exactly what `--set` means;
   - with `joulie.enabled: false`, uninstalls Joulie, deletes its NodeTwins and NodeHardwares, removes the `joulie.io/power-profile` labels and, once no Joulie pod is left, renders the Nodes' hwemu trees again: an agent writes its limits into the tree and never restores them, so without this the variant would run under the caps the agents wrote during the deploy;
   - deletes and creates the run's namespaces, and copies the trace into the simulator's run directory, `<SIM_HWEMU_ROOT>/_runs/<run id>/trace.jsonl`, with `docker exec`;
   - `simscenario run`: checks the KWOK `pod-complete` Stage, snapshots the Nodes and Joulie's objects, starts the run through the simulator's run API, captures the NodeTwins every capture interval until the run finishes, its deadline (duration plus drain) passes or it stays stalled for `abortOnRunStalled`, snapshots again, finishes the run (deleting its Job objects), records the Prometheus captures and collects the component logs. Everything goes through the API server, nothing is port-forwarded;
   - copies the simulator's `otlp/` files out of the cluster, and `simscenario evaluate` writes the variant's `summary.json`;
3. `simscenario summarize` writes the run's `summary.json` with the comparisons across variants, and `runs/latest` points at the run directory.

## Scenario files

A scenario is decoded strictly: an unknown key anywhere, including inside an assertion, is an error. Durations are Go durations such as `90s` or `4m`.

| Key | Meaning |
|---|---|
| `schemaVersion` | `1` |
| `name` | the file stem, a DNS label |
| `cluster.spec`, `cluster.timing` | a cluster spec, relative to the repository root; `dev` (default) or `production` |
| `seed` | seeds the plan and workloadgen |
| `duration`, `drain` | the submission window (at most 6 h; jobs due later are dropped) and the extra time for submitted jobs to finish |
| `abortOnRunStalled` | end the run once `joulie.sim.run.stalled` has lasted this long |
| `isolation` | `cluster` (each variant on a fresh cluster, the default with several variants) or `reuse` (allowed with one variant only) |
| `workload` | the run's settings and exactly one source: `jobs`, `generate` or `trace` (below) |
| `joulie` | `enabled` (default true), `inputPreset` (`otel`, `kepler` or `kepler-legacy`), `set` (Helm `--set` pairs) and `values` (values files) |
| `simulator.set` | Helm `--set` pairs for the simulator release |
| `variants` | up to 4, each a `name`, a `description` and its own `joulie` and `simulator`, merged key by key over the top level |
| `capture` | `interval` of the NodeTwin captures (default 15s) and `prometheus` range queries recorded as gauges named `joulie.capture.*` |
| `assertions`, `compare` | below |

A scenario may not set `NODE_POWER_*` or `FACILITY_*`, neither in `joulie.set` nor in a values file: it reaches Joulie's inputs only through `joulie.inputPreset`, so it always tests a preset Joulie ships.

`joulie.set` and `simulator.set` take `--set` syntax: `{a,b}` is a list, and a comma inside a value needs a backslash.

### The workload

`workload.namespace`, `owner`, `intent`, `completion`, `jobTTL`, `stallReport`, `baseSpeedPerCore` and `placement` become the run's options (the `SIM_WORKLOAD_*` defaults of [the configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) apply to what is unset). `baseSpeedPerCore` (default 1) sizes the trace and is sent with the run, so ideal runtimes hold whatever the simulator was deployed with. `placement` replaces the simulator's default node selector (`sim.joulie.io/cluster=<spec>`) and tolerations (the KWOK taint); an empty list or map clears the default, an unset one keeps it.

The sources:

- `jobs`: inline job kinds. Each is `name`, `class` (`performance` or `standard`), `count` groups submitted `every` apart, `pods` per group (a gang with `gang: true`), `requests` (cpu and memory), `gpus` (`resource`: `nvidia.com/gpu`, `amd.com/gpu`, `nvidia.com/gpu.shared` or `nvidia.com/mig-<profile>`, and `count`), the ideal `runtime`, `workClass`, `profile` (`cpuUtilization`, `gpuUtilization`, `memoryIntensity`, `ioIntensity`, `cpuFeedIntensityGpu`, and `peerTransferIntensity`, the share of a multi-GPU job's GPU time spent copying between its GPUs, see [Peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})), `sensitivity`, and the pod's `affinity`, `nodeSelector` and `tolerations`.
- `generate.workloadgen`: workloadgen flags without the dash, from an allowlist. The runner sets `seed`, `base-speed-per-core`, `out` and `schema-version` (always v3) itself.
- `trace`: a v3 trace file, relative to the repository root.

k3s does not run the `ExtendedResourceToleration` admission plugin, so nothing adds the GPU vendor's toleration to a pod that requests GPUs. The GPU profiles taint their Nodes (`nvidia.com/gpu`, `amd.com/gpu`), so a GPU job needs that toleration in its `tolerations`, or the run's `placement.tolerations` (as the nightly scenarios do, restating the KWOK toleration the list replaces).

### Assertions

Every assertion has a `name`, a `type` and a `severity` (`fail`, the default, or `warn`), and an `expect` comparator such as `">= 0.95"`, `"== 0"`, `"< 90s"` or `"> 0 J"`. An assertion with nothing to evaluate fails with "no data".

| Type | Fields | Observed |
|---|---|---|
| `jobs` | a job filter (`class`, `workload`, `nodeProfile`); `expect` per count: `completedFraction`, `completed`, `failed`, `incomplete` | the jobs due before the duration |
| `latency` | a job filter, `metric` (`duration`, `queueWait`, `runTime`, `slowdown`), `quantile` (0.5 to 1, or `max`), optional `reference` filter | the quantile over completed jobs, or its ratio to the reference jobs' quantile |
| `stall` | `expect` per reason: `gangIncomplete`, `noProgress`, `runStalled` | stall events |
| `placement` | `class`, `forbidden` schedulable classes, `graceSeconds` | binds of that class to a Node that had held a forbidden class for at least the grace (default the extender's `CACHE_TTL` plus the reconcile interval) |
| `energy` | `scope` (`total`, `class:<class>`, `node:<node>`), `hwType` (`cpu`, `gpu`, `all`), `perJob` | `joulie.sim.workload.energy` at the end |
| `events` | `event`, `where` attribute values | matching log records |
| `objects` | `expect` per count: `jobsNotComplete`, `strayPodsOnFakeNodes`, `nodeHardwares`, `controlBlocked` | the objects at the end |
| `twinPower` | `warmup`, `tolerance`, `quantile`, `nodes` | the quantile of the relative error between each NodeTwin's measured power and its Node's energy rate |
| `promql` | `query`, `at: end` | one instant query at the end of the run |

`slowdown` is run time over ideal run time, so queueing does not count. The `nodeProfile` of a job is its Node's power profile when its pod was bound.

### Comparisons

`compare` entries take a `metric` of a `variant` over the same metric of a `reference` variant, with an `expect` on the ratio and a `severity`. Metrics: `energy.total`, `class.<class>.energy`, `class.<class>.throughput`, and `class.<class>.<duration|queueWait|slowdown>.<p50|p95|max>`.

## Results

A run directory, `$SIMCLUSTER_DIR/runs/<UTC>-<scenario>-<sha12>[-dirty]/`, holds `scenario.effective.yaml`, `scenario.log` and a `summary.json`; each variant writes `<variant>/` (a scenario without variants writes these at the top):

- `summary.json`, schema `joulie.simcluster.summary/v1`, the same as the smoke test's: `test.suite.name`, `test.suite.run.status`, `exitCode`, `failures`, `warnings`, one entry per check under `assertions`, and for scenarios `jobs`, `classes` (throughput, energy, slowdown and duration quantiles per class), `energy` (boundary `cpu packages + gpus`) and `comparisons`;
- `otlp/simulator.metrics.jsonl` and `otlp/simulator.logs.jsonl`, the simulator's run records, and `otlp/harness.metrics.jsonl` and `otlp/harness.logs.jsonl`, the runner's: OTLP JSON Lines with the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md#workload-run-and-scenario-names" >}});
- `trace.jsonl`, `variant.env`, `run-status.json`, `values.effective.yaml`, `values.simulator.effective.yaml`, `images.env`, the snapshots in `k8s/` and the component logs in `logs/`; a variant that could not be run gets the cluster's `k3s.log` and `containerd.log` there instead.

With `SIMCLUSTER_VALIDATE_OTLP=1`, every line of every OTLP file is also posted to an OpenTelemetry Collector (`OTELCOL_IMAGE` pinned to `OTELCOL_DIGEST` of `scripts/simcluster/versions.env`, one container in Docker for the whole scenario, with the OTLP receiver and the file exporter). Each post must answer HTTP 200 with no rejected points or records, and the Collector must write as many data points and log records as were sent; otherwise the variant fails.

## Settings

`scenario.sh` reads these besides the settings of `up.sh` and `deploy.sh` ([Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md#settings" >}})). Each variant's `variant.env` sets the spec, timing, telemetry mode and power scope, and `scenario.sh` passes a scenario's `simulator.set` to `deploy.sh` through `SIMCLUSTER_SIM_EXTRA_VALUES`.

| Variable | Default | Effect |
|---|---|---|
| `SCENARIO` | (none) | The scenario file, for `make simcluster-scenario` |
| `SCENARIO_VARIANTS` | (empty: all) | Run only these variants, space separated. The run is then not summarized, so the variants can run on separate machines and `simscenario summarize` can compare them afterwards |
| `SCENARIO_RUN_DIR` | a new `$SIMCLUSTER_DIR/runs/<UTC>-<scenario>-<sha12>[-dirty]` | The run directory; `runs/latest` is left alone when it is set |
| `SIMCLUSTER_VALIDATE_OTLP` | (unset) | `1` posts the run's OTLP files to a Collector, above |
| `SIMCLUSTER_SCENARIOS` | (unset) | `1` makes `make simcluster-smoke` also run the `smoke` scenario on its cluster after the end-to-end test, into the smoke run's `scenario-smoke/` |

## simscenario

```text
simscenario validate <file> [-repo DIR]
simscenario plan <file> -out RUNDIR [-repo DIR] [-workloadgen BIN]
simscenario run -variant-dir DIR [-kubeconfig FILE]
simscenario evaluate DIR
simscenario summarize RUNDIR
```

Exit codes: 0 success, 1 a failed assertion or comparison of severity `fail`, 2 a usage error, an invalid scenario, or a run that could not be carried out.

## CI

CI runs `nightly-mixed` and `deadlock-visible` Monday to Saturday and `weekly-compare` on Sundays; the schedule is in [`tests/simcluster/README.md`](https://github.com/joulie-k8s/Joulie/blob/main/tests/simcluster/README.md#ci).
