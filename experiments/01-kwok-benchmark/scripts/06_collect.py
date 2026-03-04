#!/usr/bin/env python3
import datetime as dt
import json
import math
import pathlib
import re

import pandas as pd

ROOT = pathlib.Path("experiments/01-kwok-benchmark")
RESULTS = ROOT / "results"
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

    energy_sim_j, sim_event_count, telemetry_event_count = integrate_energy_joules_from_events(
        load_json_file(run_dir / "sim_debug_events.json"), time_scale
    )

    avg_cluster_power_w = None
    if energy_sim_j is not None and sim_seconds and sim_seconds > 0:
        avg_cluster_power_w = energy_sim_j / sim_seconds

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
        "sim_event_count": sim_event_count,
        "telemetry_event_count": telemetry_event_count,
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


if __name__ == "__main__":
    main()
