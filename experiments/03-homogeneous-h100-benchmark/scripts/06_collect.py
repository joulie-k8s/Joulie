#!/usr/bin/env python3
import datetime as dt
import json
import math
import os
import pathlib
import re

import pandas as pd

ROOT = pathlib.Path("experiments/03-homogeneous-h100-benchmark")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results")))
RUN_ID_RE = re.compile(r"_b([ABC])_s(\d+)$")
JOB_COMPLETED_RE = re.compile(r"job completed id=(?P<job>\S+) node=(?P<node>\S+) class=(?P<class>\S+) elapsed=(?P<elapsed>[0-9.]+)s")
MIN_WORKLOAD_TYPE_JOBS = 10


def parse_iso_utc(ts: str):
    if not ts or not isinstance(ts, str):
        return None
    try:
        return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except ValueError:
        return None


def count_trace_records(trace_path: pathlib.Path) -> tuple[int, int]:
    if not trace_path.exists():
        return 0, 0
    jobs = 0
    workloads = 0
    for line in trace_path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if rec.get("type", "job") == "workload":
            workloads += 1
        elif rec.get("type", "job") == "job":
            jobs += 1
    return jobs, workloads


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


def prettify_cpu_family(value: str) -> str:
    if not value:
        return "CPU"
    value = value.replace("_", " ").replace("-", " ").strip()
    value = re.sub(r"\s+", " ", value)
    value = value.lower()
    mapping = {
        "amd epyc 9654": "CPU High-Core",
        "amd epyc 9375f": "CPU High-Frequency",
        "intel xeon gold 6530": "CPU Intensive",
    }
    return mapping.get(value, value.title())


def load_node_families(path: pathlib.Path) -> dict[str, str]:
    doc = load_json_file(path)
    if not isinstance(doc, dict):
        return {}
    out = {}
    for item in doc.get("items", []):
        meta = item.get("metadata", {}) or {}
        labels = meta.get("labels", {}) or {}
        name = meta.get("name", "")
        if not name:
            continue
        gpu_family = labels.get("joulie.io/gpu.product", "")
        if gpu_family:
            out[name] = gpu_family
            continue
        cpu_family = labels.get("joulie.io/raw.cpu-model") or labels.get("joulie.io/hw.cpu-model") or ""
        out[name] = prettify_cpu_family(cpu_family)
    return out


def parse_trace_index(trace_path: pathlib.Path) -> dict[str, dict]:
    out = {}
    if not trace_path.exists():
        return out
    for line in trace_path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if rec.get("type", "job") != "job":
            continue
        job_id = rec.get("jobId", "")
        if not job_id:
            continue
        pod = rec.get("podTemplate", {}) if isinstance(rec.get("podTemplate"), dict) else {}
        req = pod.get("requests", {}) if isinstance(pod.get("requests"), dict) else {}
        gpu_key = ""
        for key in ("nvidia.com/gpu", "amd.com/gpu", "gpu"):
            if key in req:
                gpu_key = key
                break
        target_node = ""
        target_product = ""
        aff = ((pod.get("affinity") or {}).get("nodeAffinity") or {}).get("requiredDuringSchedulingIgnoredDuringExecution", {})
        for term in aff.get("nodeSelectorTerms", []):
            for expr in term.get("matchExpressions", []):
                vals = expr.get("values") or []
                if not vals:
                    continue
                if expr.get("key") == "joulie.io/node-name":
                    target_node = vals[0]
                elif expr.get("key") == "joulie.io/gpu.product":
                    target_product = vals[0]
        out[job_id] = {
            "workload_id": rec.get("workloadId", ""),
            "workload_type": rec.get("workloadType", ""),
            "pod_role": rec.get("podRole", ""),
            "job_class": rec.get("workloadClass", {}).get("cpu") if req.get("cpu") and not gpu_key else rec.get("workloadClass", {}).get("gpu"),
            "gpu_key": gpu_key,
            "target_node": target_node,
            "target_product": target_product,
            "gang": bool(rec.get("gang", False)),
        }
    return out


def parse_completed_jobs(log_path: pathlib.Path) -> list[dict]:
    rows = []
    if not log_path.exists():
        return rows
    for line in log_path.read_text().splitlines():
        m = JOB_COMPLETED_RE.search(line)
        if not m:
            continue
        rows.append(
            {
                "job_id": m.group("job"),
                "node": m.group("node"),
                "completed_class": m.group("class"),
                "elapsed_seconds": to_float(m.group("elapsed")),
            }
        )
    return rows


