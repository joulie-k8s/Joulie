---
title: "Simulator Metrics"
weight: 28
---

The simulator publishes three kinds of telemetry:

- **Prometheus metrics about itself** at `/metrics` (`joulie_sim_*`), for Prometheus to scrape.
- **OpenTelemetry metrics and events about the emulated nodes and the facility**, in [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) only. Metrics are pushed as OTLP JSON over HTTP; events are written to `events.jsonl` and pushed when a logs endpoint is set.
- **Compatibility exporters**, in emulate mode only: the Prometheus formats of real exporters (Kepler, dcgm-exporter, the ROCm device-metrics-exporter) at `/exporters/<name>/metrics`, opt-in.

The OpenTelemetry names are spelled once, in `pkg/otel/semconv`, and the controller manager reads the same names back from Prometheus. Their Prometheus spelling is computed by `pkg/otel/promname`.

## Prometheus metrics at `/metrics`

Served at `/metrics` on the simulator's listen address (`SIM_ADDR`, default `127.0.0.1:18080`, and `:18080` in a pod). The JSON debug endpoints are listed under [HTTP API]({{< relref "/docs/simulator/simulator.md#http-api" >}}).

### HTTP/request metrics

- `joulie_sim_requests_total{route,method,status}` (counter)
  - total HTTP requests by route/method/status
- `joulie_sim_request_duration_seconds{route,method}` (histogram)
  - request latency

### Control-path metrics

- `joulie_sim_controls_total{node,action}` (counter)
  - received control actions by node/action
- `joulie_sim_control_actions_total{node,action,result}` (counter)
  - control action outcomes
  - `result`: `applied|blocked|error`

### Per-node simulated state metrics

- `joulie_sim_node_cap_watts{node}` (gauge)
  - current simulated effective cap
- `joulie_sim_node_rapl_cap_watts{node}` (gauge)
  - simulated RAPL cap value
- `joulie_sim_node_throttle_pct{node}` (gauge)
  - simulated DVFS throttle percent
- `joulie_sim_node_power_watts{node}` (gauge)
  - simulated exported node power
- `joulie_sim_node_cpu_util{node}` (gauge)
  - simulated CPU utilization
- `joulie_sim_node_freq_scale{node}` (gauge)
  - simulated frequency scale
- `joulie_sim_node_running_pods{node}` (gauge)
  - running pods observed on the node
- `joulie_sim_node_class_info{node,class}` (gauge)
  - class assignment marker (`1` on active class)

In emulate mode the per-node gauges of an emulated node are set from its snapshot at every tick.

### Facility metrics

The [facility model]({{< relref "/docs/simulator/simulator.md#facility-model" >}}), as gauges:

- `joulie_sim_facility_ambient_temp_celsius` (gauge)
  - outdoor ambient temperature, in degrees Celsius
- `joulie_sim_facility_it_power_watts` (gauge)
  - total IT power of the simulated nodes
- `joulie_sim_facility_cooling_power_watts` (gauge)
  - cooling power, derived from the IT power and the PUE
- `joulie_sim_facility_pue` (gauge)
  - simulated Power Usage Effectiveness

### Workload/job metrics

- `joulie_sim_job_submitted_total{class}` (counter)
  - jobs submitted by class
- `joulie_sim_job_completed_total{class,node}` (counter)
  - jobs completed by class and node
- `joulie_sim_job_completion_seconds` (histogram)
  - job completion latency distribution

### Emulate mode and export metrics

- `joulie_sim_emulate_nodes` (gauge)
  - emulated nodes, one per cluster spec member
- `joulie_sim_emulate_tick_duration_seconds` (histogram)
  - time one tick takes to step every emulated node
- `joulie_sim_emulate_tick_overruns_total` (counter)
  - ticks that took longer than the tick interval; the next tick advances to wall time
- `joulie_sim_emulate_node_step_errors_total{node}` (counter)
  - failed steps of one emulated node
