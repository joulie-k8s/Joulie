+++
title = "Quickstart"
linkTitle = "Quickstart"
slug = "quickstart"
weight = 2
+++

Install Joulie on a cluster and check that it manages your nodes. New to Joulie? Read [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}}) first, or try it with only Docker in the [Tutorials]({{< relref "/docs/getting-started/04-tutorials.md" >}}).

## Prerequisites

- Kubernetes with Node Feature Discovery (NFD) deployed.
- Prometheus at `http://prometheus-operated.monitoring:9090` with its OTLP receiver on (`--web.enable-otlp-receiver`), receiving `hw.energy` or `hw.power` for each node. `values/prometheus-grafana.yaml` configures kube-prometheus-stack this way ([Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}})), and [`examples/04-otel-collector-bridge`](https://github.com/joulie-k8s/Joulie/tree/main/examples/04-otel-collector-bridge) converts Kepler, dcgm-exporter and AMD device-metrics-exporter metrics into those names.
- Without that data every node reads 0 W (source `prometheus-no-data`). To run without Prometheus, set `controllerManager.env.NODE_POWER_SOURCE=static`.
- Optional, for real enforcement: writable RAPL power limit files or cpufreq sysfs on the nodes, and NVML for NVIDIA GPUs.

## Install

```bash
helm upgrade --install joulie oci://registry.cern.ch/joulie/joulie \
  --version <version> \
  -n joulie-system \
  --create-namespace \
  -f values/joulie.yaml
```

Upgrading an existing release? `helm upgrade` never updates CRDs: apply the new ones first, as [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}) shows.

## Label the nodes to manage

Joulie manages only the nodes labelled `joulie.io/managed=true` (controller manager `NODE_SELECTOR`), and runs its agent only there; the install selects none by itself.

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite
```

## Verify

```bash
kubectl get nodehardwares,nodetwins
kubectl -n joulie-system logs deploy/joulie-controller-manager --tail=100
```

Each labelled node gets a `NodeHardware` and a `NodeTwin`. If none appear and the log shows `no eligible nodes matched selector`, check the labels with `kubectl get nodes --show-labels | grep 'joulie.io/managed=true'`. Then run `kubectl joulie` (below) for the per-node power, caps and twin scores.

## kubectl plugin

`kubectl joulie` shows the energy state of the cluster in the terminal: the power each node draws against its cap, its power profile, the caps its agent applied, the digital twin's scores, and where performance and standard pods run.

{{< img src="images/kubectl-joulie.gif" alt="kubectl joulie: status, node and the live dashboard on a simulated 10-node cluster" >}}

```bash
kubectl krew install joulie
kubectl joulie                 # the cluster summary and one row per managed node
kubectl joulie top             # the live dashboard
kubectl joulie node <node>     # one node: hardware, power, twin, applied caps, pods
```

`kubectl krew upgrade joulie` updates it. Every command and flag is in the [configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md#kubectl-plugin" >}}).

## Next steps

- [Tutorials]({{< relref "/docs/getting-started/04-tutorials.md" >}}): a simulated cluster with only Docker.
- [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}}): mark performance pods.
- [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}}): every setting.
- [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}): what to check before upgrading, including the CRDs.

Installing from source: see [`CONTRIBUTING.md`](https://github.com/joulie-k8s/Joulie/blob/main/CONTRIBUTING.md#local-development).
