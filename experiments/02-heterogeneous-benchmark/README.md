# 02 - Heterogeneous Benchmark

Baselines A (no management), B (static partition) and C (queue-aware) on a cluster of five GPU families, NVIDIA and AMD, and CPU-only nodes. The results, measured at regime, are in [REPORT.md](REPORT.md); how the runs are set up, sized and gated is in [experiments/README.md](../README.md). [03](../03-homogeneous-h100-benchmark/README.md) runs the same workload and policies on H100 NVL nodes only.

## Cluster inventory

- 12 H100 NVL nodes: 8 NVIDIA H100 NVL, AMD EPYC 9654, 192 cores, 1536 GiB
- 6 H100 SXM nodes: 4 NVIDIA H100 80GB HBM3, Intel Xeon Gold 6530, 64 cores, 1024 GiB
- 7 L40S nodes: 4 NVIDIA L40S, AMD EPYC 9534, 128 cores, 1536 GiB
- 2 MI300X nodes: 8 AMD Instinct MI300X, AMD EPYC 9534, 128 cores, 1536 GiB
- 6 W7900 nodes: 4 AMD Radeon PRO W7900, AMD EPYC 9534, 128 cores, 770 GiB
- 8 CPU-only nodes: 2 highcore, 2 highfreq, 4 intensive, as in [01](../01-cpu-only-benchmark/README.md)
- 41 nodes in all ([cluster-nodes.yaml](configs/cluster-nodes.yaml))

## How to run

The report's configuration is [benchmark-40n.yaml](configs/benchmark-40n.yaml). Build and load the images as [experiments/README.md](../README.md) ("Images") shows, then:

```bash
experiments/02-heterogeneous-benchmark/scripts/10_setup_cluster.sh \
  experiments/02-heterogeneous-benchmark/configs/benchmark-40n.yaml
SIM_IMAGE_PULL_POLICY=IfNotPresent experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh \
  experiments/02-heterogeneous-benchmark/configs/benchmark-40n.yaml
```

A quick debug run:

```bash
experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh \
  experiments/02-heterogeneous-benchmark/configs/benchmark-debug.yaml
```

## Benchmark configs

- [benchmark-40n.yaml](configs/benchmark-40n.yaml) - the report's: 2 seeds and a repeat, at regime
- [benchmark-debug.yaml](configs/benchmark-debug.yaml) - 600 jobs, 1 seed, fast iteration
- [benchmark.yaml](configs/benchmark.yaml) - 200 jobs, 3 seeds
- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml) - 2500 jobs, 3 seeds
- [benchmark-showcase.yaml](configs/benchmark-showcase.yaml) - 180 jobs, 1 seed, a subset of workload types (`workload.allowed_workload_types`)
- [benchmark-5k.yaml](configs/benchmark-5k.yaml) and its `-debug` and `-prod` variants - 5,000 nodes ([cluster-nodes-5k.yaml](configs/cluster-nodes-5k.yaml)) in standalone mode, run by `scripts/standalone_sweep.py`

## Notes

- The sweep pins no job to a node: a CPU-only job is restricted to the `joulie.io/hw.kind=cpu-only` nodes, and a GPU job requests `nvidia.com/gpu` or `amd.com/gpu` from the vendor pool with the lowest share of its GPUs requested (experiments/README.md, "Sizing the load"); kube-scheduler picks the node.
- `PerformanceSet` splits B's and C's performance nodes over the hardware families in proportion to their nodes, with at least one each.
- Eco GPUs are capped at the same percentage of their maximum in every family, though the families lose throughput under a cap at very different rates (below).

The GPU model of each family (`scripts/generate_heterogeneous_assets.py`), with compute throughput `(cap / natural power)^computeGamma` and memory bandwidth `1 - memoryEpsilon x (1 - cap / natural power)^memoryGamma` under a cap:

| GPU | Idle W per GPU | computeGamma | memoryEpsilon | memoryGamma |
|---|---:|---:|---:|---:|
| NVIDIA H100 NVL | 60 | 1.50 | 0.15 | 0.90 |
| NVIDIA H100 80GB HBM3 | 100 | 1.40 | 0.15 | 0.90 |
| NVIDIA L40S | 35 | 1.20 | 0.25 | 1.10 |
| AMD Instinct MI300X | 120 | 0.85 | 0.10 | 0.85 |
| AMD Radeon PRO W7900 | 25 | 1.10 | 0.30 | 1.20 |

These values are assumptions: no product brief or paper gives them ([wiki: GPU power model](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model#per-family-parameters)).
