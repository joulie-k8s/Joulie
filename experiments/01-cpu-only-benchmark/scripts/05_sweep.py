#!/usr/bin/env python3
import argparse
import datetime as dt
import json
import os
import pathlib
import subprocess
import time
from collections import deque

import yaml

DEFAULT_CONFIG = pathlib.Path("experiments/01-cpu-only-benchmark/configs/benchmark.yaml")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", "experiments/01-cpu-only-benchmark/runs/latest/results"))
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
        "--jobs",
        str(jobs),
        "--seed",
        str(seed),
        "--out",
        str(trace_path),
        "--mean-inter-arrival-sec",
        str(mean_inter_arrival_sec),
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
    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"canonical seed trace generated records={count} file={trace_path}")
    return trace_path


def _pod_power_class(affinity) -> str:
    if not isinstance(affinity, dict):
        return "general"
    node_aff = affinity.get("nodeAffinity") or {}
    required = node_aff.get("requiredDuringSchedulingIgnoredDuringExecution") or {}
    for term in required.get("nodeSelectorTerms") or []:
        for expr in (term.get("matchExpressions") or []) if isinstance(term, dict) else []:
            if not isinstance(expr, dict):
                continue
            if expr.get("key") != "joulie.io/power-profile":
                continue
            op = expr.get("operator", "")
            vals = expr.get("values") or []
            if op == "In" and "eco" in vals:
                return "eco"
            if op == "NotIn" and "eco" in vals:
                return "perf"
    return "general"


def load_kwok_nodes() -> list[dict]:
    out = run(["kubectl", "get", "nodes", "-l", "type=kwok", "-o", "json"], capture=True)
    items = json.loads(out.stdout).get("items", [])
    nodes = []
    for item in items:
        meta = item.get("metadata", {})
        labels = meta.get("labels", {}) or {}
        name = meta.get("name", "")
        nodes.append(
            {
                "name": name,
                "cpu_model": labels.get("joulie.io/hw.cpu-model", ""),
            }
        )
    return nodes


def rotate_pick(pool: deque[dict]) -> dict:
    item = pool[0]
    pool.rotate(-1)
    return item


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
            if isinstance(e, dict) and e.get("key") not in {"joulie.io/power-profile", "joulie.io/draining"}
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


def retarget_trace_for_cluster(trace_path: pathlib.Path) -> pathlib.Path:
    """Assign node/family affinities so each job targets a specific CPU node or CPU model family."""
    nodes = load_kwok_nodes()
    cpu_pool = build_family_first_pool(nodes, "cpu_model")
    if not cpu_pool:
        raise SystemExit("expected CPU KWOK nodes to exist before generating the benchmark trace")

    out_path = trace_path.with_name(trace_path.stem + "_targeted.jsonl")
    if out_path.exists():
        return out_path

    total_job_count = 0
    lines = []
    for raw in trace_path.read_text().splitlines():
        raw = raw.strip()
        if not raw:
            continue
        rec = json.loads(raw)
        if rec.get("type", "job") != "job":
            lines.append(json.dumps(rec, separators=(",", ":")))
            continue

        total_job_count += 1
        pod_tpl = rec.get("podTemplate")
        if not isinstance(pod_tpl, dict):
            lines.append(json.dumps(rec, separators=(",", ":")))
            continue

        affinity = pod_tpl.get("affinity")
        # Keep CPU workloads on the synthetic CPU-only pool, but let Kubernetes place them on
        # any compatible KWOK node instead of pre-assigning a concrete node in the harness.
        pod_tpl["affinity"] = add_required_expr(
            affinity,
            {"key": "joulie.io/hw.kind", "operator": "In", "values": ["cpu-only"]},
        )

        lines.append(json.dumps(rec, separators=(",", ":")))

    out_path.write_text("\n".join(lines) + ("\n" if lines else ""))
    log(f"retargeted canonical trace jobs_total={total_job_count} cpu_nodes={len(nodes)}")
    return out_path


