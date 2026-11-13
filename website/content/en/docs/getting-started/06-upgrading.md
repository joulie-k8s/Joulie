---
title: "Upgrading"
weight: 60
---

What to check when you upgrade a Joulie installation from v0.2 to v0.3. Go through the list before running `helm upgrade`.

## Before the upgrade

- [ ] **Apply the new CRDs.** `helm upgrade` never updates CRDs, and the API server prunes every field the installed CRD does not know, such as the governor fields. Apply them first:

  ```bash
  kubectl apply -f charts/joulie/crds/
  ```

  Without a checkout, take them from the release chart: `helm pull oci://registry.cern.ch/joulie/joulie --version <version> --untar --untardir /tmp/joulie-chart`, then `kubectl apply -f /tmp/joulie-chart/joulie/crds/`.
- [ ] **Registry.** Point every `image.repository` under `registry.cern.ch/mbunino/joulie` at `registry.cern.ch/joulie`, where charts, images and the plugin are published.
- [ ] **Operator names.** The pre-v0.2.0 aliases are gone. Use the image `joulie-controller-manager` (not `joulie-operator`), `NODE_POWER_*` (not `OPERATOR_NODE_POWER_*`), `joulie_policy_*` metrics (not `joulie_operator_*`), and the values key `controllerManager`: a values file that still sets `operator` fails the render.

## kubectl plugin

- [ ] **Scripts.** `kubectl joulie status` prints styled tables and drops columns that do not fit the terminal. A script that parsed its text reads `kubectl joulie status -o json` instead ([fields]({{< relref "/docs/getting-started/05-configuration-reference.md#kubectl-plugin" >}})).
- [ ] **Watch.** `kubectl joulie status -W` opens the live dashboard, also `kubectl joulie top`: `q` quits it.
- [ ] **Install.** `kubectl krew install joulie`.

## Measured node power

- [ ] **Prometheus.** The chart sets `NODE_POWER_SOURCE=prometheus` (preset `otel`, `http://prometheus-operated.monitoring:9090`); without node power data every node reads 0 W with source `prometheus-no-data`. Set up Prometheus as the [quickstart]({{< relref "/docs/getting-started/01-quickstart.md#prerequisites" >}}) says, or set `controllerManager.env.NODE_POWER_SOURCE=static`.
- [ ] **Kepler.** `KEPLER_AVAILABLE` is gone. Use `NODE_POWER_PRESET=kepler` (Kepler 0.10 and later) or `kepler-legacy` (Kepler 0.7 to 0.9).
- [ ] **Facility inputs.** The default preset is `otel`; the `datacenter_*` names need `FACILITY_PRESET=legacy` (removed in v0.4.0). Replace `FACILITY_ZONE_AMBIENT_METRIC_TEMPLATE` and `FACILITY_RACK_POWER_METRIC_TEMPLATE` with `FACILITY_ZONE_AMBIENT_TEMP_QUERY` and `FACILITY_RACK_POWER_QUERY`. `FACILITY_POLL_INTERVAL` of 0 or less stops the controller manager at start. `FACILITY_PROMETHEUS_ADDRESS` defaults to `PROMETHEUS_ADDRESS`.
- [ ] **Deprecated names.** Rename the `FACILITY_*_METRIC` aliases the configuration reference lists; each one read logs a warning.

## Agents and caps

