+++
title = "Tutorials"
linkTitle = "Tutorials"
slug = "tutorials"
weight = 2
+++

Six tutorials take you from a first install to the main features. Each one is a directory under [`examples/`](https://github.com/joulie-k8s/Joulie/tree/main/examples) with a README to follow and the files it applies.

The first three run on the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}): the real controller manager, agent and scheduler extender, built from your checkout, managing ten emulated bare-metal nodes in one Docker container. You need no hardware and no cluster of your own, so start there even if you plan to install Joulie on a real cluster next.

| Tutorial | What you will see | Needs |
|---|---|---|
| [01 Simulated cluster](https://github.com/joulie-k8s/Joulie/tree/main/examples/01-simulated-cluster) | A first install; the hardware the agent discovers (`NodeHardware`); the digital twin (`NodeTwin`); power profiles turned into RAPL and GPU caps, down to the files the agent writes; capping a node by hand; `kubectl joulie` on a live workload | Docker, Go, `kubectl`, `helm`, `jq` |
| [02 Workload classes](https://github.com/joulie-k8s/Joulie/tree/main/examples/02-workload-classes) | Performance and standard pods; the scheduler extender keeping performance pods off eco nodes, and the scores behind each placement | the cluster of 01 |
| [03 Grafana dashboard](https://github.com/joulie-k8s/Joulie/tree/main/examples/03-grafana-dashboard) | kube-prometheus-stack scraping Joulie, and the dashboard the chart ships | the cluster of 01, or yours |
| [04 OpenTelemetry Collector bridge](https://github.com/joulie-k8s/Joulie/tree/main/examples/04-otel-collector-bridge) | Kepler, dcgm-exporter and the AMD device-metrics-exporter turned into the `hw.*` metrics the digital twin reads | Docker |
| [05 Governor presets](https://github.com/joulie-k8s/Joulie/tree/main/examples/05-governor-presets) | CPU and GPU governor presets, a second energy dimension next to the caps, on six hardware families | Docker, Go, `kubectl`, `helm`, `jq` |
| [06 Cooling and PUE](https://github.com/joulie-k8s/Joulie/tree/main/examples/06-cooling-pue) | Cooling models and the PUE they give for a power trace, with a Modelica FMU | Python, Docker |

Then install Joulie on your cluster with the [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}). The [experiments]({{< relref "/docs/experiments/_index.md" >}}) measure what Joulie saves at scale.
