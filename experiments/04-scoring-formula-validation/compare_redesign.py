#!/usr/bin/env python3
"""Compare current Joulie formula vs scoring-redesign.md proposal.

Runs BASELINE, JOULIE (current), and REDESIGN across 8 seeds using multiprocessing.
"""

import sys
import multiprocessing as mp
import numpy as np

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48
MAX_WORKERS = min(14, mp.cpu_count() - 2)


def make_redesign_scheduler():
    """Scoring from docs/design/scoring-redesign.md.

    score = projectedHeadroom * 0.7
          + (100 - coolingStress) * 0.15
          + trendBonus  (±10)

    Key differences from current Joulie:
    - Marginal pod power is SUBTRACTED from headroom before scoring (not separate penalty)
    - Cooling stress uses nodeTDP (not reference constant)
    - Trend normalized against 10% of TDP, capped ±10 points
    - No separate GPU waste penalty (absorbed into headroom via marginal)
    - No PSU stress term
    """
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        from sim_24h_pue import (
            _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
            _alloc_cpu, _alloc_gpu, _measured_power,
            CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
            N_NODES, GPU_IDLE_WATTS_PER_GPU,
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
        # Using peak_power as cappedPower (= TDP, i.e. performance profile / no cap)
        projected_power = _measured_power + pod_marginal
        headroom_score = np.maximum(0.0,
            (_peak_power - projected_power) / np.maximum(_peak_power, 1.0) * 100.0)

        # --- 2. Cooling stress (against TDP, not reference constant) ---
        # No ambient temp in our sim's scheduling loop, so tempMultiplier = 1.0
        cooling_stress = np.minimum(100.0, (_measured_power / np.maximum(_peak_power, 1.0)) * 100.0)

        # --- 3. Trend bonus (±10) ---
        # trendReferenceW = nodeTDP * 0.1
        trend_ref = _peak_power * 0.1
        trend_score = np.clip(trends / np.maximum(trend_ref, 1.0), -1.0, 1.0)
        trend_bonus = -trend_score * 10.0

        # --- Combined ---
        scores = headroom_score * 0.7 + (100.0 - cooling_stress) * 0.15 + trend_bonus
        scores = np.clip(scores, 0.0, 100.0)

        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


def run_one(args):
    """Run a single (scheduler_name, seed) in a subprocess."""
    sched_name, seed = args

    from sim_24h_pue import generate_workload, run_simulation, DT_SEC

    rng_wl = np.random.default_rng(seed)
    jobs = generate_workload(SIM_HOURS, rng_wl)
    n_jobs = len(jobs)

    scheduler_fn = None
    if sched_name == "REDESIGN":
        scheduler_fn = make_redesign_scheduler()

    rng_sim = np.random.default_rng(seed + 1)
    df = run_simulation(jobs, sched_name, SIM_HOURS, rng_sim, scheduler_fn=scheduler_fn)
    deriv = np.diff(df["it_power_kw"].values)
    dropped = int(df["dropped"].sum())
    it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600

    result = {
        "sched": sched_name,
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
    print(f"  done: {sched_name:12s} seed={seed:5d}  IT={result['avg_kw']:.0f}kW  "
          f"std={result['std_kw']:.1f}  drops={dropped}", flush=True)
    return result


def main():
    schedulers = ["BASELINE", "JOULIE", "REDESIGN"]
    tasks = [(s, seed) for seed in SEEDS for s in schedulers]
    total = len(tasks)

    print(f"Comparing {len(schedulers)} schedulers x {len(SEEDS)} seeds = {total} runs")
    print(f"Using {MAX_WORKERS} workers on {mp.cpu_count()} cores\n", flush=True)

    with mp.Pool(processes=MAX_WORKERS) as pool:
        results = pool.map(run_one, tasks)

    # Organize by scheduler
    by_sched = {}
    for r in results:
        by_sched.setdefault(r["sched"], {})[r["seed"]] = r

    # Print comparison
    bl = by_sched["BASELINE"]
    print(f"\n{'='*90}")
    print(f"{'Scheduler':12s} | {'IT kWh%':>8s} {'Peak%':>7s} {'Std%':>7s} {'Drops':>10s} {'kWh/j%':>8s} {'P99%':>7s} | {'Wins':>5s}")
    print("-" * 90)

    for sname in ["JOULIE", "REDESIGN"]:
        sd = by_sched[sname]
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
        print(f"{sname:12s} | {it_d.mean():+7.2f}% {pk_d.mean():+6.2f}% "
              f"{st_d.mean():+6.2f}% {dr_d.mean():+9.0f} "
              f"{kj_d.mean():+7.2f}% {p9_d.mean():+6.2f}% | {e_wins}/8")

    # Per-seed detail
    print(f"\nPer-seed IT energy delta (%):")
    print(f"{'Seed':>8s} {'JOULIE':>10s} {'REDESIGN':>10s} {'Winner':>10s}")
    for seed in SEEDS:
        b = bl[seed]
        j_d = (by_sched["JOULIE"][seed]["it_kwh"] - b["it_kwh"]) / b["it_kwh"] * 100
        r_d = (by_sched["REDESIGN"][seed]["it_kwh"] - b["it_kwh"]) / b["it_kwh"] * 100
        winner = "JOULIE" if j_d < r_d else "REDESIGN"
        print(f"{seed:8d} {j_d:+9.2f}% {r_d:+9.2f}% {winner:>10s}")

    # Summary
    j_mean = np.mean([(by_sched["JOULIE"][s]["it_kwh"] - bl[s]["it_kwh"]) / bl[s]["it_kwh"] * 100 for s in SEEDS])
    r_mean = np.mean([(by_sched["REDESIGN"][s]["it_kwh"] - bl[s]["it_kwh"]) / bl[s]["it_kwh"] * 100 for s in SEEDS])
    print(f"\nMean IT energy: JOULIE={j_mean:+.2f}%  REDESIGN={r_mean:+.2f}%")
    if r_mean < j_mean:
        print(f"REDESIGN is better by {j_mean - r_mean:.2f} percentage points")
    else:
        print(f"JOULIE is better by {r_mean - j_mean:.2f} percentage points")


if __name__ == "__main__":
    mp.set_start_method("fork")
    main()
