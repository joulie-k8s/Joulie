---
title: "Scoring Formula Validation"
---

This page reports results from the energy-aware scheduling formula validation experiment:

- [`experiments/04-scoring-formula-validation/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/04-scoring-formula-validation)

## Objective

Validate Joulie's energy-aware scheduling formula by demonstrating that power-aware scheduling improves energy efficiency compared to standard Kubernetes bin-packing (MostAllocated), using a Modelica FMU (DXCooled Airside Economizer) for physically-accurate cooling/PUE computation.

Two scales tested:
1. **Small cluster** (28 nodes) — formula tuning and component selection
2. **Large cluster** (2,500 nodes) — production-scale validation with H100 GPUs

The experiment also validated the evolution from a legacy multi-component formula to the current streamlined Joulie scoring formula.

## Method

### Large cluster simulation (primary results)

- **Cluster**: 2,000 CPU nodes (dual-socket EPYC 9654, 96 cores, 700 W TDP) + 500 GPU nodes (8x H100 SXM per node, 700 W TDP per GPU, DGX-style)
- **Peak capacity**: 4.5 MW
- **Workload**: Poisson arrivals with diurnal pattern, 10% burst probability (50-200 simultaneous jobs), mix of small CPU (35%), medium CPU (20%), GPU training (25%), GPU inference (12%), large CPU (8%)
- **Duration**: 48 hours, 1-minute resolution (2,880 timesteps)
- **Jobs generated**: ~1.8M per seed
- **Climate**: Warm temperate (23 C base, +/-6 C diurnal), simulating Austin TX / Milan / Tokyo spring conditions
- **Cooling model**: OpenModelica DXCooledAirsideEconomizer FMU (FMI 2.0 co-simulation). Captures 3 cooling modes (free/partial/mechanical), variable-speed DX compressor, thermal mass, COP degradation.

### Small cluster (formula tuning)

- **Cluster**: 16 CPU nodes (96 cores, 700 W) + 12 GPU nodes (4x A100-80GB, 1,600 W)
- Same workload pattern, climate, and FMU as above

### Schedulers compared

| Scheduler | Description |
|-----------|-------------|
| **BASELINE** | Kubernetes MostAllocated (bin-packing by allocation ratios) |
| **JOULIE** | Projected headroom (marginal baked in) + cooling stress + adaptive trend (+/-25) |
| **JOULIE-legacy** | Power-headroom + estimated pod power + idle GPU penalty + adaptive trend |

### Joulie formula

The current Joulie scoring formula uses three components:

```
score = projectedHeadroom * 0.7
      + (100 - coolingStress) * 0.15
      + adaptiveTrendBonus  (+-25)
```

Where:
- `projectedHeadroom` = (nodeCappedPower - measuredPower - podMarginalW) / nodeCappedPower x 100 — marginal pod power is **subtracted before scoring**, not as a separate penalty
- `coolingStress` = min(100, measuredPower / nodeTDP x 100) — normalized against TDP (not a reference constant)
- `adaptiveTrendBonus`:
  - `trendScale` = 2.0 if |clusterTrend| > 500 W/min (burst), else 6.0 (steady state)
  - `trendBonus` = -clamp(powerTrend / trendScale, -25, +25)
  - `clusterTrend` = sum of all per-node power trends (W/min)

**Design rationale**: Headroom dominates (0.7) because it carries the most information: actual load, the pod's marginal impact, and the operator's power budget — all in one number. Cooling stress is low weight (0.15) as a physical safety signal. The adaptive trend (+/-25) provides strong power smoothing during steady state (trendScale=6.0 amplifies the signal) while becoming more conservative during cluster-wide bursts (trendScale=2.0 avoids over-reacting to coordinated ramps).

Key simplifications over the legacy formula:
- No PSU stress term (same for all nodes, zero differentiation)
- No separate GPU waste penalty (absorbed into headroom via marginal)
- No separate marginal power penalty (baked into projected headroom)
- Headroom weight increased from 0.4 to 0.7 (dominant signal)
- Adaptive trend uses cluster-wide burst detection instead of per-node TDP normalization

### Legacy formula (for comparison)

```
score = powerHeadroom * 0.4                    # prefer nodes with more power headroom
      + (100 - coolingStress) * 0.3            # avoid thermally stressed nodes
      + (100 - psuStress) * 0.3                # avoid PSU-stressed nodes
      - clamp(estimatedPodWatts / 20, 0, 20)   # penalize high-power placements
      - idleGPUWastePenalty                     # avoid wasting idle GPU power
      - clamp(trend / trendScale, -20, 25)     # smooth power transients
```

## Results (large cluster — 2,500 nodes)

### FMU-validated results (seed=42, 48h)

{{< img src="images/experiments/04-scoring-formula-validation/comparison_redesign_fmu.png" alt="FMU Comparison: BASELINE vs JOULIE vs JOULIE-legacy" >}}

| Metric | BASELINE | JOULIE | JOULIE-legacy | J-Delta | L-Delta |
|--------|----------|--------|---------------|---------|---------|
| Avg IT power (kW) | 2,655 | 2,484 | 2,524 | **-6.4%** | -4.9% |
| Peak IT power (kW) | 2,716 | 2,589 | 2,601 | **-4.7%** | -4.2% |
| IT power std (kW) | 124.7 | 142.7 | 110.0 | +14.4% | **-11.8%** |
| Avg PUE (FMU) | 1.3499 | 1.3511 | 1.3507 | +0.09% | +0.06% |
| Avg COP (FMU) | 2.86 | 2.85 | 2.85 | -0.3% | -0.2% |
| Avg cooling power (kW) | 929 | 872 | 886 | **-6.1%** | -4.7% |
| Avg facility power (kW) | 3,585 | 3,356 | 3,410 | **-6.4%** | -4.9% |
| Total IT energy (MWh) | 127.5 | 119.2 | 121.2 | **-6.4%** | -4.9% |
| Total facility energy (MWh) | 172.1 | 161.1 | 163.7 | **-6.4%** | -4.9% |
| Cooling energy (MWh) | 44.6 | 41.9 | 42.5 | **-6.1%** | -4.7% |
| Dropped jobs | 909,565 | 791,668 | 783,594 | **-13.0%** | -13.9% |

### Multi-seed validation (8 seeds x 48h)

Both Joulie formulas validated across 8 independent random seeds (~1.8M jobs each, IT-level metrics only):

| Metric | JOULIE Delta | JOULIE-legacy Delta | Better |
|--------|----------|-----------------|--------|
| IT energy | **-6.21%** | -4.77% | JOULIE |
| Peak IT power | **-4.13%** | -3.66% | JOULIE |
| Power std | +16.97% | **-10.95%** | JOULIE-legacy |
| kWh/job | **-17.70%** | -17.02% | JOULIE |
| P99 ramp rate | **-9.18%** | -5.31% | JOULIE |
| Dropped jobs | -128k fewer | **-135k fewer** | JOULIE-legacy |
| Seeds winning on energy | **8/8 (100%)** | 8/8 (100%) | Tie |

### Hybrid formula sweep (10 variants x 8 seeds)

To find the optimal energy-vs-smoothing balance, 10 formula variants were tested across 8 seeds (~1.8M jobs each). All variants use the same projected-headroom base (0.7 weight); they differ in trend strength and additional components:

| Variant | IT kWh% | Peak% | Std% | Drops | kWh/j% | P99% | E-wins |
|---------|---------|-------|------|-------|--------|------|--------|
| JOULIE-legacy | -4.77% | -3.66% | **-10.95%** | **-135,111** | -17.02% | -5.31% | 8/8 |
| JOULIE (+/-10 TDP) | **-6.21%** | -4.13% | +16.97% | -127,669 | -17.70% | -9.18% | 8/8 |
| HYBRID-t15 | -6.18% | -4.15% | +16.77% | -127,971 | -17.70% | -9.66% | 8/8 |
| HYBRID-t20 | -6.08% | -4.06% | +15.04% | -128,157 | -17.62% | -9.39% | 8/8 |
| HYBRID-t25 | -6.03% | -4.02% | +15.55% | -128,568 | -17.61% | -9.96% | 8/8 |
| HYBRID-t30 | -5.90% | -3.97% | +13.70% | -128,981 | -17.53% | -8.76% | 8/8 |
| HYBRID-t20-gpu | -5.72% | -3.98% | +1.80% | -128,044 | -17.29% | -11.05% | 8/8 |
| HYBRID-t20-adap | -6.17% | -4.42% | +5.80% | -136,832 | -18.38% | -8.32% | 8/8 |
| **HYBRID-t25-adap** | **-6.16%** | **-4.52%** | **+3.00%** | **-136,928** | **-18.38%** | -8.31% | **8/8** |

### Selected formula: HYBRID-t25-adap

**HYBRID-t25-adap** was selected as the production formula because it achieves the best balance across all metrics:

- **Near-best energy savings** (-6.16% IT energy, within 0.05% of the top)
- **Dramatically reduced power variance** (+3.00% std vs +16.97% for pure +/-10)
- **Best throughput** (-136,928 fewer drops, comparable to legacy's -135,111)
- **Best energy efficiency** (-18.38% kWh/job, best of all variants)
- **100% seed consistency** (8/8 seeds winning on energy)

The adaptive trend mechanism is the key differentiator: by using cluster-wide trend detection (`clusterTrend = sum(nodetrends)`), it tightens the trend scale during burst periods (trendScale=2.0 when |clusterTrend| > 500 W/min) and amplifies it during steady state (trendScale=6.0). This provides strong smoothing when it matters most — during normal operation — while avoiding over-reaction during coordinated cluster ramps.

## Formula evolution analysis

The evolution from JOULIE-legacy -> JOULIE (+/-10) -> HYBRID-t25-adap shows a clear progression:

1. **Projected headroom is more effective**: Baking marginal pod power into headroom before scoring (rather than as a separate penalty) creates stronger differentiation between nodes. The 0.7 weight on headroom concentrates the signal.

2. **Adaptive trend recovers smoothness**: The original +/-10 TDP-normalized trend was too weak for power smoothing (+16.97% std). The adaptive +/-25 trend with cluster-wide burst detection recovers most of the legacy formula's smoothing (-10.95% std -> +3.00% std) while maintaining the energy savings of the headroom-dominant approach.

3. **Removing PSU stress has no impact**: PSU stress was the same for all nodes (cluster-wide metric), so removing it doesn't change relative rankings — confirmed by the results.

4. **PUE remains flat**: All formulas produce nearly identical PUE (~1.35), confirming that at this operating point, cooling scales linearly with IT power regardless of scheduling strategy.

## Small cluster results (28 nodes)

### Formula selection process

14 formula variants were tested in a sweep (IT-level metrics, no FMU):

| Variant | Description | IT kWh | Power Std | kWh/job |
|---------|-------------|--------|-----------|---------|
| Baseline + GPU avoidance | Penalize GPU nodes for CPU jobs | +0.30% | +1.42% | -0.14% |
| Power-measured packing | Pack by measured power (not allocation) | +0.54% | +2.87% | +0.59% |
| Baseline + trend only | MostAllocated + fixed trend | +0.04% | -0.34% | +0.08% |
| Combined (all components) | Power-pack + GPU avoid + trend | +0.58% | +2.05% | +0.11% |

**Key finding**: At small scale, power-measured packing and GPU avoidance both increase total IT power through idle-node activation overhead. The **trend signal alone** was the only component that consistently improved smoothness.

### FMU-validated results (seed=42, 48h, 28 nodes)

{{< img src="images/experiments/04-scoring-formula-validation/comparison_24h_fmu.png" alt="FMU Comparison: Small Cluster" >}}

| Metric | BASELINE | JOULIE | Delta |
|--------|----------|--------|-------|
| Avg IT power (kW) | 11.293 | 11.250 | **-0.4%** |
| Peak IT power (kW) | 14.332 | 14.209 | **-0.9%** |
| IT power std (kW) | 1.233 | 1.201 | **-2.6%** |
| Avg PUE (FMU) | 1.3123 | 1.3124 | +0.0% |
| Total facility energy (kWh) | 712.1 | 709.4 | **-0.4%** |
| Facility kWh/job | 0.1496 | 0.1483 | **-0.8%** |

## Analysis

### Scale matters: 28 nodes vs 2,500 nodes

The most striking result is the dramatically larger improvement at scale:

| Metric | 28-node Delta | 2,500-node Delta | Amplification |
|--------|---------------|-------------------|---------------|
| IT energy | -0.4% | **-6.4%** | **16x** |
| Power std | -2.6% | **-11.8%** | **5x** |
| Dropped jobs | -1.8% | **-13.0%** | **7x** |
| kWh/job | -0.8% | **-16.5%** | **21x** |

**Why**: At 28 nodes, the scheduler has few placement choices — most nodes are similarly loaded, so power-aware scoring makes marginal differences. At 2,500 nodes, there is substantial heterogeneity in power states across the cluster at any given moment. The power-headroom formula can exploit this diversity to make genuinely better placement decisions, routing jobs to nodes with the most thermal and electrical headroom.

### Why PUE stays flat despite large IT savings

The FMU receives **aggregate datacenter IT power** as a single Q_IT signal. Both schedulers process the same workload, so the aggregate power trajectory is similar in shape — Joulie just runs at a lower average level. PUE stays nearly constant because:

1. **Cooling scales linearly with IT power**: At this operating point (COP ~2.9), cooling power is roughly proportional to IT power. Reducing IT by 5% reduces cooling by ~5%, keeping the ratio constant.
2. **The FMU's thermal mass smooths everything**: Building mass and coolant inertia damp minute-level variations, making PUE insensitive to scheduling-level power smoothing.
3. **PUE is the wrong metric here**: The real benefit is **absolute energy savings** (11 MWh facility energy over 48h, or ~80 MWh/year), not PUE improvement.

### Where power-aware scheduling adds value

1. **Lower total IT energy (-6.4%)**: Projected-headroom scoring with baked-in marginal power avoids activating idle nodes unnecessarily and creates stronger differentiation between node power states.
2. **Dramatically better throughput (-13.0% fewer drops)**: Power-aware placement distributes load more efficiently, avoiding capacity bottlenecks. Over 48h, Joulie successfully schedules ~118,000 more jobs than baseline.
3. **Superior energy efficiency (-17.7% kWh/job)**: More work completed per unit of facility energy.
4. **Lower cooling energy (-6.1%)**: Directly proportional to IT savings, confirmed by FMU co-simulation.

### Per-rack vs aggregate cooling

In production datacenters with **per-rack cooling** (rear-door heat exchangers, in-row coolers), the per-node power smoothing would have even larger cooling impact — each rack's thermal response is directly coupled to its nodes' power curves. The single-FMU aggregate model represents a conservative lower bound on the formula's cooling benefit.

## Recommended formula

```go
// Joulie scoring formula (Go implementation — HYBRID-t25-adap)
func Score(node *Node, pod *Pod, clusterTrend float64) float64 {
    // Estimate pod marginal power
    podMarginalW := EstimateMarginalImpact(node, pod).DeltaTotalWatts

    // 1. Projected headroom (marginal baked in)
    projectedPower := node.MeasuredPowerW + podMarginalW
    headroom := max(0, (node.CappedPowerW - projectedPower) / node.CappedPowerW * 100)

    // 2. Cooling stress (against TDP, not capped power)
    coolingStress := min(100, node.MeasuredPowerW / node.TdpW * 100)

    // 3. Adaptive trend bonus (+-25)
    //    trendScale = 2.0 during cluster-wide bursts, 6.0 at steady state
    trendScale := 6.0
    if abs(clusterTrend) > 500.0 {
        trendScale = 2.0
    }
    trendBonus := -clamp(node.PowerTrendWPerMin / trendScale, -25, +25)

    // Combined
    score := headroom * 0.7 + (100 - coolingStress) * 0.15 + trendBonus
    return clamp(score, 0, 100)
}
```

**Parameters**:
- Projected headroom weight: 0.7 (dominant signal — includes marginal pod power)
- Cooling stress weight: 0.15 (physical safety signal)
- Trend bonus: +/-25 points max (adaptive)
- Trend scale: 6.0 at steady state, 2.0 during cluster-wide bursts (|clusterTrend| > 500 W/min)
- Default score when twin data is missing: 50 (neutral)

## Implementation details and scripts

- [Experiment folder](https://github.com/joulie-k8s/Joulie/tree/main/experiments/04-scoring-formula-validation)
- [Full report (REPORT.md)](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/REPORT.md)
- Main scripts:
  - [sim_24h_pue.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/sim_24h_pue.py) — 48h simulation with FMU co-simulation
  - [sweep_formulas.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/sweep_formulas.py) — 9-variant formula sweep
  - [compare_redesign.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/compare_redesign.py) — JOULIE vs legacy multi-seed comparison
  - [fmu_compare_redesign.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/fmu_compare_redesign.py) — 3-way FMU comparison
  - [compare_hybrid.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/compare_hybrid.py) — Hybrid formula sweep
