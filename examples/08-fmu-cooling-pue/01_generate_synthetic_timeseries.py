#!/usr/bin/env python3
"""Generate synthetic timeseries.csv files that mimic real data center operations.

Produces one CSV per baseline (A, B, C) with realistic power profiles for a
heterogeneous GPU cluster (~40 nodes, 200+ GPUs) running a mixed workload
over multiple days.

The synthetic data captures:
  - Multi-day simulation (72h default) with diurnal workload cycles
  - Day/night load patterns (peak at 14:00, trough at 04:00)
  - Random job arrivals (training jobs, inference bursts, batch HPC)
  - Sudden load spikes (GPU training job starts, inference traffic bursts)
  - Weekend dip in batch workloads
  - Diurnal outdoor temperature cycle with weather noise
  - Baseline A: uncapped (full power)
  - Baseline B: static partition (30% perf, 70% eco at 65% CPU cap / 70% GPU cap)
  - Baseline C: queue-aware (dynamic, reacts to queue depth)

Output: data/timeseries_baseline_{A,B,C}.csv
"""
import math
import pathlib

import numpy as np
import pandas as pd

OUT_DIR = pathlib.Path(__file__).resolve().parent / "data"

# --- Cluster specification (matches experiment 02: heterogeneous) ---
NUM_NODES = 41
NUM_GPU_NODES = 33
NUM_CPU_NODES = 8

CPU_NODE_IDLE_W = 90.0
CPU_NODE_PEAK_W = 700.0
GPU_NODE_IDLE_W = 600.0
GPU_NODE_PEAK_W = 5200.0

# --- Simulation parameters ---
SIM_DURATION_H = 60 * 24.0    # 60 days (~2 months)
DT_MIN = 5.0                  # 5-minute resolution (17,280 points for 60 days)
TIME_SCALE = 1.0              # 1:1 (real time, not accelerated)

# Outdoor temperature (March-May in a temperate climate)
TEMP_BASE_C = 15.0            # Early spring baseline
TEMP_SEASONAL_RISE_C = 10.0   # Warming trend over 2 months (spring -> early summer)
TEMP_DIURNAL_AMP_C = 7.0      # Day/night swing
TEMP_PERIOD_H = 24.0
TEMP_NOISE_STD_C = 1.2        # Weather noise


def outdoor_temp(sim_h: float, rng: np.random.Generator) -> float:
    """Diurnal outdoor temperature with weather noise.

    Peak at ~15:00, trough at ~05:00 (shifted sine).
    """
    # Seasonal warming trend over the simulation period
    seasonal = TEMP_SEASONAL_RISE_C * (sim_h / (SIM_DURATION_H))

    # Shift so peak is at 15:00 (hour 15) instead of 6:00
    phase_shift = -9.0 / 24.0 * 2 * math.pi
    diurnal = TEMP_DIURNAL_AMP_C * math.sin(
        2 * math.pi * sim_h / TEMP_PERIOD_H + phase_shift
    )

    # Multi-day weather systems (3-5 day cycles, ±3°C)
    weather_drift = 3.0 * math.sin(2 * math.pi * sim_h / (90.0))
    weather_drift += 2.0 * math.sin(2 * math.pi * sim_h / (62.0) + 1.3)

    temp = TEMP_BASE_C + seasonal + diurnal + weather_drift
    temp += rng.normal(0, TEMP_NOISE_STD_C)
    return temp


def diurnal_load_factor(sim_h: float) -> float:
    """Day/night workload cycle.

    Returns 0-1 multiplier: peak ~0.95 at 14:00, trough ~0.25 at 04:00.
    Weekday pattern (days 6-7 = weekend with reduced batch load).
    """
    hour_of_day = sim_h % 24.0
    day_of_week = int(sim_h / 24.0) % 7  # 0=Mon

    # Smooth diurnal curve (shifted cosine, trough at 4am, peak at 4pm)
    phase = 2 * math.pi * (hour_of_day - 4.0) / 24.0
    diurnal = 0.60 + 0.35 * (1 - math.cos(phase)) / 2.0

    # Weekend dip: batch/HPC jobs drop ~30%
    if day_of_week >= 5:
        diurnal *= 0.70

    return diurnal


