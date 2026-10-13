---
title: "Configuration Reference"
weight: 50
---

Complete reference for all Joulie environment variables. These are set via Helm values or directly in the Deployment/DaemonSet manifests.

Defaults listed below are the **code defaults**. The Helm chart (`charts/joulie/values.yaml`) overrides some of them: notably, the controller manager `NODE_SELECTOR` defaults to `joulie.io/managed=true` in the chart even though the code default is `node-role.kubernetes.io/worker`.

## Images

The chart values `agent.image.tag`, `controllerManager.image.tag` and `schedulerExtender.image.tag` (and `image.tag` in the simulator chart) default to empty, which pulls the chart's `appVersion`. A chart installed from `oci://registry.cern.ch/joulie` carries the version it was released with, so it runs the images of that release. A chart installed from a checkout has `appVersion: latest` and runs the newest stable release. Set a tag only to run other images, for example your own build.

Pre-releases (a GitHub pre-release, or a version such as `0.2.0-rc0`) are published under their version only and never move `latest`.

## Agent

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_MODE` | `daemonset` | `daemonset` (one agent per node) or `pool` (shared agents with sharding) |
| `NODE_NAME` | (required in daemonset mode) | Name of the node this agent manages |
| `RECONCILE_INTERVAL` | `20s` | How often the agent reconciles desired state |
| `METRICS_ADDR` | `:8080` | Address for the Prometheus metrics endpoint |
| `SPEC_READ_TIMEOUT` | `5m` | How long the agent tolerates failed reads of its `NodeTwin` spec, leaving the node as it is. Past it the agent relaxes every cap and puts the governor baseline back, as when the spec is gone, until a read succeeds. A Go duration such as `90s`; a value it cannot parse keeps `5m` |
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

The agent always discovers the CPU and GPU governors of its node and publishes them in `NodeHardware` (see [Governor discovery]({{< relref "/docs/architecture/agent.md#governor-discovery" >}})). It writes the presets of its `NodeTwin`, `spec.cpu.governor` and `spec.gpu.governor`, only with `GOVERNOR_WRITES` `true`; the controller manager writes those fields only with its own `GOVERNOR_CONTROL` on (see "Governor control" under Controller manager). Before its first write the agent records the baseline in `NodeHardware`, and it puts it back when a field disappears or `GOVERNOR_WRITES` is turned off.

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `GOVERNOR_WRITES` | `false` | `false` | Write the governor presets. `false` reports `disabled` for a preset and writes no preset. An agent started with `false` after an earlier run changed the node puts the baseline back once (result `restored`), its only write, and then reports `disabled`. |
| `GOVERNOR_RESTORE_ON_EXIT` | `true` | `true` | On SIGTERM or SIGINT, put the baseline back before the agent exits, for every component it changed. A rolling update of the agent therefore restores the baseline and the new agent applies the preset again. SIGKILL and OOM kills restore nothing. |

- Writes use the agent's existing `/host-sys` mount, root user and privileges, the ones the DVFS fallback uses; the pod spec does not change. Without them the result is `refused` and `NodeHardware` shows `writable: false`.
- NVIDIA presets run `nvidia-smi -i <pci bus id> -lgc` or `-lmc`, so they need `nvidia-smi` in the agent (see "Agent GPU tools"); without it the result is `error`, as for caps.
- In the HTTP control mode (`TELEMETRY_CPU_CONTROL` or `TELEMETRY_GPU_CONTROL` `http`) and in pool mode the agent writes no governor and reports `unsupported`.
- What each preset becomes on each CPU driver and GPU family is in [CPU support]({{< relref "/docs/hardware/cpus.md#governors" >}}) and [GPU support]({{< relref "/docs/hardware/gpus.md#governors" >}}).
- The results, their retries and the restore rules are in [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}). Check them with:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,CPU-GOV:.status.controlStatus.cpuGovernor.result,CPU-NATIVE:.status.controlStatus.cpuGovernor.applied,GPU-GOV:.status.controlStatus.gpuGovernor.result,GPU-NATIVE:.status.controlStatus.gpuGovernor.applied
```

### Agent telemetry and control backends

| Variable | Default | Description |
|----------|---------|-------------|
| `TELEMETRY_CPU_SOURCE` | `host` | CPU telemetry source: `host` (the node's RAPL files), `http` (`TELEMETRY_CPU_HTTP_ENDPOINT`), `none` |
| `TELEMETRY_CPU_CONTROL` | `host` | CPU control backend: `host`, `http`, `none` |
| `TELEMETRY_GPU_CONTROL` | `host` | GPU control backend: `host`, `http`, `none` |
| `TELEMETRY_CPU_HTTP_ENDPOINT` | (empty) | HTTP endpoint for CPU telemetry, `{node}` replaced with the node name (e.g., `http://sim:18080/otlp/telemetry/{node}`). Each answer is one OTLP JSON `ExportMetricsServiceRequest`: the agent sums the `hw.type=cpu` components, from the rate of `hw.energy` or else the `hw.power` gauge, of the resource whose `k8s.node.name` is the node (a resource without it is read too). See [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}). The legacy `packagePowerWatts` JSON is still read, with one warning per endpoint and node, until v0.4.0. |
| `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT` | (empty) | HTTP endpoint for CPU control (e.g., `http://sim:18080/control/{node}`) |
| `TELEMETRY_CPU_CONTROL_MODE` | (empty) | CPU control mode override |
| `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | (empty) | HTTP endpoint for GPU control (e.g., `http://sim:18080/control/{node}`) |
| `TELEMETRY_GPU_CONTROL_MODE` | (empty) | GPU control mode override |
| `TELEMETRY_HTTP_TIMEOUT_SECONDS` | `2` | HTTP client timeout for telemetry/control requests, in whole seconds |

### Agent chart values

Helm values of `charts/joulie` that shape the agent pods. The DaemonSet and the emulated agents share one pod spec (`joulie.agentPodSpec` in `templates/_helpers.tpl`), so `agent.tolerations`, `agent.runtimeClassName` and the `agent.extra*` hooks apply to both; the pool StatefulSet takes none of them. Emulated agents ignore `agent.hostNetwork` and `agent.privileged`.

| Value | Default | Effect |
|-------|---------|--------|
| `agent.mode` | `daemonset` | `daemonset`: one agent per node matching `agent.daemonset.nodeSelector`. `pool`: the StatefulSet `joulie-agent-pool` (see "Agent pool mode"). `emulated`: one Deployment per `agent.emulated.nodes` entry, for the simulated cluster. `none`: no agent. |
| `agent.tolerations` | `nvidia.com/gpu` and `amd.com/gpu`, operator `Exists`, effect `NoSchedule` | Tolerations of the agent pods. GPU nodes usually carry their vendor's `NoSchedule` taint; an agent that does not tolerate it never runs there, and the caps the controller manager plans for those nodes are never enforced. Add any other taint your managed nodes carry; `[]` tolerates none. |
| `agent.runtimeClassName` | `""` | RuntimeClass of the agent pods, for example the one of the NVIDIA container runtime, which puts the node's driver tools into the container. |
| `agent.extraEnv` | `[]` | EnvVar entries added after the chart's own. |
| `agent.extraVolumes` | `[]` | Volume entries added after the chart's own. |
| `agent.extraVolumeMounts` | `[]` | VolumeMount entries added to the agent container after the chart's own. |
| `agent.emulated.root` | `/var/lib/joulie-hwemu` | The simulator's `SIM_HWEMU_ROOT`. Each emulated agent mounts its node's tree from there: `sys` at `/host-sys`, `cpuinfo` over `/proc/cpuinfo` (read-only), `state` at `/emu/state`, `bin` at `/emu/bin` (read-only), `<root>/lib` at `/emu/lib` (read-only), and `dev/nvidiactl` at `/dev/nvidiactl` for an entry with `devNvidiactl`. |
| `agent.emulated.hostNodeSelector` | `joulie.io/hwemu-host: "true"` | Node selector of the emulated agents: the node that holds the trees and runs the simulator. |
| `agent.emulated.path` | `/emu/bin:/usr/local/bin:/usr/bin:/bin` | `PATH` of the emulated agents, so the emulated GPU tools come first. |
| `agent.emulated.nodes` | `[]` | One entry per emulated node, as `simcluster agents` writes them: `name` (the Node, also the literal `NODE_NAME`), `profile`, `devNvidiactl`, `extraVolumes`, `extraVolumeMounts`. Entry `i` becomes the Deployment `joulie-agent-emu-<i>`, with strategy `Recreate`, no host network and no privileges. A name that is not a Node name, or is listed twice, fails the render. |

