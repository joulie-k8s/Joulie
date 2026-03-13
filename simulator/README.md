# Joulie Workload and Power Simulator

This directory contains the simulator runtime used to test Joulie in virtual environments.

## Why this exists

The simulator lets you run Joulie without host RAPL/DVFS interfaces while preserving real Kubernetes scheduling behavior.

- Kubernetes still schedules real pods on real nodes.
- Simulator converts pod placement/load into synthetic node telemetry.
- Joulie agent reads/writes via HTTP interfaces configured with `TelemetryProfile`.

GPU support is validated in simulator mode only (no bare-metal GPU validation yet).

Current scope includes CPU and GPU-cap simulation with trace-driven completion slowdown.

## Components

- `cmd/simulator/main.go`: HTTP simulator server
- `pkg/hw/profile.go`: hardware profile schema + validation
- `Dockerfile`: build `joulie-simulator` image
- `deploy/simulator.yaml`: deployment + service + RBAC
- `deploy/servicemonitor.yaml`: optional Prometheus scraping
- `config/node-classes.yaml`: sample class mapping by node labels
- `cmd/workloadgen`: synthetic trace generator (`distribution -> trace`)
- `cmd/traceextract`: trace normalizer/extractor helper (`input telemetry/export -> trace schema`)
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

- `examples/05-simulated-telemetry-control/README.md`
- `examples/07 - simulator-gpu-powercaps/README.md`
- `https://joulie-k8s.github.io/Joulie/docs/simulator/simulator/`

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
      pMaxW: 420
      alphaUtil: 1.1
      betaFreq: 1.25
      fMinMHz: 1200
      fMaxMHz: 3200
      raplCapMinW: 70
      raplCapMaxW: 600
      dvfsRampMs: 400
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
- `pMaxW`: max package power at full load/frequency.
- `alphaUtil`: utilization non-linearity exponent.
- `betaFreq`: frequency non-linearity exponent.
- `fMinMHz`,`fMaxMHz`: frequency bounds to derive minimum frequency scale.
- `raplCapMinW`,`raplCapMaxW`: cap guardrails.
- `dvfsRampMs`: throttle-to-frequency ramp time constant.

## Trace-Driven Batch Workload

Set `SIM_WORKLOAD_TRACE_PATH` to a JSONL trace file. The simulator will:

- load `type=job` records,
- create workload Pods over time,
- advance per-job progress based on node effective speed,
- delete Pods when work completes.

Minimal job record example:

```json
{"type":"job","schemaVersion":"v1","jobId":"job-1","submitTimeOffsetSec":2,"namespace":"default","podTemplate":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"joulie.io/power-profile","operator":"NotIn","values":["eco"]}]}]}}},"requests":{"cpu":"4","memory":"1Gi"}},"work":{"cpuUnits":1200},"sensitivity":{"cpu":1.0},"workloadClass":{"cpu":"cpu.compute_bound"},"workloadProfile":{"cpuUtilization":0.95,"memoryIntensity":0.15,"ioIntensity":0.05}}
```

Optional `workloadProfile` fields make the physical model more explicit:

- `cpuUtilization`: average CPU utilization target for the job
- `gpuUtilization`: average GPU utilization target for GPU jobs
- `memoryIntensity`: how strongly memory effects dominate slowdown behavior
- `ioIntensity`: how IO-bound the job is
- `cpuFeedIntensityGpu`: how strongly GPU throughput depends on CPU-side feeding

Optional `workloadClass` fields control the coarse workload family used by the throttling model:

- CPU: `cpu.compute_bound`, `cpu.memory_bound`, `cpu.io_bound`, `cpu.mixed`
- GPU: `gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound`, `gpu.mixed`
