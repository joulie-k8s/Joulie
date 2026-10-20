# Experiment 05: measurements on real hardware

The simulator rests experiment 05 on facts tagged `assumed:` in the hardware
profiles (`simulator/pkg/hwemu/profiles/`), and on output texts of the fake
`nvidia-smi` and `rocm-smi` that nobody has captured. Each measurement here
settles one of them on a real node. `hwmeasure.sh` runs one measurement per
call, in a privileged pod pinned to the node, puts back everything it
changed, and writes the raw outputs and a summary outside the repository.

Back to the [experiment README](../README.md).

## What a run does

1. **Checks the arguments.** `--kubeconfig`, `--context`, `--node` and
   `--device` are required. A kubeconfig whose path, or the file it
   resolves to, contains `prod` in any case is refused before kubectl
   runs. `KUBECONFIG` is never read: every kubectl call names the file and
   the context.
2. **Checks the node.** A cordoned node (`spec.unschedulable`, or the
   `node.kubernetes.io/unschedulable` taint) is refused unless
   `--allow-cordoned`; use that flag only on a node you cordoned yourself.
   A `NoSchedule` or `NoExecute` taint is refused unless `--allow-taint KEY`
   names it, which also gives the pod a toleration. A running pod that no
   DaemonSet owns and that is not a static pod is refused unless an
   `--expect-pod REGEX` matches its `namespace/name`, or `--allow-other-pods`.
   A Joulie agent on the node is named in the plan: with governor writes or
   caps on, it may write the same knobs, so turn Joulie off for the node first.
3. **Prints the plan** (what it answers, the node, the device, the image by
   digest, what it changes, how it restores, the options, the duration, the
   results directory) and starts only when the node name is typed back, or
   with `--yes`. `--dry-run` prints the plan and the pod and creates nothing.
4. **Runs the pod**: a ConfigMap with `probe.sh` and `peercopy.cu`, and a pod
   from `pod.yaml` with `nodeName` set, privileged, root, the host root read
   only at `/host` and the host `/sys` at `/host-sys`. For a GPU
   measurement the pod requests every allocatable `nvidia.com/gpu` or
   `amd.com/gpu` of the node (`--gpu-request` changes that), so no other GPU
   pod lands there while it runs.
5. **Measures under a restore trap.** The probe (`probe.sh`, PID 1 of the
   pod) first checks the device and its tools and changes nothing if a check
   fails (exit 2). It snapshots every knob it may touch, and before each
   change it appends the value it read to a journal, which also goes to the
   pod log. Its EXIT trap, which also runs on a failure and on TERM, INT and
   HUP, stops the loads, undoes every journal entry last first, reads every
   knob again and compares it with the snapshot.
6. **Collects and cleans up.** On success, failure or Ctrl-C the runner
   asks the probe to stop (its trap restores), copies the output directory,
   runs the comparison once more (`verify-after.txt`), and deletes the pod
   and the ConfigMap. Deleting the pod by hand also restores: the kubelet
   sends TERM and the grace period is 150 s. If the runner itself dies, the
   probe finishes, restores, and waits 15 minutes for its results to be
   collected; the pod's `activeDeadlineSeconds` ends it after that.

Exit status: 0 done, 1 the measurement failed (and was restored), 2 refused
(nothing changed), 3 the restore or its verification failed, 130 interrupted.
On 3 the runner names the files to read; every value read before a change
is in `probe.log`.

NVIDIA clock locks have no getter. A lock is undone with `-rgc` or `-rmc`,
journaled before the lock is set, so an operator's own lock on that GPU is
cleared too: check that none is set before a lock measurement. The reset is
verified by its exit status and by the clocks after it.

## Requirements

- On the workstation: bash 4.4 or newer, kubectl, jq.
- On the cluster: a namespace (`--namespace`, default `default`) that admits
  privileged pods with hostPath volumes, and pulls from the registries of
  `images.env` (or `--image REF@sha256:...` and `--image-pull-secret`).
- The images, each named by tag and digest in `images.env`:

| Name | Image | Tools |
|---|---|---|
| `tools` | stress-ng 0.22.01 on Debian 13 | bash, coreutils, util-linux, stress-ng; nvidia-smi and rocm-smi from the host through chroot |
| `cuda` | `nvidia/cuda:12.4.1-devel-ubuntu22.04` (about 4 GB) | nvcc, to build `peercopy.cu` in the pod; libcuda and nvidia-smi from the NVIDIA container runtime (`--runtime-class nvidia` where it is not the default) |
| `rocm` | `rocm/dev-ubuntu-22.04:6.2.4` (about 1 GB) | hipcc and rocm-smi; the devices come with the privileged pod |

