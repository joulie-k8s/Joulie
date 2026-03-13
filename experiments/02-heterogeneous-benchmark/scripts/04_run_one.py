#!/usr/bin/env python3
import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import platform
import subprocess
import sys
import time
from urllib.request import urlopen

ROOT = pathlib.Path(__file__).resolve().parents[1]
RESULTS = ROOT / "results"


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(f"[run_one {now}] {msg}", flush=True)


def run(cmd, check=True, capture=False, input_text=None):
    if capture:
        return subprocess.run(cmd, check=check, text=True, capture_output=True)
    return subprocess.run(cmd, check=check, text=True, input=input_text)


def sha256_file(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def safe_capture(cmd):
    try:
        return run(cmd, capture=True, check=False).stdout.strip()
    except Exception:
        return ""


def trace_stats(trace_path: pathlib.Path) -> dict:
    stats = {
        "records_total": 0,
        "workloads": 0,
        "jobs": 0,
        "gpu_jobs": 0,
        "perf_jobs": 0,
        "eco_jobs": 0,
        "general_jobs": 0,
    }
    for line in trace_path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        stats["records_total"] += 1
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        rec_type = rec.get("type", "job")
        if rec_type == "workload":
            stats["workloads"] += 1
            continue
        if rec_type != "job":
            continue
        stats["jobs"] += 1
        pod_tpl = rec.get("podTemplate", {})
        req = pod_tpl.get("requests", {}) if isinstance(pod_tpl, dict) else {}
        if isinstance(req, dict) and any(k in req for k in ("nvidia.com/gpu", "amd.com/gpu", "gpu")):
            stats["gpu_jobs"] += 1
        affinity = pod_tpl.get("affinity") if isinstance(pod_tpl, dict) else None
        affinity_json = json.dumps(affinity, sort_keys=True) if affinity is not None else ""
        if "\"eco\"" in affinity_json and "\"In\"" in affinity_json:
            stats["eco_jobs"] += 1
        elif "\"eco\"" in affinity_json and "\"NotIn\"" in affinity_json:
            stats["perf_jobs"] += 1
        else:
            stats["general_jobs"] += 1
    return stats


def generate_trace(trace_path: pathlib.Path, seed: int, jobs: int, mean_inter_arrival_sec: float):
    log(
        f"generating trace jobs={jobs} seed={seed} "
        f"mean_inter_arrival_sec={mean_inter_arrival_sec} out={trace_path}"
    )
    run([
        "go", "run", "./simulator/cmd/workloadgen",
        "--jobs", str(jobs),
        "--seed", str(seed),
        "--out", str(trace_path),
        "--mean-inter-arrival-sec", str(mean_inter_arrival_sec),
    ])
    generated = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
    log(f"trace generated records={generated}")


def reset_workload_pods():
    log("resetting previous workload pods")
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
            "--timeout=120s",
        ],
        check=False,
    )


def apply_trace_configmap(trace_path: pathlib.Path):
    log("applying simulator workload trace configmap")
    content = trace_path.read_text()
    manifest = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: joulie-simulator-workload-trace
  namespace: joulie-sim-demo
data:
  trace.jsonl: |
