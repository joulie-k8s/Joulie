---
title: "Configuration Reference"
weight: 50
---

Complete reference for all Joulie environment variables. These are set via Helm values or directly in the Deployment/DaemonSet manifests.

## Agent

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_MODE` | `daemonset` | `daemonset` (one agent per node) or `pool` (shared agents with sharding) |
| `NODE_NAME` | (required in daemonset mode) | Name of the node this agent manages |
| `RECONCILE_INTERVAL` | `20s` | How often the agent reconciles desired state |
| `METRICS_ADDR` | `:8080` | Address for the Prometheus metrics endpoint |
| `SIMULATE_ONLY` | `false` | If `true`, agent discovers hardware but does not apply power caps |
| `HARDWARE_CATALOG_PATH` | `simulator/catalog/hardware.yaml` | Path to the hardware inventory catalog YAML |

### Agent pool mode

| Variable | Default | Description |
|----------|---------|-------------|
| `POOL_NODE_SELECTOR` | `node-role.kubernetes.io/worker` | Label selector for nodes managed by pool agents |
| `POOL_SHARDS` | `1` | Total number of shards for pool mode partitioning |
| `POOL_SHARD_ID` | (from pod ordinal) | Shard ID for this agent instance |

### Agent DVFS control

| Variable | Default | Description |
|----------|---------|-------------|
| `DVFS_EMA_ALPHA` | `0.3` | Exponential moving average smoothing factor for power tracking |
| `DVFS_UPPER_MARGIN_PCT` | `5` | Power above cap threshold to trigger frequency reduction (%) |
| `DVFS_LOWER_MARGIN_PCT` | `10` | Power below cap threshold to trigger frequency increase (%) |
| `DVFS_STEP_PCT` | `5` | Frequency throttle step size (%) |
| `DVFS_COOLDOWN_S` | `5` | Minimum seconds between DVFS adjustments |
| `DVFS_TRIP_ABOVE_THRESHOLD` | `3` | Consecutive above-threshold samples before throttling |
| `DVFS_TRIP_BELOW_THRESHOLD` | `3` | Consecutive below-threshold samples before unthrottling |

## Operator

| Variable | Default | Description |
|----------|---------|-------------|
| `RECONCILE_INTERVAL` | `1m` | How often the operator reconciles cluster state |
| `METRICS_ADDR` | `:8081` | Address for the Prometheus metrics endpoint |
| `NODE_SELECTOR` | `node-role.kubernetes.io/worker` | Label selector for managed nodes |
| `RESERVED_LABEL_KEY` | `joulie.io/reserved` | Label key for nodes excluded from policy decisions |
| `POWER_PROFILE_LABEL` | `joulie.io/power-profile` | Node label key for the active power profile |

### Power cap configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PERFORMANCE_CAP_WATTS` | `5000` | Absolute CPU power cap for performance nodes (watts) |
| `ECO_CAP_WATTS` | `120` | Absolute CPU power cap for eco nodes (watts) |
| `CPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | CPU cap as percentage of max for performance nodes |
| `CPU_ECO_CAP_PCT_OF_MAX` | `60` | CPU cap as percentage of max for eco nodes |
| `CPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute watts instead of percentage |
| `GPU_PERFORMANCE_CAP_PCT_OF_MAX` | `100` | GPU cap as percentage of max for performance nodes |
| `GPU_ECO_CAP_PCT_OF_MAX` | `60` | GPU cap as percentage of max for eco nodes |
| `GPU_WRITE_ABSOLUTE_CAPS` | `false` | If `true`, write absolute GPU watts instead of percentage |
| `GPU_MODEL_CAPS_JSON` | `{}` | JSON map of GPU model name to `{"minCapWatts": N, "maxCapWatts": M}` |
| `GPU_PRODUCT_LABEL_KEYS` | `joulie.io/gpu.product,...` | Comma-separated node label keys to read GPU product name |

### Policy configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `POLICY_TYPE` | `static_partition` | Policy algorithm: `static_partition`, `queue_aware_v1`, or `rule_swap_v1` |
| `STATIC_HP_FRAC` | `0.50` | Fraction of nodes allocated to performance in `static_partition` |
| `QUEUE_HP_BASE_FRAC` | `0.60` | Base fraction of performance nodes in `queue_aware_v1` |
| `QUEUE_HP_MIN` | `1` | Minimum performance nodes in `queue_aware_v1` |
| `QUEUE_HP_MAX` | `1000000` | Maximum performance nodes in `queue_aware_v1` |
| `QUEUE_PERF_PER_HP_NODE` | `10` | Performance pods per performance node ratio in `queue_aware_v1` |

