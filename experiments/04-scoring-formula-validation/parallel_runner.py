#!/usr/bin/env python3
"""Parallel experiment runner: uses multiprocessing to run sweep variants across CPU cores.

Usage:
    python parallel_runner.py sweep_formulas   # run formula sweep in parallel
    python parallel_runner.py trend_sweep      # run trend sweep in parallel
    python parallel_runner.py ablation_sweep   # run ablation sweep in parallel
"""

import sys
import os
import json
import multiprocessing as mp
import numpy as np
from functools import partial

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48
MAX_WORKERS = min(20, mp.cpu_count() - 2)  # leave 2 cores free


def run_one(args):
    """Run a single (variant_name, seed) pair in a subprocess."""
    variant_name, seed, module_name = args

    # Import fresh in subprocess (gets its own global arrays)
    from sim_24h_pue import generate_workload, run_simulation, DT_SEC

    # Import the variant definitions
    if module_name == "sweep_formulas":
        from sweep_formulas import VARIANTS
    elif module_name == "trend_sweep":
        from trend_sweep import VARIANTS
    elif module_name == "ablation_sweep":
        from ablation_sweep import VARIANTS
    else:
        raise ValueError(f"Unknown module: {module_name}")

    rng_wl = np.random.default_rng(seed)
    jobs = generate_workload(SIM_HOURS, rng_wl)
    n_jobs = len(jobs)

    fn = VARIANTS[variant_name]
    if variant_name == "BASELINE":
        sched_name = "BASELINE"
    elif variant_name in ("JOULIE", "E-JOULIE", "JOULIE-default"):
        sched_name = "JOULIE"
        fn = None
    else:
        sched_name = variant_name

    rng_sim = np.random.default_rng(seed + 1)
    df = run_simulation(jobs, sched_name, SIM_HOURS, rng_sim, scheduler_fn=fn)
    deriv = np.diff(df["it_power_kw"].values)
    dropped = int(df["dropped"].sum())
    it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600

    result = {
        "variant": variant_name,
        "seed": seed,
        "it_kwh": it_kwh,
        "peak_kw": float(df["it_power_kw"].max()),
        "std_kw": float(df["it_power_kw"].std()),
        "dropped": dropped,
        "kwh_per_job": it_kwh / max(1, n_jobs - dropped),
        "deriv_std": float(deriv.std()),
        "p99_ramp": float(np.percentile(np.abs(deriv), 99)),
        "n_jobs": n_jobs,
    }
    print(f"  done: {variant_name:20s} seed={seed:5d}  IT_kWh={it_kwh:.1f}", flush=True)
    return result


def print_results(all_data, variant_names):
    """Print comparison table."""
    bl_data = {r["seed"]: r for r in all_data if r["variant"] == "BASELINE"}
    joulie_names = [n for n in variant_names if n != "BASELINE"]

    print(f"\n{'Variant':20s} | {'IT_kWh%':>8s} {'Peak%':>7s} {'Std%':>7s} {'Drop':>8s} {'kWh/j%':>8s} {'Ramp%':>7s} {'P99%':>7s} | Win(E+S)")
    print("-" * 110)

    for vname in joulie_names:
        vd = [r for r in all_data if r["variant"] == vname]
        it_d, pk_d, st_d, dr_d, kj_d, rp_d, p9_d = [], [], [], [], [], [], []
        for vm in vd:
            bm = bl_data[vm["seed"]]
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


def main():
    if len(sys.argv) < 2:
        print("Usage: python parallel_runner.py <module_name>")
        print("  module_name: sweep_formulas, trend_sweep, ablation_sweep")
        sys.exit(1)

    module_name = sys.argv[1]

    # Import to get variant names
    if module_name == "sweep_formulas":
        from sweep_formulas import VARIANTS
    elif module_name == "trend_sweep":
        from trend_sweep import VARIANTS
    elif module_name == "ablation_sweep":
        from ablation_sweep import VARIANTS
    else:
        sys.exit(f"Unknown module: {module_name}")

    variant_names = list(VARIANTS.keys())
    total_runs = len(variant_names) * len(SEEDS)

    print(f"Parallel {module_name}: {len(variant_names)} variants x {len(SEEDS)} seeds = {total_runs} runs")
    print(f"Using {MAX_WORKERS} workers on {mp.cpu_count()} cores\n", flush=True)

    # Build task list
    tasks = []
    for seed in SEEDS:
        for vname in variant_names:
            tasks.append((vname, seed, module_name))

    # Run in parallel
    with mp.Pool(processes=MAX_WORKERS) as pool:
        results = pool.map(run_one, tasks)

    print(f"\nAll {len(results)} runs complete.", flush=True)
    print_results(results, variant_names)


if __name__ == "__main__":
    mp.set_start_method("fork")
    main()