def generate_job_events(sim_duration_h: float, dt_min: float, rng: np.random.Generator) -> np.ndarray:
    """Generate random job arrival/departure events that create realistic load spikes.

    Returns array of shape (n_steps,) with additive load factor from job events.
    """
    n_steps = int(sim_duration_h * 60 / dt_min)
    job_load = np.zeros(n_steps)

    # --- Large GPU training jobs (start suddenly, run for hours) ---
    n_training_jobs = rng.poisson(lam=2.0 * sim_duration_h / 24.0)
    for _ in range(n_training_jobs):
        start_step = rng.integers(0, n_steps)
        duration_steps = int(rng.uniform(60, 480) / dt_min)  # 1-8 hours
        intensity = rng.uniform(0.15, 0.35)  # 15-35% of cluster
        end_step = min(start_step + duration_steps, n_steps)
        # Ramp up over ~10 minutes
        ramp_steps = min(int(10 / dt_min), duration_steps)
        for i in range(ramp_steps):
            if start_step + i < n_steps:
                job_load[start_step + i] += intensity * (i / ramp_steps)
        job_load[start_step + ramp_steps:end_step] += intensity

    # --- Inference traffic bursts (short, sharp spikes) ---
    n_bursts = rng.poisson(lam=8.0 * sim_duration_h / 24.0)
    for _ in range(n_bursts):
        start_step = rng.integers(0, n_steps)
        duration_steps = int(rng.uniform(5, 45) / dt_min)  # 5-45 min
        intensity = rng.uniform(0.05, 0.20)
        end_step = min(start_step + duration_steps, n_steps)
        # Sharp ramp (2 min)
        ramp = min(int(2 / dt_min), duration_steps)
        for i in range(ramp):
            if start_step + i < n_steps:
                job_load[start_step + i] += intensity * (i / max(ramp, 1))
        job_load[start_step + ramp:end_step] += intensity
        # Sharp decay
        decay_steps = min(int(3 / dt_min), n_steps - end_step)
        for i in range(decay_steps):
            if end_step + i < n_steps:
                job_load[end_step + i] += intensity * (1 - i / max(decay_steps, 1))

    # --- Batch HPC jobs (medium duration, moderate load) ---
    n_batch = rng.poisson(lam=5.0 * sim_duration_h / 24.0)
    for _ in range(n_batch):
        start_step = rng.integers(0, n_steps)
        duration_steps = int(rng.uniform(30, 180) / dt_min)  # 30min - 3h
        intensity = rng.uniform(0.05, 0.15)
        end_step = min(start_step + duration_steps, n_steps)
        job_load[start_step:end_step] += intensity

    return job_load