### Workload classifier

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_CLASSIFIER` | `true` | Enable the workload classifier loop |
| `CLASSIFY_INTERVAL` | `30s` | How often the classifier scans running pods |
| `RECLASSIFY_INTERVAL` | `15m` | Minimum time before re-classifying an already-classified pod |
| `CLASSIFY_METRICS_WINDOW` | `10m` | Prometheus query lookback window for dynamic metrics |
| `PROMETHEUS_ADDRESS` | `http://prometheus-operated.monitoring:9090` | Prometheus endpoint for classifier queries |
| `KEPLER_AVAILABLE` | `true` | Whether Kepler energy metrics are available in Prometheus |
| `CLASSIFY_MIN_CONFIDENCE` | `0.5` | Minimum confidence score for a profile to influence scheduling |
| `CLASSIFY_SIM_ANNOTATION_FALLBACK` | `false` | When `true`, classifier reads `sim.joulie.io/*` pod annotations as fallback when Prometheus metrics are unavailable (simulator mode) |
| `CLASSIFY_SIM_NOISE_PCT` | `10` | Gaussian noise percentage added to sim-annotation utilization values before classification (simulates measurement error) |

### Active rescheduler

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_ACTIVE_RESCHEDULING` | `false` | Enable active pod eviction for misplaced workloads |
| `RESCHEDULE_INTERVAL` | `60s` | How often the rescheduler evaluates recommendations |
| `RESCHEDULE_MAX_EVICTIONS_PER_NODE` | `1` | Maximum pod evictions per node per rescheduler cycle |
| `RESCHEDULE_DRY_RUN` | `false` | If `true`, log eviction decisions without executing them |

### Facility metrics

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_FACILITY_METRICS` | `false` | Enable polling data-center-level metrics from Prometheus |
| `FACILITY_PROMETHEUS_ADDRESS` | `http://prometheus-operated.monitoring:9090` | Prometheus endpoint for facility metric queries |
| `FACILITY_POLL_INTERVAL` | `30s` | How often facility metrics are polled |
| `FACILITY_AMBIENT_TEMP_METRIC` | `datacenter_ambient_temperature_celsius` | PromQL metric name for ambient temperature |
| `FACILITY_IT_POWER_METRIC` | `datacenter_total_it_power_watts` | PromQL metric name for total IT power draw |
| `FACILITY_COOLING_POWER_METRIC` | `datacenter_cooling_power_watts` | PromQL metric name for cooling infrastructure power |
| `FACILITY_ZONE_AMBIENT_METRIC_TEMPLATE` | (empty) | PromQL template for per-zone ambient temperature, e.g. `datacenter_ambient_temperature_celsius{zone="%s"}`. Use `%s` as the zone name placeholder. Empty = disabled. |
| `FACILITY_RACK_POWER_METRIC_TEMPLATE` | (empty) | PromQL template for per-rack power draw, e.g. `datacenter_rack_power_watts{rack="%s"}`. Use `%s` as the rack name placeholder. Empty = disabled. |

### Node topology

Joulie supports optional per-rack PSU stress and per-zone cooling stress. This is activated by adding standard node labels:

- `joulie.io/rack`: physical rack identifier (e.g., `rack-1`)
- `joulie.io/cooling-zone`: cooling zone identifier (e.g., `zone-a`)

When these labels are present, the operator computes PSU stress per-rack (sum of estimated node power within the rack) instead of cluster-wide, and uses per-zone ambient temperature from facility metrics instead of the global value. The twin model interfaces remain the same; topology just groups nodes for more accurate stress computation.

Nodes without topology labels fall back to cluster-wide stress computation.

## Scheduler extender

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `9876` | HTTP port for the scheduler extender |
| `CACHE_TTL` | `30s` | TTL for the NodeTwin status cache |
| `EVICTION_HISTORY_TTL` | `30m` | How long eviction context influences scheduling decisions |

## kubectl plugin

The `kubectl joulie` plugin requires no configuration. It reads your current kubeconfig context.

```bash
# Install
go build -o kubectl-joulie ./cmd/kubectl-joulie
mv kubectl-joulie /usr/local/bin/

# Usage
kubectl joulie status      # cluster energy overview
kubectl joulie recommend   # GPU slicing and reschedule suggestions
```
