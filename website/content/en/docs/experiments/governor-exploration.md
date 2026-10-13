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

All results on this page come from the simulator: emulated nodes
(`simulator/pkg/hwemu`) whose governor and GPU clock physics rest on
parameters tagged `assumed:` in their profiles. They show the shape of each
trade-off under those assumptions, not measured savings.

## Questions

| ID | Question | Decides |
|---|---|---|
| Q1 | Per CPU driver, which preset minimises energy to solution at each load level, and what does it cost in time? | CPU enabled defaults |
| Q2 | Per GPU family and workload kind, energy and time of `auto`, `sustained`, `efficient` (50 to 90 %), `low`, `peak`, alone and with the GPU cap at 100 % and 60 %? | GPU enabled defaults, ceiling default |
| Q3 | With a receiver gate, how much do multi-GPU jobs gain from `sustained`, and at which idle cost does it stop paying? | which GPU models to opt in to `sustained` |
| Q4 | At cluster level, does Joulie with governors beat caps only on energy without breaking the performance class? | release decision |
| Q5 | Which conclusions survive the assumed parameters? | what must be measured before trusting a default |

A default is adopted when, in Tier A and in Tier B, it lowers energy to
solution against the baseline arm with a performance-class p95 slowdown at
or below 5 % and an eco-class p95 slowdown at or below 20 %, and Q5 does not
flip its sign.

## Method

**Tier A** is an offline sweep, `simulator/cmd/govsweep`, in virtual time:
one fresh emulated node per cell, every cell in parallel. Each arm is a
preset, which govsweep plans with `pkg/governor` on the facts the node's
emulated sysfs and fake `nvidia-smi` show, exactly as the agent's discovery
reads them, and writes as the agent would; a native word in a config is
refused. A job does at the reference (no cap, `performance` or `auto`) 60 s
of work and progresses at the simulator's v1 workload rate under the
throughput the node reports after each step. Energy to solution is the
node's whole energy from the job's start to its end. The grid
(`configs/tier-a.yaml`): five CPU profiles, one per frequency driver, under
the profile's own governor and each CPU preset with boost on and off, idle,
at steady 10 to 100 % loads of four classes and under bursts; five GPU
profiles under every GPU preset (`efficient` at 50 to 90 %), with the cap at
100 % and 60 %, idle and with jobs of three GPU classes on 1, 2 and 4 GPUs,
with peer, host and gather transfers; two seeds.

**Tier B** runs one arm on a simulated cluster
(`tests/simcluster/specs/governor.yaml`, two Nodes per CPU driver and GPU
family) with Joulie in the loop, and the harness checks GS01 to GS04: the
presets each agent lists, the result each preset gives on each profile and
Node state, the native word in the emulated files, and every file back at
its baseline after an upgrade to `GOVERNOR_CONTROL=off`.

## Results

`analysis/summarize.py` regenerates these tables from a run's CSVs.

Run of 2026-10-10 on the working tree of the governor work (the CPU grid rerun on 2026-10-11, after the sweep was made to read EPP support as the agent does): `configs/tier-a.yaml`,
11,340 cells (1,540 CPU, 9,800 GPU; 1,764 GPU cells name a preset their
profile cannot express and do not run, 72 `low` cells on the L40S did not
finish within 10 times the reference and are left out), every cell run once
per seed. The two seeds differ by at most 0.1 % in energy: hwemu has no
randomness of its own, and a seed only moves the job's phase and the burst
arrivals. Changes are medians over profiles, classes and seeds against the
baseline arm (`performance` with boost on; `auto` with no cap), energy is
the host's energy to solution, time the job's makespan.

### E-G1: CPU presets (Q1)

| Load | `balanced` | `efficiency` | `efficiency`, boost off | `performance`, boost off |
|---|---|---|---|---|
| idle | 0 % energy | 0 % | 0 % | 0 % |
| steady 10 % | +5.8 % energy, +10.3 % time | +6.1 %, +10.8 % | -0.4 %, +11.6 % | -4.7 %, +5.3 % |
| steady 30 % | +3.0 %, +8.5 % | +1.6 %, +5.9 % | -14.2 %, +17.2 % | -17.1 %, +7.9 % |
| steady 50 % | -0.1 %, +3.1 % | -0.5 %, +2.5 % | -22.3 %, +13.9 % | -22.5 %, +10.5 % |
| steady 70 % | -0.4 %, +2.4 % | -0.5 %, +2.4 % | -25.8 %, +16.1 % | -25.8 %, +13.4 % |
| steady 100 % | -6.3 %, +2.1 % | -9.2 %, +3.1 % | -30.0 %, +22.0 % | -28.4 %, +17.8 % |
| bursty (I/O-bound, 50 ms every 1 s) | -15.4 %, no latency change | -14.9 %, no latency change | -15.2 % | -10.1 % |

With boost left on, a demand-driven preset saves nothing over
`performance` up to 70 % load: at low loads it holds a lower frequency, the
job runs longer, and the node's other power runs longer with it (race to
idle). At full load the preferences of the EPP drivers start to pay, 6 to
9 % at 2 to 3 % more time. Turning boost off is what saves energy, 14 to
30 % from 30 % load up, at 8 to 22 % more time. Under bursts the demand-driven presets save
about 15 % with the same burst latency at the 10 ms resolution of the
sweep. Idle power is the same under every preset because
`idleGovernorDeltaW` is 0 in every profile (assumed).

### E-G2: GPU presets (Q2)

