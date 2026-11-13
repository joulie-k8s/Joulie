---
title: "Simulator Metrics"
weight: 28
---

This page lists every metric and event the simulator emits, to write a query or a dashboard against it. There are three kinds:

| Kind | Where | Mode |
|---|---|---|
| Prometheus metrics about the simulator itself (`joulie_sim_*`) | scraped at `/metrics` | every mode |
| OpenTelemetry metrics and events about the emulated nodes and the facility | metrics pushed as OTLP JSON over HTTP; events in `events.jsonl`, pushed when a logs endpoint is set | [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) |
| Compatibility exporters: the Prometheus formats of Kepler, dcgm-exporter and the ROCm device-metrics-exporter | `/exporters/<name>/metrics`, opt-in | emulate mode |

The OpenTelemetry names are spelled once, in `pkg/otel/semconv`, and the controller manager reads the same names back from Prometheus. Their Prometheus spelling is computed by `pkg/otel/promname`.

## Prometheus metrics at `/metrics`

Served at `/metrics` on the simulator's listen address (`SIM_ADDR`, default `127.0.0.1:18080`, and `:18080` in a pod). The JSON debug endpoints are listed under [HTTP API]({{< relref "/docs/simulator/simulator.md#http-api" >}}).

### HTTP/request metrics

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_requests_total{route,method,status}` | counter | HTTP requests by route, method and status |
| `joulie_sim_request_duration_seconds{route,method}` | histogram | request latency |

### Control-path metrics

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_controls_total{node,action}` | counter | control actions received, by node and action |
| `joulie_sim_control_actions_total{node,action,result}` | counter | control action outcomes, `result` `applied`, `blocked` or `error` |

### Per-node simulated state metrics

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_node_cap_watts{node}` | gauge | current simulated effective cap |
| `joulie_sim_node_rapl_cap_watts{node}` | gauge | simulated RAPL cap value |
| `joulie_sim_node_throttle_pct{node}` | gauge | simulated DVFS throttle percent |
| `joulie_sim_node_power_watts{node}` | gauge | simulated exported node power |
| `joulie_sim_node_cpu_util{node}` | gauge | simulated CPU utilization |
| `joulie_sim_node_freq_scale{node}` | gauge | simulated frequency scale |
| `joulie_sim_node_running_pods{node}` | gauge | running pods observed on the node |
| `joulie_sim_node_class_info{node,class}` | gauge | class assignment marker (`1` on the active class) |

In emulate mode the per-node gauges of an emulated node are set from its snapshot at every tick.

### Facility metrics

The [facility model]({{< relref "/docs/simulator/simulator.md#facility-model" >}}), as gauges:

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_facility_ambient_temp_celsius` | gauge | outdoor ambient temperature, in degrees Celsius |
| `joulie_sim_facility_it_power_watts` | gauge | total IT power of the simulated nodes |
| `joulie_sim_facility_cooling_power_watts` | gauge | cooling power, derived from the IT power and the PUE |
| `joulie_sim_facility_pue` | gauge | simulated Power Usage Effectiveness |

### Workload/job metrics

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_job_submitted_total{class}` | counter | jobs submitted by class |
| `joulie_sim_job_completed_total{class,node}` | counter | jobs completed by class and node |
| `joulie_sim_job_completion_seconds` | histogram | job completion latency distribution |

### Emulate mode and export metrics

| Metric | Type | Meaning |
|---|---|---|
| `joulie_sim_emulate_nodes` | gauge | emulated nodes, one per cluster spec member |
| `joulie_sim_emulate_tick_duration_seconds` | histogram | time one tick takes to step every emulated node |
| `joulie_sim_emulate_tick_overruns_total` | counter | ticks that took longer than the tick interval |
| `joulie_sim_emulate_node_step_errors_total{node}` | counter | failed steps of one emulated node |
| `joulie_sim_kepler07_idle_excess_joules_total{instance,family}` | counter | served at `/exporters/kepler-0.7/metrics`, not at `/metrics`: the idle energy the emulated Kepler 0.7 reported beyond what its series measured ([Compatibility exporters](#compatibility-exporters-emulate-mode)) |
| `joulie_sim_otlp_requests_total{signal,result}` | counter | OTLP exports by signal (`metrics`, `logs`) and result: `success`, `partial` (some points rejected), `error`, `dropped` (logs queue full) |
| `joulie_sim_otlp_points_total{signal}` | counter | data points and log records the receiver accepted |
| `joulie_sim_otlp_dropped_points_total{signal}` | counter | values left out of the export because they were NaN, infinite or out of the int64 range |
| `joulie_sim_events_total{event}` | counter | events emitted, by `eventName` |

Energy totals of label-modelled nodes are at `/debug/energy`, and their temperatures and averaged power at `/debug/nodes`, not in Prometheus.

## OpenTelemetry metrics (emulate mode)

The names follow the OpenTelemetry semantic conventions v1.44.0 (`schemaUrl: https://opentelemetry.io/schemas/1.44.0` on every resource and scope); names no convention covers use the prefix `joulie.`. Points are cumulative and carry the time of the export that sends them. How they are sent: [Simulator telemetry export]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-telemetry-export" >}}).

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

