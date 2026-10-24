---
title: "Configuration Reference"
weight: 50
---

Complete reference for all Joulie environment variables. These are set via Helm values or directly in the Deployment/DaemonSet manifests.

Defaults listed below are the **code defaults**. The Helm chart (`charts/joulie/values.yaml`) overrides some of them: notably, the controller manager `NODE_SELECTOR` defaults to `joulie.io/managed=true` in the chart even though the code default is `node-role.kubernetes.io/worker`.

**Booleans.** The agent, the controller manager and the simulator read every on/off variable the same way: `true`, `1`, `yes` or `on`, and `false`, `0`, `no` or `off`, in any case, with surrounding spaces ignored. Unset or empty means the default. Any other value keeps the default and logs one warning naming the variable, so a typo never flips a switch. The exceptions are `SIM_OTEL_FILE_METRICS`, `OTEL_SDK_DISABLED` and `fakesmi`'s `HWEMU_UNPRIVILEGED`, whose rows say how they are read.

## Images

The chart values `agent.image.tag`, `controllerManager.image.tag` and `schedulerExtender.image.tag` (and `image.tag` in the simulator chart) default to empty, which pulls the chart's `appVersion`. A chart installed from `oci://registry.cern.ch/joulie` carries the version it was released with, so it runs the images of that release. A chart installed from a checkout has `appVersion: latest` and runs the newest stable release. Set a tag only to run other images, for example your own build.

Pre-releases (a GitHub pre-release, or a version such as `0.2.0-rc0`) are published under their version only and never move `latest`.

Every image (`joulie-agent`, `joulie-controller-manager`, `joulie-scheduler`, `joulie-simulator`) is a multi-arch index of `linux/amd64` and `linux/arm64`, so each node pulls the image of its own architecture. The kubectl plugin is released for `linux` and `darwin`, each on `amd64` and `arm64`.

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

