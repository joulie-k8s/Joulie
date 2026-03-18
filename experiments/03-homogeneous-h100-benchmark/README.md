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

## Verifying no DNS leaks from KinD

KinD clusters with kube-scheduler extenders can leak internal `.cluster.local` DNS queries to the host's upstream DNS (since kube-scheduler runs with `hostNetwork: true`). The cluster setup script (`01_create_cluster_kwokctl.sh`) applies iptables rules to redirect all DNS from the control-plane node to CoreDNS, but you should verify this after cluster creation:

```bash
# On the host VM, capture DNS traffic leaving the machine:
sudo tcpdump -i eth0 port 53 -nn

# You should see NO queries for *.cluster.local or *.svc.cluster.local.
# If you see queries like:
#   joulie-scheduler-extender.joulie-system.svc.cluster.local
#   joulie-scheduler-extender.joulie-system.svc.cluster.local.cern.ch
# then the iptables DNS redirect is not working — recreate the cluster.
```

If DNS leaks are observed, delete and recreate the cluster:

```bash
kind delete cluster --name joulie-homogeneous-h100-benchmark
REUSE_EXISTING_CLUSTER=false bash scripts/10_setup_cluster.sh
```

## Steady-State Measurement Approach

This experiment uses a **timeout-based truncation** strategy to measure cluster behavior
during quasi-steady state. Instead of waiting for all jobs to complete (which produces a
long cooldown tail as the last few pods drain), the benchmark enforces a fixed timeout
(`run.timeout` in the config, typically 300s wall-clock).

**Rationale:**

- The cooldown tail (where a nearly-empty cluster finishes the last few jobs) is not
  representative of production workloads, where new work continuously arrives.
- Energy metrics during the tail are dominated by idle-cluster power, which dilutes the
  effect of energy-aware scheduling.
- By truncating at timeout, we capture the steady-state window where the cluster is under
  sustained load — exactly the regime where Joulie's power-capping policies matter most.

**Consequences for analysis:**

- The analysis pipeline (`06_collect.py`) does **not** filter out timed-out runs. All runs
  are included in summary statistics regardless of whether all jobs completed.
- The plotting pipeline (`07_plot.py`) does **not** generate completion-rate or
  timed-out-run plots, since timeout is a deliberate measurement strategy, not a failure.
- Energy, throughput, and makespan metrics reflect the truncated measurement window.

This approach is inspired by HPC benchmarking practices where steady-state measurements
are preferred over transient startup/shutdown phases.

## Notes

- The `generate_heterogeneous_assets.py` script applies H100 NVL-specific physics parameters (`computeGamma: 1.5`, `memoryEpsilon: 0.15`, `memoryGamma: 0.9`, `idleWattsPerGpu: 60`) to all GPU nodes.
- The retargeting logic in `05_sweep.py` no longer pins jobs to exact node names. CPU-only jobs are constrained only to the CPU-only pool, while GPU jobs rely on the native `nvidia.com/gpu` extended resource so the Kubernetes scheduler can place them on any compatible H100 node.
- All scripts share the same asset generation path (`scripts/generate_heterogeneous_assets.py`), so the node classes and hardware catalog are regenerated from the homogeneous cluster-nodes.yaml before each benchmark run.