Emulated agents are what the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs: one per node the DaemonSet would reach, judged from the DaemonSet's own render.

### Agent GPU tools

The agent reads and caps GPUs by running `nvidia-smi` or `rocm-smi` from its `PATH`; it never runs `amd-smi`. The agent image (`gcr.io/distroless/static`) ships neither, and has no C library, no ELF loader and no Python, so mounting the host's tool alone is not enough: `nvidia-smi` is dynamically linked (`file $(command -v nvidia-smi)` on a GPU node shows it) and also needs the loader and the NVML library (`libnvidia-ml.so.1`), and `rocm-smi` is a Python script that needs `/usr/bin/env` and `python3`. The hooks above are where these come in: `agent.runtimeClassName` for a container runtime that injects the driver's tools and libraries, such as the NVIDIA one, or `agent.extraVolumes` and `agent.extraVolumeMounts` for a host directory, with `agent.extraEnv`.

Which combination works on a real GPU node is an open question: no recipe has been verified yet. After installing, check the GPU control status of every GPU node:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,GPU:.status.controlStatus.gpu.result,MESSAGE:.status.controlStatus.gpu.message
```

A tool that cannot run shows as `error` with a message starting `gpu inventory failed`, or as `blocked` with `no supported GPU backend detected` when the vendor cannot be told either; the GPUs then keep their limits. The simulated cluster reproduces both sides: `fakesmi` fails as the real tool does when something it needs is missing (`HWEMU_ROOTFS` under [Simulator hardware emulation](#simulator-hardware-emulation)), and the harness either mounts those files (`toolDelivery: mounted`) or not (`toolDelivery: none`).

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

The controller manager writes each node's measured power to `NodeTwin.status.powerMeasurement.measuredNodePowerW`. With `NODE_POWER_SOURCE=prometheus`, which the chart sets, it runs one grouped PromQL query per reconcile (`GET <address>/api/v1/query`) and reads every node from the answer; with `http` it reads one OTLP JSON document per node. The default preset `otel` reads the OpenTelemetry hardware metrics that producers push to Prometheus' OTLP receiver: the rate of `hw.energy`, or the `hw.power` gauge, of the `cpu` and `gpu` components, keyed by the resource attribute `k8s.node.name`. [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}) has the contract, every preset query and the Prometheus settings it needs.

The Chart column is what `charts/joulie/values.yaml` sets; "unset" means the code default applies.

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `PROMETHEUS_ADDRESS` | (empty) | `http://prometheus-operated.monitoring:9090` | Prometheus the measured inputs are read from. The default of `NODE_POWER_PROMETHEUS_ADDRESS` and `FACILITY_PROMETHEUS_ADDRESS`. |
| `PROMETHEUS_NAME_TRANSLATION` | `UnderscoreEscapingWithSuffixes` | unset | Prometheus' `otlp.translation_strategy`, which the controller manager must match: `UnderscoreEscapingWithSuffixes`, `NoUTF8EscapingWithSuffixes`, `UnderscoreEscapingWithoutSuffixes` or `NoTranslation`, case-insensitive. It spells the metric and label names of the `otel` presets, and under every facility preset the key labels of the zone, rack and node inputs, which are translated the same way. |
| `PROMETHEUS_RESOURCE_ATTRIBUTES` | `auto` | unset | Where the `otel` node power preset finds `k8s.node.name`. `labels`: on every series, which needs `k8s.node.name` in Prometheus' `otlp.promote_resource_attributes`. `target_info`: joined from `target_info` on `job` and `instance`, which needs no promotion. `auto`: the labels form first, then the `target_info` form when the first returns nothing or a series lacks the node label; it keeps the form that works until that form returns nothing or fails, and logs every switch. |
| `PROMETHEUS_RATE_WINDOW` | `5m` | `5m` | Window of every `rate()` the inputs run, and the longest interval the `http` source measures an energy rate over. At least `10s`, a whole number of seconds, and at least four export intervals of the producer, or `rate()` reads low. |
| `NODE_POWER_SOURCE` | `static` | `prometheus` | `static` (0 W), `prometheus` or `http`. An unknown value means `static`, with a warning. |
| `NODE_POWER_PRESET` | (empty: `otel`, or `custom` when `NODE_POWER_PROMETHEUS_QUERY` is set) | unset | The format the Prometheus query reads: `otel`, `kepler` (Kepler 0.10 and later), `kepler-legacy` (Kepler 0.7 to 0.9) or `custom`. A preset other than `custom` together with `NODE_POWER_PROMETHEUS_QUERY` stops the controller manager at start, so a query is never silently ignored. |
| `NODE_POWER_SCOPE` | `components` | `components` | `components`: the CPU packages plus the GPUs, the boundary of the twin's budget (CPU TDP plus GPU TDP). `host`: the whole host (`hw.host.energy` and `hw.host.power`, or Kepler 0.7 platform energy), which exceeds the budget under load. `kepler` with `host` stops the controller manager at start. |
| `NODE_POWER_NODE_LABEL` | per preset: the translated `k8s.node.name` (`k8s_node_name` by default) for `otel` and `custom`, `node_name` for `kepler`, `exported_instance` for `kepler-legacy` | unset | The label that names the node in the query result. |
| `NODE_POWER_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS` | unset | Prometheus of the node power query only. |
| `NODE_POWER_PROMETHEUS_QUERY` | (empty) | unset | Your own PromQL, in **watts**, for the `custom` preset. With `{node}` it runs once per node with the node name filled in, and every series it returns is summed (a warning names a node whose query returns more than one). Without `{node}` it is summed by `NODE_POWER_NODE_LABEL`. |
| `NODE_POWER_HTTP_ENDPOINT` | (empty) | unset | With `NODE_POWER_SOURCE=http`: a URL with `{node}`, answering one OTLP JSON `ExportMetricsServiceRequest` per node, such as the simulator's `/otlp/telemetry/{node}`. It is read with the rules of the `otel` preset under `NODE_POWER_SCOPE`; see [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}). The legacy `packagePowerWatts` JSON, CPU packages only, is still read with a warning until v0.4.0. |

Deprecated aliases, removed in the next minor release: `OPERATOR_NODE_POWER_SOURCE`, `OPERATOR_NODE_POWER_HTTP_ENDPOINT`, `OPERATOR_NODE_POWER_PROMETHEUS_ADDRESS`, `OPERATOR_NODE_POWER_PROMETHEUS_QUERY`. They are read only when the new name is unset, and the controller manager logs a warning naming the replacement.

A custom query must return instantaneous power in watts. Energy exporters
publish cumulative joules counters, which grow without bound, so they have to
be converted with `rate()`; the presets already do:

```promql
rate(kepler_node_platform_joules_total{exported_instance="{node}"}[5m])
```