`peercopy.cu` builds with nvcc and hipcc from one source (`--offload-arch`
picks the AMD targets, gfx90a, gfx942 and gfx1100 by default). It times
`cudaMemcpyPeer`/`hipMemcpyPeer` copies into one GPU, devices named by PCI
bus id, and runs a memory-bound kernel for the loaded arms. Frequency traces
use a tracefs instance of their own, so no trace-cmd is needed.

## The measurements

Set once:

```bash
KC=~/.kube/<cluster>.yaml CTX=<context> NODE=<node>
```

`experiments/05-governor-exploration/hardware/hwmeasure.sh list` prints
every measurement, its image and its options. Durations are the runner's
estimate with the default options, from the pod's start, the build of
`peercopy.cu` included; the first pull of the `cuda` or `rocm` image comes
on top.

| Measurement | Settles | Changes | Time |
|---|---|---|---|
| `w7900-level-power` | `physics.gpu.dataPathHoldW` (W7900), `gatingOffIdleW` with `--levels high,profile_peak` | perf level `high` | 11 min |
| `fabric-floor` | a cheaper `sustained` mapping (W7900) | perf level, `pp_dpm_fclk` | 6 min |
| `hopper-receiver-gate` | `gpus.transfer.receiverGate` (H100) | `-lgc` on the receiver | 4 min |
| `instinct-receiver-gate` | `gpus.transfer.receiverGate` (MI300X) | nothing by default | 4 min |
| `l40s-receiver-gate` | `gpus.transfer.receiverGate` (L40S), the L40S opt-in | `-lmc` on the receiver | 4 min |
| `nvidia-lock-floor-idle` | `gpus.clocks.lockFloorAtIdle`, idle cost of a floor; `dataPathHoldW` (L40S) with `--clock memory` | `-lgc` (or `-lmc`) floor | 4 min |
| `nvidia-lock-lifetime` | `gpus.clocks.locksResetOnDeinit`, the event reason of a lock | `-lgc` | 3 min |
| `cpu-governor-energy` | `physics.cpu.idleGovernorDeltaW`, Q1 (EPYC acpi-cpufreq) | `scaling_governor` | 15 min |
| `schedutil-steps` | the schedutil step rule of `phys.GovernorStep` | one policy's governor | 1 min |
| `conservative-steps` | the conservative rule on a 3-entry table | one policy's governor | 2 min |
| `hwp-epb` | `cpufreq.hwp` (Xeon 6530), whether EPP can be a knob | nothing | 1 min |
| `smi-formats` | the fake tools' output texts | `-lgc` then `-rgc` | 1 min |
| `nvidia-pl-mig` | `-pl` on a MIG-enabled GPU: exit status and effect | `-pl` | 1 min |
| `nvidia-pl-na` | the exit status of a refused `-pl`, with and without `--error-on-warning` | `-pl` to the current limit | 1 min |
| `amdgpu-cap-range` | `gpus.limits.minW` (MI300X `power1_cap_min`) | nothing | 1 min |

### `w7900-level-power`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh w7900-level-power --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device all
```

Answers the key unknown of `sustained` on the W7900: GPU power at level
`high` against `auto`, idle and under a memory-bound kernel. Each arm samples
`power1_average` (or `power1_input`) and the starred `pp_dpm_*` clocks every
second for 120 s (`--sample-s`), 10 s after each level change
(`--settle-s`); `--device` is one bus id or `all`. `--load none` skips the
loaded arms and runs on the `tools` image; `--levels high,profile_peak` adds
the profile level. Changes `power_dpm_force_performance_level` of each
device to each level, idle and loaded; restores `auto`, the level it read (a
device not at `auto` is refused), after each, and reads it back. Settles
`physics.gpu.dataPathHoldW` in `amd-radeon-pro-w7900-4gpu.yaml` (assumed
10 W): the median over GPUs of the idle power at `high` minus `auto`; with
`profile_peak`, also `gatingOffIdleW` (assumed 20 W), its idle power minus
`high`'s. The loaded delta shows whether a level costs anything under load.

### `fabric-floor`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh fabric-floor --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <receiver bus id> --peer <source bus id>
```

