# Joulie Documentation

This directory contains the minimal docs for the current PoC.

## What exists today

- `NodePowerProfile` CRD (cluster-scoped)
- `TelemetryProfile` CRD (cluster-scoped)
- `joulie-operator` Deployment (rule-based central assignment loop)
- `joulie-agent` DaemonSet (node-local enforcer)
- NFD-based hardware discovery in agent
- AMD and Intel CPU backend hooks via Linux `powercap` sysfs path
- GPU detection only (enforcement is not implemented yet)

## Read this first

- [Quickstart](./quickstart.md)
- [DaemonSet Configuration](./daemonset.md)
- [CRD and Policy Model](./policy.md)
- [Policy Algorithms](./policies.md)
- [Operator Notes](./operator.md)
- [Input Telemetry and Actuation Interfaces](./telemetry.md)
- [Metrics Reference](./metrics.md)
- [Simulator Architecture](./simulator.md)
- [Simulator Algorithms](./simulator-algorithms.md)
- [Example: stress-ng throttling](../examples/01-stress-ng-throttling/README.md)
- [Example: Prometheus + Grafana](../examples/02-prometheus-grafana/README.md)
- [Example: Operator Configuration](../examples/04-operator-configuration/README.md)
- [Example: Workload Scheduling Classes](../examples/03-workload-intent-classes/README.md)
- [Example: Simulated Telemetry + Control (HTTP)](../examples/05-simulated-telemetry-control/README.md)
- [Example: KWOK Simulator (workload + power + agent pool)](../examples/06-simulator-kwok/README.md)
- [Experiment: KWOK Benchmark](./experiments/kwok-benchmark.md)

## Current naming

Current APIs are:

- `NodePowerProfile` (`nodepowerprofiles.joulie.io`) for operator-assigned node state
- `TelemetryProfile` (`telemetryprofiles.joulie.io`) for telemetry source configuration and control-state reporting
