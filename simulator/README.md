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
- `Dockerfile`: build the `joulie-simulator` image, which also ships `fakesmi` at `/usr/local/lib/joulie/fakesmi` for emulate mode
- `deploy/simulator.yaml`: deployment + service + RBAC
- `deploy/servicemonitor.yaml`: optional Prometheus scraping
- `config/node-classes.yaml`: sample class mapping by node labels
- `cmd/workloadgen`: synthetic trace generator (`distribution -> trace`)
- `cmd/traceextract`: trace normalizer/extractor helper (`input telemetry/export -> trace schema`)
- `pkg/hwemu`: bare-metal node emulator for agent tests: hardware profile schema and loader, a renderer that writes a node's sysfs tree, cpuinfo and GPU tool state, and physics that turns the limits the agent writes into power and energy readings; `pkg/hwemu/profiles/` holds the builtin profiles, each with its hardware in the [simulator docs](../website/content/en/docs/simulator/emulate-mode.md#builtin-hardware-profiles):
  - Intel hosts: `intel-xeon-2s-rapl`, `intel-xeon-6530-2s-rapl`, `intel-xeon-2s-psys`, `intel-xeon-2s-multidie`, `intel-xeon6-2s-no-powercap`, `intel-xeon6-2s-tpmi`
  - AMD hosts: `amd-epyc-2s-energy-only`, `amd-epyc-9534-2s-energy-only`, `amd-epyc-2s-hsmp`
  - GPU nodes: `nvidia-h100-nvl-8gpu`, `nvidia-h100-sxm-8gpu`, `nvidia-h100-sxm-4gpu`, `nvidia-l40s-4gpu`, `amd-instinct-mi300x-8gpu`, `amd-radeon-pro-w7900-4gpu` (power cap pinned at 241 W), `intel-flex-170-4gpu` (i915 hwmon, no GPU tool)
  - a VM: `vm-no-powercap`
- `pkg/hwemu/smi` and `cmd/fakesmi`: fake `nvidia-smi`, `rocm-smi` and `amd-smi` that read and write an emulated node's state (environment in the [configuration reference](../website/content/en/docs/getting-started/05-configuration-reference.md))
- `pkg/hwemu/hwemutest`: test helpers that render a builtin profile into a test's temporary directory
- `pkg/clusterspec` and `cmd/clusterspec`: the cluster spec of emulate mode (`sim.joulie.io/v1alpha1` `ClusterSpec`): schema, validation, expansion into members and Node objects; the command validates a spec, lists its nodes and renders them for `kubectl apply`
- `pkg/emulate`: emulate mode's engine: one `hwemu` node per spec member, stepped on a fixed tick, energy ledgers that survive restarts, immutable snapshots
- `pkg/telemetry`: maps snapshots to OpenTelemetry metrics and events (names from `pkg/otel/semconv` at the repository root) and pushes them as OTLP JSON
- `pkg/compat`: compatibility exporters in the Prometheus formats of Kepler 0.7, Kepler 0.12, dcgm-exporter 4.8.4 and AMD device-metrics-exporter 1.5.2
- `pkg/facility`: the facility model (ambient temperature, cooling power, PUE) shared by both modes
- `pkg/workload`: the v1 workload engine's shared parts: label, annotation and environment names, the run API types, the Job builder, GPU and CPU slots, the rate formulas and the energy attribution
- `pkg/scenario` and `cmd/simscenario`: workload scenarios of the simulated cluster: schema, validation, plan, the runner that drives a run through the API server, assertions and `summary.json`; `scripts/simcluster/scenario.sh` runs them (`make simcluster-scenario`)
- `waok8s/`: external WAO code reference sandbox

## Install from release (recommended)

The simulator has its own Helm chart, published to the CERN OCI registry on every release:

```bash
helm upgrade --install joulie-simulator oci://registry.cern.ch/joulie/joulie-sim \
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
docker build -f simulator/Dockerfile -t registry.cern.ch/joulie/joulie-simulator:latest .
docker push registry.cern.ch/joulie/joulie-simulator:latest
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

## Emulate mode

In emulate mode the simulator emulates the bare-metal nodes of a cluster spec at the files and GPU tools the agent uses, so an unmodified agent pod discovers and caps them as real nodes, and it pushes their readings as OpenTelemetry metrics:

```bash
helm upgrade --install joulie-sim charts/joulie-simulator -n joulie-sim --create-namespace \
  --set emulate.enabled=true \
  --set-file emulate.clusterSpec=tests/simcluster/specs/smoke.yaml \
  --set otel.metricsEndpoint=http://prometheus-operated.monitoring:9090/api/v1/otlp/v1/metrics
curl -s localhost:18080/debug/emulate | jq '.nodes[] | {name, host}'   # after a port-forward
```

The chart then runs one replica as root with strategy `Recreate`, on the node labelled `joulie.io/hwemu-host=true`, and keeps the node trees under `emulate.root` on that host. Restarting the simulator resumes the trees and keeps every exported counter increasing. The simulator also runs from a workstation with a kubeconfig. See `/docs/simulator/simulator/#emulate-mode` for both, `/docs/simulator/metrics/` for the exported names, and the configuration reference for every variable.

## Use with Joulie

See:

- `experiments/kwok-cluster/README.md`
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

The keys `podW`, `dvfsDropWPerPct`, `noiseStddevW` and `gpu.powerModel` and the variables `SIM_POD_W`, `SIM_DVFS_DROP_W_PER_PCT`, `SIM_GPU_ALPHA_UTIL` and `SIM_GPU_BETA_CAP` are removed: nothing read them. A class file or an environment that still sets them works, with one warning per key.

For a node whose labels match the hardware catalogue, four values come from the catalogue unless a node class or a variable sets them: `pMaxW` (sockets x TDP, for a CPU without a measured curve), `cpuSocketCapMaxW` (TDP), `gpu.maxWattsPerGpu` (maximum board power) and `gpu.minCapWattsPerGpu` (minimum board power, else half the maximum). A measured two-socket curve scales by sockets/2. Hardware the catalogue does not know keeps 420, 400, 300 and 80 W.

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

Set `SIM_WORKLOAD_TRACE_PATH` to a JSONL trace file. Outside emulate mode the legacy engine will:

- load `type=job` records,
- create workload Pods over time,
- advance per-job progress based on node effective speed,
- delete Pods when work completes.

In emulate mode the v1 engine runs the trace instead (`SIM_WORKLOAD_MODEL`): one `batch/v1` Job per group, progress at the emulated hardware's throughput, completion through a KWOK Stage, stalls reported rather than completed by force, and results as OTLP JSON Lines under `<SIM_RUNS_DIR>/<runId>/`. A v1 run also starts through the run API, `POST /sim/v1/runs`. The two engines, the run API and their variables are in the [configuration reference](../website/content/en/docs/getting-started/05-configuration-reference.md) ("Simulator workloads and scenarios"); workload scenarios on the simulated cluster in `/docs/simulator/scenarios/`.

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