The agent always discovers the CPU and GPU governors of its node and publishes them in `NodeHardware` (see [Governor discovery]({{< relref "/docs/architecture/agent.md#governor-discovery" >}})). It writes the presets of its `NodeTwin`, `spec.cpu.governor` and `spec.gpu.governor`, only with `GOVERNOR_WRITES` `true`; the controller manager writes those fields only with its own `GOVERNOR_CONTROL` on (see [Governor control](#governor-control)). The baseline it puts back lives in `NodeHardware`: it follows every discovery until the agent's first governor write, so a change an operator makes before writes are on is what a restore puts back, and from that write it stays fixed until a restore. The agent puts it back when a field disappears.

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `GOVERNOR_WRITES` | `false` | `false` | Write the governor presets. `false` reports `disabled` for a preset and writes no preset. With `false` the agent still puts back the settings it changed in an earlier run (result `restored`), once, and writes nothing else. |
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
| `TELEMETRY_CPU_HTTP_ENDPOINT` | (empty) | HTTP endpoint for CPU telemetry, `{node}` replaced with the node name (e.g., `http://sim:18080/otlp/telemetry/{node}`). Each answer is one OTLP JSON `ExportMetricsServiceRequest`: the agent sums the `hw.type=cpu` components, from the rate of `hw.energy` or else the `hw.power` gauge, of the resource whose `k8s.node.name` is the node (a resource without it is read too). See [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}). The legacy `packagePowerWatts` JSON is still read, with one warning per endpoint and node, until v0.4.0: its power is the first positive of `packagePowerWatts` and `cpu.packagePowerWatts`, and there is no reading when neither is positive. An answer over 8 MiB is an error. |
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
| `agent.tolerations` | `- operator: Exists` (every taint) | Tolerations of the agent pods. The default tolerates every taint, so the agent runs on every node `agent.daemonset.nodeSelector` picks, GPU nodes with their vendor's taint and dedicated pools included: the node selector, not the taints, decides where it runs. A list replaces the default, and `[]` tolerates none. A managed node the agent does not reach is still planned, its caps are never enforced, and its `NodeTwin` says so (see [Managed nodes without an agent](#managed-nodes-without-an-agent)). |
| `agent.runtimeClassName` | `""` | RuntimeClass of the agent pods, for example the one of the NVIDIA container runtime, which puts the node's driver tools into the container. |
| `agent.extraEnv` | `[]` | EnvVar entries added after the chart's own. |
| `agent.extraVolumes` | `[]` | Volume entries added after the chart's own. |
| `agent.extraVolumeMounts` | `[]` | VolumeMount entries added to the agent container after the chart's own. |
| `agent.emulated.root` | `/var/lib/joulie-hwemu` | The simulator's `SIM_HWEMU_ROOT`. Each emulated agent mounts its node's tree from there: `sys` at `/host-sys`, `cpuinfo` over `/proc/cpuinfo` (read-only), `state` at `/emu/state`, `bin` at `/emu/bin` (read-only), `<root>/lib` at `/emu/lib` (read-only), and `dev/nvidiactl` at `/dev/nvidiactl` for an entry with `devNvidiactl`. |
| `agent.emulated.hostNodeSelector` | `joulie.io/hwemu-host: "true"` | Node selector of the emulated agents: the node that holds the trees and runs the simulator. |
| `agent.emulated.path` | `/emu/bin:/usr/local/bin:/usr/bin:/bin` | `PATH` of the emulated agents, so the emulated GPU tools come first. |
| `agent.emulated.fuse` | `false` | Mount `sys` and `state` from the simulator's FUSE views, `<root>/fuse/<name>/sys` and `<root>/fuse/<name>/state`, with mount propagation `HostToContainer`, for a simulator with `emulate.fuse`; the other mounts stay on the tree. The DaemonSet ignores it. |
| `agent.emulated.nodes` | `[]` | One entry per emulated node, as `simcluster agents` writes them: `name` (the Node, also the literal `NODE_NAME`), `profile`, `devNvidiactl`, `extraVolumes`, `extraVolumeMounts`. Entry `i` becomes the Deployment `joulie-agent-emu-<i>`, with strategy `Recreate`, no host network and no privileges. A name that is not a Node name, or is listed twice, fails the render. |

Emulated agents are what the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs: one per node the DaemonSet would reach, judged from the DaemonSet's own render.

### Agent GPU tools

The agent reads and caps NVIDIA GPUs by running `nvidia-smi` from its `PATH`. AMD GPUs need no tool: the agent reads and writes the `power1_cap` files of their amdgpu hwmon devices under `/host-sys` (see [Agent host backends]({{< relref "/docs/hardware/gpus.md#agent-host-backends" >}})), which needs a root agent and a writable `/host-sys`, as the chart gives by default. It runs `rocm-smi --showproductname` only as a vendor probe, and never runs `amd-smi`. Intel GPUs need no tool either: the agent reads and writes the `power1_max` or `power2_max` files of their i915 or xe hwmon devices under `/host-sys`, with the same root agent and writable `/host-sys`, and reports the GPU control backend `host-intel`. The agent image (`gcr.io/distroless/static`) ships no GPU tool, and has no C library and no ELF loader, so mounting the host's `nvidia-smi` alone is not enough: it is dynamically linked (`file $(command -v nvidia-smi)` on a GPU node shows it) and also needs the loader and the NVML library (`libnvidia-ml.so.1`). The hooks above are where these come in: `agent.runtimeClassName` for a container runtime that injects the driver's tools and libraries, such as the NVIDIA one, or `agent.extraVolumes` and `agent.extraVolumeMounts` for a host directory, with `agent.extraEnv`.

The NVIDIA inventory is `nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name,power.default_limit --format=csv,noheader,nounits`; a wrapper that answers for `nvidia-smi` must answer that query. `NodeHardware` `gpu.capRangePerGpu.defaultWatts` is its `power.default_limit`, and `maxWatts` when it prints none within the range; on AMD it is always `maxWatts`, because the driver rewrites `power1_cap_default` with the cap in force. `gpu.powerDomains` is the number of GPUs that hold a power limit of their own, published only when it is below `gpu.count`, as on two-die AMD packages such as the MI250X; the controller manager and the extender multiply the per-GPU maximum by it, and by the count when it is absent.

Which combination works on a real NVIDIA node is an open question: no recipe has been verified yet. After installing, check the GPU control status of every GPU node:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,GPU:.status.controlStatus.gpu.result,MESSAGE:.status.controlStatus.gpu.message
```

A tool that cannot run shows as `error` with a message starting `gpu inventory failed`, or as `blocked` with `no supported GPU backend detected` when the vendor cannot be told either; the GPUs then keep their limits. On an AMD node without the amdgpu driver the message is `gpu inventory failed: no amdgpu hwmon device under bus/pci/devices`, and a cap the driver refuses, or a read-only `/host-sys`, is `error` naming the GPU's bus id. On an Intel node labelled as one without an i915 or xe hwmon device it is `gpu inventory failed: no i915 or xe hwmon device with a power limit under bus/pci/devices`; Intel GPUs whose driver shows no `power<N>_rated_max` end `blocked` with `the power limit range of the Intel GPUs is unknown`, and a limit the hardware clamps is `error` with the value it reads back. The simulated cluster reproduces both sides: `fakesmi` fails as the real tool does when something it needs is missing (`HWEMU_ROOTFS` under [Simulator hardware emulation](#simulator-hardware-emulation)), and the harness either mounts those files (`toolDelivery: mounted`) or not (`toolDelivery: none`).

### Managed nodes without an agent

The controller manager plans every managed node, whether or not an agent runs there, and says in the node's `NodeTwin` whether one does: the condition `AgentReporting` in `status.conditions`, shown in the `Agent` column of `kubectl get nodetwins`. It is `True` (reason `NodeHardwareCurrent`) while an agent publishes the node's `NodeHardware`, and `False` when no agent has published it (`NoNodeHardware`) or its last write is more than 15 minutes old (`NodeHardwareStale`): the caps in `spec` are then planned and not enforced. The controller manager logs a warning each time a node turns `False`, and counts the managed nodes per status and reason in the gauge `joulie_twin_nodes_agent_reporting` ([Twin agent coverage]({{< relref "/docs/architecture/metrics.md#twin-agent-coverage" >}})). The usual causes are a taint the agent does not tolerate (`agent.tolerations`), a node outside `agent.daemonset.nodeSelector`, or an image without the node's platform.

```bash
kubectl get nodetwins -o custom-columns='NODE:.spec.nodeName,AGENT:.status.conditions[?(@.type=="AgentReporting")].status,REASON:.status.conditions[?(@.type=="AgentReporting")].reason'
```

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
| `NODE_POWER_PRESET` | (empty: `otel`, or `custom` when `NODE_POWER_PROMETHEUS_QUERY` is set) | unset | The format the Prometheus query reads: `otel`, `kepler` (Kepler 0.10 and later: the active and idle energy counters of the CPU package and die zones, which never wrap, plus the GPU energy counter), `kepler-legacy` (Kepler 0.7 to 0.9: package and GPU energy) or `custom`. Every preset query is under [The queries]({{< relref "/docs/architecture/telemetry.md#the-queries" >}}). A preset other than `custom` together with `NODE_POWER_PROMETHEUS_QUERY` stops the controller manager at start, so a query is never silently ignored. |
| `NODE_POWER_SCOPE` | `components` | `components` | `components`: the CPU packages plus the GPUs, the boundary of the twin's budget (CPU TDP plus GPU TDP). `host`: the whole host (`hw.host.energy` and `hw.host.power`, or Kepler 0.7 platform energy), which exceeds the budget under load. `kepler` with `host` stops the controller manager at start. |
| `NODE_POWER_NODE_LABEL` | per preset: the translated `k8s.node.name` (`k8s_node_name` by default) for `otel` and `custom`, `node_name` for `kepler`, `exported_instance` for `kepler-legacy` | unset | The label that names the node in the query result. |
| `NODE_POWER_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS` | unset | Prometheus of the node power query only. |
| `NODE_POWER_PROMETHEUS_QUERY` | (empty) | unset | Your own PromQL, in **watts**, for the `custom` preset. With `{node}` it runs once per node with the node name filled in, and every series it returns is summed (a warning names a node whose query returns more than one). Without `{node}` it is summed by `NODE_POWER_NODE_LABEL`. |
| `NODE_POWER_HTTP_ENDPOINT` | (empty) | unset | With `NODE_POWER_SOURCE=http`: a URL with `{node}`, answering one OTLP JSON `ExportMetricsServiceRequest` per node, such as the simulator's `/otlp/telemetry/{node}`. It is read with the rules of the `otel` preset under `NODE_POWER_SCOPE`; see [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}). The legacy `packagePowerWatts` JSON is still read with a warning until v0.4.0: its power is the first positive of `packagePowerWatts` and `cpu.packagePowerWatts`, whatever `NODE_POWER_SCOPE` says, and 0 W when neither is positive. An answer over 8 MiB is an error (`source: http-error`). |

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

Upgrading from v0.2: see [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}).

### Power cap configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERFORMANCE_CAP_WATTS` | `5000` | Absolute CPU power cap for performance nodes (watts). The CPU of a GPU node gets it whatever the node's profile. |
| `ECO_CAP_WATTS` | `120` | Absolute CPU power cap for eco CPU-only nodes (watts). A GPU node never gets it: capping the CPU of a GPU node slows the GPU data feed by more than it saves. |
| `CPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | CPU cap as percentage of max for performance nodes |
| `CPU_ECO_CAP_PCT_OF_MAX` | `60` | CPU cap as percentage of max for eco nodes |
| `CPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute watts instead of percentage |
| `GPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | GPU cap as percentage of max for performance nodes |
| `GPU_ECO_CAP_PCT_OF_MAX` | `60` | GPU cap as percentage of max for eco nodes |
| `GPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute GPU watts instead of percentage |
| `GPU_MODEL_CAPS_JSON` | `{}` | JSON map of GPU model name to `{"minCapWatts": N, "maxCapWatts": M}` |
| `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES` | `false` | If `true`, a GPU node whose `NodeHardware` reports GPUs without control (`status.gpu.controlAvailable` false: Intel GPUs without a default limit in sysfs, NVIDIA boards whose power limits read `[N/A]`) gets the CPU caps of its profile, as a CPU-only node does, instead of running uncapped in eco. Other GPU nodes get no eco CPU cap whatever this says. |
| `GPU_PRODUCT_LABEL_KEYS` | `joulie.io/gpu.product,nvidia.com/gpu.product,amd.com/gpu.product-name,beta.amd.com/gpu.product-name,amd.com/gpu.product,gpu.intel.com/product,amd.com/gpu.family` | Comma-separated node label keys to read the GPU product name from, first match wins. The AMD node labeller names the board in `amd.com/gpu.product-name` (and `beta.amd.com/gpu.product-name`); `amd.com/gpu.product` is the older key. Intel's NFD rules name a data center GPU in `gpu.intel.com/product` (for example `Flex_170`). The AMD family code in `amd.com/gpu.family` (for example `GC_11_0_0`) names no board, so it comes last. When its GPU inventory fails, or names no model, the agent reads these default labels, in this order; it does not read this variable. |

The GPU caps apply to every GPU node, per physical GPU: a node whose agent lists GPUs, whose labels state a GPU count (`joulie.io/hw.gpu-count`, `nvidia.com/gpu.count` except when the single MIG strategy exposes MIG devices, product suffixed `-MIG-`, or Intel's `gpu.intel.com/device.count`), or that advertises whole GPUs, MIG devices, AMD partitions or shared replicas ([Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})). A product read through `GPU_PRODUCT_LABEL_KEYS` loses the suffixes GPU feature discovery appends (`-MIG-<profile>`, `-MIG-INVALID`, `-SHARED`), so `GPU_MODEL_CAPS_JSON` and `GPU_GOVERNOR_MODEL_PRESETS_JSON` are keyed by the product without them.

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

Off by default. With `GOVERNOR_CONTROL` on, the controller manager writes a vendor-neutral governor preset into each managed node's `NodeTwin` (`spec.cpu.governor`, `spec.gpu.governor`), chosen by the profile the node runs at; the agent maps it to the node's native settings. Nothing changes on a node until its agent also runs with `GOVERNOR_WRITES` `true`. Which node gets which preset: [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `GOVERNOR_CONTROL` | `off` | `off`, `cpu` (CPU governors only), `gpu` (GPU governors only) or `all`. |
| `CPU_GOVERNOR_PERFORMANCE_PRESET` | `performance` | CPU preset of performance nodes: `performance` (highest frequency at any load), `balanced` (demand-driven, quick ramp) or `efficiency` (demand-driven with an energy bias). |
| `CPU_GOVERNOR_ECO_PRESET` | `efficiency` | CPU preset of eco nodes. Provisional: in the simulator's [governor exploration]({{< relref "/docs/experiments/governor-exploration.md#acceptance-tier-a-half" >}}) it saved 0.5 % energy for a 22.2 % eco p95 slowdown, above the experiment's 20 % limit. |
| `CPU_GOVERNOR_ECO_BOOST` | empty | `on` or `off`: frequency boost (turbo) with the eco preset, on nodes whose agent found a boost control (`status.cpu.governor.boost`). Empty leaves boost alone. |
| `CPU_GOVERNOR_ON_GPU_NODES` | `false` | Write the CPU governor on GPU nodes too. |
| `GPU_GOVERNOR_PERFORMANCE_PRESET` | empty | GPU preset of performance nodes: `auto`, `sustained`, `efficient`, `low`, `peak`, or empty for none. |
| `GPU_GOVERNOR_ECO_PRESET` | `efficient` | GPU preset of eco nodes; set it to an empty value to write none. |
| `GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX` | `80` | Compute clock ceiling of the `efficient` preset, in percent of the GPU's maximum compute clock, in (0, 100]. Written only with `efficient`; a value out of range gives 80. The default is provisional: in the simulator's [governor exploration]({{< relref "/docs/experiments/governor-exploration.md#acceptance-tier-a-half" >}}) 75 slowed eco work past the eco class's 20 % p95 limit and 80 did not. |
| `GPU_GOVERNOR_MODEL_PRESETS_JSON` | `{}` | Presets per GPU model: `{"<model>": {"performance": "<preset>", "eco": "<preset>", "ecoCeilingPctOfMax": <pct>}}`. Keyed like `GPU_MODEL_CAPS_JSON`, by the GPU product label value, and also matched against the GPU model the agent reports. A field left out keeps the default above, `""` writes none for that model, and an invalid field is dropped with a warning. |
| `GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU` | `false` | If `true`, a node whose GPU preset is `sustained` gets it only while a running pod on it requests 2 or more GPUs (units of any GPU resource: whole GPUs, MIG devices, partitions, shared replicas), and no GPU governor otherwise. |

GPU presets: `auto` is the vendor default. `sustained` holds the data-path clocks (memory, fabric) at their maximum and keeps idle gating, leaving the compute clock free to idle; a demand-driven governor sees only a GPU's own kernels, so a GPU that receives a peer transfer can stay at low data-path clocks, and on families where those clocks bound the transfer, `sustained` removes that limit at some idle power. No default selects it: opt a model in through `GPU_GOVERNOR_MODEL_PRESETS_JSON` once a measurement on that model shows the gain. `efficient` puts the compute clock under the ceiling and leaves the data path dynamic. `low` and `peak` are for experiments; `peak` turns idle gating off on AMD GPUs. These defaults are provisional until measured. [Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) says what each preset means, and [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}) which node gets which.

The overview dashboard's "Agent: Governors" row plots the presets in effect (`joulie_agent_governor_state`) and the writes per result (`joulie_agent_governor_writes_total`).

### Facility metrics

The facility poller reads seven inputs from Prometheus, each with one grouped query per poll. The outdoor ambient temperature, IT power and cooling power are one value each; the twin uses the IT power for PSU stress and the ambient temperature for cooling stress, and the controller manager logs PUE as `(IT power + cooling power) / IT power`. The zone, rack and node inputs return one series per cooling zone, rack or node (see "Node topology" below). The grid carbon intensity is one value, read only when `FACILITY_CARBON_INTENSITY_QUERY` is set (see "Grid carbon intensity" below). An input whose query fails or returns nothing is unknown until a later poll reads it, never the value of an earlier poll. The policy and the twins of a reconcile read the same poll. Every query is counted in `joulie_twin_input_queries_total{input,result}`, the three single values are exported as `joulie_twin_facility_*`, and the carbon intensity as `joulie_grid_carbon_intensity_grams_per_kW_h` ([Metrics]({{< relref "/docs/architecture/metrics.md#twin-input-metrics" >}})).

| Variable | Default | Chart | Description |
|----------|---------|-------|-------------|
| `ENABLE_FACILITY_METRICS` | `false` | `false` | Poll the facility inputs. The first poll runs at start. |
| `FACILITY_PROMETHEUS_ADDRESS` | `PROMETHEUS_ADDRESS`, or `http://prometheus-operated.monitoring:9090` when that is unset | unset | Prometheus of the facility queries only. |
| `FACILITY_POLL_INTERVAL` | `30s` | `30s` | How often the inputs are polled. 0 or less stops the controller manager at start. |
| `FACILITY_PRESET` | `otel` | `otel` | `otel`: the facility, cooling zone, rack and host metrics of the [input contract]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}), every input but the carbon intensity. `legacy`: the `datacenter_ambient_temperature_celsius`, `datacenter_total_it_power_watts` and `datacenter_cooling_power_watts` names, the three single inputs only; deprecated, removed in v0.4.0. `custom`: only the inputs whose query is set below. |
| `FACILITY_AMBIENT_TEMP_QUERY` | from the preset | unset | Outdoor ambient temperature, one value in degrees Celsius. |
| `FACILITY_IT_POWER_QUERY` | from the preset | unset | Power of the IT equipment, one value in watts. |
| `FACILITY_COOLING_POWER_QUERY` | from the preset | unset | Power of the cooling, one value in watts. |
| `FACILITY_ZONE_AMBIENT_TEMP_QUERY` | from the preset | unset | Ambient temperature of each cooling zone, keyed by the translated `joulie.cooling_zone.name` label (`joulie_cooling_zone_name` by default), whose values are the nodes' `joulie.io/cooling-zone` label values. |
| `FACILITY_RACK_POWER_QUERY` | from the preset | unset | Power of each rack in watts, keyed by the translated `joulie.rack.name` label (`joulie_rack_name`), whose values are the nodes' `joulie.io/rack` label values. |
| `FACILITY_NODE_INLET_TEMP_QUERY` | from the preset | unset | Inlet temperature of each node, keyed by the translated `k8s.node.name` label (`k8s_node_name`). |
| `FACILITY_CARBON_INTENSITY_QUERY` | unset (no preset reads it) | unset | Carbon intensity of the grid that supplies the cluster, one value. Unset reads none. |
| `FACILITY_CARBON_INTENSITY_UNIT` | `gCO2eq/kWh` | unset | Unit of the carbon intensity query's value: a mass `g`, `kg`, `t`, `lb` or `lbs`, optionally followed by `CO2`, `CO2e` or `CO2eq`, over `kWh` or `MWh`, in any case, such as `lbsCO2/MWh` for WattTime's `co2_moer`. Converted to gCO2eq/kWh. Any other unit stops the controller manager at start while the query is set. |
| `FACILITY_CARBON_INTENSITY_SIGNAL` | none | unset | What the value measures: `marginal` (the generators that follow a change in demand, such as WattTime's MOER) or `average` (the whole generation mix, such as Electricity Maps). Required with the query: the controller manager does not start without it. |

A query variable replaces its input's query under any preset. A bare metric name is aggregated by the input's key label, `avg` for a temperature and the carbon intensity and `sum` for a power, so `FACILITY_IT_POWER_QUERY=ups_output_power_watts` runs `sum(ups_output_power_watts)`. Any other query is kept as written and must return one series, or for the zone, rack and node inputs one series per key carrying the key label.

### Grid carbon intensity

Joulie ships no carbon intensity provider. An operator who already exports it to Prometheus (an exporter of WattTime or Electricity Maps, or one of their own) points `FACILITY_CARBON_INTENSITY_QUERY` at it, with `ENABLE_FACILITY_METRICS=true`, and says what the value is:

```yaml
controllerManager:
  env:
    ENABLE_FACILITY_METRICS: "true"
    FACILITY_CARBON_INTENSITY_QUERY: 'watttime_co2_moer{region="CAISO_NORTH"}'
    FACILITY_CARBON_INTENSITY_UNIT: "lbsCO2/MWh"
    FACILITY_CARBON_INTENSITY_SIGNAL: "marginal"
```

The query must return one series: select the grid zone of the cluster with a label matcher, since a bare metric name that holds several zones is averaged across them. The reading is converted to gCO2eq/kWh (1000 lbs CO2/MWh is 453.59 gCO2eq/kWh; kgCO2eq/MWh equals gCO2eq/kWh) and keeps its signal, so a marginal value is never compared with an average one; the mass basis, CO2 or CO2 equivalent, stays the provider's. A negative or non-finite value, which no grid has, is an error: that poll's reading is unknown and counted under `result="error"`. It is a cluster reading, exported as `joulie_grid_carbon_intensity_grams_per_kW_h{joulie_grid_carbon_intensity_signal}` and handed to the policy with the rest of the facility snapshot; it is not a `NodeTwin` field, and no policy acts on it yet.

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
| `CACHE_TTL` | `30s` | Minimum interval between rebuilding the scoring maps from the informer cache, which watches NodeTwin and NodeHardware, so a score never reads the API server. |
| `TWIN_STALENESS_THRESHOLD` | `5m` | Duration after which NodeTwin data is considered stale |

The Service `joulie-scheduler-extender` carries `schedulerExtender.service.annotations`, by default `prometheus.io/scrape: "true"`, `prometheus.io/port: "9877"` and `prometheus.io/path: /metrics`, as the agent's and the controller manager's metrics Services carry theirs, so a Prometheus that discovers targets by annotation scrapes the `joulie_scheduler_*` metrics. With the Prometheus Operator, a ServiceMonitor selects the Service's `metrics` port instead; [tutorial 03](https://github.com/joulie-k8s/Joulie/tree/main/examples/03-grafana-dashboard) has one for each component.

With `schedulerExtender.hostNetwork: true` the chart deploys the extender with strategy `Recreate`: an old and a new pod cannot hold the host port on one node, so a rolling update would never finish. Pods are still scheduled while the extender restarts only if the scheduler configuration marks it `ignorable: true`; otherwise also set `schedulerExtender.nodeName`, so the new extender pod does not wait for the scheduler that waits for it.

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

The simulator (`simulator/cmd/simulator`) reads these variables at startup to build its base hardware profile. A node class in `SIM_NODE_CLASS_CONFIG` overrides one per node with the key in parentheses, under the class's `model:`.

Temperature follows a first-order lag toward ambient plus power over the watts per degree, and is reported in the telemetry. Thermal throttling is off by default: both thresholds of a device are 0. Setting one variable of a pair turns it on, with the other at the value its row gives. In a node class set both thresholds, the full one above the start: a class with only a start threshold makes the simulator ignore every class, with one warning in the log, and a class with only a full threshold throttles from 0 °C. See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}), sections 5.5 and 6.6.

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

