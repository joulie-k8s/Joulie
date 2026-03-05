# Joulie

<p align="center">
  <img src="./website/static/images/logo.png" alt="Joulie logo" width="180">
</p>

[![CI](https://github.com/matbun/joulie/actions/workflows/ci.yml/badge.svg)](https://github.com/matbun/joulie/actions/workflows/ci.yml)
[![Release](https://github.com/matbun/joulie/actions/workflows/release.yml/badge.svg)](https://github.com/matbun/joulie/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matbun/joulie)](https://goreportcard.com/report/github.com/matbun/joulie)
[![Go Reference](https://pkg.go.dev/badge/github.com/matbun/joulie.svg)](https://pkg.go.dev/github.com/matbun/joulie)

Kubernetes-native node-level power orchestration.

## Documentation

- Website (GitHub Pages): `https://matbun.github.io/joulie/`
- Docs source (Hugo + Docsy): [`website/`](./website/)

## Quickstart

1. Build and push images:

```bash
make build-push TAG=<tag>
```

2. Install chart:

```bash
make install TAG=<tag>
```

3. Label managed nodes:

```bash
kubectl label node <node1> joulie.io/managed=true --overwrite
kubectl label node <node2> joulie.io/managed=true --overwrite
```

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

See full manifest examples in docs page:

- `https://matbun.github.io/joulie/docs/workload-compatibility/`
