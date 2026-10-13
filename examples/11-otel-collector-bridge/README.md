# Example: OpenTelemetry Collector bridge from Kepler, dcgm-exporter and the AMD device metrics exporter

Joulie's controller manager reads node power from Prometheus as the OpenTelemetry `hw.energy` and `hw.power` metrics (the default `otel` preset). A cluster that already runs Kepler, NVIDIA dcgm-exporter or the ROCm device metrics exporter can feed it without new exporters: [`collector.yaml`](./collector.yaml) is an OpenTelemetry Collector config that scrapes them, rewrites their series as `hw.*` and pushes them over OTLP to Prometheus. A source that is not deployed finds no pods and adds nothing.

The controller manager needs no change from the Joulie chart's defaults (`NODE_POWER_SOURCE=prometheus`, `NODE_POWER_PRESET` unset, `NODE_POWER_SCOPE=components`): each node's power is its CPU packages plus its GPUs. The input contract is in the [telemetry page](https://joulie-k8s.github.io/Joulie/docs/architecture/telemetry/#input-metrics-opentelemetry).

## What the config maps

| Source series | OTel metric | `hw.type` | `hw.id` | `hw.name` | Scale |
|---|---|---|---|---|---|
| Kepler v0.12 `kepler_node_cpu_joules_total{zone="package"}` | `hw.energy` (J, counter) | `cpu` | `cpu` | `package` | 1 |
| Kepler v0.12 `kepler_node_cpu_watts{zone="package"}` | `hw.power` (W, gauge) | `cpu` | `cpu` | `package` | 1 |
| dcgm-exporter 4.8.4 `DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION` (mJ) | `hw.energy` (J, counter) | `gpu` | `gpu<gpu label>` | `modelName` | 0.001 |
| dcgm-exporter 4.8.4 `DCGM_FI_DEV_POWER_USAGE` | `hw.power` (W, gauge) | `gpu` | `gpu<gpu label>` | `modelName` | 1 |
| ROCm device-metrics-exporter v1.5.2 `gpu_energy_consumed` (uJ, gauge) | `hw.energy` (J, counter) | `gpu` | `gpu<gpu_id label>` | `card_model` | 0.000001 |
| ROCm device-metrics-exporter v1.5.2 `gpu_power_usage` | `hw.power` (W, gauge) | `gpu` | `gpu<gpu_id label>` | `card_model` | 1 |

- **Node identity.** Kepler's `node_name` label, or the `hostname` label of dcgm-exporter (`Hostname` up to 4.8.2) and of the AMD exporter, becomes the resource attribute `k8s.node.name`. All three upstream charts set these labels from `spec.nodeName`.
- **AMD energy is a gauge.** The AMD exporter types `gpu_energy_consumed` as a gauge although it only grows, so the config turns it into a cumulative, monotonic sum before naming it `hw.energy`. Without that, Prometheus would name it `hw_energy_joules`, without the `_total` the preset reads. The power comes from `gpu_power_usage`, not `gpu_package_power`, which MI2xx GPUs do not report.
- **One resource per node.** The `groupbyattrs` processor splits each scrape by node. `service.instance.id` is set to the node, so Prometheus' `job` (`kepler` or `dcgm-exporter`) and `instance` stay unique per node and the `target_info` join works, even when one exporter reports many nodes.
- **Point attributes.** Only `hw.type`, `hw.id` and `hw.name` are kept, as semantic conventions define for these metrics. The pod and job labels that dcgm-exporter and the AMD exporter add in Kubernetes mode are dropped, so a GPU keeps one series when its pod changes.
- **No double counting.** Kepler's GPU series (its experimental GPU feature) are dropped: the GPUs come from dcgm-exporter and the AMD exporter only.

In Prometheus, with the default translation strategy, the series are `hw_energy_joules_total` and `hw_power_watts` with labels `hw_type`, `hw_id`, `hw_name`, `job` (`kepler`, `dcgm-exporter` or `amd-device-metrics-exporter`), `instance`, and `k8s_node_name` when promoted.

## Run it in a cluster

1. **Prometheus** needs the OTLP receiver and, preferably, `k8s.node.name` promoted; see [Prometheus settings](https://joulie-k8s.github.io/Joulie/docs/architecture/telemetry/#prometheus-settings). `values/prometheus-grafana.yaml` already sets them. Without promotion, the controller manager's default `PROMETHEUS_RESOURCE_ATTRIBUTES=auto` reads the node from `target_info`.
2. **Collector image:** the contrib distribution, `otel/opentelemetry-collector-contrib`. The core image has no `transform` or `groupbyattrs` processor. Releases before v0.144.0 name the exporter `otlphttp` instead of `otlp_http`; later ones still accept `otlphttp` as a deprecated alias.
3. **Discovery:** the scrape jobs find pods by the upstream Helm charts' `app.kubernetes.io/name` label (`kepler`, `dcgm-exporter`, `metrics-exporter`) and container port (named `http` and `metrics`; the AMD chart leaves its port 5000 unnamed, so that job matches the number). Change the `keep` rules if your pods differ. The Collector's ServiceAccount needs `get`, `list` and `watch` on `pods`.
4. **One replica.** Every replica scrapes every pod, so a second replica pushes each series twice.
5. **Endpoint:** `metrics_endpoint` points at `prometheus-operated.monitoring:9090`, the chart's default Prometheus. Change it to yours.

Check that every node has its CPU and GPUs:

```bash
curl -s localhost:9090/api/v1/query --data-urlencode \
  'query=count by (k8s_node_name, hw_type) (hw_energy_joules_total)'
```

## Check it locally

This runs the config against the simulator's exposition fixtures, `simulator/pkg/compat/testdata/{kepler-0.12,dcgm-4.8.4,amd-dme-1.5.2}.prom`, with Docker. The fixtures hold a CPU node `intel-0`, a node `nvl-0` with 8 NVIDIA GPUs and a node `mi300x-0` with 8 AMD GPUs (its GPUs only, since the Kepler fixture has no such node). The override file replaces pod discovery with the fixture server. The Collector replaces lists when it merges configs, so the override repeats each job's `metric_relabel_configs`.

```bash
cd examples/11-otel-collector-bridge
tmp=$(mktemp -d)
docker network create joulie-bridge
docker run -d --name joulie-bridge-fixtures --network joulie-bridge --network-alias fixtures \
  -v "$PWD/../../simulator/pkg/compat/testdata:/srv:ro" busybox:1.37 httpd -f -p 8000 -h /srv

cat > "$tmp/prometheus.yml" <<'EOF'
storage:
  tsdb:
    out_of_order_time_window: 30m
otlp:
  promote_resource_attributes: [k8s.node.name, host.name]
  translation_strategy: UnderscoreEscapingWithSuffixes
EOF
docker run -d --name joulie-bridge-prometheus --network joulie-bridge \
  --network-alias prometheus-operated.monitoring -p 127.0.0.1:19090:9090 \
  -v "$tmp/prometheus.yml:/etc/prometheus/prometheus.yml:ro" prom/prometheus:v3.15.0 \
  --config.file=/etc/prometheus/prometheus.yml --web.enable-otlp-receiver

cat > "$tmp/fixtures.yaml" <<'EOF'
receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: kepler
          scrape_interval: 15s
          metrics_path: /kepler-0.12.prom
          fallback_scrape_protocol: PrometheusText0.0.4  # httpd sends no metrics content type
          static_configs: [{targets: ["fixtures:8000"]}]
          metric_relabel_configs:
            - source_labels: [__name__, zone]
              regex: kepler_node_cpu_(joules_total|watts);package
              action: keep
        - job_name: dcgm-exporter
          scrape_interval: 15s
          metrics_path: /dcgm-4.8.4.prom
          fallback_scrape_protocol: PrometheusText0.0.4
          static_configs: [{targets: ["fixtures:8000"]}]
          metric_relabel_configs:
            - source_labels: [__name__]
              regex: DCGM_FI_DEV_(TOTAL_ENERGY_CONSUMPTION|POWER_USAGE)
              action: keep
        - job_name: amd-device-metrics-exporter
          scrape_interval: 15s
          metrics_path: /amd-dme-1.5.2.prom
          fallback_scrape_protocol: PrometheusText0.0.4
          static_configs: [{targets: ["fixtures:8000"]}]
          metric_relabel_configs:
            - source_labels: [__name__]
              regex: gpu_(energy_consumed|power_usage)
              action: keep
EOF
chmod a+r "$tmp"/*
docker run -d --name joulie-bridge-otelcol --network joulie-bridge \
  -v "$PWD/collector.yaml:/etc/otelcol/collector.yaml:ro" -v "$tmp/fixtures.yaml:/etc/otelcol/fixtures.yaml:ro" \
  otel/opentelemetry-collector-contrib:0.162.0 \
  --config=/etc/otelcol/collector.yaml --config=/etc/otelcol/fixtures.yaml
```

After two scrapes (about 40 s), the components per node, the GPU energy in joules (the fixtures' `9.75e+06` mJ and `1.375e+10` uJ) and the power the gauges give:

```bash
q() { curl -s 127.0.0.1:19090/api/v1/query --data-urlencode "query=$1" | python3 -c \
  'import json,sys; [print(r["metric"], r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'; }
q 'count by (k8s_node_name, hw_type) (hw_energy_joules_total)'      # intel-0 cpu 1; nvl-0 cpu 1, gpu 8; mi300x-0 gpu 8
q 'hw_energy_joules_total{k8s_node_name="nvl-0", hw_id="gpu0"}'       # 9750
q 'hw_energy_joules_total{k8s_node_name="mi300x-0", hw_id="gpu0"}'    # 13750
q 'sum by (k8s_node_name) (hw_power_watts{hw_type=~"cpu|gpu"})'       # intel-0 295; nvl-0 3123; mi300x-0 5600
```

The fixtures never change, so `rate()` of their counters is 0 and the preset query reads 0 W here; a live exporter's counters advance. Clean up:

```bash
docker rm -f joulie-bridge-otelcol joulie-bridge-prometheus joulie-bridge-fixtures
docker network rm joulie-bridge
rm -r "$tmp"
```

## How it was verified

With `otel/opentelemetry-collector-contrib:0.162.0` (`sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6`), the newest release on 2026-10-09, and `prom/prometheus:v3.15.0`, against the fixtures above, with Prometheus both promoting `k8s.node.name` and not:

- The controller manager's own `otel` preset queries (`pkg/controller/inputs/testdata/nodepower_queries.golden`), labels and `target_info` forms, return package plus GPUs per node: 3123 W for `nvl-0` (295 W package, 350 W to 357 W GPUs) and 295 W for `intel-0`. That is evaluated at the first sample, where `rate()` is empty and the preset reads the gauges. Later the same queries return 0 W for both nodes, because the static counters do not advance; that also shows each energy series shadows the gauge with the same key.
- `hw_energy_joules_total` holds 10850 J for each package and 9750 J to 9960 J for the GPUs, so the mJ scaling is applied.
- `target_info` has one series per job and node, and the labels form without promotion carries no `k8s_node_name`, so `auto` falls back to `target_info`.
- The AMD source, checked on 2026-10-10 with the same images and the `amd-dme-1.5.2` fixture: every preset form, with and without promotion, reads 5600 W for `mi300x-0` (8 GPUs at 700 W) at the first sample and 0 W once each counter has two samples. `hw_energy_joules_total` holds 13750 J for each AMD GPU, so the uJ scaling and the gauge-to-counter conversion are applied, and no `hw_energy_joules` gauge or raw `gpu_*` series reaches Prometheus.

## Limits

- Kepler sums the RAPL package zone over sockets, so each node has one `cpu` component with `hw.id` `cpu`, not one per package. Its `dram` zone is dropped; DRAM is not part of node power.
- There is no `hw.host.*`, so `NODE_POWER_SCOPE=host` reads nothing through this bridge.
- NVIDIA GPUs come through dcgm-exporter and AMD GPUs through the ROCm device metrics exporter, checked against the simulator's fixtures only. Not tested: MIG; MI2xx GPUs; and AMD GPUs in a partitioned mode (CPX, DPX, QPX), where every partition is its own `gpu_id` and only partition 0 reports power and energy.
