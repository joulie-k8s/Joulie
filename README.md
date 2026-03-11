[![CI](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml)
[![Release](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/joulie-k8s/Joulie)](https://goreportcard.com/report/github.com/joulie-k8s/Joulie)

# Joulie

Kubernetes-native node-level power orchestration.

Visit the docs page at [joulie-k8s.github.io/Joulie](https://joulie-k8s.github.io/Joulie/)

## Why Joulie

Joulie is a Kubernetes-native proof of concept for energy-aware clusters.  
The problem it targets is simple: as AI and scientific workloads grow, clusters need better control loops to reduce energy use without breaking workload performance.

Today, Joulie focuses on node-level power orchestration and simulation. The broader direction is to support greener cluster operation use cases such as:

- cluster power-envelope management
- adaptation to grid carbon/energy mix (carbon-aware operation)
- coordinated CPU and GPU power throttling
- a digital-twin model of the data center to evaluate control and scheduling impact before applying changes

High-level architecture:

<p align="center">
  <img src="./website/static/images/joulie-arch.png" alt="Joulie architecture" width="900">
</p>

Current workload compatibility model (single source of truth: pod scheduling constraints):

- performance-sensitive workloads (recommended): require `joulie.io/power-profile NotIn ["eco"]`
- eco-only workloads (advanced): require `joulie.io/power-profile=eco` (optionally `joulie.io/draining=false`)
- unconstrained workloads: no power-profile affinity

## Repository Layout

- [`charts/joulie`](./charts/joulie): Helm chart
- [`cmd/agent`](./cmd/agent): node agent
- [`cmd/operator`](./cmd/operator): control loop
- [`simulator/`](./simulator): workload and power simulator (telemetry and control)
- [`examples/`](./examples): runnable examples
- [`experiments/`](./experiments): benchmark experiments