The simulator (`simulator/cmd/simulator`) reads these variables at startup too. For a node whose labels match the hardware catalogue, a variable that is unset leaves the field to the catalogue; the built-in default applies only to hardware the catalogue does not know. A node class key (in parentheses) wins over both, and a value of 0 or less counts as unset. See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}), section 3.1.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_P_MAX_W` | sockets x catalogue TDP, else `420` | Peak CPU power of the generic model, at full load and full frequency (`pMaxW`). A node with a measured curve uses the curve instead. |
| `SIM_CPU_SOCKET_CAP_MAX_W` | catalogue TDP, else `400` | Upper end of the per-socket CPU cap: the node cap is split evenly over the sockets and clamped to this range (`cpuSocketCapMaxW`). |
| `SIM_CPU_SOCKET_CAP_MIN_W` | catalogue cTDP low end, else 0.55 x TDP, else `120` | Lower end of the per-socket CPU cap (`cpuSocketCapMinW`). |
| `SIM_GPU_MAX_WATTS_PER_GPU` | catalogue `maxBoardPowerW`, else `300` | Board power of one GPU at full load, and its highest cap (`gpu.maxWattsPerGpu`). |
| `SIM_GPU_MIN_CAP_WATTS_PER_GPU` | catalogue `minBoardPowerW`, else half the maximum, else `80` | Lowest cap of one GPU (`gpu.minCapWattsPerGpu`). |

A catalogue minimum above a maximum set by a class or a variable is lowered to that maximum. A measured two-socket curve from the catalogue is scaled by sockets/2 on a node with another socket count.

Upgrading from v0.2: see [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}).

### Simulator physics step

The simulator (`simulator/cmd/simulator`) advances the model of a node outside the emulate-mode cluster spec only in a physics step, in simulated time; a read or a control never advances it. A step splits its time into equal substeps, and the energy it books is the integral of the instantaneous power over them. See [Power Simulator]({{< relref "/docs/simulator/power-simulator.md#the-physics-step" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_POLL_INTERVAL` | `15s` (chart: `8s`) | Kubernetes mode: how often the simulator lists pods and nodes. A value of 0 or less logs a warning and takes `15s`. |
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

