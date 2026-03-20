#!/usr/bin/env python3
"""Run BASELINE, JOULIE, and REDESIGN through full FMU co-simulation for PUE comparison.

Produces 7-panel comparison plot with PUE, COP, cooling power, etc.
"""

import argparse
import os
import pathlib
import sys
import numpy as np
import pandas as pd

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.dates as mdates

# Import from the main simulation module
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from sim_24h_pue import (
    generate_workload, run_simulation, run_fmu_cooling, plot_comparison,
    DT_SEC, N_NODES, DEFAULT_FMU,
    NUM_CPU_NODES, NUM_GPU_NODES, GPU_COUNT_PER_NODE, GPU_MAX_WATTS_PER_GPU,
    CPU_MAX_WATTS, GPU_NODE_CPU_MAX_WATTS,
    _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
    _alloc_cpu, _alloc_gpu, _measured_power,
    CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
    GPU_IDLE_WATTS_PER_GPU,
)


def make_redesign_scheduler():
    """Scoring from docs/design/scoring-redesign.md."""
    def sched(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
        from sim_24h_pue import (
            _cpu_cores, _has_gpu, _gpu_count, _peak_power, _cpu_max_w, _gpu_max_w_per,
            _alloc_cpu, _alloc_gpu, _measured_power,
            CPU_UTIL_COEFF, GPU_UTIL_COEFF_STD, GPU_UTIL_COEFF_PERF,
            N_NODES, GPU_IDLE_WATTS_PER_GPU,
        )

        cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
        if job_gpu > 0:
            gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
            mask = cpu_fit & gpu_fit
        else:
            mask = cpu_fit
        if not mask.any():
            return -1

        # Pod marginal power
        util_share = np.minimum(1.0, job_cpu / np.maximum(_cpu_cores, 1))
        delta_cpu = _cpu_max_w * CPU_UTIL_COEFF * util_share
        delta_gpu = np.zeros(N_NODES)
        if job_gpu > 0:
            coeff = GPU_UTIL_COEFF_PERF if job_is_perf else GPU_UTIL_COEFF_STD
            delta_gpu = np.where(_has_gpu, job_gpu * _gpu_max_w_per * coeff, 0.0)
        pod_marginal = delta_cpu + delta_gpu

        # 1. Projected headroom (marginal baked in)
        projected_power = _measured_power + pod_marginal
        headroom_score = np.maximum(0.0,
            (_peak_power - projected_power) / np.maximum(_peak_power, 1.0) * 100.0)

        # 2. Cooling stress (against TDP)
        cooling_stress = np.minimum(100.0, (_measured_power / np.maximum(_peak_power, 1.0)) * 100.0)

        # 3. Trend bonus (±10)
        trend_ref = _peak_power * 0.1
        trend_score = np.clip(trends / np.maximum(trend_ref, 1.0), -1.0, 1.0)
        trend_bonus = -trend_score * 10.0

        # Combined
        scores = headroom_score * 0.7 + (100.0 - cooling_stress) * 0.15 + trend_bonus
        scores = np.clip(scores, 0.0, 100.0)

        scores[~mask] = -1e9
        return int(np.argmax(scores))
    return sched


def plot_three_way(results, outdir, sim_hours):
    """7-panel comparison plot for 3 schedulers."""
    colors = {"BASELINE": "#d62728", "JOULIE": "#1f77b4", "REDESIGN": "#2ca02c"}
    fig, axes = plt.subplots(7, 1, figsize=(16, 24), sharex=True)
    fig.suptitle(
        f"Datacenter Simulation: {sim_hours:.0f}h, {N_NODES}-node H100 Cluster\n"
        f"BASELINE vs JOULIE (current) vs REDESIGN (scoring-redesign.md)\n"
        f"PUE from Modelica DXCooledAirsideEconomizer FMU",
        fontsize=14, fontweight="bold",
    )
    for sn, df in results.items():
        c = colors.get(sn, "#333")
        ts = df["timestamp"]
        axes[0].plot(ts, df["active_jobs"], label=sn, color=c, alpha=0.8, lw=0.8)
        axes[1].plot(ts, df["it_power_kw"], label=sn, color=c, alpha=0.8, lw=0.8)
        if sn == "BASELINE":
            axes[2].plot(ts, df["outdoor_temp_c"], color="orange", alpha=0.7, lw=0.8, label="Outdoor")
        axes[2].plot(ts, df["indoor_temp_c"], color=c, alpha=0.8, lw=0.8, ls="--", label=f"Indoor ({sn})")
        axes[3].plot(ts, df["cop"], label=sn, color=c, alpha=0.8, lw=0.8)
        axes[4].plot(ts, df["pue"], label=sn, color=c, alpha=0.8, lw=0.8)
        axes[5].plot(ts, df["cooling_power_kw"], label=sn, color=c, alpha=0.8, lw=0.8)
        axes[6].plot(ts, df["facility_power_kw"], label=sn, color=c, alpha=0.8, lw=0.8)
    for i, (yl, xl) in enumerate([("Active Jobs",None),("IT Power (kW)",None),("Temperature (C)",None),
        ("COP (FMU)",None),("PUE (FMU)",None),("Cooling Power (kW)",None),("Facility Power (kW)","Time")]):
        axes[i].set_ylabel(yl)
        if xl: axes[i].set_xlabel(xl)
        axes[i].legend(loc="upper right", fontsize=8)
        axes[i].grid(alpha=0.3)
    for ax in axes:
        ax.xaxis.set_major_formatter(mdates.DateFormatter("%a %H:%M"))
        ax.xaxis.set_major_locator(mdates.HourLocator(interval=max(1, int(sim_hours/24)*3)))
    plt.tight_layout()
    path = os.path.join(outdir, "comparison_redesign_fmu.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"Saved {path}", flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hours", type=float, default=48)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--outdir", type=str, default="./results_redesign_fmu")
    parser.add_argument("--fmu", type=str, default="")
    args = parser.parse_args()
    os.makedirs(args.outdir, exist_ok=True)

    fmu_path = pathlib.Path(args.fmu).resolve() if args.fmu else DEFAULT_FMU
    if not fmu_path.exists():
        sys.exit(f"ERROR: FMU not found: {fmu_path}")

    peak_mw = (NUM_CPU_NODES * CPU_MAX_WATTS + NUM_GPU_NODES * (GPU_NODE_CPU_MAX_WATTS + GPU_COUNT_PER_NODE * GPU_MAX_WATTS_PER_GPU)) / 1e6
    print(f"FMU: {fmu_path.name}", flush=True)
    print(f"Cluster: {NUM_CPU_NODES} CPU + {NUM_GPU_NODES} GPU ({GPU_COUNT_PER_NODE}x H100 @ {GPU_MAX_WATTS_PER_GPU:.0f}W)", flush=True)
    print(f"Peak capacity: {peak_mw:.1f} MW", flush=True)

    print(f"\nGenerating {args.hours}h workload...", flush=True)
    rng_wl = np.random.default_rng(args.seed)
    jobs = generate_workload(args.hours, rng_wl)
    print(f"  {len(jobs)} jobs generated", flush=True)

    # Run all 3 schedulers
    redesign_fn = make_redesign_scheduler()
    schedulers = [
        ("BASELINE", "BASELINE", None),
        ("JOULIE", "JOULIE", None),
        ("REDESIGN", "REDESIGN", redesign_fn),
    ]

    raw_results = {}
    for label, sched_name, fn in schedulers:
        print(f"\nRunning {label}...", flush=True)
        rng_sim = np.random.default_rng(args.seed + 1)
        df = run_simulation(jobs, sched_name, args.hours, rng_sim, scheduler_fn=fn)
        raw_results[label] = df
        deriv = np.diff(df["it_power_kw"].values)
        dropped = int(df["dropped"].sum())
        n_completed = len(jobs) - dropped
        it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600
        print(f"  Avg IT: {df['it_power_kw'].mean():.0f} kW, Peak: {df['it_power_kw'].max():.0f} kW", flush=True)
        print(f"  Std: {df['it_power_kw'].std():.1f} kW, Drops: {dropped}", flush=True)
        print(f"  IT energy: {it_kwh:.1f} kWh, kWh/job: {it_kwh/max(1,n_completed):.4f}", flush=True)

    # Run FMU for all 3
    print(f"\n{'='*60}\nRunning FMU co-simulation for all schedulers...\n{'='*60}", flush=True)
    results = {}
    for label in ["BASELINE", "JOULIE", "REDESIGN"]:
        results[label] = run_fmu_cooling(fmu_path, raw_results[label], label)

    # Summary table
    b = results["BASELINE"]
    print(f"\n{'='*100}\nCOMPARISON SUMMARY (FMU-computed PUE)\n{'='*100}", flush=True)
    print(f"  {'Metric':30s} {'BASELINE':>12s} {'JOULIE':>12s} {'REDESIGN':>12s} {'J-Delta':>8s} {'R-Delta':>8s}", flush=True)
    print(f"  {'-'*30} {'-'*12} {'-'*12} {'-'*12} {'-'*8} {'-'*8}", flush=True)

    for label, col, agg in [
        ("Avg IT power (kW)","it_power_kw","mean"), ("Peak IT power (kW)","it_power_kw","max"),
        ("IT power std (kW)","it_power_kw","std"), ("Avg PUE (FMU)","pue","mean"),
        ("Peak PUE (FMU)","pue","max"), ("Avg COP (FMU)","cop","mean"),
        ("Avg cooling power (kW)","cooling_power_kw","mean"),
        ("Avg facility power (kW)","facility_power_kw","mean"),
    ]:
        bv = getattr(b[col], agg)()
        jv = getattr(results["JOULIE"][col], agg)()
        rv = getattr(results["REDESIGN"][col], agg)()
        jd = (jv - bv) / bv * 100 if bv > 0 else 0
        rd = (rv - bv) / bv * 100 if bv > 0 else 0
        print(f"  {label:30s} {bv:12.2f} {jv:12.2f} {rv:12.2f} {jd:+7.2f}% {rd:+7.2f}%", flush=True)

    # Energy totals
    print(flush=True)
    for sn in ["BASELINE", "JOULIE", "REDESIGN"]:
        df = results[sn]
        it_mwh = df["it_power_kw"].sum() * DT_SEC / 3600 / 1000
        fac_mwh = df["facility_power_kw"].sum() * DT_SEC / 3600 / 1000
        cool_mwh = df["cooling_power_kw"].sum() * DT_SEC / 3600 / 1000
        dropped = int(df["dropped"].sum())
        completed = len(jobs) - dropped
        fac_kwh_per_job = fac_mwh * 1000 / max(completed, 1)
        print(f"  {sn:12s}: IT={it_mwh:.1f} MWh, Facility={fac_mwh:.1f} MWh, "
              f"Cooling={cool_mwh:.1f} MWh, Drops={dropped}, kWh/job={fac_kwh_per_job:.4f}", flush=True)

    # Deltas
    print(flush=True)
    b_it = b["it_power_kw"].sum() * DT_SEC / 3600 / 1000
    b_fac = b["facility_power_kw"].sum() * DT_SEC / 3600 / 1000
    bd = int(b["dropped"].sum())
    bc = len(jobs) - bd
    b_kwh_j = b_fac * 1000 / max(bc, 1)

    for sn in ["JOULIE", "REDESIGN"]:
        df = results[sn]
        s_it = df["it_power_kw"].sum() * DT_SEC / 3600 / 1000
        s_fac = df["facility_power_kw"].sum() * DT_SEC / 3600 / 1000
        sd = int(df["dropped"].sum())
        sc = len(jobs) - sd
        s_kwh_j = s_fac * 1000 / max(sc, 1)
        print(f"  {sn} vs BASELINE: IT={((s_it-b_it)/b_it*100):+.2f}%, "
              f"Facility={((s_fac-b_fac)/b_fac*100):+.2f}%, "
              f"Drops={sd-bd:+d}, kWh/job={((s_kwh_j-b_kwh_j)/b_kwh_j*100):+.2f}%", flush=True)

    # Plot
    plot_three_way(results, args.outdir, args.hours)

    # Save CSVs
    for sn, df in results.items():
        p = os.path.join(args.outdir, f"timeseries_{sn.lower()}.csv")
        df.to_csv(p, index=False)
        print(f"Saved {p}", flush=True)


if __name__ == "__main__":
    main()
