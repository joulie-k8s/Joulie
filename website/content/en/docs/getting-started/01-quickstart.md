+++
title = "Quickstart"
linkTitle = "Quickstart"
slug = "quickstart"
weight = 2
+++

This page is the fastest path to run Joulie.
For conceptual context first, read [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}}).

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
By default, the agent DaemonSet uses the same selector scope (`joulie.io/managed=true`), so unlabeled nodes will not run agent pods.

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

## Joulie operator and agents

Joulie control is a desired-state loop:

- agent discovers per-node hardware and publishes `NodeHardware`,
- operator resolves hardware/inventory and decides per-node target state,
- operator writes that desired state as `NodePowerProfile`,
- node labels (`joulie.io/power-profile`) expose current supply to the scheduler,
- agent enforces node-local controls from desired state and telemetry/control profile.

Meaning of the key node labels:

- `joulie.io/managed=true`
  - node is in Joulie operator scope (eligible for policy decisions)
- `joulie.io/power-profile=performance`
  - node currently offers high-performance supply
- `joulie.io/power-profile=eco`
  - node currently offers low-power supply (resources are throttled according to the selected policy)
- `joulie.io/draining=true|false`
  - transition flag used while moving toward eco under safeguards

Read the architecture path after this quickstart:

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

Architecture overview:

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

### Central operator mode

The operator continuously writes `NodePowerProfile` assignments from the active policy (for example static partition or queue-aware), mapping desired states to node profiles (`performance`/`eco`).

Configuration details:

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Operator]({{< relref "/docs/architecture/operator.md" >}})
- Optional runnable manifest walkthrough:
  - [Operator Configuration Example](https://github.com/joulie-k8s/Joulie/tree/main/examples/04-operator-configuration/README.md)

Verify:

```bash
kubectl get nodehardwares
kubectl get nodepowerprofiles
kubectl -n joulie-system logs deploy/joulie-operator --tail=100
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

Look for:

- `NodeHardware` objects appearing for managed nodes,
- operator logs mentioning inventory-aware planning or desired-state assignment,
- agent logs containing desired-state source and enforcement/fallback actions.

Also verify installed images:

```bash
kubectl -n joulie-system get pods \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,IMAGEID:.status.containerStatuses[0].imageID
```

If operator logs show `no eligible nodes matched selector`, verify node labels:

```bash
kubectl get nodes --show-labels | grep 'joulie.io/managed=true'
```

## Workload and Power Simulator

For fake-node workload + power simulation (real scheduler, fake [KWOK](https://kwok.sigs.k8s.io/) nodes, real operator, agent pool mode), see:

- [Simulator Overview]({{< relref "/docs/simulator/simulator.md" >}})
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
- [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})
- [KWOK Simulator Example](https://github.com/joulie-k8s/Joulie/tree/main/examples/06-simulator-kwok)

## Next step

Continue with:

1. [Pod Compatibility for Joulie]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
2. [Agent Runtime Modes]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}})
