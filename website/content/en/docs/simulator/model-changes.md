---
title: "Model Changes"
weight: 45
---

This page lists every change since Joulie v0.2.3 that moves the numbers the simulator produces for its modelled nodes (standalone mode, and Kubernetes mode without emulation), and how far each one moved them. A result produced with an older simulator holds for the model it ran; read it together with this table. Emulated nodes (`SIM_EMULATE`) run the physics of `simulator/pkg/hwemu` and are not covered here.

The reports of experiments 01 to 03 in [`experiments/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments) were produced before every change below and have not been rerun; each carries a dated note that says which of its model statements did not hold.

## Two yardsticks

**The reference scenario** lives in `simulator/cmd/simulator/testdata/reference/`: eight inventory entries (nine nodes), one for each way the simulator resolves a node's model (a SPECpower curve, the same CPU on one socket, a second curve, a proxy CPU, an Intel CPU with official figures only, catalog GPUs on an Intel and on an EPYC host, GPUs the catalog does not know), and 40 jobs over 2 h. It runs in standalone mode under baselines A, B and C with `SIM_SA_WORKERS=1`, `SIM_SA_DT=5`, `SIM_SA_TIMEOUT=7200`, the code defaults and the embedded catalog. `TestReferenceScenarioStandalone` compares the run with `reference.golden.json` (energy as `hw.energy` by `hw.type` and by node, job statistics as `joulie.sim.*`) and fails on any key that moves by more than a relative 1e-9, printing a table of what moved.

**The debug runs** are each experiment's `configs/benchmark-debug.yaml` through the standalone sweep: one seed, 800 jobs (01) or 600 (02 and 03), 300 s of simulated time, all three baselines.

```bash
PATH=$HOME/.local/go/bin:$PATH python3 scripts/standalone_sweep.py \
  --config experiments/02-heterogeneous-benchmark/configs/benchmark-debug.yaml \
  --simulator <simulator binary> --results-dir <directory>
```

Energy in the tables is the integral of the power a run reports: the sum over the rows of `timeseries.csv` of `cpu_power_w + gpu_power_w`, times the 5 s step. From the stepNode change on it equals the golden's `hw.energy` total to within 0.001 %. The saving of B or C is 1 minus its energy over A's. Every simulator binary ran the same trace of each experiment.

**How each change was measured.** From the stepNode change on, each one compares the tree just before it with the tree just after it. The earlier changes were measured on the v0.2.3 simulator, with the old and the new behaviour selected through its inputs: the thermal thresholds through `SIM_{CPU,GPU}_THERMAL_THROTTLE_{START,FULL}_C`, the catalog corrections and the generated catalog through `SIM_HARDWARE_CATALOG_PATH`. With all of them selected, v0.2.3 gives the same energy as the tree just before the stepNode change: identical on the reference scenario, and within 0.03 % on the debug runs, less than two identical runs of that simulator differ on GPU nodes (up to 0.05 %). The other changes in between (the curve cap solver among them, and the emulate mode, workload and OpenTelemetry work) therefore moved neither yardstick.

## Changes

| Change | What changed | Reference scenario | Debug runs |
|---|---|---|---|
| Thermal throttling is opt-in | `SIM_{CPU,GPU}_THERMAL_THROTTLE_{START,FULL}_C` default to 0, which means off; they were 85 and 97 °C for CPUs, 82 and 92 °C for GPUs. A 2x EPYC 9654 node at full load on its SPECpower curve headed for 126 °C, throttled fully at 97 °C, dropped to 141 W with throughput 0, cooled and swung back. Guards: `TestCurveNodeIsNotThermallyThrottledByDefault`, `TestGPUIsNotThermallyThrottledByDefault` | A +0.2 %, B and C +1.1 %; B and C complete 30 jobs instead of 29 | under 0.02 %, within the run-to-run spread |
| A curve node throttles under its cap (D13) | Above its RAPL cap, a node on a measured curve now lowers its frequency on its own curve (`phys.SolveFreqScaleForCap`). Before, the curve clamped its power to the cap, so the solver never saw the excess and the node kept full frequency and full throughput. This change leaves the analytic model's power unchanged (`TestAnalyticCPUPowerUnchangedByF2`); its cap solver changed later, see the row on memory traffic. Guards: `TestSolveFreqScaleForCapCurveThrottles`, `TestCapSaturatedUsesTheNodeModel` | none: standalone eco sets a CPU frequency scale and the default CPU cap is 5000 W, so no CPU cap binds | none, for the same reason |
| Catalog corrections | `AMD_EPYC_9655` `node2S` is SPEC result 01457, 126 to 751 W (was 138 to 861 W); `minBoardPowerW` of `NVIDIA_H100_SXM` is 300 W (was 350) and of `AMD_INSTINCT_MI300X` 400 W (was 350). Guards: `TestCatalogEPYC9655CurveIsSPECResult01457`, `TestCatalogGPUMinimumsHavePublicSources` | A -1.3 %, B and C -1.7 %, all on the EPYC 9655 node | none at the time: the experiments read a generated catalog |
| The experiments read the embedded catalog | The sweep and the installers no longer generate a catalog from the inventory (it gave every CPU a 300 W TDP and no curve, so every CPU ran the analytic model at 80 W idle and 420 W peak) and clear `SIM_HARDWARE_CATALOG_PATH`. Guard: `TestExperimentInventoriesMatchTheEmbeddedCatalog` | none: it always read the embedded catalog | A +108 % (01), +12 % (02), +19 % (03); the saving of C falls from 31.2 to 17.0 % (01), 31.8 to 30.4 % (02), 34.5 to 33.6 % (03) |
| stepNode | The physics of a modelled node advances only in `stepNode`, in simulated time and in substeps (`SIM_SA_SUBSTEP` in standalone mode, 100 ms on the physics tick `SIM_PHYSICS_TICK` otherwise); HTTP reads and gauges no longer advance it, and energy is the integral of the instantaneous power. Guards: `TestReadsDoNotAdvancePhysics`, `TestStandaloneEnergyIntegratesInstantPower`, `TestStepNodeIgnoresWallClock` | power unchanged; the reported energy (`energy_cumulative_j`) now equals its integral, where it ran 2.5 to 3.1 % below it and changed between identical runs | power: B and C -0.1 % in 02 and 03, as the GPU cap lag now runs in simulated time; the reported energy now equals the integral of the power, where it ran 18 to 51 % below it and changed between identical runs |
| Artifact removals | Removed: the cut of idle CPU power on a node with a throttle target (up to 50 % below 20 % utilization), so idle power no longer depends on whether Joulie manages the node; the deep idle state of a capped idle GPU (10 % of idle power), so a board never draws less than its idle power; and the report of cap plus headroom for a cap below the power at the lowest frequency, so such a node reports what it draws there. Guards: `TestIdlePowerDoesNotDependOnThrottleTarget`, `TestP03GPUPowerNeverBelowIdle`, `TestCapBelowFloorReportsFloorPower` | A unchanged, B and C +0.2 % (the curve nodes) | A unchanged; B +0.6, +3.1, +4.5 % and C +0.9, +4.6, +6.4 % (01, 02, 03); the saving of C falls from 17.0 to 16.3 % (01), 30.4 to 27.2 % (02), 33.6 to 29.4 % (03) |
| A GPU cap slows a job once | A GPU cap slows a GPU job by what the capped board model says it costs. The legacy progress formula charged it a second time, by the cap over the board maximum weighted by the job's GPU sensitivity. Guard: `TestGPUCapSlowsAGPUJobOnce` | none | none: no job with a GPU sensitivity ran on a capped GPU |
| Catalog facts reach the model | With no environment or class value, a node takes from the catalog its GPU maximum and lowest cap per board (they were 300 and 80 W unless an environment variable or a class set them), its CPU socket cap range (TDP down to the lowest cTDP, else 0.55 x TDP) and, without a curve, a peak of sockets x TDP (was 420 W); `node2S` curves scale with the socket count. `SIM_POD_W`, `SIM_DVFS_DROP_W_PER_PCT`, `SIM_GPU_ALPHA_UTIL` and `SIM_GPU_BETA_CAP` are removed and warn once. Guards: `TestCatalogGPUCapsReachTheModelWhenEnvIsUnset`, `TestNode2SCurveScalesWithSocketCount`, `TestProxyCPUPeakFollowsTDP`, `TestRemovedModelKnobsWarnOnce` | A +61 %, B and C +63 %: GPU energy x2.07; CPU energy of A -5 %, mostly the one-socket node, whose curve now halves | A +4.2 % (01), +32 % (02), +20 % (03); the saving of C rises from 16.3 to 17.3 % (01), 27.2 to 30.5 % (02), 29.4 to 32.6 % (03) |
| Standalone runs hwemu profiles | An inventory entry may name an hwemu `profile`; legacy entries honour `gpu_min_cap_watts` and `gpu_max_cap_watts`; eco on a profile node with a writable PL1 is a CPU power cap. Guards: `TestStandaloneHonoursInventoryGPUCapRange`, `TestStandaloneInventoryProfileKey`, `TestStandaloneEcoOnRAPLProfileIsAPowerCap` | none | none: no profile nodes, and the inventories' GPU cap ranges equal the catalog's |
| The analytic cap solver counts memory traffic | `phys.SolveFreqScaleForCap` on the analytic model now inverts the power formula with the memory and IO activity factor that the power it then computes applies. Before, a capped node with memory or IO intensity above 0 drew below its cap (262.5 W under a 300 W cap). 20 of the 548 cases in `testdata/physics/analytic-cpu-power.golden.json` moved, all capped with memory or IO traffic; no saturation flag changed. Guards: `TestFC04CappedAnalyticNodeWithMemoryTrafficDrawsItsCap`, H15 | none: no CPU cap binds in the reference scenario | not rerun |
| An unknown GPU takes its labelled count | A node whose GPU the catalog does not know takes its device count from the `joulie.io/hw.gpu-count` label (the inventory's `gpu_count` in standalone mode). Before, it got 0 GPUs and drew no GPU power. Guard: `TestFC03UnknownGPUTakesTheLabelCount` | `ref-gpu-unknown` now draws power for its 4 GPUs: its energy in baseline A rose from about 1.09 MJ to 8.44 MJ; only the GPU and `ref-gpu-unknown` energy keys moved, in A, B and C | not rerun |
| Cap ranges stay ordered | An explicit `SIM_CPU_SOCKET_CAP_MIN_W` or `SIM_GPU_MIN_CAP_WATTS_PER_GPU` above the maximum the catalog fills in is lowered to that maximum, with a warning. Before, the range came out inverted (200 to 165 W on a Xeon Platinum 8260). Guard: `TestFC02ExplicitCapMinimumAboveTheCatalogueMaximumIsOrdered` | none: the reference scenario sets no explicit minimum | none, for the same reason |
| Clock suffix in CPU names | Catalog matching drops a trailing clock token and the `cpu` before it, so the label-safe form of a full cpuinfo name such as `Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz` matches its catalog entry instead of falling back to the generic model. Guard: `TestFC07LabelSafeCPUInfoSpellingMatchesThe8260` | none: the reference inventory spells its models without the suffix | not rerun |

No change moved the number of completed jobs of the debug runs.

## Energy after each change

The reference scenario (energy in MJ):

| After | A | B | C | A: CPU, GPU | Completed jobs A, B, C |
|---|---|---|---|---|---|
| v0.2.3 | 39.63 | 38.81 | 38.81 | 16.52, 23.11 | 33, 29, 29 |
| thermal throttling opt-in | 39.71 | 39.22 | 39.22 | 16.60, 23.11 | 33, 30, 30 |
| catalog corrections | 39.20 | 38.57 | 38.57 | 16.09, 23.11 | 33, 30, 30 |
| stepNode | 39.20 | 38.57 | 38.57 | 16.09, 23.11 | 33, 30, 30 |
| artifact removals | 39.20 | 38.65 | 38.65 | 16.09, 23.11 | 33, 30, 30 |
| catalog facts | 63.18 | 63.05 | 63.05 | 15.29, 47.89 | 33, 30, 30 |

The debug runs (energy of A in MJ, saving of B and of C against A):

| After | 01: A | 01: B, C | 02: A | 02: B, C | 03: A | 03: B, C |
|---|---|---|---|---|---|---|
| v0.2.3, generated catalog | 2.63 | 23.4 %, 31.2 % | 9.02 | 22.7 %, 31.8 % | 10.96 | 24.1 %, 34.5 % |
| embedded catalog | 5.48 | 10.9 %, 17.0 % | 10.11 | 21.6 %, 30.4 % | 13.03 | 23.6 %, 33.6 % |
| stepNode | 5.48 | 10.9 %, 17.0 % | 10.11 | 21.7 %, 30.4 % | 13.03 | 23.7 %, 33.6 % |
| artifact removals | 5.48 | 10.3 %, 16.3 % | 10.11 | 19.2 %, 27.2 % | 13.03 | 20.3 %, 29.4 % |
| catalog facts | 5.71 | 11.2 %, 17.3 % | 13.36 | 21.4 %, 30.5 % | 15.68 | 22.5 %, 32.6 % |

The changes the tables leave out moved neither yardstick by more than 0.03 %. A 300 s debug run says how a change moves an experiment, not what the experiment's 60 h result would be.

## Adding a change

A change that moves a number of a modelled node (physics in `nodephys.go`, model resolution in `modelresolve.go`, job speed, the catalog, the standalone policy) adds a row here in the same pull request:

1. Run `go test ./simulator/cmd/simulator/ -run ReferenceScenario` on the change; the failure prints the keys that moved. Review them, then rewrite the golden with `-update` and commit it.
2. Build the simulator before and after the change and run the three debug configurations with each binary on the same traces: the sweep reuses the trace it finds in `<results-dir>/traces/`, so copy that directory from the first run into the second run's results directory.
3. Add the row: what changed, the guard test, the reference and debug-run deltas. A change that moves neither yardstick but changes another mode (Kubernetes mode caps, emulate mode) still gets a row that says so.
4. Add a dated note to the reports of any experiment whose description the change makes untrue.
