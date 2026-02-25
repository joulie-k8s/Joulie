# Example: stress-ng CPU throttling via Joulie

This toy example shows a practical flow where a CPU-heavy pod is slowed down after lowering node package power cap with Joulie.

## Goal

- Run one `stress-ng` pod on a selected AMD worker node.
- Start with a higher power cap.
- Lower the cap.
- Observe lower `stress-ng` throughput and verify the enforced value on the node.

## Prerequisites

- Joulie CRD + DaemonSet already deployed.
- Node Feature Discovery (NFD) deployed.
- At least one AMD worker node exposing `/sys/class/powercap`.

## 1. Pick and label target node

```bash
kubectl get nodes -L feature.node.kubernetes.io/cpu-vendor
```

Pick one AMD worker and label it:

```bash
export TARGET_NODE=<amd-worker-node-name>
kubectl label node "$TARGET_NODE" joulie.io/power-demo=target --overwrite
```

## 2. Deploy stress workload

```bash
kubectl apply -f examples/stress-ng-throttling/namespace.yaml
kubectl apply -f examples/stress-ng-throttling/stress-ng-deployment.yaml
```

Wait until running:

```bash
kubectl -n joulie-examples get pod -o wide
```

Make sure the pod is on `TARGET_NODE`.

## 3. Apply high cap baseline

```bash
kubectl apply -f examples/stress-ng-throttling/powerpolicy-high.yaml
```

Watch stress logs for ~1 minute and note throughput values:

```bash
kubectl -n joulie-examples logs -f deploy/stress-ng-demo
```

The pod prints one round every ~25s with `stress-ng --metrics-brief`.

## 4. Reduce cap (throttle)

```bash
kubectl apply -f examples/stress-ng-throttling/powerpolicy-low.yaml
```

Keep watching logs:

```bash
kubectl -n joulie-examples logs -f deploy/stress-ng-demo
```

You should see lower bogo ops/second compared with the high-cap baseline.

## 5. Verify agent enforcement directly

Get the agent pod for the same node:

```bash
AGENT_POD=$(kubectl -n joulie-system get pod \
  -l app.kubernetes.io/name=joulie-agent \
  --field-selector spec.nodeName=$TARGET_NODE \
  -o jsonpath='{.items[0].metadata.name}')

echo "$AGENT_POD"
```

Check agent logs for applied policy lines:

```bash
kubectl -n joulie-system logs "$AGENT_POD" --tail=200 | grep "applied policy="
```

Check powercap files written on host:

```bash
kubectl -n joulie-system exec "$AGENT_POD" -- sh -c \
  'for f in /host-sys/class/powercap/*/constraint_0_power_limit_uw /host-sys/class/powercap/*:*/constraint_0_power_limit_uw; do [ -f "$f" ] && echo "$f=$(cat $f)"; done'
```

Values are in microwatts (`120000000` = 120W).

## 6. Cleanup

```bash
kubectl delete -f examples/stress-ng-throttling/powerpolicy-low.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/powerpolicy-high.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/stress-ng-deployment.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/namespace.yaml --ignore-not-found
kubectl label node "$TARGET_NODE" joulie.io/power-demo-
```
