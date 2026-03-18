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

## Install from release (recommended)

The simulator has its own Helm chart, published to the CERN OCI registry on every release:

```bash
helm upgrade --install joulie-simulator oci://registry.cern.ch/mbunino/joulie/joulie-sim \
  --version <version> \
  -n joulie-sim-demo \
  --create-namespace \
  --set image.tag=<version>
```

Or use the make target (uses the local chart source):

```bash
make simulator-install TAG=<version>
```

To uninstall:

```bash
make simulator-uninstall
```

## Build from source (when developing)

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

Then install using the local chart:

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
  - path to YAML with label-matched model overrides.

The preferred bootstrap flow is now:

1. node labels define CPU/GPU hardware identity,
2. shared inventory resolves those labels into a hardware model,
3. `SIM_NODE_CLASS_CONFIG` optionally overrides or refines profile parameters for a scenario.

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

The simulator no longer uses the older "power = baseIdle + runningPods * podW - throttle * k" approximation as its main runtime model.

Today the runtime model combines:

- utilization-dependent CPU/GPU power,
- workload-boundness signals (`memoryIntensity`, `ioIntensity`, `cpuFeedIntensityGpu`),
- CPU and GPU cap-settling dynamics,
- exported telemetry averaging windows,
- thermal state and thermal-throttle penalties.

Important hardware-profile parameters include:

- `baseIdleW`, `pMaxW`
- `alphaUtil`, `betaFreq`
- `fMinMHz`, `fMaxMHz`
- `raplCapMinW`, `raplCapMaxW`
- `dvfsRampMs`
- `cpuCapApplyTauMs`
- `cpuTelemetryWindowMs`
- `cpuAmbientTempC`, `cpuThermalTauMs`, `cpuWattsPerDeltaC`
- `cpuThermalThrottleStartC`, `cpuThermalThrottleFullC`
- `gpu.capApplyTauMs`
- `gpu.telemetryWindowMs`
- `gpu.ambientTempC`, `gpu.thermalTauMs`, `gpu.wattsPerDeltaC`
- `gpu.thermalThrottleStartC`, `gpu.thermalThrottleFullC`

Use the website docs as source of truth for the full model:

- `/docs/hardware/hardware-modeling/`
- `/docs/simulator/workload-generation/`
- `/docs/simulator/workload-simulator/`
- `/docs/simulator/power-simulator/`

## Trace-Driven Batch Workload

Set `SIM_WORKLOAD_TRACE_PATH` to a JSONL trace file. The simulator will:

- load `type=job` records,
- create workload Pods over time,
- advance per-job progress based on node effective speed,
- delete Pods when work completes.

The workload generator can also emit `type=workload` metadata records.
The simulator currently ignores those metadata records and consumes the expanded `type=job`
records directly.

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

These fields are consumed directly by the simulator runtime. They are not just annotations:

- they influence power,
- they influence slowdown under capping/throttling,
- and they influence how node-level utilization and bottleneck mix are aggregated over time.

Optional `workloadClass` fields control the coarse workload family used by the throttling model:

- CPU: `cpu.compute_bound`, `cpu.memory_bound`, `cpu.io_bound`, `cpu.mixed`
- GPU: `gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound`, `gpu.mixed`

The current generator is no longer just a flat single-job sampler.
It can emit:

- logical workload metadata (`type=workload`),
- pod-expanded distributed-training/HPO structures,
- gang metadata for multi-pod training workloads,
- workload families seeded from public AI-cluster traces.

See `/docs/simulator/workload-generation/` for the full generation model and references.
