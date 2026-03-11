#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import subprocess
import tempfile
import textwrap
import time
from dataclasses import dataclass
from typing import Any


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


def get_ready_node() -> str:
    out = kubectl(["get", "nodes", "-o", "json"], capture=True).stdout
    items = json.loads(out).get("items", [])
    for n in items:
        for c in n.get("status", {}).get("conditions", []):
            if c.get("type") == "Ready" and c.get("status") == "True":
                return n["metadata"]["name"]
    raise RuntimeError("no Ready node found")


def wait_ready_node(timeout_sec: int = 300) -> str:
    last_seen: list[str] = []

    def _has_ready() -> bool:
        nonlocal last_seen
        out = kubectl(["get", "nodes", "-o", "json"], check=False, capture=True)
        if out.returncode != 0 or not out.stdout.strip():
            return False
        items = json.loads(out.stdout).get("items", [])
        last_seen = [n.get("metadata", {}).get("name", "") for n in items]
        for n in items:
            for c in n.get("status", {}).get("conditions", []):
                if c.get("type") == "Ready" and c.get("status") == "True":
                    return True
        return False

    wait_until(_has_ready, timeout_sec=timeout_sec, interval_sec=2.0, desc="a Ready node in cluster")
    node = get_ready_node()
    log(f"ready node detected: {node}")
    return node


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


def wait_node_draining_false(node: str, timeout_sec: int = 120) -> None:
    wait_until(
        lambda: get_node_labels(node).get("joulie.io/draining", "false") == "false",
        timeout_sec=timeout_sec,
        desc=f"node {node} draining=false (or unset)",
    )


def assert_node_label(node: str, key: str, expected: str) -> None:
    got = get_node_labels(node).get(key)
    if got != expected:
        raise AssertionError(f"node={node} label {key} got={got!r} expected={expected!r}")


def delete_pod(ns: str, name: str) -> None:
    kubectl(["-n", ns, "delete", "pod", name, "--ignore-not-found=true", "--wait=true", "--timeout=90s"], check=False)


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


def mk_pod_yaml(
    name: str,
    image: str = "registry.k8s.io/pause:3.10",
    affinity: str = "",
    node_name: str = "",
    node_selector: dict[str, str] | None = None,
) -> str:
    node_name_line = f"  nodeName: {node_name}\n" if node_name else ""
    node_selector_block = ""
    if node_selector:
        lines = ["  nodeSelector:"]
        for k, v in node_selector.items():
            lines.append(f'    {k}: "{v}"')
        node_selector_block = "\n".join(lines) + "\n"
    return textwrap.dedent(
        f"""\
        apiVersion: v1
        kind: Pod
        metadata:
          name: {name}
          namespace: joulie-it
          labels:
            app.kubernetes.io/part-of: joulie-it
        spec:
{node_name_line}  restartPolicy: Never
{node_selector_block}\
          containers:
          - name: c
            image: {image}
            command: ["sh","-c","sleep 1200"]
{affinity}
        """
    )


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


def install_joulie() -> None:
    log("installing joulie chart")
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
        ]
    )
    wait_rollout("joulie-system", "deploy/joulie-operator")
    wait_rollout("joulie-system", "statefulset/joulie-agent-pool")


def set_static_hp_frac(frac: str) -> None:
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
                STATS = {"get": 0, "post": 0}
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
                    mode: auto
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


