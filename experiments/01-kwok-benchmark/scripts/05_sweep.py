#!/usr/bin/env python3
import argparse
import datetime as dt
import json
import os
import pathlib
import subprocess
import time


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(f"[sweep {now}] {msg}", flush=True)


def run(cmd, check=True, capture=False):
    if capture:
        return subprocess.run(cmd, check=check, text=True, capture_output=True)
    return subprocess.run(cmd, check=check)


def run_with_env(cmd, env: dict, check=True):
    return subprocess.run(cmd, check=check, env=env)


def generate_seed_trace(seed: int, jobs: int, mean_inter_arrival_sec: float) -> pathlib.Path:
    traces_dir = pathlib.Path("experiments/01-kwok-benchmark/results/traces")
    traces_dir.mkdir(parents=True, exist_ok=True)
    trace_path = traces_dir / f"seed_{seed}_jobs_{jobs}_mia_{str(mean_inter_arrival_sec).replace('.', '_')}.jsonl"
    if trace_path.exists():
        log(f"reusing existing seed trace seed={seed} file={trace_path}")
        return trace_path
    log(f"generating seed trace seed={seed} jobs={jobs} mean_inter_arrival_sec={mean_inter_arrival_sec}")
    run(
        [
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
        ],
        check=True,
    )
    count = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"seed trace generated records={count} file={trace_path}")
    return trace_path


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
    ap.add_argument("--seeds", type=int, default=1)
    ap.add_argument("--jobs", type=int, default=20)
    ap.add_argument("--mean-inter-arrival-sec", type=float, default=0.05)
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--settle-seconds", type=int, default=4)
    ap.add_argument("--cleanup-timeout", type=int, default=45)
    ap.add_argument(
        "--baselines",
        type=str,
        default="A,B,C",
        help="Comma-separated baselines to run (choices: A,B,C), e.g. C or A,C",
    )
    args = ap.parse_args()

    baselines = [b.strip().upper() for b in args.baselines.split(",") if b.strip()]
    invalid = [b for b in baselines if b not in {"A", "B", "C"}]
    if invalid:
        raise SystemExit(f"invalid baseline(s): {','.join(invalid)}; allowed values are A,B,C")
    if not baselines:
        raise SystemExit("no baselines selected; pass --baselines with at least one of A,B,C")

    total_runs = len(baselines) * args.seeds
    done = 0
    log(
        f"starting sweep baselines={','.join(baselines)} seeds={args.seeds} jobs={args.jobs} "
        f"mean_inter_arrival_sec={args.mean_inter_arrival_sec} timeout={args.timeout}s total_runs={total_runs}"
    )
    reset_control_state()
    baseline_policy = {
        "B": "static_partition",
        "C": "queue_aware_v1",
    }

    for baseline in baselines:
        reset_control_state()
        log(f"installing components for baseline={baseline}")
        install_env = os.environ.copy()
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
        wait_zero_active_workload_pods(args.cleanup_timeout)
        log(f"policy settle sleep seconds={args.settle_seconds}")
        time.sleep(args.settle_seconds)
        for seed in range(1, args.seeds + 1):
            done += 1
            log(f"run {done}/{total_runs}: baseline={baseline} seed={seed}")
            trace_file = generate_seed_trace(seed, args.jobs, args.mean_inter_arrival_sec)
            cleanup_workload_pods()
            wait_zero_active_workload_pods(args.cleanup_timeout)
            log(f"pre-run settle sleep seconds={args.settle_seconds}")
            time.sleep(args.settle_seconds)
            run(
                [
                    "python3",
                    "experiments/01-kwok-benchmark/scripts/04_run_one.py",
                    "--baseline",
                    baseline,
                    "--seed",
                    str(seed),
                    "--jobs",
                    str(args.jobs),
                    "--mean-inter-arrival-sec",
                    str(args.mean_inter_arrival_sec),
                    "--timeout",
                    str(args.timeout),
                    "--trace-file",
                    str(trace_file),
                ],
                check=True,
            )
            log(f"completed run {done}/{total_runs}: baseline={baseline} seed={seed}")
    log("sweep completed")


if __name__ == "__main__":
    main()
