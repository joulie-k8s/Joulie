# Changelog

The user-visible changes of each release, what to check when upgrading, and the security fixes. A pull request with a user-visible change adds its line under Unreleased. When a release is published without notes, the release workflow copies its section here into the GitHub release (`scripts/changelog-section.sh`).

## Unreleased

### Agents and nodes

- A percent CPU cap that the agent turns into watts spans, per socket, the catalog's TDP down to 0.55 x TDP. The low end of the cTDP range was the floor, but it is a configurable TDP, not the lowest package power limit, which no vendor publishes (AMD EPYC 9004 tuning guide, publication 58002). (#104)
- A control command that the HTTP backend answers with `ok: false`, as the simulator answers a cap below what the hardware accepts, is an error in the agent's control status, where it counted as applied. (#104)

### Simulator

- A CPU or GPU cap below the lowest the hardware accepts is refused with an error, and the node keeps its cap, where the simulator raised the cap to that minimum. The CPU minimum is `SIM_RAPL_CAP_MIN_W`, now 0 (was 80 W), since no vendor publishes one; a cap the CPU cannot reach holds it at its lowest-frequency power. The GPU minimum is the board's lowest power limit (`SIM_GPU_MIN_CAP_WATTS_PER_GPU`). The per-socket floor `SIM_CPU_SOCKET_CAP_MIN_W` is removed. (#104)
- The cluster CPU and GPU utilization of the simulator's time series weigh each node by its cores and its GPUs, where they averaged the nodes, so CPU-only nodes no longer dilute the GPU utilization. (#104)
- In the simulator, a node whose last job ends runs no load from that moment, where it kept that job's CPU and GPU utilization, and its power, until a new job arrived. Idle nodes counted as busy in the energy and GPU utilization of the GPU experiments, and in standalone runs whose nodes go idle (the reference scenario's CPU energy falls by 2.7 to 2.9 %). (#104)
- A pod of a legacy trace (schema v2) keeps the affinity of its record's `podTemplate`, with the NotIn-eco requirement of a performance pod ANDed into it, where it had only that requirement. The sweeps of experiments 02 and 03 restrict CPU-only jobs to `joulie.io/hw.kind=cpu-only` nodes, which the simulator ignored, so those jobs could run on GPU nodes. The record's `nodeSelector` and `tolerations` stay unread for legacy pods. (#104)
- A CPU on a measured SPECpower curve draws the curve at its utilization, with the dynamic part scaled by the frequency scale to the power `SIM_BETA_FREQ`, as the generic CPU model does. It drew the curve at utilization x throughput, so every watt a cap saved came out of delivered work, and a cap cost more throughput than power by construction. `SIM_BETA_FREQ` defaults to 1.43 (was 1.35, unsourced), fitted to published frequency and power caps on servers (`TestBetaFreqFit`); the emulated profiles that took the old default take the new one. (#104)

### Scheduler extender

- The extender returns its node score as a priority from 0 to 10, the range of the extender API, where it returned the 0 to 100 score. kube-scheduler multiplies a priority by the extender's weight and by 10 and does not clamp it, so the extender weighed ten times its configured weight and overrode every scoring plugin, such as a GPU bin-packing strategy. (#104)

### Experiments

- Experiments 01 to 03 ran again on this release's simulator, measured at regime with 2 seeds and a repeat each, and every run passed gates on the queue, the utilization, idle nodes and the node classes. On one GPU family (03) the policies drew 6 to 9 % less power and did 6 to 10 % less GPU work, at about the same energy per unit. On mixed families they used 1.4 to 2.7 % (01, CPU) and 0.1 to 4.6 % (02, GPU) more energy per unit; that sign rests on the simulator giving every core and every GPU the same speed. These replace the 3 to 11 % lower power of the 0.2.4 rerun, which ran one seed over a fixed window with jobs still running. Since then the simulator's idle nodes, CPU curve law and GPU cap exponents, the scheduler extender's weight, the node classes of experiment 02 and the measurement window have changed (above, and `simulator/CHANGES.md`); no run separates their shares. (#104)

### Upgrading

- Percent CPU caps over HTTP now reach 0.55 x TDP per socket, where the low end of the cTDP range was the floor (320 W per socket on the AMD EPYC 9654 and 9375F), so runs with percent caps change again: on experiment 01's `configs/benchmark-debug.yaml` the saving of C against A rises from 12.0 % to 13.3 % (`simulator/CHANGES.md`). (#104)
- `SIM_CPU_SOCKET_CAP_MIN_W` and the class model key `cpuSocketCapMinW` are ignored with a warning. A cap below `SIM_RAPL_CAP_MIN_W` or `SIM_GPU_MIN_CAP_WATTS_PER_GPU` is now refused rather than raised, and the agent reports it as an error. (#104)
- The scheduler extender now weighs as much as its `weight` says, ten times less than before at the same weight, so kube-scheduler's own scoring plugins take part in placement again. To keep the old balance, multiply the extender's `weight` in the scheduler configuration by 10. (#104)
- A CPU cap that binds on a node with a measured curve now costs less throughput than power, where it cost more, and `SIM_BETA_FREQ` defaults to 1.43: runs with binding CPU caps change (the reference scenario's B and C by -0.04 % of CPU energy; `simulator/CHANGES.md`). Set `SIM_BETA_FREQ=1.35` to keep the old exponent on the generic model; the old curve behaviour has no switch. (#104)
- `workloadgen` no longer accepts `-gpu-request`, which nothing read; drop it from scripts that pass it. (#104)

## v0.2.5 (2026-10-27)

### Examples and dashboards

- The examples are six tutorials that take a new user from a first install to the main features: a simulated cluster with the digital twin, the caps and `kubectl joulie`, workload classes and the scheduler extender, the Grafana dashboard, the OpenTelemetry Collector bridge, governor presets, and cooling and PUE. The first three need only Docker. Examples that no longer ran (stress-ng throttling, controller manager configuration, scheduler steering) or that the simulated cluster replaces (the simulated telemetry, KWOK and kubectl joulie demos, the Prometheus and workload class examples) are removed, and the KWOK cluster of the benchmark experiments moved to `experiments/kwok-cluster`. (#95)
- The scheduler extender's Service carries `prometheus.io/*` annotations (`schedulerExtender.service.annotations`), as the agent's and the controller manager's do, so the Scheduler row of the chart's dashboard gets data from a Prometheus that discovers targets by annotation. (#95)
- The chart's dashboard is the only one: the four under `examples/` each showed a subset of it. It draws each node's policy state, profile and cap backend as a timeline, one row per node, where it drew every state of every node as a line, and its CPU frequency panel shows the current frequency beside the maximum, in GHz where it showed MHz as kHz. A test parses every query of every dashboard in the PromQL parser and fails on a metric Joulie does not export. (#95)

### Agents and nodes

- A percent CPU cap (`packagePowerCapPctOfMax`) on a node without RAPL files that the agent caps through an HTTP control backend, such as the simulator's KWOK nodes driven by a pool agent, is resolved into watts against the hardware catalog's CPU package cap range of the node's CPU times its socket count, the range the simulator models the node with, and sent as `rapl.set_power_cap_watts`. It used to fall back to `dvfs.set_throttle_pct`. The CPU model and the socket count come from the node's labels as the simulator reads them (`joulie.io/hw.cpu-model`, else NFD's `cpu-model.name`, and `joulie.io/hw.cpu-sockets`), never from the pool agent's own `/proc/cpuinfo`. The node's own RAPL files still come first; over HTTP their range counts every package, since `capWatts` is the node's total. `TELEMETRY_CPU_CONTROL_MODE=dvfs`, a CPU missing from the catalog and a node without the socket label keep the DVFS throttle, and so does a percent cap whose watts the backend refuses. After a watt cap the agent sets the backend's DVFS throttle to 0 once, so a throttle sent for an earlier percent cap does not hold the node below its cap. The CPU control status names the catalog range it used, the digital twin's `cpuCappedPowerW` clamps a percent cap up to the bottom of the cap range as the agent does, and the standalone simulator gives a legacy CPU-only node the watt cap its pool agent would send. (#99)

### Simulator

- The simulator chart ships the Grafana dashboard `Joulie - Simulated Cluster`, off by default (`grafanaDashboard.enabled`): one cell per node that Joulie plans or the simulator measures, coloured by its inlet temperature and labelled with the power profile Joulie applied (`performance`, `draining` or `eco`), at the end of the time picker's range, then the profiles and inlet temperatures over the range, the outside air, IT and cooling power, PUE, host power, power limits and rack power. (#100)
- The emulated Kepler 0.7 exporter also serves `joulie_sim_kepler07_idle_excess_joules_total{instance, family}`, the idle energy its series report beyond the energy they measured, which Kepler 0.7 reports while a cap holds a package below the idle power it recorded. It is the simulator's own series, not Kepler's. The simulated cluster adds it to the truth of the `kepler-legacy` checks and warns about every node that has some, so the nightly smoke no longer fails on a reading Kepler 0.7 itself inflates. (#91)

### Controller manager

- The facility input reads the grid carbon intensity from the operator's own query, `FACILITY_CARBON_INTENSITY_QUERY`, with its unit (`FACILITY_CARBON_INTENSITY_UNIT`, default `gCO2eq/kWh`, `lbsCO2/MWh` for WattTime) and its signal (`FACILITY_CARBON_INTENSITY_SIGNAL`, `marginal` or `average`, required), converts it to gCO2eq/kWh, counts it in `joulie_twin_input_queries_total{input="carbon_intensity"}`, exports it as `joulie_grid_carbon_intensity_grams_per_kW_h{joulie_grid_carbon_intensity_signal}` (OpenTelemetry `joulie.grid.carbon_intensity`), and hands it to the policy with the rest of the facility snapshot, which no policy reads yet. (#101)

### Upgrading

- A pool agent that caps simulated nodes over HTTP, with `TELEMETRY_CPU_CONTROL_MODE` other than `dvfs`, now turns a percent CPU cap into watts within the catalog range where it used to throttle, so every run with percent caps changes: in experiments 01 to 03 the configs with `cpu_write_absolute_caps: false`, which are `configs/benchmark.yaml` and `configs/benchmark-overnight.yaml` of each and `configs/benchmark-showcase.yaml` and `configs/cluster.yaml` of 02, and in 01 a config without the key, which its `05_sweep.py` reads as `false`; and every standalone run (`scripts/standalone_sweep.py`): on experiment 01's `configs/benchmark-debug.yaml` the saving of C against A falls from 17.3 % to 12.0 % (`simulator/CHANGES.md`). The low end of the cTDP range of the AMD EPYC 9654 and 9375F is 320 W per socket, so on two sockets no percent cap goes below 640 W, and the 9654's SPECpower curve peaks at 817 W, above its 720 W range, so even a 100 % cap holds it at full load. The published reports of these experiments ran `cpu_write_absolute_caps: true`, whose caps were already watts. A running simulator needs no restart: the new agent clears the throttle an older one left. (#99)
- A link or script to an example needs its new path: the examples are `examples/01-simulated-cluster` to `examples/06-cooling-pue`, and `examples/07-simulator-gpu-powercaps` is `experiments/kwok-cluster`. (#95)
- A Prometheus that discovers targets by annotation now scrapes the scheduler extender; `schedulerExtender.service.annotations: {}` keeps it out. (#95)

## v0.2.4 (2026-10-21)

### kubectl plugin

- `kubectl joulie status` draws the cluster as styled tables: a summary of measured, capped and TDP power, nodes and power per class, pods per class and the facility scores, then one row per node with its hardware, class, power against its cap, trend, applied caps, digital twin scores, requested resources, pods and agent condition. Columns that do not fit the terminal are dropped. (#85)
- New commands: `top`, a live dashboard with a power sparkline per node, node details and pod placement; `node NAME`; `pods`, which flags a performance pod on an eco node; `version`. `-o json` prints the same data for scripts, and the kubectl flags (`--kubeconfig`, `--context`, `-n` and the others) work as in kubectl. (#85)
- Install with Krew, `kubectl krew install joulie`, once the plugin is in the Krew index. Each release attaches `kubectl-joulie_<tag>_<os>_<arch>.tar.gz` archives, Windows included, besides the bare binaries. (#85)

### GPU control

- AMD GPUs are capped through the amdgpu hwmon files under `/host-sys`; the agent no longer calls `rocm-smi`, which has no power cap flag. MIG devices, shared replicas and AMD partitions are capped and counted by the share of a GPU they stand for. (#75)
- Intel discrete GPUs are capped through their `i915` or `xe` hwmon files (backend `host-intel`), and the catalogue knows the Flex 170 and the Max 1100 and 1550. (#78)
- An NVIDIA GPU whose limits print a token, such as `[N/A]`, is never written: its caps end `blocked` with a message naming the GPU. (#77)
- Two-die AMD boards (MI250X) cap their primary dies only, and `capRangePerGpu.defaultWatts` is NVIDIA's `power.default_limit`. (#79)

### Agents and nodes

- The agent runs on every node labelled `joulie.io/managed=true`, tainted ones included: `agent.tolerations` defaults to `[{operator: Exists}]`. (#82)
- Every image is built for `linux/amd64` and `linux/arm64`. (#82)
- Each `NodeTwin` carries the condition `AgentReporting`, `False` on a managed node no agent reports for, where the planned caps are not enforced. `kubectl get nodetwins` shows it in an `Agent` column, and `joulie_twin_nodes_agent_reporting` counts the nodes. (#82)

### Controller manager

- CPU and GPU governor presets, a second energy dimension next to the power caps, off by default (`GOVERNOR_CONTROL=off`). (#73)
- Two opt-in GPU rules, both off by default: `GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU` and `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES`. (#83)

### Simulator and testing

- A simulated bare-metal cluster: real Joulie components on emulated nodes in one k3s container, with OpenTelemetry `hw.*` telemetry and the Kepler, dcgm-exporter and AMD device-metrics-exporter formats. (#72, #73)
- The emulated sysfs is served through FUSE, so a write the kernel would refuse fails at once and energy counters move between reads. (#81)
- Hardware measurement tooling for the governor work. (#80)

### Upgrading

- Apply the new CRDs before `helm upgrade`: the governor fields and the `AgentReporting` condition need them. The [upgrade guide](https://joulie-k8s.github.io/Joulie/stable/docs/getting-started/06-upgrading/) lists every step.
- Agent pods now start on tainted managed nodes. To keep the agent off some of them, list the taints it may tolerate in `agent.tolerations`.
- A script that parsed the text of `kubectl joulie status` reads `kubectl joulie status -o json` instead.

### Security

- The binaries and images are built with Go 1.26.10 instead of Go 1.22.9, with every Go standard library fix in between, GO-2026-6809 (`crypto/x509`) among them. (#74, #84)
- CI actions are pinned by commit and container base images by digest, and the dependencies are scanned with osv-scanner and govulncheck on every change to a manifest and nightly. (#74, #84)
- Vulnerabilities are reported privately through a GitHub security advisory, as [SECURITY.md](SECURITY.md) describes. (#85)

## Earlier releases

The notes of v0.2.3 and earlier are on their GitHub releases: [v0.2.3](https://github.com/joulie-k8s/Joulie/releases/tag/v0.2.3), [v0.2.2](https://github.com/joulie-k8s/Joulie/releases/tag/v0.2.2), [v0.2.1](https://github.com/joulie-k8s/Joulie/releases/tag/v0.2.1), [v0.2.0](https://github.com/joulie-k8s/Joulie/releases/tag/v0.2.0).
