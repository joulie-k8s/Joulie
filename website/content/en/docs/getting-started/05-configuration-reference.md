---
title: "Configuration Reference"
weight: 50
---

Complete reference for all Joulie environment variables. These are set via Helm values or directly in the Deployment/DaemonSet manifests.

Defaults listed below are the **code defaults**. The Helm chart (`charts/joulie/values.yaml`) overrides some of them: notably, the controller manager `NODE_SELECTOR` defaults to `joulie.io/managed=true` in the chart even though the code default is `node-role.kubernetes.io/worker`.

**Booleans.** The agent, the controller manager and the simulator read every on/off variable the same way: `true`, `1`, `yes` or `on`, and `false`, `0`, `no` or `off`, in any case, with surrounding spaces ignored. Unset or empty means the default. Any other value keeps the default and logs one warning naming the variable, so a typo never flips a switch. The exceptions are `SIM_OTEL_FILE_METRICS`, `OTEL_SDK_DISABLED` and `fakesmi`'s `HWEMU_UNPRIVILEGED`, whose rows say how they are read.

## Images

The image tag values (`agent.image.tag`, `controllerManager.image.tag`, `schedulerExtender.image.tag`, and `image.tag` in the simulator chart) default to empty, which pulls the chart's `appVersion`: a released chart runs its release's images, a chart from a checkout (`appVersion: latest`) the newest stable release. Every image is a multi-arch index of `linux/amd64` and `linux/arm64`.

Details: [Configuration notes, Images](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#images).

## Agent

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_MODE` | `daemonset` | `daemonset` (one agent per node) or `pool` (shared agents with sharding) |
| `NODE_NAME` | (required in daemonset mode) | Name of the node this agent manages |
| `RECONCILE_INTERVAL` | `20s` | How often the agent reconciles desired state |
| `METRICS_ADDR` | `:8080` | Address for the Prometheus metrics endpoint |
| `SPEC_READ_TIMEOUT` | `5m` | How long the agent tolerates failed reads of its `NodeTwin` spec before it relaxes every cap and puts the governor baseline back, until a read succeeds. A Go duration such as `90s`; a value it cannot parse keeps `5m` |
| `SIMULATE_ONLY` | `false` | If `true`, agent discovers hardware but does not apply power caps or governors (governor result `disabled`) |
| `HARDWARE_CATALOG_PATH` | (empty: the catalog embedded in the binary) | Path to a hardware inventory catalog YAML that replaces the embedded one |
| `KUBE_CLIENT_QPS` | `50` | Queries per second the agent's Kubernetes client may send |
| `KUBE_CLIENT_BURST` | `100` | Burst above `KUBE_CLIENT_QPS` the agent's Kubernetes client may send |

### Agent pool mode

| Variable | Default | Description |
|----------|---------|-------------|
| `POOL_NODE_SELECTOR` | `joulie.io/managed=true` | Label selector for nodes managed by pool agents |
| `POOL_SHARDS` | `1` | Total number of shards for pool mode partitioning |
| `POOL_SHARD_ID` | (from pod ordinal) | Shard ID for this agent instance |

### Agent DVFS control

| Variable | Default | Description |
|----------|---------|-------------|
| `DVFS_EMA_ALPHA` | `0.3` | Exponential moving average smoothing factor for power tracking |
| `DVFS_HIGH_MARGIN_W` | `10.0` | Power above cap (watts) to trigger frequency reduction |
| `DVFS_LOW_MARGIN_W` | `15.0` | Power below cap (watts) to trigger frequency increase |
| `DVFS_STEP_PCT` | `10` | Frequency throttle step size (%) |
| `DVFS_COOLDOWN` | `20s` | Minimum duration between DVFS adjustments |
| `DVFS_TRIP_COUNT` | `2` | Consecutive samples outside margin before acting |
| `DVFS_MIN_FREQ_KHZ` | `1500000` | Floor frequency for DVFS throttling (kHz) |

### Agent governor control

The agent always publishes its node's CPU and GPU governors in `NodeHardware` ([Governor discovery]({{< relref "/docs/architecture/agent.md#governor-discovery" >}})). It writes the presets of its `NodeTwin` only with `GOVERNOR_WRITES` `true`, and the controller manager sets them only with `GOVERNOR_CONTROL` on ([Governor control](#governor-control)).

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `GOVERNOR_WRITES` | `false` | `false` | Write the governor presets. `false` writes none and reports `disabled`, but still puts back, once, the settings an earlier run changed (result `restored`). |
| `GOVERNOR_RESTORE_ON_EXIT` | `true` | `true` | On SIGTERM or SIGINT, put the baseline back before the agent exits. SIGKILL and OOM kills restore nothing. |

- Writes need the agent's `/host-sys` mount, root user and privileges, as the chart gives; without them the result is `refused` (`NodeHardware` `writable: false`).
- NVIDIA presets need `nvidia-smi` in the agent ([Agent GPU tools](#agent-gpu-tools)); without it the result is `error`.
- HTTP control mode and pool mode write no governor (`unsupported`).
- What each preset becomes per CPU driver and GPU family: [CPU support]({{< relref "/docs/hardware/cpus.md#governors" >}}), [GPU support]({{< relref "/docs/hardware/gpus.md#governors" >}}). Results, retries and restores: [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}). Check them with:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,CPU-GOV:.status.controlStatus.cpuGovernor.result,CPU-NATIVE:.status.controlStatus.cpuGovernor.applied,GPU-GOV:.status.controlStatus.gpuGovernor.result,GPU-NATIVE:.status.controlStatus.gpuGovernor.applied
```

Details: [Configuration notes, Agent governor control](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent-governor-control).

### Agent telemetry and control backends

| Variable | Default | Description |
|----------|---------|-------------|
| `TELEMETRY_CPU_SOURCE` | `host` | CPU telemetry source: `host` (the node's RAPL files), `http` (`TELEMETRY_CPU_HTTP_ENDPOINT`), `none` |
| `TELEMETRY_CPU_CONTROL` | `host` | CPU control backend: `host`, `http`, `none` |
| `TELEMETRY_GPU_CONTROL` | `host` | GPU control backend: `host`, `http`, `none` |
| `TELEMETRY_CPU_HTTP_ENDPOINT` | (empty) | HTTP endpoint for CPU telemetry, `{node}` replaced with the node name (e.g., `http://sim:18080/otlp/telemetry/{node}`), answering OTLP JSON ([HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}})). The legacy `packagePowerWatts` JSON is read with a warning until v0.4.0. An answer over 8 MiB is an error. |
| `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT` | (empty) | HTTP endpoint for CPU control (e.g., `http://sim:18080/control/{node}`) |
| `TELEMETRY_CPU_CONTROL_MODE` | (empty) | With `TELEMETRY_CPU_CONTROL=http`: `dvfs` sends throttle percentages (`dvfs.set_throttle_pct`) and runs the DVFS loop for a cap in watts; any other value sends caps in watts (`rapl.set_power_cap_watts`) ([CPU enforcement algorithm]({{< relref "/docs/architecture/agent.md#cpu-enforcement-algorithm" >}})). |
| `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | (empty) | HTTP endpoint for GPU control (e.g., `http://sim:18080/control/{node}`) |
| `TELEMETRY_GPU_CONTROL_MODE` | (empty) | GPU control mode override |
| `TELEMETRY_HTTP_TIMEOUT_SECONDS` | `2` | HTTP client timeout for telemetry/control requests, in whole seconds |

Details: [Configuration notes, Agent telemetry and control backends](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent-telemetry-and-control-backends).

### Agent chart values

Helm values of `charts/joulie` that shape the agent pods. `agent.tolerations`, `agent.runtimeClassName` and the `agent.extra*` hooks apply to the DaemonSet and the emulated agents, not to the pool StatefulSet.

