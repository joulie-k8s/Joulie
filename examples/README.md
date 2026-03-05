# Examples

This directory contains runnable Joulie examples, ordered by setup complexity.

- [01-stress-ng-throttling](./01-stress-ng-throttling/README.md): apply high/low node profiles and observe throughput/frequency impact.
- [02-prometheus-grafana](./02-prometheus-grafana/README.md): wire dashboards and monitoring for Joulie metrics.
- [03-workload-intent-classes](./03-workload-intent-classes/README.md): workload scheduling classes via affinity (`performance`, `eco`, and implicit flex/general when unconstrained).
- [04-operator-configuration](./04-operator-configuration/README.md): tune operator policies and control-loop env settings.
- [05-simulated-telemetry-control](./05-simulated-telemetry-control/README.md): run agent telemetry/control against simulator HTTP endpoints.
- [06-simulator-kwok](./06-simulator-kwok/README.md): mixed real+KWOK setup with simulator-driven workload and power behavior.
