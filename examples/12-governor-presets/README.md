# Example: CPU and GPU governor presets

Next to power caps, Joulie can set the frequency governors of a node: one vendor-neutral preset per component in the `NodeTwin` spec, `spec.cpu.governor` and `spec.gpu.governor`. The agent turns a preset into the native setting of the node's cpufreq driver or GPU family (a governor word, an energy-performance preference, an amdgpu performance level, an `nvidia-smi` clock lock) and reports what it did.

Governor control is off by default, behind two gates:

- the controller manager writes presets only with `GOVERNOR_CONTROL` set to `cpu`, `gpu` or `all`;
- the agent applies them only with `GOVERNOR_WRITES` set to `true`; otherwise it reports `disabled` and changes nothing.

Discovery is always on: every agent lists in its `NodeHardware` the presets its node accepts. This example turns both gates on in the simulated cluster (k3s in Docker with emulated hardware, see [Simulated cluster](https://joulie-k8s.github.io/Joulie/docs/simulator/simulated-cluster/)) and follows each preset from the `NodeTwin` to the emulated `/sys` files and back.

| File | What it is |
|---|---|
| [`cluster-spec.yaml`](./cluster-spec.yaml) | 12 emulated Nodes, two per hardware family: Xeon (`intel_pstate`), EPYC (`acpi-cpufreq`), Radeon PRO W7900, Instinct MI300X, H100 NVL, L40S |
| [`values.yaml`](./values.yaml) | both gates on, every other governor setting at the chart default |
| [`nodetwin-governor.yaml`](./nodetwin-governor.yaml) | the governor fields of a `NodeTwin` spec, to apply by hand |

The presets:

| Component | Presets |
|---|---|
| CPU | `performance` (highest frequency at any load), `balanced` (demand-driven, quick ramp), `efficiency` (demand-driven with an energy bias); `boost` `on` or `off` optionally |
| GPU | `auto` (vendor default, and the restore target), `sustained` (data-path clocks at their maximum, idle gating kept), `efficient` (compute clock under a ceiling, 80 % of its maximum by default), `low` and `peak` (experiments only) |

With [`values.yaml`](./values.yaml), a performance node gets CPU `performance` and no GPU preset; an eco node gets CPU `efficiency` and GPU `efficient`. A GPU node gets no CPU preset (`CPU_GOVERNOR_ON_GPU_NODES=false`, the rule the CPU cap follows). Every setting is in the [configuration reference](https://joulie-k8s.github.io/Joulie/docs/getting-started/05-configuration-reference/#governor-control).

## Run it

Needs Docker, `kubectl`, `helm` and `jq`. From the repository root:

```bash
export SIMCLUSTER_NAME=joulie-sim-governor
export SIMCLUSTER_SPEC=examples/12-governor-presets/cluster-spec.yaml
export SIMCLUSTER_EXTRA_VALUES=examples/12-governor-presets/values.yaml
make simcluster-up
make simcluster-deploy
export KUBECONFIG=~/.cache/joulie/simcluster/$SIMCLUSTER_NAME/kubeconfig
```

`SIMCLUSTER_EXTRA_VALUES` puts `values.yaml` on top of `values/simcluster.yaml`. The policy keeps one Node of each pool in performance and runs the other in eco; which one is decided at the first reconcile, so read it from the `NodeTwin`s rather than from the name.

## 1. The presets in the NodeTwins

```bash
kubectl get nodetwins
```

The last two columns, `CPU-Gov` and `GPU-Gov`, are the presets the controller manager wrote. The agent's outcome is in `status.controlStatus`:

```bash
kubectl get nodetwins -o custom-columns=NAME:.metadata.name,PROFILE:.spec.profile,CPU-GOV:.spec.cpu.governor.preset,CPU-RESULT:.status.controlStatus.cpuGovernor.result,GPU-GOV:.spec.gpu.governor.preset,GPU-RESULT:.status.controlStatus.gpuGovernor.result
```

```text
NAME       PROFILE       CPU-GOV       CPU-RESULT   GPU-GOV     GPU-RESULT
epyc-0     performance   performance   applied      <none>      <none>
epyc-1     eco           efficiency    applied      <none>      <none>
h100-0     performance   <none>        <none>       <none>      <none>
h100-1     eco           <none>        <none>       efficient   applied-unverified
l40s-0     performance   <none>        <none>       <none>      <none>
l40s-1     eco           <none>        <none>       efficient   applied-unverified
mi300x-0   performance   <none>        <none>       <none>      <none>
mi300x-1   eco           <none>        <none>       efficient   applied
w7900-0    performance   <none>        <none>       <none>      <none>
w7900-1    eco           <none>        <none>       <none>      <none>
xeon-0     eco           efficiency    applied      <none>      <none>
xeon-1     performance   performance   applied      <none>      <none>
```

`status.controlStatus.cpuGovernor.applied` and `gpuGovernor.applied` hold the native setting:

| Pool | Mechanism | Performance Node | Eco Node |
|---|---|---|---|
| `xeon` | `intel_pstate` active, with energy-performance preference | `performance` | `powersave epp=balance_power` |
| `epyc` | `acpi-cpufreq` | `performance` | `conservative` |
| `w7900` | amdgpu performance level | nothing | nothing: `efficient` has no mapping on this family (its overdrive file needs a non-default `ppfeaturemask` bit, and its fine-grained compute clock takes only two levels), so the preset is not in its `NodeHardware` and the controller manager writes none, with one warning in its log |
| `mi300x` | amdgpu `manual` plus an overdrive ceiling | nothing | `manual sclk_max=1680`: 80 % of the 2100 MHz maximum |
| `h100`, `l40s` | `nvidia-smi -i <bus id> -lgc` | nothing | `-lgc <lowest>,<ceiling>`: the lowest supported compute clock, and the highest supported one at or below 80 % of the maximum |

The NVIDIA result stays `applied-unverified` while the GPUs idle: a clock lock cannot be read back, so the agent turns it `applied` only after three busy samples show the clocks inside the lock. A preset a node cannot express is never replaced by another one, `auto` included, so a setting its operator chose stays.

## 2. What each node accepts: NodeHardware

```bash
kubectl get nodehardware xeon-0 -o yaml
```

On the eco Xeon Node (`xeon-0` in the run above), the governor block under `status.cpu` (and under `status.gpu` on a GPU node) is discovery, read-only (abridged):

```yaml
governor:
  driver: intel_pstate
  driverMode: active
  current: powersave
  available: [performance, powersave]
  epp:
    available: [default, performance, balance_performance, balance_power, power]
    current: balance_power
  boost: {scope: no-turbo, state: "on"}
  presets:
  - {preset: performance, native: performance, result: applied}
  - {preset: balanced, native: powersave epp=balance_performance, result: applied}
  - {preset: efficiency, native: powersave epp=balance_power, result: applied}
  baseline:
    groups:
    - {policies: 0-95, governor: powersave, epp: balance_performance, boost: "on"}
  writable: true
```

- `presets` lists only what the node can express, with the native setting each preset maps to; the controller manager writes only these. The W7900 lists `auto`, `sustained` (`high`), `low` and `peak`; the MI300X `auto` and `efficient`; the H100 lists `sustained` as `equivalent` to `auto`, since its memory clock is fixed.
- `baseline` is what the agent found before its first write, and what it puts back. `current` here already shows the eco preset.

## 3. The emulated files

The agent writes the files of the node's `/sys`; in the simulated cluster they live in each Node's hwemu tree:

```bash
scripts/simcluster/tree.sh xeon-0 sys/devices/system/cpu/cpufreq/policy0/scaling_governor               # powersave
scripts/simcluster/tree.sh xeon-0 sys/devices/system/cpu/cpufreq/policy0/energy_performance_preference   # balance_power
scripts/simcluster/tree.sh epyc-1 sys/devices/system/cpu/cpufreq/policy0/scaling_governor               # conservative
scripts/simcluster/tree.sh mi300x-1 sys/bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level  # manual
scripts/simcluster/tree.sh mi300x-1 sys/bus/pci/devices/0000:10:00.0/pp_od_clk_voltage                  # OD_SCLK 1: 1575Mhz
scripts/simcluster/tree.sh w7900-1 sys/bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level   # auto
scripts/simcluster/tree.sh l40s-1 state/nvml/gpu0/locked_gpu_clocks                                     # <min>,<max> of the lock
```

Use the names of your eco Nodes. `make simcluster-tree NODE=<node>` prints the whole tree. An NVIDIA lock has no file on a real node, and no query prints it: the fake `nvidia-smi` keeps it under `state/nvml` so the emulated clocks can follow it.

## 4. Opt a GPU model in to `sustained`

No default selects `sustained`. It holds the data-path clocks (memory, fabric) at their maximum while the GPU idles, so it costs idle power on every node of that model, and what it gains depends on the model: a demand-driven governor raises those clocks only for the GPU's own kernels, so the case for `sustained` is a GPU that receives peer transfers on a family whose data-path clocks bound them. Opt a model in only once a measurement on that model shows the gain.

The opt-in is per model, in `GPU_GOVERNOR_MODEL_PRESETS_JSON`. Its key is the node's GPU product label value (the first of `GPU_PRODUCT_LABEL_KEYS` the node carries) or the model its `NodeHardware` reports:

```bash
kubectl get nodehardware w7900-0 -o jsonpath='{.status.gpu.model} {.status.gpu.rawModel}{"\n"}'   # AMD_RADEON_PRO_W7900 AMD_Radeon_PRO_W7900
```

The emulated W7900 Nodes carry the AMD labeller's `amd.com/gpu.product-name=AMD_Radeon_PRO_W7900`, which the default `GPU_PRODUCT_LABEL_KEYS` reads before `amd.com/gpu.family`, so their key is the product name. The family value (`GC_11_0_0`) names no board and is read only when no product label is present. This example opts the W7900 in because `sustained` is a level there that reads back (`high`); the MI300X cannot express it, and on the H100 it is `equivalent` to `auto`.

```bash
cat > /tmp/sustained.yaml <<'EOF'
controllerManager:
  env:
    GPU_GOVERNOR_MODEL_PRESETS_JSON: '{"AMD_Radeon_PRO_W7900": {"performance": "sustained"}}'
EOF
helm -n joulie-system upgrade joulie charts/joulie --reuse-values -f /tmp/sustained.yaml
```

After the controller manager restarts, the W7900 performance Node shows `GPU-Gov` `sustained`, `gpuGovernor.applied` `high`, and every one of its level files reads `high`:

```bash
scripts/simcluster/tree.sh w7900-0 sys/bus/pci/devices/0000:10:00.0/power_dpm_force_performance_level   # high
```

The eco W7900 Node still gets nothing: the entry sets only `performance`, and the eco default `efficient` is not in its presets. An entry can also set `eco` and `ecoCeilingPctOfMax`.

## 5. Try a preset by hand

[`nodetwin-governor.yaml`](./nodetwin-governor.yaml) holds the fields as the controller manager writes them, plus a CPU preset on a GPU node, which it writes only with `CPU_GOVERNOR_ON_GPU_NODES=true`. The controller manager writes its whole spec back at every reconcile that finds the object different from its decision, so stop it first, and set `nodeName` and `metadata.name` to the Node you want:

```bash
kubectl -n joulie-system scale deploy/joulie-controller-manager --replicas=0
kubectl apply -f examples/12-governor-presets/nodetwin-governor.yaml
kubectl get nodetwin mi300x-1 -o jsonpath='{.status.controlStatus.cpuGovernor}{"\n"}'   # result applied, applied conservative
kubectl -n joulie-system scale deploy/joulie-controller-manager --replicas=1
```

Once the controller manager is back, it drops the CPU preset (a GPU node), and the agent puts the baseline back: `cpuGovernor.result` `restored`, `applied` `schedutil`.

## 6. Turn it off

```bash
helm -n joulie-system upgrade joulie charts/joulie --reuse-values --set controllerManager.env.GOVERNOR_CONTROL=off
```

Every governor field leaves the `NodeTwin`s, and each agent writes its baseline back to what it changed: the results turn `restored`, with `applied` the baseline (`schedutil`, `powersave epp=balance_performance`, `auto`, `unlocked`), and the files read it again. The agent also restores on exit (`GOVERNOR_RESTORE_ON_EXIT=true`), so a rolling update of the agent puts the baseline back before the new agent applies the preset again. Remove the cluster with `make simcluster-down`.

## On a real cluster

- On an existing install, apply the CRDs first (`kubectl apply -f charts/joulie/crds/`): `helm upgrade` never updates them, and an older CRD prunes the governor fields.
- Install with the same `values.yaml` only after reading each node's presets in `NodeHardware`: with both gates on, the agents change the governors of every managed node. `GOVERNOR_CONTROL=cpu` turns on the CPU side alone.
- The agent writes through the `/host-sys` mount as root, as the chart's DaemonSet runs it; an agent without those privileges reports `refused`. On NVIDIA nodes discovery runs `nvidia-smi`; a node where it does not answer lists no GPU presets and gets none.
- The results, retries and restore rules are in [Governor control](https://joulie-k8s.github.io/Joulie/docs/architecture/agent/#governor-control).
