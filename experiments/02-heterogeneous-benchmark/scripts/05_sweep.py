#!/usr/bin/env python3
import argparse
import datetime as dt
import heapq
import json
import os
import pathlib
import subprocess
import sys
import time
from collections import deque

import yaml

# Allow importing the shared trace generator from scripts/.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))
from scripts.trace_generator import generate_trace_python

DEFAULT_CONFIG = pathlib.Path("experiments/02-heterogeneous-benchmark/configs/benchmark.yaml")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", "experiments/02-heterogeneous-benchmark/runs/latest/results"))
START_TS = time.time()


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    elapsed = time.time() - START_TS
    print(f"[sweep {now} +{elapsed:8.1f}s] {msg}", flush=True)


def run(cmd, check=True, capture=False):
    if capture:
        return subprocess.run(cmd, check=check, text=True, capture_output=True)
    return subprocess.run(cmd, check=check)


def run_with_env(cmd, env: dict, check=True):
    return subprocess.run(cmd, check=check, env=env)


def load_config(path: pathlib.Path) -> dict:
    if not path.exists():
        raise SystemExit(f"config file not found: {path}")
    data = yaml.safe_load(path.read_text())
    if data is None:
        return {}
    if not isinstance(data, dict):
        raise SystemExit(f"invalid config format in {path}: top-level YAML must be a mapping")
    return data


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
        raise SystemExit(f"invalid baseline(s): {','.join(invalid)}; allowed values are A,B,C")
    if not vals:
        raise SystemExit("no baselines selected")
    return vals


def _quantity(v) -> float:
    """A Kubernetes CPU or GPU quantity as a number: "11.51", "500m", 2."""
    s = str(v or 0).strip()
    if s.endswith("m"):
        return float(s[:-1]) / 1000.0
    return float(s)


def shape_trace(trace_path: pathlib.Path, arrival_window_sec: float, prefill_sec: float, base_speed_per_core: float, gpu_util_scale: float = 1.0) -> dict:
    """Make a run start at regime and last arrival_window_sec. The trace is
    read as if it had already run for prefill_sec seconds before the run
    starts, with no queueing: a job submitted before then is submitted at
    once, less the work it would have done since its submit offset (per wall
    second, cores x base_speed_per_core CPU units and max(0.1, GPUs) x
    base_speed_per_core GPU units, simulator/pkg/workload/rates.go), or left
    out when it would have finished; later jobs keep their offsets, less
    prefill_sec; jobs submitted after prefill_sec + arrival_window_sec are
    left out. gpu_util_scale multiplies each job's
    workloadProfile.gpuUtilization, at most 1: the generator's GPU jobs use
    about half the GPUs they request. Returns how many jobs it prefilled and
    left out."""
    recs = [json.loads(raw) for raw in trace_path.read_text().splitlines() if raw.strip()]
    end = prefill_sec + arrival_window_sec if arrival_window_sec > 0 else float("inf")
    kept, prefilled, dropped = [], 0, 0
    for rec in recs:
        if rec.get("type", "job") != "job":
            continue
        off = float(rec.get("submitTimeOffsetSec", 0))
        if off > end:
            dropped += 1
            continue
        if off < prefill_sec and isinstance(rec.get("work"), dict):
            req = (rec.get("podTemplate") or {}).get("requests") or {}
            cores = _quantity(req.get("cpu"))
            gpus = _quantity(req.get("nvidia.com/gpu") or req.get("amd.com/gpu"))
            work, ran = rec["work"], prefill_sec - off
            cpu = max(0.0, float(work.get("cpuUnits", 0) or 0) - ran * base_speed_per_core * cores)
            gpu = max(0.0, float(work.get("gpuUnits", 0) or 0) - ran * base_speed_per_core * max(0.1, gpus))
            if cpu <= 0 and gpu <= 0:
                dropped += 1
                continue
            work["cpuUnits"], work["gpuUnits"] = cpu, gpu
            prefilled += 1
        rec["submitTimeOffsetSec"] = max(0.0, off - prefill_sec)
        prof = rec.get("workloadProfile")
        if gpu_util_scale != 1.0 and isinstance(prof, dict) and "gpuUtilization" in prof:
            prof["gpuUtilization"] = min(1.0, float(prof["gpuUtilization"]) * gpu_util_scale)
        kept.append(rec)
    ids = {r.get("workloadId") for r in kept}
    keep = {id(r) for r in kept}
    out = []
    for rec in recs:
        if rec.get("type", "job") != "job":
            if rec.get("workloadId") in ids:
                rec["submitTimeOffsetSec"] = max(0.0, float(rec.get("submitTimeOffsetSec", 0)) - prefill_sec)
                out.append(rec)
        elif id(rec) in keep:
            out.append(rec)
    trace_path.write_text("".join(json.dumps(r, separators=(",", ":")) + "\n" for r in out))
    return {"prefilled_jobs": prefilled, "dropped_jobs": dropped}