| Value | Default | Effect |
|-------|---------|--------|
| `agent.mode` | `daemonset` | `daemonset` (one agent per node matching `agent.daemonset.nodeSelector`), `pool`, `emulated` or `none` ([Agent Runtime Modes]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}})). |
| `agent.tolerations` | `- operator: Exists` (every taint) | Tolerations of the agent pods. The default tolerates every taint, so `agent.daemonset.nodeSelector` alone decides where the agent runs. A list replaces the default, and `[]` tolerates none. A managed node the agent does not reach is planned but not enforced ([Managed nodes without an agent](#managed-nodes-without-an-agent)). |
| `agent.runtimeClassName` | `""` | RuntimeClass of the agent pods, for example the one of the NVIDIA container runtime, which puts the node's driver tools into the container. |
| `agent.extraEnv` | `[]` | EnvVar entries added after the chart's own. |
| `agent.extraVolumes` | `[]` | Volume entries added after the chart's own. |
| `agent.extraVolumeMounts` | `[]` | VolumeMount entries added to the agent container after the chart's own. |
| `agent.emulated.root` | `/var/lib/joulie-hwemu` | The simulator's `SIM_HWEMU_ROOT`, from which each emulated agent mounts its node's tree (`sys` at `/host-sys`, `cpuinfo` over `/proc/cpuinfo`, `state`, `bin` and `lib` under `/emu`). |
| `agent.emulated.hostNodeSelector` | `joulie.io/hwemu-host: "true"` | Node selector of the emulated agents: the node that holds the trees and runs the simulator. |
| `agent.emulated.path` | `/emu/bin:/usr/local/bin:/usr/bin:/bin` | `PATH` of the emulated agents, so the emulated GPU tools come first. |
| `agent.emulated.fuse` | `false` | Mount `sys` and `state` from the simulator's FUSE views, for a simulator with `emulate.fuse`. The DaemonSet ignores it. |
| `agent.emulated.nodes` | `[]` | One entry per emulated node, as `simcluster agents` writes them: `name` (the Node, also the literal `NODE_NAME`), `profile`, `devNvidiactl`, `extraVolumes`, `extraVolumeMounts`. Entry `i` becomes the Deployment `joulie-agent-emu-<i>`. A name that is not a Node name, or is listed twice, fails the render. |

Emulated agents are what the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs. Details: [Configuration notes, Agent chart values](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent-chart-values).

### Agent GPU tools

The agent reads and caps NVIDIA GPUs by running `nvidia-smi` from its `PATH`. AMD and Intel GPUs need no tool, only the chart's root agent and writable `/host-sys`: the agent writes the amdgpu `power1_cap` files, or the i915 or xe `power1_max` or `power2_max` files, with backend `host-intel` ([Agent host backends]({{< relref "/docs/hardware/gpus.md#agent-host-backends" >}})).

The agent image ships no GPU tool and no C library: bring the driver's tools and libraries in with `agent.runtimeClassName` (such as the NVIDIA container runtime), or with `agent.extraVolumes`, `agent.extraVolumeMounts` and `agent.extraEnv`. No recipe has been verified on a real NVIDIA node yet, so after installing check every GPU node:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,GPU:.status.controlStatus.gpu.result,MESSAGE:.status.controlStatus.gpu.message
```

A tool that cannot run shows `error` with a message starting `gpu inventory failed`, or `blocked` with `no supported GPU backend detected`; the GPUs then keep their limits. Details: [Configuration notes, Agent GPU tools](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent-gpu-tools).

### Managed nodes without an agent

The controller manager plans every managed node, agent or not. The `NodeTwin` condition `AgentReporting` (the `Agent` column of `kubectl get nodetwins`) is `True` while an agent publishes the node's `NodeHardware`, and `False` (`NoNodeHardware`, or `NodeHardwareStale` after 15 minutes without a write) when the caps are planned but not enforced. `joulie_twin_nodes_agent_reporting` counts the nodes per status and reason ([Twin agent coverage]({{< relref "/docs/architecture/metrics.md#twin-agent-coverage" >}})). Usual causes: a taint the agent does not tolerate, a node outside `agent.daemonset.nodeSelector`, or an image without the node's platform. The conditions `EcoProfile`, `CPUEcoCap`, `GPUEcoCap`, `CPUGovernorPreset` and `GPUGovernorPreset` say why the node got its profile, caps and governors ([Why a node got its state]({{< relref "/docs/architecture/policy.md#why-a-node-got-its-state" >}})).

```bash
kubectl get nodetwins -o custom-columns='NODE:.spec.nodeName,AGENT:.status.conditions[?(@.type=="AgentReporting")].status,REASON:.status.conditions[?(@.type=="AgentReporting")].reason'
```

Details: [Configuration notes, Agent](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent).

## Controller manager

| Variable | Default | Description |
|----------|---------|-------------|
| `RECONCILE_INTERVAL` | `1m` | How often the controller manager reconciles cluster state |
| `METRICS_ADDR` | `:8081` | Address for the Prometheus metrics endpoint |
| `LEADER_ELECT` | `false` | Run the reconcile loop only on the leader of a `coordination.k8s.io` lease. Required before running more than one controller manager replica. Helm: `controllerManager.leaderElection.enabled`, which also creates the lease Role. |
| `POD_NAMESPACE` | `joulie-system` | Namespace of the leader election lease. The chart sets it from the downward API. |
| `HARDWARE_CATALOG_PATH` | (empty: the catalog embedded in the binary) | Path to a hardware inventory catalog YAML that replaces the embedded one |
| `NODE_SELECTOR` | `node-role.kubernetes.io/worker` | Label selector for managed nodes |
| `RESERVED_LABEL_KEY` | `joulie.io/reserved` | Label key for nodes excluded from policy decisions |
| `POWER_PROFILE_LABEL` | `joulie.io/power-profile` | Node label key for the active power profile |
| `KUBE_CLIENT_QPS` | `50` | Queries per second the controller manager's Kubernetes client may send |
| `KUBE_CLIENT_BURST` | `100` | Burst above `KUBE_CLIENT_QPS` the controller manager's Kubernetes client may send |

### Measured node power

The controller manager writes each node's measured power to `NodeTwin.status.powerMeasurement.measuredNodePowerW`, from Prometheus (`NODE_POWER_SOURCE=prometheus`, which the chart sets) or from one OTLP JSON document per node (`http`). The default preset `otel` reads `hw.energy` or `hw.power` of the `cpu` and `gpu` components, keyed by `k8s.node.name`; [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}) has every preset query and the Prometheus settings.

The Chart column is what `charts/joulie/values.yaml` sets; "unset" means the code default applies.

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `PROMETHEUS_ADDRESS` | (empty) | `http://prometheus-operated.monitoring:9090` | Prometheus the measured inputs are read from. The default of `NODE_POWER_PROMETHEUS_ADDRESS` and `FACILITY_PROMETHEUS_ADDRESS`. |
| `PROMETHEUS_NAME_TRANSLATION` | `UnderscoreEscapingWithSuffixes` | unset | Prometheus' `otlp.translation_strategy`, which the controller manager must match: `UnderscoreEscapingWithSuffixes`, `NoUTF8EscapingWithSuffixes`, `UnderscoreEscapingWithoutSuffixes` or `NoTranslation`, case-insensitive. It spells the names the `otel` presets and the facility key labels query. |
| `PROMETHEUS_RESOURCE_ATTRIBUTES` | `auto` | unset | Where the `otel` node power preset finds `k8s.node.name`: `labels` (on every series; needs `k8s.node.name` in Prometheus' `otlp.promote_resource_attributes`), `target_info` (joined from `target_info`), or `auto` (labels first, then `target_info`, logging every switch). |
| `PROMETHEUS_RATE_WINDOW` | `5m` | `5m` | Window of every `rate()` the inputs run, and the longest interval the `http` source measures an energy rate over. At least `10s`, a whole number of seconds, and at least four export intervals of the producer, or `rate()` reads low. |
| `NODE_POWER_SOURCE` | `static` | `prometheus` | `static` (0 W), `prometheus` or `http`. An unknown value means `static`, with a warning. |
| `NODE_POWER_PRESET` | (empty: `otel`, or `custom` when `NODE_POWER_PROMETHEUS_QUERY` is set) | unset | The format the Prometheus query reads: `otel`, `kepler` (Kepler 0.10 and later), `kepler-legacy` (Kepler 0.7 to 0.9) or `custom` ([The queries]({{< relref "/docs/architecture/telemetry.md#the-queries" >}})). A preset other than `custom` together with `NODE_POWER_PROMETHEUS_QUERY` stops the controller manager at start. |
| `NODE_POWER_SCOPE` | `components` | `components` | `components`: the CPU packages plus the GPUs, the boundary of the twin's budget. `host`: the whole host (`hw.host.energy` and `hw.host.power`, or Kepler 0.7 platform energy), which exceeds the budget under load. `kepler` with `host` stops the controller manager at start. |
| `NODE_POWER_NODE_LABEL` | per preset: the translated `k8s.node.name` (`k8s_node_name` by default) for `otel` and `custom`, `node_name` for `kepler`, `exported_instance` for `kepler-legacy` | unset | The label that names the node in the query result. |
| `NODE_POWER_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS` | unset | Prometheus of the node power query only. |
| `NODE_POWER_PROMETHEUS_QUERY` | (empty) | unset | Your own PromQL, in **watts**, for the `custom` preset. With `{node}` it runs once per node and its series are summed; without `{node}` it is summed by `NODE_POWER_NODE_LABEL`. |
| `NODE_POWER_HTTP_ENDPOINT` | (empty) | unset | With `NODE_POWER_SOURCE=http`: a URL with `{node}`, answering OTLP JSON per node, such as the simulator's `/otlp/telemetry/{node}`, read like the `otel` preset ([HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}})). The legacy `packagePowerWatts` JSON is read with a warning until v0.4.0. An answer over 8 MiB is an error (`source: http-error`). |

A custom query must return power in watts: pass an energy counter through `rate()`, or `measuredNodePowerW` climbs forever and drives every node's headroom negative. Details: [Configuration notes, Measured node power](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#measured-node-power).

Upgrading from v0.2: see [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}).

### Power cap configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERFORMANCE_CAP_WATTS` | `5000` | Absolute CPU power cap for performance nodes (watts). The CPU of a GPU node gets it whatever the node's profile. |
| `ECO_CAP_WATTS` | `120` | Absolute CPU power cap for eco CPU-only nodes (watts). A GPU node never gets it. |
| `CPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | CPU cap as percentage of max for performance nodes |
| `CPU_ECO_CAP_PCT_OF_MAX` | `60` | CPU cap as percentage of max for eco nodes |
| `CPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute watts instead of percentage |
| `GPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | GPU cap as percentage of max for performance nodes |
| `GPU_ECO_CAP_PCT_OF_MAX` | `60` | GPU cap as percentage of max for eco nodes |
| `GPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute GPU watts instead of percentage |
| `GPU_MODEL_CAPS_JSON` | `{}` | JSON map of GPU model name to `{"minCapWatts": N, "maxCapWatts": M}` |
| `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES` | `false` | If `true`, a GPU node whose `NodeHardware` reports GPUs without control (`status.gpu.controlAvailable` false) gets the CPU caps of its profile instead of running uncapped in eco. Other GPU nodes get no eco CPU cap. |
| `GPU_PRODUCT_LABEL_KEYS` | `joulie.io/gpu.product,nvidia.com/gpu.product,amd.com/gpu.product-name,beta.amd.com/gpu.product-name,amd.com/gpu.product,gpu.intel.com/product,amd.com/gpu.family` | Comma-separated node label keys to read the GPU product name from, first match wins. The agent reads this default list when its GPU inventory names no model; it does not read this variable. |

The GPU caps apply per physical GPU on every GPU node, including nodes with MIG devices, AMD partitions or shared replicas ([Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})). `GPU_MODEL_CAPS_JSON` and `GPU_GOVERNOR_MODEL_PRESETS_JSON` are keyed by the product without the `-MIG-<profile>`, `-MIG-INVALID` and `-SHARED` suffixes. Details: [Configuration notes, Power cap configuration](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#power-cap-configuration).

### Policy configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `POLICY_TYPE` | `static_partition` | Policy algorithm: `static_partition`, `queue_aware_v1`, or `rule_swap_v1` |
| `STATIC_HP_FRAC` | `0.50` | Fraction of nodes allocated to performance in `static_partition`, split across hardware families in proportion to their size, at least one node per family |
| `QUEUE_HP_BASE_FRAC` | `0.60` | Base fraction of performance nodes in `queue_aware_v1`. The resulting count is split across hardware families in proportion to their size, at least one node per family |
| `QUEUE_HP_MIN` | `1` | Minimum performance nodes in `queue_aware_v1` |
| `QUEUE_HP_MAX` | `1000000` | Maximum performance nodes in `queue_aware_v1` |
| `QUEUE_PERF_PER_HP_NODE` | `10` | Running or pending performance pods per performance node in `queue_aware_v1` |

The controller manager exports the split as `joulie_policy_family_nodes{family}` and `joulie_policy_family_performance_nodes{family}`, where `family` is `gpu:<model>` or `cpu:<model>`. See [Metrics]({{< relref "/docs/architecture/metrics.md" >}}).

### Governor control

Off by default. With `GOVERNOR_CONTROL` on, the controller manager writes a vendor-neutral governor preset into each managed node's `NodeTwin` (`spec.cpu.governor`, `spec.gpu.governor`) by the profile it runs at, and the agent maps it to native settings once it runs with `GOVERNOR_WRITES` `true`. Which node gets which preset: [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `GOVERNOR_CONTROL` | `off` | `off`, `cpu` (CPU governors only), `gpu` (GPU governors only) or `all`. |
| `CPU_GOVERNOR_PERFORMANCE_PRESET` | `performance` | CPU preset of performance nodes: `performance` (highest frequency at any load), `balanced` (demand-driven, quick ramp) or `efficiency` (demand-driven with an energy bias). |
| `CPU_GOVERNOR_ECO_PRESET` | `efficiency` | CPU preset of eco nodes. Provisional: it does not pass the simulator's [governor exploration]({{< relref "/docs/experiments/governor-exploration.md#acceptance-tier-a-half" >}}) as a default for every CPU driver, and it lengthens compute-bound bursts. |
| `CPU_GOVERNOR_ECO_BOOST` | empty | `on` or `off`: frequency boost (turbo) with the eco preset, on nodes whose agent found a boost control (`status.cpu.governor.boost`). Empty leaves boost alone. |
| `CPU_GOVERNOR_ON_GPU_NODES` | `false` | Write the CPU governor on GPU nodes too. |
| `GPU_GOVERNOR_PERFORMANCE_PRESET` | empty | GPU preset of performance nodes: `auto`, `sustained`, `efficient`, `low`, `peak`, or empty for none. |
| `GPU_GOVERNOR_ECO_PRESET` | `efficient` | GPU preset of eco nodes; set it to an empty value to write none. |
| `GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX` | `80` | Compute clock ceiling of the `efficient` preset, in percent of the GPU's maximum compute clock, in (0, 100]. Written only with `efficient`; a value out of range gives 80. Provisional ([governor exploration]({{< relref "/docs/experiments/governor-exploration.md#acceptance-tier-a-half" >}})). |
| `GPU_GOVERNOR_MODEL_PRESETS_JSON` | `{}` | Presets per GPU model: `{"<model>": {"performance": "<preset>", "eco": "<preset>", "ecoCeilingPctOfMax": <pct>}}`, keyed like `GPU_MODEL_CAPS_JSON`. A field left out keeps the default, `""` writes none for that model, and an invalid field is dropped with a warning. |
| `GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU` | `false` | If `true`, a node whose GPU preset is `sustained` gets it only while a running pod on it requests 2 or more GPUs (units of any GPU resource: whole GPUs, MIG devices, partitions, shared replicas), and no GPU governor otherwise. |

GPU presets: `auto` is the vendor default; `sustained` holds memory and fabric clocks at their maximum (no default selects it); `efficient` caps the compute clock at the ceiling; `low` and `peak` are for experiments. All defaults are provisional until measured. Meanings: [Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}); which node gets which: [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

The overview dashboard's "Agent: Governors" row plots the presets in effect (`joulie_agent_governor_state`) and the writes per result (`joulie_agent_governor_writes_total`). Details: [Configuration notes, Governor control](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#governor-control).

### Facility metrics

The facility poller reads seven inputs from Prometheus each poll: outdoor ambient temperature, IT power and cooling power, the per-zone, per-rack and per-node inputs ([Node topology](#node-topology)), and the grid carbon intensity when its query is set. An input whose query fails or returns nothing is unknown until a later poll reads it. Metrics: `joulie_twin_input_queries_total{input,result}`, `joulie_twin_facility_*` and `joulie_grid_carbon_intensity_grams_per_kW_h` ([Metrics]({{< relref "/docs/architecture/metrics.md#twin-input-metrics" >}})).

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `ENABLE_FACILITY_METRICS` | `false` | `false` | Poll the facility inputs. The first poll runs at start. |
| `FACILITY_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS`, or `http://prometheus-operated.monitoring:9090` when that is unset | unset | Prometheus of the facility queries only. |
| `FACILITY_POLL_INTERVAL` | `30s` | `30s` | How often the inputs are polled. 0 or less stops the controller manager at start. |
| `FACILITY_PRESET` | `otel` | `otel` | `otel`: the facility, cooling zone, rack and host metrics of the [input contract]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}), every input but the carbon intensity. `legacy`: the three `datacenter_*` single inputs only; deprecated, removed in v0.4.0. `custom`: only the inputs whose query is set below. |
| `FACILITY_AMBIENT_TEMP_QUERY` | from the preset | unset | Outdoor ambient temperature, one value in degrees Celsius. |
| `FACILITY_IT_POWER_QUERY` | from the preset | unset | Power of the IT equipment, one value in watts. |
| `FACILITY_COOLING_POWER_QUERY` | from the preset | unset | Power of the cooling, one value in watts. |
| `FACILITY_ZONE_AMBIENT_TEMP_QUERY` | from the preset | unset | Ambient temperature of each cooling zone, keyed by the translated `joulie.cooling_zone.name` label (`joulie_cooling_zone_name` by default), whose values are the nodes' `joulie.io/cooling-zone` values. |
| `FACILITY_RACK_POWER_QUERY` | from the preset | unset | Power of each rack in watts, keyed by the translated `joulie.rack.name` label (`joulie_rack_name`), whose values are the nodes' `joulie.io/rack` label values. |
| `FACILITY_NODE_INLET_TEMP_QUERY` | from the preset | unset | Inlet temperature of each node, keyed by the translated `k8s.node.name` label (`k8s_node_name`). |
| `FACILITY_CARBON_INTENSITY_QUERY` | unset (no preset reads it) | unset | Carbon intensity of the grid that supplies the cluster, one value. Unset reads none. |
| `FACILITY_CARBON_INTENSITY_UNIT` | `gCO2eq/kWh` | unset | Unit of the carbon intensity value: a mass (`g`, `kg`, `t`, `lb`, `lbs`), optionally followed by `CO2`, `CO2e` or `CO2eq`, over `kWh` or `MWh`, in any case, such as `lbsCO2/MWh`. Any other unit stops the controller manager at start while the query is set. |
| `FACILITY_CARBON_INTENSITY_SIGNAL` | none | unset | What the value measures: `marginal` (such as WattTime's MOER) or `average` (the whole generation mix, such as Electricity Maps). Required with the query: the controller manager does not start without it. |

A query variable replaces its input's query under any preset. A bare metric name is aggregated by the input's key label (`avg` for temperatures and carbon intensity, `sum` for power); any other query must return one series, or one per key carrying the key label. Details: [Configuration notes, Facility metrics](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#facility-metrics).

### Grid carbon intensity

Joulie ships no carbon intensity provider. If you already export one to Prometheus, point `FACILITY_CARBON_INTENSITY_QUERY` at it, with `ENABLE_FACILITY_METRICS=true`, and say what the value is:

```yaml
controllerManager:
  env:
    ENABLE_FACILITY_METRICS: "true"
    FACILITY_CARBON_INTENSITY_QUERY: 'watttime_co2_moer{region="CAISO_NORTH"}'
    FACILITY_CARBON_INTENSITY_UNIT: "lbsCO2/MWh"
    FACILITY_CARBON_INTENSITY_SIGNAL: "marginal"
```

The query must return one series, so select the cluster's grid zone with a label matcher. The value is converted to gCO2eq/kWh and exported as `joulie_grid_carbon_intensity_grams_per_kW_h{joulie_grid_carbon_intensity_signal}`; no policy acts on it yet. Details: [Configuration notes, Grid carbon intensity](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#grid-carbon-intensity).

Deprecated aliases, removed in v0.4.0: `FACILITY_AMBIENT_TEMP_METRIC`, `FACILITY_IT_POWER_METRIC`, `FACILITY_COOLING_POWER_METRIC`, the former names of the first three query variables. They are read only when the new name is unset, with a warning naming the replacement, and the bare metric name they hold keeps its meaning.

### Node topology

Optional node labels give per-rack PSU stress and per-zone cooling stress:

- `joulie.io/rack`: physical rack identifier (e.g., `rack-1`)
- `joulie.io/cooling-zone`: cooling zone identifier (e.g., `zone-a`)

With them, PSU stress is computed per rack: the rack power input (`FACILITY_RACK_POWER_QUERY`) when it reads that rack, otherwise the sum of the estimated power of the rack's nodes. A twin's ambient temperature is the most local one known: the node's inlet (`FACILITY_NODE_INLET_TEMP_QUERY`), then its cooling zone's (`FACILITY_ZONE_AMBIENT_TEMP_QUERY`), then the outdoor ambient.

Nodes without the labels use cluster-wide stress, and still their inlet temperature when the facility reads one.

## Scheduler extender

| Variable | Default | Description |
|----------|---------|-------------|
| `EXTENDER_ADDR` | `:9876` | Bind address for the scheduler extender HTTP server |
| `METRICS_ADDR` | `:9877` | Address for the scheduler's Prometheus metrics endpoint |
| `CACHE_TTL` | `30s` | Minimum interval between rebuilding the scoring maps from the informer cache, which watches NodeTwin and NodeHardware, so a score never reads the API server. |
| `TWIN_STALENESS_THRESHOLD` | `5m` | Duration after which NodeTwin data is considered stale |

The Service `joulie-scheduler-extender` carries `schedulerExtender.service.annotations`, by default `prometheus.io/scrape: "true"`, `prometheus.io/port: "9877"` and `prometheus.io/path: /metrics`, so annotation-based discovery scrapes the `joulie_scheduler_*` metrics. With the Prometheus Operator, use a ServiceMonitor on the `metrics` port ([tutorial 03](https://github.com/joulie-k8s/Joulie/tree/main/examples/03-grafana-dashboard)).

With `schedulerExtender.hostNetwork: true` the extender deploys with strategy `Recreate`; unless the scheduler configuration marks it `ignorable: true`, also set `schedulerExtender.nodeName`. Details: [Configuration notes, Scheduler extender](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#scheduler-extender).

## Alerting rules

The chart ships four alerting rules (`charts/joulie/alerts/joulie-alerts.yaml`), each with a runbook in [Alerts and Runbooks]({{< relref "/docs/getting-started/07-runbooks.md" >}}): `JoulieCapNotApplied`, `JoulieAgentWritesBlocked`, `JoulieTwinStale` and `JoulieNodePowerNoData`. They read the agent's `joulie_agent_cap_result` and `joulie_agent_status_write_errors_total` and the controller manager's `joulie_twin_status_updated_timestamp_seconds` and `joulie_twin_nodes_power_source` ([Metrics]({{< relref "/docs/architecture/metrics.md" >}})).

| Value | Default | Effect |
|-------|---------|--------|
| `prometheusRule.enabled` | `false` | Render the rules in the PrometheusRule `<release>-joulie-alerts`, for a Prometheus run by the Prometheus Operator, whose CRD it needs. Without the operator, load the file with `rule_files`. |
| `prometheusRule.namespace` | `""` | Namespace of the PrometheusRule; empty is the release namespace. |
| `prometheusRule.labels` | `{}` | Labels added to the PrometheusRule, for the `ruleSelector` of your Prometheus (for example `release: kube-prometheus-stack`). |

## Simulator

This section covers the binaries under `simulator/cmd`. Each subsection names the binary that reads its variables. The simulator's modes are described in [Simulator]({{< relref "/docs/simulator/_index.md" >}}).

### Simulator basics

The simulator (`simulator/cmd/simulator`) reads these in every mode.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_ADDR` | `127.0.0.1:18080`; `:18080` in a pod (where `KUBERNETES_SERVICE_HOST` is set) | Listen address of the HTTP API. The API has no authentication: expose it only to trusted clients. The chart sets `:18080`. |
| `SIM_NODE_SELECTOR` | (empty: every Node) | Label selector of the Nodes the simulator models. The chart sets `joulie.io/managed=true`. |
| `SIM_NODE_CLASS_CONFIG` | (empty) | YAML file of label-matched node classes that override model parameters per node. |
| `SIM_HARDWARE_CATALOG_PATH` | (empty: the catalog embedded in the binary) | A hardware catalog YAML that replaces the embedded one. |
| `SIM_K8S_POD_WATCH` | `true` | List pods and nodes every `SIM_POLL_INTERVAL`. |
| `SIM_DEBUG_PERSIST_DIR` | `/tmp/joulie-simulator-debug` | Directory of `timeseries.csv`, `events.ndjson` and snapshots of the debug endpoints (`nodes.json`, `jobs.json`, `events.json`, `energy.json`); empty disables them. |
| `SIM_FACILITY_AMBIENT_TEMP_C`, `SIM_FACILITY_TEMP_AMPLITUDE_C`, `SIM_FACILITY_TEMP_PERIOD_H` | `20`, `5`, `24` | Outdoor ambient temperature of the facility model: a sine of this mean, amplitude and period in hours. In emulate mode the cluster spec's `facility` block wins. |
| `SIM_STANDALONE` | `false` | Run without Kubernetes ([Standalone mode]({{< relref "/docs/simulator/simulator.md#standalone-mode" >}})). |

### Simulator thermal model

The simulator (`simulator/cmd/simulator`) reads these at startup; a node class in `SIM_NODE_CLASS_CONFIG` overrides one per node with the key in parentheses, under `model:`.

Thermal throttling is off by default (both thresholds 0). Setting one variable of a pair turns it on, with the other at the value its row gives; in a node class set both, the full one above the start (wiki: [CPU temperature](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model#telemetry-and-temperature), [GPU temperature](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model#temperature)). Details: [Configuration notes, Simulator thermal model](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-thermal-model).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_CPU_AMBIENT_TEMP_C` | `24` | CPU ambient temperature in °C (`cpuAmbientTempC`) |
| `SIM_CPU_THERMAL_TAU_MS` | `4000` | CPU thermal time constant in milliseconds (`cpuThermalTauMs`) |
| `SIM_CPU_WATTS_PER_DELTA_C` | `8` | CPU watts per °C above ambient at equilibrium (`cpuWattsPerDeltaC`) |
| `SIM_CPU_THERMAL_THROTTLE_START_C` | `0` (off) | CPU temperature in °C where thermal throttling starts (`cpuThermalThrottleStartC`). `85` when only `SIM_CPU_THERMAL_THROTTLE_FULL_C` is set. |
| `SIM_CPU_THERMAL_THROTTLE_FULL_C` | `0` (off) | CPU temperature in °C where thermal throttling reaches throughput 0 (`cpuThermalThrottleFullC`). `97` when only `SIM_CPU_THERMAL_THROTTLE_START_C` is set. |
| `SIM_GPU_AMBIENT_TEMP_C` | `24` | GPU ambient temperature in °C (`gpu.ambientTempC`) |
| `SIM_GPU_THERMAL_TAU_MS` | `2500` | GPU thermal time constant in milliseconds (`gpu.thermalTauMs`) |
| `SIM_GPU_WATTS_PER_DELTA_C` | `12` | GPU watts per °C above ambient at equilibrium (`gpu.wattsPerDeltaC`) |
| `SIM_GPU_THERMAL_THROTTLE_START_C` | `0` (off) | GPU temperature in °C where thermal throttling starts (`gpu.thermalThrottleStartC`). `82` when only `SIM_GPU_THERMAL_THROTTLE_FULL_C` is set. |
| `SIM_GPU_THERMAL_THROTTLE_FULL_C` | `0` (off) | GPU temperature in °C where thermal throttling is full (`gpu.thermalThrottleFullC`). `92` when only `SIM_GPU_THERMAL_THROTTLE_START_C` is set. |

### Simulator power limits

The simulator (`simulator/cmd/simulator`) reads these at startup. For a node the hardware catalogue matches, an unset variable leaves the field to the catalogue; the built-in default applies only to hardware it does not know. A node class key (in parentheses) wins over both, and 0 or less counts as unset (wiki: [CPU power model, Where the numbers come from](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model#where-the-numbers-come-from)).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_P_MAX_W` | sockets x catalogue TDP, else `420` | Peak CPU power of the generic model, at full load and full frequency (`pMaxW`). A node with a measured curve uses the curve instead. |
| `SIM_BETA_FREQ` | `1.43` | Exponent of the frequency scale in the CPU's dynamic power, on the generic model and on a measured curve: `P = P(0) + (P(u) - P(0)) x f^beta`. Its fit is on the wiki's [CPU power model](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model). |
| `SIM_RAPL_CAP_MIN_W` | `0` | Lowest node CPU cap the simulator accepts; a cap below it is refused with an error. `0` is no floor; a cap the CPU cannot reach holds it at its lowest-frequency power. |
| `SIM_CPU_SOCKET_CAP_MAX_W` | catalogue TDP, else `400` | Upper end of the per-socket CPU cap: the node cap is split evenly over the sockets and clamped to this range (`cpuSocketCapMaxW`). |
| `SIM_GPU_MAX_WATTS_PER_GPU` | catalogue `maxBoardPowerW`, else `300` | Board power of one GPU at full load, and its highest cap (`gpu.maxWattsPerGpu`). |
| `SIM_GPU_MIN_CAP_WATTS_PER_GPU` | catalogue `minBoardPowerW`, else half the maximum, else `80` | Lowest cap of one GPU (`gpu.minCapWattsPerGpu`); a cap below it is refused with an error. |

A catalogue minimum above a maximum set by a class or a variable is lowered to that maximum. A measured two-socket curve from the catalogue is scaled by sockets/2 on a node with another socket count.

Upgrading from v0.2: see [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}).

### Simulator physics step

Outside the emulate-mode cluster spec, the simulator (`simulator/cmd/simulator`) advances a node's model only in a physics step, in simulated time, split into equal substeps ([Power Simulator]({{< relref "/docs/simulator/power-simulator.md#the-physics-step" >}})).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_POLL_INTERVAL` | `15s` (chart: `8s`) | Kubernetes mode: how often the simulator lists pods and nodes. A value of 0 or less logs a warning and takes `15s`. |
| `SIM_PHYSICS_TICK` | `1s` | Kubernetes mode without emulate mode: every modelled node is stepped by the wall time since the last tick, in substeps of at most 100 ms. A value of 0 or less logs a warning and takes `1s`. In emulate mode `SIM_EMULATE_TICK` steps them instead, and standalone mode steps them on its own tick. |
| `SIM_SA_SUBSTEP` | `SIM_SA_DT` | Standalone mode: the longest physics substep, in simulated seconds; each `SIM_SA_DT` tick is split into equal substeps no longer than this. Shorter follows the cap, frequency and thermal lags more closely, at the cost of run time. |

### Simulator hardware emulation

`fakesmi` (`simulator/cmd/fakesmi`) stands in for `nvidia-smi`, `rocm-smi` and `amd-smi` on an emulated node. It runs as the tool its name says, through a `bin/<tool>` link, and reads and writes the node's emulated hardware files instead of a driver. The agent does not read these variables; only `fakesmi` does.

| Variable | Default | Description |
|----------|---------|-------------|
| `HWEMU_SYS_ROOT` | `/host-sys` | The emulated node's `sys/` directory, where the amdgpu PCI and hwmon attributes live. The default is where an emulated agent pod mounts it. |
| `HWEMU_STATE_ROOT` | `/emu/state` | The emulated node's `state/` directory, with `tools.json` (the tools the node has) and the GPU state. The default is where an emulated agent pod mounts it. |
| `HWEMU_ROOTFS` | `/` | The file system the real tool would run in, where `fakesmi` looks for what the tool needs before it runs (below). The default is the agent container's own root. |
| `HWEMU_UNPRIVILEGED` | `false` | Run the tools as a user without root would: `1`, `true` or another word Go's `strconv.ParseBool` reads as true; anything else, unset included, means root. Sets then fail or do nothing, as the real tools do without root (`nvidia-smi -pl` exits 4 with `Insufficient Permissions`); reads work. |

A tool that `tools.json` does not list exits 127 with `fakesmi: <tool> is not installed on this emulated node`.

A listed tool first checks, under `HWEMU_ROOTFS`, that the files the real tool needs exist (the ELF loader, the C libraries and NVML for `nvidia-smi`, `python3` for `rocm-smi` and `amd-smi`), and fails with the real tool's exit code and message when one is missing. Details: [Configuration notes, Simulator hardware emulation](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-hardware-emulation).

### Simulator emulate mode

In emulate mode the simulator (`simulator/cmd/simulator`) emulates the bare-metal nodes a cluster spec lists, with a sysfs tree, `/proc/cpuinfo` and GPU tool state per node under `SIM_HWEMU_ROOT/<node>/` that an unmodified agent caps as a real node. `/control/<node>` answers 409 for an emulated node. See [Emulate Mode]({{< relref "/docs/simulator/emulate-mode.md" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EMULATE` | `false` | Turn emulate mode on. Refused with `SIM_STANDALONE`, with an empty `SIM_CLUSTER_SPEC`, and with `SIM_WORKLOAD_MODEL=legacy`, since a legacy trace cannot drive emulated nodes. |
| `SIM_CLUSTER_SPEC` | (empty) | Path of the cluster spec, a `sim.joulie.io/v1alpha1` `ClusterSpec` such as `tests/simcluster/specs/smoke.yaml`: each node, its hardware profile, pool, rack and cooling zone, and the facility. |
| `SIM_HWEMU_ROOT` | `/var/lib/joulie-hwemu` | Directory of the emulated trees, one `<node>/` per spec node. One simulator holds it at a time; a second one fails to start. |
| `SIM_HWEMU_FUSE` | `false` | Serve each spec node's `sys` and `state` through FUSE at `SIM_HWEMU_ROOT/fuse/<node>/`, so writes and counters behave as the kernel's do ([FUSE mode]({{< relref "/docs/simulator/emulate-mode.md#fuse-mode" >}})). Needs `/dev/fuse`, and as root `CAP_SYS_ADMIN`; another user needs `fusermount3`. |
| `SIM_EMULATE_TICK` | `1s` | Physics tick: every spec node advances to wall time once per tick; a late tick is caught up by the next. The nodes outside the spec are stepped on the same tick (see "Simulator physics step"). |
| `SIM_GPU_RECEIVER_GATE` | `profile` | Whether the receiver gate of [peer transfers]({{< relref "/docs/hardware/hardware-modeling.md#peer-transfers" >}}) applies: `profile` (each profile's `gpus.transfer.receiverGate.enabled`, off in every builtin profile), `off` everywhere, or `on` on every GPU profile, for what-if runs. Any other value logs a warning and takes `profile` ([peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})). |
| `SIM_EMULATE_WORKERS` | `0` | Nodes stepped in parallel; `0` means `GOMAXPROCS`, at most 12 ([Tick cost]({{< relref "/docs/simulator/emulate-mode.md#tick-cost" >}})); a set value is used as given. |
| `SIM_FAKESMI_SOURCE` | `/usr/local/lib/joulie/fakesmi` | The `fakesmi` binary that every node's `bin/nvidia-smi`, `bin/rocm-smi` and `bin/amd-smi` link to; the image ships it at the default path. Empty skips the copy, for a run outside the image. |
| `KUBECONFIG` | client-go loading rules | The cluster the simulator watches, so it can also run on a workstation; without a kubeconfig it uses the in-cluster configuration. |

Hardware profiles are the builtin ones in `simulator/pkg/hwemu/profiles/`, plus local ones from `SIM_PROFILES_DIR`. The facility values come from the spec's `facility` block and fall back to `SIM_FACILITY_AMBIENT_TEMP_C`, `SIM_FACILITY_TEMP_AMPLITUDE_C` and `SIM_FACILITY_TEMP_PERIOD_H`.

A restarted simulator resumes the trees; a tree that no longer matches its spec node stops the start ([Restarts and recovery]({{< relref "/docs/simulator/emulate-mode.md#restarts-and-recovery" >}})). Details: [Configuration notes, Simulator emulate mode](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-emulate-mode).

Helm values of `charts/joulie-simulator`:

| Value | Default | Effect |
|-------|---------|--------|
| `emulate.enabled` | `false` | Sets `SIM_EMULATE`, runs the pod as root with a readiness probe on `/readyz`, and leaves out `SIM_WORKLOAD_TRACE_PATH`: a run starts through the run API, or from a trace set in `extraEnv`. The chart fails unless `replicas` is 1. |
| `emulate.root` | `/var/lib/joulie-hwemu` | `SIM_HWEMU_ROOT`, and a `DirectoryOrCreate` hostPath mounted at the same path. |
| `emulate.tick` | `1s` | `SIM_EMULATE_TICK`. |
| `emulate.fuse` | `false` | Sets `SIM_HWEMU_FUSE=true` and runs the pod privileged with `/dev/fuse` and mount propagation `Bidirectional` on `emulate.root`, which must be on a shared mount of the host. The emulated agents need `agent.emulated.fuse` of the Joulie chart. |
| `emulate.receiverGate` | `profile` | `SIM_GPU_RECEIVER_GATE`. An unquoted `on` or `off`, which YAML reads as a boolean, is passed as the word. |
| `emulate.clusterSpec` | `""` | The spec as text, usually `--set-file emulate.clusterSpec=<spec.yaml>`, stored in the ConfigMap `joulie-simulator-cluster-spec`. A changed spec restarts the pod. |
| `emulate.clusterSpecConfigMap` | `""` | Instead, an existing ConfigMap holding the spec under the key `cluster-spec.yaml`. Set exactly one of the two. |
| `emulate.exporters` | `[]` | `SIM_EXPORTERS`, and one ServiceMonitor endpoint per exporter, with `honorLabels: false`. |
| `emulate.nodeSelector` | `joulie.io/hwemu-host: "true"` | Merged into `nodeSelector`: the simulator must run on the host whose root the emulated agent pods mount. |
| `rbac.workloadRuns` | `auto` | Whether the ClusterRole carries the rules of a v1 workload run: `auto` follows `emulate.enabled`; `true` and `false` force them. Any other value fails the render. |
| `extraEnv` | `{}` | Variables merged over `env` and over the ones the chart sets, so they win. In emulate mode, set `SIM_WORKLOAD_TRACE_PATH` here to start an implicit run. |

The chart always sets `POD_NAME` from the downward API and mounts its node class, hardware catalog and workload trace ConfigMaps as optional volumes, so a bare install starts without them.

### Simulator workloads and scenarios

The simulator (`simulator/cmd/simulator`) drives a workload trace through the cluster. `SIM_WORKLOAD_MODEL` picks the engine: `legacy` outside emulate mode, `v1` in emulate mode; the two are compared in [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md#two-engines-legacy-and-v1" >}}). The "Read by" column says which engine reads a variable.

| Variable | Read by | v1 default | Legacy default | Description |
|----------|---------|------------|----------------|-------------|
| `SIM_WORKLOAD_MODEL` | both | `v1` with `SIM_EMULATE=true` | `legacy` otherwise | `legacy` or `v1`. `v1` without emulate mode and `legacy` with it stop the start. |
| `SIM_WORKLOAD_TRACE_PATH` | both | (empty) | (empty) | A trace file, or a directory of them, loaded at startup: legacy replays it, v1 starts it as the implicit run `SIM_RUN_ID`. The chart sets `/etc/joulie-sim-trace/trace.jsonl` outside emulate mode; in emulate mode set it in the chart's `extraEnv`. |
| `SIM_WORKLOAD_TICK` | legacy | | `1s` | Period of the legacy workload loop. An unparsable value, or 0 or less, logs a warning and takes `1s`. |
| `SIM_WORKLOAD_IMAGE` | both | `registry.k8s.io/pause:3.9` | the same | Container image of every workload pod. |
| `SIM_BASE_SPEED_PER_CORE` | both | `1` | `1` | Work units one core, or one GPU, does per second at full speed, so a v3 trace job's ideal runtime is its `durationSec`. A run request's `baseSpeedPerCore` replaces it. An unparsable value takes `1`. |
| `SIM_WORKLOAD_OWNER` | v1 | `job` | fixed `pod` | `job`: one Job object per group; `pod`: bare pods. |
| `SIM_WORKLOAD_INTENT` | v1 | `annotation` | fixed `affinity` | How a pod declares its class: `annotation` (`joulie.io/workload-class`), `affinity` (`NotIn [eco]` on `joulie.io/power-profile` for a performance pod), `both` or `none`. |
| `SIM_WORKLOAD_COMPLETION` | v1 | `kwok` | fixed `delete` | `kwok`: a finished pod is annotated `sim.joulie.io/complete: "true"` and the KWOK Stage `pod-complete` moves it to `Succeeded`; `delete`: the pod is deleted. |
| `SIM_WORKLOAD_NAMESPACE` | v1 | (empty) | | Namespace of every pod; empty keeps each trace record's. |
| `SIM_WORKLOAD_NODE_SELECTOR` | v1 | `sim.joulie.io/cluster=<spec name>` | fixed `type=kwok` | `key=value` pairs, comma separated, added to every pod's node selector where the trace does not set the key. The default keeps workload pods on the cluster spec's nodes. |
| `SIM_WORKLOAD_TOLERATIONS` | v1 | `kwok.x-k8s.io/node=fake:NoSchedule` | fixed, the same | A JSON list of tolerations added to every pod; `[]` adds none. A GPU vendor's toleration is added only by the `ExtendedResourceToleration` admission plugin. |
| `SIM_WORKLOAD_JOB_TTL` | v1 | `10m` | | `ttlSecondsAfterFinished` of the Job objects; `0` deletes them as they finish. |
| `SIM_WORKLOAD_MAX_CREATES_PER_TICK` | both | `-1` (no cap) | `20` | Pods or Job objects created per tick; a positive count or `-1`. |
| `SIM_WORKLOAD_MAX_DELETES_PER_TICK` | legacy | | `10` | Finished pods deleted per tick; a positive count or `-1`. |
| `SIM_WORKLOAD_KUBE_QPS`, `SIM_WORKLOAD_KUBE_BURST` | both | `100`, `200` | `30`, `50` | Rate limits of the workload's Kubernetes client. |
| `SIM_WORKLOAD_STALL_REPORT` | v1 | `5m` | | How long a running job, or the whole run, must make no progress, with every Pending pod `Unschedulable`, before `joulie.sim.job.stalled` or `joulie.sim.run.stalled`. |
| `SIM_WORKLOAD_STALL_FORCE_COMPLETE` | legacy | | `120s` | How long a legacy job may make no progress before it is completed by force; `-1` disables it. |
| `SIM_RUNS_DIR` | v1 | `$SIM_HWEMU_ROOT/_runs` | | Directory of the run directories, `<SIM_RUNS_DIR>/<run id>/`, with the trace and the run's OTLP files. |
| `SIM_RUN_ID` | v1 | `run-<UTC yyyymmdd-hhmmss>` | | Id of the implicit run of `SIM_WORKLOAD_TRACE_PATH`; no slash, and not `.` or `..`. |
| `SIM_OTLP_FILE_INTERVAL` | v1 | `10s` | | Period of the cumulative metrics snapshot in `simulator.metrics.jsonl`; one more is written when the run ends. |

An invalid value stops the start with an error naming the variable, except `SIM_WORKLOAD_TICK` and `SIM_BASE_SPEED_PER_CORE`, which fall back to their defaults. A variable only the other engine reads logs a warning naming it.

**Run API.** In v1 emulate mode the simulator serves one run at a time on its HTTP port; in every other mode these paths answer 412.

| Path | Method | Answer |
|------|--------|--------|
| `/sim/v1/runs` | POST | Starts a run of `runId`, from a trace inside `SIM_RUNS_DIR`, with optional `options` overriding the settings above. 201 on success; 400 for a bad request or trace; 409 while a run is active or when the run id already has results. |
| `/sim/v1/runs/current` | GET | 200 with the active or last run's status (`phase`, `elapsedSeconds`, jobs per state, `runStalled`); 404 before the first run. |
| `/sim/v1/runs/current/finish` | POST | Ends the run; `?cleanup=true` also deletes its Job objects or pods. 200 with the final status. |

**Permissions.** `rbac.workloadRuns` adds the permissions a v1 run needs to the `joulie-simulator` ClusterRole (on pods, `batch` jobs and `joulie.io` nodetwins).

**KWOK Stage.** With `SIM_WORKLOAD_COMPLETION=kwok`, a pod ends only through the KWOK Stage `pod-complete` of `scripts/simcluster/manifests/stage-pod-complete.yaml`, which the simulated cluster applies; on another KWOK cluster apply it too, or set `SIM_WORKLOAD_COMPLETION=delete`. Details: [Configuration notes, Simulator workloads and scenarios](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-workloads-and-scenarios).

**Scenario runner.** `scripts/simcluster/scenario.sh` (`make simcluster-scenario SCENARIO=<file>`) has its own settings: [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md#settings" >}}). So do the other scripts of `scripts/simcluster`: [Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md#settings" >}}).

### Simulator telemetry export

In emulate mode the simulator (`simulator/cmd/simulator`) pushes OpenTelemetry metrics of every emulated node and of the facility as OTLP JSON over HTTP, such as to a Prometheus with `--web.enable-otlp-receiver`. Events are always written to `events.jsonl` and pushed only with a logs endpoint. Names: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

In every mode, `GET /otlp/telemetry/{node}` and `GET /telemetry/{node}` serve one node's OTLP JSON `ExportMetricsServiceRequest`, the format `TELEMETRY_CPU_HTTP_ENDPOINT` and `NODE_POWER_HTTP_ENDPOINT` read. `?format=legacy` serves the deprecated `packagePowerWatts` JSON until v0.4.0; a `format` other than `otlp` or `legacy` answers 400.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EXPORTERS` | (empty) | Comma-separated compatibility exporters to serve at `/exporters/<name>/metrics`: `kepler-0.7`, `kepler-0.12`, `dcgm-4.8.4`, `amd-dme-1.5.2` (ROCm device-metrics-exporter v1.5.2). `kepler-0.12` serves GPU series only with `SIM_KEPLER012_GPU`. An unknown name stops the start and lists the valid ones ([Compatibility exporters]({{< relref "/docs/simulator/metrics.md#compatibility-exporters-emulate-mode" >}})). |
| `SIM_KEPLER012_GPU` | `false` | `true` makes `kepler-0.12` also serve `kepler_node_gpu_*` for NVIDIA GPUs, as Kepler's experimental GPU option does. With the chart, set it in `extraEnv`. |
| `SIM_DCGM_HOSTNAME_LABEL` | `hostname` | Host label of `dcgm-4.8.4`: `hostname`, or `Hostname` as dcgm-exporter releases up to 4.8.2 spell it. |
| `SIM_OTEL_FILE_DIR` | the value of `SIM_DEBUG_PERSIST_DIR` (default `/tmp/joulie-simulator-debug`) | Directory of `events.jsonl`, one OTLP JSON `LogsData` object per line. Empty disables the files. |
| `SIM_OTEL_FILE_METRICS` | `false` | Also append every metrics export to `metrics.jsonl` in the same directory, one `MetricsData` object per line. Read with Go's `strconv.ParseBool`: `1`, `t`, `T`, `true`, `TRUE`, `True`, or the same spellings of false; any other value stops the start. |
| `POD_NAME` | the host name | Identifies this simulator instance in its own telemetry. The chart sets it from the downward API. |
| `OTEL_SDK_DISABLED` | `false` | `true`, in any case, turns off both pushes, as the OpenTelemetry specification reads it; any other value leaves them on. |
| `OTEL_SERVICE_NAME` | `joulie-simulator` | `service.name` of every resource. |
| `OTEL_RESOURCE_ATTRIBUTES` | (empty) | Extra resource attributes, `key=value` pairs separated by commas. They never override the node and instance attributes. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | (empty) | URL of the metrics push, used as is. For Prometheus: `http://<prometheus>:9090/api/v1/otlp/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | (empty) | URL of the events push, used as is. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (empty) | Base URL for both signals: `v1/metrics` and `v1/logs` are appended. A per-signal variable wins. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_PROTOCOL` | as the specification | Headers (`key=value,...`), timeout in milliseconds, `gzip` or none, and the protocol, which must be `http/json`. Each has `_METRICS_` and `_LOGS_` variants that win for their signal. |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | Milliseconds between metrics pushes. A `rate()` window over the pushed counters should cover at least 4 intervals. |
| `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` | `none` turns off that signal's push. |

The export follows the OpenTelemetry exporter specification, with `http/json` only and no default endpoint. A per-signal endpoint is used exactly as given, so it needs the full path. Details: [Configuration notes, Simulator telemetry export](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-telemetry-export).

Helm values of `charts/joulie-simulator`, rendered only when `emulate.enabled` is true:

| Value | Default | Effect |
|-------|---------|--------|
| `otel.metricsEndpoint` | `""` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. |
| `otel.logsEndpoint` | `""` | `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`. |
| `otel.exportIntervalMillis` | `10000` | `OTEL_METRIC_EXPORT_INTERVAL`, in milliseconds since Helm cannot parse durations. |
| `otel.resourceAttributes` | `{}` | `OTEL_RESOURCE_ATTRIBUTES`, joined as `key=value` pairs. |

For queries, Prometheus should promote `k8s.node.name` and `host.name` onto the data series (`otlp.promote_resource_attributes`); without that, the node name is joined from `target_info`. See [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}).

The simulator chart also ships the Grafana dashboard `Joulie - Simulated Cluster` ([Dashboard]({{< relref "/docs/simulator/simulated-cluster.md#dashboard" >}})), which needs `k8s.node.name` promoted. These values render whether or not `emulate.enabled` is true:

| Value | Default | Effect |
|-------|---------|--------|
| `grafanaDashboard.enabled` | `false` | Renders the ConfigMap `joulie-simulator-grafana-dashboard` with the dashboard, for a Grafana sidecar that loads dashboards from ConfigMaps. |
| `grafanaDashboard.labels` | `grafana_dashboard: "1"` | Labels of that ConfigMap: the ones the sidecar selects, kube-prometheus-stack's by default. |

### Simulator hardware profiles

The simulator (`simulator/cmd/simulator`) emulates a node from a hardware profile picked by name: one of the builtin profiles in `simulator/pkg/hwemu/profiles/`, or a local one.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_PROFILES_DIR` | (empty) | Directory of local hardware profiles: every `*.yaml` file directly in it, each with a `name` equal to its file name without `.yaml`. Empty, or a directory with no `*.yaml`, means the builtin profiles only. A missing directory, a `*.yml` file or a `.YAML` suffix is an error, never a fallback. |

A local profile may extend (`extends:`) a builtin or another local profile, but may not take a builtin's name. Details: [Configuration notes, Simulator hardware profiles](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#simulator-hardware-profiles).

## kubectl plugin

The `kubectl joulie` plugin reads `NodeTwin`, `NodeHardware`, `Node` and `Pod` objects through your kubeconfig and needs no configuration; without permission to list `NodeHardware` it shows no hardware. Install: [quickstart]({{< relref "/docs/getting-started/01-quickstart.md#kubectl-plugin" >}}).

| Command | Shows |
|---------|-------|
| `kubectl joulie`, `kubectl joulie status` | the cluster summary (measured, capped and TDP power, nodes and their power per class, pods per class, average headroom, cooling and PSU stress, PUE) and one row per managed node |
| `kubectl joulie top` | the live dashboard: the status screen refreshed every `--interval`, with a power sparkline per node; `↑` `↓` select, `enter` opens the node, `p` the pods, `s` sorts, `r` refreshes, `q` quits |
| `kubectl joulie node <node>` | one node: hardware, power, twin scores, the caps and governor presets planned and what its agent applied, why it got its profile, caps and governors (the `Why` panel), its pods |
| `kubectl joulie pods` | every pod on a managed node: its class, its node and the node's class; a performance pod on an eco node is flagged. A pod is `performance` when it has the `joulie.io/workload-class: performance` annotation, or a node selector or node affinity on `joulie.io/power-profile: performance` |
| `kubectl joulie version` | the plugin version |

| Flag | Default | Meaning |
|------|---------|---------|
| `-o`, `--output` | `table` | `json`: the same data as one JSON document, for scripts |
| `-w`, `--watch` | off | `status` and `pods`: open the live dashboard (`-W` is an alias) |
| `--interval` | `2s` | Refresh period of the live dashboard, at least `500ms` |
| `--no-color` | off | No colors. `NO_COLOR=1` does the same, and output that is not a terminal has none |
| `-n`, `--namespace` | all namespaces | `pods` only: list the pods of this namespace alone, which needs no permission to list pods cluster-wide. The node columns of the live dashboard then count these pods only |
| `--kubeconfig`, `--context`, `--cluster`, `--user`, `--server`, `--token` | as kubectl | As in kubectl |

The node table of `status` and `top`:

| Column | Shows |
|--------|-------|
| `NODE` | The node name |
| `HARDWARE` | The node's GPUs (count and model) or, on a node without GPUs, its CPUs (sockets and model), from its `NodeHardware` |
| `CLASS` | The `NodeTwin`'s schedulable class: `performance`, `eco`, `draining` or `unknown` |
| `POWER` | The measured node power, and a bar of it against the node's capped power (its TDP while no cap is known); the power source when nothing measures it |
| `TREND` | The twin's power trend in W/min; in `top`, a sparkline of the measured power, each block the share of the capped power it reaches |
| `CAP CPU·GPU` | The effective CPU and GPU caps in percent of the maximum; `-` for the GPU of a node without GPUs, and for both while the twin has no cap state |
| `HEADROOM`, `COOLING` | The twin's predicted power headroom and cooling stress, 0 to 100 |
| `CPU`, `MEM` | Requested over allocatable, summed over the pods bound to the node that have not finished |
| `GPU` | GPUs requested over allocatable, counting whole GPUs, MIG devices, AMD partitions and shared replicas, each weighted by its share of a GPU; `-` for a node without GPU resources |
| `PODS` | The pods on the node; `▲n` counts its performance pods |
| `AGENT` | The `AgentReporting` condition: `✓`, `✗` with its reason, `?` before the controller manager writes it |

The table drops the columns that do not fit the terminal, in this order: `MEM`, `TREND`, `HARDWARE`, `COOLING`, `HEADROOM`.

`-o json` prints `time`, `summary` (`nodes` per class, `pods`, `performancePods`, `misplacedPods`, `podsError`, `measuredPowerW`, `cappedPowerW`, `tdpW`, `powerTrendWPerMin`, `avgHeadroomPct`, `avgCoolingStressPct`, `avgPsuStressPct`, `avgPUE`, `agentsReporting`, `agentsNotReporting`), `nodes` for `status` and `node` (`name`, `class`, `cpuModel`, `sockets`, `gpuModel`, `gpus`, `powerSource`, `measuredPowerW`, `cappedPowerW`, `tdpW`, `powerTrendWPerMin`, `cpuCapPct`, `gpuCapPct`, `headroomPct`, `coolingStressPct`, `psuStressPct`, `cpuRequestedPct`, `memoryRequestedPct`, `gpuRequestedPct`, `pods`, `performancePods`, `agentReporting`, `agentReason`, `decisions`) and `pods` for `pods` and `node` (`namespace`, `name`, `class`, `node`, `nodeClass`, `misplaced`, `cpu`, `gpus`). `cpuCapPct` and `gpuCapPct` are left out while the twin has no cap state yet, and the GPU fields for a node without GPUs. `podsError` is set when the pods could not be listed; the pod counts are then unknown, not zero. A command's list is present even when empty (`"pods": []`). `decisions` lists the decision conditions of the node's `NodeTwin` (`EcoProfile`, `CPUEcoCap`, `GPUEcoCap`, `CPUGovernorPreset`, `GPUGovernorPreset`), each with `type`, `status` (`True` or `False`), `reason`, `message` and `lastTransitionTime`, and is left out until the controller manager writes them; the reasons are listed under [Why a node got its state]({{< relref "/docs/architecture/policy.md#why-a-node-got-its-state" >}}).
