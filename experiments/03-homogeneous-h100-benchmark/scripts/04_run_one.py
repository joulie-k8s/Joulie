#!/usr/bin/env python3
import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
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
        "--schema-version", "v2",
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


# LOAD_SAMPLES holds the host's 1-minute load average at each poll of
# run_window: the simulator advances jobs by wall-clock time, so a
# loaded host is part of the conditions of a run.
LOAD_SAMPLES: list[float] = []


def control_errors() -> int:
    """How many control commands the simulator refused (result "error" in
    joulie_sim_control_actions_total), such as a cap below what the node's
    hardware accepts. A run with any is not a measurement."""
    total = 0
    for line in fetch_via_port_forward("/metrics", 30).splitlines():
        if line.startswith("joulie_sim_control_actions_total{") and 'result="error"' in line:
            total += int(float(line.rsplit(" ", 1)[1]))
    return total


def host_load() -> float:
    """The host's 1-minute load average."""
    return os.getloadavg()[0]


def job_pools(trace_path: pathlib.Path) -> dict:
    """Each trace job's pool, the GPU resource it requests (nvidia, amd) or
    cpu for a job without GPUs, its submit offset, whether it is a
    performance job (intentClass; under A too, whose trace keeps the class
    without the affinity) and the GPUs it requests."""
    pools = {}
    for line in trace_path.read_text().splitlines():
        if '"type":"job"' not in line:
            continue
        j = json.loads(line)
        req = (j.get("podTemplate") or {}).get("requests") or {}
        pool = "nvidia" if "nvidia.com/gpu" in req else "amd" if "amd.com/gpu" in req else "cpu"
        gpus = float(req.get("nvidia.com/gpu") or req.get("amd.com/gpu") or 0)
        pools[j["jobId"]] = (pool, float(j.get("submitTimeOffsetSec") or 0), j.get("intentClass") == "performance", gpus)
    return pools


def gpu_util_targets(trace_path: pathlib.Path) -> dict:
    """Each GPU job's GPU utilization target as the simulator reads it,
    workloadProfile.gpuUtilization within [0, 1], or None when the trace
    gives none (the simulator then picks a default by work class)."""
    out = {}
    for line in trace_path.read_text().splitlines():
        if '"type":"job"' not in line:
            continue
        j = json.loads(line)
        req = (j.get("podTemplate") or {}).get("requests") or {}
        if float(req.get("nvidia.com/gpu") or req.get("amd.com/gpu") or 0) > 0:
            u = (j.get("workloadProfile") or {}).get("gpuUtilization")
            out[j["jobId"]] = None if u is None else min(1.0, max(0.0, float(u)))
    return out


def placed_gpu_demand(jobs: list, gpu_util: dict) -> float:
    """The GPU load of the placed jobs as the simulator's workload tick sums
    it: GPUs x utilization target of every job on a node with GPU work
    left; NaN when such a job has no target."""
    total = 0.0
    for j in jobs:
        if j.get("node") and not j.get("completed") and float(j.get("gpuUnitsRemaining") or 0) > 0:
            u = gpu_util.get(j.get("jobId"))
            total += float(j.get("requestedGpus") or 0) * (float("nan") if u is None else u)
    return total


