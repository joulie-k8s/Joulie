---
title: "Input Telemetry and Actuation Interfaces"
weight: 50
---

This page describes runtime IO contracts:

- how Joulie reads telemetry inputs,
- how Joulie sends control intents.

If you want the CRD-level summary first, read [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}).
This page is the detailed runtime reference for the telemetry and control contract.

It is not the `/metrics` exposition contract.
For exported metrics, see [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}}).

## Why this abstraction exists

Joulie must run in two worlds with the same control logic:

- real hardware clusters,
- simulator/KWOK clusters.

So agent/controller manager logic depends on provider interfaces, not directly on sysfs or simulator HTTP shape.

## Backend selection in one sentence

Environment variables tell the agent which telemetry backend to read from and which control backend to write to.

Conceptually:

- `NodeHardware` defines discovered node capability
- `NodeTwin.spec` defines the target
- Agent environment variables define the wiring

Backend configuration should not carry hardware identity.
Its job is only backend routing.
In particular:

- do not conflate backend selection with CPU/GPU model or inventory identity,
- do not use it as a substitute for `NodeHardware`,
- do use it to route simulator HTTP or host backends.

## Telemetry provider model

Telemetry input can come from:

- `host` (real node interfaces),
- `http` (an endpoint serving the [HTTP input format](#http-input-format), such as the simulator),
- `none`.

Cluster-level inputs come from Prometheus and are read by the controller manager, not the agent: see [Input metrics (OpenTelemetry)](#input-metrics-opentelemetry).

Quality handling should distinguish `fresh`, `stale`, and `missing` samples so policy can degrade safely.

## Control provider model

Control outputs are routed by backend:

- `host`: write real interfaces (RAPL/cpufreq),
- `http`: send intents to simulator,
- `none`.

Result semantics:

- `applied`
- `blocked`
- `error`
- `none` (no intent selected for that control path)

This allows desired vs applied behavior auditing.

## Current CPU control intents

Supported intent actions:

- `rapl.set_power_cap_watts`
- `dvfs.set_throttle_pct`

DVFS intent is normalized as `throttlePct` (`0..100`) to stay portable across heterogeneous CPUs.
Backend-specific frequency writes remain implementation details.

## GPU control intent

Supported GPU intent action:

- `gpu.set_power_cap_watts`

Payload:

- `capWattsPerGpu`

Semantics:

- node-level intent is translated to per-device enforcement,
- same cap is applied to all GPUs on the node,
- result is reported as `applied|blocked|error` in `NodeTwin.status.controlStatus.gpu`.

## Current HTTP contracts

Telemetry endpoints:

- `GET /otlp/telemetry/{node}`: one OTLP JSON document, the [HTTP input format](#http-input-format). Point `TELEMETRY_CPU_HTTP_ENDPOINT` and `NODE_POWER_HTTP_ENDPOINT` here.
- `GET /telemetry/{node}`: the same OTLP JSON document by default (`?format=otlp` asks for it explicitly).
- `GET /telemetry/{node}?format=legacy`: the legacy JSON below. It is deprecated and removed in v0.4.0, and the simulator logs one warning per process when it is read. The agent and the controller manager still read the legacy JSON, with a warning, until v0.4.0. Any other `format` is refused with 400.

Accepted minimal legacy payloads include:

```json
{ "packagePowerWatts": 245.3 }
```

or

```json
{ "cpu": { "packagePowerWatts": 245.3 } }
```

The simulator exports a richer payload than this minimal contract. Important optional fields currently include:

- top level:
  - `packagePowerWatts`
  - `instantPackagePowerWatts`
- `cpu.*`:
  - `packagePowerWatts`
  - `instantPowerWatts`
  - `utilization`
  - `memoryIntensity`
  - `ioIntensity`
  - `freqScale`
  - `temperatureC`
  - `thermalThrottlePct`
- `gpu.*`:
  - `powerWattsTotal`
  - `avgPowerWattsTotal`
  - `utilization`
  - `memoryIntensity`
  - `cpuFeedIntensity`
  - `devices`

Controllers should treat these richer fields as additive observability, not as a replacement for the minimal contract above.

### HTTP input format

The agent (`TELEMETRY_CPU_SOURCE=http`) and the controller manager (`NODE_POWER_SOURCE=http`) read the same format: one OTLP JSON `ExportMetricsServiceRequest` per GET of the endpoint, with `{node}` replaced by the node name. It is the protobuf JSON mapping of OTLP: lowerCamelCase keys, 64-bit integers as decimal strings, integer enums. A body with a top-level `resourceMetrics` key is OTLP; any other body is read as the legacy JSON above, with one deprecation warning.

- **Node filter.** A resource that carries `k8s.node.name` is read only when it names the node. A resource without it is always read, since an endpoint that serves one node need not name it.
- **Power per component**, a component being one `(hw.type, hw.id)`:
  - from a cumulative `hw.energy` series: the energy difference to the previous point over the difference of their `timeUnixNano`, the producer's clock. A decrease or a new `startTimeUnixNano` is a reset, and an interval longer than the rate window (`PROMETHEUS_RATE_WINDOW` for the controller manager, 5 minutes for the agent) is too long; either interval is dropped and the series warms up again;
  - from a delta `hw.energy` point: its value over its own interval;
  - otherwise, and while its energy series warms up, from the `hw.power` gauge, as `rate(energy) or power` does in PromQL;
  - duplicates of one component count once, with their maximum.
- **Sum.** The agent sums the `hw.type=cpu` components. The controller manager sums `cpu` and `gpu`, or reads `hw.host.energy` and `hw.host.power` as one component under `NODE_POWER_SCOPE=host`.
- **Warm-up.** A reading is unavailable while a selected component has only an energy series without a rate and no power gauge: its first point, or its first after a reset. The controller manager then writes 0 W with `source: http` for that reconcile.

A minimal document for node `worker-01` with one CPU package:

```json
{"resourceMetrics": [{
  "resource": {"attributes": [
    {"key": "k8s.node.name", "value": {"stringValue": "worker-01"}},
    {"key": "service.instance.id", "value": {"stringValue": "worker-01"}}]},
  "scopeMetrics": [{"metrics": [
    {"name": "hw.energy", "unit": "J", "sum": {"aggregationTemporality": 2, "isMonotonic": true,
      "dataPoints": [{"attributes": [{"key": "hw.id", "value": {"stringValue": "cpu0"}},
                                     {"key": "hw.type", "value": {"stringValue": "cpu"}}],
                      "startTimeUnixNano": "1760000000000000000", "timeUnixNano": "1760000060000000000",
                      "asDouble": 15000}]}},
    {"name": "hw.power", "unit": "W", "gauge": {
      "dataPoints": [{"attributes": [{"key": "hw.id", "value": {"stringValue": "cpu0"}},
                                     {"key": "hw.type", "value": {"stringValue": "cpu"}}],
                      "timeUnixNano": "1760000060000000000", "asDouble": 250}]}}]}]}]}
```

Control endpoint:

- `POST /control/{node}`

Example request:

```json
{
  "node": "worker-01",
  "action": "dvfs.set_throttle_pct",
  "throttlePct": 20,
  "ts": "2026-02-27T00:00:00Z"
}
```

Expected response includes:

- `result` (`applied|blocked|error`)
- `message`
- `state` (best-effort post-state)

## Input metrics (OpenTelemetry)

The controller manager reads two measured inputs from Prometheus: the power of each node (`NODE_POWER_SOURCE=prometheus`, the chart default) and the facility readings (`ENABLE_FACILITY_METRICS=true`). Producers write them as OpenTelemetry metrics, pushed over OTLP to Prometheus' OTLP receiver: the simulator in emulate mode, an OpenTelemetry Collector, or any exporter that follows the table below. The controller manager does not receive OTLP; it runs PromQL instant queries, `GET <PROMETHEUS_ADDRESS>/api/v1/query`. Only the controller manager queries Prometheus; the scheduler extender reads `NodeTwin`.

Each input is one grouped query per reconcile (node power) or per poll (facility). The query aggregates by the key label, so the answer holds one series per node, cooling zone or rack, and every component of a node is in its sum; the answer is never read by keeping the first series returned. A key that comes back twice is refused (the result is counted as `error`), since it means two producers that Prometheus stores as one target. A node missing from the answer has no reading (`source: prometheus-no-data`), which is never the value of an earlier reconcile.

### The contract

All names come from `pkg/otel/semconv`, and their Prometheus spellings from `pkg/otel/promname`, which is checked against the translator Prometheus uses (`tests/promql`). The `hw.*` names are OpenTelemetry semantic conventions v1.44.0 (Development stability); the `joulie.*` names are Joulie's own.

| OTel name | Unit | Instrument | Point attributes | Prometheus name (default strategy) | Read as |
|---|---|---|---|---|---|
| `hw.energy` | J | Counter | `hw.id`, `hw.type`, `hw.name`, `hw.parent` | `hw_energy_joules_total` | node power, scope `components`: rate of the `cpu` and `gpu` components |
| `hw.power` | W | Gauge | the same | `hw_power_watts` | node power, scope `components`: a component without an energy counter |
| `hw.host.energy` | J | Counter | `hw.id=host` | `hw_host_energy_joules_total` | node power, scope `host` |
| `hw.host.power` | W | Gauge | `hw.id=host` | `hw_host_power_watts` | node power, scope `host`, without an energy counter |
| `joulie.facility.ambient_temperature` | Cel | Gauge | | `joulie_facility_ambient_temperature_celsius` | outdoor ambient temperature (`avg`) |
| `joulie.facility.it.energy` | J | Counter | | `joulie_facility_it_energy_joules_total` | IT power, as a rate |
| `joulie.facility.it.power` | W | Gauge | | `joulie_facility_it_power_watts` | IT power without an energy counter |
| `joulie.facility.cooling.energy` | J | Counter | | `joulie_facility_cooling_energy_joules_total` | cooling power, as a rate |
| `joulie.facility.cooling.power` | W | Gauge | | `joulie_facility_cooling_power_watts` | cooling power without an energy counter |
| `joulie.cooling_zone.ambient_temperature` | Cel | Gauge | `joulie.cooling_zone.name` | `joulie_cooling_zone_ambient_temperature_celsius` | ambient temperature of each cooling zone |
| `joulie.rack.energy` | J | Counter | `joulie.rack.name` | `joulie_rack_energy_joules_total` | power of each rack, as a rate |
| `joulie.rack.power` | W | Gauge | `joulie.rack.name` | `joulie_rack_power_watts` | power of each rack without an energy counter |
| `hw.host.ambient_temperature` | Cel | Gauge | `hw.id=host` | `hw_host_ambient_temperature_celsius` | inlet temperature of each node (the hottest sensor) |

Resource attributes every producer sets:

- `k8s.node.name`: the node a resource describes. It is the node identity of every node metric, and Prometheus keeps it on `target_info` and, when promoted, on every series.
- `service.instance.id`: unique per producer and node. Prometheus derives the `instance` label from it, so two producers that share one are stored as one target.
- `service.name`: the `job` label.

Producer rules, which the simulator follows and tests:

- One `cpu` component per CPU package (`hw.id` `cpu<socket>`) and one `gpu` component per GPU (`gpu<index>`). RAPL core, uncore and psys zones are never `cpu`; DRAM is `memory` (`hw.parent=cpu<socket>`) and is not part of node power.
- Whole-host energy only as `hw.host.energy` and `hw.host.power`, never as `hw.energy`; `hw.host.*` carries no `hw.type`.
- Counters never decrease, also across a producer restart (the simulator keeps them in a ledger). A counter that does go back is read as a reset: `rate()` counts it as a restart from zero and the HTTP reader drops that interval, so neither reads negative power, but `rate()` reads a spike.

The cooling zone and rack names are the values of the nodes' `joulie.io/cooling-zone` and `joulie.io/rack` labels; see [Node topology]({{< relref "/docs/getting-started/05-configuration-reference.md#node-topology" >}}).

### The queries

With the default window `[5m]` and the default naming, the `otel` presets run:

```promql
# node power, scope components, labels form
sum by (k8s_node_name) (max by (k8s_node_name, hw_type, hw_id) (rate(hw_energy_joules_total{hw_type=~"cpu|gpu"}[5m])) or on (k8s_node_name, hw_type, hw_id) max by (k8s_node_name, hw_type, hw_id) (hw_power_watts{hw_type=~"cpu|gpu"}))

# node power, target_info form: every inner selector X becomes
X * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) X)

# node power, scope host
max by (k8s_node_name) (rate(hw_host_energy_joules_total[5m])) or on (k8s_node_name) max by (k8s_node_name) (hw_host_power_watts)

# facility
avg(joulie_facility_ambient_temperature_celsius)
sum(rate(joulie_facility_it_energy_joules_total[5m])) or sum(joulie_facility_it_power_watts)
sum(rate(joulie_facility_cooling_energy_joules_total[5m])) or sum(joulie_facility_cooling_power_watts)
avg by (joulie_cooling_zone_name) (joulie_cooling_zone_ambient_temperature_celsius)
sum by (joulie_rack_name) (rate(joulie_rack_energy_joules_total[5m])) or on (joulie_rack_name) sum by (joulie_rack_name) (joulie_rack_power_watts)
max by (k8s_node_name) (hw_host_ambient_temperature_celsius{k8s_node_name!=""} or hw_host_ambient_temperature_celsius{k8s_node_name=""} * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) hw_host_ambient_temperature_celsius{k8s_node_name=""}))
```

- **Energy first.** The rate of the energy counter is read, and the power gauge only for a component that has no counter.
- **`max by (node, hw_type, hw_id)`** counts a component reported by two producers once. `hw_type` is part of the key: without it a `cpu` and a `gpu` component that share an `hw.id` collapse into one, and a node of 350 W reads 100 W.
- **`PROMETHEUS_RESOURCE_ATTRIBUTES`** picks the form: `labels`, `target_info`, or `auto`, which runs the labels form and falls back to the `target_info` form when the labels form returns nothing or a series lacks `k8s_node_name`. The node inlet query reads both forms at once.

The compatibility presets read exporters that do not speak OTLP:

```promql
# NODE_POWER_PRESET=kepler (Kepler 0.10 and later), scope components
sum by (node_name) (label_replace(rate(kepler_node_cpu_joules_total{zone="package"}[5m]), "joulie_component", "cpu", "", "") or label_replace(rate(kepler_node_gpu_joules_total[5m]), "joulie_component", "gpu", "", ""))
# NODE_POWER_PRESET=kepler-legacy (Kepler 0.7 to 0.9), scope components and scope host
sum by (exported_instance) (label_replace(rate(kepler_node_package_joules_total[5m]), "joulie_component", "cpu", "", "") or label_replace(rate(kepler_node_gpu_joules_total[5m]), "joulie_component", "gpu", "", ""))
sum by (exported_instance) (rate(kepler_node_platform_joules_total[5m]))
```

The `label_replace` gives the CPU and GPU series different label sets: `a or b` drops every series of `b` whose labels equal one in `a`, which on a GPU node lost the GPU (100 W read instead of 300 W). A custom query that combines series with `or` needs the same guard.

### Name translation strategies

Prometheus' OTLP receiver spells each name under its `otlp.translation_strategy`, and `PROMETHEUS_NAME_TRANSLATION` must name the same strategy:

| OTel name | `UnderscoreEscapingWithSuffixes` (default) | `NoUTF8EscapingWithSuffixes` | `UnderscoreEscapingWithoutSuffixes` | `NoTranslation` |
|---|---|---|---|---|
| `hw.energy` | `hw_energy_joules_total` | `hw.energy_joules_total` | `hw_energy` | `hw.energy` |
| `hw.power` | `hw_power_watts` | `hw.power_watts` | `hw_power` | `hw.power` |
| `hw.host.energy` | `hw_host_energy_joules_total` | `hw.host.energy_joules_total` | `hw_host_energy` | `hw.host.energy` |
| `hw.host.power` | `hw_host_power_watts` | `hw.host.power_watts` | `hw_host_power` | `hw.host.power` |
| label `k8s.node.name` | `k8s_node_name` | `k8s.node.name` | `k8s_node_name` | `k8s.node.name` |
| label `hw.type` | `hw_type` | `hw.type` | `hw_type` | `hw.type` |
| label `hw.id` | `hw_id` | `hw.id` | `hw_id` | `hw.id` |
| label `joulie.cooling_zone.name` | `joulie_cooling_zone_name` | `joulie.cooling_zone.name` | `joulie_cooling_zone_name` | `joulie.cooling_zone.name` |
| label `joulie.rack.name` | `joulie_rack_name` | `joulie.rack.name` | `joulie_rack_name` | `joulie.rack.name` |

Under the UTF-8 strategies the queries quote names: `{"hw.energy_joules_total", "hw.type"=~"cpu|gpu"}`. A unit that is not exactly `J`, `W`, `Cel`, `Hz` or `1` changes the suffix: a producer that writes unit `joule` gets `hw_energy_joule_total`, which the presets do not read. The full table is `pkg/otel/promname/testdata/prometheus-names.tsv`. `NoTranslation` is experimental in Prometheus; it works, but no default uses it.

### Prometheus settings

Plain Prometheus, started with `--web.enable-otlp-receiver`, which serves `POST /api/v1/otlp/v1/metrics`:

```yaml
# prometheus.yml
storage:
  tsdb:
    out_of_order_time_window: 30m   # OTLP samples carry the producer's timestamps
otlp:
  promote_resource_attributes: [k8s.node.name, host.name]
  translation_strategy: UnderscoreEscapingWithSuffixes
```

With the Prometheus Operator, the same through kube-prometheus-stack values (`values/prometheus-grafana.yaml` sets them; the fields exist in kube-prometheus-stack 91.9.0, prometheus-operator v0.94.1):

```yaml
prometheus:
  prometheusSpec:
    enableOTLPReceiver: true
    otlp:
      promoteResourceAttributes: [k8s.node.name, host.name]
      translationStrategy: UnderscoreEscapingWithSuffixes
    tsdb:
      outOfOrderTimeWindow: 30m
```

Install it into the `monitoring` namespace (`helm install telemetry prometheus-community/kube-prometheus-stack -n monitoring --create-namespace -f values/prometheus-grafana.yaml`): the chart's `PROMETHEUS_ADDRESS`, `http://prometheus-operated.monitoring:9090`, resolves only there.

| Prometheus | What these inputs need from it |
|---|---|
| 3.0 | the `--web.enable-otlp-receiver` flag, UTF-8 names (`NoUTF8EscapingWithSuffixes`) |
| 3.4 | `NoTranslation` |
| 3.6 | `UnderscoreEscapingWithoutSuffixes`; `target_info` over the sample span |
| 3.10 | the fix for OTLP sums losing data (prometheus/prometheus#17954): the recommended minimum |
| 3.15 | the version `tests/promql` evaluates the queries in |

### Pitfalls

- **A series younger than its window reads low.** `rate()` extrapolates only a little before the first sample of a series: with 240 s of a 300 s window, a 350 W component read 298.6 W, and at `[5m]` a 400 W one read 180 W after 2 minutes. After a producer starts, wait one `PROMETHEUS_RATE_WINDOW` before trusting the reading.
- **A pushed counter reads low when its points arrive late.** Past the newest sample, `rate()` extrapolates by at most half the average interval between samples once that sample is older than 1.1 intervals. With pushed OTLP counters this happens whenever the lag (how old a point's `timeUnixNano` is when Prometheus stores it) exceeds a tenth of the export interval, and `rate()` then reads low by up to (lag + interval/2) / window. Keep the export interval well above the lag, or the rate window long: with a 60 s export (the OpenTelemetry default) and the default `5m` window, a lag below 6 s has no effect. An OpenTelemetry SDK's periodic reader stamps the time it collects, so its lag is the transport alone; the simulator does the same. A producer that stamps the time of an older reading adds that reading's age to the lag: with a 5 s export and a `30s` window, readings 0.6 to 0.9 s old made the facility IT power read 0.885 to 0.897 of the truth.
- **Nothing is promoted by default.** Prometheus copies no resource attribute onto the series unless `otlp.promote_resource_attributes` lists it, and the promotion list of the upstream OTLP guide leaves out `k8s.node.name` and `host.name`. Without promotion, `auto` reads through `target_info`.
- **`target_info` churn breaks a plain join.** When a producer restarts with new resource attributes, two `target_info` series share `job` and `instance` for a while, and `X * on (job, instance) group_left target_info` fails with HTTP 422 (many-to-many). Every join therefore goes through `max by (job, instance, k8s_node_name) (target_info)`. Two producers that share a `service.instance.id` are one target and still fail: give each its own.
- **The strategy must match.** A `PROMETHEUS_NAME_TRANSLATION` that differs from the receiver's `otlp.translation_strategy` selects no series.
- **Kepler GPUs.** Kepler reports node GPU energy only from 0.12, and only with its experimental GPU feature on, which is off by default. Without it the `kepler` preset reads the CPU packages alone, and a GPU node looks to have more headroom than it has.
- **Counters are not watts.** A custom query over a `*_joules_total` counter needs `rate()`.

### Diagnostics

- At start the controller manager logs one line per input with the source, the address, the preset, the scope, the naming, the resource attribute mode, the window and the query: `input node_power: ...` and `input facility <input>: ...`.
- When a query reads nothing, or not every node, it logs a hint once per kind: promote `k8s.node.name` (or use `auto`), enable `--web.enable-otlp-receiver` and match `otlp.translation_strategy`, or give every producer its own `service.instance.id`.
- `joulie_twin_input_queries_total{input,result}` counts every query by input and result (`success`, `partial`, `no_data`, `error`), and `joulie_twin_facility_*` holds the facility readings; see [Metrics]({{< relref "/docs/architecture/metrics.md#twin-input-metrics" >}}).
- `NodeTwin.status.powerMeasurement.source` is `prometheus` for a node read, `prometheus-no-data` otherwise.

## Current host contracts

Agent host mode uses Linux interfaces:

- RAPL energy/power cap files
- cpufreq files for observed and enforced frequency bounds

GPU host control backends:

- NVIDIA path: NVIDIA tooling/NVML-compatible power-limit controls
- AMD path: ROCm SMI power-limit controls where supported

If a GPU backend is unavailable on a node, result is `blocked` (not silent success).

Current deployment convention mounts host `/sys` into container `/host-sys`.

## CRD integration

Current runtime responsibilities:

- agent publishes `NodeHardware` for discovered hardware/capability state,
- controller manager writes `NodeTwin.spec` targets,
- agent reads `NodeTwin.spec` for desired state,
- agent selects telemetry/control backends via environment variables,
- agent writes control status under `NodeTwin.status.controlStatus`.

Today, the stable documented status contract is `NodeTwin.status.controlStatus`.
If additional diagnostic snapshots are present in some environments, treat them as auxiliary rather than core API contract.

## Next step

To see simulator-side algorithm details using these interfaces, read:

- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