Idle power (60 s, `--sample-s`) and 12 peer copies of 64 MiB into the
receiver at `auto`, at `high`, and under `manual` with only the top
`pp_dpm_fclk` level forced. Changes the receiver's level and its fclk mask;
restores `auto`, which also drops the mask. If the fclk arm copies as fast
as `high` at a lower idle power, the W7900 `sustained` mapping in
`pkg/governor` (`PlanAMD`, today `high`) has a cheaper alternative; that is
a code change with its mapping tests, not a profile edit.

### `hopper-receiver-gate`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh hopper-receiver-gate --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <receiver bus id> --peer <source bus id>
```

64 MiB `cudaMemcpyPeer` x12 (`--bytes`, `--iters`) from the source into a
receiver that is idle (`default`), runs its own memory-bound kernel
(`warm`), is locked at the top supported graphics clock (`lgc-max`) and at
the lowest (`lgc-min`); `--arms` picks a subset. The copies are issued from
the source's context (`--issuer dst` for the other), after the receiver sat
idle for 10 s (`--cold-s`). `nvidia-smi` samples pstate, clocks, power and
the event reasons of both GPUs every 10 ms. Changes the receiver's graphics
clock lock during the two lock arms; restores with `-rgc` after each arm and
in the trap. The summary gives the median rate per arm and the idle arm
over the best one: about 1 means no gate (`receiverGate.enabled: false`,
now measured, and `sustained` stays `equivalent` on Hopper); clearly below,
a gate with that `slowFraction`, and a graphics floor to consider for
`sustained`.

### `instinct-receiver-gate`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh instinct-receiver-gate --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <receiver bus id> --peer <source bus id>
```

The same copies with `hipMemcpyPeer` into an AMD Instinct receiver idle
under `auto` (`idle`) against one running a memory-bound kernel (`busy`),
the receiver's starred clocks and power sampled every 50 ms. Changes
nothing with the default arms; `--arms idle,busy,high` adds an arm at level
`high` on families that accept it (the MI300X rejects it), restored to the
level read. Settles `gpus.transfer.receiverGate` of
`amd-instinct-mi300x-8gpu.yaml`: whether the W7900's gate generalises.

### `l40s-receiver-gate`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh l40s-receiver-gate --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <receiver bus id> --peer <source bus id>
```

The matched receiver test on an L40S pair: the copies of
`hopper-receiver-gate` into a cold receiver under `default`, `warm` and a
memory clock lock at the top supported clock (`lmc-max`), the receiver's
`clocks.mem` sampled every 10 ms. A buffer on the receiver needs a context
there; "cold" means the receiver ran nothing for `--cold-s` seconds before
the first copy, issued from the source. Changes the receiver's memory clock
lock during `lmc-max`; restores with `-rmc`. Settles
`gpus.transfer.receiverGate` of `nvidia-l40s-4gpu.yaml`, the condition for
listing the L40S in `GPU_GOVERNOR_MODEL_PRESETS_JSON` for `sustained`.

### `nvidia-lock-floor-idle`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh nvidia-lock-floor-idle --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <bus id>
```

Idle power and clocks (60 s at 1 s, `--sample-s`) with no lock, under a
graphics lock from the supported clock nearest 75 % of the maximum to the
maximum (`--lock MIN,MAX`), and after the reset. The GPU must run no compute
process. Changes the graphics clock lock; restores with `-rgc` and checks
that the idle clock falls below the floor again. Settles
`gpus.clocks.lockFloorAtIdle` of the NVIDIA profiles (assumed true) and the
idle cost of a floor, which `peak` and `low` pay. With `--clock memory` it
locks the memory clock at its maximum instead (`-lmc`, reset with `-rmc`),
the lock `sustained` writes on GDDR parts: on an L40S that settles
`physics.gpu.dataPathHoldW` of `nvidia-l40s-4gpu.yaml`. Runs on the `tools`
image with the host's `nvidia-smi`.

### `nvidia-lock-lifetime`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh nvidia-lock-lifetime --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <bus id>
```

Locks the graphics clock at the supported clock nearest 60 % of the maximum
(`--lock`), runs a memory-bound load for 20 s (`--load-s`) sampled every
100 ms with `clocks_event_reasons`, waits 15 s (`--deinit-s`) with no client
on the GPU, runs the load again, then resets and loads once more. Changes
the lock; restores with `-rgc` and checks that the clock under load rises
above the lock. Settles `gpus.clocks.locksResetOnDeinit` (assumed false; the
answer depends on persistence mode, which the summary names) and the
readback rule of a lock: which event reason is active while it holds.

### `cpu-governor-energy`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh cpu-governor-energy --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device all-cpus
```

