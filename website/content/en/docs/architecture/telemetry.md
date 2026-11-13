---
title: "Input Telemetry and Actuation Interfaces"
weight: 50
---

How Joulie reads power and facility telemetry, and how the agent sends its control intents. Use it to wire Prometheus, an OpenTelemetry producer or the simulator to Joulie. Exported `/metrics` are on the [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}}); the CRD fields are on [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}).

## Backends

The same control logic runs on real hardware and on simulator or KWOK clusters; environment variables on the agent choose the wiring, never the hardware identity (that is `NodeHardware`).

| Direction | Variable | `host` (default) | `http` | `none` |
|---|---|---|---|---|
| CPU telemetry | `TELEMETRY_CPU_SOURCE` | the node's RAPL files | `TELEMETRY_CPU_HTTP_ENDPOINT` ([HTTP input format](#http-input-format)) | no reading |
| CPU control | `TELEMETRY_CPU_CONTROL` | RAPL and cpufreq files | `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT` | no writes |
| GPU control | `TELEMETRY_GPU_CONTROL` | `nvidia-smi`, amdgpu or i915/xe sysfs | `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | no writes |

Cluster-level inputs come from Prometheus and are read by the controller manager, not the agent: see [Input metrics (OpenTelemetry)](#input-metrics-opentelemetry). In host mode the chart mounts the host `/sys` at `/host-sys`.

## Control intents

| Action | Payload | Sent when |
|---|---|---|
| `rapl.set_power_cap_watts` | `capWatts`, the node's CPU total (the simulator splits it over the sockets) | a CPU cap in watts, or a percentage the agent can resolve ([CPU enforcement algorithm]({{< relref "/docs/architecture/agent.md#cpu-enforcement-algorithm" >}})) |
| `dvfs.set_throttle_pct` | `throttlePct`, 0 to 100 | no range to resolve a percentage, `TELEMETRY_CPU_CONTROL_MODE=dvfs`, or a refused watt cap; `0` once after an accepted watt cap |
| `gpu.set_power_cap_watts` | `capWattsPerGpu`, the same for every GPU | a GPU cap |

Each answer and each host write ends as `applied`, `blocked`, `error` or `none` (no intent) in `NodeTwin.status.controlStatus`.

## Current HTTP contracts

| Endpoint | Serves |
|---|---|
| `GET /otlp/telemetry/{node}` | one OTLP JSON document; point `TELEMETRY_CPU_HTTP_ENDPOINT` and `NODE_POWER_HTTP_ENDPOINT` here |
| `GET /telemetry/{node}` | the same document (`?format=legacy`: the deprecated JSON, removed in v0.4.0) |
| `POST /control/{node}` | a control intent; answers `result`, `message`, `state` |

```json
{"node": "worker-01", "action": "dvfs.set_throttle_pct", "throttlePct": 20, "ts": "2026-02-27T00:00:00Z"}
```

### HTTP input format

The agent (`TELEMETRY_CPU_SOURCE=http`) and the controller manager (`NODE_POWER_SOURCE=http`) read one OTLP JSON `ExportMetricsServiceRequest` per GET, `{node}` replaced by the node name. Each component's power is the rate of its `hw.energy` counter, or its `hw.power` gauge while the counter warms up; the agent sums the `cpu` components, the controller manager `cpu` and `gpu` (or `hw.host.*` with `NODE_POWER_SCOPE=host`). A body without `resourceMetrics` is read as the legacy JSON, with a warning. The full rules and a minimal document are on the wiki page [Telemetry inputs](https://github.com/joulie-k8s/Joulie/wiki/Telemetry-Inputs#http-input-format).

## Input metrics (OpenTelemetry)

The controller manager reads node power (`NODE_POWER_SOURCE=prometheus`, the chart default) and facility readings (`ENABLE_FACILITY_METRICS=true`) from Prometheus with PromQL instant queries. Producers push them as OpenTelemetry metrics to Prometheus' OTLP receiver: the simulator in emulate mode, an OpenTelemetry Collector, or any exporter that follows the table below. Only the controller manager queries Prometheus.

### The contract

Names come from `pkg/otel/semconv`; the `hw.*` names are OpenTelemetry semantic conventions v1.44.0 (Development), the `joulie.*` names Joulie's own.

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

Every producer sets the resource attributes `k8s.node.name` (the node), `service.instance.id` (unique per producer and node) and `service.name`. It reports one `cpu` component per CPU package and one `gpu` per GPU, whole-host energy only as `hw.host.*`, and counters that never decrease. Zone and rack names are the nodes' `joulie.io/cooling-zone` and `joulie.io/rack` labels ([Node topology]({{< relref "/docs/getting-started/05-configuration-reference.md#node-topology" >}})).

### The queries

Each input is one grouped query per reconcile (node power) or per poll (facility), keyed by node, zone or rack; a node missing from the answer reads `source: prometheus-no-data`. The `otel` preset reads the energy rate first and the power gauge only without a counter; `kepler` and `kepler-legacy` read Kepler's node counters. `PROMETHEUS_RESOURCE_ATTRIBUTES` (`auto` by default) chooses whether the node label is read from every series or joined from `target_info`. At start the controller manager logs every query it runs. The full PromQL of each preset, and why it is written that way, is on the wiki page [Telemetry inputs](https://github.com/joulie-k8s/Joulie/wiki/Telemetry-Inputs#the-queries).

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

A producer unit other than `J`, `W`, `Cel`, `Hz` or `1` changes the suffix and the presets miss the series. The full table is `pkg/otel/promname/testdata/prometheus-names.tsv`.

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

| Symptom | Cause | Fix |
|---|---|---|
| power reads low just after a producer starts | `rate()` over a series younger than its window | wait one `PROMETHEUS_RATE_WINDOW` |
| pushed counters read a few percent low | points arrive late relative to the export interval | keep the export interval well above the lag (60 s export, `5m` window) |
| `PROMETHEUS_RESOURCE_ATTRIBUTES=labels` reads no node | `k8s.node.name` not promoted | add it to `otlp.promote_resource_attributes`, or use `auto` |
| queries fail with HTTP 422 (many-to-many) | two producers share a `service.instance.id` | give each producer its own |
| no series at all | `PROMETHEUS_NAME_TRANSLATION` differs from `otlp.translation_strategy` | set them to the same strategy |
| GPU nodes show too much headroom with Kepler | Kepler reports GPU energy only from 0.12, behind an experimental flag | turn the flag on, or use the `otel` preset |
| power climbs forever with a custom query | a `*_joules_total` counter read without `rate()` | wrap it in `rate()` |

The numbers behind each row are on the wiki page [Telemetry inputs](https://github.com/joulie-k8s/Joulie/wiki/Telemetry-Inputs#pitfalls-in-detail).

### Grid carbon intensity

Joulie ships no carbon intensity provider: point `FACILITY_CARBON_INTENSITY_QUERY` at one you already export, with `FACILITY_CARBON_INTENSITY_UNIT` and `FACILITY_CARBON_INTENSITY_SIGNAL` (`marginal` or `average`) ([Grid carbon intensity]({{< relref "/docs/getting-started/05-configuration-reference.md#grid-carbon-intensity" >}})). The reading is converted to gCO2eq/kWh, exported as `joulie_grid_carbon_intensity_grams_per_kW_h`, and handed to the policy, which does not act on it yet.

### Diagnostics

The startup log has one `input ...` line per query. `joulie_twin_input_queries_total{input,result}` counts each query's outcome ([Twin input metrics]({{< relref "/docs/architecture/metrics.md#twin-input-metrics" >}})), and `NodeTwin.status.powerMeasurement.source` says whether a node was read.

## Next step

- [KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}})
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})

The PromQL of every preset, the HTTP format rules, the legacy payload and the producer rules in full: [Telemetry inputs](https://github.com/joulie-k8s/Joulie/wiki/Telemetry-Inputs) on the wiki.
