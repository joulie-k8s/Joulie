#!/usr/bin/env python3
"""Standalone sweep — runs the simulator in standalone mode (no K8s).

Usage:
  python3 scripts/standalone_sweep.py --config experiments/01-cpu-only-benchmark/configs/benchmark-5k-debug.yaml

Reads the same benchmark config YAML as the K8s sweep but bypasses Kind/KWOK
entirely. Each (baseline, seed) pair runs the Go simulator binary directly,
producing a timeseries.csv in the results directory.
"""

import argparse
import concurrent.futures
import csv as csv_mod
import datetime as dt
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import time

import numpy as np
import yaml

START_TS = time.time()


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    elapsed = time.time() - START_TS
    print(f"[standalone-sweep {now} +{elapsed:8.1f}s] {msg}", flush=True)


def run(cmd, check=True, capture=False, env=None):
    if capture:
        return subprocess.run(cmd, check=check, text=True, capture_output=True, env=env)
    return subprocess.run(cmd, check=check, env=env)


def load_config(path: pathlib.Path) -> dict:
    if not path.exists():
        raise SystemExit(f"config file not found: {path}")
    data = yaml.safe_load(path.read_text())
    return data if isinstance(data, dict) else {}


def get_cfg(cfg: dict, *keys, default=None):
    cur = cfg
    for k in keys:
        if not isinstance(cur, dict) or k not in cur:
            return default
        cur = cur[k]
    return cur


def to_baselines(raw) -> list[str]:
    if raw is None:
        return ["A", "B", "C"]
    if isinstance(raw, str):
        vals = [x.strip().upper() for x in raw.split(",") if x.strip()]
    elif isinstance(raw, list):
        vals = [str(x).strip().upper() for x in raw if str(x).strip()]
    else:
        raise SystemExit("baselines must be a comma-separated string or a list")
    invalid = [b for b in vals if b not in {"A", "B", "C"}]
    if invalid:
        raise SystemExit(f"invalid baseline(s): {','.join(invalid)}")
    return vals or ["A", "B", "C"]


def generate_trace_diurnal(
    results_dir: pathlib.Path,
    seed: int,
    sim_hours: float,
    gpu_ratio: float,
    work_scale: float,
    time_scale: float,
    base_speed_per_core: float,
    diurnal_peak_rate: float = 80.0,
) -> pathlib.Path:
    """Generate traces using Go workloadgen --mode diurnal (exp04-style NHPP cosine)."""
    traces_dir = results_dir / "traces"
    traces_dir.mkdir(parents=True, exist_ok=True)
    trace_path = traces_dir / f"seed_{seed}_canonical.jsonl"
    if trace_path.exists():
        log(f"reusing trace seed={seed} file={trace_path}")
        return trace_path

    log(f"generating diurnal trace seed={seed} sim_hours={sim_hours} gpu_ratio={gpu_ratio} work_scale={work_scale}")
    cmd = [
        "go", "run", "./simulator/cmd/workloadgen",
        "--mode", "diurnal",
        "--seed", str(seed),
        "--out", str(trace_path),
        "--sim-hours", str(sim_hours),
        "--gpu-ratio", str(gpu_ratio),
        "--work-scale", str(work_scale),
        "--time-scale", str(time_scale),
        "--base-speed-per-core", str(base_speed_per_core),
        "--emit-workload-records=true",
        "--diurnal-burst-min", "20",
        "--diurnal-burst-max", "80",
        "--diurnal-peak-rate", str(int(diurnal_peak_rate)),
    ]
    run(cmd, check=True)
    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"diurnal trace generated records={count}")
    return trace_path


