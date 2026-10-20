# Simulated cluster tests

The end-to-end test of the [simulated cluster](../../website/content/en/docs/simulator/simulated-cluster.md): the real Joulie components on emulated bare-metal nodes, in one k3s container in Docker. The user documentation (requirements, targets, settings, telemetry modes, run results) is on that page; this file is for contributors: what the test checks, the defects it tracks, and how CI runs it.

## What lives here

| Path | What it is |
|------|------------|
| `e2e_test.go` | `TestSimclusterE2E`, the end-to-end test, behind the `simcluster` build tag |
| `*_test.go` (others) | its helpers, and the harness's own tests (`TestE2E*`, and the specs, scripts and manifests checks), which need no cluster |
| `known_defects_test.go` | the known-defect registry (below) |
| `specs/` | cluster specs: `smoke.yaml` (8 Nodes), `medium.yaml` (50), `profiles.yaml` (one Node of every builtin profile), `policy.yaml` and `nightly.yaml` for the scenarios, `governor.yaml` for experiment 05. Every GPU pool sets its `toolDelivery` in `spec.harness`: a pool left out follows the Joulie install, which delivers no GPU tools by default |
| `smoke-values.yaml` | the Joulie values of the smoke run |
| `scenarios/` | the [workload scenario](../../website/content/en/docs/simulator/scenarios.md) library |

## Running it

```bash
make simcluster-smoke SIMCLUSTER_KEEP=1      # creates the cluster, deploys, runs TestSimclusterE2E
go vet -tags simcluster ./tests/simcluster/
go test -tags simcluster -count=1 -run '^TestE2E' ./tests/simcluster/   # the harness's own tests
```

`smoke.sh` compiles the test (`go test -c -tags simcluster`) while `up.sh` creates the cluster, then runs the binary's `TestSimclusterE2E` from this directory with `SIMCLUSTER_DIR` and `SIMCLUSTER_RUN_DIR` set; without `SIMCLUSTER_DIR` it skips. It changes the cluster as it goes (the caps go to 50 %, the extender is reloaded), so it expects a fresh deploy. The caps go to 50 % only when `E09.50pct` or `E10.50pct` runs, so a `-run` filter that leaves both out keeps a cluster at its deployed caps.

## Checks

Each check is one entry under `assertions` in the run's `summary.json`.

| ID | Checks |
|----|--------|
| E01 | every spec Node is Ready, with the labels, taints, capacity and architecture `clusterspec` renders |
| E02 | no pod without `joulie.io/workload-class` is bound to a fake Node |
| E03 | every container runs the image just built and imported |
| E04 | one emulated agent exists for exactly the Nodes the agent DaemonSet would reach; the agent of a GPU Node mounts the driver stub exactly when its pool's tool delivery (the spec's, else the install's) is `mounted` |
| E05 | each NodeHardware equals the in-process golden of its profile (`cmd/agent/testdata/hwemu`) |
| E06 | Prometheus holds series for every Node with a measured CPU package, and `target_info` of the jobs that carry node power names each one |
| E07 | every check query of the mode equals the emulated truth; E07c, in mode `kepler-legacy`: each Node has two Kepler 0.7 platform series, which sum to its host power |
| E08 | each NodeTwin's measured power is the truth, or `prometheus-no-data` where nothing measures |
| E09, E10 | lowering the CPU and GPU caps from 80 % to 50 % lowers the enforced limits (RAPL, NVIDIA GPUs whole or shared, and AMD GPUs; a GPU cap never below the published minimum) and the NodeTwins' measured power (RAPL, DVFS, NVIDIA) |
| E11 | the control status of each hardware family |
| E12 | D15, below |
| E13 | the extender filters and scores a real pod, on the host network in the `hostnet` extender variant and off it, reached through its Service, in the `service` one |
| E14 | a forced extender reload rolls out; an agent reload with an unchanged binary does nothing |
| E15 | `status.sh` exits 0 |
| E16 | the facility inputs reach the NodeTwin gauges and the cooling stress; in mode `otel-bridge`, which leaves the facility metrics off as the chart has them, no facility gauge is served and the cooling stress is computed with the ambient unknown |
| MD01 to MD04 | medium spec only: MIG-mixed and renamed time-slicing GPU Nodes are planned and capped as GPU Nodes (the physical GPU count, a GPU cap and no CPU percent cap, every GPU limit at the cap); no NFD labels caps through DVFS; no driver tools leaves the NVIDIA GPUs alone; an unmanaged pool has series and nothing else |
| GS01 | governor control on only: each agent's `NodeHardware` lists the governor presets its profile accepts, with the result each gives |
| GS02 | governor control on only: each `NodeTwin` asks for the preset its profile and node state (performance or eco) call for, and the agent's result for it is the one its profile predicts |
| GS03 | governor control on only: the emulated files hold the native words of every applied or equivalent result |
| GS04 | governor control on only, and last: after one `helm upgrade` to `GOVERNOR_CONTROL=off`, every governor result is restored or gone, and every file (governor, EPP, boost, AMD level and overdrive table, NVIDIA locks) is back at the baseline `NodeHardware` recorded |