`NODE_POWER_PRESET=kepler-legacy` with `NODE_POWER_SCOPE=host` reads the same
series without a custom query. Passing a counter directly makes
`measuredNodePowerW` climb forever, which drives power headroom negative and
cooling stress to 100% on every node. The controller manager logs a warning
when a reading exceeds twice the node's TDP.

**Upgrading to v0.3.0.**
- The chart now sets `NODE_POWER_SOURCE=prometheus`. Set `controllerManager.env.NODE_POWER_SOURCE=static` to keep 0 W.
- `NODE_POWER_SOURCE=prometheus` without a query now runs the `otel` preset; before, it read nothing.
- `KEPLER_AVAILABLE` is removed: nothing read it, and an alias would have switched every install to Kepler. Use `NODE_POWER_PRESET=kepler` or `kepler-legacy`.
- The facility default is now the `otel` preset. The `datacenter_*` names need `FACILITY_PRESET=legacy`, which is deprecated and removed in v0.4.0.
- `FACILITY_POLL_INTERVAL` of 0 or less now stops the controller manager at start.
- `FACILITY_ZONE_AMBIENT_METRIC_TEMPLATE` and `FACILITY_RACK_POWER_METRIC_TEMPLATE` are removed: they were documented as planned and never read. Use `FACILITY_ZONE_AMBIENT_TEMP_QUERY` and `FACILITY_RACK_POWER_QUERY`.
- The chart no longer sets `FACILITY_PROMETHEUS_ADDRESS`; it defaults to `PROMETHEUS_ADDRESS`.

### Power cap configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERFORMANCE_CAP_WATTS` | `5000` | Absolute CPU power cap for performance nodes (watts). The CPU of a GPU node gets it whatever the node's profile. |
| `ECO_CAP_WATTS` | `120` | Absolute CPU power cap for eco CPU-only nodes (watts). A GPU node never gets it: capping the CPU of a GPU node slows the GPU data feed by more than it saves. Earlier releases wrote it on eco GPU nodes too. |
| `CPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | CPU cap as percentage of max for performance nodes |
| `CPU_ECO_CAP_PCT_OF_MAX` | `60` | CPU cap as percentage of max for eco nodes |
| `CPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute watts instead of percentage |
| `GPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | GPU cap as percentage of max for performance nodes |
| `GPU_ECO_CAP_PCT_OF_MAX` | `60` | GPU cap as percentage of max for eco nodes |
| `GPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute GPU watts instead of percentage |
| `GPU_MODEL_CAPS_JSON` | `{}` | JSON map of GPU model name to `{"minCapWatts": N, "maxCapWatts": M}` |
| `GPU_PRODUCT_LABEL_KEYS` | `joulie.io/gpu.product,nvidia.com/gpu.product,amd.com/gpu.product-name,beta.amd.com/gpu.product-name,amd.com/gpu.product,amd.com/gpu.family` | Comma-separated node label keys to read the GPU product name from, first match wins. The AMD node labeller names the board in `amd.com/gpu.product-name` (and `beta.amd.com/gpu.product-name`); `amd.com/gpu.product` is the older key, and the family code in `amd.com/gpu.family` (for example `GC_11_0_0`) names no board, so it comes last. When its GPU inventory fails, the agent reads these default labels, in this order; it does not read this variable. |

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

Off by default. With `GOVERNOR_CONTROL` on, the controller manager writes a vendor-neutral governor preset into each managed node's `NodeTwin`, `spec.cpu.governor` and `spec.gpu.governor`, chosen by the profile the node runs at: a draining node keeps the performance presets until its performance pods finish, as it keeps the performance caps. The agent maps a preset to the node's native governor, energy-performance preference, performance level or clock locks, and writes no preset unless its own `GOVERNOR_WRITES` is `true` (see "Agent governor control").

- A node gets only a preset its agent lists in `NodeHardware` `status.cpu.governor.presets` or `status.gpu.governor.presets`. Any other preset is not written and nothing replaces it, `auto` included, so a setting the node's operator chose stays; the controller manager logs one warning per node. A node whose agent does not discover governors lists none and gets none.
- A GPU node gets no CPU governor unless `CPU_GOVERNOR_ON_GPU_NODES` is `true`, the rule the CPU cap follows. Only GPU nodes get a GPU governor.
- An empty preset writes none for that profile.
- Turning control off, for one component or both, removes the field in the next spec write; the agent then puts back the state it recorded before its first write.
- Nothing changes on a node until its agent also runs with `GOVERNOR_WRITES` `true` (see "Agent governor control").
- A value the controller manager cannot use logs a warning and keeps the default; an unknown `GOVERNOR_CONTROL` means `off`.

| Variable | Default | Description |
|----------|---------|-------------|
| `GOVERNOR_CONTROL` | `off` | `off`, `cpu` (CPU governors only), `gpu` (GPU governors only) or `all`. |
| `CPU_GOVERNOR_PERFORMANCE_PRESET` | `performance` | CPU preset of performance nodes: `performance` (highest frequency at any load), `balanced` (demand-driven, quick ramp) or `efficiency` (demand-driven with an energy bias). |
| `CPU_GOVERNOR_ECO_PRESET` | `efficiency` | CPU preset of eco nodes. |
| `CPU_GOVERNOR_ECO_BOOST` | empty | `on` or `off`: frequency boost (turbo) with the eco preset, on nodes whose agent found a boost control (`status.cpu.governor.boost`). Empty leaves boost alone. |
| `CPU_GOVERNOR_ON_GPU_NODES` | `false` | Write the CPU governor on GPU nodes too. |
| `GPU_GOVERNOR_PERFORMANCE_PRESET` | empty | GPU preset of performance nodes: `auto`, `sustained`, `efficient`, `low`, `peak`, or empty for none. |
| `GPU_GOVERNOR_ECO_PRESET` | `efficient` | GPU preset of eco nodes; set it to an empty value to write none. |
| `GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX` | `80` | Compute clock ceiling of the `efficient` preset, in percent of the GPU's maximum compute clock, in (0, 100]. Written only with `efficient`; a value out of range gives 80. The default is provisional: in the simulator's governor exploration 75 slowed eco work past the eco class's 20 % p95 limit and 80 did not (see [Model changes]({{< relref "/docs/simulator/model-changes.md" >}})). |
| `GPU_GOVERNOR_MODEL_PRESETS_JSON` | `{}` | Presets per GPU model: `{"<model>": {"performance": "<preset>", "eco": "<preset>", "ecoCeilingPctOfMax": <pct>}}`. Keyed like `GPU_MODEL_CAPS_JSON`, by the GPU product label value, and also matched against the GPU model the agent reports. A field left out keeps the default above, `""` writes none for that model, and an invalid field is dropped with a warning. |

GPU presets: `auto` is the vendor default. `sustained` holds the data-path clocks (memory, fabric) at their maximum and keeps idle gating, leaving the compute clock free to idle; a demand-driven governor sees only a GPU's own kernels, so a GPU that receives a peer transfer can stay at low data-path clocks, and on families where those clocks bound the transfer, `sustained` removes that limit at some idle power. No default selects it: opt a model in through `GPU_GOVERNOR_MODEL_PRESETS_JSON` once a measurement on that model shows the gain. `efficient` puts the compute clock under the ceiling and leaves the data path dynamic. `low` and `peak` are for experiments; `peak` turns idle gating off on AMD GPUs. These defaults are provisional until measured. [Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) says what each preset means, and the [controller manager]({{< relref "/docs/architecture/controller-manager.md#governor-presets" >}}) page which node gets which.

The overview dashboard's "Agent: Governors" row plots the presets in effect (`joulie_agent_governor_state`) and the writes per result (`joulie_agent_governor_writes_total`).

### Facility metrics

