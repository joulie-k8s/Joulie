# Examples

Six tutorials, in the order a new user meets Joulie. The first three run on the
[simulated cluster](https://joulie-k8s.github.io/Joulie/main/docs/simulator/simulated-cluster/):
the real controller manager, agent and scheduler extender, built from your
checkout, managing emulated bare-metal nodes in one Docker container. They need
no hardware and no cluster of your own.

| Tutorial | What it shows | Needs |
|---|---|---|
| [01 Simulated cluster](01-simulated-cluster/README.md) | A first install; the hardware the agent discovers; the digital twin; power profiles and the caps the agent writes; capping a node by hand; `kubectl joulie` on a live workload | Docker, Go, `kubectl`, `helm`, `jq` |
| [02 Workload classes](02-workload-classes/README.md) | Performance and standard pods; the scheduler extender keeping performance pods off eco nodes; its scores | the cluster of 01 |
| [03 Grafana dashboard](03-grafana-dashboard/README.md) | The dashboard the chart ships, on the simulated cluster; scraping Joulie on a real cluster | the cluster of 01 |
| [04 OpenTelemetry Collector bridge](04-otel-collector-bridge/README.md) | Kepler, dcgm-exporter and the AMD device-metrics-exporter as the `hw.*` metrics the digital twin reads | Docker |
| [05 Governor presets](05-governor-presets/README.md) | CPU and GPU governor presets, a second energy dimension next to the caps, on six hardware families | Docker, Go, `kubectl`, `helm`, `jq` |
| [06 Cooling and PUE](06-cooling-pue/README.md) | Cooling models and the PUE they give for a power trace, with a Modelica FMU | Python, Docker |

To install Joulie on a real cluster, follow the
[quickstart](https://joulie-k8s.github.io/Joulie/main/docs/getting-started/01-quickstart/).
The benchmark experiments and the cluster they run on are in
[`experiments/`](../experiments/README.md).
