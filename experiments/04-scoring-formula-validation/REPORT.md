# Experiment 04: Scoring Formula Validation

> **Not a valid result.** An audit of this experiment found that jobs that do not fit are dropped instead of queued, and energy is divided by the placed jobs only; the arms are mislabelled, and the selected formula never ran through the cooling model; the formula was chosen and reported on the same 8 seeds. The scripts model the cluster, the cooling and the scheduler's formula in Python and import no Joulie code, so they do not follow changes to the extender. Its results do not stand and are not published. The experiment is to be redone on the scheduling logic extracted into an importable library ([#122](https://github.com/joulie-k8s/Joulie/issues/122)), as a behaviour scenario ([#115](https://github.com/joulie-k8s/Joulie/issues/115)).

This experiment ran on Python models of the cluster, the cooling and the scheduler's formula, not on Joulie or its simulator, Joulie's test bed for its behaviour. On assumed hardware behaviour, such a model cannot show what Joulie saves on real hardware; what meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

## Objective

Test whether Joulie's energy-aware scheduling formula improves energy
efficiency compared to standard Kubernetes bin-packing (MostAllocated),
using a Modelica FMU (DXCooled Airside Economizer) for the cooling and PUE.

Two scales tested:
1. **Small cluster** (28 nodes): formula tuning and component selection
2. **Large cluster** (2,500 nodes): production-scale runs with H100 GPUs

## Method

### Simulation Setup

#### Large Cluster (Main Runs)

- **Cluster**: 2,000 CPU nodes (dual-socket EPYC 9654, 96 cores, 700W TDP) +
  500 GPU nodes (8× H100 SXM per node, 700W TDP per GPU, DGX-style)
- **Peak capacity**: 4.5 MW
- **Workload**: Poisson arrivals with diurnal pattern, 10% burst probability
  (50-200 simultaneous jobs), mix of small CPU (35%), medium CPU (20%),
  GPU training (25%), GPU inference (12%), large CPU (8%)
- **Duration**: 48 hours, 1-minute resolution (2,880 timesteps)
- **Jobs generated**: ~1.8M per seed
- **Climate**: Warm temperate (23°C base, ±6°C diurnal), simulating
  Austin TX / Milan / Tokyo spring conditions
- **Cooling model**: OpenModelica DXCooledAirsideEconomizer FMU (FMI 2.0
  co-simulation) run via Docker. Captures 3 cooling modes (free/partial/
  mechanical), variable-speed DX compressor, thermal mass, COP degradation.

#### Small Cluster (Formula Tuning)

- **Cluster**: 16 CPU nodes (96 cores, 700W) + 12 GPU nodes (4× A100-80GB, 1600W)
- Same workload pattern, climate, and FMU as above

### Schedulers Compared

| Scheduler | Description |
|-----------|-------------|
| **BASELINE** | Kubernetes MostAllocated (bin-packing by allocation ratios) |
| **JOULIE** | Projected headroom (marginal baked in) + cooling stress + adaptive trend (±25) |
| **JOULIE-legacy** | Power-headroom + estimated pod power + idle GPU penalty + adaptive trend |

### Joulie Formula

The formula this experiment tested uses three components:

```
score = projectedHeadroom * 0.7
      + (100 - coolingStress) * 0.15
      + adaptiveTrendBonus  (±25)
```

Where:
- `projectedHeadroom` = (nodeCappedPower - measuredPower - podMarginalW) / nodeCappedPower × 100
  (marginal pod power is **subtracted before scoring**, not as a separate penalty)
- `coolingStress` = min(100, measuredPower / nodeTDP × 100)
  (normalized against TDP, not a reference constant)
- `adaptiveTrendBonus`:
  - `trendScale` = 2.0 if |clusterTrend| > 500 W/min (burst), else 6.0 (steady state)
  - `trendBonus` = -clamp(powerTrend / trendScale, -25, +25)
  - `clusterTrend` = sum of all per-node power trends (W/min)

### Legacy Formula (for comparison)

The legacy formula used separate components with lower headroom weight:

```
score = powerHeadroom * 0.4                    # prefer nodes with more power headroom
      + (100 - coolingStress) * 0.3            # avoid thermally stressed nodes
      + (100 - psuStress) * 0.3                # avoid PSU-stressed nodes
      - clamp(estimatedPodWatts / 20, 0, 20)   # penalize high-power placements
      - idleGPUWastePenalty                     # avoid wasting idle GPU power
      - clamp(trend / trendScale, -20, 25)     # smooth power transients
```

## Files

| File | Description |
|------|-------------|
| `sim_24h_pue.py` | Main 48h simulation with FMU co-simulation (2,500 nodes) |
| `sweep_formulas.py` | 9-variant formula sweep (IT-level, 8 seeds) |
| `ablation_sweep.py` | Component ablation study (6 variants × 8 seeds) |
| `trend_sweep.py` | Trend strength optimization (13 variants × 8 seeds) |
| `multi_seed_validate.py` | The same runs over 8 seeds |
| `compare_redesign.py` | JOULIE vs legacy multi-seed comparison (IT-level, 8 seeds) |
| `fmu_compare_redesign.py` | 3-way FMU comparison: BASELINE vs JOULIE vs legacy |
| `compare_hybrid.py` | Hybrid formula sweep: varying trend strengths |
| `parallel_runner.py` | Generic multiprocessing wrapper for sweep modules |
| `results_2500node/` | Where the scripts write the BASELINE vs JOULIE-legacy FMU runs (not in the repository) |
| `results_redesign_fmu/` | Where the scripts write the 3-way FMU runs and their PUE plots (not in the repository) |

## Reproduction

```bash
# Full simulation with FMU (requires Docker + OpenModelica image)
python sim_24h_pue.py --hours 48 --seed 42 --outdir ./results_2500node

# The same runs over 8 seeds (~90 min)
python multi_seed_validate.py

# Joulie vs legacy IT-level comparison (~30 min, 24 cores)
python compare_redesign.py

# FMU comparison with PUE plots (~20 min)
python fmu_compare_redesign.py --hours 48 --seed 42

# Hybrid formula sweep (~30 min, 24 cores)
python compare_hybrid.py

# Component ablation (~5 hours)
python ablation_sweep.py

# Trend optimization (~8 hours)
python trend_sweep.py

# Formula sweep (~6 hours)
python sweep_formulas.py
```