The facility poller reads six inputs from Prometheus, each with one grouped query per poll. The outdoor ambient temperature, IT power and cooling power are one value each; the twin uses the IT power for PSU stress and the ambient temperature for cooling stress, and the controller manager logs PUE as `(IT power + cooling power) / IT power`. The zone, rack and node inputs return one series per cooling zone, rack or node (see "Node topology" below). An input whose query fails or returns nothing is unknown until a later poll reads it, never the value of an earlier poll. Every query is counted in `joulie_twin_input_queries_total{input,result}`, and the three single values are exported as `joulie_twin_facility_*` ([Metrics]({{< relref "/docs/architecture/metrics.md#twin-input-metrics" >}})).

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `ENABLE_FACILITY_METRICS` | `false` | `false` | Poll the facility inputs. The first poll runs at start. |
| `FACILITY_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS`, or `http://prometheus-operated.monitoring:9090` when that is unset | unset | Prometheus of the facility queries only. |
| `FACILITY_POLL_INTERVAL` | `30s` | `30s` | How often the inputs are polled. 0 or less stops the controller manager at start. |
| `FACILITY_PRESET` | `otel` | `otel` | `otel`: the facility, cooling zone, rack and host metrics of the [input contract]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}), all six inputs. `legacy`: the `datacenter_ambient_temperature_celsius`, `datacenter_total_it_power_watts` and `datacenter_cooling_power_watts` names read before v0.3.0, the three single inputs only; deprecated, removed in v0.4.0. `custom`: only the inputs whose query is set below. |
| `FACILITY_AMBIENT_TEMP_QUERY` | from the preset | unset | Outdoor ambient temperature, one value in degrees Celsius. |
| `FACILITY_IT_POWER_QUERY` | from the preset | unset | Power of the IT equipment, one value in watts. |
| `FACILITY_COOLING_POWER_QUERY` | from the preset | unset | Power of the cooling, one value in watts. |
| `FACILITY_ZONE_AMBIENT_TEMP_QUERY` | from the preset | unset | Ambient temperature of each cooling zone, keyed by the translated `joulie.cooling_zone.name` label (`joulie_cooling_zone_name` by default), whose values are the nodes' `joulie.io/cooling-zone` label values. |
| `FACILITY_RACK_POWER_QUERY` | from the preset | unset | Power of each rack in watts, keyed by the translated `joulie.rack.name` label (`joulie_rack_name`), whose values are the nodes' `joulie.io/rack` label values. |
| `FACILITY_NODE_INLET_TEMP_QUERY` | from the preset | unset | Inlet temperature of each node, keyed by the translated `k8s.node.name` label (`k8s_node_name`). |

A query variable replaces its input's query under any preset. A bare metric name is aggregated by the input's key label, `avg` for a temperature and `sum` for a power, so `FACILITY_IT_POWER_QUERY=ups_output_power_watts` runs `sum(ups_output_power_watts)`. Any other query is kept as written and must return one series, or for the zone, rack and node inputs one series per key carrying the key label.

Deprecated aliases, removed in v0.4.0: `FACILITY_AMBIENT_TEMP_METRIC`, `FACILITY_IT_POWER_METRIC`, `FACILITY_COOLING_POWER_METRIC`, the former names of the first three query variables. They are read only when the new name is unset, with a warning naming the replacement, and the bare metric name they hold keeps its meaning.

### Node topology

Joulie supports optional per-rack PSU stress and per-zone cooling stress. This is activated by adding standard node labels:

- `joulie.io/rack`: physical rack identifier (e.g., `rack-1`)
- `joulie.io/cooling-zone`: cooling zone identifier (e.g., `zone-a`)

When these labels are present, the controller manager computes PSU stress per rack instead of cluster-wide: the rack's power is the facility's rack power input (`FACILITY_RACK_POWER_QUERY`, keyed by the `joulie.io/rack` value) when it reads that rack, otherwise the sum of the estimated power of the rack's nodes. The ambient temperature of a node's twin is the most local one known: the node's inlet temperature (`FACILITY_NODE_INLET_TEMP_QUERY`), then its cooling zone's (`FACILITY_ZONE_AMBIENT_TEMP_QUERY`, keyed by the `joulie.io/cooling-zone` value), then the outdoor ambient. The twin model interfaces remain the same; topology just groups nodes for more accurate stress computation.

Nodes without topology labels fall back to cluster-wide stress computation, and still use their inlet temperature when the facility reads one.

## Scheduler extender

| Variable | Default | Description |
|----------|---------|-------------|
| `EXTENDER_ADDR` | `:9876` | Bind address for the scheduler extender HTTP server |
| `METRICS_ADDR` | `:9877` | Address for the scheduler's Prometheus metrics endpoint |
| `CACHE_TTL` | `30s` | Minimum interval between rebuilding the scoring maps from the informer cache. Reads no longer hit the API server; NodeTwin and NodeHardware are watched. |
| `TWIN_STALENESS_THRESHOLD` | `5m` | Duration after which NodeTwin data is considered stale |

With `schedulerExtender.hostNetwork: true` the chart deploys the extender with strategy `Recreate`: an old and a new pod cannot hold the host port on one node, so a rolling update would never finish. Pods are still scheduled while the extender restarts only if the scheduler configuration marks it `ignorable: true`; otherwise also set `schedulerExtender.nodeName`, so the new extender pod does not wait for the scheduler that waits for it.

## Simulator

This section covers the binaries under `simulator/cmd`. Each subsection names the binary that reads its variables.

### Simulator thermal model

The simulator (`simulator/cmd/simulator`) reads these variables at startup to build its base hardware profile. A node class in `SIM_NODE_CLASS_CONFIG` overrides one per node with the key in parentheses, under the class's `model:`.

