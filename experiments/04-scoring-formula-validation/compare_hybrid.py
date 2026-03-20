#!/usr/bin/env python3
"""Compare hybrid formulas: redesign's projected headroom + varying trend strengths.

Tests BASELINE, JOULIE (current), REDESIGN (±10 trend), and hybrids with ±15, ±20, ±25, ±30 trend.
Also tests adding GPU waste penalty back to redesign.
Runs all variants × 8 seeds using multiprocessing.
"""

import sys
import multiprocessing as mp
import numpy as np

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48
MAX_WORKERS = min(24, mp.cpu_count() - 2)


def make_hybrid_scheduler(trend_cap=10.0, add_gpu_waste=False, add_adaptive_trend=False):
    """Redesign base + configurable trend strength and optional GPU waste penalty.

    trend_cap: max absolute trend bonus (redesign default = 10)
    add_gpu_waste: if True, add idle GPU waste penalty like current Joulie
    add_adaptive_trend: if True, use adaptive trend scaling (2.0 during bursts, 6.0 steady)
    """
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        from sim_24h_pue import (
            _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
            _alloc_cpu, _alloc_gpu, _measured_power,
            CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
            N_NODES, GPU_IDLE_WATTS_PER_GPU,
            IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP,
        )

        # Feasibility mask
        cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
        if job_gpu > 0:
            gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
            mask = cpu_fit & gpu_fit
        else:
            mask = cpu_fit
        if not mask.any():
            return -1

        # --- Estimate pod marginal power ---
        util_share = np.minimum(1.0, job_cpu / np.maximum(_cpu_cores, 1))
        delta_cpu = _cpu_max_w * CPU_UTIL_COEFF * util_share
        delta_gpu = np.zeros(N_NODES)
        if job_gpu > 0:
            coeff = GPU_UTIL_COEFF_PERF if job_is_perf else GPU_UTIL_COEFF_STD
            delta_gpu = np.where(_has_gpu, job_gpu * _gpu_max_w_per * coeff, 0.0)
        pod_marginal = delta_cpu + delta_gpu

        # --- 1. Projected headroom (marginal baked in) ---
        projected_power = _measured_power + pod_marginal
        headroom_score = np.maximum(0.0,
            (_peak_power - projected_power) / np.maximum(_peak_power, 1.0) * 100.0)

        # --- 2. Cooling stress (against TDP) ---
        cooling_stress = np.minimum(100.0, (_measured_power / np.maximum(_peak_power, 1.0)) * 100.0)

        # --- 3. Trend ---
        if add_adaptive_trend:
            # Adaptive: stronger during bursts (like current Joulie but applied to redesign base)
            trend_scale = 2.0 if abs(cluster_trend) > 500.0 else 6.0
            trend_bonus = -np.clip(trends / trend_scale, -trend_cap, trend_cap)
        else:
            # Redesign-style: normalized against 10% of TDP
            trend_ref = _peak_power * 0.1
            trend_score = np.clip(trends / np.maximum(trend_ref, 1.0), -1.0, 1.0)
            trend_bonus = -trend_score * trend_cap

        # --- Combined ---
        scores = headroom_score * 0.7 + (100.0 - cooling_stress) * 0.15 + trend_bonus

        # --- Optional: GPU waste penalty ---
        if add_gpu_waste and job_gpu == 0:
            idle_gpus = _gpu_count - _alloc_gpu
            waste = np.minimum(idle_gpus.astype(np.float64) * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
            scores -= np.where(_has_gpu, np.minimum(20.0, np.maximum(0.0, waste / 10.0)), 0.0)

        scores = np.clip(scores, 0.0, 100.0)
        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


VARIANTS = {
    "BASELINE": None,
    "JOULIE": None,  # current best
    "REDESIGN-t10": ("redesign ±10 (original)", dict(trend_cap=10.0)),
    "HYBRID-t15": ("redesign ±15 trend", dict(trend_cap=15.0)),
    "HYBRID-t20": ("redesign ±20 trend", dict(trend_cap=20.0)),
    "HYBRID-t25": ("redesign ±25 trend", dict(trend_cap=25.0)),
    "HYBRID-t30": ("redesign ±30 trend", dict(trend_cap=30.0)),
    "HYBRID-t20-gpu": ("redesign ±20 + GPU waste", dict(trend_cap=20.0, add_gpu_waste=True)),
    "HYBRID-t20-adap": ("redesign ±20 adaptive trend", dict(trend_cap=20.0, add_adaptive_trend=True)),
    "HYBRID-t25-adap": ("redesign ±25 adaptive trend", dict(trend_cap=25.0, add_adaptive_trend=True)),
}


def run_one(args):
    """Run a single (variant_name, seed) in a subprocess."""
    variant_name, seed = args

    from sim_24h_pue import generate_workload, run_simulation, DT_SEC

    rng_wl = np.random.default_rng(seed)
    jobs = generate_workload(SIM_HOURS, rng_wl)
    n_jobs = len(jobs)

    scheduler_fn = None
    sched_name = variant_name

    if variant_name == "BASELINE":
        sched_name = "BASELINE"
    elif variant_name == "JOULIE":
        sched_name = "JOULIE"
    else:
        # It's a hybrid variant
        _, kwargs = VARIANTS[variant_name]
        scheduler_fn = make_hybrid_scheduler(**kwargs)

    rng_sim = np.random.default_rng(seed + 1)
    df = run_simulation(jobs, sched_name, SIM_HOURS, rng_sim, scheduler_fn=scheduler_fn)
    deriv = np.diff(df["it_power_kw"].values)
    dropped = int(df["dropped"].sum())
    it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600

    result = {
        "sched": variant_name,
        "seed": seed,
        "it_kwh": it_kwh,
        "avg_kw": float(df["it_power_kw"].mean()),
        "peak_kw": float(df["it_power_kw"].max()),
        "std_kw": float(df["it_power_kw"].std()),
        "dropped": dropped,
        "n_jobs": n_jobs,
        "kwh_per_job": it_kwh / max(1, n_jobs - dropped),
        "deriv_std": float(deriv.std()),
        "p99_ramp": float(np.percentile(np.abs(deriv), 99)),
    }
    print(f"  done: {variant_name:20s} seed={seed:5d}  IT={result['avg_kw']:.0f}kW  "
          f"std={result['std_kw']:.1f}  drops={dropped}", flush=True)
    return result


def main():
    variant_names = list(VARIANTS.keys())
    tasks = [(v, seed) for seed in SEEDS for v in variant_names]
    total = len(tasks)

    print(f"Hybrid sweep: {len(variant_names)} variants x {len(SEEDS)} seeds = {total} runs")
    print(f"Using {MAX_WORKERS} workers on {mp.cpu_count()} cores\n", flush=True)

    with mp.Pool(processes=MAX_WORKERS) as pool:
        results = pool.map(run_one, tasks)

    # Organize by variant
    by_var = {}
    for r in results:
        by_var.setdefault(r["sched"], {})[r["seed"]] = r

    bl = by_var["BASELINE"]

    # Print comparison table
    print(f"\n{'='*110}")
    print(f"{'Variant':20s} | {'IT kWh%':>8s} {'Peak%':>7s} {'Std%':>7s} {'Drops':>10s} "
          f"{'kWh/j%':>8s} {'P99%':>7s} | {'E-wins':>6s}")
    print("-" * 110)

    for vname in variant_names:
        if vname == "BASELINE":
            continue
        sd = by_var[vname]
        it_d, pk_d, st_d, dr_d, kj_d, p9_d = [], [], [], [], [], []
        for seed in SEEDS:
            b = bl[seed]
            v = sd[seed]
            it_d.append((v["it_kwh"] - b["it_kwh"]) / b["it_kwh"] * 100)
            pk_d.append((v["peak_kw"] - b["peak_kw"]) / b["peak_kw"] * 100)
            st_d.append((v["std_kw"] - b["std_kw"]) / b["std_kw"] * 100)
            dr_d.append(v["dropped"] - b["dropped"])
            kj_d.append((v["kwh_per_job"] - b["kwh_per_job"]) / b["kwh_per_job"] * 100)
            p9_d.append((v["p99_ramp"] - b["p99_ramp"]) / b["p99_ramp"] * 100)

        it_d = np.array(it_d)
        pk_d = np.array(pk_d)
        st_d = np.array(st_d)
        dr_d = np.array(dr_d)
        kj_d = np.array(kj_d)
        p9_d = np.array(p9_d)

        e_wins = (it_d < 0).sum()
        desc = ""
        if vname in VARIANTS and VARIANTS[vname] is not None:
            desc = f" ({VARIANTS[vname][0]})"
        print(f"{vname:20s} | {it_d.mean():+7.2f}% {pk_d.mean():+6.2f}% "
              f"{st_d.mean():+6.2f}% {dr_d.mean():+9.0f} "
              f"{kj_d.mean():+7.2f}% {p9_d.mean():+6.2f}% | {e_wins}/8")

    # Rank by combined score: energy savings + smoothing
    print(f"\n--- Ranking by combined score (energy% - 0.5*abs(std%)) ---")
    rankings = []
    for vname in variant_names:
        if vname == "BASELINE":
            continue
        sd = by_var[vname]
        it_deltas = [(sd[s]["it_kwh"] - bl[s]["it_kwh"]) / bl[s]["it_kwh"] * 100 for s in SEEDS]
        std_deltas = [(sd[s]["std_kw"] - bl[s]["std_kw"]) / bl[s]["std_kw"] * 100 for s in SEEDS]
        it_mean = np.mean(it_deltas)
        std_mean = np.mean(std_deltas)
        # Lower is better for both: energy should be negative, std should be negative
        # Combined: energy_savings - penalty_for_worse_std
        combined = it_mean - 0.3 * max(0, std_mean)  # penalize positive std (worse smoothing)
        rankings.append((vname, it_mean, std_mean, combined))

    rankings.sort(key=lambda x: x[3])
    for rank, (vname, it_m, std_m, comb) in enumerate(rankings, 1):
        print(f"  {rank}. {vname:20s}  energy={it_m:+.2f}%  std={std_m:+.2f}%  combined={comb:+.2f}")


if __name__ == "__main__":
    mp.set_start_method("fork")
    main()
