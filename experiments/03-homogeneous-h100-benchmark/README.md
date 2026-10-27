# 03 - Homogeneous H100 Benchmark

Baselines A (no management), B (static partition) and C (queue-aware) on a cluster whose GPU nodes are all NVIDIA H100 NVL, so every GPU node can take any GPU job. The results, measured at regime, are in [REPORT.md](REPORT.md); how the runs are set up, sized and gated is in [experiments/README.md](../README.md).

## Cluster inventory

- 33 H100 NVL nodes: 8 GPUs, 192 cores and 1536 GiB each
- 8 CPU-only nodes: 2 highcore, 2 highfreq, 4 intensive
- 41 nodes in all ([cluster-nodes.yaml](configs/cluster-nodes.yaml))

## How to run

The report's configuration is [benchmark-40n.yaml](configs/benchmark-40n.yaml). Build and load the images as [experiments/README.md](../README.md) ("Images") shows, then:

```bash
experiments/03-homogeneous-h100-benchmark/scripts/10_setup_cluster.sh \
  experiments/03-homogeneous-h100-benchmark/configs/benchmark-40n.yaml
SIM_IMAGE_PULL_POLICY=IfNotPresent experiments/03-homogeneous-h100-benchmark/scripts/20_run_benchmark.sh \
  experiments/03-homogeneous-h100-benchmark/configs/benchmark-40n.yaml
```

A quick debug run:

```bash
experiments/03-homogeneous-h100-benchmark/scripts/20_run_benchmark.sh \
  experiments/03-homogeneous-h100-benchmark/configs/benchmark-debug.yaml
```

## Benchmark configs

- [benchmark-40n.yaml](configs/benchmark-40n.yaml) - the report's: 2 seeds and a repeat, at regime
- [benchmark-debug.yaml](configs/benchmark-debug.yaml) - 600 jobs, 1 seed, fast iteration
- [benchmark.yaml](configs/benchmark.yaml) - 500 jobs, 3 seeds
- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml) - 2500 jobs, 3 seeds

## Notes

- kube-scheduler reaches the extender at a fixed ClusterIP: see [experiments/README.md](../README.md) ("DNS and the scheduler extender").
- The `generate_heterogeneous_assets.py` script applies H100 NVL-specific physics parameters (`computeGamma: 1.5`, `memoryEpsilon: 0.15`, `memoryGamma: 0.9`, `idleWattsPerGpu: 60`) to all GPU nodes.
- The retargeting logic in `05_sweep.py` no longer pins jobs to exact node names. CPU-only jobs are constrained only to the CPU-only pool, while GPU jobs rely on the native `nvidia.com/gpu` extended resource so the Kubernetes scheduler can place them on any compatible H100 node.
- All scripts share the same asset generation path (`scripts/generate_heterogeneous_assets.py`), so the node classes are regenerated from the homogeneous cluster-nodes.yaml before each benchmark run. No hardware catalog is generated: the simulator uses the one embedded in `pkg/hwinv/assets/hardware.yaml`.
