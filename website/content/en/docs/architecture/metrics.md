---
title: "Metrics Reference"
---


Joulie agent exposes Prometheus metrics on `/metrics` (default `:8080`).

This document is only for **exported observability metrics**.
For **input telemetry and control interfaces** (real hardware vs simulated HTTP), see:

- [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

## Endpoint

- Path: `/metrics`
- Address: `METRICS_ADDR` env var (default `:8080`)

## Backend/Policy

- `joulie_backend_mode{node,mode}` (gauge)
  - `mode` values: `none`, `rapl`, `dvfs`
  - Active mode has value `1`, others `0`
- `joulie_policy_cap_watts{node,policy}` (gauge)
  - Current selected policy cap in watts

This metric can also be used to derive policy states in Grafana:

- high cap (for example `>= 1000W`) -> `ActivePerformance`
- low cap (for example `< 1000W`) -> `ActiveEco`

## RAPL Power/Energy

- `joulie_rapl_energy_uj{node,zone}` (gauge)
  - Latest raw RAPL energy counter value (microjoules)
- `joulie_rapl_estimated_power_watts{node,zone}` (gauge)
  - Per-zone estimated power in watts from energy deltas
- `joulie_rapl_package_total_power_watts{node}` (gauge)
  - Sum of package-level estimated power used by DVFS fallback input

## DVFS Controller State

- `joulie_dvfs_observed_power_watts{node}` (gauge)
  - Observed package power used by control loop
- `joulie_dvfs_ema_power_watts{node}` (gauge)
  - EMA-smoothed power value used for decisions
- `joulie_dvfs_throttle_pct{node}` (gauge)
  - Current throttling percentage
- `joulie_dvfs_above_trip_count{node}` (gauge)
  - Consecutive above-threshold samples
- `joulie_dvfs_below_trip_count{node}` (gauge)
  - Consecutive below-threshold samples
- `joulie_dvfs_actions_total{node,action}` (counter)
  - `action` values: `throttle_up`, `throttle_down`

## Frequency Metrics

- `joulie_dvfs_cpu_cur_freq_khz{node,cpu}` (gauge)
  - Current CPU/policy frequency in kHz
- `joulie_dvfs_cpu_max_freq_khz{node,cpu}` (gauge)
  - Current enforced max frequency cap in kHz

`cpu` label corresponds to CPU index or cpufreq policy index, depending on host layout.

## Reliability/Errors

- `joulie_reconcile_errors_total{node}` (counter)
  - Total reconcile-loop errors

## Operator FSM Metrics

- `joulie_operator_node_state{node,state}` (gauge)
  - `state` values: `ActivePerformance`, `DrainingPerformance`, `ActiveEco`
  - active state has value `1`, others `0`
- `joulie_operator_node_profile_label{node,profile}` (gauge)
  - operator view of node profile label (`profile` values: `performance`, `draining-performance`, `eco`)
  - active profile has value `1`, other `0`
- `joulie_operator_state_transitions_total{node,from_state,to_state,result}` (counter)
  - transition events emitted by operator
  - `result` values:
    - `applied`: transition committed
    - `deferred`: transition blocked/deferred by policy guard (for example performance-intent workloads still running)

## Notes

- Metrics are pull-based; collection frequency depends on Prometheus scrape interval.
- High-cardinality metrics are the per-cpu frequency series. If needed, reduce scrape frequency or relabel/drop these series.
