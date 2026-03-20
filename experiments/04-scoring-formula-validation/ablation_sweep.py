#!/usr/bin/env python3
"""Ablation study: 2500-node H100 cluster.
Tests which Joulie formula components consistently help vs baseline.

Components:
  A. Power headroom only (no pod-power, no GPU waste, no trend)
  B. Headroom + marginal pod-power penalty
  C. Headroom + idle GPU waste penalty
  D. Headroom + trend only
  E. Full Joulie (headroom + pod-power + GPU waste + trend)
"""

import numpy as np
from sim_24h_pue import (
    generate_workload, run_simulation,
    DT_SEC, N_NODES,
    _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
    _alloc_cpu, _alloc_gpu, _measured_power,
    CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
    REFERENCE_NODE_POWER_W, REFERENCE_RACK_CAPACITY_W,
    IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP,
)

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48


def _base_headroom_scores():
    """Base score = powerHeadroom * 0.4 + (100-cooling) * 0.3 + (100-psu) * 0.3"""
    headroom_pct = np.maximum(0.0, (_peak_power - _measured_power) / np.maximum(_peak_power, 1.0)) * 100.0
    scores = headroom_pct * 0.4
    cooling_stress = np.minimum(100.0, (_measured_power / REFERENCE_NODE_POWER_W) * 80.0)
    scores += (100.0 - cooling_stress) * 0.3
    psu_stress = np.minimum(100.0, (_measured_power / REFERENCE_RACK_CAPACITY_W) * 100.0)
    scores += (100.0 - psu_stress) * 0.3
    return scores


def _feasibility_mask(job_cpu, job_gpu):
    cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
    if job_gpu > 0:
        gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
        return cpu_fit & gpu_fit
    return cpu_fit


def make_headroom_only():
    """A: Power headroom scoring, no penalties."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        mask = _feasibility_mask(job_cpu, job_gpu)
        if not mask.any():
            return -1
        scores = _base_headroom_scores()
        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


def make_headroom_plus_podpower():
    """B: Headroom + estimated pod-power penalty."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        mask = _feasibility_mask(job_cpu, job_gpu)
        if not mask.any():
            return -1
        scores = _base_headroom_scores()
        util_share = np.minimum(1.0, job_cpu / np.maximum(_cpu_cores, 1))
        delta_cpu = _cpu_max_w * CPU_UTIL_COEFF * util_share
        delta_gpu = np.zeros(N_NODES)
        if job_gpu > 0:
            coeff = GPU_UTIL_COEFF_PERF if job_is_perf else GPU_UTIL_COEFF_STD
            delta_gpu = np.where(_has_gpu, job_gpu * _gpu_max_w_per * coeff, 0.0)
        scores -= np.minimum(20.0, np.maximum(0.0, (delta_cpu + delta_gpu) / 20.0))
        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


def make_headroom_plus_gpuwaste():
    """C: Headroom + idle GPU waste penalty."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        mask = _feasibility_mask(job_cpu, job_gpu)
        if not mask.any():
            return -1
        scores = _base_headroom_scores()
        if job_gpu == 0:
            idle_gpus = _gpu_count - _alloc_gpu
            waste = np.minimum(idle_gpus.astype(np.float64) * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
            scores -= np.where(_has_gpu, np.minimum(20.0, np.maximum(0.0, waste / 10.0)), 0.0)
        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


def make_headroom_plus_trend():
    """D: Headroom + adaptive trend only."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        mask = _feasibility_mask(job_cpu, job_gpu)
        if not mask.any():
            return -1
        scores = _base_headroom_scores()
        trend_scale = 2.0 if abs(cluster_trend) > 500.0 else 6.0
        scores -= np.clip(trends / trend_scale, -20.0, 25.0)
        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


VARIANTS = {
    "BASELINE": None,
    "A-headroom": make_headroom_only(),
    "B-head+pod": make_headroom_plus_podpower(),
    "C-head+gpu": make_headroom_plus_gpuwaste(),
    "D-head+trend": make_headroom_plus_trend(),
    "E-JOULIE": None,  # full Joulie from sim_24h_pue
}


def main():
    print(f"Ablation study: {len(VARIANTS)} variants x {len(SEEDS)} seeds x {SIM_HOURS}h, {N_NODES} nodes\n", flush=True)

    all_results = {name: [] for name in VARIANTS}

    for seed in SEEDS:
        rng_wl = np.random.default_rng(seed)
        jobs = generate_workload(SIM_HOURS, rng_wl)
        n_jobs = len(jobs)

        for name in VARIANTS:
            fn = VARIANTS[name]
            if name == "BASELINE":
                sched_name = "BASELINE"
            elif name == "E-JOULIE":
                sched_name = "JOULIE"
                fn = None
            else:
                sched_name = name  # custom scheduler

            rng_sim = np.random.default_rng(seed + 1)
            df = run_simulation(jobs, sched_name, SIM_HOURS, rng_sim, scheduler_fn=fn)
            deriv = np.diff(df["it_power_kw"].values)
            dropped = int(df["dropped"].sum())
            it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600
            all_results[name].append({
                "seed": seed,
                "avg_kw": df["it_power_kw"].mean(),
                "peak_kw": df["it_power_kw"].max(),
                "std_kw": df["it_power_kw"].std(),
                "dropped": dropped,
                "it_kwh": it_kwh,
                "kwh_per_job": it_kwh / max(1, n_jobs - dropped),
                "deriv_std": deriv.std(),
                "p99_ramp": np.percentile(np.abs(deriv), 99),
            })

        print(f"  seed={seed} done", flush=True)

    bl_results = all_results["BASELINE"]
    joulie_names = [n for n in VARIANTS if n != "BASELINE"]

    print(f"\n{'Variant':18s} | {'IT kWh%':>9s} {'Peak%':>7s} {'Std%':>7s} {'Drops':>8s} {'kWh/j%':>9s} {'Ramp%':>7s} {'P99%':>7s} | {'Wins/8':>6s}")
    print("-" * 110)

    for vname in joulie_names:
        vr = all_results[vname]
        d = {"it": [], "pk": [], "st": [], "dr": [], "kj": [], "rp": [], "p9": []}
        for bm, vm in zip(bl_results, vr):
            d["it"].append((vm["it_kwh"] - bm["it_kwh"]) / bm["it_kwh"] * 100)
            d["pk"].append((vm["peak_kw"] - bm["peak_kw"]) / bm["peak_kw"] * 100)
            d["st"].append((vm["std_kw"] - bm["std_kw"]) / bm["std_kw"] * 100)
            d["dr"].append(vm["dropped"] - bm["dropped"])
            d["kj"].append((vm["kwh_per_job"] - bm["kwh_per_job"]) / bm["kwh_per_job"] * 100)
            d["rp"].append((vm["deriv_std"] - bm["deriv_std"]) / bm["deriv_std"] * 100)
            d["p9"].append((vm["p99_ramp"] - bm["p99_ramp"]) / bm["p99_ramp"] * 100)

        da = {k: np.array(v) for k, v in d.items()}
        wins = sum(1 for i in range(len(SEEDS)) if da["it"][i] <= 0 and da["st"][i] <= 0)

        print(f"{vname:18s} | {da['it'].mean():+8.2f}% {da['pk'].mean():+6.2f}% "
              f"{da['st'].mean():+6.2f}% {da['dr'].mean():+7.0f} "
              f"{da['kj'].mean():+8.2f}% {da['rp'].mean():+6.2f}% "
              f"{da['p9'].mean():+6.2f}% | {wins:3d}/8")


if __name__ == "__main__":
    main()
