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

1. Run the heterogeneous benchmark sweep, aggregate results, and generate plots:

```bash
experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh
```

The benchmark config lives in:

- [benchmark.yaml](configs/benchmark.yaml)
- [benchmark-debug.yaml](configs/benchmark-debug.yaml)
- [benchmark-showcase.yaml](configs/benchmark-showcase.yaml)
- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml)

It controls:

- baselines and seeds,
- workload mix, including GPU-workload ratio, placement intent ratios, and burst shaping,
- policy parameters,
- simulator speed/image settings,
- kind config overrides.

The repository also ships a lightweight debug profile:

- [benchmark-debug.yaml](configs/benchmark-debug.yaml)

That profile is meant for fast iteration on the existing `kind` cluster and keeps each baseline close to about one minute on the current simulator settings.

There is also a "showcase" profile:

- [benchmark-showcase.yaml](configs/benchmark-showcase.yaml)

That profile is intentionally shaped to make Joulie's strengths easier to study:

- mostly memory-heavy CPU preprocessing and short/medium GPU jobs,
- very small performance-sensitive share,
- no exclusive eco workload generation in the benchmark trace,
- optional filtering of workload families that are currently too tail-heavy for a clean benchmark loop.

There is also an overnight profile:

- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml)

That profile is meant for longer unattended sweeps:

- 300 logical workloads per seed,
- 3 seeds across all three baselines,
- the same Joulie-friendly workload shaping as the showcase profile,
- a larger timeout / cleanup budget for longer tails.

To run the full flow end to end with a single command:

```bash
experiments/02-heterogeneous-benchmark/scripts/30_run_overnight.sh
```

By default the wrapper:

- regenerates heterogeneous assets,
- reuses the existing `kind` cluster if present,
- reapplies the heterogeneous KWOK node inventory,
- runs the sweep/collect/plot pipeline,
- creates a single numbered benchmark run root under `experiments/02-heterogeneous-benchmark/runs/`,
- stores `results/`, simulator debug persistence, copied config files, and `run.log` under that same root,
- logs UTC timestamps plus elapsed seconds for each major stage into `run.log`.

The default run root format is:

- `experiments/02-heterogeneous-benchmark/runs/0007_20260314T221530Z_u<uuid>/`

The leading number makes runs easy to sort chronologically, and the UUID guarantees uniqueness. The newest run is also exposed through:

- `experiments/02-heterogeneous-benchmark/runs/latest`

Useful overrides:

```bash
REUSE_EXISTING_CLUSTER=true \
CLEAN_RESULTS=true \
ARTIFACT_DIR=experiments/02-heterogeneous-benchmark/runs/manual-overnight \
experiments/02-heterogeneous-benchmark/scripts/30_run_overnight.sh \
  experiments/02-heterogeneous-benchmark/configs/benchmark-overnight.yaml
```

Artifacts for one benchmark execution are written under its benchmark run root, for example:

- `experiments/02-heterogeneous-benchmark/runs/0007_20260314T221530Z_u<uuid>/`

including:

- per-run directories named with timestamp + UUID + baseline/seed, with traces, logs, `nodepowerprofiles.yaml`, `nodehardwares.yaml`, and simulator debug snapshots,
- a shared `results/` subtree containing per-baseline/per-seed outputs plus aggregated CSVs and plots,
- a shared `simulator-debug/` subtree used as the persistence root for simulator snapshots during that benchmark execution,
- `run.log`, `benchmark-config.yaml`, and `cluster-nodes.yaml` at the benchmark run root,
- per-run reproducibility metadata (`metadata.json`, `benchmark_config.yaml`, `kubectl_version.json`, node snapshots),
- aggregated `results/summary.csv`,
- aggregated `results/baseline_summary.csv` with mean/std/95% CI-style summaries across repeated seeds,
- aggregated `results/job_details.csv`,
- aggregated `results/workload_type_relative_to_a.csv`,
- aggregated `results/workload_type_tradeoff_vs_a.csv`,
- aggregated `results/hardware_energy.csv`,
- aggregated `results/hardware_family_relative_to_a.csv`,
- plots under `results/plots/`.

Each run also gets its own simulator debug persistence directory, so repeated simulator restarts do not overwrite the previous run's persisted debug state.

The new attribution outputs make it possible to answer questions such as:

- which workload types slowed down the most under throttling,
- which workload types were mostly unaffected while still running on energy-saving hardware,
- which hardware families delivered the best energy-savings/slowdown tradeoff,
- which hardware families paid slowdown without enough savings to justify it.

If you want a clean debug run without stale aborted runs mixed into the summaries, use:

