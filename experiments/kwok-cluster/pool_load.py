#!/usr/bin/env python3
"""The load a sweep's traces put on each pool without queueing or caps, and
the policy sizes that follow from it. The traces are generated, shaped
(prefill, window) and retargeted with the experiment's own code
(05_sweep.py) over the KWOK nodes of its inventory, and sampled every 10 s
over the measured window (run.measure_from_sec to run.timeout).

A pool is what a job can run on: the NVIDIA GPUs, the AMD GPUs and the cores
of the CPU-only nodes. For each it prints the requested load at the start of
a run, its mean and peak and the samples above capacity, then the same for
performance jobs alone (a required affinity joulie.io/power-profile NotIn
[eco]), which under B and C run only on performance nodes.

Performance nodes are split over hardware families in proportion to their
nodes (policy.PerformanceSet, copied in performance_set below), so the
performance capacity of a pool follows from the count. At each sample the
script finds the smallest count whose performance capacity holds every
pool's performance demand. From these it derives:
  B, policy.static.hp_frac: the largest count needed, over the nodes;
  C, policy.queue_aware.hp_base_frac: the smallest count needed;
  C, policy.queue_aware.perf_per_hp_node: performance pods per needed node
     (ratio of the means), so that C's demand term, ceil(performance pods /
     per node), follows the count needed;
and it prints how often the configured B and C hold the demand, and the C
that would hold it in every sample as B does. C also counts pending
performance pods, so it grows when they wait, which this static check
leaves out; the runs measure the waits. The demand is
an upper bound: it assumes no queueing and that standard jobs leave the
performance nodes to performance jobs, while the scheduler only steers them
toward eco nodes.

Seeds 3 and 4 by default: the policies are sized on seeds the sweep does not
measure (it runs seeds 1 and 2).

With --jobmix PATH it also writes the CPU jobs of every seed's trace
(workload type, class, memory and IO intensity, CPU units) to PATH, the
fixture TestBetaFreqFit fits the simulator's CPU frequency exponent over
(simulator/cmd/simulator/testdata/exp01-cpu-jobmix.json, from experiment
01, seeds 3 and 4, which the sweep does not measure).

Usage, from the repository root:
    python experiments/kwok-cluster/pool_load.py EXPERIMENT [SEED ...] [--jobmix PATH]
"""
import importlib.util, json, math, pathlib, sys, tempfile
from unittest.mock import patch, MagicMock
import yaml

args = sys.argv[1:]
jobmix = None
if "--jobmix" in args:
    i = args.index("--jobmix")
    jobmix = args[i + 1]
    del args[i:i + 2]
exp = args[0]
seeds = [int(s) for s in args[1:]] or [3, 4]
repo = pathlib.Path.cwd()


def performance_only(affinity) -> bool:
    """Whether a pod may run only on performance nodes: a required node
    affinity term with joulie.io/power-profile NotIn [eco]."""
    terms = (((affinity or {}).get("nodeAffinity") or {}).get("requiredDuringSchedulingIgnoredDuringExecution") or {}).get("nodeSelectorTerms") or []
    return any(e.get("key") == "joulie.io/power-profile" and e.get("operator") == "NotIn" and "eco" in (e.get("values") or [])
               for t in terms for e in (t.get("matchExpressions") or []))


def performance_set(nodes: list, count: int) -> set:
    """pkg/controller/policy.PerformanceSet: one slot per family, then each
    next slot to the family whose share of slots is furthest below its share
    of nodes (deservesNextSlot), families in name order, nodes by name."""
    members = {}
    for n in nodes:
        members.setdefault(n["family"], []).append(n["name"])
    fams = sorted(members)
    for f in fams:
        members[f].sort()
    count = min(max(count, len(fams)), len(nodes))
    slots = {f: 1 for f in fams}
    for _ in range(count - len(fams)):
        nxt = None
        for f in fams:
            if slots[f] == len(members[f]):
                continue
            na, qa = len(members[f]), slots[f]
            if nxt is None or na * (2 * slots[nxt] + 1) > len(members[nxt]) * (2 * qa + 1):
                nxt = f
        slots[nxt] += 1
    return {name for f in fams for name in members[f][:slots[f]]}


