"""Dagger module entrypoints for Joulie CI integration testing.

This module builds local Joulie binaries into container images, publishes them
to a registry with `dev*` tags, starts a 2-node k3s test cluster, and runs the
integration suite against the freshly built artifacts.

The 2-node cluster (server + agent) is required because the operator enforces a
per-hardware-family performance floor: with a single managed node, STATIC_HP_FRAC=0
cannot move that node to eco.  With two nodes, one stays in performance (floor)
and the other can freely transition to eco.
"""

from __future__ import annotations

import uuid
from typing import Annotated

import dagger
from dagger import Doc, dag, function, object_type

# Copied from github.com/marcosnils/daggerverse/k3s main.go.
# Evacuates the root cgroup before k3s starts so cgroup-v2 nesting works inside
# Dagger containers (where k3s is not PID 1 and cannot do this itself).
_ENTRYPOINT = """\
#!/bin/sh

set -o errexit
set -o nounset

if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  echo "[$(date -Iseconds)] [CgroupV2 Fix] Evacuating Root Cgroup ..."
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || :
  sed -e 's/ / +/g' -e 's/^/+/' <"/sys/fs/cgroup/cgroup.controllers" >"/sys/fs/cgroup/cgroup.subtree_control"
  echo "[$(date -Iseconds)] [CgroupV2 Fix] Done"
fi

exec "$@"
"""

# Pinned to the version observed in CI logs (2026-03-13).
_K3S_IMAGE = "rancher/k3s:v1.34.1-k3s1"

# Stable node names registered in Kubernetes via --node-name.
# Alphabetically "k3s-server" < "k3s-worker-0", so the operator's density sort
# (tie-broken lexicographically) always assigns performance to k3s-server and
# eco to k3s-worker-0 when STATIC_HP_FRAC=0.
K3S_SERVER_NODE = "k3s-server"
K3S_WORKER_NODE = "k3s-worker-0"


