"""The functions of Joulie's CI: one per job a pull request runs, and all.

The checks (test, race, envtest, promql, prometheus-otlp-live, helm,
experiments, docs) run the commands of the matching ci.yml or docs-preview.yml
job in pinned images. integration builds local Joulie binaries into container
images, publishes them to an ephemeral in-pipeline registry with `dev*` tags,
starts a 2-node k3s test cluster, and runs the integration suite against the
freshly built artifacts.

The 2-node cluster (server + agent) is required because the controller manager enforces a
per-hardware-family performance floor: with a single managed node, STATIC_HP_FRAC=0
cannot move that node to eco.  With two nodes, one stays in performance (floor)
and the other can freely transition to eco.
"""

from __future__ import annotations

import asyncio
import time
import uuid
from typing import Annotated

import dagger
from dagger import DefaultPath, Doc, Ignore, dag, function, object_type

from . import checks, literals

# The repository: by default the root of the Git repository the module is
# in, so `dagger -m ./ci call test` needs no --source.
Source = Annotated[
    dagger.Directory,
    DefaultPath("/"),
    Ignore(literals.SOURCE_IGNORE),
    Doc("Repository source directory (default: the repository root)."),
]

# What `all` runs, in the order it reports them; prometheus-otlp-live runs
# once per Prometheus configuration. tests/contracts checks that every
# workflow job calls a function listed here.
ALL_CHECKS = (
    "test",
    "race",
    "envtest",
    "promql",
    "prometheus-otlp-live",
    "helm",
    "experiments",
    "docs",
    "integration",
)

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

_K3S_IMAGE = literals.K3S_IMAGE
# The builder and base of the component images, as in Dockerfile.
_GOLANG_IMAGE = literals.GOLANG_IMAGE
_DISTROLESS_IMAGE = literals.DISTROLESS_IMAGE
_LOCAL_REGISTRY_ALIAS = "registry"
_LOCAL_REGISTRY_HOST = "joulie-registry.local"
_LOCAL_REGISTRY_PORT = 5000
_DEFAULT_REGISTRY_REPO = f"{_LOCAL_REGISTRY_HOST}:{_LOCAL_REGISTRY_PORT}/joulie"

# Stable node names registered in Kubernetes via --node-name.
# Both nodes form one hardware family, and the policy keeps one performance node
# per family even when STATIC_HP_FRAC=0. It picks the first name, "k3s-server" <
# "k3s-worker-0", and keeps that node on later reconciles, so k3s-server stays in
# performance and k3s-worker-0 goes to eco.
K3S_SERVER_NODE = "k3s-server"
K3S_WORKER_NODE = "k3s-worker-0"

# Scheduler configuration that registers the Joulie scheduler extender, read
# from the repository source; the simulated cluster harness mounts the same
# file into its k3s server. The extender runs as a pod (deployed by Helm);
# kube-scheduler retries if the extender is not yet available at startup.
_SCHEDULER_CONFIG_PATH = "scripts/simcluster/manifests/scheduler-config.yaml"


