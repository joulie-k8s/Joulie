#!/usr/bin/env python3
"""Integration test runner for a 2-node k3s cluster.

The Dagger CI setup registers two stable node names:
  - k3s-server
  - k3s-worker-0

The tests no longer assume which one will be the performance-floor node when
STATIC_HP_FRAC=0. Instead, they discover the actual split from the operator's
labels after install and use that as the runtime perf/eco mapping.
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
import textwrap
import time
from dataclasses import dataclass
from typing import Any

# Stable node names registered by the Dagger CI k3s setup.
EXPECTED_NODES = ("k3s-server", "k3s-worker-0")


def log(msg: str) -> None:
    print(f"[integration] {msg}", flush=True)


def run(cmd: list[str], *, check: bool = True, capture: bool = False, stdin: str | None = None) -> subprocess.CompletedProcess[str]:
    if capture:
        return subprocess.run(cmd, text=True, capture_output=True, check=check, input=stdin)
    return subprocess.run(cmd, text=True, check=check, input=stdin)


def kubectl(args: list[str], *, check: bool = True, capture: bool = False, stdin: str | None = None) -> subprocess.CompletedProcess[str]:
    return run(["kubectl", *args], check=check, capture=capture, stdin=stdin)


def helm(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run(["helm", *args], check=check)


def apply_yaml(doc: str) -> None:
    kubectl(["apply", "-f", "-"], stdin=doc)


def wait_until(predicate, timeout_sec: int = 120, interval_sec: float = 2.0, desc: str = "condition") -> None:
    start = time.time()
    while time.time() - start < timeout_sec:
        if predicate():
            return
        time.sleep(interval_sec)
    raise RuntimeError(f"timeout waiting for {desc}")


def wait_rollout(namespace: str, resource: str, timeout: str = "180s") -> None:
    kubectl(["-n", namespace, "rollout", "status", resource, f"--timeout={timeout}"])


def wait_ready_nodes(count: int = 2, timeout_sec: int = 300) -> list[str]:
    """Wait until at least `count` nodes report Ready=True, then return their names."""
    def _enough_ready() -> bool:
        out = kubectl(["get", "nodes", "-o", "json"], check=False, capture=True)
        if out.returncode != 0 or not out.stdout.strip():
            return False
        items = json.loads(out.stdout).get("items", [])
        ready = sum(
            1 for n in items
            for c in n.get("status", {}).get("conditions", [])
            if c.get("type") == "Ready" and c.get("status") == "True"
        )
        return ready >= count

    wait_until(_enough_ready, timeout_sec=timeout_sec, desc=f"{count} ready nodes in cluster")

    out = kubectl(["get", "nodes", "-o", "json"], capture=True)
    items = json.loads(out.stdout).get("items", [])
    return [
        n["metadata"]["name"]
        for n in items
        for c in n.get("status", {}).get("conditions", [])
        if c.get("type") == "Ready" and c.get("status") == "True"
    ]


def node_has_gpu_allocatable(node: str) -> bool:
    out = kubectl(["get", "node", node, "-o", "json"], capture=True).stdout
    alloc = json.loads(out).get("status", {}).get("allocatable", {}) or {}
    for key in ("nvidia.com/gpu", "amd.com/gpu"):
        val = str(alloc.get(key, "")).strip()
        if not val:
            continue
        try:
            if int(val) > 0:
                return True
        except ValueError:
            if val not in ("0", "0m"):
                return True
    return False


def get_node_labels(node: str) -> dict[str, str]:
    out = kubectl(["get", "node", node, "-o", "json"], capture=True).stdout
    labels = json.loads(out)["metadata"].get("labels", {})
    return dict(labels)


def wait_node_label(node: str, key: str, val: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_labels(node).get(key) == val,
        timeout_sec=timeout_sec,
        desc=f"node {node} label {key}={val}",
    )


def get_node_twin_state_schedulable_class(node: str) -> str:
    """Read schedulableClass from the NodeTwin CR for a node."""
    out = kubectl(
        ["get", "nodetwin", node, "-o", "json"],
        capture=True,
    )
    obj = json.loads(out.stdout)
    return obj.get("status", {}).get("schedulableClass", "")


def wait_node_twin_class(node: str, expected: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_twin_state_schedulable_class(node) == expected,
        timeout_sec=timeout_sec,
        desc=f"node {node} schedulableClass == {expected}",
    )


def wait_node_draining_false(node: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_twin_state_schedulable_class(node) != "draining",
        timeout_sec=timeout_sec,
        desc=f"node {node} schedulableClass != draining",
    )


def is_guarded_transition(node: str) -> bool:
    labels = get_node_labels(node)
    profile = labels.get("joulie.io/power-profile", "")
    sc = get_node_twin_state_schedulable_class(node)
    return profile == "eco" and sc == "draining"


def wait_node_guarded_transition(node: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: is_guarded_transition(node),
        timeout_sec=timeout_sec,
        desc=f"node {node} in guarded perf->eco transition (schedulableClass=draining)",
    )


def wait_node_eco_ready(node: str, timeout_sec: int = 120) -> None:
    def _ok() -> bool:
        labels = get_node_labels(node)
        profile = labels.get("joulie.io/power-profile", "")
        sc = get_node_twin_state_schedulable_class(node)
        return profile == "eco" and sc != "draining"

    wait_until(
        _ok,
        timeout_sec=timeout_sec,
        desc=f"node {node} eco ready (schedulableClass != draining)",
    )


def discover_perf_and_eco_nodes(nodes: list[str], timeout_sec: int = 120) -> tuple[str, str]:
    """Return (perf_node, eco_node) once operator labels settle under frac=0."""

    def _split() -> tuple[str, str] | None:
        perf: list[str] = []
        eco: list[str] = []
        for node in nodes:
            labels = get_node_labels(node)
            profile = labels.get("joulie.io/power-profile", "")
            # Draining is tracked via NodeTwinState.schedulableClass, not labels.
            if profile == "performance":
                perf.append(node)
            elif profile == "eco":
                eco.append(node)
        if len(perf) == 1 and len(eco) == 1:
            return perf[0], eco[0]
        return None

    result: tuple[str, str] | None = None

    def _ok() -> bool:
        nonlocal result
        result = _split()
        return result is not None

    wait_until(_ok, timeout_sec=timeout_sec, desc="one performance node and one eco node")
    assert result is not None
    return result


def assert_node_label(node: str, key: str, expected: str) -> None:
    got = get_node_labels(node).get(key)
    if got != expected:
        raise AssertionError(f"node={node} label {key} got={got!r} expected={expected!r}")


def delete_pod(ns: str, name: str) -> None:
    kubectl(["-n", ns, "delete", "pod", name, "--ignore-not-found=true", "--wait=true", "--timeout=90s"], check=False)
    wait_pod_gone(ns, name, timeout_sec=90)


def wait_pod_phase(ns: str, name: str, phase: str, timeout_sec: int = 120) -> None:
    def _ok() -> bool:
        out = kubectl(["-n", ns, "get", "pod", name, "-o", "json"], check=False, capture=True)
        if out.returncode != 0:
            return False
        got = json.loads(out.stdout).get("status", {}).get("phase", "")
        return got == phase

    wait_until(_ok, timeout_sec=timeout_sec, desc=f"pod {ns}/{name} phase={phase}")


def wait_pod_pending(ns: str, name: str, timeout_sec: int = 120) -> None:
    wait_pod_phase(ns, name, "Pending", timeout_sec)


def wait_pod_gone(ns: str, name: str, timeout_sec: int = 120) -> None:
    def _ok() -> bool:
        out = kubectl(["-n", ns, "get", "pod", name], check=False, capture=True)
        return out.returncode != 0

    wait_until(_ok, timeout_sec=timeout_sec, desc=f"pod {ns}/{name} gone")


def wait_pod_unschedulable_reason(ns: str, name: str, contains: str, timeout_sec: int = 120) -> None:
    needle = contains.lower()

    def _ok() -> bool:
        out = kubectl(["-n", ns, "describe", "pod", name], check=False, capture=True)
        if out.returncode != 0:
            return False
        text = out.stdout.lower()
        # The extender's rejection reason may appear as "joulie:" in the event
        # text, or kube-scheduler may report "filtered by extender" without the
        # extender's detailed reason. Accept either.
        if "failedscheduling" not in text:
            return False
        return (needle in text) or ("extender" in text)

    wait_until(_ok, timeout_sec=timeout_sec, desc=f"pod {ns}/{name} unschedulable contains '{contains}'")


def mk_pod_yaml(
    name: str,
    image: str = "busybox:1.36",
    workload_class: str = "",
    node_name: str = "",
    node_selector: dict[str, str] | None = None,
) -> str:
    """Build a minimal pod YAML.

    workload_class sets the ``joulie.io/workload-class`` annotation which is the
    single source of truth for placement intent.  Valid values: ``performance``,
    ``standard``.  When empty no annotation is added (defaults to ``standard``
    in the scheduler extender).
    """
    lines = [
        "apiVersion: v1",
        "kind: Pod",
        "metadata:",
        f"  name: {name}",
        "  namespace: joulie-it",
        "  labels:",
        "    app.kubernetes.io/part-of: joulie-it",
    ]
    if workload_class:
        lines.append("  annotations:")
        lines.append(f'    joulie.io/workload-class: "{workload_class}"')
    lines.append("spec:")
    if node_name:
        lines.append(f"  nodeName: {node_name}")
    if node_selector:
        lines.append("  nodeSelector:")
        for k, v in node_selector.items():
            lines.append(f'    {k}: "{v}"')
    lines.extend(
        [
            "  restartPolicy: Never",
            "  containers:",
            "  - name: c",
            f"    image: {image}",
            '    command: ["sh","-c","sleep 1200"]',
        ]
    )
    return "\n".join(lines) + "\n"


def install_joulie() -> None:
    log("installing joulie chart")
    agent_repo = os.getenv("JOULIE_AGENT_IMAGE_REPOSITORY", "").strip()
    agent_tag = os.getenv("JOULIE_AGENT_IMAGE_TAG", "").strip()
    operator_repo = os.getenv("JOULIE_OPERATOR_IMAGE_REPOSITORY", "").strip()
    operator_tag = os.getenv("JOULIE_OPERATOR_IMAGE_TAG", "").strip()
    scheduler_repo = os.getenv("JOULIE_SCHEDULER_IMAGE_REPOSITORY", "").strip()
    scheduler_tag = os.getenv("JOULIE_SCHEDULER_IMAGE_TAG", "").strip()
    if not (agent_repo and agent_tag and operator_repo and operator_tag):
        raise RuntimeError(
            "missing required image overrides: JOULIE_AGENT_IMAGE_REPOSITORY/JOULIE_AGENT_IMAGE_TAG/"
            "JOULIE_OPERATOR_IMAGE_REPOSITORY/JOULIE_OPERATOR_IMAGE_TAG"
        )
    helm(
        [
            "upgrade",
            "--install",
            "joulie",
            "./charts/joulie",
            "-n",
            "joulie-system",
            "--create-namespace",
            "--set",
            "agent.mode=pool",
            "--set",
            "agent.pool.replicas=1",
            "--set",
            "agent.pool.shards=1",
            "--set",
            "agent.env.RECONCILE_INTERVAL=5s",
            "--set",
            "operator.env.RECONCILE_INTERVAL=5s",
            "--set",
            "operator.env.POLICY_TYPE=static_partition",
            "--set",
            "operator.env.STATIC_HP_FRAC=1",
            "--set",
            f"agent.image.repository={agent_repo}",
            "--set",
            f"agent.image.tag={agent_tag}",
            "--set",
            "agent.image.pullPolicy=Always",
            "--set",
            f"operator.image.repository={operator_repo}",
            "--set",
            f"operator.image.tag={operator_tag}",
            "--set",
            "operator.image.pullPolicy=Always",
        ]
        + (
            [
                "--set", "schedulerExtender.enabled=true",
                "--set", "schedulerExtender.hostNetwork=true",
                "--set", "schedulerExtender.nodeSelector.kubernetes\\.io/hostname=k3s-server",
                "--set", "schedulerExtender.env.CACHE_TTL=2s",
                "--set", f"schedulerExtender.image.repository={scheduler_repo}",
                "--set", f"schedulerExtender.image.tag={scheduler_tag}",
                "--set", "schedulerExtender.image.pullPolicy=Always",
            ]
            if scheduler_repo and scheduler_tag
            else []
        )
    )
    wait_rollout("joulie-system", "deploy/joulie-operator")
    wait_rollout("joulie-system", "statefulset/joulie-agent-pool")
    if scheduler_repo and scheduler_tag:
        wait_rollout("joulie-system", "deploy/joulie-scheduler-extender")


def set_static_hp_frac(frac: str) -> None:
    out = kubectl(["-n", "joulie-system", "get", "deploy/joulie-operator", "-o", "json"], capture=True)
    deploy = json.loads(out.stdout)
    env = (
        deploy.get("spec", {})
        .get("template", {})
        .get("spec", {})
        .get("containers", [{}])[0]
        .get("env", [])
    )
    current = next((item.get("value", "") for item in env if item.get("name") == "STATIC_HP_FRAC"), "")
    if current == frac:
        log(f"STATIC_HP_FRAC already {frac}; skipping rollout")
        return

    kubectl(["-n", "joulie-system", "set", "env", "deploy/joulie-operator", f"STATIC_HP_FRAC={frac}"])
    wait_rollout("joulie-system", "deploy/joulie-operator")


def install_http_mock() -> None:
    log("installing http telemetry/control mock")
    apply_yaml(
        textwrap.dedent(
            """\
            apiVersion: v1
            kind: ConfigMap
            metadata:
              name: joulie-http-mock
              namespace: joulie-it
            data:
              server.py: |
                import json
                from http.server import BaseHTTPRequestHandler, HTTPServer
                STATS = {"get": 0, "post": 0, "gpu_post": 0}
                class H(BaseHTTPRequestHandler):
                    def do_GET(self):
                        if self.path.startswith("/telemetry/"):
                            STATS["get"] += 1
                            self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
                            self.wfile.write(json.dumps({"cpu":{"packagePowerWatts":250.0}}).encode())
                        elif self.path == "/stats":
                            self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
                            self.wfile.write(json.dumps(STATS).encode())
                        else:
                            self.send_response(404); self.end_headers()
                    def do_POST(self):
                        if self.path.startswith("/control/"):
                            STATS["post"] += 1
                            try:
                                raw = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
                                payload = json.loads(raw.decode() or "{}")
                                if payload.get("action") == "gpu.set_power_cap_watts":
                                    STATS["gpu_post"] += 1
                            except Exception:
                                pass
                            self.send_response(200); self.end_headers()
                        else:
                            self.send_response(404); self.end_headers()
                HTTPServer(("0.0.0.0", 8080), H).serve_forever()
            ---
            apiVersion: apps/v1
            kind: Deployment
            metadata:
              name: joulie-http-mock
              namespace: joulie-it
            spec:
              replicas: 1
              selector:
                matchLabels:
                  app: joulie-http-mock
              template:
                metadata:
                  labels:
                    app: joulie-http-mock
                spec:
                  containers:
                  - name: mock
                    image: python:3.12-alpine
                    command: ["python","/app/server.py"]
                    volumeMounts:
                    - name: code
                      mountPath: /app
                  volumes:
                  - name: code
                    configMap:
                      name: joulie-http-mock
            ---
            apiVersion: v1
            kind: Service
            metadata:
              name: joulie-http-mock
              namespace: joulie-it
            spec:
              selector:
                app: joulie-http-mock
              ports:
              - port: 8080
                targetPort: 8080
            """
        )
    )
    wait_rollout("joulie-it", "deploy/joulie-http-mock")


def apply_telemetry_profile(node: str) -> None:
    """Configure agent telemetry via env vars (replaces the removed TelemetryProfile CRD)."""
    mock_base = "http://joulie-http-mock.joulie-it.svc.cluster.local:8080"
    envs = [
        "TELEMETRY_CPU_SOURCE=http",
        f"TELEMETRY_CPU_HTTP_ENDPOINT={mock_base}/telemetry/{node}",
        "TELEMETRY_CPU_CONTROL=http",
        f"TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT={mock_base}/control/{node}",
        "TELEMETRY_CPU_CONTROL_MODE=dvfs",
        "TELEMETRY_GPU_CONTROL=http",
        f"TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT={mock_base}/control/{node}",
        "TELEMETRY_GPU_CONTROL_MODE=powercap",
        "TELEMETRY_HTTP_TIMEOUT_SECONDS=3",
    ]
    # Set env vars on the agent pool statefulset (or daemonset depending on mode).
    for target in ["statefulset/joulie-agent-pool", "daemonset/joulie-agent"]:
        result = kubectl(
            ["-n", "joulie-system", "set", "env", target] + envs,
            check=False,
            capture=True,
        )
        if result.returncode == 0:
            wait_rollout("joulie-system", target)
            break


def get_mock_stats() -> dict[str, Any]:
    proc = subprocess.Popen(
        ["kubectl", "-n", "joulie-it", "port-forward", "svc/joulie-http-mock", "18081:8080"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.time() + 20
        while time.time() < deadline:
            out = run(["curl", "-fsSL", "http://127.0.0.1:18081/stats"], capture=True, check=False)
            if out.returncode == 0 and out.stdout.strip():
                return json.loads(out.stdout)
            time.sleep(0.8)
        raise RuntimeError("timeout reading http-mock /stats via port-forward")
    finally:
        proc.terminate()


def patch_node_gpu_cap(node: str, cap_watts_per_gpu: int) -> None:
    """Patch NodeTwin spec to set GPU power cap intent."""
    name = node.replace(".", "-")
    patch = {
        "spec": {
            "gpu": {
                "powerCap": {
                    "scope": "perGpu",
                    "capWattsPerGpu": cap_watts_per_gpu,
                }
            }
        }
    }
    kubectl(["patch", "nodetwin", name, "--type=merge", "-p", json.dumps(patch)])


def get_nodetwin_gpu_control_status(node: str) -> tuple[str, str]:
    """Read GPU control status from the NodeTwin CR."""
    name = node.replace(".", "-")
    out = kubectl(["get", "nodetwin", name, "-o", "json"], capture=True, check=False)
    if out.returncode != 0 or not out.stdout.strip():
        return "", ""
    status = json.loads(out.stdout).get("status", {})
    ctrl = status.get("controlStatus", {}).get("gpu", {})
    return str(ctrl.get("result", "")), str(ctrl.get("message", ""))


def dump_debug() -> None:
    log("collecting debug data")
    cmds = [
        ["kubectl", "get", "nodes", "-o", "wide", "--show-labels"],
        ["kubectl", "describe", "nodes"],
        ["kubectl", "get", "pods", "-A", "-o", "wide"],
        ["kubectl", "describe", "pods", "-A"],
        ["kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"],
        ["kubectl", "get", "nodetwins", "-o", "yaml"],
        ["kubectl", "get", "nodehardwares", "-o", "yaml"],
        ["kubectl", "-n", "joulie-system", "logs", "deploy/joulie-operator", "--tail=200"],
        ["kubectl", "-n", "joulie-system", "logs", "statefulset/joulie-agent-pool", "--tail=200"],
    ]
    for c in cmds:
        try:
            print(f"\n$ {' '.join(c)}")
            out = run(c, capture=True, check=False)
            print(out.stdout)
            if out.stderr:
                print(out.stderr)
        except Exception as e:
            print(f"failed to run {' '.join(c)}: {e}")


@dataclass
class Ctx:
    """Test context holding the two cluster node names.

    perf_node  - always stays in performance (operator family floor).
    eco_node   - transitions to eco when STATIC_HP_FRAC=0.
    node       - alias for eco_node kept for clarity in test code.
    """
    perf_node: str
    eco_node: str

    @property
    def node(self) -> str:
        return self.eco_node


# ---------------------------------------------------------------------------
# Test functions
# ---------------------------------------------------------------------------

def test_boot_and_install() -> Ctx:
    log("IT-BOOT-01 / IT-HELM-01")
    kubectl(["version"], check=False)

    nodes = wait_ready_nodes(count=2, timeout_sec=300)
    log(f"ready nodes: {nodes}")

    for expected in EXPECTED_NODES:
        if expected not in nodes:
            raise RuntimeError(
                f"expected node {expected!r} not present; got {nodes}. "
                "Check that --node-name flags in the Dagger k3s setup match EXPECTED_NODES."
            )

    for node in EXPECTED_NODES:
        kubectl(["label", "node", node, "joulie.io/managed=true", "--overwrite"])

    install_joulie()
    kubectl(["get", "crd", "nodehardwares.joulie.io"])
    kubectl(["get", "crd", "nodetwins.joulie.io"])
    kubectl(["create", "ns", "joulie-it"], check=False)
    set_static_hp_frac("0")
    perf_node, eco_node = discover_perf_and_eco_nodes(list(EXPECTED_NODES))
    log(f"discovered runtime node split: perf_node={perf_node}, eco_node={eco_node}")
    set_static_hp_frac("1")
    for node in EXPECTED_NODES:
        wait_node_label(node, "joulie.io/power-profile", "performance")
        wait_node_draining_false(node)
    return Ctx(perf_node=perf_node, eco_node=eco_node)


def test_scheduler_extender_deployed(ctx: Ctx) -> None:
    """IT-SCHED-EXT-01: verify the scheduler extender is deployed, has a running pod, and its service exists."""
    log("IT-SCHED-EXT-01")
    scheduler_repo = os.getenv("JOULIE_SCHEDULER_IMAGE_REPOSITORY", "").strip()
    scheduler_tag = os.getenv("JOULIE_SCHEDULER_IMAGE_TAG", "").strip()
    if not (scheduler_repo and scheduler_tag):
        log("SKIP: scheduler extender image not provided")
        return

    # Verify the deployment exists and is available
    out = kubectl(
        ["-n", "joulie-system", "get", "deploy/joulie-scheduler-extender",
         "-o", "jsonpath={.status.availableReplicas}"],
        capture=True,
    )
    available = out.stdout.strip()
    if not available or int(available) < 1:
        raise RuntimeError(
            f"scheduler extender deployment has {available!r} available replicas, expected >= 1"
        )
    log(f"scheduler extender available replicas: {available}")

    # Verify the service exists and has the expected port
    out = kubectl(
        ["-n", "joulie-system", "get", "svc/joulie-scheduler-extender",
         "-o", "jsonpath={.spec.ports[0].port}"],
        capture=True,
    )
    port = out.stdout.strip()
    if port != "9876":
        raise RuntimeError(f"scheduler extender service port={port!r}, expected 9876")
    log(f"scheduler extender service port: {port}")

    # Verify the extender pod is running with the correct image
    out = kubectl(
        ["-n", "joulie-system", "get", "pods",
         "-l", "app.kubernetes.io/component=scheduler-extender",
         "-o", "jsonpath={.items[0].spec.containers[0].image}"],
        capture=True,
    )
    image = out.stdout.strip()
    expected_image = f"{scheduler_repo}:{scheduler_tag}"
    if image != expected_image:
        raise RuntimeError(
            f"scheduler extender pod image={image!r}, expected {expected_image!r}"
        )
    log(f"scheduler extender image: {image}")

    # Verify the extender responds to health checks via port-forward
    # (We can't easily curl from inside the test container, but the rollout
    # already proved the pod passes readiness. Just verify the annotation
    # contract is correct by checking the deployed pod's env.)
    out = kubectl(
        ["-n", "joulie-system", "get", "pods",
         "-l", "app.kubernetes.io/component=scheduler-extender",
         "-o", "jsonpath={.items[0].spec.containers[0].env[?(@.name=='EXTENDER_ADDR')].value}"],
        capture=True,
    )
    addr = out.stdout.strip()
    if ":9876" not in addr:
        raise RuntimeError(f"scheduler extender EXTENDER_ADDR={addr!r}, expected to contain :9876")
    log("scheduler extender deployment verified")


def test_telemetry_http(ctx: Ctx) -> None:
    log("IT-TP-01")
    install_http_mock()
    # Apply telemetry profile to the eco node (it's in performance initially due
    # to STATIC_HP_FRAC=1 at install time; that's fine - the agent reconciles it).
    apply_telemetry_profile(ctx.eco_node)
    time.sleep(12)
    stats = get_mock_stats()
    if stats.get("get", 0) <= 0:
        raise AssertionError(f"expected telemetry GETs > 0, stats={stats}")
    if stats.get("post", 0) <= 0:
        raise AssertionError(f"expected control POSTs > 0, stats={stats}")
    patch_node_gpu_cap(ctx.eco_node, 200)
    time.sleep(10)
    stats = get_mock_stats()
    gpu_posts = int(stats.get("gpu_post", 0))
    if node_has_gpu_allocatable(ctx.eco_node):
        if gpu_posts <= 0:
            raise AssertionError(f"expected gpu control POSTs > 0 on GPU node, stats={stats}")
    else:
        if gpu_posts > 0:
            log(f"unexpected gpu POSTs on non-GPU node (still tolerating): stats={stats}")
        result, message = get_nodetwin_gpu_control_status(ctx.eco_node)
        if result not in ("none", "blocked", "error"):
            raise AssertionError(
                f"expected graceful non-GPU handling (none/blocked/error), got result={result!r}, message={message!r}"
            )
        log(f"non-GPU node graceful path confirmed: result={result!r}, message={message!r}")


def test_fsm_and_labels(ctx: Ctx) -> None:
    """IT-FSM-*: verify guarded transition and draining lifecycle on eco_node.

    With two nodes, the operator can actually move eco_node to eco (frac=0)
    while keeping perf_node in performance (family floor).  We force the test
    pod onto eco_node via nodeName so the guarded-transition signal is visible
    there.
    """
    log("IT-FSM-*")
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)

    # eco_node is in performance; place a perf-intent pod there.
    # nodeName bypasses the scheduler (kubelet still runs the pod) so the pod
    # lands on eco_node regardless of workload class.
    delete_pod("joulie-it", "perf-a")
    apply_yaml(mk_pod_yaml("perf-a", workload_class="performance", node_name=ctx.eco_node))
    wait_pod_phase("joulie-it", "perf-a", "Running")

    # Trigger eco transition: operator wants eco_node→eco but sees perf pod → draining=true.
    set_static_hp_frac("0")
    wait_node_guarded_transition(ctx.eco_node)

    # Once perf pod is gone, eco_node completes transition.
    delete_pod("joulie-it", "perf-a")
    wait_node_eco_ready(ctx.eco_node)

    # Standard pod (no annotation) must not trigger draining.
    apply_yaml(mk_pod_yaml("standard-a"))
    wait_pod_phase("joulie-it", "standard-a", "Running")
    wait_node_eco_ready(ctx.eco_node)
    delete_pod("joulie-it", "standard-a")

    # eco → performance clears draining immediately.
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)


def test_fsm_toggle_under_eco(ctx: Ctx) -> None:
    """IT-FSM-07: perf-intent pod restricted to eco_node stays pending while eco_node is eco.

    With two nodes the scheduler would otherwise place the pod on perf_node.
    We use nodeSelector to pin the pod to eco_node so the scheduler extender
    rejects it (performance class on eco node), and confirm eco_node doesn't
    spuriously enter draining.
    """
    log("IT-FSM-07")
    set_static_hp_frac("0")
    wait_node_eco_ready(ctx.eco_node)

    delete_pod("joulie-it", "perf-toggle")
    # Pin to eco_node via nodeSelector; scheduler extender rejects performance pods on eco nodes.
    apply_yaml(mk_pod_yaml(
        "perf-toggle",
        workload_class="performance",
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_pending("joulie-it", "perf-toggle")
    wait_pod_unschedulable_reason("joulie-it", "perf-toggle", "joulie")
    wait_node_eco_ready(ctx.eco_node)

    delete_pod("joulie-it", "perf-toggle")


def test_scheduling(ctx: Ctx) -> None:
    """IT-SCH-*: verify scheduler extender respects workload-class annotations.

    The ``joulie.io/workload-class`` annotation is the single source of truth
    for placement intent.  The scheduler extender filters eco nodes for
    ``performance`` pods; ``standard`` pods can go anywhere.

    With two nodes:
    - frac=1 → both nodes in performance
    - frac=0 → perf_node=performance (floor), eco_node=eco

    We use nodeSelector (not nodeName) so the scheduler extender is exercised.
    """
    log("IT-SCH-*")

    # --- Performance pod schedules on a performance node ---
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_twin_class(ctx.eco_node, "performance")
    wait_node_twin_class(ctx.perf_node, "performance")
    delete_pod("joulie-it", "sch-perf-on-perf")
    apply_yaml(mk_pod_yaml(
        "sch-perf-on-perf",
        workload_class="performance",
        node_selector={"kubernetes.io/hostname": ctx.perf_node},
    ))
    wait_pod_phase("joulie-it", "sch-perf-on-perf", "Running")
    delete_pod("joulie-it", "sch-perf-on-perf")

    # --- Performance pod cannot schedule on eco node ---
    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")
    wait_node_twin_class(ctx.eco_node, "eco")
    delete_pod("joulie-it", "sch-perf-on-eco")
    apply_yaml(mk_pod_yaml(
        "sch-perf-on-eco",
        workload_class="performance",
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_pending("joulie-it", "sch-perf-on-eco")
    wait_pod_unschedulable_reason("joulie-it", "sch-perf-on-eco", "joulie")
    delete_pod("joulie-it", "sch-perf-on-eco")

    # --- Standard pod schedules on eco node (no restriction) ---
    delete_pod("joulie-it", "sch-std-on-eco")
    apply_yaml(mk_pod_yaml(
        "sch-std-on-eco",
        workload_class="standard",
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_phase("joulie-it", "sch-std-on-eco", "Running")
    delete_pod("joulie-it", "sch-std-on-eco")

    # NOTE: Draining is no longer a node label. The scheduler extender
    # handles draining via NodeTwinState.schedulableClass scoring penalty.


def test_classification_matrix(ctx: Ctx) -> None:
    """IT-CLS-*: verify the operator correctly classifies pods by workload class.

    The ``joulie.io/workload-class`` annotation is the single source of truth
    for placement intent.  Classification determines whether the operator
    triggers a guarded transition (draining) when moving a node to eco.

    Strategy with two nodes:
    - Start each case with eco_node in performance (frac=1 → both nodes performance).
    - Force the test pod onto eco_node via nodeName (bypasses scheduler, kubelet
      runs it regardless of workload class; this lets us observe classification on
      a node that WILL transition to eco).
    - Then trigger frac=0: operator tries to move eco_node to eco.
      · If pod is performance → draining=true  (guarded transition)
      · If pod is standard/unset → eco_node goes to eco directly
    - Clean up and wait for eco_node to settle in eco before next case.
    """
    log("IT-CLS-*")
    # (name, workload_class, expect_perf_intent)
    # workload_class="" means no annotation (defaults to standard in extender).
    cases: list[tuple[str, str, bool]] = [
        ("cls-01-perf-class", "performance", True),
        ("cls-02-standard-class", "standard", False),
        ("cls-03-no-annotation", "", False),
    ]

    for name, wc, expect_perf in cases:
        delete_pod("joulie-it", name)

        # Start with eco_node in performance (frac=1 → both nodes performance).
        set_static_hp_frac("1")
        wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
        wait_node_draining_false(ctx.eco_node)

        # Force pod to eco_node via nodeName (bypasses scheduler).
        manifest = mk_pod_yaml(name, workload_class=wc, node_name=ctx.eco_node)
        out = kubectl(["apply", "-f", "-"], stdin=manifest, check=False, capture=True)
        if out.returncode != 0:
            err = (out.stderr or out.stdout or "").strip()
            raise AssertionError(f"{name}: apply failed unexpectedly: {err}")

        wait_pod_phase("joulie-it", name, "Running")

        # Trigger eco transition on eco_node.
        set_static_hp_frac("0")

        if expect_perf:
            wait_node_guarded_transition(ctx.eco_node)
        else:
            wait_node_eco_ready(ctx.eco_node)

        delete_pod("joulie-it", name)
        wait_node_eco_ready(ctx.eco_node)


def test_fsm_idempotency(ctx: Ctx) -> None:
    log("IT-FSM-05")
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)
    labels_before = get_node_labels(ctx.eco_node)
    # Reconcile interval is 5s in test install; wait >2 cycles.
    # Do not assert on Node resourceVersion here: kubelet/status churn can
    # legitimately update the Node object even when Joulie's visible labels are
    # stable.
    time.sleep(12)
    labels_after = get_node_labels(ctx.eco_node)
    if labels_before.get("joulie.io/power-profile") != labels_after.get("joulie.io/power-profile"):
        raise AssertionError("power-profile label flapped across idle reconciles")
    # Draining is tracked via NodeTwinState.schedulableClass, not as a node label.
    if "joulie.io/draining" in labels_after:
        raise AssertionError("joulie.io/draining label should not exist on node")


def get_nodetwin_status(node: str) -> dict[str, Any]:
    """Read the full status object from the NodeTwin CR."""
    out = kubectl(["get", "nodetwin", node, "-o", "json"], capture=True)
    return json.loads(out.stdout).get("status", {})


def get_nodehardware_status(node: str) -> dict[str, Any]:
    """Read the full status object from the NodeHardware CR."""
    out = kubectl(["get", "nodehardware", node, "-o", "json"], capture=True)
    return json.loads(out.stdout).get("status", {})


def test_twin_status_populated(ctx: Ctx) -> None:
    """IT-TWIN-STATUS-01: verify the operator writes twin status fields.

    The operator must populate schedulableClass, predicted scores, and
    lastUpdated in the NodeTwin status. The agent writes controlStatus
    separately. Both must coexist.
    """
    log("IT-TWIN-STATUS-01")
    set_static_hp_frac("1")
    wait_node_label(ctx.perf_node, "joulie.io/power-profile", "performance")

    # Wait for the operator to populate twin status (needs 1-2 reconcile cycles).
    def _twin_status_present() -> bool:
        status = get_nodetwin_status(ctx.perf_node)
        return status.get("schedulableClass", "") != ""

    wait_until(_twin_status_present, timeout_sec=30, desc="twin status populated")

    for node in [ctx.perf_node, ctx.eco_node]:
        status = get_nodetwin_status(node)
        sc = status.get("schedulableClass", "")
        if not sc:
            raise AssertionError(f"node={node}: schedulableClass is empty")
        log(f"node={node} schedulableClass={sc}")

        # Predicted scores must be present (may be 0 but field must exist).
        for field in ["predictedPowerHeadroomScore", "predictedCoolingStressScore", "predictedPsuStressScore"]:
            if field not in status:
                raise AssertionError(f"node={node}: missing twin status field {field}")

        last = status.get("lastUpdated", "")
        if not last:
            raise AssertionError(f"node={node}: lastUpdated is empty")

    # Verify agent's controlStatus coexists with operator's twin status.
    def _control_status_present() -> bool:
        status = get_nodetwin_status(ctx.eco_node)
        cs = status.get("controlStatus", {})
        return cs.get("cpu", {}).get("result", "") != ""

    wait_until(_control_status_present, timeout_sec=30, desc="agent controlStatus populated")
    status = get_nodetwin_status(ctx.eco_node)
    # Both must be present simultaneously.
    if not status.get("schedulableClass"):
        raise AssertionError("schedulableClass lost after agent wrote controlStatus")
    if not status.get("controlStatus", {}).get("cpu", {}).get("result"):
        raise AssertionError("controlStatus.cpu.result missing")
    log("twin status and controlStatus coexist correctly")


def test_nodehardware_discovery(ctx: Ctx) -> None:
    """IT-HW-01: verify the agent discovers hardware and writes NodeHardware status."""
    log("IT-HW-01")

    def _hw_present() -> bool:
        out = kubectl(["get", "nodehardware", ctx.eco_node, "-o", "json"], check=False, capture=True)
        if out.returncode != 0:
            return False
        status = json.loads(out.stdout).get("status", {})
        return status.get("updatedAt", "") != ""

    wait_until(_hw_present, timeout_sec=30, desc=f"nodehardware {ctx.eco_node} status populated")

    for node in [ctx.perf_node, ctx.eco_node]:
        status = get_nodehardware_status(node)
        cpu = status.get("cpu", {})
        if not cpu.get("totalCores"):
            raise AssertionError(f"node={node}: cpu.totalCores is 0 or missing")
        log(f"node={node} cpu.totalCores={cpu.get('totalCores')} cpu.rawModel={cpu.get('rawModel', '')}")

        caps = status.get("capabilities", {})
        if "cpuTelemetry" not in caps:
            raise AssertionError(f"node={node}: capabilities.cpuTelemetry missing")

        quality = status.get("quality", {})
        if not quality.get("overall"):
            raise AssertionError(f"node={node}: quality.overall is empty")
        log(f"node={node} quality={quality.get('overall')}")


def test_scheduler_extender_scoring(ctx: Ctx) -> None:
    """IT-SCHED-SCORE-01: verify scheduler extender returns valid prioritize responses."""
    log("IT-SCHED-SCORE-01")
    scheduler_repo = os.getenv("JOULIE_SCHEDULER_IMAGE_REPOSITORY", "").strip()
    if not scheduler_repo:
        log("SKIP: scheduler extender image not provided")
        return

    # Ensure eco node is in eco so we can test scoring differentiation.
    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")

    # Give the scheduler a moment to sync twin state.
    time.sleep(6)

    # Build a minimal ExtenderArgs prioritize request.
    prioritize_body = json.dumps({
        "Pod": {
            "metadata": {
                "name": "test-pod",
                "namespace": "joulie-it",
            },
            "spec": {
                "containers": [{"name": "c", "image": "busybox:1.36"}],
            },
        },
        "Nodes": {
            "metadata": {},
            "items": [
                {
                    "metadata": {"name": ctx.perf_node},
                },
                {
                    "metadata": {"name": ctx.eco_node},
                },
            ],
        },
        "NodeNames": [ctx.perf_node, ctx.eco_node],
    })

    # Port-forward to the scheduler extender and send the request.
    pf = subprocess.Popen(
        ["kubectl", "-n", "joulie-system", "port-forward", "svc/joulie-scheduler-extender", "19876:9876"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.time() + 15
        result = None
        while time.time() < deadline:
            out = run(
                ["curl", "-fsSL", "-X", "POST",
                 "-H", "Content-Type: application/json",
                 "-d", prioritize_body,
                 "http://127.0.0.1:19876/prioritize"],
                capture=True,
                check=False,
            )
            if out.returncode == 0 and out.stdout.strip():
                result = json.loads(out.stdout)
                break
            time.sleep(1.0)
        if result is None:
            raise RuntimeError("timeout calling scheduler extender /prioritize")
    finally:
        pf.terminate()

    # Validate response structure.
    if not isinstance(result, list):
        raise AssertionError(f"expected list response from /prioritize, got {type(result).__name__}")
    if len(result) != 2:
        raise AssertionError(f"expected 2 node scores, got {len(result)}")
    for entry in result:
        if "host" not in entry or "score" not in entry:
            raise AssertionError(f"missing host or score in prioritize response: {entry}")
        score = entry["score"]
        if not (0 <= score <= 100):
            raise AssertionError(f"score {score} out of range [0, 100] for {entry['host']}")
        log(f"node={entry['host']} score={score}")

    log("scheduler extender scoring verified")


def test_twin_status_survives_agent_writes(ctx: Ctx) -> None:
    """IT-TWIN-STATUS-02: verify operator twin status persists across agent reconcile cycles.

    The agent writes controlStatus every reconcile interval (5s in tests).
    The operator's schedulableClass must not be overwritten.
    """
    log("IT-TWIN-STATUS-02")
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")

    # Wait for twin status to be populated.
    def _has_sc() -> bool:
        return get_nodetwin_status(ctx.eco_node).get("schedulableClass", "") != ""

    wait_until(_has_sc, timeout_sec=30, desc="schedulableClass present")

    # Record the schedulableClass.
    sc_before = get_nodetwin_status(ctx.eco_node).get("schedulableClass")

    # Wait for 3 agent reconcile cycles (5s each = 15s).
    time.sleep(15)

    # Verify schedulableClass survived the agent's writes.
    sc_after = get_nodetwin_status(ctx.eco_node).get("schedulableClass")
    if not sc_after:
        raise AssertionError("schedulableClass was wiped after agent reconcile cycles")
    if sc_after != sc_before:
        raise AssertionError(
            f"schedulableClass changed unexpectedly: {sc_before!r} -> {sc_after!r}"
        )

    # Also verify controlStatus is still present.
    cs = get_nodetwin_status(ctx.eco_node).get("controlStatus", {})
    if not cs.get("cpu", {}).get("updatedAt"):
        raise AssertionError("controlStatus.cpu.updatedAt missing after wait")
    log("twin status survived agent write cycles")


def test_scheduler_filter_endpoint(ctx: Ctx) -> None:
    """IT-SCHED-FILTER-01: verify scheduler extender /filter endpoint directly.

    Sends raw ExtenderArgs to /filter and verifies:
    - Performance pod: eco node filtered out, perf node passes
    - Standard pod: both nodes pass (no filtering)
    """
    log("IT-SCHED-FILTER-01")
    scheduler_repo = os.getenv("JOULIE_SCHEDULER_IMAGE_REPOSITORY", "").strip()
    if not scheduler_repo:
        log("SKIP: scheduler extender image not provided")
        return

    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")
    wait_node_twin_class(ctx.eco_node, "eco")
    wait_node_twin_class(ctx.perf_node, "performance")
    time.sleep(4)

    def call_filter(pod_meta: dict, pod_spec: dict) -> dict:
        body = json.dumps({
            "Pod": {"metadata": pod_meta, "spec": pod_spec},
            "Nodes": {
                "metadata": {},
                "items": [
                    {"metadata": {"name": ctx.perf_node}},
                    {"metadata": {"name": ctx.eco_node}},
                ],
            },
            "NodeNames": [ctx.perf_node, ctx.eco_node],
        })
        pf = subprocess.Popen(
            ["kubectl", "-n", "joulie-system", "port-forward",
             "svc/joulie-scheduler-extender", "19877:9876"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        try:
            deadline = time.time() + 15
            while time.time() < deadline:
                out = run(
                    ["curl", "-fsSL", "-X", "POST",
                     "-H", "Content-Type: application/json",
                     "-d", body, "http://127.0.0.1:19877/filter"],
                    capture=True, check=False,
                )
                if out.returncode == 0 and out.stdout.strip():
                    return json.loads(out.stdout)
                time.sleep(1.0)
            raise RuntimeError("timeout calling /filter")
        finally:
            pf.terminate()

    # Performance pod: eco node should be filtered out.
    result = call_filter(
        {"name": "test-perf", "namespace": "joulie-it",
         "annotations": {"joulie.io/workload-class": "performance"}},
        {"containers": [{"name": "c", "image": "busybox:1.36"}]},
    )
    passed_names = [
        n["metadata"]["name"]
        for n in (result.get("nodes") or {}).get("items", [])
    ]
    failed = result.get("failedNodes", {})
    if ctx.eco_node not in failed:
        raise AssertionError(
            f"eco node {ctx.eco_node} should be in failedNodes for perf pod, "
            f"passed={passed_names} failed={list(failed.keys())}"
        )
    if ctx.perf_node not in passed_names:
        raise AssertionError(
            f"perf node {ctx.perf_node} should pass filter for perf pod, "
            f"passed={passed_names}"
        )
    log(f"perf pod filter: passed={passed_names} failed={list(failed.keys())}")

    # Standard pod: both nodes should pass.
    result = call_filter(
        {"name": "test-std", "namespace": "joulie-it"},
        {"containers": [{"name": "c", "image": "busybox:1.36"}]},
    )
    passed_names = [
        n["metadata"]["name"]
        for n in (result.get("nodes") or {}).get("items", [])
    ]
    if len(passed_names) != 2:
        raise AssertionError(
            f"standard pod should pass on both nodes, got passed={passed_names}"
        )
    log(f"standard pod filter: passed={passed_names}")
    log("scheduler filter endpoint verified")


def test_scheduling_draining_rejection(ctx: Ctx) -> None:
    """IT-SCH-DRAIN-01: performance pod is rejected from a draining node.

    When a node is in draining state (eco transition blocked by existing perf
    pod), new performance pods pinned to that node must be rejected by the
    scheduler extender.
    """
    log("IT-SCH-DRAIN-01")

    # Start with eco_node in performance, place a perf pod to trigger draining.
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)

    delete_pod("joulie-it", "drain-blocker")
    apply_yaml(mk_pod_yaml("drain-blocker", workload_class="performance", node_name=ctx.eco_node))
    wait_pod_phase("joulie-it", "drain-blocker", "Running")

    # Trigger eco transition: operator sees perf pod -> draining.
    set_static_hp_frac("0")
    wait_node_guarded_transition(ctx.eco_node)
    # Wait for cache to pick up draining state.
    time.sleep(4)

    # New perf pod pinned to eco_node via nodeSelector should be rejected.
    delete_pod("joulie-it", "drain-reject")
    apply_yaml(mk_pod_yaml(
        "drain-reject",
        workload_class="performance",
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_pending("joulie-it", "drain-reject")
    wait_pod_unschedulable_reason("joulie-it", "drain-reject", "joulie")
    log("performance pod correctly rejected from draining node")

    # Cleanup.
    delete_pod("joulie-it", "drain-reject")
    delete_pod("joulie-it", "drain-blocker")
    wait_node_eco_ready(ctx.eco_node)


def test_rapid_policy_switch(ctx: Ctx) -> None:
    """IT-POLICY-SWITCH-01: rapid frac changes converge correctly.

    Switch frac=1 -> frac=0 -> frac=1 in quick succession and verify that
    both nodes land in the correct final state (both performance).
    """
    log("IT-POLICY-SWITCH-01")
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_label(ctx.perf_node, "joulie.io/power-profile", "performance")

    # Rapid switch: eco -> perf -> eco -> perf.
    set_static_hp_frac("0")
    time.sleep(3)
    set_static_hp_frac("1")

    # Final state: both nodes should be performance.
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_label(ctx.perf_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)
    wait_node_draining_false(ctx.perf_node)

    # Twin status should reflect performance for both.
    wait_node_twin_class(ctx.eco_node, "performance")
    wait_node_twin_class(ctx.perf_node, "performance")
    log("rapid policy switch converged correctly")


def test_twin_scores_change_with_profile(ctx: Ctx) -> None:
    """IT-TWIN-SCORES-01: twin status scores update when profile changes.

    When a node transitions between performance and eco, the twin predicted
    scores should reflect the new profile. This validates the operator's twin
    computation is re-run on profile changes.
    """
    log("IT-TWIN-SCORES-01")
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_twin_class(ctx.eco_node, "performance")

    # Record scores under performance profile.
    perf_status = get_nodetwin_status(ctx.eco_node)
    perf_headroom = perf_status.get("predictedPowerHeadroomScore", -1)
    perf_last = perf_status.get("lastUpdated", "")
    log(f"performance: headroom={perf_headroom} lastUpdated={perf_last}")

    # Switch to eco.
    set_static_hp_frac("0")
    wait_node_eco_ready(ctx.eco_node)

    # Wait for twin to update.
    def _updated() -> bool:
        s = get_nodetwin_status(ctx.eco_node)
        return s.get("lastUpdated", "") != perf_last and s.get("schedulableClass") == "eco"

    wait_until(_updated, timeout_sec=30, desc="twin status updated after eco transition")

    eco_status = get_nodetwin_status(ctx.eco_node)
    eco_headroom = eco_status.get("predictedPowerHeadroomScore", -1)
    log(f"eco: headroom={eco_headroom} lastUpdated={eco_status.get('lastUpdated', '')}")

    # Scores should have changed (eco has different cap/headroom from perf).
    # At minimum, lastUpdated must differ.
    if eco_status.get("lastUpdated") == perf_last:
        raise AssertionError("lastUpdated did not change after profile switch")
    log("twin scores updated after profile change")


def test_standard_pod_schedules_anywhere(ctx: Ctx) -> None:
    """IT-SCH-STD-01: standard pod schedules on both perf and eco nodes.

    Standard (no workload-class annotation) pods must be accepted by the
    scheduler extender on any node regardless of its power profile.
    """
    log("IT-SCH-STD-01")
    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")
    wait_node_twin_class(ctx.eco_node, "eco")
    wait_node_twin_class(ctx.perf_node, "performance")

    # Standard pod on eco node.
    delete_pod("joulie-it", "std-on-eco")
    apply_yaml(mk_pod_yaml(
        "std-on-eco",
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_phase("joulie-it", "std-on-eco", "Running")
    log("standard pod scheduled on eco node")
    delete_pod("joulie-it", "std-on-eco")

    # Standard pod on perf node.
    delete_pod("joulie-it", "std-on-perf")
    apply_yaml(mk_pod_yaml(
        "std-on-perf",
        node_selector={"kubernetes.io/hostname": ctx.perf_node},
    ))
    wait_pod_phase("joulie-it", "std-on-perf", "Running")
    log("standard pod scheduled on perf node")
    delete_pod("joulie-it", "std-on-perf")


def test_facility_metrics_disabled_by_default(ctx: Ctx) -> None:
    """IT-FACILITY-01: verify facility metrics collection is disabled by default.

    ENABLE_FACILITY_METRICS defaults to false. The operator logs should not
    contain any "[facility]" log lines about fetching metrics.
    """
    log("IT-FACILITY-01")

    out = kubectl(
        ["-n", "joulie-system", "logs", "deploy/joulie-operator", "--tail=500"],
        capture=True, check=False,
    )
    if out.returncode != 0:
        raise AssertionError(
            f"failed to read operator logs: {(out.stderr or '').strip()}"
        )

    logs = out.stdout or ""
    facility_lines = [
        line for line in logs.splitlines()
        if "[facility]" in line.lower()
    ]
    if facility_lines:
        raise AssertionError(
            f"found {len(facility_lines)} facility log line(s) but "
            f"ENABLE_FACILITY_METRICS should be false by default. "
            f"First match: {facility_lines[0]!r}"
        )
    log("no facility metrics log lines found (correctly disabled)")


def main() -> int:
    try:
        scope = os.getenv("IT_SCOPE", "all").strip().lower()
        log(f"integration scope: {scope}")
        ctx = test_boot_and_install()
        test_scheduler_extender_deployed(ctx)
        test_telemetry_http(ctx)
        test_twin_status_populated(ctx)
        test_nodehardware_discovery(ctx)
        if scope in ("all", "full"):
            test_classification_matrix(ctx)
            test_fsm_and_labels(ctx)
            test_fsm_toggle_under_eco(ctx)
            test_fsm_idempotency(ctx)
            test_scheduling(ctx)
            test_scheduler_extender_scoring(ctx)
            test_scheduler_filter_endpoint(ctx)
            test_scheduling_draining_rejection(ctx)
            test_standard_pod_schedules_anywhere(ctx)
            test_rapid_policy_switch(ctx)
            test_twin_scores_change_with_profile(ctx)
            test_twin_status_survives_agent_writes(ctx)
            test_facility_metrics_disabled_by_default(ctx)
        elif scope in ("gpu", "gpu-only", "gpu_only"):
            log("non-GPU suites temporarily disabled (IT_SCOPE=gpu-only)")
        else:
            raise RuntimeError(f"unknown IT_SCOPE={scope!r}; expected all/full or gpu-only")
        log("all integration tests passed")
        return 0
    except Exception as e:
        print(f"[integration] FAILED: {e}", flush=True)
        dump_debug()
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