def generate_trace_python(
    results_dir: pathlib.Path,
    seed: int,
    sim_hours: float,
    gpu_ratio: float,
    work_scale: float,
    time_scale: float,
    base_speed_per_core: float,
) -> pathlib.Path:
    """Generate traces using exp04-style NHPP arrivals with cosine diurnal pattern.

    Produces realistic datacenter workload with:
    - Cosine day/night cycle (trough at 4 AM, peak at 4 PM)
    - 10% burst probability (50-200 simultaneous jobs)
    - Mixed job sizes: small/medium CPU, GPU training (1-8 GPUs), GPU inference, large CPU
    - Direct wall-second offsets (simulator applies time_scale internally)
    """
    import math

    traces_dir = results_dir / "traces"
    traces_dir.mkdir(parents=True, exist_ok=True)
    trace_path = traces_dir / f"seed_{seed}_canonical.jsonl"
    if trace_path.exists():
        log(f"reusing trace seed={seed} file={trace_path}")
        return trace_path

    rng = np.random.default_rng(seed)
    # Sim duration in wall-seconds (the trace uses wall-second offsets).
    wall_sec = sim_hours * 3600.0 / time_scale

    # CPU workload classes.
    cpu_classes = ["cpu.compute_bound", "cpu.memory_bound", "cpu.mixed"]
    gpu_classes = ["gpu.compute", "gpu.mixed", "gpu.memory_bound"]

    # GPU resource name.
    gpu_resource = "nvidia.com/gpu"

    lines = []
    job_id = 0
    t = 0.0
    while t < wall_sec:
        # Current sim-hour for diurnal pattern.
        sim_h = (t * time_scale) / 3600.0
        hour_of_day = sim_h % 24.0
        phase = 2 * math.pi * (hour_of_day - 4.0) / 24.0
        rate = 0.25 + 0.70 * (1 - math.cos(phase)) / 2.0
        # Base rate: ~80 jobs/min at peak in sim-time, convert to wall-time.
        base_rate_sim_sec = rate * 80.0 / 60.0
        base_rate_wall_sec = base_rate_sim_sec * time_scale
        inter_arrival = rng.exponential(1.0 / max(base_rate_wall_sec, 0.001))
        t += inter_arrival
        if t >= wall_sec:
            break

        # Burst: 10% chance of 50-200 simultaneous jobs.
        burst_size = 1
        if rng.random() < 0.10:
            burst_size = int(rng.integers(50, 201))

        for _ in range(burst_size):
            job_id += 1
            wl_id = f"workload-{job_id:06d}"
            r = rng.random()
            has_gpu = r >= (1.0 - gpu_ratio * 0.70)  # Scale GPU fraction by gpu_ratio

            if r < 0.35 * (1.0 - gpu_ratio):
                # Small CPU job.
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
                # Medium CPU job.
                cpu_req = float(rng.choice([32, 64, 96, 128]))
                gpu_req = 0
                is_perf = rng.random() < 0.3
                cpu_util = float(rng.uniform(0.4, 0.9))
                gpu_util = 0.0
                duration = float(rng.uniform(300, 3600))
                wl_type = "cpu_analytics"
                cpu_class = rng.choice(cpu_classes)
                gpu_class = ""
            elif r < 0.80:
                # GPU training (2-8 GPUs).
                cpu_req = float(rng.choice([16, 32, 64]))
                gpu_req = int(rng.choice([2, 4, 4, 8]))
                is_perf = True
                cpu_util = float(rng.uniform(0.3, 0.6))
                gpu_util = float(rng.uniform(0.6, 0.95))
                duration = float(rng.uniform(3600, 28800))
                wl_type = "distributed_training" if gpu_req >= 2 else "single_gpu_training"
                cpu_class = "cpu.mixed"
                gpu_class = rng.choice(gpu_classes)
            elif r < 0.92:
                # GPU inference (1-4 GPUs).
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
                # Large CPU job.
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
                is_perf = rng.random() < 0.20
                wl_type = rng.choice(["cpu_preprocess", "cpu_analytics"])

            intent = "performance" if is_perf else "standard"
            mem_intensity = float(rng.uniform(0.3, 0.9))
            io_intensity = float(rng.uniform(0.05, 0.3))

            # Compute work units (how long the job takes to complete).
            cpu_units = duration * cpu_req * max(0.10, cpu_util) * base_speed_per_core * work_scale
            gpu_units = duration * max(gpu_req, 0) * max(0.10, gpu_util) * base_speed_per_core * work_scale if gpu_req > 0 else 0.0

            # Memory request (GiB).
            mem_gib = max(1, int(cpu_req * rng.uniform(0.5, 2.0)))

            # Build requests dict.
            requests = {"cpu": f"{cpu_req:.2f}", "memory": f"{mem_gib}Gi"}
            if gpu_req > 0:
                requests[gpu_resource] = str(gpu_req)

            # Pod template with optional affinity for performance jobs.
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

            # Workload record.
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

            # Job record.
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


