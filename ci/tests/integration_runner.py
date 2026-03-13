#!/usr/bin/env python3
"""Integration test runner for a 2-node k3s cluster.

Node layout (deterministic, set via --node-name in the Dagger CI module):
  k3s-server   → always stays in performance (operator family-floor node)
  k3s-worker-0 → transitions to eco when STATIC_HP_FRAC=0

With STATIC_HP_FRAC=0 and two same-family nodes the operator keeps exactly one
node in performance (the family floor) and puts the other in eco.  The operator
sorts by compute density then lexicographically, so "k3s-server" < "k3s-worker-0"
→ server is always the floor node.
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
PERF_NODE = "k3s-server"
ECO_NODE = "k3s-worker-0"


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


def get_node_resource_version(node: str) -> str:
    out = kubectl(["get", "node", node, "-o", "json"], capture=True).stdout
    return json.loads(out)["metadata"].get("resourceVersion", "")


def wait_node_label(node: str, key: str, val: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_labels(node).get(key) == val,
        timeout_sec=timeout_sec,
        desc=f"node {node} label {key}={val}",
    )


def wait_node_draining_false(node: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_labels(node).get("joulie.io/draining", "false") == "false",
        timeout_sec=timeout_sec,
        desc=f"node {node} draining=false (or unset)",
    )


def is_guarded_transition(node: str) -> bool:
    labels = get_node_labels(node)
    profile = labels.get("joulie.io/power-profile", "")
    draining = labels.get("joulie.io/draining", "")
    return profile == "eco" and draining == "true"


def wait_node_guarded_transition(node: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: is_guarded_transition(node),
        timeout_sec=timeout_sec,
        desc=f"node {node} in guarded perf->eco transition",
    )


def wait_node_eco_ready(node: str, timeout_sec: int = 120) -> None:
    def _ok() -> bool:
        labels = get_node_labels(node)
        profile = labels.get("joulie.io/power-profile", "")
        draining = labels.get("joulie.io/draining", "false")
        return profile == "eco" and draining in ("false", "")

    wait_until(
        _ok,
        timeout_sec=timeout_sec,
        desc=f"node {node} eco ready (non-draining)",
    )


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
        return ("failedscheduling" in text) and (needle in text)

    wait_until(_ok, timeout_sec=timeout_sec, desc=f"pod {ns}/{name} unschedulable contains '{contains}'")


def mk_pod_yaml(
    name: str,
    image: str = "busybox:1.36",
    affinity: str = "",
    node_name: str = "",
    node_selector: dict[str, str] | None = None,
) -> str:
    lines = [
        "apiVersion: v1",
        "kind: Pod",
        "metadata:",
        f"  name: {name}",
        "  namespace: joulie-it",
        "  labels:",
        "    app.kubernetes.io/part-of: joulie-it",
        "spec:",
    ]
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
    if affinity.strip():
        lines.extend(affinity.strip("\n").splitlines())
    return "\n".join(lines) + "\n"


def perf_affinity_notin_eco() -> str:
    return textwrap.indent(
        textwrap.dedent(
            """\
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                  - matchExpressions:
                    - key: joulie.io/power-profile
                      operator: NotIn
                      values: ["eco"]
            """
        ),
        "  ",
    )


def eco_affinity_with_draining_false() -> str:
    return textwrap.indent(
        textwrap.dedent(
            """\
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                  - matchExpressions:
                    - key: joulie.io/power-profile
                      operator: In
                      values: ["eco"]
                    - key: joulie.io/draining
                      operator: In
                      values: ["false"]
            """
        ),
        "  ",
    )


def eco_affinity_in_eco() -> str:
    return textwrap.indent(
        textwrap.dedent(
            """\
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                  - matchExpressions:
                    - key: joulie.io/power-profile
                      operator: In
                      values: ["eco"]
            """
        ),
        "  ",
    )


def install_joulie() -> None:
    log("installing joulie chart")
    agent_repo = os.getenv("JOULIE_AGENT_IMAGE_REPOSITORY", "").strip()
    agent_tag = os.getenv("JOULIE_AGENT_IMAGE_TAG", "").strip()
    operator_repo = os.getenv("JOULIE_OPERATOR_IMAGE_REPOSITORY", "").strip()
    operator_tag = os.getenv("JOULIE_OPERATOR_IMAGE_TAG", "").strip()
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
    )
    wait_rollout("joulie-system", "deploy/joulie-operator")
    wait_rollout("joulie-system", "statefulset/joulie-agent-pool")


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
    apply_yaml(
        textwrap.dedent(
            f"""\
            apiVersion: joulie.io/v1alpha1
            kind: TelemetryProfile
            metadata:
              name: it-http-{node.replace('.', '-')}
            spec:
              target:
                scope: node
                nodeName: {node}
              sources:
                cpu:
                  type: http
                  http:
                    endpoint: http://joulie-http-mock.joulie-it.svc.cluster.local:8080/telemetry/{{node}}
                    timeoutSeconds: 3
              controls:
                cpu:
                  type: http
                  http:
                    endpoint: http://joulie-http-mock.joulie-it.svc.cluster.local:8080/control/{{node}}
                    timeoutSeconds: 3
                    mode: dvfs
                gpu:
                  type: http
                  http:
                    endpoint: http://joulie-http-mock.joulie-it.svc.cluster.local:8080/control/{{node}}
                    timeoutSeconds: 3
                    mode: powercap
            """
        )
    )


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
    out = kubectl(["get", "nodepowerprofiles", "-o", "json"], capture=True).stdout
    items = json.loads(out).get("items", [])
    name = None
    for item in items:
        if item.get("spec", {}).get("nodeName") == node:
            name = item.get("metadata", {}).get("name")
            break
    if not name:
        raise RuntimeError(f"nodepowerprofile for node {node} not found")
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
    kubectl(["patch", "nodepowerprofile", name, "--type=merge", "-p", json.dumps(patch)])


def get_telemetryprofile_gpu_control_status(node: str) -> tuple[str, str]:
    name = f"it-http-{node.replace('.', '-')}"
    out = kubectl(["get", "telemetryprofile", name, "-o", "json"], capture=True, check=False)
    if out.returncode != 0 or not out.stdout.strip():
        return "", ""
    status = json.loads(out.stdout).get("status", {})
    gpu = status.get("control", {}).get("gpu", {})
    return str(gpu.get("result", "")), str(gpu.get("message", ""))


def dump_debug() -> None:
    log("collecting debug data")
    cmds = [
        ["kubectl", "get", "nodes", "-o", "wide", "--show-labels"],
        ["kubectl", "describe", "nodes"],
        ["kubectl", "get", "pods", "-A", "-o", "wide"],
        ["kubectl", "describe", "pods", "-A"],
        ["kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"],
        ["kubectl", "get", "nodepowerprofiles", "-o", "yaml"],
        ["kubectl", "get", "telemetryprofiles", "-o", "yaml"],
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

    perf_node  – always stays in performance (operator family floor).
    eco_node   – transitions to eco when STATIC_HP_FRAC=0.
    node       – alias for eco_node kept for clarity in test code.
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

    for expected in (PERF_NODE, ECO_NODE):
        if expected not in nodes:
            raise RuntimeError(
                f"expected node {expected!r} not present; got {nodes}. "
                "Check that --node-name flags in the Dagger k3s setup match PERF_NODE/ECO_NODE constants."
            )

    for node in (PERF_NODE, ECO_NODE):
        kubectl(["label", "node", node, "joulie.io/managed=true", "--overwrite"])

    install_joulie()
    kubectl(["get", "crd", "nodepowerprofiles.joulie.io"])
    kubectl(["get", "crd", "telemetryprofiles.joulie.io"])
    kubectl(["create", "ns", "joulie-it"], check=False)
    return Ctx(perf_node=PERF_NODE, eco_node=ECO_NODE)


def test_telemetry_http(ctx: Ctx) -> None:
    log("IT-TP-01")
    install_http_mock()
    # Apply telemetry profile to the eco node (it's in performance initially due
    # to STATIC_HP_FRAC=1 at install time; that's fine – the agent reconciles it).
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
        result, message = get_telemetryprofile_gpu_control_status(ctx.eco_node)
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
    # lands on eco_node regardless of its affinity.
    delete_pod("joulie-it", "perf-a")
    apply_yaml(mk_pod_yaml("perf-a", affinity=perf_affinity_notin_eco(), node_name=ctx.eco_node))
    wait_pod_phase("joulie-it", "perf-a", "Running")

    # Trigger eco transition: operator wants eco_node→eco but sees perf pod → draining=true.
    set_static_hp_frac("0")
    wait_node_guarded_transition(ctx.eco_node)

    # Once perf pod is gone, eco_node completes transition.
    delete_pod("joulie-it", "perf-a")
    wait_node_eco_ready(ctx.eco_node)

    # Best-effort pod must not trigger draining.
    apply_yaml(mk_pod_yaml("besteffort-a"))
    wait_pod_phase("joulie-it", "besteffort-a", "Running")
    wait_node_eco_ready(ctx.eco_node)
    delete_pod("joulie-it", "besteffort-a")

    # eco → performance clears draining immediately.
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.eco_node)


def test_fsm_toggle_under_eco(ctx: Ctx) -> None:
    """IT-FSM-07: perf-intent pod restricted to eco_node stays pending while eco_node is eco.

    With two nodes the scheduler would otherwise place the pod on perf_node.
    We use nodeSelector to pin the pod to eco_node so the unschedulability is
    observable, and confirm eco_node doesn't spuriously enter draining.
    """
    log("IT-FSM-07")
    set_static_hp_frac("0")
    wait_node_eco_ready(ctx.eco_node)

    delete_pod("joulie-it", "perf-toggle")
    # Pin to eco_node via nodeSelector (goes through scheduler → affinity enforced).
    apply_yaml(mk_pod_yaml(
        "perf-toggle",
        affinity=perf_affinity_notin_eco(),
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_pending("joulie-it", "perf-toggle")
    wait_pod_unschedulable_reason("joulie-it", "perf-toggle", "affinity")
    wait_node_eco_ready(ctx.eco_node)

    delete_pod("joulie-it", "perf-toggle")


def test_scheduling(ctx: Ctx) -> None:
    """IT-SCH-*: verify scheduler respects operator-applied power-profile labels.

    With two nodes:
    - frac=1 → both nodes in performance
    - frac=0 → perf_node=performance (floor), eco_node=eco

    We use nodeSelector (not nodeName) so the scheduler enforces affinity rules.
    """
    log("IT-SCH-*")

    # --- Perf intent schedules on a node with no label ---
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
    delete_pod("joulie-it", "sch-perf-on-unlabeled")
    kubectl(["label", "node", ctx.eco_node, "joulie.io/power-profile-"], check=False)
    apply_yaml(mk_pod_yaml(
        "sch-perf-on-unlabeled",
        affinity=perf_affinity_notin_eco(),
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_phase("joulie-it", "sch-perf-on-unlabeled", "Running")
    delete_pod("joulie-it", "sch-perf-on-unlabeled")
    # Restore; operator will re-label within one reconcile interval but we need it now.
    set_static_hp_frac("1")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")

    # --- Perf intent schedules on a performance node ---
    delete_pod("joulie-it", "sch-perf-on-perf")
    apply_yaml(mk_pod_yaml(
        "sch-perf-on-perf",
        affinity=perf_affinity_notin_eco(),
        node_selector={"kubernetes.io/hostname": ctx.perf_node},
    ))
    wait_pod_phase("joulie-it", "sch-perf-on-perf", "Running")
    delete_pod("joulie-it", "sch-perf-on-perf")

    # --- Eco-only intent cannot schedule when all nodes are in performance ---
    # frac=1 → both nodes performance → In[eco] affinity cannot be satisfied.
    delete_pod("joulie-it", "sch-eco-on-all-perf")
    apply_yaml(mk_pod_yaml("sch-eco-on-all-perf", affinity=eco_affinity_in_eco()))
    wait_pod_pending("joulie-it", "sch-eco-on-all-perf")
    wait_pod_unschedulable_reason("joulie-it", "sch-eco-on-all-perf", "affinity")
    delete_pod("joulie-it", "sch-eco-on-all-perf")

    # --- Eco-only intent schedules on eco_node ---
    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")
    delete_pod("joulie-it", "sch-eco-on-eco")
    apply_yaml(mk_pod_yaml(
        "sch-eco-on-eco",
        affinity=eco_affinity_in_eco(),
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_phase("joulie-it", "sch-eco-on-eco", "Running")
    delete_pod("joulie-it", "sch-eco-on-eco")

    # --- Eco-only intent cannot schedule on performance node ---
    # perf_node stays in performance (family floor) even with frac=0.
    delete_pod("joulie-it", "sch-eco-on-perf")
    apply_yaml(mk_pod_yaml(
        "sch-eco-on-perf",
        affinity=eco_affinity_in_eco(),
        node_selector={"kubernetes.io/hostname": ctx.perf_node},
    ))
    wait_pod_pending("joulie-it", "sch-eco-on-perf")
    wait_pod_unschedulable_reason("joulie-it", "sch-eco-on-perf", "affinity")
    delete_pod("joulie-it", "sch-eco-on-perf")

    # --- Eco + draining=false does not schedule on draining eco_node ---
    set_static_hp_frac("0")
    wait_node_label(ctx.eco_node, "joulie.io/power-profile", "eco")
    kubectl(
        [
            "label", "node", ctx.eco_node,
            "joulie.io/power-profile=eco",
            "joulie.io/draining=true",
            "--overwrite",
        ]
    )
    delete_pod("joulie-it", "sch-eco-on-draining")
    apply_yaml(mk_pod_yaml(
        "sch-eco-on-draining",
        affinity=eco_affinity_with_draining_false(),
        node_selector={"kubernetes.io/hostname": ctx.eco_node},
    ))
    wait_pod_pending("joulie-it", "sch-eco-on-draining")
    wait_pod_unschedulable_reason("joulie-it", "sch-eco-on-draining", "affinity")
    kubectl(["label", "node", ctx.eco_node, "joulie.io/draining=false", "--overwrite"])
    wait_pod_phase("joulie-it", "sch-eco-on-draining", "Running")
    delete_pod("joulie-it", "sch-eco-on-draining")


def test_classification_matrix(ctx: Ctx) -> None:
    """IT-CLS-*: verify the operator correctly classifies pods as perf-intent or not.

    Strategy with two nodes:
    - Start each case with eco_node in performance (frac=1 → both nodes performance).
    - Force the test pod onto eco_node via nodeName (bypasses scheduler, kubelet
      runs it regardless of affinity; this lets us observe classification on a
      node that WILL transition to eco).
    - Then trigger frac=0: operator tries to move eco_node to eco.
      · If pod is perf-intent  → draining=true  (guarded transition)
      · If pod is NOT perf-intent → eco_node goes to eco directly
    - Clean up and wait for eco_node to settle in eco before next case.

    Skip cases (inherently unschedulable at the scheduler level) are run with
    frac=0 to verify the scheduler rejects them; classification is moot.
    """
    log("IT-CLS-*")
    # name, affinity, expect_perf, selector, force_node_name, expect_apply_ok
    cases: list[tuple[str, str, bool, dict[str, str] | None, bool, bool]] = [
        ("cls-01-notin-eco", perf_affinity_notin_eco(), True, None, True, True),
        (
            "cls-02-notin-eco-plus-extra",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco"]
                            - key: kubernetes.io/arch
                              operator: Exists
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        (
            "cls-03-in-performance",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["performance"]
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        ("cls-04-selector-performance", "", True, {"joulie.io/power-profile": "performance"}, True, True),
        (
            "cls-05-or-terms-one-excludes-eco",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["eco"]
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        ("cls-10-best-effort", "", False, None, False, True),
        (
            "cls-11-preferred-only",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        preferredDuringSchedulingIgnoredDuringExecution:
                        - weight: 100
                          preference:
                            matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            False,
            None,
            False,
            True,
        ),
        (
            "cls-12-in-eco-only",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            False,
            None,
            True,
            True,
        ),
        (
            "cls-13-unrelated-required",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: kubernetes.io/os
                              operator: In
                              values: ["linux"]
                    """
                ),
                "  ",
            ),
            False,
            None,
            False,
            True,
        ),
        (
            "cls-14-exists-profile-key",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: Exists
                    """
                ),
                "  ",
            ),
            False,
            None,
            True,
            True,
        ),
        (
            "cls-14-doesnotexist-profile-key",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: DoesNotExist
                    """
                ),
                "  ",
            ),
            False,
            None,
            False,
            True,
        ),
        (
            "cls-15-conflicting-or-terms",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["eco"]
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        (
            "cls-20-notin-eco-plus-other",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco", "somethingElse"]
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        (
            "cls-21-multi-expr-across-terms",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["eco"]
                            - key: kubernetes.io/os
                              operator: In
                              values: ["linux"]
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            True,
            None,
            True,
            True,
        ),
        (
            "cls-22-empty-values-notin",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: NotIn
                              values: []
                    """
                ),
                "  ",
            ),
            False,
            None,
            False,
            False,
        ),
        (
            "cls-23-selector-plus-eco-required",
            textwrap.indent(
                textwrap.dedent(
                    """\
                    affinity:
                      nodeAffinity:
                        requiredDuringSchedulingIgnoredDuringExecution:
                          nodeSelectorTerms:
                          - matchExpressions:
                            - key: joulie.io/power-profile
                              operator: In
                              values: ["eco"]
                    """
                ),
                "  ",
            ),
            True,
            {"joulie.io/power-profile": "performance"},
            True,
            True,
        ),
        ("cls-24-node-name-no-constraints", "", False, None, True, True),
    ]

    # Cases that are inherently unschedulable at the scheduler level.
    # In the 2-node cluster these remain unschedulable for the same reasons:
    #   cls-23: nodeSelector=performance conflicts with required affinity In[eco]
    #   cls-14-doesnotexist: required DoesNotExist on power-profile while both
    #                        nodes carry the label (operator assigns it).
    skip_running_validation = {
        "cls-23-selector-plus-eco-required",
        "cls-14-doesnotexist-profile-key",
    }

    # Cases that require the eco_node to be in eco BEFORE the pod can schedule.
    # These cannot use the nodeName bypass because k3s 1.34+ enforces nodeAffinity
    # at the kubelet level even for nodeName pods.  Strategy: set frac=0 first,
    # wait for eco_node to become eco, then apply and verify the pod runs and the
    # node stays eco (confirming the pod is not classified as perf-intent).
    eco_first_cases = {
        "cls-12-in-eco-only",
    }

    for name, affinity, expect_perf, selector, force_node_name, expect_apply_ok in cases:
        delete_pod("joulie-it", name)

        # ── Cases where Kubernetes may outright reject the manifest ──────────
        if not expect_apply_ok:
            set_static_hp_frac("0")
            wait_node_eco_ready(ctx.eco_node)
            manifest = mk_pod_yaml(name, affinity=affinity, node_selector=selector)
            out = kubectl(["apply", "-f", "-"], stdin=manifest, check=False, capture=True)
            if out.returncode != 0:
                # k8s rejected it (expected); eco_node stays eco.
                wait_node_eco_ready(ctx.eco_node)
                continue
            # k8s accepted despite empty values; treat as non-perf-intent.
            wait_pod_phase("joulie-it", name, "Running")
            wait_node_eco_ready(ctx.eco_node)
            delete_pod("joulie-it", name)
            wait_node_eco_ready(ctx.eco_node)
            continue

        # ── Cases that are inherently unschedulable ──────────────────────────
        if name in skip_running_validation:
            set_static_hp_frac("0")
            wait_node_eco_ready(ctx.eco_node)
            manifest = mk_pod_yaml(name, affinity=affinity, node_selector=selector)
            kubectl(["apply", "-f", "-"], stdin=manifest)
            wait_pod_pending("joulie-it", name)
            wait_pod_unschedulable_reason("joulie-it", name, "affinity")
            delete_pod("joulie-it", name)
            wait_node_eco_ready(ctx.eco_node)
            continue

        # ── Cases requiring eco_node to be eco before the pod can schedule ──
        # k3s 1.34+ enforces nodeAffinity at the kubelet level even for nodeName
        # pods, so we cannot force an In[eco] pod onto a performance node.
        # Set frac=0 first, wait for eco transition, then apply normally.
        if name in eco_first_cases:
            set_static_hp_frac("0")
            wait_node_eco_ready(ctx.eco_node)
            manifest = mk_pod_yaml(name, affinity=affinity, node_selector=selector)
            kubectl(["apply", "-f", "-"], stdin=manifest)
            wait_pod_phase("joulie-it", name, "Running")
            wait_node_eco_ready(ctx.eco_node)
            delete_pod("joulie-it", name)
            wait_node_eco_ready(ctx.eco_node)
            continue

        # ── Normal classification cases ──────────────────────────────────────
        # Start with eco_node in performance (frac=1 → both nodes performance).
        set_static_hp_frac("1")
        wait_node_label(ctx.eco_node, "joulie.io/power-profile", "performance")
        wait_node_draining_false(ctx.eco_node)

        # Force pod to eco_node via nodeName when requested.
        # nodeName bypasses the scheduler so kubelet runs the pod regardless of
        # affinity; this lets us observe the operator's classification response
        # on the node that will be targeted for eco.
        # For cases without force_node_name the pod floats freely (best-effort,
        # preferred-only, unrelated-required); classification is still checked
        # via eco_node's state after frac=0.
        node_name = ctx.eco_node if force_node_name else ""
        manifest = mk_pod_yaml(name, affinity=affinity, node_name=node_name, node_selector=selector)
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
    rv_before = get_node_resource_version(ctx.eco_node)
    # Reconcile interval is 5s in test install; wait >2 cycles.
    time.sleep(12)
    labels_after = get_node_labels(ctx.eco_node)
    rv_after = get_node_resource_version(ctx.eco_node)
    if labels_before.get("joulie.io/power-profile") != labels_after.get("joulie.io/power-profile"):
        raise AssertionError("power-profile label flapped across idle reconciles")
    if labels_after.get("joulie.io/draining", "false") != "false":
        raise AssertionError("draining label should remain false in steady performance mode")
    if rv_before != rv_after:
        raise AssertionError(f"node resourceVersion changed without requested state change: {rv_before} -> {rv_after}")


def main() -> int:
    try:
        scope = os.getenv("IT_SCOPE", "all").strip().lower()
        log(f"integration scope: {scope}")
        ctx = test_boot_and_install()
        test_telemetry_http(ctx)
        if scope in ("all", "full"):
            test_classification_matrix(ctx)
            test_fsm_and_labels(ctx)
            test_fsm_toggle_under_eco(ctx)
            test_fsm_idempotency(ctx)
            test_scheduling(ctx)
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
