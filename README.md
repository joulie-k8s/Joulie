# Joulie

[![CI](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/ci.yml)
[![Release](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml/badge.svg)](https://github.com/joulie-k8s/Joulie/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/joulie-k8s/Joulie)](https://goreportcard.com/report/github.com/joulie-k8s/Joulie)
[![Go Reference](https://pkg.go.dev/badge/github.com/joulie-k8s/Joulie.svg)](https://pkg.go.dev/github.com/joulie-k8s/Joulie)

Kubernetes-native node-level power orchestration.

Visit the docs page at [joulie-k8s.github.io/Joulie](https://joulie-k8s.github.io/Joulie/)

## Repository Layout

- [`charts/joulie`](./charts/joulie): Helm chart
- [`cmd/agent`](./cmd/agent): node agent
- [`cmd/operator`](./cmd/operator): control loop
- [`simulator/`](./simulator): telemetry/control simulator
- [`examples/`](./examples): runnable examples
- [`experiments/`](./experiments): benchmark experiments

## Pod Compatibility

Joulie uses Kubernetes scheduling constraints as workload intent source:

- `performance` workloads: require `joulie.io/power-profile=performance`
- `eco` workloads: require `joulie.io/power-profile=eco`
- unconstrained workloads: no power-profile affinity
