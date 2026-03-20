---
title: "Metrics Reference"
weight: 60
---

Joulie exposes Prometheus metrics from multiple components.

This page covers **operator + agent + scheduler extender** metrics.
Simulator metrics are documented separately in:

- [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

For telemetry/control input interfaces (host/http routing), see:

- [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

## Endpoints by component

- Agent:
  - path: `/metrics`
  - default address: `:8080`
  - env override: `METRICS_ADDR`
- Operator:
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

### Reliability

- `joulie_reconcile_errors_total{node}` (counter)
  - reconcile-loop errors

## Operator metrics

### FSM state and profile label

- `joulie_operator_node_state{node,state}` (gauge)
  - `state`: `ActivePerformance|DrainingPerformance|ActiveEco`
  - active state is `1`, others `0`
- `joulie_operator_node_profile_label{node,profile}` (gauge)
  - operator-applied node label view
  - `profile`: `performance|eco`
  - active profile is `1`, others `0`

### Transition accounting

- `joulie_operator_state_transitions_total{node,from_state,to_state,result}` (counter)
  - transition events emitted by operator
  - `result`:
    - `applied`: transition committed
    - `deferred`: transition blocked/deferred by safeguards

### Heterogeneous planning

- `joulie_operator_node_compute_density{node,component}` (gauge)
  - normalized per-node density signal used for heterogeneous planning
  - `component`: `cpu|gpu`
  - higher values mean the operator considers that node relatively denser for that subsystem

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
  - useful for alerting when the operator has stopped updating twin status

## Notes

- Metrics are pull-based; values depend on scrape interval.
- Highest cardinality is usually per-CPU frequency series.
