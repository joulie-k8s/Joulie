# CPU-Only Benchmark Report

## Scope

This report documents the benchmark results from:

- [`experiments/01-cpu-only-benchmark/`](.)

It covers: experimental setup, controller policy algorithms, simulator models, measured outcomes, plot commentary, and interpretation.

---

## 1. Experimental Setup

### 1.1 Cluster and node topology

- Kind control-plane + worker (real Kubernetes control path).
- 8 fake KWOK worker nodes labeled `joulie.io/managed=true`.
- KWOK nodes are tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
- Simulator pod runs on the real kind worker.
- Workload pods target KWOK nodes via nodeSelector + toleration.

Node inventory source: [`configs/cluster-nodes.yaml`](./configs/cluster-nodes.yaml)

### 1.2 Node inventory

CPU-only cluster - no GPU nodes.

| Node prefix | Count | CPU | Cores | RAM |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | 2 | AMD EPYC 9965 192-Core | 384 (2x192) | 1536 GiB |
| kwok-cpu-highfreq | 2 | AMD EPYC 9375F 32-Core | 64 (2x32) | 770 GiB |
| kwok-cpu-intensive | 4 | AMD EPYC 9655 96-Core | 192 (2x96) | 1536 GiB |

Total: **8 nodes**, **2304 CPU cores**, **0 GPUs**.

### 1.3 Hardware model parameters (simulator)

Hardware profiles are derived from the node inventory by the simulator via product labels. CPU power model:

```
P(u, f) = IdleW + (PeakW - IdleW) * u^AlphaUtil * f^BetaFreq
```

where `u` = CPU utilization, `f` = frequency scale.

| CPU family | IdleW | PeakW | AlphaUtil | BetaFreq | FMin MHz | FMax MHz |
|---|---:|---:|---:|---:|---:|---:|
| AMD EPYC 9965 | 120 | 960 | 1.15 | 1.30 | 1500 | 3200 |
| AMD EPYC 9375F | 60 | 480 | 1.10 | 1.25 | 2800 | 4200 |
| AMD EPYC 9655 | 95 | 760 | 1.12 | 1.28 | 1500 | 3600 |

When policy sets `cpu_eco_pct_of_max=65`, RAPL caps are set to 65% of each node's peak modeled power. If the resulting cap cannot be satisfied at `FMinMHz`, the node is marked `CapSaturated`.

### 1.4 Run configuration

From [`configs/benchmark.yaml`](./configs/benchmark.yaml):

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 3 |
| Jobs | 300 |
| Mean inter-arrival | 0.20 s |
| Time scale | 60x |
| Timeout per run | 1800 s |
| Perf ratio | 20% |
| GPU ratio | 0% |
| Work scale | 0.15 |
| Allowed workload types | `cpu_preprocess`, `cpu_analytics` |

### 1.5 Baselines

- **A**: simulator only - no Joulie operator or agent (frequency/power-profile affinity stripped from pods).
- **B**: Joulie with `static_partition` policy.
- **C**: Joulie with `queue_aware_v1` policy.

---

## 2. Policy Algorithms

### 2.1 Pod classification

Pods are classified from their `joulie.io/power-profile` scheduling constraints:

- `performance` only -> performance-sensitive
- `eco` only -> eco-only
- both or unconstrained -> general
- unknown -> treated as performance-sensitive (safe default)

### 2.2 Static partition (`static_partition`)

Given `N` managed nodes:

- `hpCount = round(N * STATIC_HP_FRAC)`
- First `hpCount` nodes -> `performance` profile (full frequency, no cap)
- Remaining -> `eco` profile (RAPL cap at `cpu_eco_pct_of_max` of peak)

In this run: `STATIC_HP_FRAC=0.30`, so on 8 nodes: 2 performance, 6 eco.

### 2.3 Queue-aware (`queue_aware_v1`)

Let:

- `baseCount = round(N * QUEUE_HP_BASE_FRAC)`
- `perfIntentPods = count(running performance-sensitive pods cluster-wide)`
- `queueNeed = ceil(perfIntentPods / QUEUE_PERF_PER_HP_NODE)`

