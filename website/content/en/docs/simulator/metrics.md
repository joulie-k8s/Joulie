---
title: "Simulator Metrics"
weight: 40
---

This page documents Prometheus metrics exposed by the simulator (`simulator/cmd/simulator/main.go`).

Endpoint:

- path: `/metrics`
- address: simulator HTTP listen address (`SIM_ADDR`, default `:18080`)

Related debug endpoints (non-Prometheus):

- `/debug/nodes`
- `/debug/events`
- `/debug/energy`

## HTTP/request metrics

- `joulie_sim_requests_total{route,method,status}` (counter)
  - total HTTP requests by route/method/status
- `joulie_sim_request_duration_seconds{route,method}` (histogram)
  - request latency

## Control-path metrics

- `joulie_sim_controls_total{node,action}` (counter)
  - received control actions by node/action
- `joulie_sim_control_actions_total{node,action,result}` (counter)
  - control action outcomes
  - `result`: `applied|blocked|error`

## Per-node simulated state metrics

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

## Workload/job metrics

- `joulie_sim_job_submitted_total{class}` (counter)
  - jobs submitted by class
- `joulie_sim_job_completed_total{class,node}` (counter)
  - jobs completed by class and node
- `joulie_sim_job_completion_seconds` (histogram)
  - job completion latency distribution

## Notes

- Prometheus metrics capture online simulator state and request/control behavior.
- Integrated node/cluster energy totals are exposed through `/debug/energy` (JSON), not as Prometheus time series in the current implementation.
- Richer thermal and averaged-vs-instantaneous details are currently exposed through the HTTP telemetry/debug endpoints rather than as separate Prometheus gauges.
- In particular, fields such as `instantPackagePowerWatts`, `cpu.temperatureC`, `cpu.thermalThrottlePct`, and per-device GPU averaged power live in `/telemetry/{node}` and `/debug/nodes`.