"""
    for line in content.splitlines():
        if line.strip():
            manifest += f"    {line}\n"
    run(["kubectl", "apply", "-f", "-"], input_text=manifest)
    log("restarting simulator deployment to pick up new trace")
    run(["kubectl", "-n", "joulie-sim-demo", "rollout", "restart", "deploy/joulie-telemetry-sim"])
    run(["kubectl", "-n", "joulie-sim-demo", "rollout", "status", "deploy/joulie-telemetry-sim"])
    log("simulator rollout complete")


def wait_completion(timeout_sec: int, poll_log_sec: int):
    log(f"waiting for completion timeout_sec={timeout_sec}")
    start = time.time()
    seen_nonzero = False
    last_log = 0.0
    while time.time() - start < timeout_sec:
        out = run([
            "kubectl", "get", "pods", "-A", "-l", "app.kubernetes.io/part-of=joulie-sim-workload",
            "-o", "json",
        ], capture=True)
        items = json.loads(out.stdout).get("items", [])
        total = len(items)
        if items:
            seen_nonzero = True
        active = sum(1 for p in items if p.get("status", {}).get("phase") in ("Pending", "Running"))
        now = time.time()
        if now - last_log >= poll_log_sec:
            elapsed = int(now - start)
            log(f"progress elapsed={elapsed}s total_pods={total} active_pods={active} seen_nonzero={seen_nonzero}")
            last_log = now
        if seen_nonzero and active == 0:
            elapsed = int(time.time() - start)
            log(f"all workload pods completed elapsed={elapsed}s")
            return True
        time.sleep(1)
    log("timed out waiting for workload completion")
    return False


def collect_artifacts(
    run_dir: pathlib.Path,
    baseline: str,
    seed: int,
    start_ts: float,
    trace_path: pathlib.Path,
    time_scale: float,
    benchmark_config_path: pathlib.Path | None,
):
    log("collecting artifacts")
    (run_dir / "trace.jsonl").write_text(trace_path.read_text())

    pf = subprocess.Popen(["kubectl", "-n", "joulie-sim-demo", "port-forward", "deploy/joulie-telemetry-sim", "18080:18080"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1)
    try:
        nodes = urlopen("http://127.0.0.1:18080/debug/nodes", timeout=3).read().decode()
        events = urlopen("http://127.0.0.1:18080/debug/events", timeout=3).read().decode()
        energy = urlopen("http://127.0.0.1:18080/debug/energy", timeout=3).read().decode()
    except Exception:
        nodes, events, energy = "{}", "{}", "{}"
    finally:
        pf.terminate()

    (run_dir / "sim_debug_nodes.json").write_text(nodes)
    (run_dir / "sim_debug_events.json").write_text(events)
    (run_dir / "sim_debug_energy.json").write_text(energy)

    (run_dir / "pods.json").write_text(run(["kubectl", "get", "pods", "-A", "-o", "json"], capture=True).stdout)
    (run_dir / "nodepowerprofiles.yaml").write_text(run(["kubectl", "get", "nodepowerprofiles", "-o", "yaml"], capture=True, check=False).stdout)
    (run_dir / "nodehardwares.yaml").write_text(run(["kubectl", "get", "nodehardwares", "-o", "yaml"], capture=True, check=False).stdout)

    (run_dir / "operator.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "deploy/joulie-operator", "--tail=400"], capture=True, check=False).stdout)
    (run_dir / "agent.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "statefulset/joulie-agent-pool", "--tail=400"], capture=True, check=False).stdout)
    (run_dir / "simulator.log").write_text(run(["kubectl", "-n", "joulie-sim-demo", "logs", "deploy/joulie-telemetry-sim", "--tail=400"], capture=True).stdout)
    (run_dir / "nodes.json").write_text(run(["kubectl", "get", "nodes", "-o", "json"], capture=True, check=False).stdout)
    (run_dir / "kubectl_version.json").write_text(safe_capture(["kubectl", "version", "-o", "json"]))
    (run_dir / "go_version.txt").write_text(safe_capture(["go", "version"]) + "\n")
    (run_dir / "python_version.txt").write_text(sys.version + "\n")
    if benchmark_config_path is not None and benchmark_config_path.exists():
        (run_dir / "benchmark_config.yaml").write_text(benchmark_config_path.read_text())

    summary = {
        "baseline": baseline,
        "seed": seed,
        "wall_seconds": time.time() - start_ts,
        "trace_sha256": sha256_file(trace_path),
        "timestamp_utc": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    (run_dir / "run_summary.json").write_text(json.dumps(summary, indent=2))

    trace_summary = trace_stats(trace_path)
    benchmark_config_sha = ""
    if benchmark_config_path is not None and benchmark_config_path.exists():
        benchmark_config_sha = sha256_file(benchmark_config_path)

    metadata = {
        "git_commit": run(["git", "rev-parse", "HEAD"], capture=True, check=False).stdout.strip(),
        "git_status_porcelain": safe_capture(["git", "status", "--short"]),
        "baseline": baseline,
        "seed": seed,
        "trace_sha256": summary["trace_sha256"],
        "trace_stats": trace_summary,
        "timeScale": time_scale,
        "benchmark_config_path": str(benchmark_config_path) if benchmark_config_path is not None else "",
        "benchmark_config_sha256": benchmark_config_sha,
        "platform": {
            "python": platform.python_version(),
            "system": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
        },
        "measurement": {
            "gpu_power_telemetry_note": "Simulator exports averaged and instantaneous power; real NVIDIA NVML power may be averaged over a 1s window on many modern GPUs.",
            "cpu_power_telemetry_note": "CPU package power in real deployments is often inferred from energy deltas and therefore depends on sampling interval/window.",
        },
    }
    (run_dir / "metadata.json").write_text(json.dumps(metadata, indent=2))
    log(f"artifacts written to {run_dir}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--baseline", required=True, choices=["A", "B", "C"])
    ap.add_argument("--seed", required=True, type=int)
    ap.add_argument("--jobs", default=20, type=int)
    ap.add_argument("--timeout", default=240, type=int)
    ap.add_argument("--mean-inter-arrival-sec", default=0.05, type=float)
    ap.add_argument("--poll-log-sec", default=3, type=int)
    ap.add_argument("--trace-file", default="", type=str)
    ap.add_argument("--time-scale", default=60.0, type=float)
    ap.add_argument("--benchmark-config", default="", type=str)
    args = ap.parse_args()

    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"_b{args.baseline}_s{args.seed}"
    run_dir = RESULTS / run_id
    run_dir.mkdir(parents=True, exist_ok=False)
    log(f"starting run_id={run_id} baseline={args.baseline} seed={args.seed} jobs={args.jobs}")

    trace_path = run_dir / "trace.jsonl"
    reset_workload_pods()
    if args.trace_file.strip():
        src = pathlib.Path(args.trace_file).resolve()
        if not src.exists():
            raise SystemExit(f"trace file not found: {src}")
        trace_path.write_text(src.read_text())
        copied = sum(1 for l in trace_path.read_text().splitlines() if l.strip())
        log(f"reusing pre-generated trace file={src} records={copied}")
    else:
        generate_trace(trace_path, args.seed, args.jobs, args.mean_inter_arrival_sec)
    apply_trace_configmap(trace_path)

    start_ts = time.time()
    done = wait_completion(args.timeout, args.poll_log_sec)
    if not done:
        print("timeout waiting for completion", file=sys.stderr)

    benchmark_config_path = pathlib.Path(args.benchmark_config).resolve() if args.benchmark_config.strip() else None
    collect_artifacts(run_dir, args.baseline, args.seed, start_ts, trace_path, args.time_scale, benchmark_config_path)
    log(f"run completed run_id={run_id}")


if __name__ == "__main__":
    main()
