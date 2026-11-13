---
title: "Governor Exploration"
---

Experiment 05 measures CPU and GPU governor presets as an energy dimension of their own, next to power caps, to choose the defaults Joulie ships. The results come from emulated nodes whose governor and clock physics rest on assumed parameters: they show the shape of each trade-off, not measured savings ([#106](https://github.com/joulie-k8s/Joulie/issues/106)).

Experiment folder: [`experiments/05-governor-exploration/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/05-governor-exploration). The presets are under [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}); governor control is off by default (`GOVERNOR_CONTROL`, `GOVERNOR_WRITES`, [Configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}})).

## Questions

- **Q1**: per CPU driver, which preset minimises energy to solution at each load, and at what cost in time?
- **Q2**: per GPU family and workload kind, energy and time of each GPU preset, alone and with a 60 % cap?
- **Q3**: with a receiver gate, how much do multi-GPU jobs gain from `sustained`?
- **Q4**: at cluster level, do governors beat caps alone on energy without breaking the performance class?
- **Q5**: which conclusions survive the assumed parameters?

A default passes on a profile when it lowers energy with a p95 slowdown of at most 5 % (performance class) and 20 % (eco class), in Tier A, in Tier B and at every value Q5 sweeps.

## Method

- **Tier A**: `simulator/cmd/govsweep`, an offline sweep in virtual time, one fresh emulated node per cell, presets planned by `pkg/governor` as the agent plans them.
- **Grid**: five CPU profiles (one per frequency driver) and five GPU profiles, every preset, idle, steady and bursty loads, jobs on 1 to 4 GPUs, two seeds.
- **Measure**: the node's energy to solution and the job's time, as medians against the baseline arm (`performance` with boost on; `auto` with no cap).
- **Tier B**: one arm on a simulated cluster with Joulie in the loop, checking presets, results and restore.
- **Run**: 2026-10-28, 11,410 Tier A cells.

## Results

### E-G1: CPU presets (Q1)

| Load | `balanced` | `efficiency` | `efficiency`, boost off | `performance`, boost off |
|---|---|---|---|---|
| idle | 0 % energy | 0 % | 0 % | 0 % |
| steady 10 % | +4.3 % energy, +10.3 % time | +4.5 %, +10.8 % | -1.3 %, +11.6 % | -5.1 %, +5.3 % |
| steady 30 % | +0.7 %, +8.5 % | -0.4 %, +5.9 % | -16.2 %, +17.2 % | -18.3 %, +7.9 % |
| steady 50 % | -0.5 %, +3.1 % | -0.5 %, +2.5 % | -24.0 %, +13.9 % | -23.9 %, +10.5 % |
| steady 70 % | -0.6 %, +2.4 % | -0.6 %, +2.4 % | -27.7 %, +16.1 % | -27.3 %, +13.4 % |
| steady 100 % | -7.2 %, +2.1 % | -10.6 %, +3.1 % | -31.7 %, +22.0 % | -30.2 %, +17.8 % |
| bursty, I/O-bound (500 ms ramp) | -13.3 % energy, +12.0 % burst p95 | -12.8 %, +12.0 % | -13.2 %, +13.7 % | -8.6 %, +7.8 % |
| bursty, compute-bound (500 ms ramp) | -10.6 % energy, +108.8 % burst p95 | -9.3 %, +111.8 % | -10.3 %, +124.5 % | -5.9 %, +48.0 % |

Turning boost off saves the most (16 to 32 % from 30 % load up, at 8 to 22 % more time); with boost on, demand-driven presets save at most 0.6 % up to 70 % load and lengthen bursts at the assumed 500 ms ramp.

### E-G2: GPU presets (Q2)

| Arm | compute-bound | memory-bound | bandwidth-bound | idle node |
|---|---|---|---|---|
| `efficient` 90 % | -4.5 % energy, +6.2 % time | -7.9 %, +1.3 % | -8.2 %, +1.0 % | 0 % |
| `efficient` 80 % | -8.6 %, +13.5 % | -15.4 %, +2.6 % | -15.7 %, +2.0 % | 0 % |
| `efficient` 75 % | -10.4 %, +17.5 % | -18.9 %, +3.3 % | -19.4 %, +2.5 % | 0 % |
| `efficient` 60 % | -13.7 %, +35.9 % | -25.7 %, +11.4 % | -26.0 %, +10.2 % | 0 % |
| `sustained` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `peak` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `low` | +32 %, +330 % | +24 %, +233 % | +25 %, +228 % | 0 % |
| `auto`, cap 60 % | -7.1 %, +30.2 % | -11.0 %, +4.6 % | -10.8 %, +3.7 % | 0 % |

`efficient` lowers energy on every family that can express it, most on memory-bound work; `sustained` and `peak` only cost idle power while the receiver gate is off.

### Receiver gate (Q3)

| `slowFraction` | `auto`, gate on against off | `sustained` against `auto`, gate on |
|---|---|---|
| 1.0 (repository) | 0 % | +1.0 to +1.4 % energy, 0 % time |
| 0.7 | +8.6 % energy, +8.6 % time | -6.6 to -7.0 % energy, -7.9 % time |
| 0.5 | +24.0 % energy, +24.0 % time | -18.2 to -18.6 % energy, -19.4 % time |

`sustained` pays only when the gate slows the receiver; the gate is off in every repository profile.

### Sensitivity (Q5)

| Candidate or fact | Finding over every value Q5 sweeps |
|---|---|
| GPU `efficient` 80 % | energy at least -6.9 to -11.1 %, p95 slowdown at most 19.9 % on every GPU model |
| GPU `efficient` 90 % | energy at least -3.5 %, p95 at most +8.3 % |
| GPU `efficient` 75 % | over the slowdown limit at every value |
| CPU `efficiency` | energy negative on the two drivers where it passes Tier A; p95 over 20 % on `amd-epyc-2s-hsmp` at `eppCeilingDrop` 0.25 and `eppGain` 0.5 |
| `idleGovernorDeltaW` at 5 % of idle | demand-driven CPU presets save up to 4.1 % of idle energy (nothing at the repository's 0) |
| `freqRampMS` | sets the burst cost only |

### Acceptance (Tier A half)

| Candidate | Passes | Median energy change | p95 slowdown |
|---|---|---|---|
| CPU `efficiency` | 2 of 5 drivers | -2.2 to +0.5 % | +18.5 to +25.0 % |
| CPU `balanced` | 2 of 5 drivers | -1.6 to +0.1 % | +14.9 to +37.4 % |
| CPU `efficiency`, boost off | 0 of 5 | -26.8 to -10.4 % | +34.3 to +56.2 % |
| GPU `efficient` 75 % | 0 of 4 models | -21.0 to -13.8 % | +22.5 to +22.7 % |
| GPU `efficient` 80 % (shipped) | 4 of 4 | -17.2 to -11.2 % | +17.1 to +17.6 % |
| GPU `efficient` 90 % | 4 of 4 | -8.8 to -5.8 % | +7.8 to +8.3 % |
| GPU `efficient` 75 % with a 60 % cap | 0 of 4 | -20.2 to -12.9 % | +38.5 to +38.9 % |

GPU `efficient` at 80 % passes the Tier A half on every model; CPU `efficiency` does not pass, since a CPU default must pass on every driver. Tier B measured no energy (Q4), so no default is adopted yet ([#106](https://github.com/joulie-k8s/Joulie/issues/106), [#114](https://github.com/joulie-k8s/Joulie/issues/114)).

### Tier B

Arm `all` on a 16-Node simulated cluster passed every check (`TestSimclusterE2E`, GS01 to GS04): each result matched the mapping, and every file was restored after `GOVERNOR_CONTROL=off`. Workload runs per arm (Q4) are not part of this experiment.

## Reproduce

Tier A runs on one machine, with no Kubernetes:

```bash
experiments/05-governor-exploration/scripts/run_tier_a.sh [config] [out]
```

`config` defaults to `configs/tier-a.yaml` (`configs/tier-a-sensitivity.yaml` is the Q5 grid, `configs/tier-a-gate.yaml` the Q3 one) and `out` to a new directory under `~/.cache/joulie/experiments/05/`; the script writes `summary.md` there. Tier B needs Docker, as the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) does:

```bash
SIMCLUSTER_NAME=joulie-sim-gov experiments/05-governor-exploration/scripts/run_tier_b.sh all
```

The other arms, the settings and the result files are in the [experiment README](https://github.com/joulie-k8s/Joulie/tree/main/experiments/05-governor-exploration).

## Assumptions

- GPU idle costs: `dataPathHoldW` (10 W per GPU) and `gatingOffIdleW` (20 W) set what `sustained` and `peak` cost.
- GPU clock response: `clockBeta` and `memoryKneeFrac` set what `efficient` saves and costs.
- The receiver gate is off, with neutral fractions.
- CPU: `idleGovernorDeltaW`, `eppGain`, `eppCeilingDrop` and the 500 ms `freqRampMS`.
- The MI300X's lowest cap (`minW` 0 W) and the class intensities of the job model.

The measurements that would settle each, the per-profile tables and the full discussion: [Governor exploration](https://github.com/joulie-k8s/Joulie/wiki/Governor-Exploration).
