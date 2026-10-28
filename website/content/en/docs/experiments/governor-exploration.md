---
title: "Governor Exploration"
---

This page reports experiment 05, CPU and GPU governor presets as an energy
dimension of their own, next to power caps:

- [`experiments/05-governor-exploration/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/05-governor-exploration)

Joulie writes a governor as a vendor-neutral preset in the NodeTwin
(`spec.cpu.governor.preset`: `performance`, `balanced`, `efficiency`;
`spec.gpu.governor.preset`: `auto`, `sustained`, `efficient`, `low`, `peak`),
and the agent maps it to the native words of the node's CPU frequency driver
and GPU family. Governor control is off by default (`GOVERNOR_CONTROL`,
`GOVERNOR_WRITES`, [Configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}})).

All results on this page come from the simulator, Joulie's test bed:
emulated nodes (`simulator/pkg/hwemu`) whose governor and GPU clock physics
rest on parameters tagged `assumed:` in their profiles. They show the shape
of each trade-off under those assumptions, not measured savings. Measuring
those parameters on real nodes is [#106](https://github.com/joulie-k8s/Joulie/issues/106), part of the milestone
[Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

## Questions

| ID | Question | Decides |
|---|---|---|
| Q1 | Per CPU driver, which preset minimises energy to solution at each load level, and what does it cost in time? | CPU enabled defaults |
| Q2 | Per GPU family and workload kind, energy and time of `auto`, `sustained`, `efficient` (50 to 90 %), `low`, `peak`, alone and with the GPU cap at 100 % and 60 %? | GPU enabled defaults, ceiling default |
| Q3 | With a receiver gate, how much do multi-GPU jobs gain from `sustained`, and at which idle cost does it stop paying? | which GPU models to opt in to `sustained` |
| Q4 | At cluster level, does Joulie with governors beat caps only on energy without breaking the performance class? | release decision |
| Q5 | Which conclusions survive the assumed parameters? | what must be measured before trusting a default |

A default is judged on each profile. On a profile it passes when, in
Tier A and in Tier B, it lowers energy to solution against the baseline arm
with a performance-class p95 slowdown at or below 5 % and an eco-class p95
slowdown at or below 20 %, and at every value Q5 gives an assumed fact the
energy change stays negative and the p95 slowdown within the limit. Burst
latency counts against the same limit once the frequency ramp is measured on
real nodes ([#106](https://github.com/joulie-k8s/Joulie/issues/106)); until then it is reported and not judged. A GPU default is adopted for the GPU models it
passes on, since `GPU_GOVERNOR_MODEL_PRESETS_JSON` sets presets per model; a
CPU default applies to every driver, so it must pass on every CPU profile.

In Tier A the energy change is the median over the profile's job cells, one
value per cell with its two seeds averaged (the emulator is deterministic);
idle cells, with no job to slow down, are reported apart, and bursty cells,
timed on a fixed window, with their burst latency. A cell that timed out
counts with its slowdown at the limit, a lower bound, so a pass is not
established on its profile.

## Method

**Tier A** is an offline sweep, `simulator/cmd/govsweep`, in virtual time:
one fresh emulated node per cell, 8 cells at a time. Each arm is a
preset, which govsweep plans with `pkg/governor` on the facts the node's
emulated sysfs and fake `nvidia-smi` show, exactly as the agent's discovery
reads them (`pkg/governor/discover`, shared with the agent): the L40S
architecture is `ada`, and an AMD device bound to a driver other than
`amdgpu` is skipped. It writes as the agent would; a native word in a config
is refused. An arm a profile cannot express is decided once per arm: its
other cells report `unsupported` without running. A job does at the reference (no cap, `performance` or `auto`) 60 s
of work and progresses at the simulator's v1 workload rate under the
throughput the node reports after each step. A GPU cap the board cannot
take (outside its `minW` to `maxW`) is not run, and a cap that does not take
on a GPU the job holds fails the cell. The bursty cells step the node
every 1 ms without its file I/O (`StepUnobserved`): nothing reads or
writes the tree during the bursts, and the state after each step is the
one a full step gives. Energy to solution is the node's whole energy from
the job's start to its end. The grid
(`configs/tier-a.yaml`): five CPU profiles, one per frequency driver, under
the profile's own governor and each CPU preset with boost on and off, idle,
at steady 10 to 100 % loads of four classes and under I/O-bound and
compute-bound bursts; five GPU
profiles under every GPU preset (`efficient` at 50 to 90 %), with the cap at
100 % and 60 %, idle and with jobs of three GPU classes on 1, 2 and 4 GPUs,
with peer, host and gather transfers; two seeds.

**Tier B** runs one arm on a simulated cluster
(`tests/simcluster/specs/governor.yaml`, two Nodes per CPU driver and GPU
family) with Joulie in the loop, and the harness checks the presets each
agent lists, the result each preset gives on each profile and Node state,
the native word in the emulated files, and every file back at its baseline
after an upgrade to `GOVERNOR_CONTROL=off`.

## Results

`analysis/summarize.py` regenerates these tables from a run's CSVs, and
`scripts/run_tier_a.sh` keeps in the run directory the code it built from
(`code.txt`, `code.diff`) and the config.

Run of 2026-10-28, `configs/tier-a.yaml`: 11,410 cells (1,610 CPU, 9,800
GPU). 2,156 GPU cells do not run: their preset is one the profile cannot
express, or, on the W7900, a 60 % cap is below the board's 241 W floor. 72
`low` cells on the L40S did not finish within 10 times the reference and
are left out. The two seeds give the same energy in every steady and GPU
cell, and bursty cells differ by at most 0.1 %: hwemu has no randomness of
its own, so the seeds repeat a cell rather than sample it. Changes are
medians over profiles, classes and seeds against the baseline arm
(`performance` with boost on; `auto` with no cap); energy is the host's
energy to solution, time the job's makespan.

### E-G1: CPU presets (Q1)

| Load | `balanced` | `efficiency` | `efficiency`, boost off | `performance`, boost off |
|---|---|---|---|---|
| idle | 0 % energy | 0 % | 0 % | 0 % |
| steady 10 % | +4.3 % energy, +10.3 % time | +4.5 %, +10.8 % | -1.3 %, +11.6 % | -5.1 %, +5.3 % |
| steady 30 % | +0.7 %, +8.5 % | -0.4 %, +5.9 % | -16.2 %, +17.2 % | -18.3 %, +7.9 % |
| steady 50 % | -0.5 %, +3.1 % | -0.5 %, +2.5 % | -24.0 %, +13.9 % | -23.9 %, +10.5 % |
| steady 70 % | -0.6 %, +2.4 % | -0.6 %, +2.4 % | -27.7 %, +16.1 % | -27.3 %, +13.4 % |
| steady 100 % | -7.2 %, +2.1 % | -10.6 %, +3.1 % | -31.7 %, +22.0 % | -30.2 %, +17.8 % |
| bursty, I/O-bound (500 ms ramp) | -13.3 % energy, +12.0 % burst p95 | -12.8 %, +12.0 % | -13.2 %, +13.7 % | -8.6 %, +7.8 % |
| bursty, compute-bound (500 ms ramp) | -10.6 % energy, +108.8 % burst p95 | -9.3 %, +111.8 % | -10.3 %, +124.5 % | -5.9 %, +48.0 % |

The bursts are 30 of 50 ms every second at full load, timed to 1 ms; under
`performance` with boost on each takes 51 ms. The burst rows are at the
500 ms frequency ramp every profile assumes; Q5 gives them over 5 to 500 ms.

With boost left on, a demand-driven preset saves at most 0.6 % over
`performance` up to 70 % load and costs up to 4.5 % at 10 %: at low loads
it holds a lower frequency, the job runs longer, and the node's other power
runs longer with it (race to idle). At full load the preferences of the EPP
drivers pay, 7 to 11 % at 2 to 3 % more time. Turning boost off is what
saves energy, 16 to 32 % from 30 % load up, at 8 to 22 % more time.

Under bursts the demand-driven presets save 9 to 13 % energy and lengthen
every burst, because each burst starts at a low frequency that ramps up
over `freqRampMS`. Over ramps of 5 to 500 ms (Q5), `balanced` and
`efficiency` lengthen a compute-bound burst by 18 to 112 % and an I/O-bound
one by 4 to 12 %, medians over profiles. The profiles assume 500 ms, the
slow end: the kernel's schedutil recomputes its target at most every
`rate_limit_us`, by default 1.5 times the driver's transition latency or
1 ms, and ondemand samples about every 2 ms
([cpufreq](https://docs.kernel.org/admin-guide/pm/cpufreq.html)); the
delay of the load signal a governor follows is not modelled. Idle power is
the same under every preset because `idleGovernorDeltaW` is 0 in every
profile (assumed).

### E-G2: GPU presets (Q2)

| Arm | compute-bound | memory-bound | bandwidth-bound | idle node |
|---|---|---|---|---|
| `efficient` 90 % | -4.5 % energy, +6.2 % time | -7.9 %, +1.3 % | -8.2 %, +1.0 % | 0 % |
| `efficient` 80 % | -8.6 %, +13.5 % | -15.4 %, +2.6 % | -15.7 %, +2.0 % | 0 % |
| `efficient` 75 % | -10.4 %, +17.5 % | -18.9 %, +3.3 % | -19.4 %, +2.5 % | 0 % |
| `efficient` 60 % | -13.7 %, +35.9 % | -25.7 %, +11.4 % | -26.0 %, +10.2 % | 0 % |
| `sustained` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `peak` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `low` | +32 %, +330 % | +24 %, +233 % | +25 %, +228 % | 0 % |
| `auto`, cap 60 % | -7.1 %, +30.2 % | -11.0 %, +4.6 % | -10.8 %, +3.7 % | 0 % |

`efficient` lowers energy on every family that can express it (MI300X,
H100 SXM and NVL, L40S; the W7900 has no overdrive), most on memory-bound
work, where the compute clock above the knee buys little. `sustained` and
`peak` cost their idle power (`dataPathHoldW`, `gatingOffIdleW`) and gain
nothing while the receiver gate is off. A 60 % cap is refused on the
W7900, whose floor is 241 W, and binds on the L40S only once a job holds
the GPU (persistence mode is off).

### Receiver gate (Q3)

`configs/tier-a-gate.yaml` (90 cells, one seed) forces the gate on, with
`slowFraction` below the repository's neutral 1.0, for a job of four GPUs
that gathers into an idle one (0.3 of the link per sender). The H100 NVL
and SXM are left out: their HBM runs at one clock, so the gate cannot act
on them.

| `slowFraction` | `auto`, gate on against off | `sustained` against `auto`, gate on |
|---|---|---|
| 1.0 (repository) | 0 % | +1.0 to +1.4 % energy, 0 % time |
| 0.7 | +8.6 % energy, +8.6 % time | -6.6 to -7.0 % energy, -7.9 % time |
| 0.5 | +24.0 % energy, +24.0 % time | -18.2 to -18.6 % energy, -19.4 % time |

The ranges are over the W7900 and the L40S; the MI300X cannot express
`sustained`, and the gate costs it the same shares. On the W7900 the gate
adds 10.3 kJ to the job at `slowFraction` 0.5 and 3.7 kJ at 0.7, and
`sustained` gives back 9.7 and 3.1 kJ. What `sustained` costs is the held
data path of the idle receiver: 0.6 kJ per job at the repository's
`dataPathHoldW` of 10 W, the +1.0 to +1.4 % at `slowFraction` 1.0.

### Sensitivity (Q5)

`configs/tier-a-sensitivity.yaml` (25,580 cells, one seed) runs each
assumed fact at a low, mid and high value, one at a time, on Tier A's cells
for the candidate arms; its cells without a variant equal Tier A's row for
row. A fact a profile does not have does not run there: the EPP facts on
the two profiles without EPP, `autoHoldS` on NVIDIA.

- GPU `efficient` 80 % keeps a negative energy change at every value Q5
  sweeps on every GPU model, at least -6.9 to -11.1 % (at `clockBeta` 1.0), and its
  p95 slowdown stays within 20 % at every value, at most 19.2 to 19.9 %
  (at `memoryKneeFrac` 0.87), where its worst cell reaches 21.4 to 22.1 %. 90 % holds too, at least -3.5 % and at most
  +8.3 %. 75 % keeps its saving and is over the slowdown limit at every
  value, as in Tier A.
- CPU `efficiency` keeps a negative energy change at every value on the
  two drivers where it passes Tier A, at worst -0.6 % (`amd-epyc-2s-hsmp`)
  and -0.2 % (`intel-xeon-6530-2s-rapl`). On `amd-epyc-2s-hsmp` its p95
  slowdown exceeds 20 % at `eppCeilingDrop` 0.25 and `eppGain` 0.5 (+22.5 %
  at worst).
- With `idleGovernorDeltaW` at 5 % of idle, the demand-driven CPU presets
  save up to 4.1 % of idle energy; at 0, the repository's value, nothing.
- `freqRampMS` sets the burst cost (E-G1). The steady and job cells step
  every 100 ms, and a ramp no longer than a step completes within it, so
  5 and 50 ms give them the same results.

### Acceptance (Tier A half)

Each candidate on each profile: the median energy change over the
profile's job cells, the p95 slowdown (nearest rank over the cells: the
19th of 20 CPU cells, the 46th of 48 GPU cells) and, in brackets, the
worst cell, against the eco limit of 20 %, which the rule applies to the
p95.

| CPU candidate | `amd-epyc-2s-hsmp` | `amd-epyc-9534-2s-energy-only` | `intel-xeon-2s-rapl` | `intel-xeon-6530-2s-rapl` | `intel-xeon6-2s-tpmi` | Passes |
|---|---|---|---|---|---|---|
| `efficiency` | -2.2 %, +19.2 % (+21.9 %) | -0.5 %, +25.0 % (+35.0 %) | +0.5 %, +21.1 % (+24.4 %) | -0.6 %, +18.5 % (+37.4 %) | -1.3 %, +22.2 % (+25.8 %) | 2 of 5 |
| `balanced` | -1.6 %, +14.9 % (+20.1 %) | -0.6 %, +25.0 % (+35.0 %) | +0.1 %, +16.4 % (+22.3 %) | -0.6 %, +37.4 % (+37.5 %) | -1.3 %, +17.1 % (+23.5 %) | 2 of 5 |
| `efficiency`, boost off | -26.8 %, +45.0 % (+62.3 %) | -19.3 %, +34.3 % (+35.0 %) | -10.4 %, +44.6 % (+61.6 %) | -21.0 %, +54.6 % (+57.9 %) | -22.8 %, +56.2 % (+80.0 %) | 0 of 5 |
| `efficiency`: compute-bound burst p95 | +94.0 % | +70.6 % | +111.8 % | +121.6 % | +126.5 % | not judged |

| GPU candidate | MI300X | H100 NVL | H100 SXM | L40S | Passes |
|---|---|---|---|---|---|
| `efficient` 75 % | -13.8 %, +22.5 % (+25.0 %) | -14.7 %, +22.7 % (+25.3 %) | -18.7 %, +22.5 % (+25.0 %) | -21.0 %, +22.5 % (+25.0 %) | 0 of 4 |
| `efficient` 80 % | -11.2 %, +17.1 % (+19.0 %) | -11.9 %, +17.3 % (+19.2 %) | -15.5 %, +17.6 % (+19.6 %) | -17.2 %, +17.4 % (+19.3 %) | 4 of 4 |
| `efficient` 90 % | -5.8 %, +7.8 % (+8.7 %) | -6.1 %, +7.9 % (+8.8 %) | -8.2 %, +8.3 % (+9.3 %) | -8.8 %, +7.9 % (+8.8 %) | 4 of 4 |
| `efficient` 75 % with a 60 % cap | -12.9 %, +38.9 % (+43.2 %) | -13.8 %, +38.8 % (+43.1 %) | -17.9 %, +38.5 % (+42.7 %) | -20.2 %, +38.5 % (+42.8 %) | 0 of 4 |

Tier B has measured no energy or slowdown (Q4), so the rule adopts no
default; with assumed physics, Q4 waits for measured hardware behaviour
([#106](https://github.com/joulie-k8s/Joulie/issues/106)) and, for a claim, a pilot on a real cluster ([#114](https://github.com/joulie-k8s/Joulie/issues/114)). On the Tier A half and Q5, GPU `efficient` at 80 %, the shipped
default, passes on every GPU model that can express it, at every value Q5
sweeps, with no Tier A cell over 20 %; 90 % passes too, at about half the saving;
75 % fails on every model. The four models agree by construction: they
share the assumed `clockBeta` 1.5 and `memoryKneeFrac` 0.65, and the
compute-bound class spends 80 % of its time on the compute clock (the class
weights in `simulator/pkg/phys/model.go` at the class's assumed memory
intensity, 0.2), so a 20 % lower clock costs it 19 %. Q5 does not vary the
class weights; at a memory intensity of 0 the same job would slow by 21 %,
and a job bound by the compute clock alone by 25 %. Q5 did not run the 60 % cap with `efficient` 75 %, which fails
Tier A anyway. CPU `efficiency`, the shipped CPU default, passes on 2 of the
5 drivers, with a cell over 20 % on both of them, and Q5 holds on one of
them (`intel-xeon-6530-2s-rapl`); a CPU default applies to every driver, so
it does not pass. The rule does not
judge burst latency; at the 500 ms ramp the profiles assume, `efficiency`
lengthens a compute-bound burst by 71 to 127 %.

### Tier B

Arm `all` (`GOVERNOR_CONTROL=all`, `GOVERNOR_WRITES=true`) on a fresh
simulated cluster of 16 Nodes, run on 2026-10-28 on commit `6cc3a1d3`
with experiment 05's changes, before the lock fix in the simulator's
workload tick: every check passes (`TestSimclusterE2E`, GS01 to GS04, 311 s). Every agent lists the
presets its profile maps to, every NodeTwin asks for the preset of its
Node's state (CPU `performance` or `efficiency` on CPU-only Nodes, GPU
`efficient` on eco GPU Nodes that list it, nothing else), every result is
the one the mapping predicts, the emulated files hold the native words of
the 11 results, and after the upgrade to `GOVERNOR_CONTROL=off` all 11 are
`restored` and every file is back at its baseline. Workload runs per arm
(Q4) are not part of this experiment: see [#106](https://github.com/joulie-k8s/Joulie/issues/106) and [#114](https://github.com/joulie-k8s/Joulie/issues/114).

## Reproduce

Tier A runs on one machine, with no Kubernetes:

```bash
experiments/05-governor-exploration/scripts/run_tier_a.sh [config] [out]
```

`config` defaults to `configs/tier-a.yaml` (`configs/tier-a-sensitivity.yaml` is the Q5 grid, `configs/tier-a-gate.yaml` the Q3 one) and `out` to a new directory under `~/.cache/joulie/experiments/05/`; the script writes `summary.md` there. Tier B needs Docker, as the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) does:

```bash
SIMCLUSTER_NAME=joulie-sim-gov experiments/05-governor-exploration/scripts/run_tier_b.sh all
```

The other arms, the settings and the result files are in the [experiment README](https://github.com/joulie-k8s/Joulie/tree/main/experiments/05-governor-exploration).

## What is assumed, and what would settle it

| Fact | Effect on the results | Measurement that settles it |
|---|---|---|
| `dataPathHoldW` (10 W per GPU) | the idle cost of `sustained` | idle and loaded power at `sustained` against `auto`, per GPU model |
| `gatingOffIdleW` (20 W) | the idle cost of `peak` on AMD | idle power at the profile level |
| `clockBeta`, `memoryKneeFrac` | how much `efficient` saves and costs per class | a clock sweep of a compute-bound and a memory-bound kernel |
| receiver gate (off, neutral fractions) | whether `sustained` speeds up peer copies into an idle GPU | a peer copy into an idle receiver under `auto` and `sustained`, per family |
| `idleGovernorDeltaW`, `eppGain`, `eppCeilingDrop` | the CPU idle and partial-load trade-off | energy of each CPU governor at steady loads, per driver |
| `freqRampMS` (500 ms) | how much a demand-driven preset lengthens a burst | the latency of a short compute-bound burst under each governor, per driver |
| the MI300X's lowest cap (`minW` 0 W, by analogy with an MI300A) | whether a cap below the real floor is refused | `amd-smi static --limit` on an MI300X |
| class intensities, the job model | the size of every slowdown | traces of real jobs |

The measurements behind `dataPathHoldW`, `gatingOffIdleW`, the receiver gate
and `idleGovernorDeltaW` run on a real node, one command each, with the
scripts in [`experiments/05-governor-exploration/hardware/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/05-governor-exploration/hardware).

The receiver gate stays off in every repository profile, so the main grid
shows no gate effect; `configs/tier-a-gate.yaml` forces it on with
`slowFraction` below 1 to show the shape of Q3.
