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
- [Operator Notes](./operator.md)
- [Input Telemetry and Actuation Interfaces](./telemetry.md)
- [Metrics Reference](./metrics.md)
- [Simulator Architecture](./simulator.md)
- [Example: stress-ng throttling](../examples/stress-ng-throttling/README.md)
- [Example: Prometheus + Grafana](../examples/prometheus-grafana/README.md)
- [Example: Operator Configuration](../examples/operator-configuration/README.md)
- [Example: Workload Intent Classes](../examples/workload-intent-classes/README.md)
- [Example: Simulated Telemetry + Control (HTTP)](../examples/simulated-telemetry-control/README.md)
- [Example: KWOK Simulator (workload + power + agent pool)](../examples/simulator-kwok/README.md)

## Current naming

Current APIs are:

- `NodePowerProfile` (`nodepowerprofiles.joulie.io`) for operator-assigned node state
- `TelemetryProfile` (`telemetryprofiles.joulie.io`) for telemetry source configuration and control-state reporting
