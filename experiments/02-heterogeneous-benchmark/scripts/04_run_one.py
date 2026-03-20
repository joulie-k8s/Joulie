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
import tempfile
import time
import uuid
from urllib.request import urlopen

ROOT = pathlib.Path(__file__).resolve().parents[1]
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results"))).resolve()
SIM_DEBUG_PERSIST_DIR = os.environ.get("SIM_DEBUG_PERSIST_DIR", "/tmp/joulie-simulator-debug").strip() or "/tmp/joulie-simulator-debug"
START_TS = time.time()
TRACE_CONFIGMAP_PREFIX = "joulie-simulator-workload-trace"
TRACE_PART_SIZE_BYTES = 900_000


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    elapsed = time.time() - START_TS
    print(f"[run_one {now} +{elapsed:8.1f}s] {msg}", flush=True)


def run(cmd, check=True, capture=False, input_text=None):
    if capture:
        return subprocess.run(cmd, check=check, text=True, capture_output=True, input=input_text)
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


def _affinity_power_class(affinity) -> str:
    """Return 'eco', 'perf', or 'general' based on joulie.io/power-profile affinity expression.

    Parses the actual affinity structure instead of string-matching the JSON to avoid false
    positives from node-name/gpu-product 'In' operators appearing alongside a power-profile
    'NotIn [eco]' expression.
    """
    if not isinstance(affinity, dict):
        return "general"
    node_aff = affinity.get("nodeAffinity")
    if not isinstance(node_aff, dict):
        return "general"
    required = node_aff.get("requiredDuringSchedulingIgnoredDuringExecution")
    if not isinstance(required, dict):
        return "general"
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
        cls = _affinity_power_class(affinity)
        if cls == "eco":
            stats["eco_jobs"] += 1
        elif cls == "perf":
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


def apply_trace_configmap(trace_path: pathlib.Path, persist_dir: str):
    log(f"applying simulator workload trace parts persist_dir={persist_dir}")
    chunk_names = apply_trace_chunks(trace_path)
    patch_simulator_trace_projected_volume(chunk_names)
    run(
        [
            "kubectl",
            "-n",
            "joulie-sim-demo",
            "set",
            "env",
            "deploy/joulie-telemetry-sim",
            f"SIM_DEBUG_PERSIST_DIR={persist_dir}",
        ]
    )
    log("restarting simulator deployment to pick up new trace")
    run(["kubectl", "-n", "joulie-sim-demo", "rollout", "restart", "deploy/joulie-telemetry-sim"])
    run(["kubectl", "-n", "joulie-sim-demo", "rollout", "status", "deploy/joulie-telemetry-sim"])
    log("simulator rollout complete")


def split_trace_parts(trace_path: pathlib.Path) -> list[bytes]:
    parts: list[bytes] = []
    current = bytearray()
    with trace_path.open("rb") as f:
        for raw_line in f:
            if not raw_line:
                continue
            if len(raw_line) > TRACE_PART_SIZE_BYTES:
                raise RuntimeError(f"trace line too large for chunking: {len(raw_line)} bytes")
            if current and len(current) + len(raw_line) > TRACE_PART_SIZE_BYTES:
                parts.append(bytes(current))
                current = bytearray()
            current.extend(raw_line)
    if current:
        parts.append(bytes(current))
    if not parts:
        parts.append(b"")
    return parts


def delete_old_trace_configmaps():
    out = run(
        ["kubectl", "-n", "joulie-sim-demo", "get", "configmap", "-o", "name"],
        capture=True,
        check=False,
    )
    names = []
    for line in out.stdout.splitlines():
        line = line.strip()
        if not line.startswith("configmap/"):
            continue
        name = line.split("/", 1)[1]
        if name == TRACE_CONFIGMAP_PREFIX or name.startswith(f"{TRACE_CONFIGMAP_PREFIX}-"):
            names.append(name)
    if names:
        run(["kubectl", "-n", "joulie-sim-demo", "delete", "configmap", *names, "--ignore-not-found=true"], check=False)


