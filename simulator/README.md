# Joulie Simulator

This directory contains the simulator runtime used to test Joulie in virtual environments.

## Why this exists

The simulator lets you run Joulie without host RAPL/DVFS interfaces while preserving real Kubernetes scheduling behavior.

- Kubernetes still schedules real pods on real nodes.
- Simulator converts pod placement/load into synthetic node telemetry.
- Joulie agent reads/writes via HTTP interfaces configured with `TelemetryProfile`.

Planned extension path:

- scenario-driven cluster bootstrap (including fake GPU resources),
- synthetic/replayed workload generation,
- batch progress model where throttling increases job completion time.

## Components

- `cmd/simulator/main.go`: HTTP simulator server
- `Dockerfile`: build `joulie-simulator` image
- `deploy/simulator.yaml`: deployment + service + RBAC
- `deploy/servicemonitor.yaml`: optional Prometheus scraping
- `config/node-classes.yaml`: sample class mapping by node labels
- `kind-small.yaml`: optional local kind cluster config
- `waok8s/`: external WAO code reference sandbox

## Build

From repo root:

```bash
docker build -f simulator/Dockerfile -t registry.cern.ch/mbunino/joulie/joulie-simulator:latest .
docker push registry.cern.ch/mbunino/joulie/joulie-simulator:latest
```

Or use make targets:

```bash
make simulator-build TAG=<tag>
make simulator-push TAG=<tag>
```

## Deploy

```bash
kubectl apply -f simulator/deploy/simulator.yaml
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
kubectl -n joulie-sim-demo logs -f deploy/joulie-telemetry-sim
```

With dynamic image tag override:

```bash
make simulator-install TAG=<tag>
```

Build + push + install in one command:

```bash
make simulator-build-push-deploy TAG=<tag>
```

## Observe Simulated Values

Port-forward simulator:

```bash
kubectl -n joulie-sim-demo port-forward deploy/joulie-telemetry-sim 18080:18080
```

Inspect current per-node simulated state and class mapping:

```bash
curl -s localhost:18080/debug/nodes | jq
```

Inspect recent telemetry/control events (ring buffer):

```bash
curl -s localhost:18080/debug/events | jq
```

Inspect Prometheus metrics exposed by simulator:

```bash
curl -s localhost:18080/metrics | egrep 'joulie_sim_node_(power_watts|cap_watts|throttle_pct|running_pods|class_info)'
curl -s localhost:18080/metrics | egrep 'joulie_sim_controls_total|joulie_sim_requests_total'
```

## Use with Joulie

See:

- `examples/simulated-telemetry-control/README.md`
- `docs/simulator.md`

## Node Discovery and Class Mapping

The simulator can auto-scope and auto-map nodes:

- `SIM_NODE_SELECTOR`:
  - limits simulated nodes (default in deploy manifest: `joulie.io/managed=true`)
- `SIM_NODE_CLASS_CONFIG`:
  - path to YAML with class rules (`matchLabels`) and model overrides.

Class config example:

```yaml
classes:
  - name: intel-default
    matchLabels:
      feature.node.kubernetes.io/cpu-model.vendor_id: Intel
    model:
      baseIdleW: 70
      podW: 110
      dvfsDropWPerPct: 1.6
      defaultCapW: 5000
```

### Model parameters

The simulator computes node power with:

`power = baseIdleW + (runningPods * podW) - (throttlePct * dvfsDropWPerPct)`

then clamps to `[20W, capWatts + raplHeadW]`.

- `baseIdleW`: baseline node power at zero load.
- `podW`: added watts per running pod on that node.
- `dvfsDropWPerPct`: watts removed per DVFS throttle percent point.
- `raplHeadW`: allowed overshoot above cap (`capWatts`) before clamp.
- `defaultCapW`: initial cap for nodes before any control action.