def generate_canonical_seed_trace(
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
    time_scale: float = 1.0,
    arrival_window_sec: float = 0.0,
    prefill_sec: float = 0.0,
    base_speed_per_core: float = 1.0,
    gpu_util_scale: float = 1.0,
) -> pathlib.Path:
    traces_dir = RESULTS / "traces"
    traces_dir.mkdir(parents=True, exist_ok=True)
    ratio_id = f"perf_{str(perf_ratio).replace('.', '_')}_cbpb_{str(compute_bound_perf_boost).replace('.', '_')}"
    workload_id = (
        f"gpur_{str(gpu_ratio).replace('.', '_')}"
        f"_burstp_{str(burst_day_probability).replace('.', '_')}"
        f"_burstm_{str(burst_multiplier).replace('.', '_')}"
        f"_burstjobs_{str(burst_mean_jobs).replace('.', '_')}"
        f"_meta_{'on' if emit_workload_records else 'off'}"
        f"_wscale_{str(work_scale).replace('.', '_')}"
        f"_win_{str(arrival_window_sec).replace('.', '_')}_prefill_{str(prefill_sec).replace('.', '_')}_gpuutil_{str(gpu_util_scale).replace('.', '_')}"
    )
    trace_name = (
        f"seed_{seed}_jobs_{jobs}_mia_{str(mean_inter_arrival_sec).replace('.', '_')}_"
        f"{ratio_id}_{workload_id}_canonical.jsonl"
    )
    trace_path = traces_dir / trace_name
    if trace_path.exists():
        log(f"reusing canonical seed trace seed={seed} file={trace_path}")
        return trace_path
    log(
        f"generating canonical seed trace seed={seed} jobs={jobs} "
        f"mean_inter_arrival_sec={mean_inter_arrival_sec}"
    )
    cmd = [
        "go",
        "run",
        "./simulator/cmd/workloadgen",
        "--schema-version",
        "v2",
        "--jobs",
        str(jobs),
        "--seed",
        str(seed),
        "--out",
        str(trace_path),
        "--mean-inter-arrival-sec",
        str(mean_inter_arrival_sec),
    ]
    cmd.extend(
        [
            "--perf-ratio",
            str(perf_ratio),
            "--compute-bound-perf-boost",
            str(compute_bound_perf_boost),
            "--gpu-ratio",
            str(gpu_ratio),
            "--burst-day-probability",
            str(burst_day_probability),
            "--burst-mean-jobs",
            str(burst_mean_jobs),
            "--burst-multiplier",
            str(burst_multiplier),
            "--dip-day-probability",
            str(dip_day_probability),
            "--dip-multiplier",
            str(dip_multiplier),
            "--emit-workload-records",
            str(emit_workload_records).lower(),
            "--time-scale",
            str(time_scale),
        ]
    )
    run(cmd, check=True)
    if work_scale != 1.0 or allowed_workload_types:
        allowed = set(allowed_workload_types or [])
        filtered_scaled_lines = []
        for raw in trace_path.read_text().splitlines():
            raw = raw.strip()
            if not raw:
                continue
            rec = json.loads(raw)
            if allowed and rec.get("workloadType") not in allowed:
                continue
            if rec.get("type", "job") == "job":
                work = rec.get("work")
                if isinstance(work, dict):
                    if "cpuUnits" in work:
                        work["cpuUnits"] = float(work["cpuUnits"]) * work_scale
                    if "gpuUnits" in work:
                        work["gpuUnits"] = float(work["gpuUnits"]) * work_scale
            filtered_scaled_lines.append(json.dumps(rec, separators=(",", ":")))
        trace_path.write_text("\n".join(filtered_scaled_lines) + ("\n" if filtered_scaled_lines else ""))
    if arrival_window_sec > 0 or prefill_sec > 0 or gpu_util_scale != 1.0:
        shaped = shape_trace(trace_path, arrival_window_sec, prefill_sec, base_speed_per_core, gpu_util_scale)
        log(f"trace shaped arrival_window_sec={arrival_window_sec} prefill_sec={prefill_sec} prefilled_jobs={shaped['prefilled_jobs']} dropped_jobs={shaped['dropped_jobs']}")
    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"canonical seed trace generated records={count} file={trace_path}")
    return trace_path


def load_kwok_nodes() -> list[dict]:
    out = run(["kubectl", "get", "nodes", "-l", "type=kwok", "-o", "json"], capture=True)
    items = json.loads(out.stdout).get("items", [])
    nodes = []
    for item in items:
        meta = item.get("metadata", {})
        labels = meta.get("labels", {}) or {}
        alloc = ((item.get("status", {}) or {}).get("allocatable", {}) or {})
        name = meta.get("name", "")
        gpu_product = labels.get("joulie.io/gpu.product", "")
        gpu_count = int(labels.get("joulie.io/hw.gpu-count", alloc.get("nvidia.com/gpu", alloc.get("amd.com/gpu", "0"))) or 0)
        vendor = "none"
        if "nvidia.com/gpu" in alloc or labels.get("feature.node.kubernetes.io/pci-10de.present") == "true":
            vendor = "nvidia"
        elif "amd.com/gpu" in alloc or labels.get("feature.node.kubernetes.io/pci-1002.present") == "true":
            vendor = "amd"
        nodes.append(
            {
                "name": name,
                "vendor": vendor,
                "gpu_product": gpu_product,
                "gpu_count": gpu_count,
                "cpu_model": labels.get("joulie.io/hw.cpu-model", ""),
                "cpu_only": gpu_count <= 0,
            }
        )
    return nodes


