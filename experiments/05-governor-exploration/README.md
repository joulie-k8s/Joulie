# Experiment 05: governor exploration

> **To be rerun (2026-10-28).** Tier A ran on 2026-10-14, before later changes to the simulator: the CPU model (`SIM_BETA_FREQ`, a CPU on a measured curve) and the H100 NVL's default limit. An audit also found that its acceptance pools idle cells, drops timed-out cells and treats seeds of a deterministic emulator as replicates. The numbers below are not reproducible from this tree; Tier A is being rerun.

CPU frequency governors and GPU clock governors as an energy dimension of
their own, next to power caps. Joulie writes them as vendor-neutral presets
(`spec.cpu.governor`, `spec.gpu.governor` of the NodeTwin); the agent maps
each preset to the native words of the node's CPU frequency driver and GPU
family (`pkg/governor`). This experiment asks which presets lower energy to
solution, at which cost in time, and which conclusions survive the
parameters the simulator assumes.

Docs page: [Governor exploration](../../website/content/en/docs/experiments/governor-exploration.md).

> **Checked (2026-10-23, #89):** Tier A on one CPU profile (`amd-epyc-9534-2s-energy-only`), one GPU profile (`amd-radeon-pro-w7900-4gpu`), one seed and the 50 % steady load ran 1022 cells with none failed, and Tier B arm `all` passed its governor checks (`TestSimclusterE2E`, 231 s).

## Questions

| ID | Question | Decides |
|---|---|---|
| Q1 | Per CPU driver, which preset minimises energy to solution at each load level, and what does it cost in time? | CPU enabled defaults |
| Q2 | Per GPU family and workload kind, energy and time of `auto`, `sustained`, `efficient` (50 to 90 %), `low`, `peak`, alone and with the GPU cap at 100 % and 60 %? | GPU enabled defaults, ceiling default |
| Q3 | With a receiver gate, how much do multi-GPU jobs gain from `sustained`, and at which idle cost (`dataPathHoldW`) does it stop paying? | which GPU models to opt in to `sustained`, and the break-even to measure on hardware |
| Q4 | At cluster level, does Joulie with governors beat caps only on energy without breaking the performance class? | release decision |
| Q5 | Which conclusions survive the assumed parameters? | what must be measured before trusting a default |

## Acceptance rule

A default is adopted when, in Tier A and in Tier B, it lowers energy to
solution against the baseline arm with a performance-class p95 slowdown at
or below 5 % and an eco-class p95 slowdown at or below 20 %, and Q5 does not
flip its sign.

| Metric | Source |
|---|---|
| energy to solution, J (host, CPU, GPU) | `hw.host.energy`, `hw.energy` |
| makespan; job slowdown p50, p95 against the `performance` or `auto` arm | the job ledger (Tier A), the workload ledger (Tier B) |
| energy-delay product | derived (`edp_js`) |
| idle energy | the idle cells (Tier A), `hw.energy` while no job runs (Tier B) |
| delivered peer bandwidth | `peer_gbps` (Tier A); `rate(hw_gpu_io_bytes_total{joulie_hw_gpu_io_path="peer",network_io_direction="receive"}[5m])` (Tier B) |

## Layout

| Path | What |
|---|---|
| `configs/tier-a.yaml` | Tier A grid: E-G1 (CPU) and E-G2 (GPU), 2 seeds |
| `configs/tier-a-sensitivity.yaml` | Q5: each assumed fact at a low, mid and high value, one at a time, on a smaller grid; the receiver gate forced on |
| `configs/tier-b-*.yaml` | Joulie values of each Tier B arm, after `tier-b-common.yaml` |
| `scripts/run_tier_a.sh` | builds `simulator/cmd/govsweep`, runs a config, summarises it |
| `scripts/run_tier_b.sh` | one arm on a fresh simulated cluster, the governor checks of the harness, then removes the cluster |
| `analysis/summarize.py` | the Markdown summary of a Tier A run (standard library only) |
| `analysis/test_summarize.py` | its tests: `python3 -B -m unittest experiments/05-governor-exploration/analysis/test_summarize.py` |
| `hardware/` | the measurements on real hardware that replace the assumed facts, one command each |

Results are written outside the repository, under
`${XDG_CACHE_HOME:-~/.cache}/joulie/experiments/05/`.

## Tier A: offline sweep (no Kubernetes, virtual time)

```bash
experiments/05-governor-exploration/scripts/run_tier_a.sh                     # configs/tier-a.yaml
experiments/05-governor-exploration/scripts/run_tier_a.sh experiments/05-governor-exploration/configs/tier-a-sensitivity.yaml
```

`govsweep` renders one emulated node (`simulator/pkg/hwemu`) per cell, in its
own directory, and runs every cell in parallel. For each cell it reads the
node's cpufreq, amdgpu and `nvidia-smi` facts as the agent's discovery does,
plans the arm's preset with `pkg/governor`, and writes the plan's sysfs
attributes or runs its `nvidia-smi` commands against the node's fake tools,
reading each back. A native word (a governor name, an EPP value, an amdgpu
level, a clock lock) in a config is refused. GPU caps are set with
`nvidia-smi -pl` and, on AMD, `rocm-smi --setpoweroverdrive`, which writes the
`power1_cap` the agent writes directly, once the job holds its GPUs.

| Axis | Values |
|---|---|
| CPU profiles | `amd-epyc-9534-2s-energy-only` (acpi-cpufreq), `intel-xeon-6530-2s-rapl` (intel_cpufreq), `intel-xeon-2s-rapl` (intel_pstate active, HWP), `amd-epyc-2s-hsmp` (amd-pstate-epp), `intel-xeon6-2s-tpmi` |
| CPU arms | the profile's own governor (nothing written), `performance`, `balanced`, `efficiency`, each with boost on and off |
| CPU loads | idle; steady 10, 30, 50, 70, 100 % of every logical CPU in each class; bursty (`cpu.io_bound`, 30 bursts of 50 ms every 1 s, up to 100 ms late) |
| CPU classes | `cpu.compute_bound`, `cpu.memory_bound`, `cpu.mixed`, `cpu.io_bound` |
| GPU profiles | `amd-radeon-pro-w7900-4gpu`, `amd-instinct-mi300x-8gpu`, `nvidia-h100-sxm-8gpu`, `nvidia-h100-nvl-8gpu`, `nvidia-l40s-4gpu` |
| GPU arms | `auto`, `sustained`, `efficient` at 50, 60, 70, 75, 80, 90 %, `low`, `peak`; cap 100 % and 60 % |
| GPU workloads | `gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound` on 1 GPU; 2 and 4 GPUs with a ring of peer copies at intensity 0.1, 0.3, 0.6; the same from the host or a NIC; 4 GPUs gathering into an idle GPU at 0.1, 0.3, 0.6; an idle node |
| Seeds | 2 |

A job does a fixed amount of work: what it does at the reference (no cap,
`performance` or `auto`, nothing slowed) in 60 s. It progresses at the
simulator's v1 workload rate (`simulator/pkg/workload.Rates`) under the
throughput the node reports after each step: `CPUThroughput` against a
governor-independent reference, `GPUThroughput` under the cap and the
compute ceiling, and the rate its transfers get over the rate they demand.
Energy to solution is the node's ledger from the job's start to its end, so
idle GPUs of the node and the platform count. The baseline arm is
`performance` with boost on (CPU) and `auto` with no cap (GPU).

Outputs per run: `tier-a-cpu.csv` and `tier-a-gpu.csv`, one row per cell
(columns in `simulator/cmd/govsweep/output.go`), `tier-a-cpu.otlp.jsonl` and
`tier-a-gpu.otlp.jsonl` (one OTLP JSON metrics export per cell, the node at
its end under the `pkg/otel/semconv` names, host name = cell id),
`govsweep.log` and `summary.md`.

## Tier B: the simulated cluster, Joulie in the loop

```bash
SIMCLUSTER_NAME=joulie-sim-gov experiments/05-governor-exploration/scripts/run_tier_b.sh all
```

| Arm | Values |
|---|---|
| `off` | no Joulie: deployed as `caps-only`, then removed with its NodeTwins, NodeHardwares, power-profile labels and every limit its agents wrote (as `scripts/simcluster/scenario.sh` does for `joulie.enabled: false`); the baseline the other arms are compared with, so it runs no GS check |
| `caps-only` | Joulie as released: caps, no governor (`GOVERNOR_CONTROL=off`) |
| `cpu`, `gpu`, `all` | caps plus the governor presets of `GOVERNOR_CONTROL`, written by every agent (`GOVERNOR_WRITES=true`) |
| `all-sustained` | `all`, with every GPU model of the spec opted in to `sustained` on performance Nodes through `GPU_GOVERNOR_MODEL_PRESETS_JSON` |
| `all-sustained-gate-on` | `all-sustained`, with the simulator's receiver gate on for every GPU (`emulate.receiverGate: "on"`, `SIM_GPU_RECEIVER_GATE`) instead of each profile's own, which is off in every builtin profile |

The cluster spec is `tests/simcluster/specs/governor.yaml`: two Nodes per
CPU driver and per GPU family, so that with `STATIC_HP_FRAC=0.1` every
family has one Node in performance and one in eco. The script creates the
cluster itself, refuses any kube context but the one it created, runs the
governor checks of `tests/simcluster/e2e_test.go` (presets per
profile in NodeHardware, the result each preset gives, the native word in
the emulated files, and the baseline back after an upgrade to
`GOVERNOR_CONTROL=off`), and removes the cluster unless `SIMCLUSTER_KEEP=1`.
Workload runs per arm (`make simcluster-scenario`, utilization raised to at
least 50 % first, `static_partition` and `queue_aware_v1`) are the next step
of Q4 and are not part of this package.

## What is assumed

Every number the conclusions rest on that no measurement backs is a
simulator parameter tagged `assumed:` in its profile (`simulator/pkg/hwemu/profiles/`):

| Fact | Where | Swept in Q5 |
|---|---|---|
| `idleGovernorDeltaW`: idle power while a governor holds the top frequency | `physics.cpu` | 0, 2, 5 % of idle |
| `eppGain`, `eppCeilingDrop`: how EPP moves frequency | `physics.cpu` | 0.5, 1, 2; 0, 0.2, 0.25 |
| `dataPathHoldW`: idle power of data-path clocks held at their maximum (`sustained`) | `physics.gpu` | 1, 10, 50 W |
| `gatingOffIdleW`: idle power with idle gating off (`peak` on AMD) | `physics.gpu` | 5, 20, 50 W |
| `clockBeta`: exponent of the compute clock in dynamic power | `physics.gpu` | 1.0, 1.5, 2.5 |
| `memoryKneeFrac`: compute clock fraction below which memory-bound work slows | `physics.gpu` | 0.4, 0.65, 0.87 |
| receiver gate `slowFraction`: the link share an idle or low receiver lets in | `gpus.transfer.receiverGate` | 0.5, 0.7 (neutral 1.0 in the repository) |
| `autoHoldS`: how long `auto` holds the data path after a kernel | `gpus.dpm` | 2, 10, 60 s |
| class intensities (memory, IO) of each workload class | `configs/tier-a.yaml` | no |
| the job model: v1 rates, ring transfers, one job per node | `simulator/cmd/govsweep` | no |

The receiver gate is off, with neutral fractions, in every repository
profile; Tier A therefore shows no gate effect, and `tier-a-sensitivity.yaml`
forces it on with `slowFraction` below 1 to show the shape of Q3. Replacing
the assumptions takes measurements on real hardware: the idle and loaded
power of `sustained` against `auto` per GPU model, whether each GPU family
gates inbound peer copies on an idle receiver, the idle power of an NVIDIA
clock lock floor, and the energy of each CPU governor under steady loads on
each driver.
[`hardware/`](hardware/README.md) runs each of these measurements on one
node with a single command, and restores what it changes.

## Results

See the docs page for the results of the last run and the conditions they
were produced under.