# A pending job counts as queued, waiting for capacity, after this long; a
# shorter wait is placement: pods are created and bound at the simulator's
# workload ticks and the scheduler's pace.
QUEUED_AFTER_SEC = 30.0
# The gates. Every run: no refused control, the
# polls cover the measured window with no gap above GATE_MAX_GAP_SEC, and
# at the end no node without a job shows a load (idle_load_failures). The setup of the load, checked on baseline A, which
# caps nothing and keeps no job off a node: on average over the measured
# window at most GATE_QUEUED_SHARE of the running pods queued (at least one
# job), and a utilization of the binding resource at regime of at least
# GATE_MIN_UTIL (Matteo: about 50 %, at least 45 %); in a GPU run, that
# utilization also within GATE_UTIL_TOL of the GPU load of the placed jobs
# (placed_gpu_util). Queueing and utilization under B and C are what the
# policies cause: results, not gates. The last two checks came with the
# simulator fix of 2026-10-27: a node used to keep its last job's load, and
# 03's baseline A, seed 1, read a GPU utilization of 0.664 against about
# 0.565 from its placed jobs, with 10 idle nodes at 0.10 to 1.0. Counting
# each job from its submission instead of its placement adds the jobs that
# wait for a node: 0.041 on the next run of that baseline, which has 1.6
# 8-GPU jobs waiting on average. GATE_UTIL_TOL: the polls and the
# simulator's samples read the same jobs, so they differ by sampling only,
# a poll every 10 s against a sample every second: at most 0.0025 on that
# run, against 0.068 or more from a kept load.
GATE_MAX_GAP_SEC = 25.0
GATE_QUEUED_SHARE = 0.01
GATE_MIN_UTIL = 0.45
GATE_UTIL_TOL = 0.03


def idle_load_failures(run_dir: pathlib.Path) -> list[str]:
    """The nodes that end the run with no job but a load, as a failure: a
    GPU utilization above 0, or a CPU utilization above 0 with no pod
    outside the workload (otherPods). The jobs are read before and after the
    nodes, and a node runs a job when one was active on it in either read
    or ended between them, so a job that starts or ends while the nodes are
    read flags no node. A state that cannot be read, or lists no node,
    fails too: the check would pass without looking at a node."""
    try:
        nodes = json.loads((run_dir / "sim_debug_nodes.json").read_text()).get("nodes") or []
        before = json.loads((run_dir / "sim_debug_jobs_before.json").read_text()).get("jobs") or []
        after = json.loads((run_dir / "sim_debug_jobs.json").read_text()).get("jobs") or []
    except (OSError, ValueError, AttributeError) as err:
        return [f"the simulator's node and job state cannot be read: {err}"]
    if not any(n.get("state") for n in nodes):
        return ["the simulator's node state lists no node"]
    ended_before = {j.get("jobId") for j in before if j.get("completed")}
    busy = {j.get("node") for j in before + after if j.get("submitted") and not j.get("completed")}
    busy |= {j.get("node") for j in after if j.get("completed") and j.get("jobId") not in ended_before}
    stale = []
    for n in nodes:
        st = n.get("state")
        if st and n["node"] not in busy and (st.get("gpuUtil") or (st.get("cpuUtil") and not st.get("otherPods"))):
            stale.append(f"{n['node']} GPU {st.get('gpuUtil') or 0:.2f} CPU {st.get('cpuUtil') or 0:.2f}")
    return [f"{len(stale)} nodes with no job at a load above 0: {', '.join(stale[:5])}"] if stale else []


def placed_gpu_util(run_dir: pathlib.Path, rows: list, measure_from: float, window: float):
    """The GPU utilization of the placed jobs over the window: the mean of
    the polls' gpu_demand (work_progress.csv) over all the GPUs of
    nodes.json. None when a GPU job has no utilization target."""
    if any(u is None for u in gpu_util_targets(run_dir / "trace.jsonl").values()):
        return None
    vals = [float(r["gpu_demand"]) for r in rows if measure_from <= float(r["elapsed_sec"]) <= window]
    if any(v != v for v in vals):  # NaN: a placed job without a target
        return None
    items = json.loads((run_dir / "nodes.json").read_text())["items"]
    total = sum(float(n["status"].get("allocatable", {}).get(k) or 0) for n in items for k in ("nvidia.com/gpu", "amd.com/gpu"))
    return sum(vals) / len(vals) / total