Temperature follows a first-order lag toward ambient plus power divided by the watts per degree, and is reported in the telemetry response. It reduces throughput only when thermal throttling is enabled, which it is not by default: both thresholds of a device are 0, which means off. Setting either variable of a pair turns throttling on for that device, and the other one takes the former default shown in its row. A node class key works differently: a threshold the class leaves unset comes from the base profile, which is 0 unless the environment sets the pair, so set both in a class, the full threshold above the start. A class full threshold alone is accepted and throttles linearly from 0 °C, so even an idle node is throttled. A class start threshold alone is rejected: every class in `SIM_NODE_CLASS_CONFIG` is ignored, with only a warning in the log, and all nodes run on the base profile. For a GPU start this happens only when the profile has GPUs, that is with `SIM_GPU_COUNT` or a class `gpu.count` above 0. With no GPU count it is accepted and GPU throttling stays off. See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}), sections 5.5 and 6.6, for why throttling is opt-in.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_CPU_AMBIENT_TEMP_C` | `24` | CPU ambient temperature in °C (`cpuAmbientTempC`) |
| `SIM_CPU_THERMAL_TAU_MS` | `4000` | CPU thermal time constant in milliseconds (`cpuThermalTauMs`) |
| `SIM_CPU_WATTS_PER_DELTA_C` | `8` | CPU watts per °C above ambient at equilibrium (`cpuWattsPerDeltaC`) |
| `SIM_CPU_THERMAL_THROTTLE_START_C` | `0` (off) | CPU temperature in °C where thermal throttling starts (`cpuThermalThrottleStartC`). `85` when only `SIM_CPU_THERMAL_THROTTLE_FULL_C` is set, the default before throttling became opt-in. |
| `SIM_CPU_THERMAL_THROTTLE_FULL_C` | `0` (off) | CPU temperature in °C where thermal throttling reaches throughput 0 (`cpuThermalThrottleFullC`). `97` when only `SIM_CPU_THERMAL_THROTTLE_START_C` is set. |
| `SIM_GPU_AMBIENT_TEMP_C` | `24` | GPU ambient temperature in °C (`gpu.ambientTempC`) |
| `SIM_GPU_THERMAL_TAU_MS` | `2500` | GPU thermal time constant in milliseconds (`gpu.thermalTauMs`) |
| `SIM_GPU_WATTS_PER_DELTA_C` | `12` | GPU watts per °C above ambient at equilibrium (`gpu.wattsPerDeltaC`) |
| `SIM_GPU_THERMAL_THROTTLE_START_C` | `0` (off) | GPU temperature in °C where thermal throttling starts (`gpu.thermalThrottleStartC`). `82` when only `SIM_GPU_THERMAL_THROTTLE_FULL_C` is set. |
| `SIM_GPU_THERMAL_THROTTLE_FULL_C` | `0` (off) | GPU temperature in °C where thermal throttling is full (`gpu.thermalThrottleFullC`). `92` when only `SIM_GPU_THERMAL_THROTTLE_START_C` is set. |

### Simulator power limits

The simulator (`simulator/cmd/simulator`) reads these variables at startup too. For a node whose labels match the hardware catalogue, a variable that is unset leaves the field to the catalogue; the built-in default applies only to hardware the catalogue does not know. A node class key (in parentheses) wins over both, and a value of 0 or less counts as unset. See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}), section 3.1.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_P_MAX_W` | sockets x catalogue TDP, else `420` | Peak CPU power of the generic model, at full load and full frequency (`pMaxW`). A node with a measured curve uses the curve instead. |
| `SIM_CPU_SOCKET_CAP_MAX_W` | catalogue TDP, else `400` | Upper end of the per-socket CPU cap: the node cap is split evenly over the sockets and clamped to this range (`cpuSocketCapMaxW`). |
| `SIM_CPU_SOCKET_CAP_MIN_W` | catalogue cTDP low end, else 0.55 x TDP, else `120` | Lower end of the per-socket CPU cap (`cpuSocketCapMinW`). |
| `SIM_GPU_MAX_WATTS_PER_GPU` | catalogue `maxBoardPowerW`, else `300` | Board power of one GPU at full load, and its highest cap (`gpu.maxWattsPerGpu`). |
| `SIM_GPU_MIN_CAP_WATTS_PER_GPU` | catalogue `minBoardPowerW`, else half the maximum, else `80` | Lowest cap of one GPU (`gpu.minCapWattsPerGpu`). |

A catalogue minimum above a maximum set by a class or a variable is lowered to that maximum. A measured two-socket curve from the catalogue is scaled by sockets/2 on a node with another socket count.

**Upgrading to v0.3.0.**
- A node the catalogue matches now takes the five values above from the catalogue: an H100 SXM caps between 300 and 700 W instead of 80 and 300 W, and two EPYC 9534 sockets peak at 560 W instead of 420 W. Set the variable, or the class key, to keep a value.
- A one-socket node with a measured curve now draws half the two-socket curve.
- `SIM_POD_W`, `SIM_DVFS_DROP_W_PER_PCT`, `SIM_GPU_ALPHA_UTIL` and `SIM_GPU_BETA_CAP`, and the node class keys `podW`, `dvfsDropWPerPct`, `noiseStddevW` and `gpu.powerModel`, are removed: nothing read them. The simulator still starts with them set and logs one warning per variable or key.

### Simulator physics step