- [ ] **Agents on tainted nodes.** The agent tolerates every taint (`agent.tolerations: [{operator: Exists}]`), so it runs and enforces caps on every node labelled `joulie.io/managed=true`. To keep it off some, list the taints it may tolerate in `agent.tolerations`; those nodes then report `AgentReporting` `False`.
- [ ] **arm64 nodes.** Images are published for `linux/amd64` and `linux/arm64`, so arm64 nodes get an agent too; a mirror must copy both platforms.
- [ ] **Nodes without an agent.** `NodeTwin` carries the condition `AgentReporting` (the `Agent` column of `kubectl get nodetwins`), `False` on a managed node no agent publishes `NodeHardware` for, whose caps are not enforced ([Managed nodes without an agent]({{< relref "/docs/getting-started/05-configuration-reference.md#managed-nodes-without-an-agent" >}})). It needs the new CRDs.
- [ ] **CPU of GPU nodes.** An eco GPU node gets no CPU cap; only its GPUs take the eco cap. Set `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES=true` to cap the CPU of a GPU node whose `NodeHardware` reports no GPU control.
- [ ] **Twin GPU budget.** A planned GPU cap counts in the twin's budget only where it applies, so eco nodes whose GPU cap is `blocked` or `error`, or that report no GPU control, show less headroom than before ([Digital twin]({{< relref "/docs/architecture/digital-twin.md#node-tdp-and-the-capped-budget" >}})).
- [ ] **AMD GPU model keys.** An AMD node's model is its product name (`amd.com/gpu.product-name`, such as `AMD_Radeon_PRO_W7900`), read before `amd.com/gpu.family`. Rekey `GPU_MODEL_CAPS_JSON` and `GPU_GOVERNOR_MODEL_PRESETS_JSON` entries keyed by a family such as `GC_11_0_0`.
- [ ] **AMD GPU caps.** AMD GPUs are now capped through their amdgpu hwmon files under `/host-sys`, and publish their cap range and `gpu.powerDomains` in `NodeHardware`. Keep the chart's root agent and writable `/host-sys`, and apply the new CRDs, or a two-die package such as an MI250X keeps a doubled GPU budget.
- [ ] **GPU default limit.** `gpu.capRangePerGpu.defaultWatts` is the NVIDIA `power.default_limit`, which can be below `maxWatts`: a script or dashboard that took it for the maximum must read `maxWatts`. A wrapper that answers for `nvidia-smi` must answer the inventory query, which now ends in `power.default_limit`.
- [ ] **Intel GPU caps.** Intel discrete GPUs are now capped through their i915 or xe hwmon files under `/host-sys` (backend `host-intel`) and take the eco GPU cap. A values file with its own `GPU_PRODUCT_LABEL_KEYS` must include `gpu.intel.com/product`.
- [ ] **NVIDIA GPUs without a settable power limit.** A GPU whose limits print `[N/A]` reports `blocked` instead of `error` and is named in `NodeHardware` `gpu.warnings`; a `-pl` that prints `not supported` reports `error`.
- [ ] **Shared, partitioned and DRA GPU nodes.** Nodes with MIG devices, time-slicing or MPS replicas, AMD partitions, `gpu.intel.com/xe`, or a DRA GPU count are GPU nodes: in eco their GPUs take the eco cap and their CPU none. Label a node `joulie.io/reserved=true` to keep Joulie off it ([Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})).
- [ ] **GPU product suffixes.** Key `GPU_MODEL_CAPS_JSON` and `GPU_GOVERNOR_MODEL_PRESETS_JSON` by the GPU product without the `-MIG-<profile>`, `-MIG-INVALID` and `-SHARED` suffixes, such as `NVIDIA-H100-NVL`.
- [ ] **Extender GPU estimate.** Pods that request MIG devices, AMD partitions, shared replicas or `gpu.intel.com/xe` add GPU watts to the extender's estimate ([Scheduler]({{< relref "/docs/architecture/scheduler.md" >}})).
- [ ] **Governors.** Governor control is new and off by default (`GOVERNOR_CONTROL`, `GOVERNOR_WRITES`); nothing changes until you turn it on. `GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU=true` holds `sustained` only while a pod of 2 or more GPUs runs on the node. See [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

## Monitoring and examples

- [ ] **Extender metrics.** The extender's Service carries `prometheus.io/*` annotations (`schedulerExtender.service.annotations`), so annotation-based discovery starts scraping `joulie_scheduler_*`. Set the value to `{}` to keep it unscraped.
- [ ] **Example dashboards.** The four dashboards under `examples/` are gone; the chart's dashboard (`grafanaDashboard.enabled`) holds what they showed.
- [ ] **Example paths.** The examples are six tutorials (`examples/01-simulated-cluster` to `examples/06-cooling-pue`). The KWOK cluster of the benchmark experiments moved from `examples/07-simulator-gpu-powercaps` to `experiments/kwok-cluster`.

## Simulator

- [ ] **Telemetry format.** `/telemetry/{node}` serves OTLP JSON. Joulie up to v0.2 reading the simulator needs `?format=legacy` (removed in v0.4.0), or a v0.3 agent on `/otlp/telemetry/{node}`.
- [ ] **Listen address.** Outside a pod the simulator listens on `127.0.0.1:18080`. Set `SIM_ADDR=:18080` to reach it from other machines; the HTTP API has no authentication.
- [ ] **Model values.** Nodes the hardware catalogue matches take their peak power and cap ranges from it (an H100 SXM caps between 300 and 700 W instead of 80 and 300 W). Set the [variable or the node class key]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-power-limits" >}}) to keep a value.
- [ ] **Thermal throttling** is off by default; set the `SIM_*_THERMAL_THROTTLE_*_C` variables to turn it on ([Simulator thermal model]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-thermal-model" >}})).
- [ ] **Removed knobs.** Remove `SIM_POD_W`, `SIM_DVFS_DROP_W_PER_PCT`, `SIM_GPU_ALPHA_UTIL`, `SIM_GPU_BETA_CAP` and the node class keys `podW`, `dvfsDropWPerPct`, `noiseStddevW` and `gpu.powerModel`: they are ignored, with one warning each.

Results of an older simulator: [`simulator/CHANGES.md`](https://github.com/joulie-k8s/Joulie/blob/main/simulator/CHANGES.md) lists every change that moved the numbers, and how far.

Details behind each item: [Upgrade notes](https://github.com/joulie-k8s/Joulie/wiki/Upgrade-Notes).
