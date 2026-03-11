#!/usr/bin/env python3
import argparse
import datetime as dt
import json
import os
import pathlib
import subprocess
import time

import yaml

DEFAULT_CONFIG = pathlib.Path("experiments/01-kwok-benchmark/configs/benchmark.yaml")


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(f"[sweep {now}] {msg}", flush=True)


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
    eco_ratio: float,
    cpu_units_min: float,
    cpu_units_max: float,
) -> pathlib.Path:
    traces_dir = pathlib.Path("experiments/01-kwok-benchmark/results/traces")
    traces_dir.mkdir(parents=True, exist_ok=True)
    ratio_id = f"perf_{str(perf_ratio).replace('.', '_')}_eco_{str(eco_ratio).replace('.', '_')}"
    units_id = f"cpuu_{str(cpu_units_min).replace('.', '_')}_{str(cpu_units_max).replace('.', '_')}"
    trace_name = (
        f"seed_{seed}_jobs_{jobs}_mia_{str(mean_inter_arrival_sec).replace('.', '_')}_"
        f"{ratio_id}_{units_id}_canonical.jsonl"
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
        "--cpu-units-min",
        str(cpu_units_min),
        "--cpu-units-max",
        str(cpu_units_max),
    ]
    cmd.extend(
        [
            "--perf-ratio",
            str(perf_ratio),
            "--eco-ratio",
            str(eco_ratio),
        ]
    )
    run(cmd, check=True)
    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"canonical seed trace generated records={count} file={trace_path}")
    return trace_path


def derive_baseline_trace(baseline: str, canonical_trace: pathlib.Path, strip_affinity_for_a: bool) -> pathlib.Path:
    traces_dir = canonical_trace.parent
    out_name = canonical_trace.name.replace("_canonical.jsonl", f"_baseline_{baseline}.jsonl")
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
    run(["kubectl", "delete", "nodepowerprofiles", "--all", "--ignore-not-found"], check=False)
    run(["kubectl", "delete", "telemetryprofiles", "--all", "--ignore-not-found"], check=False)
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
    ap.add_argument("--eco-ratio", type=float, default=None, help="must be 0 for this benchmark profile")
    ap.add_argument("--cpu-units-min", type=float, default=None)
    ap.add_argument("--cpu-units-max", type=float, default=None)
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
        args.perf_ratio if args.perf_ratio is not None else float(get_cfg(cfg, "workload", "perf_ratio", default=0.30))
    )
    eco_ratio = (
        args.eco_ratio if args.eco_ratio is not None else float(get_cfg(cfg, "workload", "eco_ratio", default=0.00))
    )
    cpu_units_min = (
        args.cpu_units_min
        if args.cpu_units_min is not None
        else float(get_cfg(cfg, "workload", "cpu_units_min", default=600.0))
    )
    cpu_units_max = (
        args.cpu_units_max
        if args.cpu_units_max is not None
        else float(get_cfg(cfg, "workload", "cpu_units_max", default=3600.0))
    )
    baseline_a_strip_affinity = bool(get_cfg(cfg, "workload", "baseline_a_strip_affinity", default=True))

    baselines_raw = args.baselines if args.baselines.strip() else get_cfg(cfg, "run", "baselines", default=["A", "B", "C"])
    baselines = to_baselines(baselines_raw)

    if perf_ratio < 0 or eco_ratio < 0:
        raise SystemExit("perf_ratio and eco_ratio must be >= 0")
    if perf_ratio + eco_ratio > 1:
        raise SystemExit("perf_ratio + eco_ratio must be <= 1")
    if cpu_units_min <= 0:
        raise SystemExit("cpu_units_min must be > 0")
    if cpu_units_max < cpu_units_min:
        raise SystemExit("cpu_units_max must be >= cpu_units_min")

    total_runs = len(baselines) * seeds
    done = 0
    log(
        f"starting sweep config={cfg_path} baselines={','.join(baselines)} seeds={seeds} jobs={jobs} "
        f"mean_inter_arrival_sec={mean_inter_arrival_sec} timeout={timeout}s time_scale={time_scale} total_runs={total_runs}"
    )

    install_env_base = os.environ.copy()

    # Image and manifest config
    install_env_base["JOULIE_REGISTRY"] = str(get_cfg(cfg, "images", "joulie_registry", default="registry.cern.ch/mbunino/joulie"))
    install_env_base["JOULIE_TAG"] = str(get_cfg(cfg, "images", "joulie_tag", default="latest"))
    install_env_base["SIM_REGISTRY"] = str(get_cfg(cfg, "images", "sim_registry", default="registry.cern.ch/mbunino/joulie"))
    install_env_base["SIM_IMAGE"] = str(get_cfg(cfg, "images", "sim_image", default="joulie-simulator"))
    install_env_base["SIM_TAG"] = str(get_cfg(cfg, "images", "sim_tag", default=""))

    simulator_manifest = get_cfg(cfg, "install", "simulator_manifest", default="")
    if simulator_manifest:
        install_env_base["SIMULATOR_MANIFEST"] = str(simulator_manifest)

    # Policy config
    install_env_base["STATIC_HP_FRAC"] = str(get_cfg(cfg, "policy", "static", "hp_frac", default=0.50))
    install_env_base["QUEUE_HP_BASE_FRAC"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_base_frac", default=0.60))
    install_env_base["QUEUE_HP_MIN"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_min", default=1))
    install_env_base["QUEUE_HP_MAX"] = str(get_cfg(cfg, "policy", "queue_aware", "hp_max", default=5))
    install_env_base["QUEUE_PERF_PER_HP_NODE"] = str(get_cfg(cfg, "policy", "queue_aware", "perf_per_hp_node", default=10))
    install_env_base["PERFORMANCE_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "performance_watts", default=500))
    install_env_base["ECO_CAP_WATTS"] = str(get_cfg(cfg, "policy", "caps", "eco_watts", default=140))
    install_env_base["OPERATOR_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "operator_reconcile_interval", default="20s"))
    install_env_base["AGENT_RECONCILE_INTERVAL"] = str(get_cfg(cfg, "policy", "loop", "agent_reconcile_interval", default="10s"))
    install_env_base["SIM_BASE_SPEED_PER_CORE"] = str(get_cfg(cfg, "simulator", "base_speed_per_core", default=1.0))
    log(
        "configured images "
        f"sim={install_env_base['SIM_REGISTRY']}/{install_env_base['SIM_IMAGE']}"
        + (f":{install_env_base['SIM_TAG']}" if install_env_base["SIM_TAG"] else " (manifest-tag)")
        + f" operator={install_env_base['JOULIE_REGISTRY']}/joulie-operator:{install_env_base['JOULIE_TAG']}"
        + f" agent={install_env_base['JOULIE_REGISTRY']}/joulie-agent:{install_env_base['JOULIE_TAG']}"
    )
    log(
        "configured policy "
        f"caps(perf={install_env_base['PERFORMANCE_CAP_WATTS']}W eco={install_env_base['ECO_CAP_WATTS']}W) "
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
        run_with_env(
            [
                "bash",
                "experiments/01-kwok-benchmark/scripts/03_install_components.sh",
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
                eco_ratio=eco_ratio,
                cpu_units_min=cpu_units_min,
                cpu_units_max=cpu_units_max,
            )
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
                    "experiments/01-kwok-benchmark/scripts/04_run_one.py",
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
                    "--trace-file",
                    str(trace_file),
                ],
                check=True,
            )
            log(f"completed run {done}/{total_runs}: baseline={baseline} seed={seed}")

    log("sweep completed")


if __name__ == "__main__":
    main()