def generate_trace(
    results_dir: pathlib.Path,
    seed: int,
    jobs: int,
    mean_inter_arrival_sec: float,
    perf_ratio: float,
    compute_bound_perf_boost: float,
    gpu_ratio: float,
    burst_day_probability: float,
    burst_mean_jobs: float,
    burst_multiplier: float,
    dip_day_probability: float,
    dip_multiplier: float,
    emit_workload_records: bool,
    work_scale: float,
    allowed_workload_types: list[str] | None,
    time_scale: float,
) -> pathlib.Path:
    traces_dir = results_dir / "traces"
    traces_dir.mkdir(parents=True, exist_ok=True)
    trace_path = traces_dir / f"seed_{seed}_canonical.jsonl"
    if trace_path.exists():
        log(f"reusing trace seed={seed} file={trace_path}")
        return trace_path

    log(f"generating trace seed={seed} jobs={jobs} mia={mean_inter_arrival_sec}")
    cmd = [
        "go", "run", "./simulator/cmd/workloadgen",
        "--jobs", str(jobs),
        "--seed", str(seed),
        "--out", str(trace_path),
        "--mean-inter-arrival-sec", str(mean_inter_arrival_sec),
        "--perf-ratio", str(perf_ratio),
        "--compute-bound-perf-boost", str(compute_bound_perf_boost),
        "--gpu-ratio", str(gpu_ratio),
        "--burst-day-probability", str(burst_day_probability),
        "--burst-mean-jobs", str(burst_mean_jobs),
        "--burst-multiplier", str(burst_multiplier),
        "--dip-day-probability", str(dip_day_probability),
        "--dip-multiplier", str(dip_multiplier),
        f"--emit-workload-records={str(emit_workload_records).lower()}",
        "--time-scale", str(time_scale),
    ]
    run(cmd, check=True)

    # Apply work_scale and allowed_workload_types filter.
    if work_scale != 1.0 or allowed_workload_types:
        allowed = set(allowed_workload_types or [])
        filtered = []
        for raw_line in trace_path.read_text().splitlines():
            raw_line = raw_line.strip()
            if not raw_line:
                continue
            rec = json.loads(raw_line)
            if allowed and rec.get("workloadType") not in allowed:
                continue
            if rec.get("type", "job") == "job":
                work = rec.get("work")
                if isinstance(work, dict):
                    if "cpuUnits" in work:
                        work["cpuUnits"] = float(work["cpuUnits"]) * work_scale
                    if "gpuUnits" in work:
                        work["gpuUnits"] = float(work["gpuUnits"]) * work_scale
            filtered.append(json.dumps(rec, separators=(",", ":")))
        trace_path.write_text("\n".join(filtered) + ("\n" if filtered else ""))

    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"trace generated records={count}")
    return trace_path


def derive_baseline_trace(baseline: str, canonical: pathlib.Path, strip_affinity_for_a: bool) -> pathlib.Path:
    """For baseline A, optionally strip power-profile affinity from the trace."""
    if baseline != "A" or not strip_affinity_for_a:
        return canonical
    out = canonical.parent / f"{canonical.stem}_baseline_A.jsonl"
    if out.exists():
        return out
    lines = []
    for raw_line in canonical.read_text().splitlines():
        raw_line = raw_line.strip()
        if not raw_line:
            continue
        rec = json.loads(raw_line)
        if "podTemplate" in rec:
            rec["podTemplate"].pop("affinity", None)
        if "intentClass" in rec:
            rec["intentClass"] = "standard"
        lines.append(json.dumps(rec, separators=(",", ":")))
    out.write_text("\n".join(lines) + ("\n" if lines else ""))
    return out


