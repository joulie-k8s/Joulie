# Quickstart

## Prerequisites

- Kubernetes cluster with worker nodes
- Node Feature Discovery (NFD) deployed
- Access to push images to `registry.cern.ch/mbunino/joulie`
- Nodes where power cap sysfs is exposed under `/sys/class/powercap`

## 1. Build and push image

From repo root:

```bash
make build-push TAG=v0.1.0
```

This pushes:

- `registry.cern.ch/mbunino/joulie/joulie-agent:v0.1.0`

## 2. Set image tag in DaemonSet manifest

Edit `deploy/joulie.yaml` and set:

```yaml
image: registry.cern.ch/mbunino/joulie/joulie-agent:v0.1.0
```

## 3. Install CRD + agent

```bash
kubectl apply -f config/crd/bases/joulie.io_powerpolicies.yaml
kubectl apply -f deploy/joulie.yaml
```

## 4. Apply a policy

```bash
kubectl apply -f config/samples/powerpolicy-amd-worker.yaml
```

## 5. Verify

```bash
kubectl get powerpolicies
kubectl -n joulie-system get pods -o wide
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=100
```

Look for log lines containing `applied policy=`.