The simulator (`simulator/cmd/simulator`) advances the model of a node outside the emulate-mode cluster spec only in a physics step, in simulated time; a read or a control never advances it. A step splits its time into equal substeps, and the energy it books is the integral of the instantaneous power over them. See [Power Simulator]({{< relref "/docs/simulator/power-simulator.md#the-physics-step" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_POLL_INTERVAL` | `15s` | Kubernetes mode: how often the simulator lists pods and nodes. A value of 0 or less logs a warning and takes `15s`. |
| `SIM_PHYSICS_TICK` | `1s` | Kubernetes mode without emulate mode: every modelled node is stepped by the wall time since the last tick, in substeps of at most 100 ms. A value of 0 or less logs a warning and takes `1s`. In emulate mode `SIM_EMULATE_TICK` steps them instead, and standalone mode steps them on its own tick. |
| `SIM_SA_SUBSTEP` | `SIM_SA_DT` | Standalone mode: the longest physics substep, in simulated seconds. Each `SIM_SA_DT` tick is split into equal substeps no longer than this; the default is one substep per tick. A shorter substep follows the cap, frequency and thermal lags more closely at the cost of run time. |

### Simulator hardware emulation

`fakesmi` (`simulator/cmd/fakesmi`) stands in for `nvidia-smi`, `rocm-smi` and `amd-smi` on an emulated node. It runs as the tool its name says, through a `bin/<tool>` link, and reads and writes the node's emulated hardware files instead of a driver. The agent does not read these variables; only `fakesmi` does.

| Variable | Default | Description |
|----------|---------|-------------|
| `HWEMU_SYS_ROOT` | `/host-sys` | The emulated node's `sys/` directory, where the amdgpu PCI and hwmon attributes live. The default is where an emulated agent pod mounts it. |
| `HWEMU_STATE_ROOT` | `/emu/state` | The emulated node's `state/` directory: `tools.json`, which lists the tools the node has and the variant each one emulates, the NVML state of NVIDIA GPUs, and the per-card AMD values sysfs has no text attribute for (`amdgpu/card<N>/`). The default is where an emulated agent pod mounts it. |
| `HWEMU_ROOTFS` | `/` | The file system the real tool would run in, where `fakesmi` looks for what the tool needs before it runs (below). The default is the agent container's own root. |
| `HWEMU_UNPRIVILEGED` | `false` | Run the tools as a user without root would: `1`, `true` or another word Go's `strconv.ParseBool` reads as true; anything else, unset included, means root. A set then changes nothing: `nvidia-smi -pl`, `-pm`, `-lgc`, `-lmc`, `-rgc` and `-rmc` exit 4 with `Insufficient Permissions`; `amd-smi set` and `reset` fail with `PermissionError: Command requires elevation`; a `rocm-smi` set exits 0 with the failure only at `--loglevel debug`, and prints nothing at all on a family whose profile sets `gpus.dpm.unprivilegedSetIsNoop`. Reads work as with root. |

A tool that `tools.json` does not list exits 127 with `fakesmi: <tool> is not installed on this emulated node`.

A listed tool first checks the requirements of its `tools.json` entry, in order, under `HWEMU_ROOTFS`: each requirement is a list of paths, and one existing path meets it. The first requirement with none of its paths present ends the call, before any GPU state is read, with the exit code and message the real tool gives without that file. The requirements are profile data (`gpus.tools[].requires`); a tool without requirements always runs. The builtin profiles `nvidia-h100-nvl-8gpu`, `nvidia-h100-sxm-8gpu`, `amd-instinct-mi300x-8gpu` and `amd-radeon-pro-w7900-4gpu` list:

| Tool | Requirement (any one path) | Exit | Message |
|------|----------------------------|------|---------|
| `nvidia-smi` | `/lib64/ld-linux-x86-64.so.2`, the ELF loader | `127` | none: exec fails, which a shell reports as 127 |
| `nvidia-smi` | `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1` or `/usr/lib64/libnvidia-ml.so.1` | `12` | `NVIDIA-SMI couldn't find libnvidia-ml.so library in your system. ...`; 12 is nvidia-smi's "NVML Shared Library couldn't be found or loaded" |
| `rocm-smi`, `amd-smi` | `/usr/bin/env`, from the scripts' `#!/usr/bin/env python3` | `127` | none |
| `rocm-smi`, `amd-smi` | `/usr/bin/python3` or `/usr/local/bin/python3` | `127` | `/usr/bin/env: 'python3': No such file or directory` |

An empty `HWEMU_ROOTFS` is the default, `/`. Only a caller of package `simulator/pkg/hwemu/smi` that builds its own `Env` with an empty `RootFS` skips the checks, as the in-process agent tests do.

### Simulator emulate mode

In emulate mode the simulator (`simulator/cmd/simulator`) emulates the bare-metal nodes a cluster spec lists: for each one it keeps a sysfs tree, `/proc/cpuinfo` and GPU tool state under `SIM_HWEMU_ROOT/<node>/`, and advances them on a fixed tick, so an unmodified agent pod that mounts them discovers and caps the node as it would a real one. Nodes the spec does not list keep the label-based model. `/control/<node>` answers 409 for an emulated node, whose limits are written to its files instead. See [Emulate mode]({{< relref "/docs/simulator/simulator.md#emulate-mode" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EMULATE` | `false` | Turn emulate mode on. The simulator refuses to start when `SIM_STANDALONE` is also set, or when `SIM_CLUSTER_SPEC` is empty. Workloads run on the v1 engine; `SIM_WORKLOAD_MODEL=legacy` is refused, since a legacy trace cannot drive emulated nodes (see [Simulator workloads and scenarios](#simulator-workloads-and-scenarios)). |
| `SIM_CLUSTER_SPEC` | (empty) | Path of the cluster spec, a `sim.joulie.io/v1alpha1` `ClusterSpec` such as `tests/simcluster/specs/smoke.yaml`. It names each node, its hardware profile, pool, rack and cooling zone, and the facility. |
| `SIM_HWEMU_ROOT` | `/var/lib/joulie-hwemu` | Directory of the emulated trees: one `<node>/` per spec node, `lib/fakesmi` and `facility-ledger.json`. One simulator holds it at a time; a second one fails to start. |
| `SIM_EMULATE_TICK` | `1s` | Physics tick: every spec node advances to wall time once per tick. A tick that runs late is counted and the next one catches up. The nodes outside the spec are stepped on the same tick (see "Simulator physics step"). |
| `SIM_GPU_RECEIVER_GATE` | `profile` | The receiver gate of the peer links between the GPUs of a node, which bounds the copies into a GPU by that GPU's data-path state: `profile` follows each hardware profile's `gpus.transfer.receiverGate.enabled`, off in every builtin profile; `off` disables it everywhere; `on` enables it on every GPU profile with the profile's fractions, for what-if runs (a profile that leaves a fraction unset lets the whole link through). Any other value logs a warning naming the variable and takes `profile`. A gated copy slows the v1 job that makes it (see [peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})). |
| `SIM_EMULATE_WORKERS` | `0` | Nodes stepped in parallel; `0` means `GOMAXPROCS`, at most 12. A step is mostly system calls on the node's files, which contend past about 12 workers (see [Tick cost]({{< relref "/docs/simulator/simulator.md#tick-cost" >}})); a set value is used as given. |
| `SIM_FAKESMI_SOURCE` | `/usr/local/lib/joulie/fakesmi` | The `fakesmi` binary to install as `SIM_HWEMU_ROOT/lib/fakesmi`, where every node's `bin/nvidia-smi`, `bin/rocm-smi` and `bin/amd-smi` link points. It is replaced by rename, and only when its sha256 changed. The simulator image ships it at the default path. Empty skips the copy, for a run outside the image. |
| `KUBECONFIG` | client-go loading rules | The cluster the simulator watches, so it can also run on a workstation. Without a kubeconfig it uses the in-cluster configuration. The simulator logs the API server it uses. |

Hardware profiles are the builtin ones in `simulator/pkg/hwemu/profiles/`, plus local ones from `SIM_PROFILES_DIR`. The facility values come from the spec's `facility` block and fall back to `SIM_FACILITY_AMBIENT_TEMP_C`, `SIM_FACILITY_TEMP_AMPLITUDE_C` and `SIM_FACILITY_TEMP_PERIOD_H`.

**Restarts.** An existing tree is resumed, not rendered again: energy counters, the limits agents wrote and NVML state continue from disk, and only missing files are put back, so nothing an agent pod holds open is replaced. Exported energy counters never decrease across a restart; they continue from a ledger in the tree. No energy accrues while the simulator is down, so a restart shows as a gap in the rate, never as a reset.

**Recovery.** A tree rendered for another profile, node or profile values, or one with an entry of the wrong kind (a file where a directory belongs), stops the start with an error that names the node. Make sure no agent pod mounts that node, remove `SIM_HWEMU_ROOT/<node>`, and start the simulator again: the node is rendered fresh, with its counters back at zero.

Helm values of `charts/joulie-simulator`:

| Value | Default | Effect |
|-------|---------|--------|
| `emulate.enabled` | `false` | Sets `SIM_EMULATE`. Also runs the pod as root with strategy `Recreate`, adds a readiness probe on `/readyz` (ready once every tree exists), and leaves out `SIM_WORKLOAD_TRACE_PATH`: a run starts through the run API instead. The chart fails unless `replicas` is 1. |
| `emulate.root` | `/var/lib/joulie-hwemu` | `SIM_HWEMU_ROOT`, and a `DirectoryOrCreate` hostPath mounted at the same path. |
| `emulate.tick` | `1s` | `SIM_EMULATE_TICK`. |
| `emulate.receiverGate` | `profile` | `SIM_GPU_RECEIVER_GATE`. An unquoted `on` or `off`, which YAML reads as a boolean, is passed as the word. |
| `emulate.clusterSpec` | `""` | The spec as text, usually `--set-file emulate.clusterSpec=<spec.yaml>`. The chart stores it in the ConfigMap `joulie-simulator-cluster-spec`, mounts it at `/etc/joulie-sim-spec` and points `SIM_CLUSTER_SPEC` at it. A changed spec restarts the pod. |
| `emulate.clusterSpecConfigMap` | `""` | Instead, an existing ConfigMap holding the spec under the key `cluster-spec.yaml`. Set exactly one of the two. |
| `emulate.exporters` | `[]` | `SIM_EXPORTERS`, and one ServiceMonitor endpoint per exporter, with `honorLabels: false`. |
| `emulate.nodeSelector` | `joulie.io/hwemu-host: "true"` | Merged into `nodeSelector`: the simulator must run on the host whose root the emulated agent pods mount. |

The chart always sets `POD_NAME` from the downward API and mounts its node class, hardware catalog and workload trace ConfigMaps as optional volumes, so a bare install starts without them.

### Simulator workloads and scenarios

The simulator (`simulator/cmd/simulator`) drives a workload trace through the cluster with one of two engines, `SIM_WORKLOAD_MODEL`:

- **legacy**, outside emulate mode: the engine experiments 01 to 03 were measured with. It creates bare pods and deletes each when its work is done, infers the class from the pod's affinity, and completes a job by force after it stalls for `SIM_WORKLOAD_STALL_FORCE_COMPLETE`.
- **v1**, in emulate mode: each group of trace jobs becomes one `batch/v1` Job (Indexed when larger than one, `backoffLimit: 0`), pods carry their class as the annotation `joulie.io/workload-class`, a finished pod is marked complete for KWOK instead of deleted, and nothing is completed by force: a job or a run that makes no progress is reported as stalled. Each job's progress follows the throughput of the emulated hardware it runs on, energy is attributed from the emulated nodes' energy ledgers, and the results are OTLP JSON Lines in the run's directory. See [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}).

| Variable | Read by | v1 default | Legacy default | Description |
|----------|---------|------------|----------------|-------------|
| `SIM_WORKLOAD_MODEL` | both | `v1` with `SIM_EMULATE=true` | `legacy` otherwise | `legacy` or `v1`. `v1` without emulate mode and `legacy` with it stop the start. |
| `SIM_WORKLOAD_TRACE_PATH` | both | (empty) | (empty) | A trace file, or a directory of them, loaded at startup. Legacy replays it from startup. v1 starts it as an implicit run with id `SIM_RUN_ID`; a run that fails to start is logged and the run API stays open. The chart sets `/etc/joulie-sim-trace/trace.jsonl` outside emulate mode. |
| `SIM_WORKLOAD_TICK` | legacy | | `1s` | Period of the legacy workload loop. A value that does not parse as a duration, or of 0 or less, logs a warning and takes `1s`. A v1 run advances once per emulator tick (`SIM_EMULATE_TICK`) and does not read it. |
| `SIM_WORKLOAD_IMAGE` | both | `registry.k8s.io/pause:3.9` | the same | Container image of every workload pod. |
| `SIM_BASE_SPEED_PER_CORE` | both | `1` | `1` | Work units one core, or one GPU, does per second at full speed. A v3 trace sizes work with it, so a job's ideal runtime is its `durationSec`. A run request's `baseSpeedPerCore` replaces it for that run. A value that does not parse as a number is ignored without a warning and takes `1`. |
| `SIM_WORKLOAD_OWNER` | v1 | `job` | fixed `pod` | `job`: one Job object per group; `pod`: bare pods. |
| `SIM_WORKLOAD_INTENT` | v1 | `annotation` | fixed `affinity` | How a pod declares its class: `annotation` (`joulie.io/workload-class`), `affinity` (a performance pod gets `NotIn [eco]` on `joulie.io/power-profile`, ANDed into each required term of the trace's affinity), `both` or `none`. |
| `SIM_WORKLOAD_COMPLETION` | v1 | `kwok` | fixed `delete` | `kwok`: a finished pod is annotated `sim.joulie.io/complete: "true"` and the KWOK Stage `pod-complete` moves it to `Succeeded`; `delete`: the pod is deleted. |
| `SIM_WORKLOAD_NAMESPACE` | v1 | (empty) | | Namespace of every pod; empty keeps each trace record's. |
| `SIM_WORKLOAD_NODE_SELECTOR` | v1 | `sim.joulie.io/cluster=<spec name>` | fixed `type=kwok` | `key=value` pairs, comma separated, added to every pod's node selector where the trace does not set the key. The default keeps workload pods off every Node outside the cluster spec, such as the one running the simulator. |
| `SIM_WORKLOAD_TOLERATIONS` | v1 | `kwok.x-k8s.io/node=fake:NoSchedule` | fixed, the same | A JSON list of tolerations added to every pod; `[]` adds none. Nothing adds a GPU vendor's toleration for a GPU request unless the cluster runs the `ExtendedResourceToleration` admission plugin. |
| `SIM_WORKLOAD_JOB_TTL` | v1 | `10m` | | `ttlSecondsAfterFinished` of the Job objects; `0` deletes them as they finish. |
| `SIM_WORKLOAD_MAX_CREATES_PER_TICK` | both | `-1` (no cap) | `20` | Pods or Job objects created per tick; a positive count or `-1`. |
| `SIM_WORKLOAD_MAX_DELETES_PER_TICK` | legacy | | `10` | Finished pods deleted per tick; a positive count or `-1`. |
| `SIM_WORKLOAD_KUBE_QPS`, `SIM_WORKLOAD_KUBE_BURST` | both | `100`, `200` | `30`, `50` | Rate limits of the workload's Kubernetes client. |
| `SIM_WORKLOAD_STALL_REPORT` | v1 | `5m` | | How long a running job must make no progress before `joulie.sim.job.stalled`, and the whole run before `joulie.sim.run.stalled`: a submitted job not done, no progress, no pod becoming Running or Succeeded, and every Pending pod `Unschedulable`, all for this long. Jobs not yet due never count. |
| `SIM_WORKLOAD_STALL_FORCE_COMPLETE` | legacy | | `120s` | How long a legacy job may make no progress before it is completed by force; `-1` disables it. |
| `SIM_RUNS_DIR` | v1 | `$SIM_HWEMU_ROOT/_runs` | | Directory of the run directories, `<SIM_RUNS_DIR>/<run id>/`: `trace.jsonl`, and `otlp/simulator.metrics.jsonl` and `otlp/simulator.logs.jsonl`. |
| `SIM_RUN_ID` | v1 | `run-<UTC yyyymmdd-hhmmss>` | | Id of the implicit run of `SIM_WORKLOAD_TRACE_PATH`; no slash, and not `.` or `..`. |
| `SIM_OTLP_FILE_INTERVAL` | v1 | `10s` | | Period of the cumulative metrics snapshot in `simulator.metrics.jsonl`; one more is written when the run ends. |

An invalid value stops the start with an error naming the variable, except `SIM_WORKLOAD_TICK` and `SIM_BASE_SPEED_PER_CORE`, which fall back to their defaults (see their rows). A variable only the other model reads logs a warning naming it; under v1 that includes `SIM_WORKLOAD_TICK`. The legacy defaults are the constants of the simulator before these became settings, so a legacy run behaves as before, except for two loader fixes: a trace line longer than 64 KiB loads (up to 16 MiB), and two jobs whose pod names collide in one namespace (pods are named `sim-<jobId>`, sanitized and cut to 50 characters) both run, the later one with the lowest free `-<n>` suffix, instead of the second create failing and the job being completed by force.

**Run API.** In v1 emulate mode the simulator serves one run at a time on its HTTP port; in every other mode these paths answer 412.

| Path | Method | Answer |
|------|--------|--------|
| `/sim/v1/runs` | POST | Starts a run. Body: `runId`, optional `trace` (a path inside the simulator, default `<SIM_RUNS_DIR>/<runId>/trace.jsonl`), optional `clusterName`, and `options` overriding the settings above for this run (`owner`, `intent`, `completion`, `namespace`, `jobTTL`, `nodeSelector`, `tolerations`, `maxCreatesPerTick`, `stallReport`, `baseSpeedPerCore`; an empty `nodeSelector` or `tolerations` clears the default, `null` keeps it). 201 with `runId`, `jobs`, `groups` and `startedAt`; 400 for a bad request or trace, naming the trace line; 409 while a run is active, or when the run id already has results. |
| `/sim/v1/runs/current` | GET | 200 with the active or last run's status: `phase` (`running`, `finished` or `aborted`), `elapsedSeconds`, `allSubmitted`, the jobs per state and `runStalled`; 404 before the first run. |
| `/sim/v1/runs/current/finish` | POST | Ends the run; jobs not done end incomplete. `?cleanup=true` also deletes the run's Job objects in the background. 200 with the final status once the run's files are closed. |

**Permissions.** The `joulie-simulator` ClusterRole of `charts/joulie-simulator` grants `get`, `list`, `watch`, `create` and `delete` on pods and nodes, which both engines use; `patch` on pods, for the v1 completion annotation; `get`, `list`, `watch`, `create` and `delete` on `batch` jobs, for the v1 Job objects and their cleanup; and `get`, `list` and `watch` on `joulie.io` nodetwins, which a v1 run observes for each node's schedulable class and never writes.

**KWOK Stage.** With `SIM_WORKLOAD_COMPLETION=kwok`, a pod ends only through a KWOK Stage named `pod-complete` that selects Running pods annotated `sim.joulie.io/complete: "true"`: `scripts/simcluster/manifests/stage-pod-complete.yaml`, adapted from KWOK v0.8.0. It replaces KWOK's own `pod-complete` Stage, which completes every Running pod a Job owns on a timer, before it did its work; the simulated cluster applies it after KWOK's Stages. On another KWOK cluster, apply it the same way, or set `SIM_WORKLOAD_COMPLETION=delete`. `simscenario run` refuses a cluster whose `pod-complete` Stage does not select the annotation.

**Scenario runner.** `scripts/simcluster/scenario.sh` (`make simcluster-scenario SCENARIO=<file>`) reads these settings besides those of the simulated cluster scripts:

| Variable | Default | Description |
|----------|---------|-------------|
| `SCENARIO` | (none) | The scenario file, for `make simcluster-scenario`. |
| `SCENARIO_VARIANTS` | (empty: all) | Run only these variants, space separated, and leave the run unsummarized for a later `simscenario summarize`. |
| `SCENARIO_RUN_DIR` | a new `$SIMCLUSTER_DIR/runs/<UTC>-<scenario>-<sha12>[-dirty]` | The run directory; `runs/latest` is left alone when it is set. |
| `SIMCLUSTER_VALIDATE_OTLP` | (unset) | `1`: post every line of the run's OTLP files to an OpenTelemetry Collector and require that it accepts and writes all of them. |
| `SIMCLUSTER_SIM_EXTRA_VALUES` | (empty) | Simulator values files, space separated, after the generated ones, for `deploy.sh` and `reload.sh`; `scenario.sh` sets it from a scenario's `simulator.set`. `deploy.sh` records them in `deploy.env`, so a later `reload.sh` installs with them without the variable. |
| `SIMCLUSTER_SCENARIOS` | (unset) | `1`: `smoke.sh` also runs the `smoke` scenario on its cluster after the end-to-end test. |

### Simulator telemetry export

In emulate mode the simulator (`simulator/cmd/simulator`) pushes OpenTelemetry metrics of every emulated node and of the facility as OTLP JSON over HTTP, for example straight to a Prometheus started with `--web.enable-otlp-receiver`. Events are always written to `events.jsonl` and pushed only when a logs endpoint is set. Names and Prometheus spellings: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

In every mode the simulator also answers reads of one node over HTTP: `GET /otlp/telemetry/{node}` and `GET /telemetry/{node}` serve the same OTLP JSON `ExportMetricsServiceRequest`, the format `TELEMETRY_CPU_HTTP_ENDPOINT` and `NODE_POWER_HTTP_ENDPOINT` read. `/telemetry/{node}` used to serve the legacy `packagePowerWatts` JSON: a client that still parses it adds `?format=legacy`, which is deprecated and removed in v0.4.0, and the simulator logs one warning per process when it is used. `?format=otlp` asks for the default explicitly; any other `format` value is refused with 400.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EXPORTERS` | (empty) | Comma-separated compatibility exporters to serve at `/exporters/<name>/metrics`: `kepler-0.7`, `kepler-0.12`, `dcgm-4.8.4`, `amd-dme-1.5.2` (ROCm device-metrics-exporter v1.5.2, AMD GPUs). An unknown name stops the start and lists the valid ones. |
| `SIM_DCGM_HOSTNAME_LABEL` | `hostname` | Host label of `dcgm-4.8.4`: `hostname`, or `Hostname` as dcgm-exporter releases up to 4.8.2 spell it. |
| `SIM_OTEL_FILE_DIR` | the value of `SIM_DEBUG_PERSIST_DIR` (default `/tmp/joulie-simulator-debug`) | Directory of `events.jsonl`, one OTLP JSON `LogsData` object per line. Empty disables the files. |
| `SIM_OTEL_FILE_METRICS` | `false` | Also append every metrics export to `metrics.jsonl` in the same directory, one `MetricsData` object per line. |
| `POD_NAME` | the host name | Identifies this simulator instance in its own telemetry. The chart sets it from the downward API. |
| `OTEL_SDK_DISABLED` | `false` | `true` turns off both pushes. |
| `OTEL_SERVICE_NAME` | `joulie-simulator` | `service.name` of every resource. |
| `OTEL_RESOURCE_ATTRIBUTES` | (empty) | Extra resource attributes, `key=value` pairs separated by commas. They never override `service.instance.id`, `k8s.node.name` or `host.name`. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | (empty) | URL of the metrics push, used as is. For Prometheus: `http://<prometheus>:9090/api/v1/otlp/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | (empty) | URL of the events push, used as is. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (empty) | Base URL for both signals: `v1/metrics` and `v1/logs` are appended. A per-signal variable wins. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_PROTOCOL` | as the specification | Headers (`key=value,...`), timeout in milliseconds, `gzip` or none, and the protocol, which must be `http/json`. Each has `_METRICS_` and `_LOGS_` variants that win for their signal. |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | Milliseconds between metrics pushes. A `rate()` window over the pushed counters should cover at least 4 intervals. |
| `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` | `none` turns off that signal's push. |

The export follows the OpenTelemetry exporter specification with three deviations:

- Only `http/json` is supported; the specification's default is `http/protobuf`, and any other protocol stops the start.
- Nothing is pushed without an endpoint; the specification's default is `http://localhost:4318`.
- A 404 from the logs endpoint turns the logs push off after one warning. Prometheus has no logs receiver, so a base endpoint pointing at Prometheus would otherwise fail on every push.

Points are cumulative: Prometheus rejects delta temporality unless its `otlp-deltatocumulative` feature is on. A push is retried on 429, 502, 503 and 504, honouring `Retry-After`, and never on 400 (such as out-of-order samples) or 500. Each request carries at most 64 node resources, plus the facility. A per-signal endpoint is used exactly as given, so it needs the full path; a base URL there is a common mistake.

Helm values of `charts/joulie-simulator`, rendered only when `emulate.enabled` is true:

| Value | Default | Effect |
|-------|---------|--------|
| `otel.metricsEndpoint` | `""` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. |
| `otel.logsEndpoint` | `""` | `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`. |
| `otel.exportIntervalMillis` | `10000` | `OTEL_METRIC_EXPORT_INTERVAL`, in milliseconds since Helm cannot parse durations. |
| `otel.resourceAttributes` | `{}` | `OTEL_RESOURCE_ATTRIBUTES`, joined as `key=value` pairs. |

For queries, Prometheus should promote `k8s.node.name` and `host.name` onto the data series (`otlp.promote_resource_attributes`); without that, the node name is joined from `target_info`. See [Simulator Metrics]({{< relref "/docs/simulator/metrics.md#prometheus" >}}).

### Simulator hardware profiles

The simulator (`simulator/cmd/simulator`) emulates a node from a hardware profile picked by name: one of the builtin profiles in `simulator/pkg/hwemu/profiles/`, or a local one. Local profiles stay outside the repository and are the only ones whose measured sources (`measured:` provenance) may name the machine they were measured on, such as a calibration of your own nodes. A builtin profile may cite a measurement too, but describes the machine by its hardware only.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_PROFILES_DIR` | (empty) | Directory of local hardware profiles: every `*.yaml` file directly in it, each with a `name` equal to its file name without `.yaml`. Empty means the builtin profiles only, and so does a directory with no `*.yaml` in it, such as a mounted ConfigMap that holds no profile yet. Hidden entries (a leading dot) are skipped. A directory that does not exist, a path that is not a directory and a `*.yml` file, or a `.yaml` suffix in another case such as `.YAML`, are errors, never a fallback to the builtins. |

A local profile may extend (`extends:`) a builtin profile or another local one, by the same merge rules the builtins use. It may not take a builtin's name, since one of the two would be hidden: give it a new name and extend the builtin instead. The simulator logs the local profiles once, as `local profiles loaded dir=<dir> profiles=<names>`.

## kubectl plugin

The `kubectl joulie` plugin requires no configuration. It reads your current kubeconfig context.

```bash
# Install
go build -o kubectl-joulie ./cmd/kubectl-joulie
mv kubectl-joulie /usr/local/bin/

# Usage
kubectl joulie status      # cluster energy overview
```