- `joulie_sim_otlp_requests_total{signal,result}` (counter)
  - OTLP exports by signal (`metrics`, `logs`) and result
  - `result`: `success|partial|error|dropped` (`partial`: the receiver rejected some points; `dropped`: the logs queue was full)
- `joulie_sim_otlp_points_total{signal}` (counter)
  - data points and log records the receiver accepted
- `joulie_sim_otlp_dropped_points_total{signal}` (counter)
  - values left out of the export because they were NaN, infinite or out of the int64 range
- `joulie_sim_events_total{event}` (counter)
  - events emitted, by `eventName`

### Notes

- Prometheus metrics capture online simulator state and request/control behavior.
- Integrated node/cluster energy totals of the label-modelled nodes are exposed through `/debug/energy` (JSON), not as Prometheus time series. Emulated nodes export their energy as OpenTelemetry counters (below).
- Thermal state and averaged versus instantaneous power are not Prometheus gauges: fields such as `instantPackagePowerWatts`, `cpu.temperatureC`, `cpu.thermalThrottlePct` and per-device GPU averaged power are in the legacy JSON of `/telemetry/{node}?format=legacy` (until v0.4.0) and in `/debug/nodes`.

## OpenTelemetry metrics (emulate mode)

The names follow the OpenTelemetry semantic conventions v1.44.0, and every resource and scope carries `schemaUrl: https://opentelemetry.io/schemas/1.44.0`. Every `hw.*` name is at Development stability upstream and may be renamed there; the pin makes such a rename a deliberate change in Joulie. Names no convention covers use the prefix `joulie.`. Points are cumulative, so Prometheus accepts them without `otlp-deltatocumulative`. Every pushed point carries the time of the export that sends it, with the values of the newest emulation step, as an OpenTelemetry SDK's periodic reader stamps the time it collects; a step that has not changed since the last export is sent again at the later time, and counters keep their ledger's start as `startTimeUnixNano`. A point stamped with the step's time would reach Prometheus up to a step old, and `rate()` would read low (see the [pitfalls]({{< relref "/docs/architecture/telemetry.md#pitfalls" >}})). The per-node `/otlp/telemetry/{node}` route keeps the step's time instead, because its reader derives power from the change between two reads. How they are sent is described under [Simulator telemetry export]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-telemetry-export" >}}).

### Resources

Each emulated node is one resource:

| Attribute | Value |
|---|---|
| `service.name` | `OTEL_SERVICE_NAME`, default `joulie-simulator` |
| `service.namespace` | `joulie` |
| `service.instance.id`, `k8s.node.name`, `host.name` | the node name |
| `host.id` | `hwemu-` followed by the first 32 hex characters of sha256(node name) |
| `k8s.cluster.name` | the cluster spec's name |
| `service.version` | the simulator's module version |

The facility is a resource of its own, with `service.instance.id` `facility` and `k8s.cluster.name`. `OTEL_RESOURCE_ATTRIBUTES` adds attributes but never overrides `service.instance.id`, `k8s.node.name` or `host.name`. In Prometheus a node's series get `job="joulie/joulie-simulator"` and `instance="<node>"`; that is a convenience of the default setup, not part of the contract, so select nodes by `k8s_node_name`.

### Metrics

Prometheus names are those of the default translation strategy (`UnderscoreEscapingWithSuffixes`).

