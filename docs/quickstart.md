# Quickstart

## Prerequisites

- Kubernetes cluster with worker nodes
- Node Feature Discovery (NFD) deployed
- Access to push images to `registry.cern.ch/mbunino/joulie`
- Optional for real enforcement: nodes exposing writable power interfaces
  - RAPL power limit files, or
  - cpufreq sysfs interfaces

## 1. Build and push image

From repo root:

```bash
make build-push TAG=0.0.7
```

This pushes:

- `registry.cern.ch/mbunino/joulie/joulie-agent:0.0.7`
- `registry.cern.ch/mbunino/joulie/joulie-operator:0.0.7`

## 2. Install CRDs + components

```bash
make install TAG=0.0.7
```

This applies CRDs/manifests and sets both images to the requested tag.

## 3. Label nodes managed by the operator

The default operator selector in `deploy/joulie.yaml` is:

- `NODE_SELECTOR=joulie.io/managed=true`

Label the nodes you want managed:

```bash
kubectl label node <node1> joulie.io/managed=true --overwrite
kubectl label node <node2> joulie.io/managed=true --overwrite
```

## 4. Update to a new image tag later

```bash
make rollout TAG=0.0.7
```

## 5. Choose a control mode

### A) Central operator mode (recommended first test)

The operator writes `NodePowerProfile` assignments and swaps `eco`/`performance` across nodes every reconcile interval.

Verify:

```bash
kubectl get nodepowerprofiles
kubectl -n joulie-system logs deploy/joulie-operator --tail=100
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

### B) Legacy direct policy mode

```bash
kubectl apply -f config/samples/powerpolicy-amd-worker.yaml
```

## 6. Verify

```bash
kubectl get powerpolicies
kubectl get nodepowerprofiles
kubectl -n joulie-system get pods -o wide
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

Look for log lines containing desired-state source and enforcement/fallback actions.

If operator logs show `no eligible nodes matched selector`, verify node labels:

```bash
kubectl get nodes --show-labels | grep 'joulie.io/managed=true'
```
