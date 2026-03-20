#!/usr/bin/env python3
"""Trend strength sweep: 2500-node H100 cluster.
Finds optimal trend scale/threshold for the full Joulie formula."""

import numpy as np
from sim_24h_pue import (
    generate_workload, run_simulation, schedule_joulie_vec,
    DT_SEC, N_NODES,
    _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
    _alloc_cpu, _alloc_gpu, _measured_power,
    CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
    REFERENCE_NODE_POWER_W, REFERENCE_RACK_CAPACITY_W,
    IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP,
)

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48


def make_joulie_variant(steady_scale, burst_scale, burst_threshold):
    """Full Joulie formula with configurable trend parameters."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        # Feasibility
        cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
        if job_gpu > 0:
            gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
            mask = cpu_fit & gpu_fit
        else:
            mask = cpu_fit
        if not mask.any():
            return -1

        # Base: power headroom
        headroom_pct = np.maximum(0.0, (_peak_power - _measured_power) / np.maximum(_peak_power, 1.0)) * 100.0
        scores = headroom_pct * 0.4
        cooling_stress = np.minimum(100.0, (_measured_power / REFERENCE_NODE_POWER_W) * 80.0)
        scores += (100.0 - cooling_stress) * 0.3
        psu_stress = np.minimum(100.0, (_measured_power / REFERENCE_RACK_CAPACITY_W) * 100.0)
        scores += (100.0 - psu_stress) * 0.3

        # Marginal pod power
        util_share = np.minimum(1.0, job_cpu / np.maximum(_cpu_cores, 1))
        delta_cpu = _cpu_max_w * CPU_UTIL_COEFF * util_share
        delta_gpu = np.zeros(N_NODES)
        if job_gpu > 0:
            coeff = GPU_UTIL_COEFF_PERF if job_is_perf else GPU_UTIL_COEFF_STD
            delta_gpu = np.where(_has_gpu, job_gpu * _gpu_max_w_per * coeff, 0.0)
        scores -= np.minimum(20.0, np.maximum(0.0, (delta_cpu + delta_gpu) / 20.0))

        # Idle GPU waste
        if job_gpu == 0:
            idle_gpus = _gpu_count - _alloc_gpu
            waste = np.minimum(idle_gpus.astype(np.float64) * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
            scores -= np.where(_has_gpu, np.minimum(20.0, np.maximum(0.0, waste / 10.0)), 0.0)

        # Trend with custom parameters
        ts = burst_scale if abs(cluster_trend) > burst_threshold else steady_scale
        scores -= np.clip(trends / ts, -20.0, 25.0)

        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


VARIANTS = {
    "BASELINE": None,
    "JOULIE-default": None,  # Built-in (scale 6/2, threshold 500)
    # Fixed trend scale (no burst adaptation)
    "fixed-20": make_joulie_variant(20.0, 20.0, 1e9),
    "fixed-10": make_joulie_variant(10.0, 10.0, 1e9),
    "fixed-6": make_joulie_variant(6.0, 6.0, 1e9),
    "fixed-4": make_joulie_variant(4.0, 4.0, 1e9),
    "fixed-2": make_joulie_variant(2.0, 2.0, 1e9),
    # Adaptive: different burst thresholds
    "adapt-6/2-t200": make_joulie_variant(6.0, 2.0, 200.0),
    "adapt-6/2-t500": make_joulie_variant(6.0, 2.0, 500.0),
    "adapt-6/2-t1000": make_joulie_variant(6.0, 2.0, 1000.0),
    "adapt-8/3-t500": make_joulie_variant(8.0, 3.0, 500.0),
    "adapt-4/1-t500": make_joulie_variant(4.0, 1.0, 500.0),
    "adapt-10/4-t500": make_joulie_variant(10.0, 4.0, 500.0),
}


def main():
    print(f"Trend sweep: {len(VARIANTS)} variants x {len(SEEDS)} seeds, {N_NODES} nodes\n", flush=True)

    all_data = {}
    for seed in SEEDS:
        rng_wl = np.random.default_rng(seed)
        jobs = generate_workload(SIM_HOURS, rng_wl)
        n_jobs = len(jobs)

        for name in VARIANTS:
            if name not in all_data:
                all_data[name] = []
            fn = VARIANTS[name]
            if name == "BASELINE":
                sched_name = "BASELINE"
            elif name == "JOULIE-default":
                sched_name = "JOULIE"
                fn = None
            else:
                sched_name = name

            rng_sim = np.random.default_rng(seed + 1)
            df = run_simulation(jobs, sched_name, SIM_HOURS, rng_sim, scheduler_fn=fn)
            deriv = np.diff(df["it_power_kw"].values)
            dropped = int(df["dropped"].sum())
            it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600
            all_data[name].append({
                "seed": seed,
                "it_kwh": it_kwh,
                "peak_kw": df["it_power_kw"].max(),
                "std_kw": df["it_power_kw"].std(),
                "dropped": dropped,
                "kwh_per_job": it_kwh / max(1, n_jobs - dropped),
                "deriv_std": deriv.std(),
                "p99_ramp": np.percentile(np.abs(deriv), 99),
            })

        print(f"  seed={seed} done", flush=True)

    bl_data = all_data["BASELINE"]
    joulie_names = [n for n in VARIANTS if n != "BASELINE"]

    print(f"\n{'Variant':20s} | {'IT_kWh%':>8s} {'Peak%':>7s} {'Std%':>7s} {'Drop':>8s} {'kWh/j%':>8s} {'Ramp%':>7s} {'P99%':>7s} | Win(E+S)")
    print("-" * 110)

    for vname in joulie_names:
        vd = all_data[vname]
        it_d, pk_d, st_d, dr_d, kj_d, rp_d, p9_d = [], [], [], [], [], [], []
        for bm, vm in zip(bl_data, vd):
            it_d.append((vm["it_kwh"] - bm["it_kwh"]) / bm["it_kwh"] * 100)
            pk_d.append((vm["peak_kw"] - bm["peak_kw"]) / bm["peak_kw"] * 100)
            st_d.append((vm["std_kw"] - bm["std_kw"]) / bm["std_kw"] * 100)
            dr_d.append(vm["dropped"] - bm["dropped"])
            kj_d.append((vm["kwh_per_job"] - bm["kwh_per_job"]) / bm["kwh_per_job"] * 100)
            rp_d.append((vm["deriv_std"] - bm["deriv_std"]) / bm["deriv_std"] * 100)
            p9_d.append((vm["p99_ramp"] - bm["p99_ramp"]) / bm["p99_ramp"] * 100)

        it_d, pk_d, st_d = np.array(it_d), np.array(pk_d), np.array(st_d)
        dr_d, kj_d, rp_d, p9_d = np.array(dr_d), np.array(kj_d), np.array(rp_d), np.array(p9_d)

        e_wins = (it_d <= 0).sum()
        s_wins = (st_d <= 0).sum()

        print(f"{vname:20s} | {it_d.mean():+7.2f}% {pk_d.mean():+6.2f}% "
              f"{st_d.mean():+6.2f}% {dr_d.mean():+7.0f} "
              f"{kj_d.mean():+7.2f}% {rp_d.mean():+6.2f}% "
              f"{p9_d.mean():+6.2f}% | {e_wins}/{s_wins}")


if __name__ == "__main__":
    main()