| Arm | compute-bound | memory-bound | bandwidth-bound | idle node |
|---|---|---|---|---|
| `efficient` 90 % | -3.1 % energy, +6.1 % time | -6.7 %, +1.3 % | -7.0 %, +1.0 % | 0 % |
| `efficient` 80 % | -5.9 %, +13.3 % | -13.2 %, +2.6 % | -13.7 %, +2.0 % | 0 % |
| `efficient` 75 % | -7.2 %, +17.5 % | -16.3 %, +3.3 % | -16.9 %, +2.5 % | 0 % |
| `efficient` 60 % | -10.4 %, +35.3 % | -22.2 %, +10.9 % | -22.8 %, +9.7 % | 0 % |
| `sustained` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `peak` | 0 %, 0 % | 0 %, 0 % | 0 %, 0 % | +7.9 % |
| `low` | +38 %, +257 % | +37 %, +233 % | +36 %, +228 % | 0 % |
| `auto`, cap 60 % | 0 %, +17.3 % | -7.6 %, +2.6 % | -7.6 %, +2.1 % | 0 % |

`efficient` lowers energy on every family that can express it (MI300X,
H100 SXM and NVL, L40S; the W7900 has no overdrive), most on memory-bound
work, where the compute clock above the knee buys little. The 75 % default
of this run costs 17.5 % time on compute-bound jobs, and its p95 slowdown
over all workloads, +22.5 %, is just above the 20 % the eco class allows;
80 % (+17.3 % p95, -10.7 % energy) passes, and is now the default.
`sustained` and `peak` cost their idle power (`dataPathHoldW`,
`gatingOffIdleW`) and gain nothing while the receiver gate is off. A 60 %
cap does not bind on the W7900, whose cap range is a single value, and
binds on the L40S only once a job holds the GPU (persistence mode is off).

### Receiver gate (Q3) and sensitivity (Q5)

`configs/tier-a-sensitivity.yaml` (5,632 cells) forces the gate on with
`slowFraction` below 1, where a job of four GPUs gathers into an idle one
(memory-bound, 0.3 of the link per sender):

| `slowFraction` | `auto` against no gate | `sustained` against `auto` |
|---|---|---|
| 1.0 (repository) | 0 % | +1.3 % energy, 0 % time |
| 0.7 | +8.6 % energy, +8.6 % time | -6.6 % energy, -7.9 % time |
| 0.5 | +24 % energy, +24 % time | -18.2 % energy, -19.4 % time |

On the W7900 the gate adds 9.2 kJ to such a 60 s job at `slowFraction` 0.5
and 3.3 kJ at 0.7. `sustained` holds only the idle receiver's data path
(the busy GPUs raise their own), so it pays as long as that costs less than
about 150 W (0.5) or 55 W (0.7); the repository assumes 10 W. Hopper never gates: its memory clock
stays at its rated value under `auto`.

Over the other assumed facts at low, mid and high values:
`efficient` 75 % keeps a negative energy change at every value of
`dataPathHoldW`, `gatingOffIdleW` and `autoHoldS`, falls to -0.9 % at
`clockBeta` 1.0 and to 0 % at `memoryKneeFrac` 0.87, so its saving rests on
two assumptions a clock sweep on each family must check. The CPU presets'
energy change against `performance` stays within 0.5 % in either direction
over `idleGovernorDeltaW`, `eppGain` and `eppCeilingDrop`.

### Acceptance (Tier A half)

| Candidate default | Class | Energy | p95 slowdown | Tier A |
|---|---|---|---|---|
| CPU `efficiency` (boost as found) | eco | -0.5 % | +22.2 % | fails |
| CPU `efficiency`, boost off | eco | -16.3 % | +48.0 % | fails |
| GPU `efficient` 75 % | eco | -13.2 % | +22.5 % | fails, narrowly |
| GPU `efficient` 80 % | eco | -10.7 % | +17.3 % | passes |
| GPU `efficient` 90 % | eco | -5.5 % | +7.9 % | passes |

### Tier B

Arm `all` (`GOVERNOR_CONTROL=all`, `GOVERNOR_WRITES=true`) on a fresh
simulated cluster of 16 Nodes: GS01 to GS04 pass. Every agent lists the
presets its profile maps to, every NodeTwin asks for the preset of its
Node's state (CPU `performance` or `efficiency` on CPU-only Nodes, GPU
`efficient` on eco GPU Nodes that list it, nothing else), every result is
the one the mapping predicts, the emulated files hold the native words of
the 11 results, and after the upgrade to `GOVERNOR_CONTROL=off` all 11 are
`restored` and every file is back at its baseline. Workload runs per arm,
for Q4, are not done yet.

## What is assumed, and what would settle it

| Fact | Effect on the results | Measurement that settles it |
|---|---|---|
| `dataPathHoldW` (10 W per GPU) | the idle cost of `sustained` | idle and loaded power at `sustained` against `auto`, per GPU model |
| `gatingOffIdleW` (20 W) | the idle cost of `peak` on AMD | idle power at the profile level |
| `clockBeta`, `memoryKneeFrac` | how much `efficient` saves and costs per class | a clock sweep of a compute-bound and a memory-bound kernel |
| receiver gate (off, neutral fractions) | whether `sustained` speeds up peer copies into an idle GPU | a peer copy into an idle receiver under `auto` and `sustained`, per family |
| `idleGovernorDeltaW`, `eppGain`, `eppCeilingDrop` | the CPU idle and partial-load trade-off | energy of each CPU governor at steady loads, per driver |
| class intensities, the job model | the size of every slowdown | traces of real jobs |

The receiver gate stays off in every repository profile, so the main grid
shows no gate effect; `configs/tier-a-sensitivity.yaml` forces it on with
`slowFraction` below 1 to show the shape of Q3.
