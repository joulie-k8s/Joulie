---
title: "Metrics Reference"
weight: 60
---

Joulie exposes Prometheus metrics from multiple components.

This page covers **controller manager + agent + scheduler extender** metrics.
Simulator metrics are documented separately in:

- [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

For telemetry/control input interfaces (host/http routing), see:

- [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

## Endpoints by component

- Agent:
  - path: `/metrics`
  - default address: `:8080`
  - env override: `METRICS_ADDR`
- Controller manager:
  - path: `/metrics`
  - default address: `:8081`
  - env override: `METRICS_ADDR`
- Scheduler extender:
  - path: `/metrics`
  - default address: `:9877`
  - env override: `METRICS_ADDR`

## Agent metrics

### Backend and selected cap

- `joulie_backend_mode{node,mode}` (gauge)
  - `mode`: `none|rapl|dvfs`
  - active mode is `1`, others `0`
- `joulie_policy_cap_watts{node,policy}` (gauge)
  - current selected policy cap in watts

### RAPL power/energy

- `joulie_rapl_energy_uj{node,zone}` (gauge)
  - latest raw RAPL energy counter in microjoules
- `joulie_rapl_estimated_power_watts{node,zone}` (gauge)
  - per-zone estimated power from energy deltas
- `joulie_rapl_package_total_power_watts{node}` (gauge)
  - sum of package-level estimated power

### DVFS controller

- `joulie_dvfs_observed_power_watts{node}` (gauge)
  - observed package power used by DVFS controller
- `joulie_dvfs_ema_power_watts{node}` (gauge)
  - EMA-smoothed power used for decisions
- `joulie_dvfs_throttle_pct{node}` (gauge)
  - current throttle percentage
- `joulie_dvfs_above_trip_count{node}` (gauge)
  - consecutive above-threshold samples
- `joulie_dvfs_below_trip_count{node}` (gauge)
  - consecutive below-threshold samples
- `joulie_dvfs_actions_total{node,action}` (counter)
  - `action`: `throttle_up|throttle_down`

### CPU frequency observability

- `joulie_dvfs_cpu_cur_freq_khz{node,cpu}` (gauge)
  - current CPU/policy frequency in kHz
- `joulie_dvfs_cpu_max_freq_khz{node,cpu}` (gauge)
  - enforced max frequency cap in kHz

### Governors

Written by the agent's [governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}).

- `joulie_agent_governor_writes_total{node,component,preset,result}` (counter)
  - one per governor write: a preset applied, or the baseline restored (`preset="baseline"`)
  - `component`: `cpu|gpu`
  - `result`: `applied|applied-unverified|equivalent|unsupported|not-applied|refused|restored|error`
  - a reconcile that only checks the setting, and a preset the node cannot express or the agent may not write (`disabled`), count nothing
- `joulie_agent_governor_state{node,component,preset}` (gauge)
  - the preset in effect is `1` (result `applied`, `applied-unverified` or `equivalent`), every other preset of the component `0`; all `0` before a write and after a restore

### Reliability

- `joulie_reconcile_errors_total{node}` (counter)
  - reconcile-loop errors

## Policy controller metrics

Exported by the controller manager. Until the next minor release each metric is also exported under its deprecated `joulie_operator_*` alias (same labels, same values, help text `Deprecated, use joulie_policy_...`); update dashboards and alerts to the `joulie_policy_*` names before that release.

### FSM state and profile label

- `joulie_policy_node_state{node,state}` (gauge)
  - `state`: `ActivePerformance|DrainingPerformance|ActiveEco`
  - active state is `1`, others `0`
- `joulie_policy_node_profile_label{node,profile}` (gauge)
  - controller-manager-applied node label view
  - `profile`: `performance|eco`
  - active profile is `1`, others `0`

### Transition accounting

- `joulie_policy_state_transitions_total{node,from_state,to_state,result}` (counter)
  - transition events emitted by the controller manager
  - `result`:
    - `applied`: transition committed
    - `deferred`: transition blocked/deferred by safeguards

### Heterogeneous planning

These two gauges have no `joulie_operator_*` alias. Both are reset every reconcile and computed from the plan after the downgrade guard, so a draining node counts as not performance. `family` is `gpu:<model>` when the node has GPUs, otherwise `cpu:<model>` (see [Heterogeneous planning]({{< relref "/docs/architecture/controller-manager.md#heterogeneous-planning" >}})).

- `joulie_policy_family_nodes{family}` (gauge)
  - eligible nodes in each hardware family
- `joulie_policy_family_performance_nodes{family}` (gauge)
  - nodes of each hardware family planned in the `performance` profile

## Twin input metrics

Exported by the controller manager. They show whether the measured inputs (see [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}})) are read.

- `joulie_twin_input_queries_total{input,result}` (counter)
  - one increment per Prometheus query of a measured input
  - `input`: `node_power`, or the facility input: `ambient_temp`, `it_power`, `cooling_power`, `zone_ambient_temp`, `rack_power`, `node_inlet_temp`
  - `result`:
    - `success`: every series was read
    - `partial`: some series were skipped, for a missing key label or a value that is not finite
    - `no_data`: no usable series
    - `error`: Prometheus failed the query or could not be reached, or the answer was refused (duplicate keys, several series for a single value)
- `joulie_twin_facility_ambient_temperature_celsius{preset}` (gauge)
  - outdoor ambient temperature the facility input last read, in degrees Celsius
- `joulie_twin_facility_it_power_watts{preset}` (gauge)
  - IT power the facility input last read, in watts
- `joulie_twin_facility_cooling_power_watts{preset}` (gauge)
  - cooling power the facility input last read, in watts

`preset` is `FACILITY_PRESET` (`otel`, `legacy` or `custom`). A facility gauge is absent while its input is unknown, so a stale reading never outlives the poll that lost it. PUE is `(it_power + cooling_power) / it_power`; the overview dashboard's "Twin inputs" row plots it.

## Scheduler extender metrics

### Request counters

- `joulie_scheduler_filter_requests_total{workload_class}` (counter)
  - total filter requests by workload class
  - `workload_class`: `standard|performance`
- `joulie_scheduler_prioritize_requests_total{workload_class}` (counter)
  - total prioritize (scoring) requests by workload class

### Request latency

- `joulie_scheduler_filter_duration_seconds{workload_class}` (histogram)
  - time to process a filter request
- `joulie_scheduler_prioritize_duration_seconds{workload_class}` (histogram)
  - time to process a prioritize request

### Scoring signals

- `joulie_scheduler_final_node_score{node,workload_class}` (gauge)
  - final scheduling score (0-100) for each node and workload class
  - updated on every prioritize call; reflects the combined headroom + cooling + trend + bonus formula
- `joulie_scheduler_node_headroom_score{node}` (gauge)
  - power headroom score per node
  - can go negative when projected power (measured + pod marginal) exceeds the capped budget

### Data freshness

- `joulie_scheduler_stale_twin_data{node}` (gauge)
  - `1` if the NodeTwin status is older than the staleness threshold (default 5m), `0` otherwise
  - a node with stale data receives a neutral score (50) instead of its computed value
  - useful for alerting when the controller manager has stopped updating twin status

## Notes

- Metrics are pull-based; values depend on scrape interval.
- Highest cardinality is usually per-CPU frequency series.
