# Quickstart

## Prerequisites

- Kubernetes cluster with worker nodes
- Node Feature Discovery (NFD) deployed
- Access to push images to `registry.cern.ch/mbunino/joulie`
- Optional for real enforcement: nodes exposing writable power interfaces
  - RAPL power limit files, or
  - cpufreq sysfs interfaces

## 1. Build and push image

If you changed source code, from repo root:

```bash
make build-push TAG=<tag>
```

This pushes:

- `registry.cern.ch/mbunino/joulie/joulie-agent:<tag>`
- `registry.cern.ch/mbunino/joulie/joulie-operator:<tag>`

You can also do build+push+install in one command:

```bash
make build-push-install TAG=<tag>
```

If you use `make build-push-install`, you can skip step 2.

## 2. Install CRDs + components

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

Important: by default, install does not auto-select nodes.
Default expected selector value is:

- `joulie.io/managed=true` (operator env `NODE_SELECTOR`)

So you must label target worker nodes, for example:

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite
```

If this is missing, operator logs will show `no eligible nodes matched selector`.

To remove Joulie components and CRD:

```bash
make uninstall
```

## 3. Label nodes managed by the operator

The default operator selector in Helm values (`values/joulie.yaml`) is:

- `NODE_SELECTOR=joulie.io/managed=true`

Label the nodes you want managed:

```bash
kubectl label node <node1> joulie.io/managed=true --overwrite
kubectl label node <node2> joulie.io/managed=true --overwrite
```

## 4. Update to a new image tag later

```bash
make rollout TAG=<new-tag>
```

## 5. Control mode

### Central operator mode (single path)

The operator writes `NodePowerProfile` assignments and swaps `ActivePerformance`/`ActiveEco` across nodes every reconcile interval (profile mapping `performance`/`eco`).

Configuration details and patch examples:

- [Operator Configuration Example](../examples/operator-configuration/README.md)

Verify:

```bash
kubectl get nodepowerprofiles
kubectl -n joulie-system logs deploy/joulie-operator --tail=100
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

## 6. Verify

```bash
kubectl get nodepowerprofiles
kubectl -n joulie-system get pods -o wide
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

Look for log lines containing desired-state source and enforcement/fallback actions.

If operator logs show `no eligible nodes matched selector`, verify node labels:

```bash
kubectl get nodes --show-labels | grep 'joulie.io/managed=true'
```
