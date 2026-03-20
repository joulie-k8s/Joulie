#!/usr/bin/env python3
"""Shared noisy trace generator for standalone and KWOK experiments.

Generates realistic datacenter workload traces using NHPP arrivals with:
- Cosine day/night cycle (trough at 4 AM, peak at 4 PM)
- Ornstein-Uhlenbeck rate noise (slow-varying rate fluctuations)
- Scheduled mega-bursts (scaled by cluster size)
- Maintenance dip windows
- Surge windows (sustained rate increases)
- Mixed job sizes: CPU and GPU workloads
"""
import json
import math
import pathlib
import time
import datetime as dt

import numpy as np

START_TS = time.time()


def _log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    elapsed = time.time() - START_TS
    print(f"[trace-gen {now} +{elapsed:8.1f}s] {msg}", flush=True)


def generate_trace_python(
    traces_dir: pathlib.Path,
    seed: int,
    sim_hours: float,
    gpu_ratio: float,
    work_scale: float,
    time_scale: float,
    base_speed_per_core: float,
    diurnal_peak_rate: float = 80.0,
    perf_ratio: float = 0.20,
    burst_scale: float = 1.0,
    log_fn=None,
) -> pathlib.Path:
    """Generate a noisy workload trace.

    Args:
        traces_dir: Directory to write the trace file.
        seed: Random seed.
        sim_hours: Duration in simulated hours.
        gpu_ratio: Fraction of GPU workloads (0.0 = CPU-only).
        work_scale: Multiplier for job work units (longer jobs at higher values).
        time_scale: Sim-seconds per wall-second.
        base_speed_per_core: Speed multiplier for work computation.
        diurnal_peak_rate: Jobs/min at peak in sim-time.
        perf_ratio: Fraction of jobs that are performance-sensitive.
        burst_scale: Scale factor for burst sizes (1.0 = 5k-node cluster,
                     0.008 = 40-node cluster). Affects mega-burst job counts.
        log_fn: Optional logging function. Defaults to internal logger.

    Returns:
        Path to the generated trace file.
    """
    log = log_fn or _log
    traces_dir.mkdir(parents=True, exist_ok=True)
    trace_path = traces_dir / f"seed_{seed}_canonical.jsonl"
    if trace_path.exists():
        log(f"reusing trace seed={seed} file={trace_path}")
        return trace_path

    rng = np.random.default_rng(seed)
    wall_sec = sim_hours * 3600.0 / time_scale

    cpu_classes = ["cpu.compute_bound", "cpu.memory_bound", "cpu.mixed"]
    gpu_classes = ["gpu.compute", "gpu.mixed", "gpu.memory_bound"]
    gpu_resource = "nvidia.com/gpu"

    # ── Pre-generate burst events and maintenance dips per simulated day ──
    n_days = max(1, int(math.ceil(sim_hours / 24.0)))

    # Mega-bursts: scaled by cluster size.
    # 5k-node cluster: 2000-8000 jobs per burst.
    # 40-node cluster (burst_scale=0.008): 16-64 jobs per burst.
    burst_min = max(5, int(2000 * burst_scale))
    burst_max = max(10, int(8000 * burst_scale))
    micro_burst_min = max(2, int(10 * burst_scale))
    micro_burst_max = max(5, int(50 * burst_scale))

    burst_events = []
    for day in range(n_days):
        n_bursts = int(rng.integers(4, 9))
        for _ in range(n_bursts):
            burst_hour = rng.uniform(0, 24)
            burst_sim_sec = (day * 24 + burst_hour) * 3600.0
            burst_wall_sec = burst_sim_sec / time_scale
            if burst_wall_sec < wall_sec:
                burst_size = int(rng.integers(burst_min, burst_max + 1))
                burst_events.append((burst_wall_sec, burst_size))
    burst_events.sort()
    burst_idx = 0

    # Maintenance dip windows.
    dip_windows = []
    for day in range(n_days):
        if gpu_ratio == 0:
            n_dips = int(rng.integers(0, 2))
            for _ in range(n_dips):
                dip_start_hour = rng.uniform(0, 21)
                dip_duration_min = rng.uniform(20, 60)
                dip_start_sim = (day * 24 + dip_start_hour) * 3600.0
                dip_end_sim = dip_start_sim + dip_duration_min * 60.0
                dip_windows.append((dip_start_sim / time_scale, dip_end_sim / time_scale))
        else:
            n_dips = int(rng.integers(1, 4))
            for _ in range(n_dips):
                dip_start_hour = rng.uniform(0, 21)
                dip_duration_min = rng.uniform(60, 180)
                dip_start_sim = (day * 24 + dip_start_hour) * 3600.0
                dip_end_sim = dip_start_sim + dip_duration_min * 60.0
                dip_windows.append((dip_start_sim / time_scale, dip_end_sim / time_scale))
    dip_windows.sort()

    # Surge windows: 1-2 per day, 1-3 sim-hours at 2-3x normal rate.
    surge_windows = []
    for day in range(n_days):
        n_surges = int(rng.integers(1, 3))
        for _ in range(n_surges):
            surge_hour = rng.uniform(8, 20)
            surge_duration_h = rng.uniform(1, 3)
            surge_start_sim = (day * 24 + surge_hour) * 3600.0
            surge_end_sim = surge_start_sim + surge_duration_h * 3600.0
            surge_mult = rng.uniform(2.0, 3.0)
            surge_windows.append((surge_start_sim / time_scale, surge_end_sim / time_scale, surge_mult))
    surge_windows.sort()

    # ── Ornstein-Uhlenbeck rate noise state ──
    ou_noise = 0.0
    ou_theta = 0.08
    ou_sigma = 0.25
    last_t = 0.0

    log(f"generating noisy trace seed={seed} sim_hours={sim_hours} peak_rate={diurnal_peak_rate} "
        f"burst_scale={burst_scale:.4f} bursts={len(burst_events)} dips={len(dip_windows)} surges={len(surge_windows)}")

    lines = []
    job_id = 0
    t = 0.0
    while t < wall_sec:
        sim_h = (t * time_scale) / 3600.0
        hour_of_day = sim_h % 24.0
        phase = 2 * math.pi * (hour_of_day - 4.0) / 24.0
        rate = 0.08 + 0.88 * (1 - math.cos(phase)) / 2.0

        # Apply OU noise.
        dt_val = t - last_t
        if dt_val > 0:
            ou_noise += ou_theta * (0.0 - ou_noise) * dt_val + ou_sigma * math.sqrt(dt_val) * rng.standard_normal()
            ou_noise = max(-1.5, min(1.5, ou_noise))
        last_t = t
        rate *= math.exp(ou_noise)

        # Maintenance dip.
        in_dip = any(ws <= t <= we for ws, we in dip_windows)
        if in_dip:
            rate *= 0.02

        # Surge windows.
        for sw_start, sw_end, sw_mult in surge_windows:
            if sw_start <= t <= sw_end:
                rate *= sw_mult
                break

        # Inter-arrival time.
        base_rate_sim_sec = rate * diurnal_peak_rate / 60.0
        base_rate_wall_sec = base_rate_sim_sec * time_scale
        inter_arrival = rng.exponential(1.0 / max(base_rate_wall_sec, 0.001))
        t += inter_arrival
        if t >= wall_sec:
            break

        # Check for mega-burst.
        burst_size = 1
        while burst_idx < len(burst_events) and burst_events[burst_idx][0] <= t:
            burst_size += burst_events[burst_idx][1]
            burst_idx += 1

        # Micro-bursts (3% chance).
        if burst_size == 1 and rng.random() < 0.03:
            burst_size = int(rng.integers(micro_burst_min, micro_burst_max + 1))

        for _ in range(burst_size):
            job_id += 1
            wl_id = f"workload-{job_id:06d}"
            r = rng.random()

            if r < 0.35 * (1.0 - gpu_ratio):
                cpu_req = float(rng.choice([4, 8, 16, 32]))
                gpu_req = 0
                is_perf = False
                cpu_util = float(rng.uniform(0.3, 0.8))
                gpu_util = 0.0
                duration = float(rng.uniform(60, 600))
                wl_type = "cpu_preprocess"
                cpu_class = rng.choice(cpu_classes)
                gpu_class = ""
            elif r < 0.55 * (1.0 - gpu_ratio * 0.3):
                cpu_req = float(rng.choice([32, 64, 96, 128]))
                gpu_req = 0
                is_perf = rng.random() < perf_ratio
                cpu_util = float(rng.uniform(0.4, 0.9))
                gpu_util = 0.0
                duration = float(rng.uniform(300, 3600))
                wl_type = "cpu_analytics"
                cpu_class = rng.choice(cpu_classes)
                gpu_class = ""
            elif r < 0.80:
                cpu_req = float(rng.choice([16, 32, 64]))
                gpu_req = int(rng.choice([2, 4, 4, 8]))
                is_perf = rng.random() < 0.8
                cpu_util = float(rng.uniform(0.3, 0.6))
                gpu_util = float(rng.uniform(0.6, 0.95))
                duration = float(rng.uniform(3600, 28800))
                wl_type = "distributed_training" if gpu_req >= 2 else "single_gpu_training"
                cpu_class = "cpu.mixed"
                gpu_class = rng.choice(gpu_classes)
            elif r < 0.92:
                cpu_req = float(rng.choice([4, 8, 16]))
                gpu_req = int(rng.choice([1, 2, 4]))
                is_perf = False
                cpu_util = float(rng.uniform(0.3, 0.6))
                gpu_util = float(rng.uniform(0.3, 0.7))
                duration = float(rng.uniform(60, 900))
                wl_type = "debug_eval"
                cpu_class = "cpu.mixed"
                gpu_class = rng.choice(gpu_classes)
            else:
                cpu_req = float(rng.choice([128, 192]))
                gpu_req = 0
                is_perf = True
                cpu_util = float(rng.uniform(0.5, 0.95))
                gpu_util = 0.0
                duration = float(rng.uniform(1800, 7200))
                wl_type = "cpu_analytics"
                cpu_class = rng.choice(cpu_classes)
                gpu_class = ""

            # Override GPU to 0 for CPU-only experiments.
            if gpu_ratio == 0:
                gpu_req = 0
                gpu_util = 0.0
                gpu_class = ""
                is_perf = rng.random() < perf_ratio
                wl_type = rng.choice(["cpu_preprocess", "cpu_analytics"])

            intent = "performance" if is_perf else "standard"
            mem_intensity = float(rng.uniform(0.3, 0.9))
            io_intensity = float(rng.uniform(0.05, 0.3))

            cpu_units = duration * cpu_req * max(0.10, cpu_util) * base_speed_per_core * work_scale
            gpu_units = duration * max(gpu_req, 0) * max(0.10, gpu_util) * base_speed_per_core * work_scale if gpu_req > 0 else 0.0

            mem_gib = max(1, int(cpu_req * rng.uniform(0.5, 2.0)))
            requests = {"cpu": f"{cpu_req:.2f}", "memory": f"{mem_gib}Gi"}
            if gpu_req > 0:
                requests[gpu_resource] = str(gpu_req)

            pod_template = {"requests": requests}
            if is_perf:
                pod_template["affinity"] = {
                    "nodeAffinity": {
                        "requiredDuringSchedulingIgnoredDuringExecution": {
                            "nodeSelectorTerms": [{
                                "matchExpressions": [{
                                    "key": "joulie.io/power-profile",
                                    "operator": "NotIn",
                                    "values": ["eco"],
                                }],
                            }],
                        },
                    },
                }

            wl_class = {}
            if cpu_class:
                wl_class["cpu"] = cpu_class
            if gpu_class:
                wl_class["gpu"] = gpu_class

            profile = {
                "cpuUtilization": cpu_util,
                "gpuUtilization": gpu_util,
                "memoryIntensity": mem_intensity,
                "ioIntensity": io_intensity,
            }

            wl_rec = {
                "type": "workload",
                "schemaVersion": "v2",
                "workloadId": wl_id,
                "submitTimeOffsetSec": t,
                "namespace": "default",
                "workloadType": wl_type,
                "durationSec": duration,
                "intentClass": intent,
                "workloadClass": wl_class,
                "sharedIntensityProfile": profile,
                "pods": [{"role": "worker", "replicas": 1, "requests": requests}],
            }
            lines.append(json.dumps(wl_rec, separators=(",", ":")))

            job_rec = {
                "type": "job",
                "schemaVersion": "v2",
                "jobId": f"{wl_id}-worker-01",
                "workloadId": wl_id,
                "workloadType": wl_type,
                "podRole": "worker",
                "submitTimeOffsetSec": t,
                "namespace": "default",
                "intentClass": intent,
                "podTemplate": pod_template,
                "work": {"cpuUnits": cpu_units, "gpuUnits": gpu_units},
                "sensitivity": {"cpu": 0.5 if is_perf else 0, "gpu": 0.5 if (is_perf and gpu_req > 0) else 0},
                "workloadClass": wl_class,
                "workloadProfile": profile,
            }
            lines.append(json.dumps(job_rec, separators=(",", ":")))

    trace_path.write_text("\n".join(lines) + "\n")
    job_count = sum(1 for l in lines if '"type":"job"' in l)
    log(f"trace generated (python) seed={seed} jobs={job_count} wall_sec={wall_sec:.0f} sim_hours={sim_hours:.0f}")
    return trace_path
