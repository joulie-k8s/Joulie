"""Dagger module entrypoints for Joulie CI integration testing.

This module builds local Joulie binaries into container images, publishes them
to a registry with `dev*` tags, starts a k3s test cluster, and runs the
integration suite against the freshly built artifacts.
"""

from __future__ import annotations

import uuid
from typing import Annotated

import dagger
from dagger import Doc, dag, function, object_type


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
        """Build and publish one component image from repo source.

        Args:
            source: Repository source directory.
            component: Component name (`agent` or `operator`).
            registry_repo: Registry repository prefix.
            tag: Image tag (must start with `dev` in integration flow).
            username: Registry username secret.
            password: Registry password/token secret.

        Returns:
            Fully qualified image reference that was published.
        """
        host = registry_repo.split("/")[0]
        user = await username.plaintext()

        builder = (
            dag.container()
            .from_("golang:1.23")
            .with_workdir("/src")
            .with_mounted_directory("/src", source)
            .with_exec(["go", "version"])
            .with_exec(["go", "mod", "download"])
            .with_env_variable("CGO_ENABLED", "0")
            .with_env_variable("GOOS", "linux")
            .with_env_variable("GOARCH", "amd64")
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

    @function
    async def integration(
        self,
        source: Annotated[dagger.Directory, Doc("Repository source directory mounted at /src inside the CI container.")],
        username: Annotated[dagger.Secret, Doc("Registry username secret used to authenticate image pushes.")],
        password: Annotated[dagger.Secret, Doc("Registry password/token secret used to authenticate image pushes.")],
        registry_repo: Annotated[str, Doc("Target OCI repository prefix (without component suffix).")] = "registry.cern.ch/mbunino/joulie",
        tag: Annotated[str, Doc("Image tag to publish and deploy. Must start with 'dev'. Auto-generated when empty.")] = "",
    ) -> str:
        """
        Build/push local Joulie images and run the k3s integration suite.

        Workflow:
        1. Build `agent` and `operator` images from current repo source.
        2. Publish images to `registry_repo` using a `dev*` tag.
        3. Start a k3s service via the Daggerverse k3s module.
        4. Install Joulie Helm chart with the freshly published images.
        5. Execute integration tests and return runner stdout.
        """
        if not tag:
            tag = f"dev-{uuid.uuid4().hex[:12]}"
        if not tag.startswith("dev"):
            raise ValueError("integration image tag must start with 'dev'")

        agent_ref = await self._publish_component_image(source, "agent", registry_repo, tag, username, password)
        operator_ref = await self._publish_component_image(source, "operator", registry_repo, tag, username, password)

        k3s_mod = dag.k3_s("joulie-ci")
        k3s = k3s_mod.server()
        await k3s.start()
        kubeconfig = k3s_mod.config(local=False)

        kubectl_version = "v1.31.2"
        helm_version = "v3.16.1"
        client = (
            dag.container()
            .from_("python:3.12-slim")
            .with_exec(["apt-get", "update"])
            .with_exec(
                [
                    "apt-get",
                    "install",
                    "-y",
                    "--no-install-recommends",
                    "bash",
                    "curl",
                    "ca-certificates",
                    "git",
                    "sed",
                ]
            )
            .with_exec(
                [
                    "sh",
                    "-lc",
                    f"curl -fsSL -o /usr/local/bin/kubectl "
                    f"https://dl.k8s.io/release/{kubectl_version}/bin/linux/amd64/kubectl && chmod +x /usr/local/bin/kubectl",
                ]
            )
            .with_exec(
                [
                    "sh",
                    "-lc",
                    f"curl -fsSL https://get.helm.sh/helm-{helm_version}-linux-amd64.tar.gz "
                    "| tar -xz -C /tmp && mv /tmp/linux-amd64/helm /usr/local/bin/helm && chmod +x /usr/local/bin/helm",
                ]
            )
            .with_mounted_directory("/src", source)
            .with_file("/tmp/kubeconfig.yaml", kubeconfig)
            .with_env_variable("KUBECONFIG", "/tmp/kubeconfig.yaml")
            .with_service_binding("k3s", k3s)
            .with_env_variable("JOULIE_AGENT_IMAGE_REPOSITORY", f"{registry_repo}/joulie-agent")
            .with_env_variable("JOULIE_AGENT_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_OPERATOR_IMAGE_REPOSITORY", f"{registry_repo}/joulie-operator")
            .with_env_variable("JOULIE_OPERATOR_IMAGE_TAG", tag)
            .with_env_variable("JOULIE_AGENT_IMAGE_REF", agent_ref)
            .with_env_variable("JOULIE_OPERATOR_IMAGE_REF", operator_ref)
            .with_workdir("/src")
            .with_exec(["bash", "ci/scripts/run-integration.sh"])
        )

        return await client.stdout()
