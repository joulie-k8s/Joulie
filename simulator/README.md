# Energy-aware simulation of heterogeneous k8s cluster

This directory is the staging area for virtual experiments where hardware behavior is simulated.

## Goal

Compare [WAO](https://github.com/waok8s/waok8s) and Joulie under the same virtual workload conditions by simulating:

- node hardware capabilities,
- node/global telemetry,
- control effects (RAPL/DVFS now, GPU later).

## Simulator concept

The simulator should implement this loop:

1. ingest a **cluster template** (what hardware each node has),
2. ingest **allocation state** (which workloads are running where),
3. compute/write **telemetry values** used by controllers/schedulers,
4. receive **control intents** (for example Joulie RAPL/DVFS actions),
5. evolve next-step telemetry according to those actions.

This keeps benchmark conditions identical when comparing WAO vs Joulie.

## Quick start (local kind)

```bash
kind create cluster --config kind-small.yaml
```
