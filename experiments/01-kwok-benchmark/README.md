# KWOK Benchmark Experiment

This experiment harness measures throughput/latency vs energy-control behavior in a mixed real+fake setup:

- real scheduler/API server,
- fake KWOK worker nodes for workload pods,
- simulator telemetry/control loop,
- optional Joulie operator+agent(pool).

Workload scheduling in benchmark pods uses affinity on `joulie.io/power-profile`:

- `performance`: requires `joulie.io/power-profile=performance`
- `eco`: requires `joulie.io/power-profile=eco`
- no power-profile affinity: implicit general placement

Current trace behavior:

- per-seed canonical trace is generated once and reused across baselines,
- baseline `A` is derived from that canonical trace by stripping only power-profile affinity,
- baselines `B`/`C` use the canonical trace as generated.

## Baselines

- `A`: simulator only (no operator/agent), using affinity-free workload pods (Joulie-free baseline).
- `B`: simulator + Joulie with static partition-oriented config.
- `C`: simulator + Joulie with queue-aware policy-oriented config.

## Config file

`configs/benchmark.yaml` contains:

- run controls: baselines, seeds, jobs, inter-arrival, timeout, settle/cleanup,
- run scaling: `run.time_scale` used in run metadata and derived simulated-time metrics,
- workload mix: `perf_ratio`, `eco_ratio` (remaining fraction is no-affinity general), plus `cpu_units_min/max`,
- baseline A comparability: `baseline_a_strip_affinity` removes only affinity from a canonical seed trace while keeping submit/work/request values identical,
- simulator speed control: `simulator.base_speed_per_core` (higher value = faster job completion),
- policy controls: caps (`performance_watts`, `eco_watts`), control-loop intervals, static and queue-aware parameters,
- image/tag/registry overrides,
- optional simulator manifest path.

Note: in `configs/benchmark.yaml`, `images.sim_tag: ""` means "do not override image tag, use the simulator image pinned in the manifest".
For the `20_run_benchmark.sh` flow, `configs/benchmark.yaml` is the single source of truth for benchmark/image/policy configuration.

## Artifacts per run

Each run writes to `results/<run_id>/`:

- `run_summary.json`
- `metadata.json`
- `trace.jsonl`
- `pods.json`
- `nodepowerprofiles.yaml`
- operator/agent/simulator logs
- simulator debug snapshots
  - `sim_debug_energy.json` (simulator-integrated energy over all managed nodes)

## Python setup (venv)

Create and activate a virtual environment, then install dependencies:

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
```

You can later reactivate with:

```bash
source .venv/bin/activate
```

## Quick run

Benchmark run configuration is centralized in:

- `configs/benchmark.yaml`

Use two entry points.

### 1) Environment + cluster setup

From within this experiment directory:

```bash
source .venv/bin/activate
./scripts/10_setup_cluster.sh
```

Equivalent expanded commands run by `10_setup_cluster.sh`:

```bash
./scripts/00_prereqs_check.sh
./scripts/01_create_cluster_kwokctl.sh
./scripts/02_apply_nodes.sh
```

### 2) Benchmark sweep + collect + plot (YAML-configured)

From the repo root:

```bash
source experiments/01-kwok-benchmark/.venv/bin/activate
experiments/01-kwok-benchmark/scripts/20_run_benchmark.sh
```

Optionally pass a custom config file path:

```bash
experiments/01-kwok-benchmark/scripts/20_run_benchmark.sh \
  experiments/01-kwok-benchmark/configs/benchmark.yaml
```

`05_sweep.py` still accepts CLI overrides, but the default workflow is to edit `configs/benchmark.yaml` only.

While the Joulie experiment is being run, you can watch the power profiles applied to the nodes with:

```bash
watch -n 5 'kubectl get nodepowerprofiles -o wide; echo; kubectl get nodes -L type,joulie.io/managed,joulie.io/power-profile' 
```

However, in the no-Joulie baseline no power profiles will be applied to the nodes!

You can manually check that the right containers were used by running:

```bash
kubectl get pods -A -o=jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{range .spec.containers[*]}{.name}{"="}{.image}{" "}{end}{"\n"}{end}' \
| egrep 'joulie-(agent|operator)|joulie-telemetry-sim'
```

Policy/image override for manual component installs (when running `03_install_components.sh` directly):

```bash
export POLICY_TYPE=static_partition
export STATIC_HP_FRAC=0.50
export QUEUE_HP_BASE_FRAC=0.60
export QUEUE_HP_MIN=1
export QUEUE_HP_MAX=5
export QUEUE_PERF_PER_HP_NODE=10
export JOULIE_TAG=dev0.0.13
export SIM_TAG=dev0.0.13
```

`05_sweep.py` manages policy per baseline automatically:

- `A` -> simulator-only / no policy (operator+agent not installed)
- `B` -> `static_partition`
- `C` -> `queue_aware_v1`

`rule_swap_v1` is kept only for debugging and should not be used as benchmark default.

Optional registry/image overrides for manual install script:

```bash
export JOULIE_REGISTRY=registry.cern.ch/mbunino/joulie
export SIM_REGISTRY=registry.cern.ch/mbunino/joulie
export SIM_IMAGE=joulie-simulator
```

`01_create_cluster_kwokctl.sh` uses `manifests/kind-cluster.yaml` by default.
Override with:

```bash
KIND_CLUSTER_CONFIG=/path/to/kind-config.yaml ./scripts/01_create_cluster_kwokctl.sh
```

## Results

- Run dirs: `results/<run_id>/...`
- Aggregated CSV: `results/summary.csv`
- Analysis report: [`REPORT.md`](./REPORT.md)
- Report snapshot assets (commit-friendly): [`assets/`](./assets/)
- Plots: `results/plots/*.png`
  - `runtime_distribution.png`
  - `throughput_vs_energy.png`
  - `energy_vs_makespan.png`
  - `baseline_means.png`
- Seed-shared traces reused across baselines: `results/traces/*.jsonl`

## Notes

- This is the first benchmark harness implementation.
- Energy in `summary.csv` now prefers simulator-integrated energy (`sim_debug_energy.json`) and falls back to debug telemetry events if unavailable.
- Throughput-vs-energy and energy-vs-makespan tradeoff plots are generated from `summary.csv`.
- If a run has missing `sim_debug_events.json` telemetry entries, energy fields can be empty for that run.