def gate_failures(run_dir: pathlib.Path, measure_from: float, window: float, baseline: str = "A") -> list[str]:
    """Why the run in run_dir fails the gates, empty when it passes."""
    import csv
    rows = list(csv.DictReader((run_dir / "work_progress.csv").open()))
    t = [float(r["elapsed_sec"]) for r in rows]
    out = []
    if not t or t[0] > measure_from or t[-1] < window - 1:
        out.append(f"the polls cover {t[0] if t else 'nothing'} to {t[-1] if t else ''} s, not {measure_from:.0f} to {window:.0f} s")
    gaps = [b - a for a, b in zip(t, t[1:])]
    if gaps and max(gaps) > GATE_MAX_GAP_SEC:
        out.append(f"a gap of {max(gaps):.0f} s between polls")
    out += idle_load_failures(run_dir)
    if baseline != "A":
        return out
    ts = list(csv.DictReader((run_dir / "timeseries.csv").open()))
    gpu = any(float((json.loads(l).get("work") or {}).get("gpuUnits") or 0) > 0 for l in (run_dir / "trace.jsonl").open() if '"type":"job"' in l)
    col = "cluster_gpu_util" if gpu else "cluster_cpu_util"
    win = [r for r in ts if measure_from <= float(r["elapsed_sec"]) <= window]
    util = sum(float(r[col]) for r in win) / max(1, len(win))
    if util < GATE_MIN_UTIL:
        out.append(f"{col} at regime {util:.3f} < {GATE_MIN_UTIL}")
    if gpu:
        try:
            demand, why = placed_gpu_util(run_dir, rows, measure_from, window), "a GPU job without workloadProfile.gpuUtilization"
        except (OSError, ValueError, KeyError, IndexError, ZeroDivisionError) as err:
            demand, why = None, f"{type(err).__name__}: {err}"
        if demand is None:
            out.append(f"the placed jobs give no utilization to check: {why}")
        elif abs(util - demand) > GATE_UTIL_TOL:
            out.append(f"{col} at regime {util:.3f}, the placed jobs give {demand:.3f}")
    # The queue over the whole measured window, not one poll: a single poll
    # near the start of the window (run.measure_from_sec) flipped between 1 and 3 queued jobs from one 10 s poll to the
    # next (03, seed 1, baseline A), while the gate is about a sustained
    # backlog.
    queued = [float(r["jobs_queued"]) for r in rows if measure_from <= float(r["elapsed_sec"]) <= window]
    pods = sum(float(r["pods_running"]) for r in win) / max(1, len(win))
    mean_queued = sum(queued) / max(1, len(queued))
    if mean_queued > max(1.0, GATE_QUEUED_SHARE * pods):
        out.append(f"{mean_queued:.1f} jobs queued on average from {measure_from:.0f} to {window:.0f} s, {pods:.0f} running")
    return out


def load_failures(record: dict) -> list[str]:
    """A run whose mean 1-minute host load exceeds the host's CPUs ran on an
    oversubscribed host, whatever its baseline, and stops the sweep to be run
    again. The simulator advances jobs by wall-clock time, so a loaded host
    can reach the measurement; each report gives every run's load."""
    if record.get("host_load1_mean", 0.0) > record.get("host_cpus", float("inf")):
        return [f"mean host load {record['host_load1_mean']:.1f} exceeds the {record['host_cpus']} CPUs"]
    return []


