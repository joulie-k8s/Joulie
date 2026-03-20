---
title: "Installation"
weight: 1
---

This page covers how to install the Joulie simulator in a Kubernetes cluster.

## Prerequisites

- A running Kubernetes cluster (real or [kind](https://kind.sigs.k8s.io/) for local development)
- `kubectl` configured for the target cluster
- `helm` v3+ (for Helm installation)

## Install via Helm (recommended)

The simulator is published as an OCI Helm chart. Install it with:

```bash
helm install joulie-sim oci://registry.cern.ch/mbunino/joulie/joulie-sim \
  -n joulie-system --create-namespace
```

To customize values, download the default values first:

```bash
helm show values oci://registry.cern.ch/mbunino/joulie/joulie-sim > values.yaml
```

Then install with overrides:

```bash
helm install joulie-sim oci://registry.cern.ch/mbunino/joulie/joulie-sim \
  -n joulie-system --create-namespace \
  -f values.yaml
```

To upgrade an existing release:

```bash
helm upgrade joulie-sim oci://registry.cern.ch/mbunino/joulie/joulie-sim \
  -n joulie-system
```

To uninstall:

```bash
helm uninstall joulie-sim -n joulie-system
```

## Install from source (for developers)

If you are developing the simulator or need a custom build, you can build and deploy from the repository.

### Build and push the image

From the repo root:

```bash
make simulator-build TAG=<tag>
make simulator-push TAG=<tag>
```

### Deploy with raw manifests

```bash
kubectl apply -f simulator/deploy/simulator.yaml
```

Or use the Makefile shortcut with a custom image tag:

```bash
make simulator-install TAG=<tag>
```

### Deploy with the local Helm chart

You can also install from the chart source in the repository:

```bash
helm install joulie-sim ./charts/joulie-simulator \
  -n joulie-system --create-namespace \
  --set image.tag=<tag>
```

## Verify the installation

After installation, check that the simulator is running correctly.

### Check pod status

```bash
kubectl -n joulie-system get pods -l app.kubernetes.io/name=joulie-simulator
```

All pods should be in `Running` state with `1/1` ready.

### Check rollout status

```bash
kubectl -n joulie-system rollout status deploy/joulie-simulator
```

### View logs

```bash
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-simulator --tail=50
```

Look for startup messages confirming the simulator has loaded its configuration and is listening.

### Health check

Port-forward and hit the health endpoint:

```bash
kubectl -n joulie-system port-forward svc/joulie-simulator 18080:18080 &
curl -s http://localhost:18080/healthz
```

A healthy simulator returns HTTP 200.

### Check simulated nodes

Once the simulator is running and nodes are labeled with `joulie.io/managed=true`, verify it has discovered them:

```bash
curl -s http://localhost:18080/debug/nodes | jq
```

### Prometheus metrics

The simulator exposes Prometheus metrics at `/metrics`:

```bash
curl -s http://localhost:18080/metrics | head -30
```

See [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}) for the full metric reference.