| OTel name | Unit | Instrument | Emitted for | Point attributes | Prometheus name |
|---|---|---|---|---|---|
| `hw.host.energy` | J | Counter (int) | a measured host | `hw.id=host` | `hw_host_energy_joules_total` |
| `hw.host.power` | W | Gauge | a measured host | `hw.id=host` | `hw_host_power_watts` |
| `hw.host.ambient_temperature` | Cel | Gauge | a measured host: the inlet, which is its cooling zone's temperature | `hw.id=host` | `hw_host_ambient_temperature_celsius` |
| `hw.energy` | J | Counter | measured CPU packages, DRAM zones and GPUs | `hw.id`, `hw.type`, `hw.name`, `hw.parent` (memory only) | `hw_energy_joules_total` |
| `hw.power` | W | Gauge | the same | `hw.id`, `hw.type`, `hw.name`, `hw.parent` (memory only) | `hw_power_watts` |
| `hw.cpu.speed` | Hz | Gauge (int) | CPU packages with cpufreq | `hw.id`, `hw.name`, `hw.vendor`, `hw.model` | `hw_cpu_speed_hertz` |
| `hw.cpu.speed.limit` | Hz | Gauge (int), one point per `hw.limit_type`: `max` and `throttled` | the same | `hw.id`, `hw.name`, `hw.vendor`, `hw.model`, `hw.limit_type` | `hw_cpu_speed_limit_hertz` |
| `hw.gpu.utilization` | 1 | Gauge | GPUs | `hw.id`, `hw.name`, `hw.vendor`, `hw.model`, `hw.gpu.task=general` | `hw_gpu_utilization_ratio` |
| `joulie.hw.power.limit` | W | Gauge, one point per `joulie.hw.limit.state`: `requested` and `enforced` | components with a power limit | `hw.id`, `hw.type`, `joulie.hw.limit.state` | `joulie_hw_power_limit_watts` |
| `hw.gpu.io` | By | Counter (int), one point per `network.io.direction` and `joulie.hw.gpu.io.path` | GPUs, on the `peer` path only: the bytes a GPU received from and sent to the other GPUs of its node. The emulator models no host or network interface bytes, so `host` and `nic` have no point. The counters come from the node's ledger and start at its start | `hw.id`, `hw.name`, `hw.vendor`, `hw.model`, `network.io.direction` (`receive`, `transmit`), `joulie.hw.gpu.io.path` (`peer`, `host`, `nic`) | `hw_gpu_io_bytes_total` |
| `joulie.hw.gpu.speed` | Hz | Gauge (int), one point per `joulie.hw.clock.domain` | GPUs with a clock model, every GPU of the builtin profiles: `graphics` and `memory` on NVIDIA, plus `fabric` and `soc` on an AMD family whose profile has fclk and socclk tables | `hw.id`, `hw.vendor`, `hw.model`, `joulie.hw.clock.domain` (`graphics`, `memory`, `fabric`, `soc`) | `joulie_hw_gpu_speed_hertz` |
| `joulie.hw.gpu.speed.limit` | Hz | Gauge (int), one point per `joulie.hw.clock.domain` and `hw.limit_type`: `max` and `throttled` | the GPUs and domains of `joulie.hw.gpu.speed`: `max` is the domain's hardware maximum (the top of its `pp_dpm_*` table, NVML's maximum clock), `throttled` the maximum in effect, the clock a busy GPU reaches under the present performance level, overdrive range or clock lock (on NVIDIA at most the sustained maximum) | `hw.id`, `hw.vendor`, `hw.model`, `joulie.hw.clock.domain`, `hw.limit_type` | `joulie_hw_gpu_speed_limit_hertz` |
| `joulie.hw.governor` | 1 | UpDownCounter (int): 1 for the state a component is in, 0 for the one it left | CPU packages and GPUs with a governor state: 1 for the state, and 0 for the state the component was in before, sent with every export until the state changes again and dropped by a simulator restart. `joulie.hw.governor.preset` is the preset `pkg/governor` maps the native state back to, empty when no single preset does ([Preset names]({{< relref "/docs/hardware/hardware-modeling.md#preset-names" >}})) | `hw.id`, `hw.type`, `joulie.hw.governor.preset`, `joulie.hw.governor.native` | `joulie_hw_governor` |
| `joulie.facility.ambient_temperature` | Cel | Gauge | the facility | | `joulie_facility_ambient_temperature_celsius` |
| `joulie.facility.it.energy` | J | Counter | the facility | | `joulie_facility_it_energy_joules_total` |
| `joulie.facility.it.power` | W | Gauge | the facility | | `joulie_facility_it_power_watts` |
| `joulie.facility.cooling.energy` | J | Counter | the facility | | `joulie_facility_cooling_energy_joules_total` |
| `joulie.facility.cooling.power` | W | Gauge | the facility | | `joulie_facility_cooling_power_watts` |
| `joulie.facility.pue` | 1 | Gauge | the facility; for dashboards, since the controller manager derives PUE itself | | `joulie_facility_pue_ratio` |
| `joulie.cooling_zone.ambient_temperature` | Cel | Gauge | each cooling zone | `joulie.cooling_zone.name` | `joulie_cooling_zone_ambient_temperature_celsius` |
| `joulie.rack.energy` | J | Counter | each rack | `joulie.rack.name` | `joulie_rack_energy_joules_total` |
| `joulie.rack.power` | W | Gauge | each rack | `joulie.rack.name` | `joulie_rack_power_watts` |