sp = importlib.util.spec_from_file_location("sw", repo / f"experiments/{exp}/scripts/05_sweep.py")
sw = importlib.util.module_from_spec(sp)
with patch("subprocess.run", return_value=MagicMock(stdout="{}", returncode=0)):
    sp.loader.exec_module(sw)
cfg = yaml.safe_load(open(repo / f"experiments/{exp}/configs/benchmark-40n.yaml"))
w, run, pol = cfg["workload"], cfg["run"], cfg["policy"]
inv = yaml.safe_load(open(repo / cfg["inventory"]["source"]))
nodes = []
for n in inv["nodes"]:
    for i in range(n.get("replicas", 1)):
        g = int(n.get("gpu_count", 0)); v = str(n.get("vendor", "none")).lower()
        vendor = "amd" if "amd" in v else "nvidia" if g else "none"
        nodes.append({"name": f"{n['node_name_prefix']}-{i}", "vendor": vendor, "gpu_product": n.get("product", ""), "gpu_count": g,
                      "cpu_model": n.get("cpu", ""), "cpu_only": g <= 0, "cores": float(n.get("cpu_cores", 0)),
                      "family": f"gpu:{n.get('product', '')}" if g else f"cpu:{n.get('cpu', '')}"})
sw.load_kwok_nodes = lambda: nodes


def pool_of(node) -> str:
    return node["vendor"] if node["gpu_count"] else "cpu"


def capacity(names=None) -> dict:
    cap = {}
    for n in nodes:
        if names is None or n["name"] in names:
            cap[pool_of(n)] = cap.get(pool_of(n), 0) + (n["gpu_count"] or n["cores"])
    return cap


cap = capacity()
pools = sorted(cap)
perf_cap = [capacity(performance_set(nodes, k)) for k in range(len(nodes) + 1)]
sw.RESULTS = pathlib.Path(tempfile.mkdtemp())
speed = float(cfg["simulator"]["base_speed_per_core"])
R, W = float(run["measure_from_sec"]), float(run["timeout"])
N = len(nodes)
B = int(round(N * float(pol["static"]["hp_frac"])))
qa = pol["queue_aware"]
C_base, C_min, C_max, C_per = int(round(N * float(qa["hp_base_frac"]))), int(qa["hp_min"]), int(qa["hp_max"]), int(qa["perf_per_hp_node"])
need_all, pods_all = [], []
mix = []
for seed in seeds:
    p = sw.generate_canonical_seed_trace(seed=seed, jobs=run["jobs"], mean_inter_arrival_sec=run["mean_inter_arrival_sec"],
        perf_ratio=w["perf_ratio"], compute_bound_perf_boost=w.get("compute_bound_perf_boost", 3.5), gpu_ratio=w["gpu_ratio"],
        burst_day_probability=w["burst_day_probability"], burst_mean_jobs=w["burst_mean_jobs"], burst_multiplier=w["burst_multiplier"],
        dip_day_probability=w["dip_day_probability"], dip_multiplier=w["dip_multiplier"], emit_workload_records=w["emit_workload_records"],
        work_scale=w["work_scale"], allowed_workload_types=w.get("allowed_workload_types"), time_scale=run["time_scale"],
        arrival_window_sec=w["arrival_window_sec"], prefill_sec=w.get("prefill_sec", 0.0), base_speed_per_core=speed,
        gpu_util_scale=w.get("gpu_util_scale", 1.0))
    # The GPU experiments pick a job's vendor by the pools' load, from its length.
    t = sw.retarget_trace_for_cluster(p, speed) if any(n["gpu_count"] for n in nodes) else sw.retarget_trace_for_cluster(p)
    if jobmix:
        for l in open(t):
            j = json.loads(l)
            if j.get("type") == "job" and (j.get("work") or {}).get("cpuUnits"):
                wp = j.get("workloadProfile") or {}
                mix.append([j["workloadType"], (j.get("workloadClass") or {}).get("cpu", ""), round(wp.get("memoryIntensity", 0), 3),
                            round(wp.get("ioIntensity", 0), 3), round(j["work"]["cpuUnits"])])
    jobs = []
    for l in open(t):
        if '"type":"job"' not in l:
            continue
        j = json.loads(l); r = j["podTemplate"]["requests"]
        pool = "nvidia" if "nvidia.com/gpu" in r else "amd" if "amd.com/gpu" in r else "cpu"
        g = sw._quantity(r.get(f"{pool}.com/gpu")) if pool != "cpu" else 0.0
        c = sw._quantity(r.get("cpu")); wk = j["work"]
        d = max(wk["gpuUnits"] / (speed * max(0.1, g)) if wk["gpuUnits"] else 0, wk["cpuUnits"] / (speed * c) if wk["cpuUnits"] and c else 0)
        jobs.append((pool, j["submitTimeOffsetSec"], d, g if pool != "cpu" else c, performance_only(j["podTemplate"].get("affinity"))))
    times = list(range(int(R), int(W) + 1, 10))
    load = {q: [] for q in pools}; perf = {q: [] for q in pools}
    for tt in times:
        active = [x for x in jobs if x[1] <= tt < x[1] + x[2]]
        for q in pools:
            load[q].append(sum(x[3] for x in active if x[0] == q))
            perf[q].append(sum(x[3] for x in active if x[0] == q and x[4]))
        pods = sum(1 for x in active if x[4])
        need = next((k for k in range(N + 1) if all(perf_cap[k].get(q, 0) >= perf[q][-1] for q in pools)), N)
        need_all.append(need); pods_all.append(pods)
    t0 = [sum(x[3] for x in jobs if x[0] == q and x[1] <= 0 < x[1] + x[2]) / cap[q] for q in pools]
    print(f"{exp} seed {seed}:")
    for q, a in zip(pools, t0):
        ld = [v / cap[q] for v in load[q]]; pf = [v / cap[q] for v in perf[q]]
        print(f"  {q:6} {cap[q]:6.0f}: all jobs at 0 s {a:.2f}, mean {sum(ld)/len(ld):.2f} peak {max(ld):.2f} over capacity {sum(v > 1 for v in ld)}/{len(ld)}"
              f" | performance jobs mean {sum(pf)/len(pf):.2f} peak {max(pf):.2f}")
