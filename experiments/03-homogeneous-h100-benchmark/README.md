# 03 - Homogeneous H100 Benchmark

This experiment is structurally identical to [02 - heterogeneous-benchmark](../02-heterogeneous-benchmark/README.md) but uses a **homogeneous cluster with only NVIDIA H100 NVL nodes**.

## Purpose

The experiment tests the hypothesis that Joulie's energy-aware scheduling policy is more effective on a homogeneous cluster, where every GPU node can accept any GPU job without vendor- or product-specific placement constraints. On a heterogeneous cluster, family-first placement and per-family hardware physics can cause:

- AMD jobs queuing behind a bottleneck family while NVIDIA nodes sit idle (and vice versa),
- asymmetric throttling sensitivity (e.g. MI300X vs H100 NVL react very differently to the same power cap percentage),
- policy decisions that are optimal per-family but suboptimal for total throughput.

Running the identical workload trace on a homogeneous H100 cluster isolates these effects and lets us quantify how much heterogeneity costs in throughput/energy tradeoff.

## Cluster inventory

- **33 H100 NVL nodes** (8 GPUs × 192 cores × 1536 GiB RAM each) - matches the total GPU node count of experiment 02
- **8 CPU-only nodes** (same mix as experiment 02: 2 highcore, 2 highfreq, 4 intensive)
- **Total: 41 nodes** - same as experiment 02 for a fair comparison

The cluster description lives in:

- [cluster-nodes.yaml](configs/cluster-nodes.yaml)

## How to run

The workflow is identical to experiment 02. Generate assets:

```bash
experiments/03-homogeneous-h100-benchmark/scripts/00_generate_assets.sh
```

Run the full overnight sweep:

```bash
experiments/03-homogeneous-h100-benchmark/scripts/30_run_overnight.sh \
  experiments/03-homogeneous-h100-benchmark/configs/benchmark-overnight.yaml
```

Or a quick debug run:

```bash
experiments/03-homogeneous-h100-benchmark/scripts/20_run_benchmark.sh \
  experiments/03-homogeneous-h100-benchmark/configs/benchmark-debug.yaml
```

## Benchmark configs

- [benchmark-debug.yaml](configs/benchmark-debug.yaml) - 32 jobs, 1 seed, fast iteration
- [benchmark.yaml](configs/benchmark.yaml) - 120 jobs, 2 seeds
- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml) - 2500 jobs, 3 seeds, identical workload parameters to experiment 02 overnight config for a controlled comparison

## Comparing results with experiment 02

To compare energy/throughput tradeoffs between homogeneous and heterogeneous clusters:

1. Run both experiments with the same `benchmark-overnight.yaml` workload parameters (they share the same default parameters by design).
2. Compare `results/baseline_summary.csv` from each experiment - look at `energy_total_wh` and `makespan_s` across baselines B and C.
3. The `hardware_family_tradeoff_vs_a.png` plots show per-family energy savings in experiment 02; in experiment 03 there is only one family (H100 NVL), so the plot shows a single point.

## Notes

- The `generate_heterogeneous_assets.py` script applies H100 NVL-specific physics parameters (`computeGamma: 1.5`, `memoryEpsilon: 0.15`, `memoryGamma: 0.9`, `idleWattsPerGpu: 60`) to all GPU nodes.
- The retargeting logic in `05_sweep.py` no longer pins jobs to exact node names. CPU-only jobs are constrained only to the CPU-only pool, while GPU jobs rely on the native `nvidia.com/gpu` extended resource so the Kubernetes scheduler can place them on any compatible H100 node.
- All scripts share the same asset generation path (`scripts/generate_heterogeneous_assets.py`), so the node classes and hardware catalog are regenerated from the homogeneous cluster-nodes.yaml before each benchmark run.
