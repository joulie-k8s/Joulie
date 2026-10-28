# 01 - CPU-only Benchmark

Baselines A (no management), B (static partition) and C (queue-aware) on a cluster of CPU-only nodes of three AMD EPYC families. The results, measured at regime, are in [REPORT.md](REPORT.md); how the runs are set up, sized and gated is in [experiments/README.md](../README.md).

These results come from the Joulie simulator, Joulie's test bed for its behaviour (caps applied, placement, policies), not evidence of energy savings. Their energy figures rest on hardware behaviour the simulator assumes: every CPU core does the same work per second whatever its model, and cap responses, power curves and workload intensities are assumed or fitted, not measured on real nodes. They do not show what Joulie saves on real hardware; what meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

## Cluster inventory

- 20 intensive nodes: AMD EPYC 9655, 2 sockets, 192 cores, 1536 GiB
- 10 highcore nodes: AMD EPYC 9965, 2 sockets, 384 cores, 1536 GiB
- 10 highfreq nodes: AMD EPYC 9375F, 2 sockets, 64 cores, 770 GiB
- 40 nodes in all ([cluster-nodes.yaml](configs/cluster-nodes.yaml))

## How to run

The report's configuration is [benchmark-40n.yaml](configs/benchmark-40n.yaml). Build and load the images as [experiments/README.md](../README.md) ("Images") shows, then:

```bash
experiments/01-cpu-only-benchmark/scripts/10_setup_cluster.sh \
  experiments/01-cpu-only-benchmark/configs/benchmark-40n.yaml
SIM_IMAGE_PULL_POLICY=IfNotPresent experiments/01-cpu-only-benchmark/scripts/20_run_benchmark.sh \
  experiments/01-cpu-only-benchmark/configs/benchmark-40n.yaml
```

A quick debug run:

```bash
experiments/01-cpu-only-benchmark/scripts/20_run_benchmark.sh \
  experiments/01-cpu-only-benchmark/configs/benchmark-debug.yaml
```

## Benchmark configs

- [benchmark-40n.yaml](configs/benchmark-40n.yaml) - the report's: 2 seeds and a repeat, at regime
- [benchmark-debug.yaml](configs/benchmark-debug.yaml) - 800 jobs, 1 seed, fast iteration
- [benchmark.yaml](configs/benchmark.yaml) - 300 jobs, 3 seeds
- [benchmark-overnight.yaml](configs/benchmark-overnight.yaml) - 2500 jobs, 3 seeds
- [benchmark-5k.yaml](configs/benchmark-5k.yaml) and its `-debug` and `-prod` variants - 5,000 nodes ([cluster-nodes-5k.yaml](configs/cluster-nodes-5k.yaml)) in standalone mode, run by `scripts/standalone_sweep.py`