DOCKER_IMAGE = "openmodelica/openmodelica:v1.26.3-ompython"


def apply_fmu_to_timeseries(ts_path: pathlib.Path, fmu_path: pathlib.Path, time_scale: float):
    """Run the FMU co-simulation on a timeseries.csv and overwrite PUE/cooling columns.

    Uses Docker to avoid glibc compatibility issues with the FMU shared library.
    """
    log(f"applying FMU to {ts_path} (time_scale={time_scale})")

    with tempfile.TemporaryDirectory(prefix="joulie-fmu-") as tmpdir:
        work_dir = pathlib.Path(tmpdir)

        # Copy FMU and timeseries to work dir.
        shutil.copy2(fmu_path, work_dir / fmu_path.name)
        shutil.copy2(ts_path, work_dir / "timeseries.csv")

        # Write runner script.
        runner = f"""\
#!/usr/bin/env python3
import csv, sys
import numpy as np
from fmpy import read_model_description, simulate_fmu

fmu = "{fmu_path.name}"

rows = []
with open("timeseries.csv") as f:
    reader = csv.DictReader(f)
    fieldnames = reader.fieldnames
    for r in reader:
        rows.append(r)

n = len(rows)
time_scale = {time_scale}

time_arr = np.zeros(n)
q_it_arr = np.zeros(n)
t_out_arr = np.zeros(n)

for i, r in enumerate(rows):
    time_arr[i] = float(r["elapsed_sec"]) * time_scale
    q_it_arr[i] = max(float(r["it_power_w"]), 1.0)
    t_out_arr[i] = float(r["ambient_temp_c"]) + 273.15

step_size = (time_arr[1] - time_arr[0]) if n > 1 else 60.0
stop_time = time_arr[-1] if n > 0 else 86400

dtype = [("time", np.float64), ("Q_IT", np.float64), ("T_outdoor", np.float64)]
signals = np.array(list(zip(time_arr, q_it_arr, t_out_arr)), dtype=dtype)

print(f"Running FMU: {{n}} steps, step_size={{step_size:.0f}}s, stop={{stop_time:.0f}}s", file=sys.stderr)

result = simulate_fmu(
    fmu,
    stop_time=stop_time,
    step_size=step_size,
    input=signals,
    output=["P_cooling", "T_indoor", "COP"],
)

# Interpolate FMU results to match input timesteps.
fmu_time = result["time"]
p_cooling = np.interp(time_arr, fmu_time, result["P_cooling"])
p_cooling = np.maximum(0, p_cooling)

# Rewrite timeseries with corrected PUE/cooling.
with open("timeseries_fmu.csv", "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(fieldnames)
    for i, r in enumerate(rows):
        it_w = float(r["it_power_w"])
        cool_w = p_cooling[i]
        pue = (it_w + cool_w) / max(it_w, 1.0)
        fac_w = it_w + cool_w
        r["pue"] = f"{{pue:.4f}}"
        r["cooling_power_w"] = f"{{cool_w:.1f}}"
        r["facility_power_w"] = f"{{fac_w:.1f}}"
        w.writerow([r[col] for col in fieldnames])

print(f"Wrote {{n}} rows to timeseries_fmu.csv", file=sys.stderr)
"""
        (work_dir / "run_fmu.py").write_text(runner)

        # Run inside Docker.
        result = subprocess.run(
            [
                "docker", "run", "--rm",
                "-v", f"{work_dir}:/work",
                "-w", "/work",
                DOCKER_IMAGE,
                "bash", "-c",
                "pip install fmpy numpy 2>/dev/null | tail -1 && python3 run_fmu.py",
            ],
            capture_output=True, text=True, timeout=600,
        )

        if result.stderr:
            for line in result.stderr.strip().split("\n")[-5:]:
                log(f"  [FMU] {line}")

        fmu_output = work_dir / "timeseries_fmu.csv"
        if not fmu_output.exists():
            log(f"ERROR: FMU Docker run failed. stdout: {result.stdout[:500]}")
            return False

        # Overwrite original timeseries.
        shutil.copy2(fmu_output, ts_path)
        log(f"FMU post-processing complete: {ts_path}")
        return True