Package energy from RAPL `energy_uj` (package zones and psys, never their
subzones) under `performance`, `schedutil` and `conservative`
(`--governors`) at steady 0, 30, 70 and 100 % loads of every CPU
(`--loads`, stress-ng `--cpu-load`), 60 s per cell (`--measure-s`) after
10 s (`--settle-s`), with the stress-ng work done per cell. The node must be
otherwise idle: run it on a node you drained, since a pod that lands during
the run spoils the cells. Changes `scaling_governor` of every policy;
restores each one and compares the governors, frequency limits, EPP and
boost with the snapshot. Settles `physics.cpu.idleGovernorDeltaW` of the
EPYC profiles (`amd-epyc-9534-2s-energy-only.yaml`,
`amd-epyc-2s-energy-only.yaml`, assumed 0): idle package power under
`performance` minus the lowest demand-driven governor. The loaded cells
answer Q1 on that driver.

### `schedutil-steps`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh schedutil-steps --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device cpu3
```

One CPU loaded at 35 % (`--loads`) for 30 s (`--measure-s`) with stress-ng
pinned to it, its `power:cpu_frequency` events recorded in a tracefs
instance of its own and `scaling_cur_freq` sampled every 100 ms. Changes
that CPU's policy governor to `schedutil` only when it is not already, and
creates the tracefs instance; restores both. The summary puts the observed
residency next to the rule hwemu implements (1.25 x max x load, resolved to
the lowest table entry at or above it, `phys.GovernorStep`); a mismatch is
a change to that rule with its tests, not a profile edit.

### `conservative-steps`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh conservative-steps --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device cpu3
```

The same trace under `conservative` at 30 and 70 %. Shows whether steps of
`freq_step` (5 %) of the maximum move between the entries of a 3-entry table
or stay at the lowest. Changes and restores as `schedutil-steps`. Settles
the conservative rule of `phys.GovernorStep` on such tables.

### `hwp-epb`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh hwp-epb --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device all-cpus
```

Read only: the `hwp`, `hwp_epp` and `epb` flags of every CPU in
`/proc/cpuinfo`, `energy_perf_bias` and the policy's driver, governor and
EPP files, the intel_pstate and amd_pstate status. Settles `cpufreq.hwp` of
`intel-xeon-6530-2s-rapl.yaml` (assumed false) and whether EPP can ever be a
knob on that CPU (it needs HWP and an `energy_performance_preference` file).

### `smi-formats`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh smi-formats --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <bus id>
```

The text the real tools print: `nvidia-smi -h`, `--help-query-gpu` (the
query field names), `-q -d SUPPORTED_CLOCKS`, `-q -d CLOCK,PERFORMANCE,POWER`,
the clock query of the agent's discovery (`pkg/governor/discover`) under both names of its reasons field, the limits query, and the
confirmation of `-lgc <min>,<max>` and `-rgc` on one GPU; on AMD nodes
`rocm-smi --showclocks [--json]`, `--showperflevel [--json]` and
`--showpower` (`--image rocm` for the image's rocm-smi when the host has
none). `--device all` or `--read-only` runs only the queries;
`--with-lmc` adds `-lmc` and `-rmc` (not on Hopper). Changes the graphics
lock to the full supported range for a moment; restores with `-rgc`. Each
output replaces the texts `simulator/pkg/hwemu/smi` marks as assumed, and
`SUPPORTED_CLOCKS` replaces the assumed clock lists of the NVIDIA profiles.
Bus ids, serial numbers and UUIDs in the outputs stay out of the
repository.

### `nvidia-pl-mig`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh nvidia-pl-mig --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <bus id>
```

On a GPU whose `mig.mode.current` is `Enabled` (else refused): the limits,
then `nvidia-smi -i <bus> -pl` at 90 % of the current limit, no lower than
the minimum (`--watts`), its exit status, stdout and stderr, and the limits
2 s later. Restores the limit read before and reads it back. Settles what
the fake `nvidia-smi` and the agent's GPU cap path do with `-pl` on a
MIG-enabled GPU.

### `nvidia-pl-na`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh nvidia-pl-na --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device <bus id>
```

On a GPU whose `power.min_limit` and `power.max_limit` print a token such as
`[N/A]` (else refused): `-pl` at the current limit, then the same with
`--error-on-warning`, each with its exit status and text. A set to the
current limit changes nothing even if it is accepted; the limit read before
(or the default one) is written back in any case. Settles the exit statuses
of a refused `-pl` in the fake `nvidia-smi` (`nvSetPowerLimit`).

### `amdgpu-cap-range`