The facility is a resource of its own, with `service.instance.id` `facility`. Select nodes in Prometheus by `k8s_node_name`.

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
| `hw.gpu.io` | By | Counter (int), one point per `network.io.direction` and `joulie.hw.gpu.io.path` | GPUs: bytes received from and sent to the other GPUs of the node (`peer` path only) | `hw.id`, `hw.name`, `hw.vendor`, `hw.model`, `network.io.direction` (`receive`, `transmit`), `joulie.hw.gpu.io.path` (`peer`, `host`, `nic`) | `hw_gpu_io_bytes_total` |
| `joulie.hw.gpu.speed` | Hz | Gauge (int), one point per `joulie.hw.clock.domain` | GPUs with a clock model | `hw.id`, `hw.vendor`, `hw.model`, `joulie.hw.clock.domain` (`graphics`, `memory`, `fabric`, `soc`) | `joulie_hw_gpu_speed_hertz` |
| `joulie.hw.gpu.speed.limit` | Hz | Gauge (int), one point per `joulie.hw.clock.domain` and `hw.limit_type`: `max` and `throttled` | the same: `max` the hardware maximum, `throttled` the maximum in effect | `hw.id`, `hw.vendor`, `hw.model`, `joulie.hw.clock.domain`, `hw.limit_type` | `joulie_hw_gpu_speed_limit_hertz` |
| `joulie.hw.governor` | 1 | UpDownCounter (int): 1 for the state a component is in, 0 for the one it left | CPU packages and GPUs with a governor state; the preset is empty when no single preset matches ([Preset names]({{< relref "/docs/hardware/hardware-modeling.md#preset-names" >}})) | `hw.id`, `hw.type`, `joulie.hw.governor.preset`, `joulie.hw.governor.native` | `joulie_hw_governor` |
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

Only readings a real collector could measure are emitted, so a virtual machine node gets no `hw.*` metric. Whole-host energy is only `hw.host.energy`, never `hw.energy`, so a sum over `hw.energy` never counts the host twice. Counters never decrease, not even across a simulator restart.

### Prometheus

A Prometheus started with `--web.enable-otlp-receiver` accepts the push at `/api/v1/otlp/v1/metrics`. Its settings (promoting `k8s.node.name` and `host.name`, the translation strategy, the out-of-order window), the spelling of every name under the other translation strategies, and the pitfalls are under [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}), [Name translation strategies]({{< relref "/docs/architecture/telemetry.md#name-translation-strategies" >}}) and [Pitfalls]({{< relref "/docs/architecture/telemetry.md#pitfalls" >}}). The full table for every name is `pkg/otel/promname/testdata/prometheus-names.tsv`.

## Events (emulate mode)

Events are OpenTelemetry log records with an `eventName`, on their node's resource; read the attributes, not the body. They are always appended to `events.jsonl` in `SIM_OTEL_FILE_DIR` and pushed only when a logs endpoint is set.

| `eventName` | Severity | When | Attributes |
|---|---|---|---|
| `joulie.sim.node.ready` | 9 INFO | an emulated node's tree is in place and its first step ran, once per node per start | `joulie.sim.profile`, `joulie.sim.pool`, `joulie.sim.tree.resumed`, `joulie.sim.tree.added` |
| `joulie.sim.node.mismatch` | 13 WARN | the Kubernetes Node of a spec node is missing or names another profile; once per change, not once per poll | `error.type` (`node_missing`, `profile_mismatch`), `joulie.sim.profile`, `joulie.sim.node.profile` |
| `joulie.sim.cap.applied` | 9 INFO | a power limit written to a node file took effect, possibly quantized or clamped | `hw.id`, `hw.type`, `joulie.sim.file`, `joulie.sim.outcome`, `joulie.sim.value.written`, `joulie.sim.value.effective` |
| `joulie.sim.setting.applied` | 9 INFO | another setting took effect, such as a frequency bound or a governor | the same |
| `joulie.sim.cap.reset` | 13 WARN | a GPU limit went back to its default: persistence mode off and no client | the same, with `hw.type=gpu`; written is the old limit, effective the default |
| `joulie.sim.write.rejected` | 13 WARN | a write was reverted | the same, and `error.type` (`reverted_out_of_range`, `parse_error`, `restored`, `einval`, `eopnotsupp`, `ebusy`, `enodata`) |