def rotate_pick(pool: deque[dict]) -> dict:
    item = pool[0]
    pool.rotate(-1)
    return item


def job_seconds(req: dict, work: dict, gpus: float, base_speed_per_core: float) -> float:
    """How long a job runs without queueing or caps: it advances
    cores x base_speed_per_core CPU units and max(0.1, GPUs) x
    base_speed_per_core GPU units per second and ends when both are done
    (simulator/pkg/workload/rates.go)."""
    cores = _quantity(req.get("cpu"))
    cpu = float(work.get("cpuUnits", 0) or 0) / (base_speed_per_core * cores) if cores > 0 else 0.0
    gpu = float(work.get("gpuUnits", 0) or 0) / (base_speed_per_core * max(0.1, gpus))
    return max(cpu, gpu)


def pick_vendor(pools: dict, at: float, gpus: float, seconds: float) -> str:
    """The GPU vendor pool for a job submitted at at: among the pools with a
    node that can host its GPUs, the one whose requested GPUs, this job's
    included, are the lowest share of its GPUs. Jobs go where GPUs are free,
    as users of a cluster with several vendors do. A pick in proportion to
    each pool's GPUs ignored how long jobs run, and in the small AMD pool
    (40 of 188 GPUs) a few long jobs put the load above capacity at peaks
    while NVIDIA had room. Jobs must come in submit order."""
    best = None
    for vendor in sorted(pools):
        p = pools[vendor]
        if p["largest"] < gpus:
            continue
        while p["active"] and p["active"][0][0] <= at:
            p["load"] -= heapq.heappop(p["active"])[1]
        share = (p["load"] + gpus) / p["gpus"]
        if best is None or share < best[0]:
            best = (share, vendor)
    if best is None:
        raise SystemExit(f"no GPU node can host {gpus} GPUs")
    p = pools[best[1]]
    p["load"] += gpus
    heapq.heappush(p["active"], (at + seconds, gpus))
    return best[1]


def build_family_first_pool(nodes: list[dict], family_key: str) -> deque[dict]:
    by_family: dict[str, list[dict]] = {}
    for node in nodes:
        family = str(node.get(family_key) or "")
        by_family.setdefault(family, []).append(node)
    ordered: list[dict] = []
    seen_names: set[str] = set()
    for family in sorted(by_family):
        first = by_family[family][0]
        ordered.append(first)
        seen_names.add(first["name"])
    for node in nodes:
        if node["name"] in seen_names:
            continue
        ordered.append(node)
    return deque(ordered)


def ensure_required_term(affinity: dict | None) -> dict:
    if not isinstance(affinity, dict):
        affinity = {}
    node_affinity = affinity.setdefault("nodeAffinity", {})
    required = node_affinity.setdefault("requiredDuringSchedulingIgnoredDuringExecution", {})
    terms = required.setdefault("nodeSelectorTerms", [])
    if not isinstance(terms, list) or not terms:
        terms = [{"matchExpressions": []}]
        required["nodeSelectorTerms"] = terms
    return affinity