n_s = len(need_all)


def c_counts(base, per):
    return [min(max(max(base, math.ceil(pods / per)), C_min), C_max) for pods in pods_all]


def holds(counts):
    return sum(need <= k for need, k in zip(need_all, counts))


per = sum(pods_all) / max(1, sum(need_all))
full = min(((sum(c_counts(b, q)) / n_s, -q, b) for b in range(min(need_all), max(need_all) + 1) for q in range(1, 41)
            if holds(c_counts(b, q)) == n_s), default=None)
if jobmix:
    cmd = "python experiments/kwok-cluster/pool_load.py " + " ".join([exp, *map(str, seeds), "--jobmix", jobmix])
    doc = {"source": f"the CPU jobs of seeds {', '.join(map(str, seeds))} of {exp}, configs/benchmark-40n.yaml (workloadType, workloadClass.cpu, memoryIntensity, ioIntensity, work.cpuUnits); regenerate with: {cmd}", "jobs": mix}
    pathlib.Path(jobmix).write_text(json.dumps(doc, separators=(",", ":")) + "\n")
    print(f"wrote {len(mix)} jobs to {jobmix}")
print(f"needed performance nodes of {N}: min {min(need_all)} mean {sum(need_all)/n_s:.1f} max {max(need_all)}; performance pods per needed node {per:.1f}")
print(f"  B: hp_frac >= {max(need_all)/N:.3f} ({max(need_all)} nodes)")
print(f"  C: hp_base_frac {min(need_all)/N:.3f} ({min(need_all)} nodes), perf_per_hp_node {int(per)}")
if full:
    print(f"  C holding every sample, pending pods left out: base {full[2]} nodes, {-full[1]} pods per node, {full[0]:.1f} nodes on average")
cc = c_counts(C_base, C_per)
print(f"configured: B {B} nodes holds the demand in {holds([B] * n_s)}/{n_s} samples; C (base {C_base}, {C_per} pods per node, {C_min} to {C_max}) "
      f"{min(cc)} to {max(cc)} nodes, mean {sum(cc)/n_s:.1f}, holds it in {holds(cc)}/{n_s}")
