# Example: stress-ng CPU throttling via Joulie

This toy example shows a practical flow where a CPU-heavy pod is slowed down after lowering node power budget with Joulie.

On this platform, package power caps via writable RAPL limit files may be unavailable. In that case Joulie falls back to a DVFS controller (frequency caps) and logs a warning.

## Goal

- Run one `stress-ng` pod on a selected AMD worker node.
- Start with a higher policy.
- Lower the policy.
- Observe lower `stress-ng` throughput and verify DVFS throttling on host.

## Prerequisites

- Joulie CRD + DaemonSet deployed.
- NFD deployed.
- One worker node labeled with:
  - `feature.node.kubernetes.io/cpu-model.vendor_id=AMD`

## 1. Pick and label target node

```bash
kubectl get nodes -L feature.node.kubernetes.io/cpu-model.vendor_id
export TARGET_NODE=<amd-worker-node-name>
kubectl label node "$TARGET_NODE" joulie.io/power-demo=target --overwrite
```

## 2. Deploy stress workload

```bash
kubectl apply -f examples/stress-ng-throttling/namespace.yaml
kubectl apply -f examples/stress-ng-throttling/stress-ng-deployment.yaml
kubectl -n joulie-examples get pod -o wide
```

Make sure the pod runs on `TARGET_NODE`.

## 3. Apply high baseline policy

```bash
kubectl apply -f examples/stress-ng-throttling/powerpolicy-high.yaml
kubectl -n joulie-examples logs -f deploy/stress-ng-demo
```

The workload prints `stress-ng --metrics-brief` every ~25s.

## 4. Apply low policy (throttle)

```bash
kubectl apply -f examples/stress-ng-throttling/powerpolicy-low.yaml
kubectl -n joulie-examples logs -f deploy/stress-ng-demo
```

Expect lower `bogo ops/s` compared to the high policy.

## 5. Verify Joulie action

Get the agent pod on the same node:

```bash
AGENT_POD=$(kubectl -n joulie-system get pod \
  -l app.kubernetes.io/name=joulie-agent \
  --field-selector spec.nodeName=$TARGET_NODE \
  -o jsonpath='{.items[0].metadata.name}')

echo "$AGENT_POD"
```

Watch enforcement logs:

```bash
kubectl -n joulie-system logs -f "$AGENT_POD" | egrep 'warning: RAPL|dvfs-control|throttle-up|throttle-down|action='
```

## 6. Verify on the host (recommended)

SSH to `TARGET_NODE` and watch current/max frequency per policy domain:

```bash
watch -n 1 '
for d in /sys/devices/system/cpu/cpufreq/policy*; do
  [ -d "$d" ] || continue
  p=$(basename "$d")
  cur=$(cat "$d/scaling_cur_freq" 2>/dev/null || echo 0)
  max=$(cat "$d/scaling_max_freq" 2>/dev/null || echo 0)
  printf "%-10s cur=%8.3f MHz  max=%8.3f MHz\n" "$p" "$(awk "BEGIN{print $cur/1000}")" "$(awk "BEGIN{print $max/1000}")"
done | sort -V
'
```

When throttling kicks in, some/all `max` values are reduced (for example to `1500 MHz`).

## 7. Behavior after stopping stress-ng

If you kill the stress workload while low policy is still applied, frequencies may stay low for a while. This is expected: the controller uses smoothing + hysteresis + cooldown.

To bring frequencies up again, apply a less restrictive policy (or high policy):

```bash
kubectl apply -f examples/stress-ng-throttling/powerpolicy-high.yaml
```

Then monitor agent logs for `throttle-down` steps (meaning less throttling), and monitor host `scaling_max_freq` values rising.

If you want immediate host reset, use:

```bash
sudo /home/mbunino2/code/Joulie/scripts/restore-cpufreq.sh
```

## 8. Cleanup

```bash
kubectl delete -f examples/stress-ng-throttling/powerpolicy-low.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/powerpolicy-high.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/stress-ng-deployment.yaml --ignore-not-found
kubectl delete -f examples/stress-ng-throttling/namespace.yaml --ignore-not-found
kubectl label node "$TARGET_NODE" joulie.io/power-demo-
```