def add_required_expr(affinity: dict | None, expr: dict) -> dict:
    affinity = ensure_required_term(affinity)
    terms = affinity["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
    for term in terms:
        exprs = term.setdefault("matchExpressions", [])
        if not any(e.get("key") == expr.get("key") for e in exprs if isinstance(e, dict)):
            exprs.append(expr)
    return affinity


def strip_power_profile_affinity(affinity: dict | None) -> dict | None:
    if not isinstance(affinity, dict):
        return affinity
    node_aff = affinity.get("nodeAffinity")
    if not isinstance(node_aff, dict):
        return affinity
    required = node_aff.get("requiredDuringSchedulingIgnoredDuringExecution")
    if not isinstance(required, dict):
        return affinity
    terms = required.get("nodeSelectorTerms")
    if not isinstance(terms, list):
        return affinity
    keep_terms = []
    for term in terms:
        if not isinstance(term, dict):
            continue
        exprs = term.get("matchExpressions", [])
        if not isinstance(exprs, list):
            continue
        kept_exprs = [
            e
            for e in exprs
            if isinstance(e, dict) and e.get("key") not in {"joulie.io/power-profile"}
        ]
        if kept_exprs:
            keep_terms.append({"matchExpressions": kept_exprs})
    if keep_terms:
        required["nodeSelectorTerms"] = keep_terms
        return affinity
    node_aff.pop("requiredDuringSchedulingIgnoredDuringExecution", None)
    if not node_aff:
        affinity.pop("nodeAffinity", None)
    return affinity or None


def retarget_trace_for_cluster(trace_path: pathlib.Path, base_speed_per_core: float = 1.0) -> pathlib.Path:
    nodes = load_kwok_nodes()
    gpu_nodes = [n for n in nodes if not n["cpu_only"]]
    cpu_nodes = build_family_first_pool([n for n in nodes if n["cpu_only"]], "cpu_model")
    if not gpu_nodes or not cpu_nodes:
        raise SystemExit("expected both GPU and CPU-only KWOK nodes to exist before generating the benchmark trace")

    out_path = trace_path.with_name(trace_path.stem + "_targeted.jsonl")
    if out_path.exists():
        return out_path

    # Build GPU pools used only to choose a vendor/resource class proportionally to the actual
    # fleet composition. Kubernetes still does the real node-level placement.
    #
    # We intentionally do not add node-name affinity here. The benchmark should exercise the
    # scheduler against the allocatable extended GPU resources already advertised by the KWOK
    # nodes, not a pre-computed node assignment from the harness.
    pools: dict[str, dict] = {}
    for n in gpu_nodes:
        p = pools.setdefault("amd" if n["vendor"] == "amd" else "nvidia", {"gpus": 0, "largest": 0, "active": [], "load": 0.0})
        p["gpus"] += n["gpu_count"]
        p["largest"] = max(p["largest"], n["gpu_count"])
    records = [json.loads(raw) for raw in (r.strip() for r in trace_path.read_text().splitlines()) if raw]
    vendor_of: dict[int, str] = {}
    for k in sorted(range(len(records)), key=lambda k: float(records[k].get("submitTimeOffsetSec") or 0)):
        rec = records[k]
        req = (rec.get("podTemplate") or {}).get("requests") if rec.get("type", "job") == "job" else None
        key = next((x for x in ("nvidia.com/gpu", "amd.com/gpu", "gpu") if isinstance(req, dict) and x in req), None)
        if key is not None:
            gpus = _quantity(req[key])
            vendor_of[k] = pick_vendor(pools, float(rec.get("submitTimeOffsetSec") or 0), gpus,
                                       job_seconds(req, rec.get("work") or {}, gpus, base_speed_per_core))

    total_job_count = 0
    gpu_job_count = 0
    cpu_job_count = 0
    lines = []
    for k, rec in enumerate(records):
        if rec.get("type", "job") != "job":
            lines.append(json.dumps(rec, separators=(",", ":")))
            continue

        total_job_count += 1
        pod_tpl = rec.get("podTemplate")
        if not isinstance(pod_tpl, dict):
            lines.append(json.dumps(rec, separators=(",", ":")))
            continue
        req = pod_tpl.get("requests")
        if not isinstance(req, dict):
            lines.append(json.dumps(rec, separators=(",", ":")))
            continue

        gpu_qty = None
        gpu_key = None
        for key in ("nvidia.com/gpu", "amd.com/gpu", "gpu"):
            if key in req:
                gpu_key = key
                gpu_qty = req[key]
                break

        affinity = pod_tpl.get("affinity")
        # Keep only broad pool compatibility constraints so Kubernetes does the actual placement.
        # The generated trace already carries perf/eco intent via power-profile affinity; we add
        # only the minimum pool compatibility needed for realistic scheduling on this synthetic
        # fleet (CPU-only jobs on CPU-only nodes; GPU jobs on a chosen GPU vendor resource).
        if gpu_key is None:
            cpu_job_count += 1
            pod_tpl["affinity"] = add_required_expr(
                affinity,
                {"key": "joulie.io/hw.kind", "operator": "In", "values": ["cpu-only"]},
            )
        else:
            gpu_job_count += 1
            req.pop(gpu_key, None)
            if vendor_of[k] == "amd":
                req["amd.com/gpu"] = gpu_qty
            else:
                req["nvidia.com/gpu"] = gpu_qty
            pod_tpl["affinity"] = affinity

        lines.append(json.dumps(rec, separators=(",", ":")))

    out_path.write_text("\n".join(lines) + ("\n" if lines else ""))
    log(
        "retargeted canonical trace "
        f"jobs_total={total_job_count} gpu_jobs={gpu_job_count} cpu_jobs={cpu_job_count} "
        f"gpu_nodes={len(gpu_nodes)} cpu_nodes={len(cpu_nodes)}"
    )
    if gpu_job_count < len(gpu_nodes):
        log(
            "warning: GPU job count is lower than GPU node count; "
            "some GPU nodes will not receive work in this run"
        )
    if cpu_job_count < len(cpu_nodes):
        log(
            "warning: CPU-only job count is lower than CPU-only node count; "
            "some CPU-only nodes will not receive work in this run"
        )
    return out_path


def derive_baseline_trace(baseline: str, canonical_trace: pathlib.Path, strip_affinity_for_a: bool) -> pathlib.Path:
    traces_dir = canonical_trace.parent
    stem = canonical_trace.stem
    if stem.endswith("_targeted"):
        stem = stem[:-len("_targeted")]
    out_name = f"{stem}_baseline_{baseline}.jsonl"
    out_path = traces_dir / out_name
    if out_path.exists():
        return out_path
    if baseline != "A" or not strip_affinity_for_a:
        out_path.write_text(canonical_trace.read_text())
        return out_path

    # Baseline A: same exact jobs/timing/work, only remove power-profile affinity constraints.
    lines = []
    for raw in canonical_trace.read_text().splitlines():
        raw = raw.strip()
        if not raw:
            continue
        rec = json.loads(raw)
        pod_tpl = rec.get("podTemplate")
        if isinstance(pod_tpl, dict):
            pod_tpl["affinity"] = strip_power_profile_affinity(pod_tpl.get("affinity"))
            if not pod_tpl.get("affinity"):
                pod_tpl.pop("affinity", None)
        lines.append(json.dumps(rec, separators=(",", ":")))
    out_path.write_text("\n".join(lines) + ("\n" if lines else ""))
    return out_path


def cleanup_workload_pods():
    log("cleaning previous workload pods")
    run(
        [
            "kubectl",
            "delete",
            "pods",
            "-A",
            "-l",
            "app.kubernetes.io/part-of=joulie-sim-workload",
            "--ignore-not-found",
            "--wait=true",
            "--timeout=180s",
        ],
        check=False,
    )


def run_schedule(baselines: list[str], seeds: int, repeat_first: bool = False) -> list[tuple[int, str]]:
    """The sweep's runs as (seed, baseline) pairs: every baseline once per
    seed. An odd seed rotates the order by one place per pair of seeds and
    the even seed after it reverses that order, so over any two seeds each
    baseline holds the same mean position. Running all of A, then all of B,
    then all of C would confound the baseline with whatever else loads the
    host meanwhile; the simulator advances jobs by wall-clock time, so host
    load reaches the measurement. With repeat_first the first run is run
    again at the end, to measure run-to-run noise."""
    out = []
    for seed in range(1, seeds + 1):
        k = ((seed - 1) // 2) % len(baselines)
        order = baselines[k:] + baselines[:k]
        if seed % 2 == 0:
            order = order[::-1]
        out += [(seed, b) for b in order]
    return out + out[:1] if repeat_first else out


def reset_control_state():
    log("resetting control state (profiles + node power labels)")
    resources = {"nodepowerprofiles": "nodepowerprofiles.joulie.io", "telemetryprofiles": "telemetryprofiles.joulie.io"}
    available = set(
        line.strip()
        for line in run(["kubectl", "api-resources", "-o", "name"], capture=True, check=False).stdout.splitlines()
        if line.strip()
    )
    for short, full in resources.items():
        if short in available or full in available:
            run(["kubectl", "delete", short, "--all", "--ignore-not-found"], check=False)
    run(
        [
            "kubectl",
            "label",
            "nodes",
            "-l",
            "joulie.io/managed=true",
            "joulie.io/power-profile-",
        ],
        check=False,
    )
    run(
        [
            "kubectl",
            "label",
            "nodes",
            "-l",
            "joulie.io/managed=true",
            "joulie.io/draining-",
        ],
        check=False,
    )


def wait_zero_active_workload_pods(timeout_sec: int):
    log(f"waiting for zero active workload pods timeout_sec={timeout_sec}")
    start = time.time()
    while time.time() - start < timeout_sec:
        out = run(
            [
                "kubectl",
                "get",
                "pods",
                "-A",
                "-l",
                "app.kubernetes.io/part-of=joulie-sim-workload",
                "-o",
                "json",
            ],
            capture=True,
        )
        items = json.loads(out.stdout).get("items", [])
        active = sum(
            1
            for p in items
            if not str(((p.get("metadata", {}) or {}).get("name", ""))).startswith("sim-bootstrap-")
            if p.get("status", {}).get("phase") in ("Pending", "Running")
        )
        if active == 0:
            log("active workload pods: 0")
            return True
        log(f"active workload pods: {active}")
        time.sleep(1)
    log("timeout waiting for zero active workload pods")
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default=str(DEFAULT_CONFIG), help="Path to benchmark YAML config")
    ap.add_argument("--seeds", type=int, default=None)
    ap.add_argument("--jobs", type=int, default=None)
    ap.add_argument("--mean-inter-arrival-sec", type=float, default=None)
    ap.add_argument("--timeout", type=int, default=None)
    ap.add_argument("--settle-seconds", type=int, default=None)
    ap.add_argument("--cleanup-timeout", type=int, default=None)
    ap.add_argument("--perf-ratio", type=float, default=None)
    ap.add_argument("--compute-bound-perf-boost", type=float, default=None)
    ap.add_argument("--gpu-ratio", type=float, default=None)
    ap.add_argument("--burst-day-probability", type=float, default=None)
    ap.add_argument("--burst-mean-jobs", type=float, default=None)
    ap.add_argument("--burst-multiplier", type=float, default=None)
    ap.add_argument("--emit-workload-records", type=str, default="")
    ap.add_argument("--baselines", type=str, default="")
    ap.add_argument("--trace-mode", choices=["go", "python"], default="go",
                    help="Trace generation mode: 'go' (workloadgen) or 'python' (noisy NHPP)")
    args = ap.parse_args()

    cfg_path = pathlib.Path(args.config)
    cfg = load_config(cfg_path)

    seeds = args.seeds if args.seeds is not None else int(get_cfg(cfg, "run", "seeds", default=1))
    jobs = args.jobs if args.jobs is not None else int(get_cfg(cfg, "run", "jobs", default=20))
    mean_inter_arrival_sec = (
        args.mean_inter_arrival_sec
        if args.mean_inter_arrival_sec is not None
        else float(get_cfg(cfg, "run", "mean_inter_arrival_sec", default=0.05))
    )
    timeout = args.timeout if args.timeout is not None else int(get_cfg(cfg, "run", "timeout", default=240))
    settle_seconds = (
        args.settle_seconds if args.settle_seconds is not None else int(get_cfg(cfg, "run", "settle_seconds", default=4))
    )
    cleanup_timeout = (
        args.cleanup_timeout
        if args.cleanup_timeout is not None
        else int(get_cfg(cfg, "run", "cleanup_timeout", default=45))
    )
    time_scale = float(get_cfg(cfg, "run", "time_scale", default=60.0))
    perf_ratio = (
        args.perf_ratio if args.perf_ratio is not None else float(get_cfg(cfg, "workload", "perf_ratio", default=0.30))
    )
    compute_bound_perf_boost = (
        args.compute_bound_perf_boost if args.compute_bound_perf_boost is not None else float(get_cfg(cfg, "workload", "compute_bound_perf_boost", default=3.5))
    )
    gpu_ratio = (
        args.gpu_ratio if args.gpu_ratio is not None else float(get_cfg(cfg, "workload", "gpu_ratio", default=0.00))
    )
    burst_day_probability = (
        args.burst_day_probability
        if args.burst_day_probability is not None
        else float(get_cfg(cfg, "workload", "burst_day_probability", default=0.25))
    )
    burst_mean_jobs = (
        args.burst_mean_jobs
        if args.burst_mean_jobs is not None
        else float(get_cfg(cfg, "workload", "burst_mean_jobs", default=8.0))
    )
    burst_multiplier = (
        args.burst_multiplier
        if args.burst_multiplier is not None
        else float(get_cfg(cfg, "workload", "burst_multiplier", default=2.0))
    )
    dip_day_probability = float(get_cfg(cfg, "workload", "dip_day_probability", default=0.30))
    dip_multiplier = float(get_cfg(cfg, "workload", "dip_multiplier", default=0.08))
    emit_workload_records_raw = (
        args.emit_workload_records
        if args.emit_workload_records.strip()
        else get_cfg(cfg, "workload", "emit_workload_records", default=True)
    )
    emit_workload_records = str(emit_workload_records_raw).strip().lower() not in {"false", "0", "no"}
    work_scale = float(get_cfg(cfg, "workload", "work_scale", default=1.0))
    arrival_window_sec = float(get_cfg(cfg, "workload", "arrival_window_sec", default=0.0))
    prefill_sec = float(get_cfg(cfg, "workload", "prefill_sec", default=0.0))
    trace_base_speed = float(get_cfg(cfg, "simulator", "base_speed_per_core", default=1.0))
    gpu_util_scale = float(get_cfg(cfg, "workload", "gpu_util_scale", default=1.0))
    diurnal_peak_rate = float(get_cfg(cfg, "workload", "diurnal_peak_rate", default=2.0))
    trace_mode = args.trace_mode
    baseline_a_strip_affinity = bool(get_cfg(cfg, "workload", "baseline_a_strip_affinity", default=True))
    allowed_workload_types = get_cfg(cfg, "workload", "allowed_workload_types", default=None)
    if allowed_workload_types is not None and not isinstance(allowed_workload_types, list):
        raise SystemExit("workload.allowed_workload_types must be a YAML list when set")

    baselines_raw = args.baselines if args.baselines.strip() else get_cfg(cfg, "run", "baselines", default=["A", "B", "C"])
    baselines = to_baselines(baselines_raw)

    if perf_ratio < 0:
        raise SystemExit("perf_ratio must be >= 0")
    if compute_bound_perf_boost < 1:
        raise SystemExit("compute_bound_perf_boost must be >= 1")
    if gpu_ratio < 0 or gpu_ratio > 1:
        raise SystemExit("gpu_ratio must be in [0,1]")
    if burst_day_probability < 0 or burst_day_probability > 1:
        raise SystemExit("burst_day_probability must be in [0,1]")
    if burst_mean_jobs < 0:
        raise SystemExit("burst_mean_jobs must be >= 0")
    if burst_multiplier < 1:
        raise SystemExit("burst_multiplier must be >= 1")
    if work_scale <= 0:
        raise SystemExit("work_scale must be > 0")

    total_runs = len(baselines) * seeds + bool(get_cfg(cfg, "run", "repeat_first_run", default=False))
    done = 0
    log(
        f"starting sweep config={cfg_path} baselines={','.join(baselines)} seeds={seeds} jobs={jobs} "
        f"mean_inter_arrival_sec={mean_inter_arrival_sec} timeout={timeout}s time_scale={time_scale} total_runs={total_runs}"
    )

    install_env_base = os.environ.copy()
    inventory_source = str(get_cfg(cfg, "inventory", "source", default="experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml"))
    run(["bash", "experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh", inventory_source], check=True)

    # Extract cluster name from Kind config so install scripts use the correct worker node name.
    kind_cfg_path = pathlib.Path(get_cfg(cfg, "install", "kind_cluster_config", default="experiments/02-heterogeneous-benchmark/configs/kind-cluster.yaml"))
    if kind_cfg_path.exists():
        kind_cfg = yaml.safe_load(kind_cfg_path.read_text()) or {}
        install_env_base["CLUSTER_NAME"] = kind_cfg.get("name", "joulie-heterogeneous")

    # Image and manifest config
    install_env_base["JOULIE_REGISTRY"] = str(get_cfg(cfg, "images", "joulie_registry", default="registry.cern.ch/joulie"))
    install_env_base["JOULIE_TAG"] = str(get_cfg(cfg, "images", "joulie_tag", default="latest"))
    install_env_base["SIM_REGISTRY"] = str(get_cfg(cfg, "images", "sim_registry", default="registry.cern.ch/joulie"))
    install_env_base["SIM_IMAGE"] = str(get_cfg(cfg, "images", "sim_image", default="joulie-simulator"))
    install_env_base["SIM_TAG"] = str(get_cfg(cfg, "images", "sim_tag", default=""))

    simulator_manifest = get_cfg(cfg, "simulator", "manifest", default="") or get_cfg(cfg, "install", "simulator_manifest", default="")
    if simulator_manifest:
        install_env_base["SIMULATOR_MANIFEST"] = str(simulator_manifest)

    # Policy config
    install_env_base["STATIC_HP_FRAC"] = str(get_cfg(cfg, "policy", "static", "hp_frac", default=0.50))
    install_env_base["QUEUE_HP_BASE_FRAC"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_base_frac", default=0.60))
    install_env_base["QUEUE_HP_MIN"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_min", default=1))
    install_env_base["QUEUE_HP_MAX"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_max", default=5))
    install_env_base["QUEUE_PERF_PER_HP_NODE"] = str(get_cfg(cfg, "policy", "queue_aware", "perf_per_hp_node", default=10))
    install_env_base["PERFORMANCE_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "performance_watts", default=5000))
    install_env_base["ECO_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "eco_watts", default=140))
    install_env_base["CPU_PERFORMANCE_CAP_PCT_OF_MAX"] = str(
        get_cfg(cfg, "policy", "caps", "cpu_performance_pct_of_max", default=100)
    )
    install_env_base["CPU_ECO_CAP_PCT_OF_MAX"] = str(
        get_cfg(cfg, "policy", "caps", "cpu_eco_pct_of_max", default=60)
    )
    install_env_base["CPU_WRITE_ABSOLUTE_CAPS"] = str(get_cfg(cfg, "policy", "cpu_write_absolute_caps", default=True)).lower()
    install_env_base["GPU_PERFORMANCE_CAP_PCT_OF_MAX"] = str(get_cfg(cfg, "policy", "caps", "gpu_performance_pct_of_max", default=100))
    install_env_base["GPU_ECO_CAP_PCT_OF_MAX"] = str(get_cfg(cfg, "policy", "caps", "gpu_eco_pct_of_max", default=60))
    install_env_base["GPU_WRITE_ABSOLUTE_CAPS"] = str(get_cfg(cfg, "policy", "gpu_write_absolute_caps", default=True)).lower()
    install_env_base["OPERATOR_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "operator_reconcile_interval", default="20s"))
    install_env_base["AGENT_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "agent_reconcile_interval", default="10s"))
    install_env_base["SIM_BASE_SPEED_PER_CORE"] = str(get_cfg(cfg, "simulator", "base_speed_per_core", default=1.0))
    install_env_base["SIM_FACILITY_AMBIENT_BASE_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_base_c", default=22.0))
    install_env_base["SIM_FACILITY_AMBIENT_AMPLITUDE_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_amplitude_c", default=8.0))
    install_env_base["SIM_FACILITY_AMBIENT_PERIOD_SEC"] = str(get_cfg(cfg, "simulator", "facility_ambient_period_sec", default=600))
    install_env_base["ENABLE_FACILITY_METRICS"] = str(get_cfg(cfg, "operator", "enable_facility_metrics", default=False)).lower()
    log(
        "configured images "
        f"sim={install_env_base['SIM_REGISTRY']}/{install_env_base['SIM_IMAGE']}"
        + (f":{install_env_base['SIM_TAG']}" if install_env_base["SIM_TAG"] else " (manifest-tag)")
        + f" controller-manager={install_env_base['JOULIE_REGISTRY']}/joulie-controller-manager:{install_env_base['JOULIE_TAG']}"
        + f" agent={install_env_base['JOULIE_REGISTRY']}/joulie-agent:{install_env_base['JOULIE_TAG']}"
    )
    log(
        "configured policy "
        f"caps(cpu_perf={install_env_base['CPU_PERFORMANCE_CAP_PCT_OF_MAX']}% "
        f"cpu_eco={install_env_base['CPU_ECO_CAP_PCT_OF_MAX']}% "
        f"gpu_perf={install_env_base['GPU_PERFORMANCE_CAP_PCT_OF_MAX']}% "
        f"gpu_eco={install_env_base['GPU_ECO_CAP_PCT_OF_MAX']}% "
        f"cpu_abs={install_env_base['CPU_WRITE_ABSOLUTE_CAPS']} "
        f"gpu_abs={install_env_base['GPU_WRITE_ABSOLUTE_CAPS']}) "
        f"static.hp_frac={install_env_base['STATIC_HP_FRAC']} "
        f"queue(base={install_env_base['QUEUE_HP_BASE_FRAC']} min={install_env_base['QUEUE_HP_MIN']} "
        f"max={install_env_base['QUEUE_HP_MAX']} perf_per_hp={install_env_base['QUEUE_PERF_PER_HP_NODE']}) "
        f"loops(op={install_env_base['OPERATOR_RECONCILE_INTERVAL']} agent={install_env_base['AGENT_RECONCILE_INTERVAL']})"
    )

    baseline_policy = {
        "B": "static_partition",
        "C": "queue_aware_v1",
    }

    reset_control_state()

    schedule = run_schedule(baselines, seeds, bool(get_cfg(cfg, "run", "repeat_first_run", default=False)))
    log("run order: " + " ".join(f"{b}{s}" for s, b in schedule))
    for run_seed, baseline in schedule:
        reset_control_state()
        log(f"installing components for baseline={baseline}")
        install_env = install_env_base.copy()
        if baseline in baseline_policy:
            install_env["POLICY_TYPE"] = baseline_policy[baseline]
        else:
            install_env.pop("POLICY_TYPE", None)
        # For queue-aware (baseline C), compute the controller manager reconcile interval
        # from simulated seconds divided by time_scale.
        if baseline == "C":
            qa_sim_sec = float(get_cfg(cfg, "policy", "loop", "queue_aware_operator_reconcile_sim_seconds", default=300))
            qa_wall_sec = max(1, int(qa_sim_sec / time_scale))
            install_env["OPERATOR_RECONCILE_INTERVAL"] = f"{qa_wall_sec}s"
            log(f"baseline C: queue-aware controller manager reconcile = {qa_sim_sec}s simulated / {time_scale} = {qa_wall_sec}s wall")
        run_with_env(
            [
                "bash",
                "experiments/02-heterogeneous-benchmark/scripts/03_install_components.sh",
                baseline,
            ],
            env=install_env,
            check=True,
        )
        cleanup_workload_pods()
        if not wait_zero_active_workload_pods(cleanup_timeout):
            raise SystemExit(f"workload pods left after {cleanup_timeout} s of cleanup: the next run would start on them")
        log(f"policy settle sleep seconds={settle_seconds}")
        time.sleep(settle_seconds)

        for seed in (run_seed,):  # one run per (seed, baseline) pair of the schedule
            done += 1
            log(f"run {done}/{total_runs}: baseline={baseline} seed={seed}")
            if trace_mode == "python":
                sim_hours = timeout * time_scale / 3600.0
                base_speed = float(get_cfg(cfg, "simulator", "base_speed_per_core", default=2.0))
                inv_path = pathlib.Path(get_cfg(cfg, "inventory", "source", default=""))
                n_nodes = 41
                if inv_path.exists():
                    inv = yaml.safe_load(inv_path.read_text()) or {}
                    n_nodes = sum(n.get("replicas", 1) for n in inv.get("nodes", []))
                burst_scale = max(0.001, n_nodes / 5000.0)
                canonical_trace = generate_trace_python(
                    traces_dir=RESULTS / "traces",
                    seed=seed,
                    sim_hours=sim_hours,
                    gpu_ratio=gpu_ratio,
                    work_scale=work_scale,
                    time_scale=time_scale,
                    base_speed_per_core=base_speed,
                    diurnal_peak_rate=diurnal_peak_rate,
                    perf_ratio=perf_ratio,
                    burst_scale=burst_scale,
                    log_fn=log,
                )
            else:
                canonical_trace = generate_canonical_seed_trace(
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
                    arrival_window_sec=arrival_window_sec,
                    prefill_sec=prefill_sec,
                    base_speed_per_core=trace_base_speed,
                    gpu_util_scale=gpu_util_scale,
                )
            canonical_trace = retarget_trace_for_cluster(canonical_trace, trace_base_speed)
            trace_file = derive_baseline_trace(
                baseline=baseline,
                canonical_trace=canonical_trace,
                strip_affinity_for_a=baseline_a_strip_affinity,
            )
            log(f"using baseline trace file={trace_file}")
            cleanup_workload_pods()
            if not wait_zero_active_workload_pods(cleanup_timeout):
                raise SystemExit(f"workload pods left after {cleanup_timeout} s of cleanup: the next run would start on them")
            log(f"pre-run settle sleep seconds={settle_seconds}")
            time.sleep(settle_seconds)
            run(
                [
                    "python3",
                    "experiments/02-heterogeneous-benchmark/scripts/04_run_one.py",
                    "--baseline",
                    baseline,
                    "--seed",
                    str(seed),
                    "--jobs",
                    str(jobs),
                    "--mean-inter-arrival-sec",
                    str(mean_inter_arrival_sec),
                    "--timeout",
                    str(timeout),
                    "--time-scale",
                    str(time_scale),
                    "--benchmark-config",
                    str(cfg_path),
                    "--trace-file",
                    str(trace_file),
                ],
                check=True,
            )
            log(f"completed run {done}/{total_runs}: baseline={baseline} seed={seed}")

    log("sweep completed")


if __name__ == "__main__":
    main()
