# Example: Simulator GPU Power Caps (Heterogeneous)

This example validates GPU capping end-to-end in simulator mode on a small heterogeneous virtual cluster:

- one NVIDIA fake node,
- one AMD fake node,
- one CPU-only fake node.

It demonstrates that tighter GPU caps reduce simulated GPU power and increase completion time for GPU jobs.

## Important validation note

GPU support has been validated in simulator mode only (no bare-metal GPU access yet).
The host code paths are designed for NVIDIA/AMD bare metal, but this example is simulator-first.

## Files

- `manifests/00-kwok-nodes.yaml`: heterogeneous fake nodes (NVIDIA + AMD + CPU-only)
- `manifests/10-node-classes.yaml`: simulator hardware classes with GPU model caps
- `manifests/20-simulator.yaml`: simulator deployment using class config + GPU trace
- `manifests/30-joulie-values-pool.yaml`: Joulie Helm values for pool mode + GPU cap policy
- `manifests/40-telemetryprofile-template.yaml`: node-scoped HTTP telemetry/control profile template
- `manifests/50-workload-trace-configmap.yaml`: mixed CPU/GPU workload trace

## Run

1. Create a kind + KWOK environment (same as `examples/06-simulator-kwok`).
2. Apply fake nodes and simulator stack:

```bash
kubectl apply -f manifests/00-kwok-nodes.yaml
kubectl apply -f manifests/10-node-classes.yaml
kubectl apply -f manifests/50-workload-trace-configmap.yaml
kubectl apply -f manifests/20-simulator.yaml
```

3. Install Joulie in pool mode:

```bash
helm upgrade --install joulie ../../charts/joulie \
  -n joulie-system --create-namespace \
  -f manifests/30-joulie-values-pool.yaml
```

4. Create one TelemetryProfile per managed node:

```bash
for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/TARGET_NODE/$n/g" manifests/40-telemetryprofile-template.yaml | kubectl apply -f -
done
```

5. Observe control + energy behavior:

```bash
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=200 | egrep 'gpu|dvfs|policy'
kubectl -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=200 | egrep 'gpu|control node=|job completed'
```

6. Compare profiles:

- performance GPU cap (`GPU_PERFORMANCE_CAP_PCT_OF_MAX=100`) baseline,
- eco GPU cap (`GPU_ECO_CAP_PCT_OF_MAX=60`) reduced power / longer GPU completion.