def derive_baseline_trace(baseline: str, canonical_trace: pathlib.Path, strip_affinity_for_a: bool) -> pathlib.Path:
    traces_dir = canonical_trace.parent
    stem = canonical_trace.stem
    if stem.endswith("_targeted"):
        stem = stem[: -len("_targeted")]
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
            "joulie.io/draining=false",
            "--overwrite",
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
        args.perf_ratio if args.perf_ratio is not None else float(get_cfg(cfg, "workload", "perf_ratio", default=0.20))
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
        else float(get_cfg(cfg, "workload", "burst_day_probability", default=0.20))
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

    total_runs = len(baselines) * seeds
    done = 0
    log(
        f"starting sweep config={cfg_path} baselines={','.join(baselines)} seeds={seeds} jobs={jobs} "
        f"mean_inter_arrival_sec={mean_inter_arrival_sec} timeout={timeout}s time_scale={time_scale} total_runs={total_runs}"
    )

    install_env_base = os.environ.copy()
    inventory_source = str(get_cfg(cfg, "inventory", "source", default="experiments/01-cpu-only-benchmark/configs/cluster-nodes.yaml"))
    run(["bash", "experiments/01-cpu-only-benchmark/scripts/00_generate_assets.sh", inventory_source], check=True)

    # Image and manifest config
    install_env_base["JOULIE_REGISTRY"] = str(get_cfg(cfg, "images", "joulie_registry", default="registry.cern.ch/mbunino/joulie"))
    install_env_base["JOULIE_TAG"] = str(get_cfg(cfg, "images", "joulie_tag", default="latest"))
    install_env_base["SIM_REGISTRY"] = str(get_cfg(cfg, "images", "sim_registry", default="registry.cern.ch/mbunino/joulie"))
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
    install_env_base["CPU_PERFORMANCE_CAP_PCT_OF_MAX"] = str(
        get_cfg(cfg, "policy", "caps", "cpu_performance_pct_of_max", default=100)
    )
    install_env_base["CPU_ECO_CAP_PCT_OF_MAX"] = str(
        get_cfg(cfg, "policy", "caps", "cpu_eco_pct_of_max", default=80)
    )
    install_env_base["CPU_WRITE_ABSOLUTE_CAPS"] = str(get_cfg(cfg, "policy", "cpu_write_absolute_caps", default=False)).lower()
    install_env_base["PERFORMANCE_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "performance_watts", default=5000))
    install_env_base["ECO_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "eco_watts", default=120))
    install_env_base["OPERATOR_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "operator_reconcile_interval", default="20s"))
    install_env_base["AGENT_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "agent_reconcile_interval", default="10s"))
    install_env_base["SIM_BASE_SPEED_PER_CORE"] = str(get_cfg(cfg, "simulator", "base_speed_per_core", default=1.0))
    install_env_base["SIM_CLASSIFIER_MISCLASSIFY_RATE"] = str(get_cfg(cfg, "simulator", "classifier_misclassify_rate", default=0.10))
    install_env_base["SIM_FACILITY_AMBIENT_BASE_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_base_c", default=22.0))
    install_env_base["SIM_FACILITY_AMBIENT_AMPLITUDE_C"] = str(get_cfg(cfg, "simulator", "facility_ambient_amplitude_c", default=8.0))
    install_env_base["SIM_FACILITY_AMBIENT_PERIOD_SEC"] = str(get_cfg(cfg, "simulator", "facility_ambient_period_sec", default=600))
    install_env_base["ENABLE_CLASSIFIER"] = str(get_cfg(cfg, "operator", "enable_classifier", default=True)).lower()
    install_env_base["ENABLE_FACILITY_METRICS"] = str(get_cfg(cfg, "operator", "enable_facility_metrics", default=False)).lower()
    # GENERATED_CLASSES/CATALOG: prefer env vars already set by 30_run_overnight.sh, fall back to generated/ dir
    install_env_base["GENERATED_CLASSES"] = os.environ.get(
        "GENERATED_CLASSES", "experiments/01-cpu-only-benchmark/generated/10-node-classes.yaml"
    )
    install_env_base["GENERATED_CATALOG"] = os.environ.get(
        "GENERATED_CATALOG", "experiments/01-cpu-only-benchmark/generated/hardware.generated.yaml"
    )

    log(
        "configured images "
        f"sim={install_env_base['SIM_REGISTRY']}/{install_env_base['SIM_IMAGE']}"
        + (f":{install_env_base['SIM_TAG']}" if install_env_base["SIM_TAG"] else " (manifest-tag)")
        + f" operator={install_env_base['JOULIE_REGISTRY']}/joulie-operator:{install_env_base['JOULIE_TAG']}"
        + f" agent={install_env_base['JOULIE_REGISTRY']}/joulie-agent:{install_env_base['JOULIE_TAG']}"
    )
    log(
        "configured policy "
        f"caps(cpu_perf={install_env_base['CPU_PERFORMANCE_CAP_PCT_OF_MAX']}% "
        f"cpu_eco={install_env_base['CPU_ECO_CAP_PCT_OF_MAX']}% "
        f"cpu_abs={install_env_base['CPU_WRITE_ABSOLUTE_CAPS']}) "
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

    for baseline in baselines:
        reset_control_state()
        log(f"installing components for baseline={baseline}")
        install_env = install_env_base.copy()
        if baseline in baseline_policy:
            install_env["POLICY_TYPE"] = baseline_policy[baseline]
        else:
            install_env.pop("POLICY_TYPE", None)
        # For queue-aware (baseline C), compute the operator reconcile interval
        # from simulated seconds divided by time_scale.
        if baseline == "C":
            qa_sim_sec = float(get_cfg(cfg, "policy", "loop", "queue_aware_operator_reconcile_sim_seconds", default=300))
            qa_wall_sec = max(1, int(qa_sim_sec / time_scale))
            install_env["OPERATOR_RECONCILE_INTERVAL"] = f"{qa_wall_sec}s"
            log(f"baseline C: queue-aware operator reconcile = {qa_sim_sec}s simulated / {time_scale} = {qa_wall_sec}s wall")
        run_with_env(
            [
                "bash",
                "experiments/01-cpu-only-benchmark/scripts/03_install_components.sh",
                baseline,
            ],
            env=install_env,
            check=True,
        )
        cleanup_workload_pods()
        wait_zero_active_workload_pods(cleanup_timeout)
        log(f"policy settle sleep seconds={settle_seconds}")
        time.sleep(settle_seconds)

        for seed in range(1, seeds + 1):
            done += 1
            log(f"run {done}/{total_runs}: baseline={baseline} seed={seed}")
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
            )
            canonical_trace = retarget_trace_for_cluster(canonical_trace)
            trace_file = derive_baseline_trace(
                baseline=baseline,
                canonical_trace=canonical_trace,
                strip_affinity_for_a=baseline_a_strip_affinity,
            )
            log(f"using baseline trace file={trace_file}")
            cleanup_workload_pods()
            wait_zero_active_workload_pods(cleanup_timeout)
            log(f"pre-run settle sleep seconds={settle_seconds}")
            time.sleep(settle_seconds)
            run(
                [
                    "python3",
                    "experiments/01-cpu-only-benchmark/scripts/04_run_one.py",
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