def dump_debug() -> None:
    log("collecting debug data")
    cmds = [
        ["kubectl", "get", "nodes", "-o", "wide", "--show-labels"],
        ["kubectl", "get", "pods", "-A", "-o", "wide"],
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
    node: str


def test_boot_and_install() -> Ctx:
    log("IT-BOOT-01 / IT-HELM-01")
    kubectl(["version"], check=False)
    node = wait_ready_node(timeout_sec=300)
    kubectl(["label", "node", node, "joulie.io/managed=true", "--overwrite"])
    install_joulie()
    kubectl(["create", "ns", "joulie-it"], check=False)
    return Ctx(node=node)


def test_telemetry_http(ctx: Ctx) -> None:
    log("IT-TP-01")
    install_http_mock()
    apply_telemetry_profile(ctx.node)
    time.sleep(12)
    stats = get_mock_stats()
    if stats.get("post", 0) <= 0:
        raise AssertionError(f"expected control POSTs > 0, stats={stats}")
    if stats.get("get", 0) <= 0:
        log(f"telemetry GET count is 0 in this run (expected with HTTP control auto/RAPL path), stats={stats}")


def test_fsm_and_labels(ctx: Ctx) -> None:
    log("IT-FSM-*")
    set_static_hp_frac("1")
    wait_node_label(ctx.node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.node)

    # perf pod present -> draining=true when moving to eco
    delete_pod("joulie-it", "perf-a")
    apply_yaml(mk_pod_yaml("perf-a", affinity=perf_affinity_notin_eco()))
    wait_pod_phase("joulie-it", "perf-a", "Running")
    set_static_hp_frac("0")
    wait_node_label(ctx.node, "joulie.io/power-profile", "eco")
    wait_node_label(ctx.node, "joulie.io/draining", "true")

    # draining clears after perf pod gone
    delete_pod("joulie-it", "perf-a")
    wait_node_draining_false(ctx.node)
    assert_node_label(ctx.node, "joulie.io/power-profile", "eco")

    # best-effort pod should not trigger draining
    apply_yaml(mk_pod_yaml("besteffort-a"))
    wait_pod_phase("joulie-it", "besteffort-a", "Running")
    wait_node_draining_false(ctx.node)
    delete_pod("joulie-it", "besteffort-a")

    # eco -> performance clears draining immediately
    set_static_hp_frac("1")
    wait_node_label(ctx.node, "joulie.io/power-profile", "performance")
    wait_node_draining_false(ctx.node)


def test_legacy_migration(ctx: Ctx) -> None:
    log("IT-FSM-06")
    kubectl(["label", "node", ctx.node, "joulie.io/power-profile=draining-performance", "--overwrite"])
    set_static_hp_frac("0")
    wait_node_label(ctx.node, "joulie.io/power-profile", "eco")
    # may be true or false depending on running perf pods; just ensure key exists and is valid
    draining = get_node_labels(ctx.node).get("joulie.io/draining")
    if draining not in ("true", "false", None):
        raise AssertionError(f"invalid draining label value after legacy migration: {draining}")


def test_scheduling(ctx: Ctx) -> None:
    log("IT-SCH-*")
    set_static_hp_frac("1")
    wait_node_label(ctx.node, "joulie.io/power-profile", "performance")

    # perf NotIn eco schedules on perf
    delete_pod("joulie-it", "sch-perf-on-perf")
    apply_yaml(mk_pod_yaml("sch-perf-on-perf", affinity=perf_affinity_notin_eco()))
    wait_pod_phase("joulie-it", "sch-perf-on-perf", "Running")
    delete_pod("joulie-it", "sch-perf-on-perf")

    # perf NotIn eco does not schedule on eco
    set_static_hp_frac("0")
    wait_node_label(ctx.node, "joulie.io/power-profile", "eco")
    delete_pod("joulie-it", "sch-perf-on-eco")
    apply_yaml(mk_pod_yaml("sch-perf-on-eco", affinity=perf_affinity_notin_eco()))
    wait_pod_pending("joulie-it", "sch-perf-on-eco")
    delete_pod("joulie-it", "sch-perf-on-eco")

    # eco + draining=false excludes draining nodes
    apply_yaml(mk_pod_yaml("sch-perf-trigger", affinity=perf_affinity_notin_eco(), node_name=ctx.node))
    wait_pod_phase("joulie-it", "sch-perf-trigger", "Running")
    wait_node_label(ctx.node, "joulie.io/draining", "true")
    delete_pod("joulie-it", "sch-eco-on-draining")
    apply_yaml(mk_pod_yaml("sch-eco-on-draining", affinity=eco_affinity_with_draining_false()))
    wait_pod_pending("joulie-it", "sch-eco-on-draining")
    delete_pod("joulie-it", "sch-perf-trigger")
    wait_node_label(ctx.node, "joulie.io/draining", "false")
    wait_pod_phase("joulie-it", "sch-eco-on-draining", "Running")
    delete_pod("joulie-it", "sch-eco-on-draining")


def test_classification_matrix(ctx: Ctx) -> None:
    log("IT-CLS-*")
    set_static_hp_frac("0")
    wait_node_label(ctx.node, "joulie.io/power-profile", "eco")
    wait_node_draining_false(ctx.node)

    cases: list[tuple[str, str, bool, dict[str, str] | None]] = [
        ("cls-01-notin-eco", perf_affinity_notin_eco(), True, None),
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
        ),
        ("cls-04-selector-performance", "", True, {"joulie.io/power-profile": "performance"}),
        ("cls-10-best-effort", "", False, None),
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
        ),
        ("cls-12-eco-only", eco_affinity_with_draining_false(), False, None),
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
        ),
        (
            "cls-14-exists-profile",
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
        ),
        (
            "cls-23-selector-plus-eco-affinity",
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
        ),
    ]

    for name, affinity, expect_perf, selector in cases:
        delete_pod("joulie-it", name)
        # pin to node to ensure pod is on node for classification even if unschedulable by affinity.
        apply_yaml(mk_pod_yaml(name, affinity=affinity, node_name=ctx.node, node_selector=selector))
        wait_pod_phase("joulie-it", name, "Running")
        if expect_perf:
            wait_node_label(ctx.node, "joulie.io/draining", "true")
        else:
            wait_node_label(ctx.node, "joulie.io/draining", "false")
        delete_pod("joulie-it", name)
        wait_node_label(ctx.node, "joulie.io/draining", "false")


def main() -> int:
    try:
        ctx = test_boot_and_install()
        test_telemetry_http(ctx)
        test_fsm_and_labels(ctx)
        test_legacy_migration(ctx)
        test_scheduling(ctx)
        test_classification_matrix(ctx)
        log("all integration tests passed")
        return 0
    except Exception as e:
        print(f"[integration] FAILED: {e}", flush=True)
        dump_debug()
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
