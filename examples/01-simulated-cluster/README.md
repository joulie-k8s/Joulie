# 01 Simulated cluster: a first tour of Joulie

Joulie on ten emulated bare-metal nodes, on one machine with Docker. The
controller manager, the agent and the scheduler extender are the real ones,
built from your checkout. The agent discovers each node's CPUs and GPUs and
caps them through the same sysfs files and `nvidia-smi` calls it uses on a real
node; the simulator behind those files turns the caps into power and job
speed. You will see what the agent discovers, what the digital twin
makes of it, the caps each power profile gets, and the cluster at work under a
mixed workload.

Time: about ten minutes, plus the first build and image pulls. Needs Docker,
Go, `kubectl`, `helm` and `jq`, and `/dev/fuse` on the host; the
[simulated cluster page](https://joulie-k8s.github.io/Joulie/main/docs/simulator/simulated-cluster/#requirements)
lists the host requirements.

## Start the cluster

From the repository root:

```bash
export SIMCLUSTER_NAME=joulie-sim-demo
export SIMCLUSTER_SPEC=tests/simcluster/specs/demo.yaml
make simcluster-up       # k3s in one Docker container, ten fake Nodes from the spec
make simcluster-deploy   # images built from the tree; Prometheus, the simulator, the Joulie chart
export KUBECONFIG=~/.cache/joulie/simcluster/$SIMCLUSTER_NAME/kubeconfig
```

[`demo.yaml`](../../tests/simcluster/specs/demo.yaml) describes the cluster:
five hardware families, two racks in two cooling zones. Every component runs
on the one real node, `joulie-sim-demo-infra`; the other ten are fake Nodes,
each with an emulated agent of its own.

## 1. The nodes and the hardware the agent found

```bash
kubectl get nodes -L sim.joulie.io/pool,joulie.io/power-profile
kubectl get nodehardware
```

<!-- tutorials: check
eventually '[ "$(kubectl get nodehardware -o json | jq "[.items[] | select(.status.cpu.rawModel)] | length")" -eq 10 ]'
-->

```text
NAME       NODE       CPU                        GPU                   GPUCOUNT   QUALITY
epyc-0     epyc-0     AMD_EPYC_9534                                    0          exact
h100-0     h100-0     INTEL_XEON_PLATINUM_8260   NVIDIA_H100_SXM       8          exact
l40s-0     l40s-0     AMD_EPYC_9534              NVIDIA_L40S           4          exact
mi300x-0   mi300x-0   AMD_EPYC_9654              AMD_INSTINCT_MI300X   8          exact
xeon-0     xeon-0     INTEL_XEON_GOLD_6530                             0          exact
...
```

Each agent writes one `NodeHardware` with what it read on its node:
`/proc/cpuinfo`, the RAPL zones, `nvidia-smi` or the amdgpu files. `QUALITY`
`exact` means the model matched an entry of the hardware catalog, which adds
what the node cannot tell, such as the TDP. The CPU cap range comes from the
RAPL zones:

```bash
kubectl get nodehardware xeon-1 -o jsonpath='{.status.cpu.rawModel} sockets={.status.cpu.sockets} capRange={.status.cpu.capRange}{"\n"}'
# INTEL(R) XEON(R) GOLD 6530 sockets=2 capRange={"maxWattsPerSocket":270,"minWattsPerSocket":0,"type":"package"}
```

## 2. The digital twin

The controller manager keeps one `NodeTwin` per node: the profile it chose
(`spec`), and its model of the node (`status`).

```bash
kubectl get nodetwins
```

```text
NAME       NODE       PROFILE       CLASS         POWERHEADROOM       COOLINGSTRESS        PSUSTRESS            CPU-CAP   DRAINING   CPU-GOV   GPU-GOV   AGENT
epyc-0     epyc-0     performance   performance   91.25563901373695   9.109558598109171    0.6108354206998197   100       false                          True
epyc-1     epyc-1     eco           eco           85.42606502289493   9.109558598109171    0.6108354206998197   60        false                          True
h100-1     h100-1     eco           eco           89.43662234895223   7.470264464292682    4.247342291932753              false                          True
xeon-1     xeon-1     eco           eco           81.10786206671563   11.808687071622996   0.6108354206998197   60        false                          True
...
```

The default policy, `static_partition`, keeps part of each hardware family in
`performance` and runs the rest in `eco`. `POWERHEADROOM`, `COOLINGSTRESS` and
`PSUSTRESS` are the scores the scheduler extender reads; `AGENT` `True` says an
agent reports for the node, so its caps are enforced.

The measured power comes from Prometheus, where the simulator pushes the
OpenTelemetry `hw.power` and `hw.energy` metrics a real exporter would:

```bash
kubectl get nodetwin xeon-1 -o jsonpath='{.status.powerMeasurement}{"\n"}'
# {"cpuCappedPowerW":324,"cpuTdpW":540,...,"measuredNodePowerW":99.99530325986962,"nodeCappedPowerW":324,"nodeTdpW":540,...,"source":"prometheus"}
```

`cpuCappedPowerW` is 324 W: 60 % of the two packages' 540 W, the eco cap.

## 3. Power profiles become caps

An eco node gets a CPU cap of 60 % of its maximum, and a GPU node a GPU cap of
60 % per GPU; a performance node gets 100 %. The controller manager writes the
cap into the twin's `spec`, the agent applies it and reports the result in
`status.controlStatus`:

```bash
kubectl get nodetwin xeon-1 -o jsonpath='{.spec.cpu} {.status.controlStatus.cpu.result}: {.status.controlStatus.cpu.message}{"\n"}'
# {"packagePowerCapPctOfMax":60} applied: RAPL package cap applied
kubectl get nodetwin h100-1 -o jsonpath='{.spec.gpu} {.status.controlStatus.gpu.result}: {.status.controlStatus.gpu.message}{"\n"}'
# {"powerCap":{"capPctOfMax":60,"scope":"perGpu"}} applied: applied gpu cap 420.0W on 8 devices
```

The agent wrote the same files it writes on a real node. Each fake Node's
files live in its emulated tree, which `scripts/simcluster/tree.sh` reads:

```bash
scripts/simcluster/tree.sh xeon-1 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw   # 162000000: 60 % of 270 W, in microwatts
scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw   # 270000000: performance, uncapped
scripts/simcluster/tree.sh h100-1 state/nvml/gpu0/power_limit_mw                                # 420000: what nvidia-smi -pl set
```

<!-- tutorials: check
eventually '[ "$(scripts/simcluster/tree.sh xeon-1 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw)" = 162000000 ]'
eventually '[ "$(scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw)" = 270000000 ]'
eventually '[ "$(scripts/simcluster/tree.sh h100-1 state/nvml/gpu0/power_limit_mw)" = 420000 ]'
-->

## 4. Cap a node by hand

The controller manager owns the twin's `spec` and writes it back at every
reconcile, so stop it before you change a cap yourself:

```bash
kubectl -n joulie-system scale deploy/joulie-controller-manager --replicas=0
kubectl patch nodetwin xeon-0 --type merge -p '{"spec":{"cpu":{"packagePowerCapPctOfMax":40}}}'
sleep 20
kubectl get nodetwin xeon-0 -o jsonpath='{.status.controlStatus.cpu.result}{"\n"}'                # applied
scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw   # 108000000
```

<!-- tutorials: check
eventually '[ "$(scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw)" = 108000000 ]'
-->

The twin is named after its node: the agent reads only the twin of its own
node, so a `NodeTwin` under another name is never applied. Start the controller
manager again and it puts back the performance cap within a reconcile:

```bash
kubectl -n joulie-system scale deploy/joulie-controller-manager --replicas=1
sleep 30
scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw   # 270000000
```

<!-- tutorials: check
eventually '[ "$(scripts/simcluster/tree.sh xeon-0 sys/class/powercap/intel-rapl:0/constraint_0_power_limit_uw)" = 270000000 ]'
-->

## 5. A workload, seen through `kubectl joulie`

The `demo` scenario submits 112 jobs over 40 minutes: NVIDIA training and
inference, AMD fine-tuning, performance and standard CPU work. They are real
Kubernetes Jobs, placed by the scheduler and the Joulie extender on the fake
Nodes, where the simulator runs them on the emulated hardware. In a second
terminal, from the repository root:

<!-- tutorials: skip -->
```bash
make simcluster-scenario SCENARIO=tests/simcluster/scenarios/demo.yaml SIMCLUSTER_NAME=joulie-sim-demo
```

It reuses the running cluster and ends after about 45 minutes with its checks
and a summary per workload class:

```text
CHECK                   STATUS  SEVERITY  OBSERVED  EXPECTED
placement               pass    fail      0         == 0
jobs.completedFraction  pass    fail      1         >= 0.9
CLASS        JOBS  COMPLETED  JOBS/H  ENERGY J   J/JOB      SLOWDOWN P50  DURATION P95 S
performance  44    44         58.2    1.373e+07  3.12e+05   1.005         459.9
standard     68    68         90.0    1.683e+07  2.474e+05  1.063         628.0
demo: success (exit 0)
```

`placement` counts performance pods bound to an eco node. Meanwhile, build the
plugin and watch:

```bash
make kubectl-plugin
export PATH=$PWD/bin:$PATH
kubectl joulie status        # the cluster, then one row per node
kubectl joulie pods          # where each pod runs, by workload class
```

<!-- tutorials: skip -->
```bash
kubectl joulie top           # the live view; q quits
```

```text
 ⚡ JOULIE  cluster energy                                                                                                    default  18:41:29
╭─ Cluster ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
│ POWER      15.2 kW ━━━━━━━━━━━━━━━━━━━╸━━━━━━━━━━━━ 62% of 24.7 kW capped  ·  ↗ +534 W/min                                                   │
│ BUDGET   24.7 kW capped of 30.0 kW TDP  ·  caps hold back 5.30 kW                                                                            │
│ NODES    10  ● 5 performance 9.82 kW  ● 5 eco 5.42 kW    ·  agents 10/10 reporting                                                           │
│ PODS     29  ▲ 13 performance  · 16 standard  ·  ✓ every performance pod on a performance node                                               │
│ FACILITY headroom 53%  ·  cooling 43% (peak 96% h100-0)  ·  PSU 15%  ·  PUE 1.20                                                             │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
╭─ Nodes ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
│ NODE      HARDWARE            CLASS          POWER                 TREND  CAP CPU·GPU  HEADROOM     COOLING      CPU  MEM   GPU  PODS  AGENT │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ epyc-0    2× EPYC 9534        ● performance     151 W ━━╸━━━━━━━    ↗ +7     100% · -  ━━━━━━  73%  ━╸━━━━  28%  38%  13%     -  3 ▲3  ✓     │
│ epyc-1    2× EPYC 9534        ● eco             109 W ━━━━━━━━━━    ↗ +1      60% · -  ━━━━━━  68%  ━━━━━━  20%  57%  12%     -    11  ✓     │
│ h100-0    8× H100 SXM         ● performance   4.96 kW ━━━━━━━━━━     → 0  100% · 100%  ╸━━━━━  16%  ━━━━━╸  96%  34%  17%  100%  2 ▲2  ✓     │
│ h100-1    8× H100 SXM         ● eco           1.32 kW ━━━╸━━━━━━   ↗ +71   100% · 60%  ━━━╸━━  64%  ━╸━━━━  25%  17%   9%   25%     2  ✓     │
│ l40s-0    4× L40S             ● performance   1.30 kW ━━━━━━╸━━━  ↗ +186  100% · 100%  ━━━━━━  34%  ━━━━╸━  76%   6%   4%  100%  1 ▲1  ✓     │
│ mi300x-0  8× Instinct MI300X  ● performance   3.27 kW ━━━━╸━━━━━     → 0  100% · 100%  ━━━━━━  51%  ━━━━━━  56%   4%   4%   50%     1  ✓     │
│ mi300x-1  8× Instinct MI300X  ● eco           3.79 kW ━━━━━━━━╸━  ↗ +279   100% · 60%  ╸━━━━━  12%  ━━━╸━━  64%   8%   8%  100%     2  ✓     │
│ xeon-0    2× Xeon Gold 6530   ● performance     140 W ━━╸━━━━━━━    ↘ -8     100% · -  ━━━━━━  74%  ━╸━━━━  27%  44%   9%     -  7 ▲7  ✓     │
│ xeon-1    2× Xeon Gold 6530   ● eco             100 W ━━━━━━━━━━     → 0      60% · -  ━━━━━━  69%  ━━━━━━  19%   0%   0%     -     0  ✓     │
│ xeon-2    2× Xeon Gold 6530   ● eco             100 W ━━━━━━━━━━    ↘ -2      60% · -  ━━━━━━  69%  ━━━━━━  19%   0%   0%     -     0  ✓     │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
```

The caps hold back 5.30 kW of the 30.0 kW the nodes could draw. The eco GPU
nodes run their GPUs at 60 % and their CPUs uncapped: a CPU cap on a GPU node
slows the GPUs' data feed by more than it saves. Every performance pod (`▲`)
runs on a performance node.

`status` reads the same `NodeTwin`s, `NodeHardware` and pods as above;
`-o json` prints it for scripts. The
[configuration reference](https://joulie-k8s.github.io/Joulie/main/docs/getting-started/05-configuration-reference/#kubectl-plugin)
lists every command and flag.

## Clean up

<!-- tutorials: skip -->
```bash
make simcluster-down SIMCLUSTER_NAME=joulie-sim-demo
```

Keep the cluster for [02 Workload classes](../02-workload-classes/README.md)
and [03 Grafana dashboard](../03-grafana-dashboard/README.md), which run on it.
