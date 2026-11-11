# Experiments

Experiments on Joulie's policies, each with its scripts, configs and a
report. The [docs site](https://joulie-k8s.github.io/Joulie/main/docs/experiments/)
has a page per experiment. To learn Joulie, start with the
[examples](../examples/README.md) instead.

These results come from the simulator, Joulie's test bed for its behaviour
(caps applied, placement, policies, governors), not evidence of energy savings.
Their energy figures rest on hardware behaviour the simulator assumes: every CPU
core and every GPU does the same work per second whatever its model ([#107](https://github.com/joulie-k8s/Joulie/issues/107)), and
cap responses, power curves and workload intensities are assumed or fitted, not
measured on our nodes. What meaningful experiments need is tracked in the
milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

| Experiment | What it measures | How it runs |
|---|---|---|
| [01 CPU-only benchmark](01-cpu-only-benchmark/README.md) | Throughput and energy of baselines A (no Joulie), B (static partition) and C (queue-aware) on a CPU cluster | kind with KWOK fake nodes and the simulator in HTTP mode ([`kwok-cluster`](kwok-cluster/README.md)) |
| [02 Heterogeneous benchmark](02-heterogeneous-benchmark/README.md) | The same baselines on a mixed cluster of five GPU families and CPU-only nodes | as 01 |
| [03 Homogeneous H100 benchmark](03-homogeneous-h100-benchmark/README.md) | The same baselines on H100 NVL nodes only | as 01 |
| [04 Scoring formula validation](04-scoring-formula-validation/REPORT.md) | The scheduler extender's scoring formula against bin packing, with a Modelica FMU for the cooling; its results are not valid (see its report); it is to be redone on the extracted scheduling library ([#122](https://github.com/joulie-k8s/Joulie/issues/122)) as a behaviour scenario ([#115](https://github.com/joulie-k8s/Joulie/issues/115)) | Python models of the cluster, no Joulie code |
| [05 Governor exploration](05-governor-exploration/README.md) | Energy and time per CPU and GPU governor preset, workload kind and cap; the cluster-level question (Q4) needs measured hardware behaviour ([#106](https://github.com/joulie-k8s/Joulie/issues/106)) and a pilot ([#114](https://github.com/joulie-k8s/Joulie/issues/114)) | Tier A: emulated nodes in `govsweep`; Tier B: the simulated cluster |

## Protocol

The reruns of experiments 01 to 03 found flaws in the model only after hours of runs. A campaign (the sweeps a pull request reports, each run by an experiment's `scripts/20_run_benchmark.sh`, which calls `scripts/05_sweep.py`) follows these steps; each says what blocks it.

1. **A reviewed static audit before any run.** The pull request states, before the first run, what the campaign runs on, and a reviewer other than its author approves it.
   - The model: every row added to [`simulator/CHANGES.md`](../simulator/CHANGES.md) since the commit the last reports ran on; on the images' commit, `TestReferenceScenarioStandalone` passes and the debug runs (`scripts/standalone_sweep.py --config experiments/0N-*/configs/benchmark-debug.yaml`) complete.
   - The workload: `workloadgen` pinned to `-schema-version v2` (`TestWorkloadgenCallersPinSchemaVersion`), the load sized with `kwok-cluster/size_load.py`, and the performance nodes of B and C with `kwok-cluster/pool_load.py` on seeds 3 and 4 (see Sizing the load).
   - The config: the images (see Images); every cap inside the hardware range of every node it reaches, the percent caps resolved into watts as the agent does; and `run.timeout` equal to `workload.arrival_window_sec`, since the run stops at the first and nothing checks that they match.

   No run starts without the approval. This is a manual check today.
2. **One batched campaign per pull request.** Every run the pull request reports (experiments, arms, seeds, repeats, ablations) is listed in it before the first one starts, and runs on the same images. A flaw found mid-campaign stops it; the fix lands in the same pull request, the audit is done again, and the whole batch runs again. A run outside the batch is not reported.
3. **Repeats for every arm.** `run.repeat_first_run` repeats only the first run of the schedule, one arm on one seed. Until the sweep repeats every arm, run the sweep a second time with the same config, into its own `RUNS_ROOT`: `workloadgen` draws the same trace from the same seed and config, so the second pass repeats every arm on every seed. A difference between arms smaller than the spread between an arm's repeats is not reported as an effect.
4. **Ablations designed in.** To separate the caps from the placement, B and C also run with their eco caps off: a copy of the config with `run.baselines: [B, C]` and `policy.caps.cpu_eco_pct_of_max` and `policy.caps.gpu_eco_pct_of_max` at 100, the performance level, in its own `RUNS_ROOT`. A against B without caps is the placement; B without caps against B is the caps; the same for C. A report that credits a difference to the caps or to the placement without the ablation is not merged.
5. **A measured window at regime.** Runs are measured after ramp-up via prefill and before drain: the trace is prefilled (`workload.prefill_sec`), the run stops at `run.timeout`, the end of arrivals (`workload.arrival_window_sec`), and the report measures from `run.measure_from_sec` to it (see Sizing the load). Only baseline A is gated on reaching regime: a utilization below 45 % or a queue above 1 % of its running pods fails it (see the gates below). Under B and C, queueing and utilization are results, not gates.
6. **Gates that stop a run.** A failing gate stops the sweep, and `kwok-cluster/report_figures.py` refuses a run that failed one as it ended:
   - a cap below the lowest the hardware accepts: the simulator refuses it (`rapl.set_power_cap_watts` and `gpu.set_power_cap_watts` in `simulator/cmd/simulator/main.go`), and `04_run_one.py` stops on any refused control command;
   - the polls not covering the window, a load left on a node with no job, a GPU node whose simulated model differs from its node class, and under A the queue, utilization and placed-load checks (`gate_failures` in `04_run_one.py`, listed under Sizing the load);
   - a mean host load above the host's CPUs (`load_failures`): the sweep stops and the run is run again by hand;
   - before a run, a node label left under A or a scheduler extender that never answers under B and C (`03_install_components.sh`).

   Not gated today, so checked by hand in the audit: a cap above the hardware's maximum, which the simulator lowers to the maximum without an error, a `run.timeout` that differs from `workload.arrival_window_sec`, and a campaign whose images or config differ from the ones the pull request lists.

## Baselines

Experiments 01 to 03 run each seed's jobs, at the same times and with the same work, under three baselines. A runs without Joulie: `03_install_components.sh` uninstalls it, clears the nodes' `joulie.io/power-profile` labels and stops if one remains. Its trace is the seed's trace without the `joulie.io/power-profile` and `joulie.io/draining` affinity terms (`derive_baseline_trace` in `05_sweep.py`); the simulator still gives a performance job a `joulie.io/power-profile NotIn [eco]` term from its intent class, which every node meets under A, since none carries the label. B (static partition) and C (queue-aware) run Joulie with its scheduler extender. Between baselines the sweep deletes the `NodePowerProfile` and `TelemetryProfile` objects and the profile labels.

## Images

The `configs/benchmark-40n.yaml` of experiments 01 to 03 pin the images the reports ran: `joulie.local/joulie-*:rerun-2026-10-28`, built from commit `6cc3a1d3` (on the branch of PR #105: `git fetch origin pull/105/head`) and loaded into the kind cluster, with no registry. The later commits change nothing these runs exercise: a lock the simulator's workload tick now takes before reading a node's capacity, an emulated GPU profile, and experiment 05's sweep and analysis. To run such a config, check out that commit, build the four images, create the cluster, load them, and keep the simulator from pulling:

```bash
git fetch origin pull/105/head && git checkout 6cc3a1d3
tag=rerun-2026-10-28 e=02-heterogeneous-benchmark
for c in agent controller-manager scheduler; do
  docker build --build-arg COMPONENT=$c -f Dockerfile -t joulie.local/joulie-$c:$tag .
done
docker build -f simulator/Dockerfile -t joulie.local/joulie-simulator:$tag .
./experiments/$e/scripts/10_setup_cluster.sh experiments/$e/configs/benchmark-40n.yaml
for c in agent controller-manager scheduler simulator; do
  kind load docker-image joulie.local/joulie-$c:$tag --name joulie-heterogeneous
done
SIM_IMAGE_PULL_POLICY=IfNotPresent ./experiments/$e/scripts/20_run_benchmark.sh experiments/$e/configs/benchmark-40n.yaml
```

The cluster name is `name` in the experiment's `configs/kind-cluster.yaml`, or `joulie-cpu-benchmark` for 01, which sets none. To run released images instead, set `images.joulie_registry` and `images.sim_registry` to `registry.cern.ch/joulie` and both tags to the release.

## Sizing the load

Experiments 01 to 03 measure the cluster at regime: after the ramp-up and before the drain. The simulator has no time scale: a job advances `base_speed_per_core` work units per requested core (or GPU) per wall second, and `time_scale` only maps wall-clock offsets onto the simulated day.

A run starts at regime. With `workload.prefill_sec` (1500 s) the trace is read as if it had already run that long without queueing, so every job submitted before then starts at once with the work it would have left. Jobs longer than the prefill are rare but carry a visible share of the load, so even A's backlog creeps up during the window; each report lists it. The run lasts the arrival window, `workload.arrival_window_sec` (900 s, 30 simulated hours), and stops there, before the cluster drains. The report measures from `run.measure_from_sec` (120 s) to the end, and `run.repeat_first_run` runs the first run again to measure run-to-run noise.

`workload.work_scale`, and in 02 and 03 `workload.gpu_util_scale` (each GPU job's GPU utilization raised by 45 %, at most 1, since the generated GPU jobs use about half the GPUs they request), put the utilization of the binding resource (CPU cores in 01, GPUs in 02 and 03) at regime at 0.55 of the cluster on the lighter of seeds 1 and 2, weighted by capacity as the simulator reports it, without queueing or caps:

```bash
python experiments/kwok-cluster/size_load.py 01-cpu-only-benchmark 1620 2400 0.55 cpu
python experiments/kwok-cluster/size_load.py 02-heterogeneous-benchmark 1620 2400 0.55 gpu 1.45
python experiments/kwok-cluster/size_load.py 03-homogeneous-h100-benchmark 1620 2400 0.55 gpu 1.45
```

The sizing rule, the same for 01 to 03: `size_load.py` sizes each experiment for a utilization of 0.55 at regime. If baseline A at that size fails its queue gate, the work scale comes down in proportion to the utilization A measured, to aim at 0.52; if A fails again, once more, to aim at 0.47; if it still fails, the report says the experiment could not start at regime. If A falls below the 45 % utilization gate instead, the scale goes up in proportion to aim at 0.52, and an experiment whose A cannot pass both gates is reported as not run. A short pilot of A alone checks a changed scale before the full sweep. A new scale also resizes B and C on seeds 3 and 4. Sweeps run one at a time: the simulator advances jobs by wall-clock time, and two sweeps side by side would run one experiment's B and C beside the other's; the host is shared, so each run records its 1-minute load average (mean and maximum). Each report gives the load of every run, and its noise table its own repeat with the load of both runs. The scales: 01 0.2627 and 03 0.0646, as sized; 02 0.0401: at 0.0469 its baseline A failed the queue gate, its 8-GPU jobs waiting for whole nodes; at 0.0380 its GPU utilization, from the jobs' requests counted from submission, was 0.445 on seed 1, below the utilization gate; at 0.0444 (0.0380 x 0.52 / 0.445) A read 0.521 on seed 1 and failed the queue gate again, with 1.5 jobs queued on average, 0.6 of them 8-GPU jobs; 0.0401 is 0.0444 x 0.47 / 0.521, the rule's last step. The experiments run at different utilizations, which each report states; compare the policies within an experiment, not across experiments.

In 02 a GPU job goes to the vendor pool (`nvidia.com/gpu`, `amd.com/gpu`) whose requested GPUs, the job's included, are the lowest share of its GPUs when it is submitted, among the pools with a node that can host it: jobs go where GPUs are free, as users of a cluster with several vendors do. Over the window of seeds 1 and 2 both pools carry 0.58 to 0.63 of their GPUs on average, with peaks of 0.78 to 0.82, never above capacity (`experiments/kwok-cluster/pool_load.py`). Each report lists the pending jobs per pool. A GPU job asks for 1, 2, 4 or 8 GPUs, with probability 80, 10, 7 and 3 % (`sampleTotalGPUs` in `simulator/cmd/workloadgen`), whatever its workload type. In 02 and 03 kube-scheduler bin-packs the GPUs (`NodeResourcesFit` with `MostAllocated` over the GPU resources, `scripts/01_create_cluster_kwokctl.sh`), so whole nodes stay free for the 8-GPU jobs; with its default spreading, the 1-GPU jobs took a GPU on every 8-GPU node and every 8-GPU job waited. Under B and C the scheduler extender takes part with weight 1, the weight the scheduler page tells operators to use, so it counts as much as one scoring plugin; its power headroom score favours emptier nodes and may spread GPU jobs. That is the system under test, and each report gives the pending 8-GPU jobs of every run.

Performance jobs may run only on performance nodes under B and C, and `PerformanceSet` splits those over hardware families in proportion to their nodes. Both policies are sized on seeds 3 and 4, which the sweep does not measure. B's static partition (`policy.static.hp_frac`) holds every pool's performance demand at its peak: 19 of 40 nodes in 01, 31 of 41 in 02, 27 of 41 in 03. C starts from the fewest nodes the demand needs (`policy.queue_aware.hp_base_frac`) and adds one per `perf_per_hp_node` performance pods, running or pending (6, 3 and 5, the pods per needed node on those seeds), up to every node.

C counts pods, not the GPUs or cores they request, so it follows the demand loosely. Leaving its pending pods aside, it holds the demand in 157, 145 and 123 of 158 samples on seeds 3 and 4 (01, 02, 03); sized to hold every sample, it would keep as many nodes as B. On the measured seeds the demand is higher: B holds it in 158, 158 and 146 of 158 samples, C in 153, 107 and 34 before its pending pods grow it (`experiments/kwok-cluster/pool_load.py`; 02 at work scale 0.0401). These counts are an upper bound, since they assume no queueing and that standard jobs leave the performance nodes to performance jobs, while the scheduler only steers them toward eco nodes. Each report gives, per run, the performance nodes held and the pending performance jobs; these include the few seconds of placement every job takes, so a policy's cost to the jobs it protects is its difference against A. `python experiments/kwok-cluster/pool_load.py EXPERIMENT [SEED ...]` prints the loads and sizes.

Each run checks its gates as it ends (`04_run_one.py`), and a failing run stops the sweep: every run must cover the measured window with its polls, have no refused control, and end with no load on a node that has no job (no GPU load, and no CPU load beyond its pods outside the workload); baseline A must also keep, on average over the measured window, at most 1 % of its running pods queued (pending for over 30 s, at least one job allowed) and reach at least 45 % utilization at regime, and in a GPU experiment its GPU utilization must be within 0.03 of the GPU load of the placed jobs (each placed job's GPUs times its GPU utilization, summed at every poll). The last two checks catch a simulator that keeps a node's load after its last job. Queueing and utilization under B and C are results, not gates. A run whose mean 1-minute host load exceeds the host's CPUs fails the load gate and is run again, except the repeat of the first run, which only estimates noise: the report script leaves it out and says why. Each report's noise table lists every replicate of a baseline with its mean host load, so what load does to a run can be read there. `experiments/kwok-cluster/report_figures.py` draws each report's figures and tables from a sweep and refuses a run that failed its gates.

## DNS and the scheduler extender

kube-scheduler runs with `hostNetwork: true`, so it resolves names through the node's resolver, not CoreDNS, and on KinD that resolver cannot resolve `.cluster.local` names. Each experiment's `01_create_cluster_kwokctl.sh` therefore gives kube-scheduler the extender's fixed ClusterIP (`EXTENDER_CLUSTER_IP`, `10.96.100.76`, which the Helm values set as `schedulerExtender.clusterIP`), and no name is resolved. `EXTENDER_CLUSTER_IP=""` uses the service's DNS name instead, which works only where the node's resolver knows `.cluster.local`. Before each B or C run the installer probes the extender's `/healthz` from the control plane at that address, 12 times, 5 s apart, and stops the sweep if it never answers.