def collect_job_rows(run_dir: pathlib.Path, summary_row: dict) -> list[dict]:
    trace_index = parse_trace_index(run_dir / "trace.jsonl")
    node_families = load_node_families(run_dir / "nodes.json")
    rows = []
    for item in parse_completed_jobs(run_dir / "simulator.log"):
        meta = trace_index.get(item["job_id"], {})
        node = item["node"]
        family = node_families.get(node, meta.get("target_product", "") or meta.get("target_node", "unknown"))
        rows.append(
            {
                "run_id": summary_row["run_id"],
                "baseline": summary_row["baseline"],
                "seed": summary_row["seed"],
                "job_id": item["job_id"],
                "workload_id": meta.get("workload_id", ""),
                "workload_type": meta.get("workload_type", ""),
                "pod_role": meta.get("pod_role", ""),
                "gang": meta.get("gang", False),
                "node": node,
                "hardware_family": family,
                "gpu_key": meta.get("gpu_key", ""),
                "elapsed_seconds": item["elapsed_seconds"],
            }
        )
    return rows


def collect_hardware_energy_rows(run_dir: pathlib.Path, summary_row: dict) -> list[dict]:
    energy_doc = load_json_file(run_dir / "sim_debug_energy.json")
    if not isinstance(energy_doc, dict):
        return []
    by_node = energy_doc.get("byNodeJoules", {})
    if not isinstance(by_node, dict):
        return []
    node_families = load_node_families(run_dir / "nodes.json")
    agg = {}
    for node, joules in by_node.items():
        fam = node_families.get(node, "unknown")
        agg[fam] = agg.get(fam, 0.0) + (to_float(joules) or 0.0)
    rows = []
    for fam, joules in sorted(agg.items()):
        rows.append(
            {
                "run_id": summary_row["run_id"],
                "baseline": summary_row["baseline"],
                "seed": summary_row["seed"],
                "hardware_family": fam,
                "energy_joules": joules,
                "energy_kwh_sim": joules / 3_600_000.0,
            }
        )
    return rows


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


