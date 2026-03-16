#!/usr/bin/env python3
import datetime as dt
import json
import math
import os
import pathlib
import re

import pandas as pd

ROOT = pathlib.Path("experiments/01-kwok-benchmark")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results"))).resolve()
RUN_ID_RE = re.compile(r"_b([ABC])_s(\d+)$")


def parse_iso_utc(ts: str):
    if not ts or not isinstance(ts, str):
        return None
    try:
        return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except ValueError:
        return None


def count_trace_jobs(trace_path: pathlib.Path) -> int:
    if not trace_path.exists():
        return 0
    return sum(1 for line in trace_path.read_text().splitlines() if line.strip())


def parse_run_id_fallback(run_dir: pathlib.Path):
    m = RUN_ID_RE.search(run_dir.name)
    if not m:
        return "", None
    baseline = m.group(1)
    try:
        seed = int(m.group(2))
    except ValueError:
        seed = None
    return baseline, seed


def load_json_file(path: pathlib.Path):
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text())
    except json.JSONDecodeError:
        return None


def to_float(v):
    try:
        out = float(v)
        if math.isfinite(out):
            return out
    except (TypeError, ValueError):
        return None
    return None


def workload_pod_status(pods_doc: dict):
    if not isinstance(pods_doc, dict):
        return None, None
    items = pods_doc.get("items")
    if not isinstance(items, list):
        return None, None
    total = 0
    active = 0
    for item in items:
        meta = item.get("metadata", {}) or {}
        labels = meta.get("labels", {}) or {}
        if labels.get("app.kubernetes.io/part-of") != "joulie-sim-workload":
            continue
        total += 1
        phase = (item.get("status", {}) or {}).get("phase", "")
        if phase in {"Pending", "Running"}:
            active += 1
    return total, active


def integrate_energy_joules_from_events(events_doc: dict, time_scale: float):
    if not isinstance(events_doc, dict):
        return None, 0, 0
    events = events_doc.get("events")
    if not isinstance(events, list):
        return None, 0, 0

    telemetry_count = 0
    by_node = {}
    for e in events:
        if not isinstance(e, dict):
            continue
        if e.get("kind") != "telemetry":
            continue
        payload = e.get("payload")
        if not isinstance(payload, dict):
            continue
        power = to_float(payload.get("packagePowerWatts"))
        ts = parse_iso_utc(e.get("timestamp"))
        node = e.get("node") or ""
        if power is None or ts is None or not node:
            continue
        telemetry_count += 1
        by_node.setdefault(node, []).append((ts, power))

    if not by_node:
        return None, len(events), telemetry_count

    energy_wall_j = 0.0
    for samples in by_node.values():
        samples.sort(key=lambda x: x[0])
        prev_ts = None
        prev_power = None
        for ts, power in samples:
            if prev_ts is not None and prev_power is not None:
                dt_s = (ts - prev_ts).total_seconds()
                if dt_s > 0:
                    energy_wall_j += prev_power * dt_s
            prev_ts = ts
            prev_power = power

    # Convert wall-time integration to simulated-time integration.
    energy_sim_j = energy_wall_j * max(0.0, time_scale)
    return energy_sim_j, len(events), telemetry_count


def energy_from_debug_energy(debug_energy_doc: dict, time_scale: float):
    if not isinstance(debug_energy_doc, dict):
        return None
    total_j = to_float(debug_energy_doc.get("totalJoules"))
    if total_j is None:
        return None
    return total_j * max(0.0, time_scale)