A listed tool first checks the requirements of its `tools.json` entry, in order, under `HWEMU_ROOTFS`: each requirement is a list of paths, and one existing path meets it. The first requirement with none of its paths present ends the call, before any GPU state is read, with the exit code and message the real tool gives without that file. The requirements are profile data (`gpus.tools[].requires`); a tool without requirements always runs. Every builtin GPU profile lists, the NVIDIA ones for `nvidia-smi` and the AMD ones for `rocm-smi` and `amd-smi`:

| Tool | Requirement (any one path) | Exit | Message |
|------|----------------------------|------|---------|
| `nvidia-smi` | `/lib64/ld-linux-x86-64.so.2`, the ELF loader | `127` | none: exec fails, which a shell reports as 127 |
| `nvidia-smi` | in this order, `libpthread.so.0`, `libm.so.6`, `libdl.so.2`, `librt.so.1` and `libc.so.6`, each under `/lib/x86_64-linux-gnu`, `/usr/lib/x86_64-linux-gnu`, `/lib64` or `/usr/lib64` | `127` | the loader's `nvidia-smi: error while loading shared libraries: <library>: cannot open shared object file: No such file or directory` for the first one missing |
| `nvidia-smi` | `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1` or `/usr/lib64/libnvidia-ml.so.1` | `12` | `NVIDIA-SMI couldn't find libnvidia-ml.so library in your system. ...`; 12 is nvidia-smi's "NVML Shared Library couldn't be found or loaded" |
| `rocm-smi`, `amd-smi` | `/usr/bin/env`, from the scripts' `#!/usr/bin/env python3` | `127` | none |
| `rocm-smi`, `amd-smi` | `/usr/bin/python3` or `/usr/local/bin/python3` | `127` | `/usr/bin/env: 'python3': No such file or directory` |