def run_standalone(
    simulator_bin: str,
    baseline: str,
    trace_path: pathlib.Path,
    output_dir: pathlib.Path,
    inventory_path: str,
    cfg: dict,
    time_scale: float,
    catalog_path: str,
):
    output_dir.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["SIM_STANDALONE"] = "true"
    env["SIM_STANDALONE_INVENTORY"] = inventory_path
    env["SIM_STANDALONE_BASELINE"] = baseline
    env["SIM_WORKLOAD_TRACE_PATH"] = str(trace_path)
    env["SIM_DEBUG_PERSIST_DIR"] = str(output_dir)
    env["SIM_BASE_SPEED_PER_CORE"] = str(get_cfg(cfg, "simulator", "base_speed_per_core", default=1.0))

    # Facility ambient: period_sec in config is wall-seconds, convert to hours for SIM_FACILITY_TEMP_PERIOD_H.
    ambient_period_sec = float(get_cfg(cfg, "simulator", "facility_ambient_period_sec", default=720))
    # In standalone mode there is no time_scale distinction (we advance 1 wall-sec per tick).
    # The ambient period should represent 24 sim-hours = 24*3600/time_scale wall-seconds.
    env["SIM_FACILITY_AMBIENT_TEMP_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_base_c", default=22.0))
    env["SIM_FACILITY_TEMP_AMPLITUDE_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_amplitude_c", default=8.0))
    env["SIM_FACILITY_TEMP_PERIOD_H"] = str(ambient_period_sec / 3600.0)

    # Hardware catalog.
    if catalog_path:
        env["SIM_HARDWARE_CATALOG_PATH"] = catalog_path

    # Timeout: truncate at steady state. Config timeout is wall-seconds.
    timeout = int(get_cfg(cfg, "run", "timeout", default=0))
    if timeout > 0:
        env["SIM_SA_TIMEOUT"] = str(timeout)

    # Policy env vars.
    env["SIM_SA_CPU_ECO_PCT"] = str(get_cfg(cfg, "policy", "caps", "cpu_eco_pct_of_max", default=60))
    env["SIM_SA_GPU_ECO_PCT"] = str(get_cfg(cfg, "policy", "caps", "gpu_eco_pct_of_max", default=70))
    env["SIM_SA_HP_FRAC"] = str(get_cfg(cfg, "policy", "static", "hp_frac", default=0.30))
    env["SIM_SA_HP_BASE_FRAC"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_base_frac", default=0.30))
    env["SIM_SA_HP_MIN"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_min", default=1))
    env["SIM_SA_HP_MAX"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_max", default=25))
    env["SIM_SA_PERF_PER_HP_NODE"] = str(get_cfg(cfg, "policy", "queue_aware", "perf_per_hp_node", default=10))
    # Reconcile interval in wall-seconds (standalone doesn't use time_scale).
    qa_sim_sec = float(get_cfg(cfg, "policy", "loop", "queue_aware_operator_reconcile_sim_seconds", default=300))
    env["SIM_SA_RECONCILE_INTERVAL"] = str(qa_sim_sec / time_scale)

    log(f"running standalone baseline={baseline} trace={trace_path} output={output_dir}")
    t0 = time.time()
    result = subprocess.run([simulator_bin], env=env, capture_output=True, text=True)
    elapsed = time.time() - t0

    # Write simulator logs.
    (output_dir / "simulator.log").write_text(result.stdout + result.stderr)

    if result.returncode != 0:
        log(f"ERROR: standalone baseline={baseline} failed rc={result.returncode} in {elapsed:.1f}s")
        log(f"  stderr: {result.stderr[-500:]}")
        return False

    ts_file = output_dir / "timeseries.csv"
    rows = sum(1 for _ in ts_file.read_text().splitlines()) - 1 if ts_file.exists() else 0
    log(f"standalone baseline={baseline} completed in {elapsed:.1f}s rows={rows}")

    # Write metadata.
    metadata = {
        "baseline": baseline,
        "mode": "standalone",
        "wall_seconds": elapsed,
        "timeseries_rows": rows,
        "timeScale": time_scale,
        "timestamp_utc": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    (output_dir / "metadata.json").write_text(json.dumps(metadata, indent=2))
    return True


def main():
    ap = argparse.ArgumentParser(description="Standalone sweep (no K8s)")
    ap.add_argument("--config", required=True, type=str, help="Path to benchmark config YAML")
    ap.add_argument("--simulator", default="", type=str, help="Path to simulator binary (default: build from source)")
    ap.add_argument("--results-dir", default="", type=str, help="Override results directory")
    ap.add_argument("--baselines", default="", type=str, help="Override baselines (comma-separated)")
    ap.add_argument("--fmu", default="", type=str, help="Path to FMU file for PUE co-simulation post-processing")
    ap.add_argument("--trace-mode", default="go", choices=["go", "python"],
                    help="Trace generator: 'go' (workloadgen binary) or 'python' (exp04-style NHPP cosine diurnal)")
    args = ap.parse_args()

    cfg_path = pathlib.Path(args.config).resolve()
    cfg = load_config(cfg_path)

    # Determine experiment root from config path.
    exp_root = cfg_path.parent.parent

    results_dir = pathlib.Path(args.results_dir) if args.results_dir else exp_root / "results"
    results_dir = results_dir.resolve()
    results_dir.mkdir(parents=True, exist_ok=True)

    # Build or locate simulator binary.
    simulator_bin = args.simulator
    if not simulator_bin:
        log("building simulator binary...")
        simulator_bin = str(results_dir / "joulie-simulator")
        run(["go", "build", "-o", simulator_bin, "./simulator/cmd/simulator/"], check=True)
        log(f"simulator built: {simulator_bin}")

    # Generate hardware catalog from inventory.
    inventory_source = str(get_cfg(cfg, "inventory", "source", default=""))
    if not inventory_source:
        raise SystemExit("inventory.source not set in config")

    generated_dir = results_dir / "generated"
    generated_dir.mkdir(parents=True, exist_ok=True)
    catalog_path = str(generated_dir / "hardware.generated.yaml")
    run([
        "python3", "scripts/generate_heterogeneous_assets.py",
        "--input", inventory_source,
        "--out-nodes", str(generated_dir / "00-kwok-nodes.yaml"),
        "--out-classes", str(generated_dir / "10-node-classes.yaml"),
        "--out-catalog", catalog_path,
    ], check=True)
    log(f"generated hardware catalog: {catalog_path}")

    # Extract config values.
    jobs = int(get_cfg(cfg, "run", "jobs", default=100))
    seeds = int(get_cfg(cfg, "run", "seeds", default=1))
    mean_inter_arrival_sec = float(get_cfg(cfg, "run", "mean_inter_arrival_sec", default=0.1))
    time_scale = float(get_cfg(cfg, "run", "time_scale", default=120))
    perf_ratio = float(get_cfg(cfg, "workload", "perf_ratio", default=0.20))
    compute_bound_perf_boost = float(get_cfg(cfg, "workload", "compute_bound_perf_boost", default=3.5))
    gpu_ratio = float(get_cfg(cfg, "workload", "gpu_ratio", default=0.0))
    burst_day_probability = float(get_cfg(cfg, "workload", "burst_day_probability", default=0.50))
    burst_mean_jobs = float(get_cfg(cfg, "workload", "burst_mean_jobs", default=25.0))
    burst_multiplier = float(get_cfg(cfg, "workload", "burst_multiplier", default=3.5))
    dip_day_probability = float(get_cfg(cfg, "workload", "dip_day_probability", default=0.30))
    dip_multiplier = float(get_cfg(cfg, "workload", "dip_multiplier", default=0.08))
    emit_workload_records = str(get_cfg(cfg, "workload", "emit_workload_records", default=True)).lower() not in {"false", "0", "no"}
    work_scale = float(get_cfg(cfg, "workload", "work_scale", default=1.0))
    diurnal_peak_rate = float(get_cfg(cfg, "workload", "diurnal_peak_rate", default=80.0))
    baseline_a_strip_affinity = bool(get_cfg(cfg, "workload", "baseline_a_strip_affinity", default=True))
    allowed_workload_types = get_cfg(cfg, "workload", "allowed_workload_types", default=None)

    baselines_raw = args.baselines if args.baselines.strip() else get_cfg(cfg, "run", "baselines", default=["A", "B", "C"])
    baselines = to_baselines(baselines_raw)

    total_runs = len(baselines) * seeds
    log(f"standalone sweep config={cfg_path} baselines={','.join(baselines)} seeds={seeds} jobs={jobs} total_runs={total_runs}")

    def run_one(baseline: str, seed: int, canonical_trace: pathlib.Path):
        """Run a single (baseline, seed) pair: simulate + optional FMU post-processing."""
        trace_file = derive_baseline_trace(baseline, canonical_trace, baseline_a_strip_affinity)
        run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"_b{baseline}_s{seed}"
        output_dir = results_dir / run_id

        ok = run_standalone(
            simulator_bin=simulator_bin,
            baseline=baseline,
            trace_path=trace_file,
            output_dir=output_dir,
            inventory_path=inventory_source,
            cfg=cfg,
            time_scale=time_scale,
            catalog_path=catalog_path,
        )

        # Apply FMU post-processing for PUE if requested.
        if ok and args.fmu:
            fmu_path = pathlib.Path(args.fmu).resolve()
            ts_file = output_dir / "timeseries.csv"
            if ts_file.exists():
                apply_fmu_to_timeseries(ts_file, fmu_path, time_scale)

        return baseline, seed, ok

    base_speed_per_core = float(get_cfg(cfg, "simulator", "base_speed_per_core", default=2.0))
    timeout = int(get_cfg(cfg, "run", "timeout", default=1800))
    sim_hours = timeout * time_scale / 3600.0  # total sim-hours from timeout

    # Generate traces first (sequential), then run baselines in parallel.
    futures = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(baselines)) as pool:
        for seed in range(1, seeds + 1):
            if args.trace_mode == "python":
                canonical_trace = generate_trace_diurnal(
                    results_dir=results_dir,
                    seed=seed,
                    sim_hours=sim_hours,
                    gpu_ratio=gpu_ratio,
                    work_scale=work_scale,
                    time_scale=time_scale,
                    base_speed_per_core=base_speed_per_core,
                    diurnal_peak_rate=diurnal_peak_rate,
                )
            else:
                canonical_trace = generate_trace(
                    results_dir=results_dir,
                    seed=seed,
                    jobs=jobs,
                    mean_inter_arrival_sec=mean_inter_arrival_sec,
                    perf_ratio=perf_ratio,
                    compute_bound_perf_boost=compute_bound_perf_boost,
                    gpu_ratio=gpu_ratio,
                    burst_day_probability=burst_day_probability,
                    burst_mean_jobs=burst_mean_jobs,
                    burst_multiplier=burst_multiplier,
                    dip_day_probability=dip_day_probability,
                    dip_multiplier=dip_multiplier,
                    emit_workload_records=emit_workload_records,
                    work_scale=work_scale,
                    allowed_workload_types=allowed_workload_types,
                    time_scale=time_scale,
                )

            for baseline in baselines:
                futures.append(pool.submit(run_one, baseline, seed, canonical_trace))

        done = 0
        for future in concurrent.futures.as_completed(futures):
            done += 1
            baseline, seed, ok = future.result()
            status = "OK" if ok else "FAILED"
            log(f"[{done}/{total_runs}] baseline={baseline} seed={seed} {status}")

    log("standalone sweep completed")


if __name__ == "__main__":
    main()