def apply_trace_chunks(trace_path: pathlib.Path) -> list[str]:
    parts = split_trace_parts(trace_path)
    delete_old_trace_configmaps()
    chunk_names: list[str] = []
    with tempfile.TemporaryDirectory(prefix="joulie-trace-parts-") as tmpdir:
        tmpdir_path = pathlib.Path(tmpdir)
        manifests_dir = tmpdir_path / "manifests"
        manifests_dir.mkdir()
        for idx, payload in enumerate(parts):
            key = f"part-{idx:03d}.jsonl"
            name = f"{TRACE_CONFIGMAP_PREFIX}-{idx:03d}"
            cm = {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": name, "namespace": "joulie-sim-demo"},
                "data": {key: payload.decode("utf-8", errors="replace")},
            }
            (manifests_dir / f"{name}.json").write_text(json.dumps(cm))
            chunk_names.append(name)
        log(f"creating {len(chunk_names)} configmap manifests in single batch")
        run(["kubectl", "create", "-f", str(manifests_dir)])
    log(f"trace split into configmap parts count={len(chunk_names)}")
    return chunk_names


def patch_simulator_trace_projected_volume(chunk_names: list[str]):
    sources = []
    for idx, name in enumerate(chunk_names):
        key = f"part-{idx:03d}.jsonl"
        sources.append(
            {
                "configMap": {
                    "name": name,
                    "items": [{"key": key, "path": key}],
                }
            }
        )
    deploy = json.loads(
        run(
            [
                "kubectl",
                "-n",
                "joulie-sim-demo",
                "get",
                "deployment",
                "joulie-telemetry-sim",
                "-o",
                "json",
            ],
            capture=True,
        ).stdout
    )
    deploy.pop("status", None)
    metadata = deploy.get("metadata", {})
    for key in (
        "annotations",
        "creationTimestamp",
        "generation",
        "managedFields",
        "resourceVersion",
        "uid",
    ):
        metadata.pop(key, None)
    spec = deploy.setdefault("spec", {}).setdefault("template", {}).setdefault("spec", {})
    volumes = spec.setdefault("volumes", [])
    replaced = False
    for volume in volumes:
        if volume.get("name") == "workload-trace":
            volume.clear()
            volume.update({"name": "workload-trace", "projected": {"sources": sources}})
            replaced = True
            break
    if not replaced:
        volumes.append({"name": "workload-trace", "projected": {"sources": sources}})

    containers = spec.get("containers", [])
    simulator = next((c for c in containers if c.get("name") == "simulator"), None)
    if simulator is None:
        raise RuntimeError("simulator container not found in deployment")
    env = simulator.setdefault("env", [])
    env_entry = next((e for e in env if e.get("name") == "SIM_WORKLOAD_TRACE_PATH"), None)
    if env_entry is None:
        env.append({"name": "SIM_WORKLOAD_TRACE_PATH", "value": "/etc/joulie-sim-trace"})
    else:
        env_entry["value"] = "/etc/joulie-sim-trace"
    mounts = simulator.setdefault("volumeMounts", [])
    mount_entry = next((m for m in mounts if m.get("name") == "workload-trace" or m.get("mountPath") == "/etc/joulie-sim-trace"), None)
    if mount_entry is None:
        mounts.append({"name": "workload-trace", "mountPath": "/etc/joulie-sim-trace", "readOnly": True})
    else:
        mount_entry["name"] = "workload-trace"
        mount_entry["mountPath"] = "/etc/joulie-sim-trace"
        mount_entry["readOnly"] = True

    run(["kubectl", "apply", "-f", "-"], input_text=json.dumps(deploy))


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
    pod_persist_dir: str,
    host_persist_dir: pathlib.Path,
):
    log("collecting artifacts")
    (run_dir / "trace.jsonl").write_text(trace_path.read_text())
    host_persist_dir.mkdir(parents=True, exist_ok=True)

    def valid_debug_payload(text: str) -> bool:
        text = (text or "").strip()
        if not text or text == "{}":
            return False
        try:
            obj = json.loads(text)
        except json.JSONDecodeError:
            return False
        return isinstance(obj, dict) and bool(obj)

    def fetch_via_port_forward(endpoint: str, timeout_sec: int) -> str:
        pf = subprocess.Popen(
            ["kubectl", "-n", "joulie-sim-demo", "port-forward", "deploy/joulie-telemetry-sim", "18080:18080"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        try:
            time.sleep(1.5)
            return urlopen(f"http://127.0.0.1:18080{endpoint}", timeout=timeout_sec).read().decode()
        finally:
            pf.terminate()
            try:
                pf.wait(timeout=5)
            except Exception:
                pf.kill()

    def fetch_via_exec(filename: str) -> str:
        out = run(
            [
                "kubectl",
                "-n",
                "joulie-sim-demo",
                "exec",
                "deploy/joulie-telemetry-sim",
                "--",
                "cat",
                f"{pod_persist_dir}/{filename}",
            ],
            capture=True,
            check=False,
        )
        return out.stdout.strip()

    def fetch_debug_artifact(endpoint: str, filename: str) -> str:
        for timeout_sec in (5, 15, 30):
            try:
                payload = fetch_via_port_forward(endpoint, timeout_sec)
                if valid_debug_payload(payload):
                    return payload
            except Exception:
                pass
            time.sleep(1)
        payload = fetch_via_exec(filename)
        if valid_debug_payload(payload):
            return payload
        return "{}"

    def mirror_persisted_file(filename: str):
        payload = fetch_via_exec(filename)
        if payload:
            (host_persist_dir / filename).write_text(payload)

    nodes = fetch_debug_artifact("/debug/nodes", "nodes.json")
    jobs = fetch_debug_artifact("/debug/jobs", "jobs.json")
    events = fetch_debug_artifact("/debug/events", "events.json")
    energy = fetch_debug_artifact("/debug/energy", "energy.json")

    (run_dir / "sim_debug_nodes.json").write_text(nodes)
    (run_dir / "sim_debug_jobs.json").write_text(jobs)
    (run_dir / "sim_debug_events.json").write_text(events)
    (run_dir / "sim_debug_energy.json").write_text(energy)

    (host_persist_dir / "nodes.json").write_text(nodes)
    (host_persist_dir / "jobs.json").write_text(jobs)
    (host_persist_dir / "events.json").write_text(events)
    (host_persist_dir / "energy.json").write_text(energy)
    mirror_persisted_file("events.ndjson")
    mirror_persisted_file("timeseries.csv")

    # Also copy timeseries.csv to run_dir for easy access.
    ts_src = host_persist_dir / "timeseries.csv"
    if ts_src.exists():
        (run_dir / "timeseries.csv").write_text(ts_src.read_text())
    else:
        # Fetch via HTTP debug endpoint (works on distroless containers).
        for ts_timeout in (10, 30, 60):
            try:
                ts_csv = fetch_via_port_forward("/debug/timeseries", ts_timeout)
                if ts_csv and len(ts_csv) > 20:
                    (run_dir / "timeseries.csv").write_text(ts_csv)
                    break
            except Exception:
                pass
            time.sleep(2)

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

    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"_u{uuid.uuid4().hex}_b{args.baseline}_s{args.seed}"
    run_dir = RESULTS / run_id
    run_dir.mkdir(parents=True, exist_ok=False)
    log(f"starting run_id={run_id} baseline={args.baseline} seed={args.seed} jobs={args.jobs}")
    pod_persist_dir = f"/tmp/joulie-simulator-debug/{run_id}"
    host_persist_dir = pathlib.Path(SIM_DEBUG_PERSIST_DIR).resolve() / run_id

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
    apply_trace_configmap(trace_path, pod_persist_dir)

    start_ts = time.time()
    done = wait_completion(args.timeout, args.poll_log_sec)
    if not done:
        print("timeout waiting for completion", file=sys.stderr)

    benchmark_config_path = pathlib.Path(args.benchmark_config).resolve() if args.benchmark_config.strip() else None
    collect_artifacts(
        run_dir,
        args.baseline,
        args.seed,
        start_ts,
        trace_path,
        args.time_scale,
        benchmark_config_path,
        pod_persist_dir,
        host_persist_dir,
    )
    log(f"run completed run_id={run_id}")


if __name__ == "__main__":
    main()