GS01 to GS04 skip unless the deployed controller manager runs with `GOVERNOR_CONTROL` not `off`. `experiments/05-governor-exploration/scripts/run_tier_b.sh` deploys `specs/governor.yaml`, one pool per CPU frequency driver and per GPU family, with governor control on, and runs them. GS04 changes the release, so the cluster needs a fresh deploy afterwards.

## Known defects

A defect the run reproduces today, and that waits for a fix or a decision, is registered in `known_defects_test.go`. Its check asserts the defect's observation, so the run stays green while the defect lasts, and `summary.json` lists it under `knownDefects`. The day it no longer reproduces, the run fails with "remove it from tests/simcluster/known_defects_test.go": the fix has landed, and the check must assert the fixed behaviour instead.

| ID | Check | Observation | Changes with |
|----|-------|-------------|--------------|
| D15 | E12 | A managed Node the agent DaemonSet does not reach (here an untolerated taint) gets a NodeTwin with a cap, but no NodeHardware and no `controlStatus`, and its hardware keeps the uncapped limit: the cap is planned, never enforced. | tolerating every taint, or the controller manager planning only Nodes that run an agent |

The defects that reach users are also listed on the user pages: [GPU known limitations](../../website/content/en/docs/hardware/gpus.md#known-limitations) and the `agent.tolerations` row of the [configuration reference](../../website/content/en/docs/getting-started/05-configuration-reference.md#agent-chart-values).

## Scenario library checks

`simulator/pkg/scenario/scenarios_test.go` checks the scenario library without a cluster: every scenario validates and plans under the load limit, every job fits some Node of its spec (resources, node selector and taints), the nightly load stays between 0.3 and 0.8 of the cluster and, for nightly-mixed's performance jobs, of the performance Nodes, the nightly workload sized by the cluster alone is refused, the deadlock gang fits its Node one pod at a time but never whole, and the Nodes `perf-isolation` and `cap-cost` expect in eco are the ones `policy.BuildStaticPlan` puts there.

## CI

`.github/workflows/simcluster.yml` has two jobs; the Dagger integration job (`integration-tests.yml`, see [`tests/integration`](../integration/README.md)) is separate.

- **`smoke`**, on pull requests and pushes to `main` that touch `cmd/`, `pkg/`, `api/`, `charts/`, `config/`, `simulator/`, `ci/`, `scripts/simcluster/`, `tests/simcluster/`, `values/simcluster*.yaml`, the `Makefile`, `go.mod`, `go.sum` or the workflow, and on demand. It vets the test, runs the harness's own tests, then `make simcluster-smoke`, with a 30 minute timeout around the test's 20 (ten rate windows, at least 20 minutes: 50 in `production` timing). The air-gap bundle is cached by the hash of `versions.env`.
- **`nightly`**, daily at 03:23 UTC and on demand, with a 70 minute timeout: the medium spec in `production` timing; the smoke spec once for each of `otel-unpromoted`, `otel-labels-unpromoted`, `otel-utf8`, `otel-collector`, `otel-bridge`, `kepler`, `kepler-legacy`, `otel` with host scope, and `otel` with the extender reached through its Service (`SIMCLUSTER_EXTENDER=service`); and the profiles spec in mode `otel-bridge` with the extender Service. `TestSC21ModesAgree` fails when a telemetry mode has no nightly entry. One failing variant does not cancel the others.

`.github/workflows/simcluster-nightly.yml` runs the scenario library on a schedule, with `permissions: contents: read`, and never cancels a run in progress:

- Monday to Saturday at 03:17 UTC: `nightly-mixed` (130 minute timeout) and `deadlock-visible` with the OTLP check, as separate jobs.
- Sunday at 03:17 UTC: `weekly-compare`, one variant per runner (150 minute timeout) into a run directory named once for all of them, then a job that plans the same directory, collects the variants and runs `simscenario summarize`.
- On demand (`workflow_dispatch`): `nightly` or `weekly`.

Every job uploads its `runs/` directory, also when it fails, with a 10 minute timeout on the upload; a scenario job deletes its cluster first (`down.sh` keeps `runs/`), since `scenario.sh` keeps it. The scenario runs are kept 14 days.
