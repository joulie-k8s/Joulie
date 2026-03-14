[![CI](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml)
[![Release](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/joulie-k8s/Joulie)](https://goreportcard.com/report/github.com/joulie-k8s/Joulie)

# Joulie

**A Kubernetes-native digital twin for energy-efficient data centers.**

Visit the docs at [joulie-k8s.github.io/Joulie](https://joulie-k8s.github.io/Joulie/)

## What it is

Joulie builds a real-time digital twin of your Kubernetes cluster's energy state.
It continuously ingests telemetry — CPU/GPU power draw via RAPL and NVML/DCGM,
per-pod resource utilization via cAdvisor, and optional energy counters from
[Kepler](https://github.com/sustainable-computing-io/kepler) — to maintain an
up-to-date model of every node's thermal and power state.

That model drives two things:

1. **Energy control**: the operator applies CPU and GPU power caps (via RAPL and NVML)
   and publishes them as `NodePowerProfile` Kubernetes CRs. The node agent enforces them.

2. **Scheduling decisions**: a scheduler extender reads the twin's computed
   `NodeTwinState` — power headroom, predicted cooling stress, PSU load — to steer
   new pods toward nodes with the best energy-efficiency / performance trade-off.
   Performance workloads are kept on uncapped nodes; best-effort batch jobs
   are directed to eco (capped) nodes to preserve performance capacity.

The feedback loop: telemetry → twin update → cap decisions → new pod placement →
updated telemetry. This keeps the cluster's power envelope stable and prevents
cooling or PSU spikes without sacrificing critical workload performance.

<p align="center">
  <img src="./website/static/images/joulie-arch.png" alt="Joulie architecture" width="900">
</p>

## Why it matters

As AI and scientific workloads scale, clusters face:
- **Cooling bottlenecks**: GPU-dense racks exceed cooling capacity during training bursts
- **PSU/PDU overcommit**: peak power draw exceeds rack power budgets
- **Carbon cost**: flat power profiles waste energy during low-demand periods

Joulie addresses these by making the scheduler and operator aware of the physical
energy state of the cluster in real time, and by providing a digital twin that
can predict the impact of scheduling decisions before they are made.

## Architecture

Joulie has four components:

| Component | What it does |
|-----------|-------------|
| **Agent** (`cmd/agent`) | Runs on every node. Discovers hardware (CPU/GPU caps, slicing modes). Enforces RAPL/NVML power caps. Publishes `NodeHardware` CR. |
| **Operator** (`cmd/operator`) | Cluster-wide control loop. Reads `NodeHardware` + `NodePowerProfile` + Prometheus metrics. Runs the digital twin model. Publishes `NodeTwinState`. Triggers pod migration under thermal/PSU pressure. |
| **Scheduler extender** (`cmd/scheduler`) | HTTP extender for kube-scheduler. Reads `NodeTwinState` (30s TTL cache). Rejects eco nodes for performance pods. Scores nodes by power headroom and stress. |
| **Digital twin** (`pkg/operator/twin`) | O(1) parametric model. Computes power headroom, cooling stress (% of cooling capacity), PSU stress (% of PDU capacity). CoolingModel is pluggable — default: linear proxy; future: openModelica thermal simulation. |

## CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks, GPU slicing modes |
| `NodePowerProfile` | Operator/user | Desired state: power cap % |
| `NodeTwinState` | Operator | Twin output: headroom score, cooling stress, PSU stress, migration recommendations |
| `WorkloadProfile` | Operator (classifier) | Per-pod: workload class, CPU/GPU intensity, cap sensitivity |

## Workload classes

Joulie uses a single `joulie.io/workload-class` pod annotation to drive placement:

| Class | Scheduler behavior |
|-------|--------------------|
| `performance` | Hard-rejects eco (capped) nodes. Must run on full-power nodes. |
| `standard` | Default. Prefers performance nodes, tolerates eco. |
| `best-effort` | Slight preference for eco nodes. Leaves performance capacity free. |

The scheduler extender is always deployed as part of Joulie (lightweight HTTP server).
Without it, pods run anywhere and get standard Kubernetes scheduling.

## Key labels

| Label / Annotation | Where | Purpose |
|--------------------|-------|---------|
| `joulie.io/power-profile` | Node label | `eco` or `performance`. Set by operator. |
| `joulie.io/workload-class` | Pod annotation | `performance`, `standard`, `best-effort`. |
| `joulie.io/reschedulable` | Pod annotation | `true` if pod can be restarted on another node. |
| `joulie.io/cpu-sensitivity` | Pod annotation | `high`/`medium`/`low`. Overrides classifier output. |
| `joulie.io/gpu-sensitivity` | Pod annotation | `high`/`medium`/`low`. Overrides classifier output. |

## Repository layout

```
cmd/agent/          Node agent: hardware discovery, RAPL/NVML cap enforcement
cmd/operator/       Cluster operator: twin computation, NodeTwinState, migration
cmd/scheduler/      HTTP scheduler extender: filter + score via NodeTwinState
pkg/operator/twin/  Digital twin model (CoolingModel interface)
pkg/workloadprofile/classifier/  Workload classifier (util % primary, Kepler optional)
pkg/agent/hardware/ Hardware discovery (CPU/GPU caps, freq landmarks, slicing)
simulator/          Workload and power simulator for offline experiments
charts/joulie/      Helm chart
config/crd/         CRD manifests
experiments/        Benchmark experiments
  01-kwok-benchmark/
  02-heterogeneous-benchmark/
  03-heterogeneous_cluster_control_loop/  ← new: control loop benchmark
examples/           Runnable examples
website/            Documentation site
```

## Quick start

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Install via Helm
helm install joulie charts/joulie \
  --set agent.enabled=true \
  --set operator.enabled=true

# Annotate a performance pod
kubectl annotate pod my-gpu-job joulie.io/workload-class=performance
kubectl annotate pod my-batch-job joulie.io/workload-class=best-effort joulie.io/reschedulable=true
```

See the [docs](https://joulie-k8s.github.io/Joulie/) for full setup instructions.

## Run the experiments

```bash
# Fast simulation (no cluster needed) — 3 scenarios, ~200 jobs, ~1s
go run ./experiments/03-heterogeneous_cluster_control_loop/

# On a KWOK cluster
experiments/03-heterogeneous_cluster_control_loop/scripts/10_setup_cluster.sh
experiments/03-heterogeneous_cluster_control_loop/scripts/20_run_scenarios.sh
python3 experiments/03-heterogeneous_cluster_control_loop/scripts/30_collect.py
python3 experiments/03-heterogeneous_cluster_control_loop/scripts/40_plot.py
```
