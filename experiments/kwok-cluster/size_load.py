#!/usr/bin/env python3
"""Size the load of a benchmark-40n experiment: solve work_scale so that the
utilization of the binding resource while the cluster is at regime, from R
to W seconds after the start (after the ramp-up, until the arrivals stop
and the run is stopped), reaches a target on the lighter seed.

Generates seeds 1 and 2 with the experiment's own trace code (05_sweep.py,
work_scale 1). Each job submitted by W runs from its submit offset for its
uncapped runtime, cpuUnits / (base_speed_per_core x cores) or gpuUnits /
(base_speed_per_core x max(0.1, GPUs)), as the simulator runs it
(simulator/pkg/workload/rates.go); only the part inside [R, W] counts.
Requested load is sum(requested cores or GPUs x time in [R, W]) / ((W - R) x
cluster capacity); utilization load weights each job by its workloadProfile
utilization, the GPU one multiplied by GPU_UTIL_SCALE (at most 1), as the simulator weights utilization by
capacity. It assumes no queueing and no caps, so a run measures somewhat
less.

Usage, from the repository root:
    python experiments/kwok-cluster/size_load.py EXPERIMENT R W TARGET cpu|gpu [GPU_UTIL_SCALE]
    python experiments/kwok-cluster/size_load.py 02-heterogeneous-benchmark 1620 2400 0.50 gpu 1.25
"""
import importlib.util, json, pathlib, sys, tempfile
from unittest.mock import patch, MagicMock
if len(sys.argv) not in (6, 7):
    sys.exit(__doc__)
exp, R, W, target, kind = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4]), sys.argv[5]
gscale = float(sys.argv[6]) if len(sys.argv) > 6 else 1.0
repo = pathlib.Path.cwd()
sp = importlib.util.spec_from_file_location("sw", repo / f"experiments/{exp}/scripts/05_sweep.py")
sw = importlib.util.module_from_spec(sp)
with patch("subprocess.run", return_value=MagicMock(stdout="{}", returncode=0)):
    sp.loader.exec_module(sw)
import yaml
cfg = yaml.safe_load(open(repo / f"experiments/{exp}/configs/benchmark-40n.yaml"))
w, run = cfg["workload"], cfg["run"]
inv = yaml.safe_load(open(repo / cfg["inventory"]["source"]))
cores = sum(n.get("replicas", 1) * float(n.get("cpu_cores", 0)) for n in inv["nodes"])
gpus = sum(n.get("replicas", 1) * float(n.get("gpu_count", 0)) for n in inv["nodes"])
speed = float(cfg["simulator"]["base_speed_per_core"])
tmp = pathlib.Path(tempfile.mkdtemp())
sw.RESULTS = tmp
jobs_by_seed = {}
for seed in (1, 2):
    p = sw.generate_canonical_seed_trace(seed=seed, jobs=run["jobs"], mean_inter_arrival_sec=run["mean_inter_arrival_sec"],
        perf_ratio=w["perf_ratio"], compute_bound_perf_boost=w.get("compute_bound_perf_boost", 3.5), gpu_ratio=w["gpu_ratio"],
        burst_day_probability=w["burst_day_probability"], burst_mean_jobs=w["burst_mean_jobs"], burst_multiplier=w["burst_multiplier"],
        dip_day_probability=w["dip_day_probability"], dip_multiplier=w["dip_multiplier"], emit_workload_records=w["emit_workload_records"],
        work_scale=1.0, allowed_workload_types=w.get("allowed_workload_types"), time_scale=run["time_scale"])
    jobs_by_seed[seed] = [json.loads(l) for l in open(p) if '"type":"job"' in l]
def load(jobs, ws):
    rc = uc = rg = ug = 0.0; n = 0
    for j in jobs:
        s0 = j["submitTimeOffsetSec"]
        if s0 > W: continue
        n += 1
        r = j["podTemplate"]["requests"]; c = sw._quantity(r.get("cpu")); g = sw._quantity(r.get("nvidia.com/gpu") or r.get("amd.com/gpu"))
        wk = j["work"]; prof = j.get("workloadProfile") or {}
        t = max(wk["cpuUnits"] * ws / (speed * c) if c else 0, wk["gpuUnits"] * ws / (speed * max(0.1, g)) if wk["gpuUnits"] else 0)
        o = max(0.0, min(s0 + t, W) - max(s0, R))
        rc += c * o; uc += c * float(prof.get("cpuUtilization", 1)) * o
        rg += g * o; ug += g * min(1.0, float(prof.get("gpuUtilization", 1)) * gscale) * o
    span = W - R
    return n, rc / (span * cores), uc / (span * cores), (rg / (span * gpus) if gpus else 0), (ug / (span * gpus) if gpus else 0)
idx = 2 if kind == "cpu" else 4
lo, hi = 1e-4, 50.0
for _ in range(60):
    mid = (lo * hi) ** 0.5
    lighter = min(load(jobs_by_seed[seed], mid)[idx] for seed in (1, 2))
    lo, hi = (mid, hi) if lighter < target else (lo, mid)
ws = lo
for seed in (1, 2):
    n, rc, uc, rg, ug = load(jobs_by_seed[seed], ws)
    print(f"{exp} seed {seed}: ws={ws:.4f} jobs={n} req_cpu={rc:.2f} util_cpu={uc:.2f} req_gpu={rg:.2f} util_gpu={ug:.2f}")
