---
title: "Scoring Formula Validation"
---

> **Not a valid result.** An audit of this experiment found that jobs that do not fit are dropped instead of queued, and energy is divided by the placed jobs only; the arms are mislabelled, and the selected formula never ran through the cooling model; the formula was chosen and reported on the same 8 seeds. The scripts model the cluster, the cooling and the scheduler's formula in Python and import no Joulie code, so they do not follow changes to the extender. Its results do not stand and are not published. The experiment is to be redone on the scheduling logic extracted into an importable library ([#122](https://github.com/joulie-k8s/Joulie/issues/122)), as a behaviour scenario ([#115](https://github.com/joulie-k8s/Joulie/issues/115)).

This experiment ran on Python models of the cluster, the cooling and the scheduler's formula, not on Joulie or its simulator, Joulie's test bed for its behaviour. On assumed hardware behaviour, such a model cannot show what Joulie saves on real hardware; what meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

This page describes the experiment on the energy-aware scheduling formula:

- [`experiments/04-scoring-formula-validation/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/04-scoring-formula-validation)

## Objective

Test whether Joulie's energy-aware scheduling formula improves energy efficiency compared to standard Kubernetes bin-packing (MostAllocated), using a Modelica FMU (DXCooled Airside Economizer) for the cooling and PUE.

Two scales tested:
1. **Small cluster** (28 nodes): formula tuning and component selection
2. **Large cluster** (2,500 nodes): production-scale runs with H100 GPUs

## Method

### Large cluster simulation (main runs)

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

The formula this experiment tested uses three components:

```
score = projectedHeadroom * 0.7
      + (100 - coolingStress) * 0.15
      + adaptiveTrendBonus  (+-25)
```

Where:
- `projectedHeadroom` = (nodeCappedPower - measuredPower - podMarginalW) / nodeCappedPower x 100: marginal pod power is **subtracted before scoring**, not as a separate penalty
- `coolingStress` = min(100, measuredPower / nodeTDP x 100), normalized against TDP (not a reference constant)
- `adaptiveTrendBonus`:
  - `trendScale` = 2.0 if |clusterTrend| > 500 W/min (burst), else 6.0 (steady state)
  - `trendBonus` = -clamp(powerTrend / trendScale, -25, +25)
  - `clusterTrend` = sum of all per-node power trends (W/min)

### Legacy formula (for comparison)

```
score = powerHeadroom * 0.4                    # prefer nodes with more power headroom
      + (100 - coolingStress) * 0.3            # avoid thermally stressed nodes
      + (100 - psuStress) * 0.3                # avoid PSU-stressed nodes
      - clamp(estimatedPodWatts / 20, 0, 20)   # penalize high-power placements
      - idleGPUWastePenalty                     # avoid wasting idle GPU power
      - clamp(trend / trendScale, -20, 25)     # smooth power transients
```

## Implementation details and scripts

- [Experiment folder](https://github.com/joulie-k8s/Joulie/tree/main/experiments/04-scoring-formula-validation)
- [Full report (REPORT.md)](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/REPORT.md)
- Main scripts:
  - [sim_24h_pue.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/sim_24h_pue.py): 48h simulation with FMU co-simulation
  - [sweep_formulas.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/sweep_formulas.py): 9-variant formula sweep
  - [compare_redesign.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/compare_redesign.py): JOULIE vs legacy multi-seed comparison
  - [fmu_compare_redesign.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/fmu_compare_redesign.py): 3-way FMU comparison
  - [compare_hybrid.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/04-scoring-formula-validation/compare_hybrid.py): Hybrid formula sweep
