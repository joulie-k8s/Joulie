---
title: "Upgrading"
weight: 60
---

What to check when you upgrade a Joulie installation from v0.2 to v0.3. Go through the list before running `helm upgrade`.

## Before the upgrade

- [ ] **Apply the new CRDs.** `helm upgrade` never updates CRDs, and the API server prunes every field the installed CRD does not know, such as the governor fields of `NodeTwin` and `NodeHardware`. Apply them first:

  ```bash
  kubectl apply -f charts/joulie/crds/
  ```

  Without a checkout, take them from the release chart: `helm pull oci://registry.cern.ch/joulie/joulie --version <version> --untar --untardir /tmp/joulie-chart`, then `kubectl apply -f /tmp/joulie-chart/joulie/crds/`.
- [ ] **Registry.** Charts, images and the kubectl plugin are published under `registry.cern.ch/joulie`. A values file that sets an `image.repository` under `registry.cern.ch/mbunino/joulie` must point at `registry.cern.ch/joulie`; every tag up to 0.2.0 exists there with the same digest.

## Measured node power

- [ ] **Prometheus.** The chart sets `NODE_POWER_SOURCE=prometheus`, which reads the `otel` preset from `http://prometheus-operated.monitoring:9090`. Without Prometheus and node power data, every node reads 0 W with source `prometheus-no-data`. Set up Prometheus as the [quickstart]({{< relref "/docs/getting-started/01-quickstart.md#prerequisites" >}}) says, or set `controllerManager.env.NODE_POWER_SOURCE=static` to keep 0 W.
- [ ] **Kepler.** `KEPLER_AVAILABLE` is gone. Use `NODE_POWER_PRESET=kepler` (Kepler 0.10 and later) or `kepler-legacy` (Kepler 0.7 to 0.9).
- [ ] **Facility inputs.** The facility default is the `otel` preset. The `datacenter_*` metric names need `FACILITY_PRESET=legacy`, deprecated and removed in v0.4.0. `FACILITY_ZONE_AMBIENT_METRIC_TEMPLATE` and `FACILITY_RACK_POWER_METRIC_TEMPLATE` are gone: use `FACILITY_ZONE_AMBIENT_TEMP_QUERY` and `FACILITY_RACK_POWER_QUERY`. `FACILITY_POLL_INTERVAL` of 0 or less stops the controller manager at start. The chart does not set `FACILITY_PROMETHEUS_ADDRESS`; it defaults to `PROMETHEUS_ADDRESS`.
- [ ] **Deprecated names.** Rename the variables the configuration reference lists as deprecated aliases (`OPERATOR_NODE_POWER_*`, `FACILITY_*_METRIC`): the controller manager logs a warning for each one it reads.

## Agents and caps

- [ ] **Agents on GPU nodes.** The agent DaemonSet tolerates the `nvidia.com/gpu` and `amd.com/gpu` `NoSchedule` taints (`agent.tolerations`), so agent pods appear on GPU nodes and enforce the caps planned for them. Set `agent.tolerations: []` to keep them off.
- [ ] **CPU of GPU nodes.** An eco GPU node gets no CPU cap: the CPU of a GPU node gets `PERFORMANCE_CAP_WATTS` whatever its profile, and only its GPUs take the eco cap.
- [ ] **AMD GPU model keys.** `GPU_PRODUCT_LABEL_KEYS` reads `amd.com/gpu.product-name` before `amd.com/gpu.family`, so an AMD node's model is its product name, such as `AMD_Radeon_PRO_W7900`. A `GPU_MODEL_CAPS_JSON` or `GPU_GOVERNOR_MODEL_PRESETS_JSON` keyed by a family value such as `GC_11_0_0` must use the product name.
- [ ] **Governors.** Governor control is new and off by default (`GOVERNOR_CONTROL`, `GOVERNOR_WRITES`); nothing changes until you turn it on. See [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

## Simulator

- [ ] **Telemetry format.** `/telemetry/{node}` serves OTLP JSON. Joulie up to v0.2 reading the simulator needs `?format=legacy` on that URL (deprecated, removed in v0.4.0), or the `/otlp/telemetry/{node}` endpoint with a v0.3 agent.
- [ ] **Listen address.** Outside a pod the simulator listens on `127.0.0.1:18080`. Set `SIM_ADDR=:18080` to reach it from other machines; the HTTP API has no authentication.
- [ ] **Model values.** A node the hardware catalogue matches takes its peak power, CPU cap range and GPU cap range from the catalogue: an H100 SXM caps between 300 and 700 W instead of 80 and 300 W, and two EPYC 9534 sockets peak at 560 W instead of 420 W. Set the [variable or the node class key]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-power-limits" >}}) to keep a value. A one-socket node with a measured curve draws half the two-socket curve.
- [ ] **Thermal throttling** is off by default; set the `SIM_*_THERMAL_THROTTLE_*_C` variables to turn it on ([Simulator thermal model]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-thermal-model" >}})).
- [ ] **Removed knobs.** `SIM_POD_W`, `SIM_DVFS_DROP_W_PER_PCT`, `SIM_GPU_ALPHA_UTIL`, `SIM_GPU_BETA_CAP`, and the node class keys `podW`, `dvfsDropWPerPct`, `noiseStddevW` and `gpu.powerModel` are ignored, with one warning each. Remove them.

Results of an older simulator: [`simulator/CHANGES.md`](https://github.com/joulie-k8s/Joulie/blob/main/simulator/CHANGES.md) lists every change that moved the numbers, and how far.
