---
title: "Metrics Reference"
weight: 60
---

Joulie exposes Prometheus metrics from multiple components.

This page covers **operator + agent** metrics.
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
  - `profile`: `performance|draining-performance|eco`
  - active profile is `1`, others `0`

### Transition accounting

- `joulie_operator_state_transitions_total{node,from_state,to_state,result}` (counter)
  - transition events emitted by operator
  - `result`:
    - `applied`: transition committed
    - `deferred`: transition blocked/deferred by safeguards

## Notes

- Metrics are pull-based; values depend on scrape interval.
- Highest cardinality is usually per-CPU frequency series.