@object_type
class JoulieCi:
    """Joulie's CI: every job a pull request runs, as a function."""

    @function
    async def test(self, source: Source) -> str:
        """The test job of ci.yml.

        go mod tidy -diff for both modules, make verify-manifests and
        verify-fmt, go build and go vet, and go test ./..., with jq,
        shellcheck and fuse3 installed and the FUSE tests required
        (JOULIE_REQUIRE_FUSE=1): the container may mount, and the CI user
        mounts through fusermount3, as on a GitHub runner. The script tests
        run under the image's bash 5.2, the runner's major version.
        """
        # fusermount3 -u reads /etc/mtab, which Ubuntu links to the mount
        # table and the Debian image lacks.
        container = checks.go_container(
            source,
            packages=("jq", "shellcheck", "fuse3"),
            setup=("ln -sf ../proc/self/mounts /etc/mtab",),
        )
        script = """
step() { printf '\\n==> %s\\n' "$*"; }
step go mod tidy -diff; go mod tidy -diff; (cd tests/promql && go mod tidy -diff)
step make verify-manifests; make verify-manifests
step make verify-fmt; make verify-fmt
step go build ./...; go build ./...
step go vet ./...; go vet ./...
step go test ./...; JOULIE_REQUIRE_FUSE=1 go test ./...
"""
        return await checks.run(container, script, privileged=True)

    @function
    async def race(self, source: Source) -> str:
        """go test -race over the binaries (./cmd/...) and the simulator, for
        their caches and goroutines: the simulator's pod poll and workload
        tick share node state."""
        return await checks.run(checks.go_container(source), "go test -race ./cmd/...\ngo test -race -timeout 30m ./simulator/cmd/simulator/")

    @function
    async def envtest(self, source: Source) -> str:
        """The envtest job of ci.yml: make test-envtest, which downloads the
        control plane binaries into bin/ and runs tests/envtest."""
        return await checks.run(checks.go_container(source), "make test-envtest")

    @function
    async def promql(self, source: Source) -> str:
        """The promql job of ci.yml: make test-promql, the tests/promql module."""
        return await checks.run(checks.go_container(source), "make test-promql")

    @function
    async def prometheus_otlp_live(
        self,
        source: Source,
        config: Annotated[
            str, Doc("Prometheus OTLP settings: promoted, unpromoted, utf8 or utf8-unpromoted.")
        ] = "promoted",
    ) -> str:
        """One configuration of the prometheus-otlp-live job of ci.yml.

        Starts the Prometheus that scripts/simcluster/versions.env pins, with
        the OTLP receiver and the configuration's settings, and runs the live
        input tests (build tag promlive) against it.
        """
        if config not in literals.PROMETHEUS_OTLP_CONFIGS:
            raise ValueError(f"config {config!r} is not one of {', '.join(literals.PROMETHEUS_OTLP_CONFIGS)}")
        versions = await checks.versions_env(source)
        prometheus_config = (
            "storage:\n  tsdb:\n    out_of_order_time_window: 30m\n"
            f"otlp: {literals.PROMETHEUS_OTLP_CONFIGS[config]}\n"
        )
        prometheus = (
            dag.container()
            .from_(f"{versions['PROMETHEUS_IMAGE']}@{versions['PROMETHEUS_DIGEST']}")
            .with_new_file("/etc/prometheus/prometheus.yml", prometheus_config)
            .with_exposed_port(9090)
            .as_service(
                args=["--config.file=/etc/prometheus/prometheus.yml", "--web.enable-otlp-receiver"],
                use_entrypoint=True,
            )
        )
        container = (
            checks.go_container(source)
            .with_service_binding("prometheus", prometheus)
            .with_env_variable("JOULIE_PROM_URL", "http://prometheus:9090")
        )
        script = """
for _ in $(seq 1 60); do curl -sf "$JOULIE_PROM_URL/-/ready" >/dev/null && break; sleep 1; done
go test -tags promlive -count=1 -run Live -v ./pkg/controller/inputs/
"""
        return await checks.run(container, script)

    @function
    async def helm(self, source: Source) -> str:
        """The helm job of ci.yml: helm lint of both charts and
        scripts/verify-chart-renders.sh, with the Helm release
        scripts/simcluster/versions.env pins."""
        versions = await checks.versions_env(source)
        container = checks.go_container(
            source, setup=(checks.helm_setup(versions["HELM_VERSION"]),)
        ).with_env_variable("KUBECONFIG", "/dev/null")
        script = """
helm version --short
helm lint charts/joulie
helm lint charts/joulie-simulator
bash scripts/verify-chart-renders.sh --verbose
"""
        return await checks.run(container, script)

    @function
    async def experiments(self, source: Source) -> str:
        """The experiments job of ci.yml: the experiments' requirements and
        make test-experiments, on Python 3.12."""
        container = (
            dag.container()
            .from_(literals.PYTHON_IMAGE)
            .with_mounted_cache("/root/.cache/pip", dag.cache_volume("joulie-ci-pip"))
            .with_directory("/src", source)
            .with_workdir("/src")
        )
        script = """
pip install --quiet --root-user-action=ignore $(printf -- '-r %s ' experiments/*/requirements.txt)
make test-experiments
"""
        return await checks.run(container, script)

    @function
    def docs(
        self,
        source: Source,
        base_url: Annotated[str, Doc("Hugo baseURL; empty keeps hugo.yaml's.")] = "",
        versions_config: Annotated[
            dagger.File | None,
            Doc("A Docsy versions config added after hugo.yaml, as docs-preview.yml generates it."),
        ] = None,
    ) -> dagger.Directory:
        """The build of docs-preview.yml: the website with Hugo extended,
        Docsy and PostCSS. Returns the built site; deploying it stays in the
        workflow."""
        version = literals.HUGO_VERSION
        tarball = f"hugo_extended_{version}_linux-amd64.tar.gz"
        go_root = dag.container().from_(literals.GOLANG_IMAGE).directory("/usr/local/go")
        container = (
            dag.container()
            .from_(literals.NODE_IMAGE)
            .with_directory("/usr/local/go", go_root)
            .with_env_variable("PATH", "/usr/local/go/bin:${PATH}", expand=True)
            .with_exec(
                [
                    "sh",
                    "-c",
                    f"cd /tmp && curl -fsSLO https://github.com/gohugoio/hugo/releases/download/v{version}/{tarball}"
                    f" && echo '{literals.HUGO_SHA256}  {tarball}' | sha256sum -c -"
                    f" && tar -xzf {tarball} hugo && install hugo /usr/local/bin/hugo",
                ]
            )
            .with_env_variable("HUGO_CACHEDIR", "/hugo-cache")
            .with_mounted_cache("/hugo-cache", dag.cache_volume("joulie-ci-hugo"))
            .with_directory("/src", source)
            .with_workdir("/src/website")
            .with_exec(["npm", "install", "--no-save", "--silent", "postcss", "postcss-cli", "autoprefixer"])
            .with_exec(["hugo", "mod", "get", f"github.com/google/docsy@{literals.DOCSY_VERSION}"])
            .with_exec(
                ["hugo", "mod", "get", f"github.com/google/docsy/dependencies@{literals.DOCSY_DEPENDENCIES_VERSION}"]
            )
        )
        build = ["hugo", "--gc", "--minify"]
        if base_url:
            build += ["--baseURL", base_url]
        config = "hugo.yaml"
        if versions_config is not None:
            container = container.with_file("/tmp/docsy-versions.yaml", versions_config)
            config += ",/tmp/docsy-versions.yaml"
        build += ["--config", config]
        return container.with_exec(build).directory("/src/website/public")

    def _vulnscan(self, source: dagger.Directory) -> dagger.Container:
        """The container that ran ci/scripts/vulnscan.sh into /tmp/vulnscan."""
        return (
            checks.go_container(
                source,
                packages=("jq",),
                setup=(checks.osv_scanner_setup(literals.OSV_SCANNER_VERSION, literals.OSV_SCANNER_SHA256),),
            )
            .with_exec(["go", "install", f"golang.org/x/vuln/cmd/govulncheck@{literals.GOVULNCHECK_VERSION}"])
            .with_env_variable("PATH", "/home/ci/go/bin:${PATH}", expand=True)
            .with_env_variable("VULN_MANIFESTS", " ".join(literals.VULN_MANIFESTS))
            .with_env_variable("GOVULNCHECK_MODULES", " ".join(literals.GOVULNCHECK_MODULES))
            .with_exec(["bash", "ci/scripts/vulnscan.sh", "/tmp/vulnscan"])
        )

    @function
    async def vulnscan(self, source: Source) -> str:
        """The vulnscan job: osv-scanner over every dependency manifest and
        govulncheck over the Go modules (ci/scripts/vulnscan.sh). Returns the
        summary; fails, with it, when the code reaches a vulnerability that
        osv-scanner.toml does not ignore. Not in all: an advisory published
        today fails it whatever the change, so the pull request gate runs it
        only when a manifest changes."""
        container = self._vulnscan(source)
        summary = await container.file("/tmp/vulnscan/summary.txt").contents()
        if (await container.file("/tmp/vulnscan/status").contents()).strip() != "0":
            raise RuntimeError(summary)
        return summary

    @function
    def vulnscan_report(self, source: Source) -> dagger.Directory:
        """The files of the scan, findings or not: osv.json, osv.sarif,
        govulncheck-<module>.json and .sarif, summary.txt and status. The
        nightly scan uploads the SARIF files and fixes from osv.json."""
        return self._vulnscan(source).directory("/tmp/vulnscan")

    @function
    def vulnfix(
        self,
        source: Source,
        report: Annotated[dagger.Directory, Doc("The files of vulnscan-report.")],
    ) -> dagger.Directory:
        """Applies the fixes of a scan with scripts/vulnfix.sh: the
        osv-scanner findings, and the standard library ones govulncheck says
        the code reaches, which need a toolchain bump. Returns report.md, the
        pull request body, and changes/, the files it changed at their
        repository paths."""
        modules = []
        for module in literals.GOVULNCHECK_MODULES:
            name = "root" if module == "." else module.replace("/", "-")
            modules.append(f"{module}=/tmp/report/govulncheck-{name}.json")
        script = f"""
bash scripts/vulnfix.sh /tmp/report/osv.json /tmp/vulnfix/report.md {" ".join(modules)}
# The index holds the files as they were before the fixes.
for f in $(git diff --name-only; git ls-files --others --exclude-standard); do
  mkdir -p "/tmp/vulnfix/changes/$(dirname "$f")"
  cp "$f" "/tmp/vulnfix/changes/$f"
done
"""
        container = (
            checks.go_container(source, packages=("jq",))
            .with_directory("/tmp/report", report)
            .with_exec(["mkdir", "-p", "/tmp/vulnfix/changes"])
            .with_exec(["bash", "-euo", "pipefail", "-c", script])
        )
        return container.directory("/tmp/vulnfix")

    @function
    async def all(self, source: Source) -> str:
        """Every check of ALL_CHECKS at once, prometheus-otlp-live in each
        configuration. Returns one PASS line per check, with its duration;
        when a check failed, fails with every line and the end of each
        failed check's output."""
        jobs = []
        for name in ALL_CHECKS:
            if name == "prometheus-otlp-live":
                for config in literals.PROMETHEUS_OTLP_CONFIGS:
                    jobs.append((f"{name} ({config})", lambda c=config: self.prometheus_otlp_live(source, c)))
            elif name == "docs":
                jobs.append((name, lambda: self.docs(source).entries()))
            else:
                jobs.append((name, lambda n=name: getattr(self, n.replace("-", "_"))(source)))

        async def timed(label, run):
            start = time.monotonic()
            try:
                await run()
                return label, None, time.monotonic() - start
            except Exception as err:  # noqa: BLE001 - every failure is reported
                return label, err, time.monotonic() - start

        results = await asyncio.gather(*(timed(label, run) for label, run in jobs))
        lines = [f"{'FAIL' if err else 'PASS'}  {label}  ({secs:.0f}s)" for label, err, secs in results]
        failed = [(label, err) for label, err, _ in results if err]
        summary = "\n".join(lines)
        if failed:
            details = "\n\n".join(f"==> {label}\n{checks.failure_text(err)}" for label, err in failed)
            raise RuntimeError(f"{len(failed)} of {len(results)} checks failed\n{summary}\n\n{details}")
        return summary + "\n"

    def _local_registry_service(self) -> dagger.Service:
        """Return an ephemeral OCI registry service for this Dagger session."""
        return (
            dag.container()
            .from_(literals.REGISTRY_IMAGE)
            .with_env_variable("REGISTRY_STORAGE_DELETE_ENABLED", "true")
            .with_exposed_port(_LOCAL_REGISTRY_PORT)
            .as_service(use_entrypoint=True)
        )

    def _registry_endpoint(self, registry_repo: str) -> str:
        """Return the mirror endpoint k3s should use to reach the local registry."""
        _ = registry_repo  # kept for symmetry with callers and future overrides
        return f"http://{_LOCAL_REGISTRY_ALIAS}:{_LOCAL_REGISTRY_PORT}"

    def _registries_yaml(self, registry_repo: str) -> str:
        """Return a K3s registries.yaml mapping the fake registry host to the sidecar."""
        registry_host = registry_repo.split("/")[0]
        endpoint = self._registry_endpoint(registry_repo)
        return (
            "mirrors:\n"
            f'  "{registry_host}":\n'
            "    endpoint:\n"
            f'      - "{endpoint}"\n'
        )

    async def _publish_component_image(
        self,
        source: dagger.Directory,
        component: str,
        registry_repo: str,
        tag: str,
        registry: dagger.Service,
    ) -> str:
        """Build and publish one component image from repo source.

        Uses crane to push inside a container with the registry service bound,
        since Dagger's publish() runs in buildkit which cannot resolve ephemeral
        registry hostnames.
        """
        go_mod_cache = dag.cache_volume("joulie-go-mod-cache")
        go_build_cache = dag.cache_volume("joulie-go-build-cache")

        go_base = (
            dag.container()
            .from_(_GOLANG_IMAGE)
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
            .from_(_DISTROLESS_IMAGE)
            .with_file("/joulie", builder.file("/out/joulie"))
            .with_entrypoint(["/joulie"])
        )
        # Export the image as an OCI tarball, then push it with crane from a
        # container that has the registry service bound (so the hostname resolves).
        # Extract the path portion from registry_repo (strip the host prefix).
        _, _, repo_path = registry_repo.partition("/")
        if not repo_path:
            repo_path = registry_repo
        tarball = runtime.as_tarball()
        push_ref = f"{_LOCAL_REGISTRY_ALIAS}:{_LOCAL_REGISTRY_PORT}/{repo_path}/joulie-{component}:{tag}"
        pusher = (
            dag.container()
            .from_(literals.CRANE_IMAGE)
            .with_user("0")
            .with_service_binding(_LOCAL_REGISTRY_ALIAS, registry)
            .with_file("/tmp/image.tar", tarball)
            .with_exec(["crane", "push", "--insecure", "/tmp/image.tar", push_ref])
        )
        await pusher.stdout()
        # Return the ref as k3s will see it (via registries.yaml mirror mapping).
        return f"{registry_repo}/joulie-{component}:{tag}"

    def _k3s_server_service(
        self,
        config_cache: dagger.CacheVolume,
        registry: dagger.Service,
        registries_yaml: str,
        scheduler_config: dagger.File,
        run_id: str,
        token: str,
    ) -> dagger.Service:
        """Return the k3s server as a Dagger service.

        Uses a static --token (derived from run_id) so the agent can join without
        reading any files from the server's filesystem.  config_cache is mounted
        at /etc/rancher/k3s so k3s writes k3s.yaml there; a separate lazy
        container reads the kubeconfig from the same cache volume later.
        scheduler_config is the repository's _SCHEDULER_CONFIG_PATH.
        """
        return (
            dag.container()
            .from_(_K3S_IMAGE)
            .with_new_file("/usr/bin/entrypoint.sh", _ENTRYPOINT, permissions=0o755)
            .with_entrypoint(["entrypoint.sh"])
            # config_cache is written by k3s (k3s.yaml) and read by _kubeconfig_file().
            .with_mounted_cache("/etc/rancher/k3s", config_cache)
            .with_new_file("/tmp/registries.yaml", registries_yaml)
            .with_exec(["cp", "/tmp/registries.yaml", "/etc/rancher/k3s/registries.yaml"])
            .with_file("/tmp/scheduler-config.yaml", scheduler_config)
            .with_exec(["cp", "/tmp/scheduler-config.yaml", "/etc/rancher/k3s/scheduler-config.yaml"])
            .with_mounted_temp("/etc/lib/cni")
            .with_mounted_temp("/var/lib/kubelet")
            .with_mounted_temp("/var/lib/rancher")
            .with_mounted_temp("/var/log")
            .with_service_binding(_LOCAL_REGISTRY_ALIAS, registry)
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
                        "--disable-default-registry-endpoint "
                        "--disable traefik "
                        "--disable metrics-server "
                        "--egress-selector-mode=disabled "
                        "--kube-scheduler-arg=--config=/etc/rancher/k3s/scheduler-config.yaml"
                    ),
                ],
                insecure_root_capabilities=True,
                use_entrypoint=True,
            )
        )

    def _k3s_worker_service(
        self,
        started_server: dagger.Service,
        registry: dagger.Service,
        registries_yaml: str,
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
            .with_new_file("/tmp/registries.yaml", registries_yaml)
            .with_exec(["mkdir", "-p", "/etc/rancher/k3s"])
            .with_exec(["cp", "/tmp/registries.yaml", "/etc/rancher/k3s/registries.yaml"])
            .with_mounted_temp("/etc/lib/cni")
            .with_mounted_temp("/var/lib/kubelet")
            .with_mounted_temp("/var/lib/rancher")
            .with_mounted_temp("/var/log")
            .with_service_binding("k3s-server", started_server)
            .with_service_binding(_LOCAL_REGISTRY_ALIAS, registry)
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
                        "--disable-default-registry-endpoint "
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

        This is lazily evaluated. Actual execution happens when the file is used
        by the client container, by which point the server has been running long
        enough to have written k3s.yaml.
        """
        return (
            dag.container()
            .from_(literals.ALPINE_IMAGE)
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
        source: Source,
        registry_repo: Annotated[str, Doc("OCI repository prefix served by the in-pipeline registry.")] = _DEFAULT_REGISTRY_REPO,
        tag: Annotated[str, Doc("Image tag (must start with 'dev'). Auto-generated when empty.")] = "",
        it_scope: Annotated[str, Doc("Integration scope: all/full or gpu-only.")] = "all",
    ) -> str:
        """
        Build/push local Joulie images and run the 2-node k3s integration suite.

        Workflow:
        1. Build `agent`, `controller-manager`, and `scheduler` images from current repo source.
        2. Publish images to an ephemeral in-pipeline registry using a `dev*` tag.
        3. Start a 2-node k3s cluster (server + agent) with a static join token.
        4. Install Joulie Helm chart with the freshly published images.
        5. Execute integration tests and return runner stdout.

        Node roles (deterministic via --node-name and the policy's name order):
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
        registries_yaml = self._registries_yaml(registry_repo)

        # --- Start registry and cluster nodes so containerd warms up during image builds ---
        # start() returns as soon as the container is scheduled (near-instant);
        # actual k3s readiness is checked later via exposed-port polling.
        # No deadlock: server has no dependency on worker; worker retries against
        # the server until it answers (503 → ready), which is fine.
        registry_svc = self._local_registry_service()
        started_registry = await registry_svc.start()
        server_svc = self._k3s_server_service(
            config_cache,
            started_registry,
            registries_yaml,
            source.file(_SCHEDULER_CONFIG_PATH),
            run_id,
            cluster_token,
        )
        worker_svc = self._k3s_worker_service(server_svc, started_registry, registries_yaml, cluster_token)
        started_server, started_worker = await asyncio.gather(
            server_svc.start(),
            worker_svc.start(),
        )

        # --- Build and publish images (runs while cluster warms up) ---
        # Push images via crane inside a container with the registry service
        # bound, since Dagger's publish() runs in buildkit which cannot resolve
        # ephemeral service hostnames.
        agent_ref, controller_manager_ref, scheduler_ref = await asyncio.gather(
            self._publish_component_image(source, "agent", registry_repo, tag, started_registry),
            self._publish_component_image(source, "controller-manager", registry_repo, tag, started_registry),
            self._publish_component_image(source, "scheduler", registry_repo, tag, started_registry),
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
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_REPOSITORY", f"{registry_repo}/joulie-controller-manager")
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_REPOSITORY", f"{registry_repo}/joulie-scheduler")
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_AGENT_IMAGE_REF", agent_ref)
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_REF", controller_manager_ref)
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_REF", scheduler_ref)
            .with_env_variable("IT_SCOPE", it_scope)
            .with_workdir("/src")
            .with_exec(["bash", "ci/scripts/run-integration.sh"])
        )

        return await client.stdout()

    @function
    async def integration_new_components(
        self,
        source: Source,
        registry_repo: Annotated[str, Doc("OCI repository prefix served by the in-pipeline registry.")] = _DEFAULT_REGISTRY_REPO,
        tag: Annotated[str, Doc("Image tag (must start with 'dev'). Auto-generated when empty.")] = "",
    ) -> str:
        """
        Build/push Joulie images, start a single-node k3s cluster, install the Helm
        chart with new CRDs, and run the Go integration tests in tests/integration/.

        Workflow:
        1. Build `agent`, `controller-manager`, and `scheduler` images from current repo source.
        2. Publish images to an ephemeral in-pipeline registry using a `dev*` tag.
        3. Start a single-node k3s cluster (server only, no worker).
        4. Install Joulie Helm chart so CRDs are registered.
        5. Run `go test -tags=integration ./tests/integration/...` and return stdout.

        Single-node design rationale:
          The new integration tests (IT-ARCH-01, IT-TWIN-01, IT-PROF-01, IT-SCHED-01/02,
          IT-FSM-01) do not require the 2-node performance-floor logic.  A single k3s
          server is faster to start and sufficient for CRD lifecycle, object write, and
          scheduler-logic assertions.
        """
        if not tag:
            tag = f"dev-{uuid.uuid4().hex[:12]}"
        if not tag.startswith("dev"):
            raise ValueError("integration_new_components image tag must start with 'dev'")

        run_id = tag
        cluster_token = f"joulie-ci-new-{run_id}"
        config_cache = dag.cache_volume(f"k3s-config-new-{run_id}")
        registries_yaml = self._registries_yaml(registry_repo)

        # --- Start local registry and single-node k3s server ---
        registry_svc = self._local_registry_service()
        started_registry = await registry_svc.start()
        server_svc = self._k3s_server_service(
            config_cache,
            started_registry,
            registries_yaml,
            source.file(_SCHEDULER_CONFIG_PATH),
            run_id,
            cluster_token,
        )
        started_server = await server_svc.start()

        # --- Build and publish images concurrently ---
        agent_ref, controller_manager_ref, scheduler_ref = await asyncio.gather(
            self._publish_component_image(source, "agent", registry_repo, tag, started_registry),
            self._publish_component_image(source, "controller-manager", registry_repo, tag, started_registry),
            self._publish_component_image(source, "scheduler", registry_repo, tag, started_registry),
        )

        # --- Kubeconfig ---
        kubeconfig = self._kubeconfig_file(config_cache, run_id, started_server)

        go_mod_cache = dag.cache_volume("joulie-go-mod-cache")
        go_build_cache = dag.cache_volume("joulie-go-build-cache")

        # --- Install CRDs via Helm then run Go integration tests ---
        helm_cli = dag.helm().with_kubeconfig_file(kubeconfig)
        client = (
            helm_cli.container()
            .with_exec([
                "apk", "add", "--no-cache",
                "bash", "go", "git", "ca-certificates", "sed", "kubectl",
            ])
            .with_mounted_directory("/src", source)
            .with_file("/tmp/kubeconfig.yaml", kubeconfig)
            .with_env_variable("KUBECONFIG", "/tmp/kubeconfig.yaml")
            .with_service_binding("k3s", started_server)
            .with_exec(["sed", "-i",
                r"s|server: https://[^:]*:6443|server: https://k3s:6443|",
                "/tmp/kubeconfig.yaml"])
            # Wait for API server to be ready before installing chart.
            .with_exec(["sh", "-c",
                "for i in $(seq 1 120); do kubectl get nodes >/dev/null 2>&1 && break; sleep 1; done"])
            # Install Helm chart so Joulie CRDs are registered.
            .with_env_variable("JOULIE_AGENT_IMAGE_REPOSITORY", f"{registry_repo}/joulie-agent")
            .with_env_variable("JOULIE_AGENT_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_REPOSITORY", f"{registry_repo}/joulie-controller-manager")
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_REPOSITORY", f"{registry_repo}/joulie-scheduler")
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_AGENT_IMAGE_REF", agent_ref)
            .with_env_variable("JOULIE_CONTROLLER_MANAGER_IMAGE_REF", controller_manager_ref)
            .with_env_variable("JOULIE_SCHEDULER_IMAGE_REF", scheduler_ref)
            .with_exec(["sh", "-c",
                "helm upgrade --install joulie /src/charts/joulie "
                "--set agent.image.repository=${JOULIE_AGENT_IMAGE_REPOSITORY} "
                "--set agent.image.tag=${JOULIE_AGENT_IMAGE_TAG} "
                "--set controllerManager.image.repository=${JOULIE_CONTROLLER_MANAGER_IMAGE_REPOSITORY} "
                "--set controllerManager.image.tag=${JOULIE_CONTROLLER_MANAGER_IMAGE_TAG} "
                "--set schedulerExtender.image.repository=${JOULIE_SCHEDULER_IMAGE_REPOSITORY} "
                "--set schedulerExtender.image.tag=${JOULIE_SCHEDULER_IMAGE_TAG} "
                "--wait --timeout=5m"])
            # Go cache volumes for faster repeated runs.
            .with_mounted_cache("/go/pkg/mod", go_mod_cache)
            .with_mounted_cache("/root/.cache/go-build", go_build_cache)
            .with_env_variable("GOMODCACHE", "/go/pkg/mod")
            .with_env_variable("GOCACHE", "/root/.cache/go-build")
            .with_workdir("/src")
            # Run the new Go integration tests with the integration build tag.
            .with_exec(["go", "test", "-v", "-tags=integration",
                "-timeout=10m", "./tests/integration/..."])
        )

        return await client.stdout()