The point attributes become the labels `hw_id`, `hw_type`, `hw_name`, `hw_parent`, `hw_vendor`, `hw_model`, `hw_limit_type`, `joulie_hw_limit_state`, `network_io_direction`, `joulie_hw_gpu_io_path`, `joulie_hw_clock_domain`, `joulie_hw_governor_preset`, `joulie_hw_governor_native`, `joulie_cooling_zone_name` and `joulie_rack_name`. The resource attributes `k8s_node_name` and `host_name` are always on `target_info`, and on the data series only when Prometheus promotes them (below).

### Components

| Component | `hw.id` | `hw.type` | `hw.name` | Also |
|---|---|---|---|---|
| CPU package | `cpu<socket>` | `cpu` | `package-<socket>` | `hw.vendor` (`Intel` or `AMD`) and `hw.model` on the CPU metrics |
| DRAM zone | `memory<socket>` | `memory` | `dram` | `hw.parent=cpu<socket>` |
| GPU | `gpu<index>` | `gpu` | the product name | `hw.vendor` and `hw.model` on the GPU metrics |
| Host | `host` | none | | |

### Producer rules

- Whole-host energy is reported only as `hw.host.energy`, never as `hw.energy`, so a sum over `hw.energy` never counts the host twice. `hw.host.*` points carry no `hw.type`.
- One `cpu` component per package. RAPL core, uncore and psys zones are never `cpu`. DRAM is `memory`; Joulie does not count it as node power.
- Host power is the packages, the DRAM zones, the platform remainder and the GPUs together. Power supply losses are not included.
- Only readings a real collector could measure are emitted: package and DRAM `energy_uj`, NVML total energy, amdgpu power. A node whose profile is a virtual machine gets no `hw.*` metric at all, and a CPU without a DRAM zone gets no `memory` component.
- Exported counters never decrease, not even across a simulator restart: they continue from a ledger kept in the tree root, the GPU bytes of `hw.gpu.io` as well as the energies. The ledger is rewritten each tick, in place and whole up to 2 KB, by rename above, so a killed simulator leaves the last whole tick. While the simulator is down no energy accrues, so a restart shows as a gap in the rate, never as a reset. A counter's start time is the ledger's start.

### Prometheus

A Prometheus started with `--web.enable-otlp-receiver` accepts the push at `/api/v1/otlp/v1/metrics`. Its settings (promoting `k8s.node.name` and `host.name`, the translation strategy, the out-of-order window), the spelling of every name under the other translation strategies, and the pitfalls are under [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}), [Name translation strategies]({{< relref "/docs/architecture/telemetry.md#name-translation-strategies" >}}) and [Pitfalls]({{< relref "/docs/architecture/telemetry.md#pitfalls" >}}). The full table for every name is `pkg/otel/promname/testdata/prometheus-names.tsv`.

## Events (emulate mode)

Events are OpenTelemetry log records with an `eventName`. `timeUnixNano` is when the change happened in the emulated node, `observedTimeUnixNano` when the simulator emitted it. The body is only a display string; read the attributes. A record is emitted on its node's resource, so the node is not an attribute.

