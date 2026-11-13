---
title: "Scoring Formula Validation"
---

> **Not a valid result.** Its results do not stand and are not published. The experiment is to be redone on the scheduling logic extracted into an importable library ([#122](https://github.com/joulie-k8s/Joulie/issues/122)), as a behaviour scenario ([#115](https://github.com/joulie-k8s/Joulie/issues/115)).

An audit found that jobs that do not fit are dropped instead of queued, energy is divided by the placed jobs only, the arms are mislabelled, the selected formula never ran through the cooling model, and the formula was chosen and reported on the same 8 seeds. The scripts are Python models of the cluster, the cooling and the formula; they import no Joulie code and do not follow changes to the extender.

## Objective

Test whether Joulie's energy-aware scheduling formula beats Kubernetes bin packing (MostAllocated) on energy, with a Modelica FMU (DXCooled Airside Economizer) for the cooling and PUE, on a 28-node cluster (formula tuning) and a 2,500-node one (main runs).

| Scheduler | Description |
|---|---|
| BASELINE | Kubernetes MostAllocated (bin packing by allocation ratios) |
| JOULIE | projected headroom (marginal power baked in) + cooling stress + adaptive trend (+/-25) |
| JOULIE-legacy | power headroom + estimated pod power + idle GPU penalty + adaptive trend |

Folder: [`experiments/04-scoring-formula-validation/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/04-scoring-formula-validation). The method, both formulas and the scripts: [Benchmark experiments](https://github.com/joulie-k8s/Joulie/wiki/Benchmark-Experiments#04-scoring-formula-validation).
