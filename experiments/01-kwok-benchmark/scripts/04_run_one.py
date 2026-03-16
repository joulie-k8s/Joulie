#!/usr/bin/env python3
import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import tempfile
import subprocess
import sys
import time
from urllib.request import urlopen

ROOT = pathlib.Path(__file__).resolve().parents[1]
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results"))).resolve()
TRACE_CONFIGMAP_PREFIX = "joulie-simulator-workload-trace"
TRACE_PART_SIZE_BYTES = 900_000


def log(msg: str):
    now = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(f"[run_one {now}] {msg}", flush=True)


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
        run(
            ["kubectl", "-n", "joulie-sim-demo", "delete", "configmap", *names, "--ignore-not-found=true"],
            check=False,
        )


def apply_trace_chunks(trace_path: pathlib.Path) -> list[str]:
    parts = split_trace_parts(trace_path)
    delete_old_trace_configmaps()
    chunk_names: list[str] = []
    with tempfile.TemporaryDirectory(prefix="joulie-trace-parts-") as tmpdir:
        tmpdir_path = pathlib.Path(tmpdir)
        for idx, payload in enumerate(parts):
            key = f"part-{idx:03d}.jsonl"
            name = f"{TRACE_CONFIGMAP_PREFIX}-{idx:03d}"
            part_path = tmpdir_path / key
            part_path.write_bytes(payload)
            rendered = run(
                [
                    "kubectl",
                    "-n",
                    "joulie-sim-demo",
                    "create",
                    "configmap",
                    name,
                    f"--from-file={key}={part_path}",
                    "--dry-run=client",
                    "-o",
                    "yaml",
                ],
                capture=True,
            ).stdout
            replace = run(["kubectl", "replace", "-f", "-"], check=False, capture=True, input_text=rendered)
            if replace.returncode != 0:
                run(["kubectl", "create", "-f", "-"], input_text=rendered)
            chunk_names.append(name)
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


def apply_trace_configmap(trace_path: pathlib.Path):
    log("applying simulator workload trace parts")
    chunk_names = apply_trace_chunks(trace_path)
    patch_simulator_trace_projected_volume(chunk_names)
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

    (run_dir / "operator.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "deploy/joulie-operator", "--tail=400"], capture=True, check=False).stdout)
    (run_dir / "agent.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "statefulset/joulie-agent-pool", "--tail=400"], capture=True, check=False).stdout)
    (run_dir / "simulator.log").write_text(run(["kubectl", "-n", "joulie-sim-demo", "logs", "deploy/joulie-telemetry-sim", "--tail=400"], capture=True).stdout)

    summary = {
        "baseline": baseline,
        "seed": seed,
        "wall_seconds": time.time() - start_ts,
        "trace_sha256": sha256_file(trace_path),
        "timestamp_utc": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    (run_dir / "run_summary.json").write_text(json.dumps(summary, indent=2))

    metadata = {
        "git_commit": run(["git", "rev-parse", "HEAD"], capture=True, check=False).stdout.strip(),
        "baseline": baseline,
        "seed": seed,
        "trace_sha256": summary["trace_sha256"],
        "timeScale": time_scale,
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
    # Accepted for parity with newer sweep scripts; the runner does not need to
    # read the benchmark config directly once the trace file and runtime knobs
    # have already been materialized by the sweep.
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

    collect_artifacts(run_dir, args.baseline, args.seed, start_ts, trace_path, args.time_scale)
    log(f"run completed run_id={run_id}")


if __name__ == "__main__":
    main()