In the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md#deviations-from-a-real-cluster" >}}), `toolDelivery: mounted` mounts one stub file at the first path of each requirement: seven on an NVIDIA node (the loader, the five libraries and NVML), two on an AMD node.

An empty `HWEMU_ROOTFS` is the default, `/`. Only a caller of package `simulator/pkg/hwemu/smi` that builds its own `Env` with an empty `RootFS` skips the checks, as the in-process agent tests do.

### Simulator emulate mode

In emulate mode the simulator (`simulator/cmd/simulator`) emulates the bare-metal nodes a cluster spec lists: for each one it keeps a sysfs tree, `/proc/cpuinfo` and GPU tool state under `SIM_HWEMU_ROOT/<node>/`, and advances them on a fixed tick, so an unmodified agent pod that mounts them discovers and caps the node as it would a real one. Nodes the spec does not list keep the label-based model. `/control/<node>` answers 409 for an emulated node, whose limits are written to its files instead. See [Emulate Mode]({{< relref "/docs/simulator/emulate-mode.md" >}}).

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EMULATE` | `false` | Turn emulate mode on. The simulator refuses to start when `SIM_STANDALONE` is also set, or when `SIM_CLUSTER_SPEC` is empty. Workloads run on the v1 engine; `SIM_WORKLOAD_MODEL=legacy` is refused, since a legacy trace cannot drive emulated nodes (see [Simulator workloads and scenarios](#simulator-workloads-and-scenarios)). |
| `SIM_CLUSTER_SPEC` | (empty) | Path of the cluster spec, a `sim.joulie.io/v1alpha1` `ClusterSpec` such as `tests/simcluster/specs/smoke.yaml`. It names each node, its hardware profile, pool, rack and cooling zone, and the facility. |
| `SIM_HWEMU_ROOT` | `/var/lib/joulie-hwemu` | Directory of the emulated trees: one `<node>/` per spec node, `lib/fakesmi` and `facility-ledger.json`, and `fuse/` with `SIM_HWEMU_FUSE`. One simulator holds it at a time; a second one fails to start. |
| `SIM_HWEMU_FUSE` | `false` | Serve each spec node's `sys` and `state` through FUSE at `SIM_HWEMU_ROOT/fuse/<node>/`, where the agents mount them: a write the kernel would refuse fails at once with its errno, a write it takes is in effect at once, and energy counters move at read time ([FUSE mode]({{< relref "/docs/simulator/emulate-mode.md#fuse-mode" >}})). Needs `/dev/fuse`, and as root `CAP_SYS_ADMIN`; another user needs `fusermount3`. |
| `SIM_EMULATE_TICK` | `1s` | Physics tick: every spec node advances to wall time once per tick. A tick that runs late is counted and the next one catches up. The nodes outside the spec are stepped on the same tick (see "Simulator physics step"). |
| `SIM_GPU_RECEIVER_GATE` | `profile` | Whether the receiver gate of [peer transfers]({{< relref "/docs/hardware/hardware-modeling.md#peer-transfers" >}}) applies: `profile` follows each hardware profile's `gpus.transfer.receiverGate.enabled`, off in every builtin profile; `off` disables it everywhere; `on` enables it on every GPU profile with the profile's fractions, for what-if runs (a profile that leaves a fraction unset lets the whole link through). Any other value logs a warning naming the variable and takes `profile`. A gated copy slows the v1 job that makes it (see [peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})). |
| `SIM_EMULATE_WORKERS` | `0` | Nodes stepped in parallel; `0` means `GOMAXPROCS`, at most 12. A step is mostly system calls on the node's files, which contend past about 12 workers (see [Tick cost]({{< relref "/docs/simulator/emulate-mode.md#tick-cost" >}})); a set value is used as given. |
| `SIM_FAKESMI_SOURCE` | `/usr/local/lib/joulie/fakesmi` | The `fakesmi` binary to install as `SIM_HWEMU_ROOT/lib/fakesmi`, where every node's `bin/nvidia-smi`, `bin/rocm-smi` and `bin/amd-smi` link points. It is replaced by rename, and only when its sha256 changed. The simulator image ships it at the default path. Empty skips the copy, for a run outside the image. |
| `KUBECONFIG` | client-go loading rules | The cluster the simulator watches, so it can also run on a workstation. Without a kubeconfig it uses the in-cluster configuration. The simulator logs the API server it uses. |

Hardware profiles are the builtin ones in `simulator/pkg/hwemu/profiles/`, plus local ones from `SIM_PROFILES_DIR`. The facility values come from the spec's `facility` block and fall back to `SIM_FACILITY_AMBIENT_TEMP_C`, `SIM_FACILITY_TEMP_AMPLITUDE_C` and `SIM_FACILITY_TEMP_PERIOD_H`.

A restarted simulator resumes the trees; a tree that no longer matches its spec node stops the start. Both, and the recovery, are under [Restarts and recovery]({{< relref "/docs/simulator/emulate-mode.md#restarts-and-recovery" >}}).

Helm values of `charts/joulie-simulator`:

| Value | Default | Effect |
|-------|---------|--------|
| `emulate.enabled` | `false` | Sets `SIM_EMULATE`. Also runs the pod as root with strategy `Recreate`, adds a readiness probe on `/readyz` (ready once every tree exists), and leaves out `SIM_WORKLOAD_TRACE_PATH`: a run starts through the run API, or from a trace set in `extraEnv`. The chart fails unless `replicas` is 1. |
| `emulate.root` | `/var/lib/joulie-hwemu` | `SIM_HWEMU_ROOT`, and a `DirectoryOrCreate` hostPath mounted at the same path. |
| `emulate.tick` | `1s` | `SIM_EMULATE_TICK`. |
| `emulate.fuse` | `false` | Sets `SIM_HWEMU_FUSE=true`, and runs the pod privileged with `CAP_SYS_ADMIN`, the hostPath `/dev/fuse` (type `CharDevice`) and mount propagation `Bidirectional` on `emulate.root`, which must be on a shared mount of the host. The emulated agents need `agent.emulated.fuse` of the Joulie chart. |
| `emulate.receiverGate` | `profile` | `SIM_GPU_RECEIVER_GATE`. An unquoted `on` or `off`, which YAML reads as a boolean, is passed as the word. |
| `emulate.clusterSpec` | `""` | The spec as text, usually `--set-file emulate.clusterSpec=<spec.yaml>`. The chart stores it in the ConfigMap `joulie-simulator-cluster-spec`, mounts it at `/etc/joulie-sim-spec` and points `SIM_CLUSTER_SPEC` at it. A changed spec restarts the pod. |
| `emulate.clusterSpecConfigMap` | `""` | Instead, an existing ConfigMap holding the spec under the key `cluster-spec.yaml`. Set exactly one of the two. |
| `emulate.exporters` | `[]` | `SIM_EXPORTERS`, and one ServiceMonitor endpoint per exporter, with `honorLabels: false`. |
| `emulate.nodeSelector` | `joulie.io/hwemu-host: "true"` | Merged into `nodeSelector`: the simulator must run on the host whose root the emulated agent pods mount. |
| `rbac.workloadRuns` | `auto` | Whether the ClusterRole carries the rules of a v1 workload run (**Permissions** under [Simulator workloads and scenarios](#simulator-workloads-and-scenarios)): `auto` follows `emulate.enabled`; `true` and `false` force them. Any other value fails the render. |
| `extraEnv` | `{}` | Variables merged over `env` and over the ones the chart sets, so they win. In emulate mode, set `SIM_WORKLOAD_TRACE_PATH` here to start an implicit run. |

The chart always sets `POD_NAME` from the downward API and mounts its node class, hardware catalog and workload trace ConfigMaps as optional volumes, so a bare install starts without them.

### Simulator workloads and scenarios

The simulator (`simulator/cmd/simulator`) drives a workload trace through the cluster. `SIM_WORKLOAD_MODEL` picks the engine: `legacy` outside emulate mode, `v1` in emulate mode; the two are compared in [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md#two-engines-legacy-and-v1" >}}). The "Read by" column says which engine reads a variable.

| Variable | Read by | v1 default | Legacy default | Description |
|----------|---------|------------|----------------|-------------|
| `SIM_WORKLOAD_MODEL` | both | `v1` with `SIM_EMULATE=true` | `legacy` otherwise | `legacy` or `v1`. `v1` without emulate mode and `legacy` with it stop the start. |
| `SIM_WORKLOAD_TRACE_PATH` | both | (empty) | (empty) | A trace file, or a directory of them, loaded at startup. Legacy replays it from startup. v1 starts it as an implicit run with id `SIM_RUN_ID`; a run that fails to start is logged and the run API stays open. The chart sets `/etc/joulie-sim-trace/trace.jsonl` outside emulate mode; in emulate mode set it through the chart's `extraEnv`. |
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

An invalid value stops the start with an error naming the variable, except `SIM_WORKLOAD_TICK` and `SIM_BASE_SPEED_PER_CORE`, which fall back to their defaults (see their rows). A variable only the other model reads logs a warning naming it; under v1 that includes `SIM_WORKLOAD_TICK`. A trace line may be up to 16 MiB. The legacy engine names pods `sim-<jobId>`, sanitized and at most 50 characters; a name already taken gets the lowest free `-<n>` suffix.

**Run API.** In v1 emulate mode the simulator serves one run at a time on its HTTP port; in every other mode these paths answer 412.

| Path | Method | Answer |
|------|--------|--------|
| `/sim/v1/runs` | POST | Starts a run. Body: `runId`, optional `trace` (a path inside `SIM_RUNS_DIR`, default `<SIM_RUNS_DIR>/<runId>/trace.jsonl`; any other path is refused, since the API has no authentication), optional `clusterName`, and `options` overriding the settings above for this run (`owner`, `intent`, `completion`, `namespace`, `jobTTL`, `nodeSelector`, `tolerations`, `maxCreatesPerTick`, `stallReport`, `baseSpeedPerCore`; an empty `nodeSelector` or `tolerations` clears the default, `null` keeps it). 201 with `runId`, `jobs`, `groups` and `startedAt`; 400 for a bad request or trace, naming the trace line; 409 while a run is active, or when the run id already has results. |
| `/sim/v1/runs/current` | GET | 200 with the active or last run's status: `phase` (`running`, `finished` or `aborted`), `elapsedSeconds`, `allSubmitted`, the jobs per state and `runStalled`; 404 before the first run. |
| `/sim/v1/runs/current/finish` | POST | Ends the run; jobs not done end incomplete. `?cleanup=true` also deletes the run's Job objects, with their pods, or its bare pods, in the background: one `deletecollection` call per namespace by the run's label, or one delete per object when the role lacks that verb. 200 with the final status once the run's files are closed. |

**Permissions.** The `joulie-simulator` ClusterRole of `charts/joulie-simulator` grants `get`, `list`, `watch`, `create` and `delete` on pods and nodes, which both engines use. With `rbac.workloadRuns` (by default when `emulate.enabled` is true) it adds what a v1 run needs: `patch` and `deletecollection` on pods, for the completion annotation and the cleanup; `get`, `list`, `watch`, `create`, `delete` and `deletecollection` on `batch` jobs, for the Job objects and their cleanup; and `get`, `list` and `watch` on `joulie.io` nodetwins, which a run observes for each node's schedulable class and never writes.

**KWOK Stage.** With `SIM_WORKLOAD_COMPLETION=kwok`, a pod ends only through a KWOK Stage named `pod-complete` that selects Running pods annotated `sim.joulie.io/complete: "true"`: `scripts/simcluster/manifests/stage-pod-complete.yaml`, adapted from KWOK v0.8.0. It replaces KWOK's own `pod-complete` Stage, which completes every Running pod a Job owns on a timer, before it did its work; the simulated cluster applies it after KWOK's Stages. On another KWOK cluster, apply it the same way, or set `SIM_WORKLOAD_COMPLETION=delete`. `simscenario run` refuses a cluster whose `pod-complete` Stage does not select the annotation.

**Scenario runner.** `scripts/simcluster/scenario.sh` (`make simcluster-scenario SCENARIO=<file>`) has its own settings: [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md#settings" >}}). So do the other scripts of `scripts/simcluster`: [Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md#settings" >}}).

### Simulator telemetry export

In emulate mode the simulator (`simulator/cmd/simulator`) pushes OpenTelemetry metrics of every emulated node and of the facility as OTLP JSON over HTTP, for example straight to a Prometheus started with `--web.enable-otlp-receiver`. Events are always written to `events.jsonl` and pushed only when a logs endpoint is set. Names and Prometheus spellings: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

In every mode the simulator also answers reads of one node over HTTP: `GET /otlp/telemetry/{node}` and `GET /telemetry/{node}` serve the same OTLP JSON `ExportMetricsServiceRequest`, the format `TELEMETRY_CPU_HTTP_ENDPOINT` and `NODE_POWER_HTTP_ENDPOINT` read. `?format=legacy` serves the deprecated `packagePowerWatts` JSON until v0.4.0, with one warning per process. `?format=otlp` asks for the default explicitly; any other `format` value is refused with 400.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_EXPORTERS` | (empty) | Comma-separated compatibility exporters to serve at `/exporters/<name>/metrics`: `kepler-0.7`, `kepler-0.12`, `dcgm-4.8.4`, `amd-dme-1.5.2` (ROCm device-metrics-exporter v1.5.2, AMD GPUs). `kepler-0.12` reads the RAPL zones of each node's tree, and serves its GPU series only with `SIM_KEPLER012_GPU`. An unknown name stops the start and lists the valid ones. See [Compatibility exporters]({{< relref "/docs/simulator/metrics.md#compatibility-exporters-emulate-mode" >}}). |
| `SIM_KEPLER012_GPU` | `false` | `true` makes `kepler-0.12` also serve `kepler_node_gpu_*` for NVIDIA GPUs, as Kepler's experimental GPU option does. With the chart, set it in `extraEnv`. |
| `SIM_DCGM_HOSTNAME_LABEL` | `hostname` | Host label of `dcgm-4.8.4`: `hostname`, or `Hostname` as dcgm-exporter releases up to 4.8.2 spell it. |
| `SIM_OTEL_FILE_DIR` | the value of `SIM_DEBUG_PERSIST_DIR` (default `/tmp/joulie-simulator-debug`) | Directory of `events.jsonl`, one OTLP JSON `LogsData` object per line. Empty disables the files. |
| `SIM_OTEL_FILE_METRICS` | `false` | Also append every metrics export to `metrics.jsonl` in the same directory, one `MetricsData` object per line. Read with Go's `strconv.ParseBool`: `1`, `t`, `T`, `true`, `TRUE`, `True`, or the same spellings of false; any other value stops the start. |
| `POD_NAME` | the host name | Identifies this simulator instance in its own telemetry. The chart sets it from the downward API. |
| `OTEL_SDK_DISABLED` | `false` | `true`, in any case, turns off both pushes, as the OpenTelemetry specification reads it; any other value leaves them on. |
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