Then:

- `hpCount = clamp(max(baseCount, queueNeed), QUEUE_HP_MIN, QUEUE_HP_MAX, N)`

In this run: `QUEUE_HP_BASE_FRAC=0.30`, `QUEUE_HP_MIN=2`, `QUEUE_HP_MAX=15`, `QUEUE_PERF_PER_HP_NODE=20`.

### 2.4 Downgrade guard

When a node transitions `performance -> eco`, the operator defers the cap change while performance-sensitive pods are still running there, marking it `joulie.io/draining=true` until safe.

---

## 3. Simulator Algorithms

### 3.1 CPU power model

Per-node power at utilization `u` and frequency scale `f`:

```
P(u, f) = IdleW + (PeakW - IdleW) * u^AlphaUtil * f^BetaFreq
```

### 3.2 RAPL cap enforcement and DVFS

At each simulator tick:

1. Policy writes `rapl.set_power_cap_watts` -> updates `CapWatts` (clamped to `[MinCapW, MaxCapW]`).
2. If `P(u, f) > CapWatts`, solver finds the maximum feasible `f`:
   - `f_target = solveFreqScaleForCap(u, CapWatts)`
   - clamped to `[FMinMHz/FMaxMHz, 1.0]`
3. If even `FMinMHz` exceeds cap, node is flagged `CapSaturated=true`.
4. Frequency ramps toward target with `DvfsRampMS` time constant.
5. Final effective power: `min(P(u, f_effective), CapWatts + RaplHeadW)`.

### 3.3 Energy integration

At each workload loop tick of duration `dt` (wall seconds):

```
E_node += P_node * dt          // per-node Joules (wall time)
E_cluster += sum(P_node) * dt
```

Collection (`06_collect.py`) reads `/debug/energy` and scales by `time_scale`:

```
energy_sim_joules = totalJoules * time_scale
energy_sim_kwh    = energy_sim_joules / 3_600_000
```

### 3.4 Job progress and CPU slowdown

For a CPU job `j` on a node with current frequency scale `f`:

```
speed_j = requestedCPU_j * baseSpeedPerCore * (1 - (1-f) * sensitivityCPU_j)
cpuUnitsRemaining_j -= speed_j * dt / max(1, concurrentJobsOnNode)
```

Effective slowdown from throttling (single-job, no sharing):

```
slowdown = 1 / (1 - (1-f) * sensitivityCPU)
```

For `cpu_preprocess` and `cpu_analytics`, `sensitivityCPU in [0.7, 0.9]`, so a 35% frequency reduction (eco cap at 65%) translates to roughly 25-32% speed reduction on the worst case.

---

## 4. Measured Results

Source: [`runs/latest/results/summary.csv`](./runs/latest/results/summary.csv)

### 4.1 Per-seed results

| Baseline | Seed | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh sim) | Avg power (W) |
|---|---:|---:|---:|---:|---:|
| A | 1 | 317.98 | 113.21 | 17.63 | 3326 |
| A | 2 | 276.18 | 130.35 | 15.01 | 3261 |
| A | 3 | 239.74 | 150.17 | 13.25 | 3315 |
| B | 1 | 330.14 | 109.04 | 12.22 | 2221 |
| B | 2 | 275.86 | 130.50 | 10.10 | 2197 |
| B | 3 | 240.20 | 149.87 | 8.98 | 2242 |
| C | 1 | 328.92 | 109.45 | 12.25 | 2235 |
| C | 2 | 275.26 | 130.78 | 9.99 | 2177 |
| C | 3 | 239.66 | 150.21 | 9.02 | 2259 |

All 9 runs completed successfully (no timeouts, no gang deadlocks).

### 4.2 Baseline means (all 3 seeds)

| Baseline | Mean wall (s) | Mean throughput (jobs/sim-hr) | Mean energy (kWh sim) | Mean power (W) |
|---|---:|---:|---:|---:|
| A | 278.0 | 131.24 | 15.30 | 3301 |
| B | 282.1 | 129.80 | 10.43 | 2220 |
| C | 281.3 | 130.15 | 10.42 | 2224 |