`error.type` `einval`, `eopnotsupp`, `ebusy` and `enodata` are the errno the real driver returns; with plain files the value is reverted at the next step, in [FUSE mode]({{< relref "/docs/simulator/emulate-mode.md#fuse-mode" >}}) the write fails at once.

## Workload run and scenario names

A v1 workload run writes the `joulie.sim.*` names to `<SIM_RUNS_DIR>/<runId>/otlp/` (`simulator.metrics.jsonl`, `simulator.logs.jsonl`), and the scenario runner writes the `joulie.capture.*` and `joulie.scenario.*` names to `harness.metrics.jsonl` and `harness.logs.jsonl` beside them, as OTLP JSON Lines; none is pushed. See [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}).

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

## Controller manager names

The controller manager exports this name itself, from the grid carbon intensity its facility input reads (`FACILITY_CARBON_INTENSITY_QUERY`); the simulator does not emit it. See [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#grid-carbon-intensity" >}}).

| OTel name | Unit | Instrument | Point attributes | Prometheus name |
|---|---|---|---|---|
| `joulie.grid.carbon_intensity` | `g/(kW.h)` | Gauge | `joulie.grid.carbon_intensity.signal` (`marginal` or `average`) | `joulie_grid_carbon_intensity_grams_per_kW_h` |

## Compatibility exporters (emulate mode)

`SIM_EXPORTERS` turns on exporters that reproduce the Prometheus format of a pinned release of a real exporter, computed from the same counters the agent reads, to test Joulie's `kepler` and `kepler-legacy` input presets and Collector bridges. The OpenTelemetry export above is the default.

| Name | Reproduces | Series | Notes |
|---|---|---|---|
| `kepler-0.7` | Kepler v0.7.12 (the same schema through 0.9.0) | `kepler_node_{package,core,uncore,dram,other,gpu,platform}_joules_total{package, instance, mode}` with the constant label `source` | One series per socket and `mode` (`dynamic`, `idle`); GPU series for NVIDIA only. Under a cap the idle and dynamic modes can add up to more than the node draws ([`kepler-legacy` under a cap]({{< relref "/docs/architecture/telemetry.md#the-queries" >}})). A node without RAPL emits nothing. |
| `kepler-0.12` | Kepler v0.12.0 | `kepler_node_cpu_{joules_total,watts}` and `kepler_node_cpu_{active,idle}_{joules_total,watts}` with `zone`, `path` and the constant label `node_name`; for NVIDIA GPUs `kepler_node_gpu_{,active_,idle_}{joules_total,watts}` with `gpu`, `gpu_uuid`, `gpu_name`, `vendor` | One zone per RAPL zone of the node's tree, named as Kepler names them. GPU series only with `SIM_KEPLER012_GPU=true`. A node without RAPL emits nothing. |
| `dcgm-4.8.4` | dcgm-exporter 4.8.4 | `DCGM_FI_DEV_POWER_USAGE` (W), `DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION` (mJ), `DCGM_FI_DEV_GPU_UTIL` (%) per NVIDIA GPU | The labels of the real exporter; `SIM_DCGM_HOSTNAME_LABEL` spells `Hostname` as releases up to 4.8.2 do. |
| `amd-dme-1.5.2` | ROCm device-metrics-exporter v1.5.2 | `gpu_package_power` and `gpu_power_usage` (whole W), `gpu_energy_consumed` (uJ), `gpu_gfx_activity` (%) per AMD GPU, all typed gauge as upstream does | The fourteen labels upstream always sets; readings of 0 are left out, and no power cap metric is served, as upstream. |

Counters an exporter keeps in memory restart from zero with the simulator, as the real exporters do; those that are hardware counters upstream (Kepler 0.12 zones, DCGM and AMD GPU energy) continue across a restart.

The producer rules, timestamps, event semantics and how each exporter maps the emulated hardware: [Simulator telemetry](https://github.com/joulie-k8s/Joulie/wiki/Simulator-Telemetry) on the wiki.
