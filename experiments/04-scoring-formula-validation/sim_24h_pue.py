#!/usr/bin/env python3
"""
48-hour datacenter simulation: 2500-node H100 cluster (vectorized).

Compares:
  - BASELINE: k8s MostAllocated (bin-packing by allocation ratios)
  - JOULIE:   power-headroom + estimated-pod-power + trend scoring

PUE from Modelica DXCooledAirsideEconomizer FMU (FMI 2.0, Docker).

The Joulie formula mirrors the Go scheduler (pkg/scheduler/powerest/model.go):
  score = powerHeadroom * 0.4 + (100 - coolingStress) * 0.3 + (100 - psuStress) * 0.3
        - marginalPowerPenalty(estimated_pod_watts)
        - idleGPUPenalty - trendPenalty

Usage:
    python sim_24h_pue.py [--hours 48] [--seed 42] [--outdir ./results_2500node]
"""

import argparse
import copy
import csv
import math
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time as _time

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.dates as mdates
import numpy as np
import pandas as pd
from datetime import datetime, timedelta

# Default FMU path
_SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
_REPO_ROOT = _SCRIPT_DIR.parent.parent
DEFAULT_FMU = _REPO_ROOT / "examples" / "08-fmu-cooling-pue" / "cooling_models" / "DXCooledAirsideEconomizer.fmu"
DOCKER_IMAGE = "openmodelica/openmodelica:v1.26.3-ompython"

# ---------------------------------------------------------------------------
# Cluster hardware — 2500-node H100 cluster
# ---------------------------------------------------------------------------
NUM_CPU_NODES = 2000
NUM_GPU_NODES = 500
N_NODES = NUM_CPU_NODES + NUM_GPU_NODES

CPU_CORES = 96               # dual-socket EPYC 9654
CPU_MAX_WATTS = 700.0        # node-level TDP

GPU_CORES = 128              # dual-socket host CPU for DGX
GPU_MAX_WATTS_PER_GPU = 700.0    # H100 SXM TDP
GPU_COUNT_PER_NODE = 8           # DGX H100
GPU_NODE_CPU_MAX_WATTS = 500.0
GPU_IDLE_WATTS_PER_GPU = 100.0   # H100 idle

# Power estimation coefficients (from Go: pkg/scheduler/powerest/model.go)
CPU_UTIL_COEFF = 0.7
GPU_UTIL_COEFF_STD = 0.65
GPU_UTIL_COEFF_PERF = 0.85

REFERENCE_NODE_POWER_W = 4000.0
REFERENCE_RACK_CAPACITY_W = 50000.0
IDLE_GPU_WATTS_PER_DEVICE = 100.0
IDLE_GPU_PENALTY_CAP = 500.0

TREND_WINDOW = 10
DT_SEC = 60.0

# ---------------------------------------------------------------------------
# Vectorized cluster state (numpy arrays for speed)
# ---------------------------------------------------------------------------
# Static per-node arrays (set once at init)
_cpu_cores = np.zeros(N_NODES, dtype=np.float64)
_has_gpu = np.zeros(N_NODES, dtype=bool)
_gpu_count = np.zeros(N_NODES, dtype=np.int32)
_peak_power = np.zeros(N_NODES, dtype=np.float64)
_cpu_max_w = np.zeros(N_NODES, dtype=np.float64)
_gpu_max_w_per = np.zeros(N_NODES, dtype=np.float64)
_is_cpu_node = np.zeros(N_NODES, dtype=bool)

# Dynamic per-node arrays (updated each step)
_alloc_cpu = np.zeros(N_NODES, dtype=np.float64)
_alloc_gpu = np.zeros(N_NODES, dtype=np.int32)
_measured_power = np.zeros(N_NODES, dtype=np.float64)
_power_hist = np.zeros((N_NODES, TREND_WINDOW), dtype=np.float64)
_hist_ptr = 0  # ring buffer pointer (shared across all nodes, same step)
_hist_filled = 0

# Per-node job lists (can't vectorize job management)
_node_jobs = [[] for _ in range(N_NODES)]