For queries, Prometheus should promote `k8s.node.name` and `host.name` onto the data series (`otlp.promote_resource_attributes`); without that, the node name is joined from `target_info`. See [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}).

### Simulator hardware profiles

The simulator (`simulator/cmd/simulator`) emulates a node from a hardware profile picked by name: one of the builtin profiles in `simulator/pkg/hwemu/profiles/`, or a local one. Local profiles stay outside the repository and are the only ones whose measured sources (`measured:` provenance) may name the machine they were measured on, such as a calibration of your own nodes. A builtin profile may cite a measurement too, but describes the machine by its hardware only.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIM_PROFILES_DIR` | (empty) | Directory of local hardware profiles: every `*.yaml` file directly in it, each with a `name` equal to its file name without `.yaml`. Empty means the builtin profiles only, and so does a directory with no `*.yaml` in it, such as a mounted ConfigMap that holds no profile yet. Hidden entries (a leading dot) are skipped. A directory that does not exist, a path that is not a directory and a `*.yml` file, or a `.yaml` suffix in another case such as `.YAML`, are errors, never a fallback to the builtins. |

A local profile may extend (`extends:`) a builtin profile or another local one, by the same merge rules the builtins use. It may not take a builtin's name, since one of the two would be hidden: give it a new name and extend the builtin instead. The simulator logs the local profiles once, as `local profiles loaded dir=<dir> profiles=<names>`.

## kubectl plugin

The `kubectl joulie` plugin reads the cluster through your kubeconfig, as kubectl does, and needs no configuration. It lists `NodeTwin`, `NodeHardware`, `Node` and `Pod` objects; without permission to list `NodeHardware` it shows no hardware and works otherwise. Install it with `kubectl krew install joulie`, or see the [quickstart]({{< relref "/docs/getting-started/01-quickstart.md#kubectl-plugin" >}}).

| Command | Shows |
|---------|-------|
| `kubectl joulie`, `kubectl joulie status` | the cluster summary (measured, capped and TDP power, nodes and their power per class, pods per class, average headroom, cooling and PSU stress, PUE) and one row per managed node |
| `kubectl joulie top` | the live dashboard: the status screen refreshed every `--interval`, with a power sparkline per node that takes one point per `NodeTwin` update; `↑` `↓` select, `enter` opens the node, `p` the pods, `s` sorts, `r` refreshes, `q` quits |
| `kubectl joulie node <node>` | one node: hardware, power, twin scores, the caps and governor presets planned and what its agent applied, its pods |
| `kubectl joulie pods` | every pod on a managed node: its class, its node and the node's class; a performance pod on an eco node is flagged. A pod is `performance` when it asks for a performance node, as the controller manager reads it: the `joulie.io/workload-class: performance` annotation, or a node selector or node affinity on `joulie.io/power-profile: performance`; any other pod is `standard` |
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

`-o json` prints `time`, `summary` (`nodes` per class, `pods`, `performancePods`, `misplacedPods`, `podsError`, `measuredPowerW`, `cappedPowerW`, `tdpW`, `powerTrendWPerMin`, `avgHeadroomPct`, `avgCoolingStressPct`, `avgPsuStressPct`, `avgPUE`, `agentsReporting`, `agentsNotReporting`), `nodes` for `status` and `node` (`name`, `class`, `cpuModel`, `sockets`, `gpuModel`, `gpus`, `powerSource`, `measuredPowerW`, `cappedPowerW`, `tdpW`, `powerTrendWPerMin`, `cpuCapPct`, `gpuCapPct`, `headroomPct`, `coolingStressPct`, `psuStressPct`, `cpuRequestedPct`, `memoryRequestedPct`, `gpuRequestedPct`, `pods`, `performancePods`, `agentReporting`, `agentReason`) and `pods` for `pods` and `node` (`namespace`, `name`, `class`, `node`, `nodeClass`, `misplaced`, `cpu`, `gpus`). `cpuCapPct` and `gpuCapPct` are left out while the twin has no cap state yet, and the GPU fields for a node without GPUs. `podsError` is set when the pods could not be listed; the pod counts are then unknown, not zero. A command's list is present even when empty (`"pods": []`).
