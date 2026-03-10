+++
title = "Quickstart"
linkTitle = "Quickstart"
slug = "quickstart"
weight = 1
+++


## Prerequisites

- Kubernetes cluster with worker nodes
- Node Feature Discovery (NFD) deployed
- Optional for real enforcement: nodes exposing writable power interfaces
  - RAPL power limit files, or
  - cpufreq sysfs interfaces

## Install from release (recommended)

Install directly from OCI chart release:

```bash
helm upgrade --install joulie oci://registry.cern.ch/mbunino/joulie/joulie \
  --version <version> \
  -n joulie-system \
  --create-namespace \
  -f values/joulie.yaml
```

### Label nodes managed by the operator

**Important**: Joulie will only target nodes with a specific label, and ignore
all the others. By default, install does not auto-select nodes.
Default expected selector value is:

- `joulie.io/managed=true` (operator env `NODE_SELECTOR`)

So you must label target worker nodes, for example:

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite
```

If this is missing, operator logs will show `no eligible nodes matched selector`.

## Install from source (when developing)

If you changed source code, from repo root:

```bash
make build-push TAG=<tag>
make install TAG=<tag>
```

This pushes:

- `registry.cern.ch/mbunino/joulie/joulie-agent:<tag>`
- `registry.cern.ch/mbunino/joulie/joulie-operator:<tag>`

You can also do build+push+install in one command:

```bash
make build-push-install TAG=<tag>
```

Use `make help` to see all targets.

### Install CRDs + components (source workflow)

If images for `TAG` are already in the registry (no source changes), run:

```bash
make install TAG=<tag>
```

This performs a Helm install/upgrade (`charts/joulie`) and sets both images to the requested tag.
Default values come from:

- `values/joulie.yaml`

You can override with:

```bash
make install TAG=<tag> HELM_VALUES=<path-to-values.yaml>
```

Node selection behavior is the same as release install:

- operator expects `joulie.io/managed=true` by default
- label target nodes before verifying reconciliation

Joulie will only target nodes with that specific label.

To remove Joulie components and CRD:

```bash
make uninstall
```

### Update to a new image tag later

```bash
make rollout TAG=<new-tag>
```

## Control mode

Joulie control is based on a simple desired-state loop:

- the operator computes a per-node desired state and writes it as `NodePowerProfile`,
- nodes are treated as `ActivePerformance` or `ActiveEco` (with optional draining transitions),
- the agent on each node reads desired state plus telemetry/control config and enforces caps/throttling.

See architecture details:

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Operator]({{< relref "/docs/architecture/operator.md" >}})
- [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

Architecture overview:

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

### Central operator mode (single path)

The operator writes `NodePowerProfile` assignments and swaps `ActivePerformance`/`ActiveEco` across nodes every reconcile interval (profile mapping `performance`/`eco`).

Configuration details:

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Operator]({{< relref "/docs/architecture/operator.md" >}})
- Optional runnable manifest walkthrough:
  - [Operator Configuration Example](https://github.com/joulie-k8s/Joulie/tree/main/examples/04-operator-configuration/README.md)

Verify:

```bash
kubectl get nodepowerprofiles
kubectl -n joulie-system logs deploy/joulie-operator --tail=100
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

### Verify

```bash
kubectl get nodepowerprofiles
kubectl -n joulie-system get pods -o wide
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
kubectl -n joulie-system get pods \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,IMAGEID:.status.containerStatuses[0].imageID
```

Look for log lines containing desired-state source and enforcement/fallback actions.

If operator logs show `no eligible nodes matched selector`, verify node labels:

```bash
kubectl get nodes --show-labels | grep 'joulie.io/managed=true'
```

## Workload and Power Simulator

For fake-node workload + power simulation (real scheduler, fake KWOK nodes, real operator, agent pool mode), see:

- [Simulator Overview]({{< relref "/docs/simulator/simulator.md" >}})
- [Simulator Algorithms]({{< relref "/docs/simulator/simulator-algorithms.md" >}})
- [KWOK Simulator Example](https://github.com/joulie-k8s/Joulie/tree/main/examples/06-simulator-kwok)