def init_cluster():
    """Initialize static cluster arrays."""
    global _hist_ptr, _hist_filled
    _hist_ptr = 0
    _hist_filled = 0
    for i in range(NUM_CPU_NODES):
        _cpu_cores[i] = CPU_CORES
        _has_gpu[i] = False
        _gpu_count[i] = 0
        _peak_power[i] = CPU_MAX_WATTS
        _cpu_max_w[i] = CPU_MAX_WATTS
        _gpu_max_w_per[i] = 0.0
        _is_cpu_node[i] = True
    for j in range(NUM_GPU_NODES):
        i = NUM_CPU_NODES + j
        _cpu_cores[i] = GPU_CORES
        _has_gpu[i] = True
        _gpu_count[i] = GPU_COUNT_PER_NODE
        _peak_power[i] = GPU_NODE_CPU_MAX_WATTS + GPU_COUNT_PER_NODE * GPU_MAX_WATTS_PER_GPU
        _cpu_max_w[i] = GPU_NODE_CPU_MAX_WATTS
        _gpu_max_w_per[i] = GPU_MAX_WATTS_PER_GPU
        _is_cpu_node[i] = False
    _alloc_cpu[:] = 0
    _alloc_gpu[:] = 0
    _measured_power[:] = 0
    _power_hist[:] = 0
    for i in range(N_NODES):
        _node_jobs[i] = []


def compute_all_power():
    """Vectorized power computation for all nodes."""
    # We still need per-node loops because jobs have varying utilization
    for i in range(N_NODES):
        jobs = _node_jobs[i]
        cpu_power = 0.0
        if jobs:
            for j in jobs:
                core_share = j[0] / _cpu_cores[i]  # cpu_cores
                cpu_power += _cpu_max_w[i] * CPU_UTIL_COEFF * core_share * j[3]  # cpu_util
            cpu_power += _cpu_max_w[i] * 0.10
        else:
            cpu_power = _cpu_max_w[i] * 0.05

        gpu_power = 0.0
        if _has_gpu[i]:
            active_gpus = 0
            for j in jobs:
                if j[1] > 0:  # gpu_count
                    coeff = GPU_UTIL_COEFF_PERF if j[2] == 1 else GPU_UTIL_COEFF_STD  # is_perf
                    gpu_power += j[1] * _gpu_max_w_per[i] * coeff * j[4]  # gpu_util
                    active_gpus += j[1]
            idle_gpus = max(0, _gpu_count[i] - active_gpus)
            gpu_power += idle_gpus * GPU_IDLE_WATTS_PER_GPU

        _measured_power[i] = cpu_power + gpu_power


def compute_trends():
    """Vectorized trend computation for all nodes. Returns (N_NODES,) array."""
    n = min(_hist_filled, TREND_WINDOW)
    if n < 2:
        return np.zeros(N_NODES)
    # Extract window from ring buffer
    indices = np.arange(_hist_ptr - n, _hist_ptr) % TREND_WINDOW
    window = _power_hist[:, indices]  # (N_NODES, n)
    xs = np.arange(n, dtype=np.float64)
    x_mean = xs.mean()
    y_mean = window.mean(axis=1)
    denom = ((xs - x_mean) ** 2).sum()
    if denom < 1e-9:
        return np.zeros(N_NODES)
    slopes = ((xs[np.newaxis, :] - x_mean) * (window - y_mean[:, np.newaxis])).sum(axis=1) / denom
    return slopes


# ---------------------------------------------------------------------------
# Job representation: tuple (cpu_cores, gpu_count, is_perf, cpu_util, gpu_util, remaining_sec)
# Using tuples for speed instead of dataclass objects
# ---------------------------------------------------------------------------