def extract_event_counts(events_doc: dict):
    if not isinstance(events_doc, dict):
        return 0, 0
    events = events_doc.get("events")
    if not isinstance(events, list):
        return 0, 0
    telemetry_count = 0
    for e in events:
        if not isinstance(e, dict):
            continue
        if e.get("kind") == "telemetry":
            telemetry_count += 1
    return len(events), telemetry_count


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

    jobs_total, workloads_total = count_trace_records(run_dir / "trace.jsonl")

    sim_seconds = None
    throughput_wall = None
    throughput_sim = None
    throughput_sim_hour = None
    if wall_seconds and wall_seconds > 0:
        sim_seconds = wall_seconds * time_scale
        throughput_wall = jobs_total / wall_seconds
        throughput_sim = jobs_total / sim_seconds if sim_seconds > 0 else None
        throughput_sim_hour = throughput_sim * 3600 if throughput_sim is not None else None

    events_doc = load_json_file(run_dir / "sim_debug_events.json")
    sim_event_count, telemetry_event_count = extract_event_counts(events_doc)
    energy_sim_j = energy_from_debug_energy(load_json_file(run_dir / "sim_debug_energy.json"), time_scale)
    energy_source = "debug_energy"
    if energy_sim_j is None:
        energy_sim_j, sim_event_count, telemetry_event_count = integrate_energy_joules_from_events(
            events_doc, time_scale
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
        "workloads_total": workloads_total,
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
    job_rows = []
    hw_energy_rows = []
    for d in sorted(RESULTS.iterdir()):
        if not d.is_dir():
            continue
        if d.name in {"plots", "traces"}:
            continue
        row = collect_one_run(d)
        if row["baseline"]:
            rows.append(row)
            job_rows.extend(collect_job_rows(d, row))
            hw_energy_rows.extend(collect_hardware_energy_rows(d, row))

    if not rows:
        print("no runs found")
        return

    out = RESULTS / "summary.csv"
    df = pd.DataFrame(rows)
    df.sort_values(["baseline", "seed", "run_id"], inplace=True)
    df.to_csv(out, index=False)
    print(f"wrote {out}")

    all_run_ids = set(df["run_id"].tolist())

    if job_rows:
        jobs_out = RESULTS / "job_details.csv"
        jobs_df = pd.DataFrame(job_rows)
        jobs_df.sort_values(["baseline", "seed", "job_id"], inplace=True)
        jobs_df.to_csv(jobs_out, index=False)
        print(f"wrote {jobs_out}")

        a_jobs = jobs_df[jobs_df["baseline"] == "A"][["seed", "job_id", "elapsed_seconds"]].rename(
            columns={"elapsed_seconds": "elapsed_seconds_a"}
        )
        merged_jobs = jobs_df.merge(a_jobs, on=["seed", "job_id"], how="inner")
        merged_jobs = merged_jobs[merged_jobs["baseline"] != "A"].copy()
        if not merged_jobs.empty:
            merged_jobs["slowdown_pct_vs_a"] = 100.0 * (
                merged_jobs["elapsed_seconds"] / merged_jobs["elapsed_seconds_a"] - 1.0
            )
            merged_jobs["slowdown_pct_vs_a"] = pd.to_numeric(merged_jobs["slowdown_pct_vs_a"], errors="coerce")
            merged_jobs.loc[~merged_jobs["slowdown_pct_vs_a"].map(math.isfinite), "slowdown_pct_vs_a"] = pd.NA

            wt_out = RESULTS / "workload_type_relative_to_a.csv"
            wt_df = (
                merged_jobs.groupby(["baseline", "workload_type"], as_index=False)
                .agg(
                    jobs=("job_id", "count"),
                    mean_elapsed_seconds=("elapsed_seconds", "mean"),
                    mean_slowdown_pct_vs_a=("slowdown_pct_vs_a", "mean"),
                    p95_slowdown_pct_vs_a=("slowdown_pct_vs_a", lambda s: float(s.quantile(0.95))),
                )
            )
            wt_df.sort_values(["baseline", "workload_type"], inplace=True)
            wt_df.to_csv(wt_out, index=False)
            print(f"wrote {wt_out}")

    if hw_energy_rows:
        hw_energy_out = RESULTS / "hardware_energy.csv"
        hw_energy_df = pd.DataFrame(hw_energy_rows)
        hw_energy_df.sort_values(["baseline", "seed", "hardware_family"], inplace=True)
        hw_energy_df.to_csv(hw_energy_out, index=False)
        print(f"wrote {hw_energy_out}")

        if job_rows:
            jobs_df = pd.DataFrame(job_rows)
            a_jobs = jobs_df[jobs_df["baseline"] == "A"][["seed", "job_id", "elapsed_seconds"]].rename(
                columns={"elapsed_seconds": "elapsed_seconds_a"}
            )
            merged_jobs = jobs_df.merge(a_jobs, on=["seed", "job_id"], how="inner")
            merged_jobs = merged_jobs[merged_jobs["baseline"] != "A"].copy()
            if not merged_jobs.empty:
                merged_jobs["slowdown_pct_vs_a"] = 100.0 * (
                    merged_jobs["elapsed_seconds"] / merged_jobs["elapsed_seconds_a"] - 1.0
                )
                merged_jobs["slowdown_pct_vs_a"] = pd.to_numeric(merged_jobs["slowdown_pct_vs_a"], errors="coerce")
                merged_jobs.loc[~merged_jobs["slowdown_pct_vs_a"].map(math.isfinite), "slowdown_pct_vs_a"] = pd.NA
                hw_slowdown = (
                    merged_jobs.groupby(["baseline", "seed", "hardware_family"], as_index=False)
                    .agg(
                        jobs=("job_id", "count"),
                        mean_slowdown_pct_vs_a=("slowdown_pct_vs_a", "mean"),
                        p95_slowdown_pct_vs_a=("slowdown_pct_vs_a", lambda s: float(s.quantile(0.95))),
                    )
                )
                a_energy = hw_energy_df[hw_energy_df["baseline"] == "A"][
                    ["seed", "hardware_family", "energy_kwh_sim"]
                ].rename(columns={"energy_kwh_sim": "energy_kwh_sim_a"})
                merged_hw = hw_energy_df.merge(a_energy, on=["seed", "hardware_family"], how="inner")
                merged_hw = merged_hw[merged_hw["baseline"] != "A"].copy()
                if not merged_hw.empty:
                    merged_hw["energy_savings_pct_vs_a"] = 100.0 * (
                        1.0 - merged_hw["energy_kwh_sim"] / merged_hw["energy_kwh_sim_a"]
                    )
                    merged_hw = merged_hw.merge(hw_slowdown, on=["baseline", "seed", "hardware_family"], how="left")
                    hw_out = RESULTS / "hardware_family_relative_to_a.csv"
                    hw_group = (
                        merged_hw.groupby(["baseline", "hardware_family"], as_index=False)
                        .agg(
                            runs=("seed", "count"),
                            mean_energy_savings_pct_vs_a=("energy_savings_pct_vs_a", "mean"),
                            mean_slowdown_pct_vs_a=("mean_slowdown_pct_vs_a", "mean"),
                            p95_slowdown_pct_vs_a=("p95_slowdown_pct_vs_a", "mean"),
                        )
                    )
                    hw_group.sort_values(["baseline", "hardware_family"], inplace=True)
                    hw_group.to_csv(hw_out, index=False)
                    print(f"wrote {hw_out}")

                    if job_rows:
                        jobs_df = pd.DataFrame(job_rows)
                        a_jobs = jobs_df[jobs_df["baseline"] == "A"][["seed", "job_id", "elapsed_seconds"]].rename(
                            columns={"elapsed_seconds": "elapsed_seconds_a"}
                        )
                        merged_jobs = jobs_df.merge(a_jobs, on=["seed", "job_id"], how="inner")
                        merged_jobs = merged_jobs[merged_jobs["baseline"] != "A"].copy()
                        if not merged_jobs.empty:
                            merged_jobs["slowdown_pct_vs_a"] = 100.0 * (
                                merged_jobs["elapsed_seconds"] / merged_jobs["elapsed_seconds_a"] - 1.0
                            )
                            merged_jobs["slowdown_pct_vs_a"] = pd.to_numeric(merged_jobs["slowdown_pct_vs_a"], errors="coerce")
                            merged_jobs.loc[~merged_jobs["slowdown_pct_vs_a"].map(math.isfinite), "slowdown_pct_vs_a"] = pd.NA
                            merged_jobs = merged_jobs.merge(
                                merged_hw[
                                    [
                                        "baseline",
                                        "seed",
                                        "hardware_family",
                                        "energy_savings_pct_vs_a",
                                    ]
                                ],
                                on=["baseline", "seed", "hardware_family"],
                                how="left",
                            )
                            workload_tradeoff_out = RESULTS / "workload_type_tradeoff_vs_a.csv"
                            workload_tradeoff_df = (
                                merged_jobs.groupby(["baseline", "workload_type"], as_index=False)
                                .agg(
                                    jobs=("job_id", "count"),
                                    mean_slowdown_pct_vs_a=("slowdown_pct_vs_a", "mean"),
                                    p95_slowdown_pct_vs_a=("slowdown_pct_vs_a", lambda s: float(s.quantile(0.95))),
                                    mean_energy_savings_exposure_pct_vs_a=("energy_savings_pct_vs_a", "mean"),
                                )
                            )
                            workload_tradeoff_df["sample_quality"] = workload_tradeoff_df["jobs"].apply(
                                lambda n: "stable" if n >= MIN_WORKLOAD_TYPE_JOBS else "low"
                            )
                            workload_tradeoff_df["tradeoff_score"] = (
                                workload_tradeoff_df["mean_energy_savings_exposure_pct_vs_a"]
                                - workload_tradeoff_df["mean_slowdown_pct_vs_a"]
                            )
                            workload_tradeoff_df.sort_values(["baseline", "tradeoff_score"], ascending=[True, False], inplace=True)
                            workload_tradeoff_df.to_csv(workload_tradeoff_out, index=False)
                            print(f"wrote {workload_tradeoff_out}")

    numeric_metrics = [
        "wall_seconds",
        "throughput_jobs_per_sim_hour",
        "energy_sim_kwh_est",
        "avg_cluster_power_w_est",
    ]
    baseline_rows = []
    for baseline, grp in df.groupby("baseline"):
        row = {
            "baseline": baseline,
            "runs_total": len(grp),
        }
        for metric in numeric_metrics:
            series = pd.to_numeric(grp[metric], errors="coerce").dropna()
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
