# Examples

This directory contains runnable Joulie examples, ordered by setup complexity.

- [01-stress-ng-throttling](./01-stress-ng-throttling/README.md): apply high/low node profiles and observe throughput/frequency impact.
- [02-prometheus-grafana](./02-prometheus-grafana/README.md): wire dashboards and monitoring for Joulie metrics.
- [03-workload-intent-classes](./03-workload-intent-classes/README.md): workload scheduling via affinity (`performance` via `NotIn eco`, `eco`, and unconstrained/general).
- [04-operator-configuration](./04-operator-configuration/README.md): tune controller manager policies and control-loop env settings.
- [05-simulated-telemetry-control](./05-simulated-telemetry-control/README.md): run agent telemetry/control against simulator HTTP endpoints.
- [06-simulator-kwok](./06-simulator-kwok/README.md): mixed real+KWOK setup with simulator-driven workload and power behavior.
- [07-simulator-gpu-powercaps](./07%20-%20simulator-gpu-powercaps/README.md): heterogeneous GPU simulation (NVIDIA + AMD) with node-level GPU cap effects on power and completion time.
- [08-fmu-cooling-pue](./08-fmu-cooling-pue/README.md): FMU-based cooling and PUE simulation.
- [09-scheduler-steering](./09-scheduler-steering/README.md): energy-aware scheduler extender steering workloads to optimal nodes.
- [10-kubectl-joulie-demo](./10-kubectl-joulie-demo/README.md): end-to-end demo with `kubectl joulie status` plugin on a 6-node heterogeneous KWOK cluster.
- [11-otel-collector-bridge](./11-otel-collector-bridge/README.md): OpenTelemetry Collector config that turns Kepler, dcgm-exporter and AMD device-metrics-exporter series into the `hw.*` metrics the default node power preset reads.
- [12-governor-presets](./12-governor-presets/README.md): CPU and GPU governor presets on a simulated cluster of six hardware families: the two gates, the presets each node lists, the native settings they become, opting a GPU model in to `sustained`, and the restore.
