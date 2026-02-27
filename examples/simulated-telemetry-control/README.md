# Example: Simulated Telemetry + Control over HTTP

This example shows Joulie using HTTP for both:

- telemetry input (`cpu.packagePowerWatts`), and
- control output (`rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`).

It is useful on virtual clusters where host `/sys` power interfaces are unavailable.

## Why DVFS is a percentage here

In Joulie today, DVFS control is modeled as `throttlePct` (0-100), not as an absolute Hz target.

- On real host mode, the agent translates this percentage to concrete `scaling_max_freq` writes per CPU/policy.
- On HTTP simulated mode, the same percentage is sent as a control intent and the simulator decides how to map it to synthetic power/frequency behavior.

Why this is practical:

- CPU frequency domains and available steps differ by hardware.
- A normalized percentage keeps one portable control signal across Intel/AMD and simulated nodes.
- Absolute Hz is still represented on real nodes via observed metrics (`*_cpu_max_freq_khz`, `*_cpu_cur_freq_khz`).

## What this demonstrates

- `TelemetryProfile.spec.sources.cpu.type=http`: agent reads observed power from HTTP.
- `TelemetryProfile.spec.controls.cpu.type=http`: agent sends control intents to HTTP.
- Two control modes:
  - `mode: rapl` -> agent sends RAPL-like cap command over HTTP.
  - `mode: dvfs` -> agent uses DVFS loop and sends throttle percentage over HTTP.

## Prerequisites

- Joulie installed.
- At least one worker node.
- No extra env var is required for virtual nodes.
  Agent auto-detects whether host cpufreq files exist:
  - if present, host DVFS writes are available;
  - if missing, host DVFS writes are disabled;
  - DVFS still works when control is routed to HTTP (`spec.controls.cpu.type=http`).

## NodePowerProfile vs TelemetryProfile

Both are needed and they have different roles:

- `NodePowerProfile`: desired power target for a node (what to enforce).
- `TelemetryProfile`: where to read inputs and where to send control intents (how to observe/actuate).

In this example:

- `NodePowerProfile` sets low cap (`120W`) for `TARGET_NODE`.
- `TelemetryProfile` routes:
  - input power reads to simulator HTTP (`spec.sources.cpu.http.endpoint`),
  - control writes to simulator HTTP (`spec.controls.cpu.http.endpoint`).

Agent usage:

1. read local `NodePowerProfile` for desired cap,
2. read matching node-scoped `TelemetryProfile` for source/control routing,
3. execute backend (`rapl` or `dvfs`) accordingly.

## 1. Deploy simulator

```bash
kubectl apply -f examples/simulated-telemetry-control/namespace.yaml
kubectl apply -f examples/simulated-telemetry-control/telemetry-simulator.yaml
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
kubectl -n joulie-sim-demo get pods,svc,endpoints
```

## 2. Select a target node

```bash
export TARGET_NODE=<worker-node-name>
```

## 3. Apply node power profile

```bash
sed "s/__TARGET_NODE__/$TARGET_NODE/" examples/simulated-telemetry-control/nodepowerprofile-low.yaml | kubectl apply -f -
```

## 4. Test RAPL-like HTTP control mode

```bash
sed "s/__TARGET_NODE__/$TARGET_NODE/" examples/simulated-telemetry-control/telemetryprofile-http-rapl.yaml | kubectl apply -f -
```

Watch agent logs on that node:

```bash
AGENT_POD=$(kubectl -n joulie-system get pod -l app.kubernetes.io/name=joulie-agent --field-selector spec.nodeName=$TARGET_NODE -o jsonpath='{.items[0].metadata.name}')
kubectl -n joulie-system logs -f "$AGENT_POD" | egrep 'desired state|applied policy|dvfs-control|warning'
```

Watch simulator logs for HTTP control calls:

```bash
kubectl -n joulie-sim-demo logs -f deploy/joulie-telemetry-sim
```

You should now see detailed lines like:

- `control node=... action=rapl.set_power_cap_watts capW=... throttlePct=... powerW=...`
- `telemetry node=... powerW=... capW=... throttlePct=...`

## 5. Switch to DVFS HTTP control mode

```bash
sed "s/__TARGET_NODE__/$TARGET_NODE/" examples/simulated-telemetry-control/telemetryprofile-http-dvfs.yaml | kubectl apply -f -
```

In simulator logs you should see `dvfs.set_throttle_pct` POSTs.

## 6. Inspect control status in CRD

```bash
kubectl get telemetryprofile sim-http-demo -o yaml | yq '.status.control.cpu'
```

Without `yq`:

```bash
kubectl get telemetryprofile sim-http-demo -o jsonpath='{.status.control.cpu}{"\n"}'
```

## 7. Cleanup

```bash
kubectl delete telemetryprofile sim-http-demo --ignore-not-found
kubectl delete nodepowerprofile sim-http-demo-profile --ignore-not-found
kubectl delete -f examples/simulated-telemetry-control/telemetry-simulator.yaml --ignore-not-found
kubectl delete -f examples/simulated-telemetry-control/namespace.yaml --ignore-not-found
```