def run_window(duration_sec: int, poll_log_sec: int, progress_path: pathlib.Path, pools: dict | None = None, gpu_util: dict | None = None) -> int:
    """Let the run go for duration_sec, the arrival window, and stop: the
    report measures the cluster at regime, before the drain. Every 10 s it
    reads the simulator's jobs (/debug/jobs) and energy (/debug/energy) and
    writes to progress_path the elapsed seconds, the CPU and GPU work units
    done so far (total minus remaining, over every job), the CPU work of the
    jobs without GPU work, the jobs submitted,
    completed and pending (submitted, on no node yet; in all, per pool of
    pools, a job's GPU resource or cpu, of performance jobs, of jobs asking
    for 8 GPUs or more, which need a whole 8-GPU node, and queued:
    pending for more than QUEUED_AFTER_SEC since its submit offset), and the
    cluster's
    cumulative IT energy, all on one clock; each node's cumulative energy
    goes, one JSON line per poll, to energy_by_node.jsonl beside it. Returns the
    number of rows written."""
    log(f"running for {duration_sec} s, the arrival window")
    start = time.time()
    last_log = 0.0
    rows = 0
    with progress_path.open("w") as f:
        f.write("elapsed_sec,cpu_units_done,gpu_units_done,cpu_only_units_done,jobs_submitted,jobs_completed,jobs_pending,jobs_queued,pending_nvidia,pending_amd,pending_cpu,pending_perf,pending_8gpu,energy_j,gpu_demand\n")
        by_node = progress_path.with_name("energy_by_node.jsonl").open("w")
        while True:
            elapsed = time.time() - start
            try:
                jobs = json.loads(fetch_via_port_forward("/debug/jobs", 30)).get("jobs") or []
                ereply = json.loads(fetch_via_port_forward("/debug/energy", 30))
                energy = float(ereply.get("totalJoules") or 0)
            except Exception as e:  # a failed fetch is retried at the next poll
                log(f"warning: /debug/jobs or /debug/energy fetch failed: {e}")
                jobs = None
            LOAD_SAMPLES.append(host_load())
            if jobs is not None:
                cpu = sum(float(j.get("cpuUnitsTotal") or 0) - float(j.get("cpuUnitsRemaining") or 0) for j in jobs)
                gpu = sum(float(j.get("gpuUnitsTotal") or 0) - float(j.get("gpuUnitsRemaining") or 0) for j in jobs)
                cpu_only = sum(float(j.get("cpuUnitsTotal") or 0) - float(j.get("cpuUnitsRemaining") or 0) for j in jobs if not float(j.get("gpuUnitsTotal") or 0))
                sub = sum(1 for j in jobs if j.get("submitted"))
                done = sum(1 for j in jobs if j.get("completed"))
                waiting = [j for j in jobs if j.get("submitted") and not j.get("completed") and not j.get("node")]
                info = [(pools or {}).get(j.get("jobId"), ("", elapsed, False, 0.0)) for j in waiting]
                by_pool = {p: sum(1 for pool, _, _, _ in info if pool == p) for p in ("nvidia", "amd", "cpu")}
                queued = sum(1 for _, off, _, _ in info if elapsed - off > QUEUED_AFTER_SEC)
                perf = sum(1 for _, _, x, _ in info if x)
                whole = sum(1 for _, _, _, g in info if g >= 8)
                f.write(f"{elapsed:.1f},{cpu:.3f},{gpu:.3f},{cpu_only:.3f},{sub},{done},{len(waiting)},{queued},{by_pool['nvidia']},{by_pool['amd']},{by_pool['cpu']},{perf},{whole},{energy:.1f},{placed_gpu_demand(jobs, gpu_util or {}):.3f}\n")
                f.flush()
                by_node.write(json.dumps({"elapsed_sec": round(elapsed, 1), "byNodeJoules": ereply.get("byNodeJoules") or {}}) + "\n")
                by_node.flush()
                rows += 1
                if elapsed - last_log >= poll_log_sec:
                    log(f"progress elapsed={int(elapsed)}s submitted={sub} completed={done}")
                    last_log = elapsed
            if elapsed >= duration_sec:
                return rows
            time.sleep(min(10.0, max(0.0, duration_sec - (time.time() - start))))


# SIM_PORT is the simulator's HTTP port in its pod.
SIM_PORT = 18080


def forwarded_port(line: str):
    """The local port of a kubectl port-forward "Forwarding from" line, or None."""
    m = re.match(r"Forwarding from 127\.0\.0\.1:(\d+) -> %d" % SIM_PORT, line.strip())
    return int(m.group(1)) if m else None


