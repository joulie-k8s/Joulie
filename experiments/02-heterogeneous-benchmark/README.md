# 02 - Heterogeneous Benchmark

This experiment packages the heterogeneous-cluster inventory and the asset-generation path used by the new hardware-aware control model.

It is centered on four things:

- a checked-in heterogeneous cluster description in YAML,
- generation of KWOK nodes and simulator catalog assets from that description,
- a quick end-to-end smoke validation path that exercises the heterogeneous GPU example on the generated assets.
- a full sweep/collect/plot benchmark harness comparable to `experiments/01-kwok-benchmark`, but targeting the heterogeneous inventory.

The workflow:

- agent publishes `NodeHardware`,
- operator resolves discovered hardware against the shared inventory,
- operator plans with CPU/GPU density awareness,
- simulator uses the same CPU/GPU inventory for node composition and fallback modeling.

## Files

- `configs/cluster-nodes.yaml`
  - checked-in heterogeneous cluster description
- `configs/cluster.yaml`
  - experiment-level config reference
- `scripts/00_generate_assets.sh`
  - generates manifests and simulator catalog artifacts from the YAML cluster description
- `configs/benchmark.yaml`
  - heterogeneous benchmark run configuration
- `scripts/10_setup_cluster.sh`
  - cluster bootstrap for the heterogeneous benchmark
- `scripts/03_install_components.sh`
  - installs simulator and Joulie components for a given baseline
- `scripts/04_run_one.py`
  - executes one benchmark run and stores full artifacts
- `scripts/05_sweep.py`
  - runs the configured multi-seed/multi-baseline sweep
- `scripts/06_collect.py`
  - aggregates run summaries into `results/summary.csv`
- `scripts/07_plot.py`
  - produces plots under `results/plots/`
- `scripts/20_run_benchmark.sh`
  - convenience wrapper for sweep + collect + plot
- `scripts/10_run_smoke.sh`
  - refreshes assets and runs a heterogeneous smoke validation using the GPU simulator example

## Cluster inventory source

The experiment no longer depends on the spreadsheet at runtime.

The source of truth for this experiment is:

- [cluster-nodes.yaml](configs/cluster-nodes.yaml)

That file captures the heterogeneous node mix in a repo-native format and is the input consumed by the asset generator.

## Generate assets

Run:

```bash
experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh
```

By default, this reads:

- `experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml`

and refreshes:

- `examples/07 - simulator-gpu-powercaps/manifests/00-kwok-nodes.yaml`
- `examples/07 - simulator-gpu-powercaps/manifests/10-node-classes.yaml`
- `simulator/catalog/hardware.generated.yaml`

You can also pass a different YAML/CSV/XLSX inventory file as the first argument.

## Python setup

The sweep/collect/plot scripts require:

- `PyYAML`
- `pandas`
- `matplotlib`

Install them with:

```bash
python3 -m venv experiments/02-heterogeneous-benchmark/.venv
source experiments/02-heterogeneous-benchmark/.venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -r experiments/02-heterogeneous-benchmark/requirements.txt
```

## Quick smoke validation

For a runnable, debug-friendly heterogeneous validation path, use:

```bash
experiments/02-heterogeneous-benchmark/scripts/10_run_smoke.sh
```

This does two things:

1. regenerates assets from the checked-in heterogeneous cluster YAML
2. runs the heterogeneous GPU simulator example end to end using those generated assets

The smoke run is intentionally meant as a short validation path for:

- inventory matching,
- `NodeHardware` publication,
- operator planning against heterogeneous hardware,
- simulator GPU/CPU mixed-node behavior,
- GPU-cap application on supported fake nodes.

Artifacts are written under `tmp/heterogeneous-smoke-*` unless `ARTIFACT_DIR` is set.

Useful overrides:

```bash
CLUSTER_NAME=joulie-heterogeneous \
ARTIFACT_DIR="$(pwd)/tmp/heterogeneous-smoke" \
experiments/02-heterogeneous-benchmark/scripts/10_run_smoke.sh
```

## Full benchmark workflow

1. Create the cluster and apply the heterogeneous node inventory:

```bash
experiments/02-heterogeneous-benchmark/scripts/10_setup_cluster.sh
```

2. Run the heterogeneous benchmark sweep, aggregate results, and generate plots:

```bash
experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh
```

The benchmark config lives in:

- [benchmark.yaml](configs/benchmark.yaml)

It controls:

- baselines and seeds,
- workload mix, including GPU-job ratio and GPU work units,
- policy parameters,
- simulator speed/image settings,
- kind config overrides.

Artifacts are written under:

- `experiments/02-heterogeneous-benchmark/results/`

including:

- per-run directories with traces, logs, `nodepowerprofiles.yaml`, `nodehardwares.yaml`, and simulator debug snapshots,
- per-run reproducibility metadata (`metadata.json`, `benchmark_config.yaml`, `kubectl_version.json`, node snapshots),
- aggregated `results/summary.csv`,
- aggregated `results/baseline_summary.csv` with mean/std/95% CI-style summaries across repeated seeds,
- plots under `results/plots/`.

## Current scope

Today this experiment provides:

- a checked-in heterogeneous inventory,
- asset generation from that inventory,
- a smoke validation path,
- a sweep/collect/plot benchmark harness.

It also records a basic reproducibility bundle for each run:

- git commit and dirty status,
- benchmark config snapshot + SHA256,
- trace SHA256 + workload mix summary,
- cluster node snapshot,
- `kubectl` version,
- simulator/operator/agent logs.

What it still does not provide yet is a polished report layer equivalent to:

- `experiments/01-kwok-benchmark/REPORT.md`

The core benchmark machinery now exists; the next refinement is a more curated experiment report and interpretation layer for the heterogeneous results.

## Notes

- The first heterogeneous policy version reasons on CPU and GPU density only.
- Unknown hardware uses per-device fallback.
- `NodeHardware` is not hand-authored in this experiment; it is published by the agent.
- This experiment is intended to be the benchmark-facing consumer of the shared hardware inventory and physical model.

## Known caveats

- Simulator power telemetry intentionally distinguishes averaged vs instantaneous power. When comparing results to real GPU runs, remember that NVML power telemetry on many modern NVIDIA GPUs is itself averaged over a 1-second window.
- Real CPU package power is often reconstructed from energy-counter deltas, so sampling cadence and averaging windows matter there as well.
- This harness is a reproducible simulation/benchmark path, not yet an external-meter calibration harness. External-meter validation remains the next step for bare-metal model calibration.