@object_type
class JoulieCi:
    """Dagger object exposing CI integration workflows for Joulie."""

    async def _publish_component_image(
        self,
        source: dagger.Directory,
        component: str,
        registry_repo: str,
        tag: str,
        username: dagger.Secret,
        password: dagger.Secret,
    ) -> str:
        """Build and publish one component image from repo source."""
        host = registry_repo.split("/")[0]
        user = await username.plaintext()
        go_mod_cache = dag.cache_volume("joulie-go-mod-cache")
        go_build_cache = dag.cache_volume("joulie-go-build-cache")

        go_base = (
            dag.container()
            .from_("golang:1.23")
            .with_workdir("/src")
            .with_env_variable("CGO_ENABLED", "0")
            .with_env_variable("GOOS", "linux")
            .with_env_variable("GOARCH", "amd64")
            .with_env_variable("GOMODCACHE", "/go/pkg/mod")
            .with_env_variable("GOCACHE", "/root/.cache/go-build")
            .with_mounted_cache("/go/pkg/mod", go_mod_cache)
            .with_mounted_cache("/root/.cache/go-build", go_build_cache)
        )
        deps = (
            go_base
            .with_file("/src/go.mod", source.file("go.mod"))
            .with_file("/src/go.sum", source.file("go.sum"))
            .with_exec(["go", "mod", "download"])
        )
        builder = (
            deps
            .with_mounted_directory("/src", source)
            .with_exec(["go", "build", "-o", "/out/joulie", f"./cmd/{component}"])
        )
        runtime = (
            dag.container()
            .from_("gcr.io/distroless/static:nonroot")
            .with_file("/joulie", builder.file("/out/joulie"))
            .with_entrypoint(["/joulie"])
            .with_registry_auth(host, user, password)
        )
        ref = f"{registry_repo}/joulie-{component}:{tag}"
        await runtime.publish(ref)
        return ref

    def _k3s_server_service(
        self,
        config_cache: dagger.CacheVolume,
        run_id: str,
        token: str,
    ) -> dagger.Service:
        """Return the k3s server as a Dagger service.

        Uses a static --token (derived from run_id) so the agent can join without
        reading any files from the server's filesystem.  config_cache is mounted
        at /etc/rancher/k3s so k3s writes k3s.yaml there; a separate lazy
        container reads the kubeconfig from the same cache volume later.
        """
        return (
            dag.container()
            .from_(_K3S_IMAGE)
            .with_new_file("/usr/bin/entrypoint.sh", _ENTRYPOINT, permissions=0o755)
            .with_entrypoint(["entrypoint.sh"])
            # config_cache is written by k3s (k3s.yaml) and read by _kubeconfig_file().
            .with_mounted_cache("/etc/rancher/k3s", config_cache)
            .with_mounted_temp("/etc/lib/cni")
            .with_mounted_temp("/var/lib/kubelet")
            .with_mounted_temp("/var/lib/rancher")
            .with_mounted_temp("/var/log")
            # Wipe any stale kubeconfig from a previous run sharing the cache volume.
            .with_env_variable("CACHEBUST", run_id)
            .with_exec(["rm", "-f", "/etc/rancher/k3s/k3s.yaml"])
            .with_exposed_port(6443)
            .as_service(
                args=[
                    "sh", "-c",
                    (
                        "k3s server "
                        "--bind-address $(ip route | grep src | awk '{print $NF}') "
                        "--tls-san k3s-server "
                        "--tls-san k3s "
                        f"--node-name {K3S_SERVER_NODE} "
                        f"--token {token} "
                        # overlayfs-on-overlayfs doesn't work inside Dagger containers
                        # on RHEL 8 (4.18 kernel); native snapshotter avoids the
                        # multi-minute 'Waiting for containerd startup' loop.
                        "--snapshotter native "
                        "--disable traefik "
                        "--disable metrics-server "
                        "--egress-selector-mode=disabled"
                    ),
                ],
                insecure_root_capabilities=True,
                use_entrypoint=True,
            )
        )

    def _k3s_worker_service(
        self,
        started_server: dagger.Service,
        token: str,
    ) -> dagger.Service:
        """Return the k3s worker agent as a Dagger service.

        Connects to the server via the 'k3s-server' service binding hostname,
        which is covered by the server's --tls-san k3s-server certificate.
        Uses the same static token as the server.
        """
        return (
            dag.container()
            .from_(_K3S_IMAGE)
            .with_new_file("/usr/bin/entrypoint.sh", _ENTRYPOINT, permissions=0o755)
            .with_entrypoint(["entrypoint.sh"])
            .with_mounted_temp("/etc/lib/cni")
            .with_mounted_temp("/var/lib/kubelet")
            .with_mounted_temp("/var/lib/rancher")
            .with_mounted_temp("/var/log")
            .with_service_binding("k3s-server", started_server)
            # Expose the kubelet API port so Dagger can health-check the service
            # and properly wire up the service binding lifecycle.  Without an
            # exposed port Dagger may silently skip starting the worker.
            .with_exposed_port(10250)
            .as_service(
                args=[
                    "sh", "-c",
                    (
                        "k3s agent "
                        "--server https://k3s-server:6443 "
                        f"--token {token} "
                        f"--node-name {K3S_WORKER_NODE} "
                        "--snapshotter native"
                    ),
                ],
                insecure_root_capabilities=True,
                use_entrypoint=True,
            )
        )

    def _kubeconfig_file(
        self,
        config_cache: dagger.CacheVolume,
        run_id: str,
        started_server: dagger.Service,
    ) -> dagger.File:
        """Wait for k3s to write its kubeconfig and return it as a Dagger File.

        This is lazily evaluated — actual execution happens when the file is used
        by the client container, by which point the server has been running long
        enough to have written k3s.yaml.
        """
        return (
            dag.container()
            .from_("alpine")
            .with_env_variable("CACHEBUST", run_id)
            .with_mounted_cache("/cache/k3s", config_cache)
            # Keep the server alive while we poll for the kubeconfig.
            .with_service_binding("k3s-server", started_server)
            .with_exec(["sh", "-c",
                "while [ ! -f /cache/k3s/k3s.yaml ]; do "
                "echo 'k3s.yaml not ready, waiting...' && sleep 0.5; done"])
            .with_exec(["cp", "/cache/k3s/k3s.yaml", "k3s.yaml"])
            # Rewrite the server URL to the service-binding hostname so kubectl
            # can reach the API server regardless of what k3s wrote (it may write
            # its internal Dagger short-hostname which isn't DNS-resolvable from
            # other containers).  The cert has --tls-san k3s so TLS validates.
            .with_exec(["sed", "-i",
                r"s|https://[^:]*:6443|https://k3s:6443|g",
                "k3s.yaml"])
            .file("k3s.yaml")
        )

    @function
    async def integration(
        self,
        source: Annotated[dagger.Directory, Doc("Repository source directory.")],
        username: Annotated[dagger.Secret, Doc("Registry username.")],
        password: Annotated[dagger.Secret, Doc("Registry password/token.")],
        registry_repo: Annotated[str, Doc("OCI repository prefix.")] = "registry.cern.ch/mbunino/joulie",
        tag: Annotated[str, Doc("Image tag (must start with 'dev'). Auto-generated when empty.")] = "",
        it_scope: Annotated[str, Doc("Integration scope: all/full or gpu-only.")] = "all",
    ) -> str:
        """
        Build/push local Joulie images and run the 2-node k3s integration suite.

        Workflow:
        1. Build `agent` and `operator` images from current repo source.
        2. Publish images to `registry_repo` using a `dev*` tag.
        3. Start a 2-node k3s cluster (server + agent) with a static join token.
        4. Install Joulie Helm chart with the freshly published images.
        5. Execute integration tests and return runner stdout.

        Node roles (deterministic via --node-name + operator sort):
          k3s-server   → always stays in performance (family floor)
          k3s-worker-0 → transitions to eco when STATIC_HP_FRAC=0
        """
        if not tag:
            tag = f"dev-{uuid.uuid4().hex[:12]}"
        if not tag.startswith("dev"):
            raise ValueError("integration image tag must start with 'dev'")
        if not it_scope:
            it_scope = "all"

        run_id = tag  # unique per pipeline run
        # Static join token: known upfront, no filesystem reading required.
        # Derived from run_id so it's unique per run (avoids stale token issues
        # if cache volumes happen to be reused).
        cluster_token = f"joulie-ci-{run_id}"

        # config_cache is mounted in the server so k3s writes k3s.yaml there;
        # _kubeconfig_file() reads it from the same volume (lazy evaluation).
        config_cache = dag.cache_volume(f"k3s-config-{run_id}")

        # --- Start both cluster nodes so containerd warms up during image builds ---
        # start() returns as soon as the container is scheduled (near-instant);
        # actual k3s readiness is checked later via exposed-port polling.
        # No deadlock: server has no dependency on worker; worker retries against
        # the server until it answers (503 → ready), which is fine.
        import asyncio
        server_svc = self._k3s_server_service(config_cache, run_id, cluster_token)
        worker_svc = self._k3s_worker_service(server_svc, cluster_token)
        started_server, started_worker = await asyncio.gather(
            server_svc.start(),
            worker_svc.start(),
        )

        # --- Build and publish images (runs while cluster warms up) ---
        agent_ref, operator_ref = await asyncio.gather(
            self._publish_component_image(source, "agent", registry_repo, tag, username, password),
            self._publish_component_image(source, "operator", registry_repo, tag, username, password),
        )

        # --- Kubeconfig (lazily evaluated when the client container runs) ---
        kubeconfig = self._kubeconfig_file(config_cache, run_id, started_server)

        # --- Run integration suite ---
        helm_cli = dag.helm().with_kubeconfig_file(kubeconfig)
        client = (
            helm_cli.container()
            .with_exec([
                "apk", "add", "--no-cache",
                "bash", "python3", "py3-pip",
                "ca-certificates", "git", "sed", "kubectl",
            ])
            .with_mounted_directory("/src", source)
            .with_file("/tmp/kubeconfig.yaml", kubeconfig)
            .with_env_variable("KUBECONFIG", "/tmp/kubeconfig.yaml")
            # Bind both already-running services.  Dagger polls port 10250 on
            # started_worker here, blocking until the worker kubelet is ready.
            .with_service_binding("k3s", started_server)
            .with_service_binding("k3s-worker", started_worker)
            # Rewrite the server URL to the service-binding hostname so kubectl
            # can reach the API server. The cert has --tls-san k3s so TLS validates.
            # Must run AFTER with_file (file exists) and with_service_binding (network ready).
            .with_exec(["sed", "-i",
                r"s|server: https://[^:]*:6443|server: https://k3s:6443|",
                "/tmp/kubeconfig.yaml"])
            .with_env_variable("JOULIE_AGENT_IMAGE_REPOSITORY", f"{registry_repo}/joulie-agent")
            .with_env_variable("JOULIE_AGENT_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_OPERATOR_IMAGE_REPOSITORY", f"{registry_repo}/joulie-operator")
            .with_env_variable("JOULIE_OPERATOR_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_AGENT_IMAGE_REF", agent_ref)
            .with_env_variable("JOULIE_OPERATOR_IMAGE_REF", operator_ref)
            .with_env_variable("IT_SCOPE", it_scope)
            .with_workdir("/src")
            .with_exec(["bash", "ci/scripts/run-integration.sh"])
        )

        return await client.stdout()
