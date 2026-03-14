# Example: Scheduler Steering

This example demonstrates how the Joulie scheduler extender steers workload
placement based on node power profiles and twin state.

## Prerequisites

- Kubernetes cluster with Joulie installed (`helm install joulie charts/joulie`)
- Joulie scheduler extender deployed (see below)

## What this example shows

1. The scheduler extender filters out eco nodes for performance workloads
2. Best-effort workloads are allowed on eco nodes
3. Node scoring favors nodes with high power headroom and low thermal stress

## Setup

### 1. Deploy the scheduler extender

```bash
kubectl apply -f scheduler-extender-deployment.yaml
kubectl apply -f scheduler-extender-config.yaml
```

### 2. Apply NodeTwinState fixtures

For demonstration, apply a pre-configured NodeTwinState:

```bash
kubectl apply -f nodetwinstate-fixture.yaml
```

### 3. Submit workloads

```bash
# Performance workload: will avoid eco nodes
kubectl apply -f performance-pod.yaml

# Best-effort workload: can run on eco nodes
kubectl apply -f best-effort-pod.yaml
```

### 4. Observe placement

```bash
kubectl get pods -o wide
kubectl describe pod performance-app | grep Node:
```

## Cleanup

```bash
kubectl delete -f .
```