They are always appended to `events.jsonl` in `SIM_OTEL_FILE_DIR`, one `LogsData` object per line, and pushed only when a logs endpoint is set. `/debug/events` and `events.ndjson` in `SIM_DEBUG_PERSIST_DIR` use the simulator's own JSON instead.

| `eventName` | Severity | When | Attributes |
|---|---|---|---|
| `joulie.sim.node.ready` | 9 INFO | an emulated node's tree is in place and its first step ran, once per node per start | `joulie.sim.profile`, `joulie.sim.pool`, `joulie.sim.tree.resumed`, `joulie.sim.tree.added` |
| `joulie.sim.node.mismatch` | 13 WARN | the Kubernetes Node of a spec node is missing or names another profile; once per change, not once per poll | `error.type` (`node_missing`, `profile_mismatch`), `joulie.sim.profile`, `joulie.sim.node.profile` |
| `joulie.sim.cap.applied` | 9 INFO | a power limit written to a node file took effect, possibly quantized or clamped | `hw.id`, `hw.type`, `joulie.sim.file`, `joulie.sim.outcome`, `joulie.sim.value.written`, `joulie.sim.value.effective` |
| `joulie.sim.setting.applied` | 9 INFO | another setting took effect, such as a frequency bound or a governor | the same |
| `joulie.sim.cap.reset` | 13 WARN | a GPU limit went back to its default: persistence mode off and no client | the same, with `hw.type=gpu`; written is the old limit, effective the default |
| `joulie.sim.write.rejected` | 13 WARN | a write was reverted | the same, and `error.type` (`reverted_out_of_range`, `parse_error`, `restored`, `einval`, `eopnotsupp`, `ebusy`, `enodata`) |

`joulie.sim.outcome` is the emulator's own name for what happened to the write, such as `accepted`, `clamped`, `rejected` or `reset-persistence-off`. `restored` means a file only the emulator writes was overwritten and put back. `einval`, `eopnotsupp`, `ebusy` and `enodata` mean the real driver refuses the value with that errno (outcome `rejected`): a file tree cannot fail the write itself, so the emulator reverts the value at its next step, and a reader sees its write not applied. Among others, `einval` comes from an unknown governor or EPP word and an out-of-range amdgpu `power1_cap`, `ebusy` from a non-zero EPP under the performance governor, and `enodata` from any write to the limit or time window of a locked or disabled RAPL constraint.

## Workload run and scenario names

A v1 workload run in emulate mode writes the `joulie.sim.*` names below to its run directory, `<SIM_RUNS_DIR>/<runId>/otlp/` (`simulator.metrics.jsonl`, a cumulative snapshot every `SIM_OTLP_FILE_INTERVAL` and one at the end; `simulator.logs.jsonl`, the run's and its jobs' events, with copies of the emulated nodes' hardware events). The scenario runner writes the `joulie.capture.*` and `joulie.scenario.*` names to `harness.metrics.jsonl` and `harness.logs.jsonl` next to them. Both are OTLP JSON Lines, one `MetricsData` or `LogsData` object per line, in the format of the OpenTelemetry Collector's file exporter; neither is pushed to the OTLP endpoint, and the `joulie_sim_*` Prometheus series above are a different set. Every resource of a run carries `joulie.sim.run.id`. See [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}).