```bash
experiments/05-governor-exploration/hardware/hwmeasure.sh amdgpu-cap-range --kubeconfig "$KC" --context "$CTX" --node "$NODE" --device all
```

Read only: `power1_cap`, `power1_cap_min`, `power1_cap_max`,
`power1_cap_default` and the power reading of every amdgpu device, and its
runtime power state (a suspended GPU refuses reads). Settles
`gpus.limits.minW` of `amd-instinct-mi300x-8gpu.yaml` (assumed 0) and checks
`defaultW` (assumed equal to the maximum).

## From a result to a profile change

Each run directory,
`${XDG_CACHE_HOME:-~/.cache}/joulie/experiments/05/hardware/<UTC>-<measurement>/`,
holds `summary.md`, `plan.txt`, `pod.yaml`, `node.json`, `pods.json`,
`run.log`, `probe.log` and `raw/` (the snapshot, the journal,
`restore.txt`, `verify.txt`, every sample and tool output). The summary
ends with the profile line the result supports, with `<id>` for its source.

1. Add a source to the profile's `sources:` block that describes the
   measurement, never the machine:
   `w7900pwr: {kind: measured, ref: "w7900-level-power on a node with <count>x AMD Radeon PRO W7900, <YYYY-MM>", note: "idle power at high minus auto, median over GPUs"}`.
   No node name, host name, cluster name, bus id or site goes into a
   profile, a test or a commit.
2. Set the fact with that source: `dataPathHoldW: {v: <value>, src: "measured:w7900pwr#idle power at high minus auto"}`.
3. Numbers from private clusters stay out of the repository until the user
   approves each one. A receiver gate measured on lab hardware goes into a
   private profile under `SIM_PROFILES_DIR` first; every repository profile
   keeps `receiverGate.enabled: false` until then.
4. A fact that moves a simulated number follows the repository's rule for
   such changes: regenerate the reference golden and add a row to
   `simulator/CHANGES.md`; a mapping or step rule change goes to
   `pkg/governor` or `simulator/pkg/phys` with its tests.

## Tests

```bash
go test ./experiments/05-governor-exploration/hardware/
```

`hwmeasure_test.go` runs both scripts against a fake kubectl that runs the
probe as a local process over a synthetic sysfs tree, with fake stress-ng
and nvidia-smi: the argument checks, the prod kubeconfig refusal, the
cordon, taint and pod refusals, the plan and the dry run, the confirmation,
the restore on a failure, on Ctrl-C and on TERM (CPU governors and an NVIDIA
lock), a knob changed in two arms restored after each, a restore the kernel
refuses (exit 3), a pod that never runs, the read-only measurements leaving
the tree unchanged, the old driver names of the reasons field, the image
digests, and this README naming every measurement.

End to end on a throwaway simulated cluster, where an emulated node stands
in for the hardware. The emulated trees live on the cluster's infra Node,
`/var/lib/joulie-hwemu/<pool>-<index>`, so the pod runs there, reads a tree
through `--host-root` (which also turns the kernel trace off), and the infra
Node's own pods are named as expected. The read-only measurements and the
CPU governor ones run there; the GPU ones need a GPU runtime.

```bash
export SIMCLUSTER_NAME=joulie-sim-hw SIMCLUSTER_SPEC=tests/simcluster/specs/smoke.yaml
scripts/simcluster/up.sh && scripts/simcluster/deploy.sh
KC=~/.cache/joulie/simcluster/$SIMCLUSTER_NAME/kubeconfig
E='(joulie-system|joulie-sim|monitoring|kube-system)/.*'
H=experiments/05-governor-exploration/hardware/hwmeasure.sh
$H amdgpu-cap-range --kubeconfig "$KC" --context default --node $SIMCLUSTER_NAME-infra --device all \
  --host-root /var/lib/joulie-hwemu/mi300x-0 --expect-pod "$E" --yes
$H conservative-steps --kubeconfig "$KC" --context default --node $SIMCLUSTER_NAME-infra --device cpu1 \
  --measure-s 5 --host-root /var/lib/joulie-hwemu/mi300x-0 --expect-pod "$E" --yes
scripts/simcluster/down.sh
```

## Files

| File | What |
|---|---|
| `hwmeasure.sh` | the runner: checks, plan, pod, collection, cleanup |
| `probe.sh` | the measurements, run in the pod as PID 1, with the journal and the restore trap |
| `peercopy.cu` | peer copy timing and a memory-bound load, for nvcc and hipcc |
| `pod.yaml` | the pod template |
| `images.env` | the images, by tag and digest |
| `hwmeasure_test.go` | the tests |