def generate_workload(sim_hours, rng):
    """Generate jobs as list of dicts (for scheduling), optimized."""
    jobs = []
    job_id = 0
    sim_sec = sim_hours * 3600

    t = 0.0
    while t < sim_sec:
        sim_h = t / 3600.0
        hour_of_day = sim_h % 24.0
        phase = 2 * math.pi * (hour_of_day - 4.0) / 24.0
        rate = 0.25 + 0.70 * (1 - math.cos(phase)) / 2.0
        base_rate_per_sec = rate * 80.0 / 60.0

        dt = rng.exponential(1.0 / max(base_rate_per_sec, 0.001))
        t += dt
        if t >= sim_sec:
            break

        burst_size = 1
        if rng.random() < 0.10:
            burst_size = int(rng.integers(50, 201))

        for _ in range(burst_size):
            r = rng.random()
            if r < 0.35:
                jobs.append((t, float(rng.choice([1,2,4,8])), 0, 0, float(rng.uniform(0.3,0.8)), 0.0, float(rng.uniform(60,600))))
            elif r < 0.55:
                is_perf = 1 if rng.random() < 0.3 else 0
                jobs.append((t, float(rng.choice([16,32,48,64])), 0, is_perf, float(rng.uniform(0.4,0.9)), 0.0, float(rng.uniform(300,3600))))
            elif r < 0.80:
                jobs.append((t, float(rng.choice([16,32,64])), int(rng.choice([1,2,4,8])), 1, float(rng.uniform(0.3,0.6)), float(rng.uniform(0.6,0.95)), float(rng.uniform(3600,28800))))
            elif r < 0.92:
                jobs.append((t, float(rng.choice([4,8,16])), int(rng.choice([1,2])), 0, float(rng.uniform(0.3,0.6)), float(rng.uniform(0.3,0.7)), float(rng.uniform(60,900))))
            else:
                jobs.append((t, float(rng.choice([64,96])), 0, 1, float(rng.uniform(0.5,0.95)), 0.0, float(rng.uniform(1800,7200))))
            job_id += 1

    # Sort by arrival time
    jobs.sort(key=lambda x: x[0])
    return jobs
    # tuple: (arrival_sec, cpu_cores, gpu_count, is_perf, cpu_util, gpu_util, duration_sec)


# ---------------------------------------------------------------------------
# Schedulers (vectorized where possible)
# ---------------------------------------------------------------------------

def schedule_baseline_vec(job_cpu, job_gpu):
    """Vectorized MostAllocated: returns best node index or -1."""
    # Feasibility mask
    cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
    if job_gpu > 0:
        gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
        mask = cpu_fit & gpu_fit
    else:
        mask = cpu_fit

    if not mask.any():
        return -1

    # MostAllocated score = cpu_util + gpu_util
    scores = _alloc_cpu / np.maximum(_cpu_cores, 1)
    gpu_util = np.where(_has_gpu & (_gpu_count > 0),
                        _alloc_gpu.astype(np.float64) / np.maximum(_gpu_count, 1),
                        0.0)
    scores += gpu_util

    scores[~mask] = -1e9
    return int(np.argmax(scores))