| OTel name | Unit | Instrument | Point attributes | Prometheus name |
|---|---|---|---|---|
| `joulie.sim.workload.jobs.submitted` | `{job}` | Counter (int) | `joulie.workload.class` | `joulie_sim_workload_jobs_submitted_total` |
| `joulie.sim.workload.jobs.finished` | `{job}` | Counter (int) | `joulie.workload.class`, `joulie.sim.job.outcome` | `joulie_sim_workload_jobs_finished_total` |
| `joulie.sim.workload.jobs.active` | `{job}` | UpDownCounter (int) | `joulie.workload.class`, `joulie.sim.job.state` | `joulie_sim_workload_jobs_active` |
| `joulie.sim.workload.job.duration` | s | Histogram | `joulie.workload.class` | `joulie_sim_workload_job_duration_seconds` |
| `joulie.sim.workload.job.queue_wait` | s | Histogram | `joulie.workload.class` | `joulie_sim_workload_job_queue_wait_seconds` |
| `joulie.sim.workload.job.slowdown` | 1 | Histogram | `joulie.workload.class` | `joulie_sim_workload_job_slowdown` |
| `joulie.sim.workload.work` | `{unit}` | Counter | `joulie.workload.class`, `joulie.sim.work.kind` | `joulie_sim_workload_work_total` |
| `joulie.sim.workload.energy` | J | Counter | `joulie.workload.class`, `hw.type`, `k8s.node.name`, `joulie.sim.profile` | `joulie_sim_workload_energy_joules_total` |
| `joulie.capture.nodetwin.measured_power` | W | Gauge | `k8s.node.name`, `joulie.nodetwin.power_source` | `joulie_capture_nodetwin_measured_power_watts` |
| `joulie.capture.nodetwin.schedulable_class` | 1 | UpDownCounter (int) | `k8s.node.name`, `joulie.node.schedulable_class` | `joulie_capture_nodetwin_schedulable_class` |
| `joulie.capture.nodetwin.control_result` | 1 | UpDownCounter (int) | `k8s.node.name`, `hw.type`, `joulie.nodetwin.control.backend`, `joulie.nodetwin.control.result` | `joulie_capture_nodetwin_control_result` |
| `joulie.scenario.assertion.value` | `{value}` | Gauge | `joulie.assertion.name`, `joulie.assertion.source` | `joulie_scenario_assertion_value` |

| `eventName` | Severity |
|---|---|
| `joulie.sim.run.started`, `joulie.sim.run.finished`, `joulie.sim.run.resumed` | 9 INFO |
| `joulie.sim.run.stalled` | 13 WARN |
| `joulie.sim.job.submitted`, `joulie.sim.job.scheduled`, `joulie.sim.job.started`, `joulie.sim.job.resumed`, `joulie.sim.job.completed` | 9 INFO |
| `joulie.sim.job.adopted`, `joulie.sim.job.stalled`, `joulie.sim.job.incomplete` | 13 WARN |
| `joulie.sim.job.failed` | 17 ERROR |
| `joulie.scenario.started`, `joulie.scenario.variant.started`, `joulie.scenario.finished` | 9 INFO |
| `joulie.scenario.assertion.evaluated` | 9 INFO, or 17 ERROR with `error.type=assertion_failed` for a failed check |

## Calibration sweep names

A calibration sweep records these two names beside the hardware metrics above, and `hwcalib fit` reads them back. Neither the simulator nor any Joulie component emits them. See [Hardware Calibration]({{< relref "/docs/simulator/calibration.md" >}}).

| OTel name | Unit | Instrument | Point attributes | Prometheus name |
|---|---|---|---|---|
| `system.cpu.utilization` | 1 | Gauge | `cpu.mode`, `cpu.logical_number` (opt-in) | `system_cpu_utilization_ratio` |
| `joulie.calibration.setpoint` | `{setpoint}` | Gauge | `joulie.calibration.knob`, `joulie.calibration.class`, `joulie.calibration.memory_intensity` | `joulie_calibration_setpoint` |

## Compatibility exporters (emulate mode)

`SIM_EXPORTERS` turns on exporters that reproduce the Prometheus format of a pinned release of a real exporter, at `/exporters/<name>/metrics`. They are computed from the same counters the agent and the fake GPU tools read, so a query written for the real exporter reads the emulated nodes. They exist to test Joulie's `kepler` and `kepler-legacy` input presets and Collector bridges; the OpenTelemetry export above is the default. With the chart, the ServiceMonitor scrapes each listed exporter with `honorLabels: false`.

