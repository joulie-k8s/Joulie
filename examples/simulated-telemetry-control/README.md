# Example: Simulated Telemetry + Control over HTTP (All Managed Nodes)

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
- Same setup can be applied to all managed worker nodes (not just one node).

## Prerequisites

- Joulie installed.
- At least one worker node.
- Nodes you want to simulate must be labeled as managed (`joulie.io/managed=true` by default).
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

- `NodePowerProfile` sets low cap (`120W`) per managed node.
- `TelemetryProfile` routes for each managed node:
  - input power reads to simulator HTTP (`spec.sources.cpu.http.endpoint`),
  - control writes to simulator HTTP (`spec.controls.cpu.http.endpoint`).

Agent usage:

1. read local `NodePowerProfile` for desired cap,
2. read matching node-scoped `TelemetryProfile` for source/control routing,
3. execute backend (`rapl` or `dvfs`) accordingly.

## 1. Deploy simulator

```bash
kubectl apply -f examples/simulated-telemetry-control/namespace.yaml
make simulator-install TAG=<simulator-tag>
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
kubectl -n joulie-sim-demo get pods,svc,endpoints
```

Enable Prometheus scraping of simulator metrics:

```bash
kubectl apply -f examples/simulated-telemetry-control/servicemonitor-simulator.yaml
```

If your Prometheus release label/namespace differs, adapt `release` and `metadata.namespace`
in the ServiceMonitor.

If you are rebuilding the simulator image locally:

```bash
make simulator-build-push-deploy TAG=<simulator-tag>
```

## 2. Select managed nodes

```bash
kubectl get nodes -l joulie.io/managed=true
```

The simulator already auto-scopes to those nodes via:

- `SIM_NODE_SELECTOR=joulie.io/managed=true`
- optional class mapping from `/etc/joulie-sim/node-classes.yaml`
  (mounted from `ConfigMap` in `simulator/deploy/simulator.yaml`).

## 3. Pre-clean old demo resources (recommended)

This avoids duplicate TelemetryProfiles targeting the same node.

```bash
kubectl delete telemetryprofile sim-http-demo --ignore-not-found
kubectl delete telemetryprofiles -l app.kubernetes.io/part-of=sim-http-demo --ignore-not-found
kubectl delete nodepowerprofiles -l app.kubernetes.io/part-of=sim-http-demo --ignore-not-found
```

## 4. Apply NodePowerProfile on all managed nodes

```bash
for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/target-node/$n/g" examples/simulated-telemetry-control/nodepowerprofile-low.yaml | kubectl apply -f -
done

kubectl get nodepowerprofiles -l app.kubernetes.io/part-of=sim-http-demo -o wide
```

## 5. Apply TelemetryProfile in RAPL mode on all managed nodes

```bash
for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/target-node/$n/g" examples/simulated-telemetry-control/telemetryprofile-http-rapl.yaml | kubectl apply -f -
done

kubectl get telemetryprofiles -l app.kubernetes.io/part-of=sim-http-demo
```

Watch agent logs across nodes:

```bash
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --since=5m | egrep 'desired state|applied policy|dvfs-control|warning'
```

Watch simulator logs for HTTP control and telemetry:

```bash
kubectl -n joulie-sim-demo logs -f deploy/joulie-telemetry-sim
```

Optional: inspect simulator debug endpoints directly:

```bash
kubectl -n joulie-sim-demo port-forward deploy/joulie-telemetry-sim 18080:18080
curl -s localhost:18080/debug/nodes | jq
curl -s localhost:18080/debug/events | jq
```

Optional: Grafana/Prometheus access shortcuts:

```bash
kubectl port-forward svc/telemetry-kube-prometheus-prometheus 9090:9090 1>/dev/null &
kubectl port-forward svc/telemetry-grafana 5000:80 1>/dev/null &
```

You should see lines like:

- `control node=... action=rapl.set_power_cap_watts capW=... throttlePct=... powerW=...`
- `telemetry node=... powerW=... capW=... throttlePct=...`

## 6. Switch all managed nodes to DVFS mode

```bash
for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/target-node/$n/g" examples/simulated-telemetry-control/telemetryprofile-http-dvfs.yaml | kubectl apply -f -
done
```

In simulator logs you should now see `dvfs.set_throttle_pct` POSTs for nodes that are in constrained state.

## 7. Inspect control status in TelemetryProfile CRs

```bash
kubectl get telemetryprofiles -l app.kubernetes.io/part-of=sim-http-demo \
  -o custom-columns=NAME:.metadata.name,NODE:.spec.target.nodeName,BACKEND:.status.control.cpu.backend,RESULT:.status.control.cpu.result,UPDATED:.status.control.cpu.updatedAt
```

## 8. Grafana dashboard (simulator loop)

Import this dashboard JSON manually in Grafana:

- `examples/simulated-telemetry-control/dashboard-simulated-telemetry.json`

It shows:

- simulated power vs cap per node,
- throttle and running pods per node,
- control action rate (`rapl.*`, `dvfs.*`),
- class assignment per node (`joulie_sim_node_class_info`),
- simulator HTTP request rate.

## 9. Cleanup

```bash
kubectl delete telemetryprofiles -l app.kubernetes.io/part-of=sim-http-demo --ignore-not-found
kubectl delete nodepowerprofiles -l app.kubernetes.io/part-of=sim-http-demo --ignore-not-found
kubectl delete -f examples/simulated-telemetry-control/servicemonitor-simulator.yaml --ignore-not-found
kubectl delete -f simulator/deploy/simulator.yaml --ignore-not-found
kubectl delete -f examples/simulated-telemetry-control/namespace.yaml --ignore-not-found
```