def schedule_joulie_vec(job_cpu, job_gpu, job_is_perf, trends, cluster_trend):
    """Vectorized Joulie: power-headroom + pod-power + idle-GPU + trend."""
    # Feasibility mask
    cpu_fit = (_alloc_cpu + job_cpu) <= _cpu_cores
    if job_gpu > 0:
        gpu_fit = _has_gpu & ((_alloc_gpu + job_gpu) <= _gpu_count)
        mask = cpu_fit & gpu_fit
    else:
        mask = cpu_fit

    if not mask.any():
        return -1

    # --- Base score: power headroom (Go: powerHeadroom * 0.4) ---
    headroom_pct = np.maximum(0.0, (_peak_power - _measured_power) / np.maximum(_peak_power, 1.0)) * 100.0
    scores = headroom_pct * 0.4

    # --- Cooling stress proxy (Go: (100 - coolingStress) * 0.3) ---
    cooling_stress = np.minimum(100.0, (_measured_power / REFERENCE_NODE_POWER_W) * 80.0)
    scores += (100.0 - cooling_stress) * 0.3

    # --- PSU stress proxy (Go: (100 - psuStress) * 0.3) ---
    psu_stress = np.minimum(100.0, (_measured_power / REFERENCE_RACK_CAPACITY_W) * 100.0)
    scores += (100.0 - psu_stress) * 0.3

    # --- Marginal power penalty (estimated pod power) ---
    # CPU delta (per node type)
    util_share = np.minimum(1.0, job_cpu / np.maximum(_cpu_cores, 1))
    delta_cpu = _cpu_max_w * CPU_UTIL_COEFF * util_share
    delta_gpu = np.zeros(N_NODES)
    if job_gpu > 0:
        coeff = GPU_UTIL_COEFF_PERF if job_is_perf else GPU_UTIL_COEFF_STD
        delta_gpu = np.where(_has_gpu, job_gpu * _gpu_max_w_per * coeff, 0.0)
    delta_total = delta_cpu + delta_gpu
    scores -= np.minimum(20.0, np.maximum(0.0, delta_total / 20.0))

    # --- Idle GPU waste penalty ---
    if job_gpu == 0:
        idle_gpus = _gpu_count - _alloc_gpu
        waste = np.minimum(idle_gpus.astype(np.float64) * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
        waste_penalty = np.where(_has_gpu, np.minimum(20.0, np.maximum(0.0, waste / 10.0)), 0.0)
        scores -= waste_penalty

    # --- Power trend: adaptive smoothing ---
    trend_scale = 2.0 if abs(cluster_trend) > 500.0 else 6.0
    trend_penalty = np.clip(trends / trend_scale, -20.0, 25.0)
    scores -= trend_penalty

    scores[~mask] = -1e9
    return int(np.argmax(scores))


# ---------------------------------------------------------------------------
# Outdoor temperature
# ---------------------------------------------------------------------------

def outdoor_temp(sim_h, rng):
    temp_base = 23.0
    diurnal_amp = 6.0
    phase_shift = -9.0 / 24.0 * 2 * math.pi
    diurnal = diurnal_amp * math.sin(2 * math.pi * sim_h / 24.0 + phase_shift)
    weather = 2.0 * math.sin(2 * math.pi * sim_h / 72.0)
    noise = rng.normal(0, 0.5)
    return temp_base + diurnal + weather + noise


# ---------------------------------------------------------------------------
# FMU co-simulation
# ---------------------------------------------------------------------------

def run_fmu_cooling(fmu_path, df, label):
    fmu_path = fmu_path.resolve()
    if not fmu_path.exists():
        sys.exit(f"ERROR: FMU not found: {fmu_path}")

    with tempfile.TemporaryDirectory(prefix="joulie-fmu-") as tmpdir:
        work_dir = pathlib.Path(tmpdir)
        input_df = pd.DataFrame({
            "elapsed_sec": np.arange(len(df)) * DT_SEC,
            "it_power_w": (df["it_power_kw"].values * 1000).round(1),
            "ambient_temp_c": df["outdoor_temp_c"].values.round(2),
        })
        input_df.to_csv(work_dir / "fmu_input.csv", index=False)
        shutil.copy2(fmu_path, work_dir / fmu_path.name)

        runner = f'''#!/usr/bin/env python3
import csv, sys, numpy as np
from fmpy import read_model_description, simulate_fmu
fmu = "{fmu_path.name}"
md = read_model_description(fmu)
rows = list(csv.DictReader(open("fmu_input.csv")))
n = len(rows)
time_arr = np.array([float(r["elapsed_sec"]) for r in rows])
q_it_arr = np.array([float(r["it_power_w"]) for r in rows])
t_out_arr = np.array([float(r["ambient_temp_c"]) + 273.15 for r in rows])
step_size = time_arr[1] - time_arr[0] if n > 1 else 60.0
stop_time = time_arr[-1] if n > 0 else 86400
dtype = [("time", np.float64), ("Q_IT", np.float64), ("T_outdoor", np.float64)]
signals = np.array(list(zip(time_arr, q_it_arr, t_out_arr)), dtype=dtype)
print(f"Running FMU: {{n}} steps, step={{step_size:.0f}}s, stop={{stop_time:.0f}}s", file=sys.stderr)
result = simulate_fmu(fmu, stop_time=stop_time, step_size=step_size,
    output_interval=step_size, input=signals, output=["P_cooling","T_indoor","COP"])
with open("fmu_output.csv","w",newline="") as f:
    w = csv.writer(f)
    w.writerow(["time_s","p_cooling_w","t_indoor_k","cop"])
    for i in range(len(result)):
        w.writerow([round(result["time"][i],1),round(result["P_cooling"][i],1),
            round(result["T_indoor"][i],3),round(result["COP"][i],4)])
print(f"Wrote {{len(result)}} rows", file=sys.stderr)
'''
        (work_dir / "run_fmu.py").write_text(runner)

        print(f"  [{label}] Running FMU via Docker ({len(df)} timesteps)...", flush=True)
        result = subprocess.run(
            ["docker", "run", "--rm", "-v", f"{work_dir}:/work", "-w", "/work",
             DOCKER_IMAGE, "bash", "-c",
             "pip install fmpy numpy 2>/dev/null | tail -1 && python3 run_fmu.py"],
            capture_output=True, text=True, timeout=900,
        )
        if result.stderr:
            for line in result.stderr.strip().split("\n")[-5:]:
                print(f"    [FMU] {line}", flush=True)

        output_path = work_dir / "fmu_output.csv"
        if not output_path.exists():
            sys.exit(f"FMU failed.\nstdout: {result.stdout[:500]}\nstderr: {result.stderr[:500]}")
        fmu_df = pd.read_csv(output_path)

    input_time = np.arange(len(df)) * DT_SEC
    fmu_time = fmu_df["time_s"].values
    p_cooling = np.maximum(0, np.interp(input_time, fmu_time, fmu_df["p_cooling_w"].values))
    t_indoor_k = np.interp(input_time, fmu_time, fmu_df["t_indoor_k"].values)
    cop_vals = np.interp(input_time, fmu_time, fmu_df["cop"].values)

    enriched = df.copy()
    it_w = enriched["it_power_kw"].values * 1000
    enriched["cooling_power_kw"] = np.round(p_cooling / 1000, 3)
    enriched["pue"] = np.round((it_w + p_cooling) / np.maximum(it_w, 1.0), 4)
    enriched["cop"] = np.round(cop_vals, 2)
    enriched["indoor_temp_c"] = np.round(t_indoor_k - 273.15, 2)
    enriched["facility_power_kw"] = np.round((it_w + p_cooling) / 1000, 3)

    print(f"  [{label}] FMU done: avg PUE={enriched['pue'].mean():.4f}, avg COP={enriched['cop'].mean():.2f}", flush=True)
    return enriched


# ---------------------------------------------------------------------------
# Main simulation loop (vectorized)
# ---------------------------------------------------------------------------

def run_simulation(jobs, scheduler_name, sim_hours, rng, scheduler_fn=None):
    """Run simulation. scheduler_name is "BASELINE"/"JOULIE", or pass scheduler_fn(job_cpu, job_gpu, job_is_perf, trends, cluster_trend)->int."""
    global _hist_ptr, _hist_filled

    init_cluster()
    is_joulie = scheduler_name == "JOULIE" or (scheduler_fn is not None and scheduler_name != "BASELINE")

    sim_sec = sim_hours * 3600.0
    n_steps = int(sim_sec / DT_SEC)

    rec_it_power = np.zeros(n_steps)
    rec_outdoor = np.zeros(n_steps)
    rec_arrivals = np.zeros(n_steps, dtype=np.int32)
    rec_dropped = np.zeros(n_steps, dtype=np.int32)
    rec_active = np.zeros(n_steps, dtype=np.int32)

    job_idx = 0
    n_jobs = len(jobs)
    cluster_trend_hist = []

    t0 = _time.monotonic()

    for step in range(n_steps):
        t_sec = step * DT_SEC
        sim_h = t_sec / 3600.0

        # 1. Tick: advance and remove completed jobs
        total_active = 0
        for i in range(N_NODES):
            node_jobs = _node_jobs[i]
            if node_jobs:
                # job tuple: (cpu_cores, gpu_count, is_perf, cpu_util, gpu_util, remaining_sec)
                kept = []
                for j in node_jobs:
                    rem = j[5] - DT_SEC
                    if rem > 0:
                        kept.append((j[0], j[1], j[2], j[3], j[4], rem))
                _node_jobs[i] = kept
                total_active += len(kept)
                # Update allocations
                _alloc_cpu[i] = sum(j[0] for j in kept)
                _alloc_gpu[i] = sum(j[1] for j in kept)
            else:
                _alloc_cpu[i] = 0.0
                _alloc_gpu[i] = 0

        # 2. Compute power (vectorized-ish — still per-node due to variable jobs)
        compute_all_power()

        # Store in history ring buffer
        _power_hist[:, _hist_ptr % TREND_WINDOW] = _measured_power
        _hist_ptr += 1
        _hist_filled = min(_hist_filled + 1, TREND_WINDOW)

        # Compute trends (vectorized across all nodes)
        if is_joulie:
            trends = compute_trends()
            cluster_total = _measured_power.sum()
            cluster_trend_hist.append(cluster_total)
            if len(cluster_trend_hist) > TREND_WINDOW:
                cluster_trend_hist = cluster_trend_hist[-TREND_WINDOW:]
            if len(cluster_trend_hist) >= 2:
                ct_arr = np.array(cluster_trend_hist)
                xs = np.arange(len(ct_arr), dtype=np.float64)
                xm = xs.mean()
                ym = ct_arr.mean()
                d = ((xs - xm)**2).sum()
                cluster_trend = float(((xs - xm) * (ct_arr - ym)).sum() / d) if d > 1e-9 else 0.0
            else:
                cluster_trend = 0.0

        # 3. Schedule arriving jobs
        arrivals = 0
        dropped = 0
        while job_idx < n_jobs and jobs[job_idx][0] <= t_sec:
            jt = jobs[job_idx]
            job_idx += 1
            arrivals += 1
            # jt: (arrival, cpu, gpu, is_perf, cpu_util, gpu_util, duration)
            job_cpu = jt[1]
            job_gpu = jt[2]
            job_is_perf = jt[3]

            if scheduler_fn is not None and scheduler_name != "BASELINE":
                ni = scheduler_fn(job_cpu, job_gpu, job_is_perf, trends, cluster_trend)
            elif is_joulie:
                ni = schedule_joulie_vec(job_cpu, job_gpu, job_is_perf, trends, cluster_trend)
            else:
                ni = schedule_baseline_vec(job_cpu, job_gpu)

            if ni >= 0:
                # Place job: (cpu, gpu, is_perf, cpu_util, gpu_util, remaining)
                _node_jobs[ni].append((job_cpu, job_gpu, job_is_perf, jt[4], jt[5], jt[6]))
                _alloc_cpu[ni] += job_cpu
                _alloc_gpu[ni] += job_gpu
                # Recompute this node's power
                node_jobs = _node_jobs[ni]
                cpu_power = 0.0
                for j in node_jobs:
                    cpu_power += _cpu_max_w[ni] * CPU_UTIL_COEFF * (j[0] / _cpu_cores[ni]) * j[3]
                cpu_power += _cpu_max_w[ni] * 0.10
                gpu_power = 0.0
                if _has_gpu[ni]:
                    active_g = 0
                    for j in node_jobs:
                        if j[1] > 0:
                            c = GPU_UTIL_COEFF_PERF if j[2] == 1 else GPU_UTIL_COEFF_STD
                            gpu_power += j[1] * _gpu_max_w_per[ni] * c * j[4]
                            active_g += j[1]
                    gpu_power += max(0, _gpu_count[ni] - active_g) * GPU_IDLE_WATTS_PER_GPU
                _measured_power[ni] = cpu_power + gpu_power
                # Update history for the placed node
                _power_hist[ni, (_hist_ptr - 1) % TREND_WINDOW] = _measured_power[ni]
                total_active += 1
            else:
                dropped += 1

        rec_it_power[step] = _measured_power.sum() / 1000
        rec_outdoor[step] = outdoor_temp(sim_h, rng)
        rec_arrivals[step] = arrivals
        rec_dropped[step] = dropped
        rec_active[step] = total_active

        if step > 0 and step % 360 == 0:
            elapsed = _time.monotonic() - t0
            eta = elapsed / step * (n_steps - step)
            print(f"    step {step}/{n_steps} ({sim_h:.0f}h) — "
                  f"IT={rec_it_power[step]:.0f}kW, jobs={total_active}, "
                  f"drops={dropped} [{elapsed:.0f}s, ~{eta:.0f}s ETA]", flush=True)

    start_time = datetime(2026, 3, 19, 0, 0, 0)
    return pd.DataFrame({
        "step": np.arange(n_steps),
        "timestamp": [start_time + timedelta(seconds=s * DT_SEC) for s in range(n_steps)],
        "sim_h": np.arange(n_steps) * DT_SEC / 3600,
        "arrivals": rec_arrivals,
        "dropped": rec_dropped,
        "active_jobs": rec_active,
        "it_power_kw": rec_it_power,
        "outdoor_temp_c": rec_outdoor,
    })


# ---------------------------------------------------------------------------
# Plotting
# ---------------------------------------------------------------------------

def plot_comparison(results, outdir, sim_hours):
    colors = {"BASELINE": "#d62728", "JOULIE": "#1f77b4"}
    fig, axes = plt.subplots(7, 1, figsize=(16, 24), sharex=True)
    fig.suptitle(
        f"Datacenter Simulation: {sim_hours:.0f}h, {N_NODES}-node H100 Cluster\n"
        f"BASELINE (MostAllocated) vs JOULIE (power-headroom + pod-power + trend)\n"
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
    path = os.path.join(outdir, "comparison_24h_fmu.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"Saved {path}", flush=True)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hours", type=float, default=48)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--outdir", type=str, default="./results_2500node")
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

    raw_results = {}
    for sched_name in ["BASELINE", "JOULIE"]:
        print(f"\nRunning {sched_name}...", flush=True)
        rng_sim = np.random.default_rng(args.seed + 1)
        df = run_simulation(jobs, sched_name, args.hours, rng_sim)
        raw_results[sched_name] = df
        deriv = np.diff(df["it_power_kw"].values)
        print(f"  Avg IT: {df['it_power_kw'].mean():.0f} kW, Peak: {df['it_power_kw'].max():.0f} kW", flush=True)
        print(f"  Std: {df['it_power_kw'].std():.1f} kW, Drops: {df['dropped'].sum()}", flush=True)
        print(f"  Max ramp: {deriv.max():.1f} kW/min, Deriv std: {deriv.std():.2f}", flush=True)

    print(f"\n{'='*60}\nRunning FMU co-simulation...\n{'='*60}", flush=True)
    results = {}
    for sn, df in raw_results.items():
        results[sn] = run_fmu_cooling(fmu_path, df, sn)

    # Summary
    b = results["BASELINE"]
    j = results["JOULIE"]
    print(f"\n{'='*80}\nCOMPARISON SUMMARY (FMU-computed PUE)\n{'='*80}", flush=True)
    print(f"  {'Metric':30s} {'BASELINE':>12s} {'JOULIE':>12s} {'Delta':>8s}", flush=True)
    print(f"  {'-'*30} {'-'*12} {'-'*12} {'-'*8}", flush=True)
    for label, col, agg in [
        ("Avg IT power (kW)","it_power_kw","mean"), ("Peak IT power (kW)","it_power_kw","max"),
        ("IT power std (kW)","it_power_kw","std"), ("Avg PUE (FMU)","pue","mean"),
        ("Peak PUE (FMU)","pue","max"), ("Avg COP (FMU)","cop","mean"),
        ("Avg cooling power (kW)","cooling_power_kw","mean"), ("Avg facility power (kW)","facility_power_kw","mean"),
    ]:
        bv = getattr(b[col], agg)()
        jv = getattr(j[col], agg)()
        d = (jv - bv) / bv * 100 if bv > 0 else 0
        print(f"  {label:30s} {bv:12.2f} {jv:12.2f} {d:+7.2f}%", flush=True)

    print(flush=True)
    b_it = b["it_power_kw"].sum() * DT_SEC / 3600 / 1000
    j_it = j["it_power_kw"].sum() * DT_SEC / 3600 / 1000
    b_fac = b["facility_power_kw"].sum() * DT_SEC / 3600 / 1000
    j_fac = j["facility_power_kw"].sum() * DT_SEC / 3600 / 1000
    b_cool = b["cooling_power_kw"].sum() * DT_SEC / 3600 / 1000
    j_cool = j["cooling_power_kw"].sum() * DT_SEC / 3600 / 1000
    bd = int(b["dropped"].sum()); jd = int(j["dropped"].sum())
    bc = len(jobs)-bd; jc = len(jobs)-jd
    print(f"  {'Dropped jobs':30s} {bd:12d} {jd:12d} {(jd-bd)/max(bd,1)*100:+7.1f}%", flush=True)
    print(f"  {'Total IT energy (MWh)':30s} {b_it:12.1f} {j_it:12.1f} {(j_it-b_it)/b_it*100:+7.2f}%", flush=True)
    print(f"  {'Total facility energy (MWh)':30s} {b_fac:12.1f} {j_fac:12.1f} {(j_fac-b_fac)/b_fac*100:+7.2f}%", flush=True)
    print(f"  {'Cooling energy (MWh)':30s} {b_cool:12.1f} {j_cool:12.1f} {(j_cool-b_cool)/max(b_cool,.001)*100:+7.2f}%", flush=True)
    bkj = b_fac*1000/max(bc,1); jkj = j_fac*1000/max(jc,1)
    print(f"  {'Facility kWh/job':30s} {bkj:12.4f} {jkj:12.4f} {(jkj-bkj)/bkj*100:+7.2f}%", flush=True)

    plot_comparison(results, args.outdir, args.hours)
    for sn, df in results.items():
        p = os.path.join(args.outdir, f"timeseries_{sn.lower()}.csv")
        df.to_csv(p, index=False)
        print(f"Saved {p}", flush=True)


if __name__ == "__main__":
    main()
