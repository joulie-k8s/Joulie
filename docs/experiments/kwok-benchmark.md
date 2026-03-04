# KWOK Benchmark Experiment

This document describes the first benchmark harness implementation under:

- `experiments/01-kwok-benchmark/`

## Motivation

The benchmark focuses on repeatable comparisons of scheduler+control behavior across baselines:

- baseline A: no Joulie control path,
- baseline B: static partition style,
- baseline C: queue-aware style.

The setup keeps real scheduling semantics while using fake KWOK nodes and simulator-driven telemetry/control effects.

## Assumptions

- Kubernetes scheduler and API are real.
- Fake nodes are tainted and selected by workload pods.
- Simulator injects and advances batch work from trace input.
- Experiment scripts orchestrate install/run/collect/plot.

## What is measured now

Current harness outputs:

- per-run wall runtime proxy (`wall_seconds`),
- time-scale aware simulated runtime (`sim_seconds`),
- per-run workload size (`jobs_total`),
- throughput metrics (`jobs/wall-sec`, `jobs/sim-sec`, `jobs/sim-hour`),
- estimated simulated-time energy from simulator telemetry events (`energy_sim_joules_est`, `energy_sim_kwh_est`),
- estimated average cluster power (`avg_cluster_power_w_est`),
- run metadata (baseline, seed, commit, trace hash),
- cluster snapshots/logs for debugging.

## Scripts

Main entrypoints:

- `scripts/00_prereqs_check.sh`
- `scripts/01_create_cluster_kwokctl.sh`
- `scripts/02_apply_nodes.sh`
- `scripts/03_install_components.sh`
- `scripts/04_run_one.py`
- `scripts/05_sweep.py`
- `scripts/06_collect.py`
- `scripts/07_plot.py`
- `scripts/99_cleanup.sh`

## Outputs

- `experiments/01-kwok-benchmark/results/<run_id>/...`
- `experiments/01-kwok-benchmark/results/summary.csv`
- `experiments/01-kwok-benchmark/results/plots/*.png`

Key plots produced now:

- `runtime_distribution.png` (box+points by baseline, replacing index-based scatter),
- `throughput_vs_energy.png` (tradeoff + Pareto frontier),
- `energy_vs_makespan.png` (tradeoff with baseline means),
- `baseline_means.png` (mean energy / throughput / makespan by baseline).

## Notes on energy interpretation

- Energy is computed by integrating per-node telemetry event `packagePowerWatts` over time.
- Integration is first done in wall time from event timestamps, then scaled by `timeScale` to approximate simulated-time energy.
- If telemetry debug events are missing or sparse, energy fields can be empty or less reliable for that run.