```bash
CLEAN_RESULTS=true \
experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh \
  experiments/02-heterogeneous-benchmark/configs/benchmark-debug.yaml
```

`CLEAN_RESULTS=true` removes old per-run result directories, plots, traces, and aggregate CSVs before starting the new sweep.

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

## Trace format note

The benchmark now uses the newer workload generator trace format.

Generated traces can contain both:

- `type=workload`
  - logical workload metadata records
- `type=job`
  - pod-expanded runnable records consumed by the simulator

For this experiment:

- runtime behavior is driven by `type=job` records,
- benchmark trace statistics count runnable jobs rather than raw JSONL lines,
- baseline A still derives from the same canonical trace by stripping only power-profile affinity from `type=job` records.

## Placement targeting in experiment 02

The generator itself produces logical AI workload traces and pod-expanded runnable jobs, but it does not know about the concrete KWOK nodes in the benchmark cluster.

Experiment 02 therefore adds a second step during the sweep:

1. generate a canonical trace with `workloadgen`,
2. retarget `type=job` records onto the currently available KWOK nodes,
3. derive baseline-specific traces from that retargeted canonical trace.

The current retargeting logic is intentionally **family-first**:

- CPU-only jobs are first spread across CPU families,
- GPU jobs are first spread across GPU product families,
- only then are additional jobs reused across more node instances in those families.

This keeps the debug benchmark lightweight while still making sure the heterogeneous hardware families are actually exercised.

In other words:

- all fake nodes from `cluster-nodes.yaml` are created,
- the debug profile aims to cover all major hardware families,
- but it does not guarantee that every individual fake node instance receives a job in the lightweight run.

Between baselines, the sweep also resets control state to avoid contamination:

- deletes `NodePowerProfile` and `TelemetryProfile` objects,
- removes `joulie.io/power-profile` from managed nodes,
- reinitializes `joulie.io/draining=false` on managed nodes as an explicit clean baseline-state marker,
- baseline `A` runs without Joulie deployed at all.

Eco-only workload affinity in the benchmark and docs now uses:

- `joulie.io/power-profile In ["eco"]`
- `joulie.io/draining NotIn ["true"]`

instead of requiring `draining=false`, because `NotIn ["true"]` is safer when the label is temporarily absent.

If you need one-pod-per-node-instance coverage, use a heavier benchmark config than the debug profile.

The sweep can also optionally restrict the canonical trace to a subset of workload families via:

- `workload.allowed_workload_types`

This is useful for curated "showcase" studies where we want the benchmark to emphasize the parts of the workload space that best demonstrate the energy/throughput tradeoff, without being dominated by simulator-pathological long-tail gang workloads.

## Notes

- The first heterogeneous policy version reasons on CPU and GPU density only.
- Unknown hardware uses per-device fallback.
- `NodeHardware` is not hand-authored in this experiment; it is published by the agent.
- This experiment is intended to be the benchmark-facing consumer of the shared hardware inventory and physical model.
- The benchmark scripts can reuse an already existing `kind` cluster via the cluster-setup scripts; recreating the cluster is not required for the debug profile.

## Plot guide

The plot set now includes both aggregate and attribution views.

Aggregate views:

- `throughput_vs_energy.png`
- `energy_vs_makespan.png`
- `relative_tradeoff_vs_a.png`
- `relative_tradeoff_bars_vs_a.png`

Attribution views:

- `workload_type_tradeoff_vs_a.png`
  - slowdown vs energy-savings exposure by workload type
- `workload_type_rankings_baseline_B.png`
- `workload_type_rankings_baseline_C.png`
  - which workload types were helped most and hurt most
- `hardware_family_tradeoff_vs_a.png`
  - energy savings vs slowdown by hardware family
- `hardware_family_rankings_baseline_B.png`
- `hardware_family_rankings_baseline_C.png`
  - which hardware families delivered the best and worst throttling tradeoffs

For workload types, "energy-savings exposure" is not a direct per-job energy meter. It is derived by combining:

- the hardware-family energy savings measured from simulator node energy,
- the hardware families actually used by each workload type.

That keeps the analysis interpretable without pretending we have exact per-job energy accounting.

## Known caveats

- Simulator power telemetry intentionally distinguishes averaged vs instantaneous power. When comparing results to real GPU runs, remember that NVML power telemetry on many modern NVIDIA GPUs is itself averaged over a 1-second window.
- Real CPU package power is often reconstructed from energy-counter deltas, so sampling cadence and averaging windows matter there as well.
- This harness is a reproducible simulation/benchmark path, not yet an external-meter calibration harness. External-meter validation remains the next step for bare-metal model calibration.