def generate_baseline(baseline: str, seed: int = 42) -> pd.DataFrame:
    """Generate synthetic timeseries for one baseline."""
    rng = np.random.default_rng(seed)
    n_steps = int(SIM_DURATION_H * 60 / DT_MIN)

    # Pre-generate job events
    job_events = generate_job_events(SIM_DURATION_H, DT_MIN, rng)

    rows = []
    energy_cumulative_j = 0.0

    for i in range(n_steps):
        sim_min = i * DT_MIN
        sim_h = sim_min / 60.0
        wall_sec = sim_min * 60.0  # wall seconds (1:1 time scale)

        # Base load: diurnal pattern + job events + noise
        base_load = diurnal_load_factor(sim_h)
        load = base_load + job_events[i] + rng.normal(0, 0.03)
        load = max(0.03, min(1.0, load))

        # CPU and GPU utilisation (GPUs lag slightly, more bursty)
        cpu_util = load * 0.85 + rng.normal(0, 0.02)
        gpu_util = load * 0.78 + rng.normal(0, 0.03)
        cpu_util = max(0.01, min(1, cpu_util))
        gpu_util = max(0.01, min(1, gpu_util))

        # --- Power model per baseline ---
        if baseline == "A":
            cpu_power = NUM_CPU_NODES * (CPU_NODE_IDLE_W + (CPU_NODE_PEAK_W - CPU_NODE_IDLE_W) * cpu_util)
            gpu_power = NUM_GPU_NODES * (GPU_NODE_IDLE_W + (GPU_NODE_PEAK_W - GPU_NODE_IDLE_W) * gpu_util)
        elif baseline == "B":
            perf_frac = 0.30
            eco_cpu_cap = 0.65
            eco_gpu_cap = 0.70

            n_cpu_perf = max(1, round(NUM_CPU_NODES * perf_frac))
            n_cpu_eco = NUM_CPU_NODES - n_cpu_perf
            cpu_perf_power = n_cpu_perf * (CPU_NODE_IDLE_W + (CPU_NODE_PEAK_W - CPU_NODE_IDLE_W) * cpu_util)
            cpu_eco_power = n_cpu_eco * (CPU_NODE_IDLE_W + (CPU_NODE_PEAK_W - CPU_NODE_IDLE_W) * min(cpu_util, eco_cpu_cap) * eco_cpu_cap)
            cpu_power = cpu_perf_power + cpu_eco_power

            n_gpu_perf = max(1, round(NUM_GPU_NODES * perf_frac))
            n_gpu_eco = NUM_GPU_NODES - n_gpu_perf
            gpu_perf_power = n_gpu_perf * (GPU_NODE_IDLE_W + (GPU_NODE_PEAK_W - GPU_NODE_IDLE_W) * gpu_util)
            gpu_eco_power = n_gpu_eco * (GPU_NODE_IDLE_W + (GPU_NODE_PEAK_W - GPU_NODE_IDLE_W) * min(gpu_util, eco_gpu_cap) * eco_gpu_cap)
            gpu_power = gpu_perf_power + gpu_eco_power
        else:  # C: queue-aware
            # Dynamic: more perf nodes when load is high, aggressive eco when low
            perf_frac = 0.15 + 0.25 * load  # 15-40% depending on load
            eco_cpu_cap = 0.55 + 0.15 * (1 - load)  # tighter cap when load is low
            eco_gpu_cap = 0.60 + 0.15 * (1 - load)

            n_cpu_perf = max(1, round(NUM_CPU_NODES * perf_frac))
            n_cpu_eco = NUM_CPU_NODES - n_cpu_perf
            cpu_perf_power = n_cpu_perf * (CPU_NODE_IDLE_W + (CPU_NODE_PEAK_W - CPU_NODE_IDLE_W) * cpu_util)
            cpu_eco_power = n_cpu_eco * (CPU_NODE_IDLE_W + (CPU_NODE_PEAK_W - CPU_NODE_IDLE_W) * min(cpu_util, eco_cpu_cap) * eco_cpu_cap)
            cpu_power = cpu_perf_power + cpu_eco_power

            n_gpu_perf = max(1, round(NUM_GPU_NODES * perf_frac))
            n_gpu_eco = NUM_GPU_NODES - n_gpu_perf
            gpu_perf_power = n_gpu_perf * (GPU_NODE_IDLE_W + (GPU_NODE_PEAK_W - GPU_NODE_IDLE_W) * gpu_util)
            gpu_eco_power = n_gpu_eco * (GPU_NODE_IDLE_W + (GPU_NODE_PEAK_W - GPU_NODE_IDLE_W) * min(gpu_util, eco_gpu_cap) * eco_gpu_cap)
            gpu_power = gpu_perf_power + gpu_eco_power

        it_power = cpu_power + gpu_power
        ambient = outdoor_temp(sim_h, rng)

        # Simple placeholder PUE (will be replaced by cooling model post-processing)
        pue = 1.1 + 0.008 * (ambient - 15.0)
        pue = max(1.0, min(3.0, pue))
        cooling_power = it_power * (pue - 1.0)
        facility_power = it_power + cooling_power

        dt_real_sec = DT_MIN * 60.0
        energy_cumulative_j += it_power * dt_real_sec

        pods = int(load * 300 + rng.normal(0, 5))
        pods = max(0, pods)

        ts = pd.Timestamp("2026-03-15T00:00:00Z") + pd.Timedelta(seconds=wall_sec)

        rows.append({
            "timestamp_utc": ts.isoformat().replace("+00:00", "Z"),
            "elapsed_sec": round(wall_sec, 1),
            "it_power_w": round(it_power, 1),
            "cpu_power_w": round(cpu_power, 1),
            "gpu_power_w": round(gpu_power, 1),
            "pue": round(pue, 4),
            "cooling_power_w": round(cooling_power, 1),
            "facility_power_w": round(facility_power, 1),
            "ambient_temp_c": round(ambient, 2),
            "cluster_cpu_util": round(cpu_util, 4),
            "cluster_gpu_util": round(gpu_util, 4),
            "nodes_active": NUM_NODES,
            "pods_running": pods,
            "energy_cumulative_j": round(energy_cumulative_j, 1),
        })

    return pd.DataFrame(rows)


def main():
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    for baseline in ["A", "B", "C"]:
        print(f"generating baseline {baseline}...", end=" ", flush=True)
        df = generate_baseline(baseline, seed=42 + ord(baseline))
        path = OUT_DIR / f"timeseries_baseline_{baseline}.csv"
        df.to_csv(path, index=False)
        duration_h = df["elapsed_sec"].max() / 3600
        peak_it = df["it_power_w"].max() / 1000
        mean_it = df["it_power_w"].mean() / 1000
        print(f"{len(df)} rows, {duration_h:.0f}h, peak={peak_it:.1f} kW, mean={mean_it:.1f} kW -> {path.name}")

    # Also write a combined metadata file
    import json
    meta = {
        "time_scale": TIME_SCALE,
        "sim_duration_h": SIM_DURATION_H,
        "dt_min": DT_MIN,
        "cluster": {
            "cpu_nodes": NUM_CPU_NODES,
            "gpu_nodes": NUM_GPU_NODES,
            "total_nodes": NUM_NODES,
        },
    }
    (OUT_DIR / "metadata.json").write_text(json.dumps(meta, indent=2))
    print(f"\nmetadata -> {OUT_DIR / 'metadata.json'}")


if __name__ == "__main__":
    main()