def collect_one_run(run_dir: pathlib.Path):
    run_summary = load_json_file(run_dir / "run_summary.json") or {}
    metadata = load_json_file(run_dir / "metadata.json") or {}

    baseline, seed = parse_run_id_fallback(run_dir)
    baseline = run_summary.get("baseline", baseline)
    seed = run_summary.get("seed", seed)

    wall_seconds = to_float(run_summary.get("wall_seconds"))
    time_scale = to_float(metadata.get("timeScale"))
    if time_scale is None or time_scale <= 0:
        time_scale = 1.0

    jobs_total = count_trace_jobs(run_dir / "trace.jsonl")

    sim_seconds = None
    throughput_wall = None
    throughput_sim = None
    throughput_sim_hour = None
    if wall_seconds and wall_seconds > 0:
        sim_seconds = wall_seconds * time_scale
        throughput_wall = jobs_total / wall_seconds
        throughput_sim = jobs_total / sim_seconds if sim_seconds > 0 else None
        throughput_sim_hour = throughput_sim * 3600 if throughput_sim is not None else None

    sim_event_count = 0
    telemetry_event_count = 0
    energy_sim_j = energy_from_debug_energy(load_json_file(run_dir / "sim_debug_energy.json"), time_scale)
    energy_source = "debug_energy"
    if energy_sim_j is None:
        energy_sim_j, sim_event_count, telemetry_event_count = integrate_energy_joules_from_events(
            load_json_file(run_dir / "sim_debug_events.json"), time_scale
        )
        energy_source = "debug_events" if energy_sim_j is not None else "none"

    avg_cluster_power_w = None
    if energy_sim_j is not None and sim_seconds and sim_seconds > 0:
        avg_cluster_power_w = energy_sim_j / sim_seconds

    workload_pods_total, workload_pods_active = workload_pod_status(load_json_file(run_dir / "pods.json"))
    run_completed = workload_pods_active == 0 if workload_pods_active is not None else False
    if workload_pods_active is None:
        run_outcome = "unknown"
    else:
        run_outcome = "completed" if run_completed else "incomplete"

    return {
        "run_id": run_dir.name,
        "baseline": baseline,
        "seed": seed,
        "timestamp_utc": run_summary.get("timestamp_utc", ""),
        "wall_seconds": wall_seconds,
        "time_scale": time_scale,
        "sim_seconds": sim_seconds,
        "jobs_total": jobs_total,
        "throughput_jobs_per_wall_sec": throughput_wall,
        "throughput_jobs_per_sim_sec": throughput_sim,
        "throughput_jobs_per_sim_hour": throughput_sim_hour,
        "energy_sim_joules_est": energy_sim_j,
        "energy_sim_kwh_est": (energy_sim_j / 3_600_000.0) if energy_sim_j is not None else None,
        "avg_cluster_power_w_est": avg_cluster_power_w,
        "energy_source": energy_source,
        "sim_event_count": sim_event_count,
        "telemetry_event_count": telemetry_event_count,
        "workload_pods_total_at_collection": workload_pods_total,
        "workload_pods_active_at_collection": workload_pods_active,
        "run_completed": run_completed,
        "run_outcome": run_outcome,
        "trace_sha256": run_summary.get("trace_sha256", ""),
        "git_commit": metadata.get("git_commit", ""),
    }


def main():
    rows = []
    for d in sorted(RESULTS.iterdir()):
        if not d.is_dir():
            continue
        if d.name in {"plots", "traces"}:
            continue
        row = collect_one_run(d)
        if row["baseline"]:
            rows.append(row)

    if not rows:
        print("no runs found")
        return

    out = RESULTS / "summary.csv"
    df = pd.DataFrame(rows)
    df.sort_values(["baseline", "seed", "run_id"], inplace=True)
    df.to_csv(out, index=False)
    print(f"wrote {out}")

    numeric_metrics = [
        "wall_seconds",
        "throughput_jobs_per_sim_hour",
        "energy_sim_kwh_est",
        "avg_cluster_power_w_est",
    ]
    baseline_rows = []
    for baseline, grp in df.groupby("baseline"):
        completed = grp[grp["run_completed"] == True].copy()
        row = {
            "baseline": baseline,
            "runs_total": len(grp),
            "runs_completed": len(completed),
            "runs_incomplete": len(grp) - len(completed),
            "completion_rate_pct": 100.0 * len(completed) / len(grp) if len(grp) else None,
        }
        for metric in numeric_metrics:
            series = pd.to_numeric(completed[metric], errors="coerce").dropna()
            if series.empty:
                row[f"{metric}_mean"] = None
                row[f"{metric}_std"] = None
                row[f"{metric}_ci95"] = None
                continue
            mean = float(series.mean())
            std = float(series.std(ddof=1)) if len(series) > 1 else 0.0
            ci95 = 1.96 * std / math.sqrt(len(series)) if len(series) > 1 else 0.0
            row[f"{metric}_mean"] = mean
            row[f"{metric}_std"] = std
            row[f"{metric}_ci95"] = ci95
        baseline_rows.append(row)

    baseline_out = RESULTS / "baseline_summary.csv"
    baseline_df = pd.DataFrame(baseline_rows)
    if not baseline_df.empty:
        baseline_df.sort_values(["baseline"], inplace=True)
    baseline_df.to_csv(baseline_out, index=False)
    print(f"wrote {baseline_out}")


if __name__ == "__main__":
    main()