def fetch_via_port_forward(endpoint: str, timeout_sec: int) -> str:
    """GET endpoint from the simulator through a port-forward on a local port
    kubectl picks. A fixed local port fails whenever another process holds
    it, and every debug artifact of the run then came back empty."""
    pf = subprocess.Popen(
        ["kubectl", "-n", "joulie-sim-demo", "port-forward", "deploy/joulie-telemetry-sim", f":{SIM_PORT}"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    try:
        port = forwarded_port(pf.stdout.readline())
        if port is None:
            pf.terminate()
            raise RuntimeError("kubectl port-forward failed: " + (pf.communicate(timeout=5)[1].strip() or "no Forwarding line"))
        return urlopen(f"http://127.0.0.1:{port}{endpoint}", timeout=timeout_sec).read().decode()
    finally:
        pf.terminate()
        try:
            pf.wait(timeout=5)
        except Exception:
            pf.kill()


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
            except Exception as err:
                log(f"warning: {endpoint}: {err}")
            time.sleep(1)
        payload = fetch_via_exec(filename)
        if valid_debug_payload(payload):
            return payload
        return "{}"

    def mirror_persisted_file(filename: str):
        payload = fetch_via_exec(filename)
        if payload:
            (host_persist_dir / filename).write_text(payload)

    # The jobs before and after the nodes: idle_load_failures reads a node
    # as running a job when one was active on it at either read.
    jobs_before = fetch_debug_artifact("/debug/jobs", "jobs.json")
    nodes = fetch_debug_artifact("/debug/nodes", "nodes.json")
    jobs = fetch_debug_artifact("/debug/jobs", "jobs.json")
    events = fetch_debug_artifact("/debug/events", "events.json")
    energy = fetch_debug_artifact("/debug/energy", "energy.json")

    (run_dir / "sim_debug_nodes.json").write_text(nodes)
    (run_dir / "sim_debug_jobs_before.json").write_text(jobs_before)
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

    (run_dir / "controller-manager.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "deploy/joulie-controller-manager", "--tail=-1"], capture=True, check=False).stdout)
    (run_dir / "agent.log").write_text(run(["kubectl", "-n", "joulie-system", "logs", "statefulset/joulie-agent-pool", "--tail=-1"], capture=True, check=False).stdout)
    (run_dir / "simulator.log").write_text(run(["kubectl", "-n", "joulie-sim-demo", "logs", "deploy/joulie-telemetry-sim", "--tail=-1"], capture=True).stdout)
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
    rows = run_window(args.timeout, args.poll_log_sec, run_dir / "work_progress.csv", job_pools(trace_path), gpu_util_targets(trace_path))
    refused = control_errors()
    (run_dir / "completion.json").write_text(json.dumps({
        "window_seconds": args.timeout, "stopped_after_seconds": time.time() - start_ts, "progress_rows": rows,
        "control_errors": refused, "host_cpus": os.cpu_count(),
        "host_load1_mean": sum(LOAD_SAMPLES) / max(1, len(LOAD_SAMPLES)), "host_load1_max": max(LOAD_SAMPLES, default=0.0),
    }, indent=2) + "\n")

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
    run_cfg = {}
    if args.benchmark_config.strip():
        import yaml
        run_cfg = (yaml.safe_load(open(args.benchmark_config)) or {}).get("run") or {}
    record = json.loads((run_dir / "completion.json").read_text())
    failures = gate_failures(run_dir, float(run_cfg.get("measure_from_sec", 0)), float(args.timeout), args.baseline) + load_failures(record)
    record["gate_failures"] = failures
    (run_dir / "completion.json").write_text(json.dumps(record, indent=2) + "\n")
    if failures:
        raise SystemExit(f"run {run_id} fails the gates: " + "; ".join(failures))
    if refused:
        raise SystemExit(f"run {run_id}: the simulator refused {refused} control commands (joulie_sim_control_actions_total result=error)")
    if not rows:
        raise SystemExit(f"run {run_id}: no progress sample from the simulator")

if __name__ == "__main__":
    main()