| Name | Reproduces | Series | Notes |
|---|---|---|---|
| `kepler-0.7` | Kepler v0.7.12 (the same schema through 0.9.0) | `kepler_node_{package,core,uncore,dram,other,gpu,platform}_joules_total{package, instance, mode}` with the constant label `source` | One series per socket, `mode` `dynamic` and `idle`, so each node has two `platform` series. `instance` is the node name; scraped with `honor_labels: false` it becomes `exported_instance`. GPU series for NVIDIA GPUs only, `source="NVML"`; the others `source="rapl-sysfs"`. `platform` is the host energy, and `other` the platform energy not counted in package, DRAM and GPU. Idle and dynamic split as upstream: each series keeps the power of its lowest-load interval as the idle power of every later interval, and dynamic is the rest; an interval that measured less gets dynamic 0 and the full recorded idle, so under a cap that holds a package below that idle the two modes add up to more than it draws ([`kepler-legacy` under a cap]({{< relref "/docs/architecture/telemetry.md#the-queries" >}})). A node without RAPL emits nothing. |
| `kepler-0.12` | Kepler v0.12.0 | `kepler_node_cpu_{joules_total,watts}` and `kepler_node_cpu_{active,idle}_{joules_total,watts}` with `zone`, `path` and the constant label `node_name`; for NVIDIA GPUs `kepler_node_gpu_{,active_,idle_}{joules_total,watts}` with `gpu`, `gpu_uuid`, `gpu_name`, `vendor` | The zones are the RAPL zones of the node's tree, named as Kepler names them: the socket index is stripped, so the packages of a 2-socket node are one zone `package` with `path="aggregated-package"`, while a die zone `package-N-die-M` keeps its name. `kepler_node_cpu_joules_total` is the zone's `energy_uj` and wraps at `max_energy_range_uj`, as upstream; the active and idle counters never wrap. Active energy is the node's CPU usage share; the first read of a node counts the whole counter as idle, at 0 W. The GPU series are served only with `SIM_KEPLER012_GPU=true`, as Kepler serves them only with its experimental GPU option. A node whose tree has no RAPL zone emits nothing, GPUs included. |
| `dcgm-4.8.4` | dcgm-exporter 4.8.4 | `DCGM_FI_DEV_POWER_USAGE` (W), `DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION` (mJ), `DCGM_FI_DEV_GPU_UTIL` (%) per NVIDIA GPU | Labels `gpu`, `UUID`, `pci_bus_id`, `device=nvidia<i>`, `modelName`, `hostname` (`Hostname` with `SIM_DCGM_HOSTNAME_LABEL`, as releases up to 4.8.2 spell it) and `DCGM_FI_DRIVER_VERSION`. The energy is the GPU's NVML total energy counter. |
| `amd-dme-1.5.2` | ROCm device-metrics-exporter v1.5.2 | `gpu_package_power` and `gpu_power_usage` (whole W), `gpu_energy_consumed` (uJ), `gpu_gfx_activity` (%) per AMD GPU, all typed gauge as upstream does | The fourteen labels upstream always sets: `gpu_id` (the index), `hostname` (the node), `card_model` (the product name here), `serial_number` (synthetic, stable per node and index), `gpu_partition_id="0"`, `gpu_compute_partition_type="spx"`, `gpu_memory_partition_type="nps1"`, `deployment_mode="baremetal"`, and `pod`, `namespace`, `container`, `job_id`, `job_user`, `job_partition`, which stay empty. The energy is the GPU's energy ledger. A power or energy reading of 0, or of a GPU without a power sensor, is left out, as upstream exports nothing for a field it reads as 0. Upstream has no power cap metric at v1.5.2, so none is served. |

The exporters keep their counters in memory, so they restart from zero with the simulator, as the real exporters do. Counters that are hardware counters upstream report the emulated hardware's own, which continue across a restart: the Kepler 0.12 totals the zones' `energy_uj`, the DCGM energy the GPU's device counter, the AMD GPU energy its ledger.