### 4.3 Relative to A

| Baseline | Energy Delta | Throughput Delta | Power Delta |
|---|---:|---:|---:|
| B | **-31.8%** | -1.1% (negligible) | -32.7% |
| C | **-31.9%** | -0.8% (negligible) | -32.6% |

---

## 5. Plot Commentary

Plots are in: [`img/`](./img/)

### 5.1 Runtime distribution

![Runtime Distribution](./img/runtime_distribution.png)

- All three baselines complete within nearly identical wall-time windows.
- Run-to-run jitter (seed variance) is larger than any inter-baseline difference.
- Confirms that power capping at 65% does not measurably affect total job completion time on this CPU-only workload mix.

### 5.2 Energy vs makespan

![Energy vs Makespan](./img/energy_vs_makespan.png)

- B and C are consistently shifted to lower energy with near-identical makespan across all 3 seeds.
- Both Joulie baselines cluster tightly together, indicating that static and queue-aware policies behave similarly on this CPU-only workload.

### 5.3 Baseline means

![Baseline Means](./img/baseline_means.png)

- Energy is the clear differentiator; throughput and wall-time bars are indistinguishable.
- B and C achieve roughly the same ~32% energy reduction, driven by 6 nodes (75% of cluster) running at 65% CPU cap.

### 5.4 Completion summary

![Completion Summary](./img/completion_summary.png)

- All 3 seeds completed for all baselines (100% completion rate).
- No gang-scheduling or timeout issues on this CPU-only workload.

---

## 6. Interpretation

### Why does energy reduce by ~32% without throughput penalty?

The CPU-only workload types (`cpu_preprocess`, `cpu_analytics`) in this experiment have moderate CPU-frequency sensitivity (`sensitivityCPU in [0.7, 0.9]`). A 35% frequency reduction (eco cap at 65%) produces a 25-32% per-job slowdown. However:

1. **Cluster is over-provisioned**: 2304 cores spread over 8 nodes with only 300 lightweight CPU jobs means even eco nodes have spare capacity - jobs can use more cores to compensate.
2. **Scheduling load-balances**: unconstrained jobs naturally land on both performance and eco nodes; the scheduler fills eco nodes with general jobs at reduced frequency, while performance-sensitive jobs land on the 2 uncapped nodes.
3. **Energy scales with power x time**: eco nodes draw significantly less power for the same simulated duration -> energy falls without extending total makespan.
4. **Aggressive cap (65%) maximizes savings**: compared to previous runs at 80% eco cap that achieved ~8% savings, the 65% cap reduces power draw on eco nodes by roughly 35%, leading to ~32% total cluster energy savings.

### Why are static and queue-aware nearly identical here?

Both `static_partition` (B) and `queue_aware_v1` (C) achieve the same ~32% energy reduction because:

- With only 20% performance-affinity jobs on a small 8-node cluster, the queue-aware policy rarely needs to adjust HP node count beyond its base fraction.
- Both policies maintain a similar eco/performance split throughout the run.
- The operator reconcile interval (20 s) is fast enough that queue-aware can respond to demand, but demand is steady enough that it converges to a static-like split.

### Known limitations

- The simulator does not model memory-bandwidth contention between concurrent jobs.
- CPU `sensitivityCPU` values are heuristic estimates, not measured from real hardware.
- Gang jobs (multi-pod) are excluded from this benchmark (they require a gang scheduler like Kueue).

---

## 7. Best-Fit Use Case

The strongest observed benefit is:

- **energy reduction (-31.8% for static, -31.9% for queue-aware) with negligible throughput penalty** in a CPU-only mixed-workload cluster with 65% eco cap.

Both policies perform equivalently on CPU-only clusters. `static_partition` is simpler to configure and reason about; `queue_aware_v1` becomes more valuable when the performance-sensitive fraction is larger or more bursty.

---

## 8. Reproducibility

- Config: [`configs/benchmark.yaml`](./configs/benchmark.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py)
- Run artifacts: [`runs/latest/`](./runs/latest/)
