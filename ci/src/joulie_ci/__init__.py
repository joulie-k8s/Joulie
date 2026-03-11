from __future__ import annotations

import uuid

import dagger
from dagger import dag, function, object_type


@object_type
class JoulieCi:
    async def _publish_component_image(
        self,
        source: dagger.Directory,
        component: str,
        registry_repo: str,
        tag: str,
        username: dagger.Secret,
        password: dagger.Secret,
    ) -> str:
        host = registry_repo.split("/")[0]
        user = await username.plaintext()

        builder = (
            dag.container()
            .from_("golang:1.22")
            .with_workdir("/src")
            .with_mounted_directory("/src", source)
            .with_exec(["go", "mod", "download"])
            .with_exec(
                [
                    "sh",
                    "-lc",
                    f"CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie ./cmd/{component}",
                ]
            )
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
        source: dagger.Directory,
        username: dagger.Secret,
        password: dagger.Secret,
        registry_repo: str = "registry.cern.ch/mbunino/joulie",
        tag: str = "",
    ) -> str:
        """
        Run Joulie integration tests on a lightweight k3s cluster started as a Dagger service.
        """
        if not tag:
            tag = f"dev-{uuid.uuid4().hex[:12]}"
        if not tag.startswith("dev"):
            raise ValueError("integration image tag must start with 'dev'")

        agent_ref = await self._publish_component_image(source, "agent", registry_repo, tag, username, password)
        operator_ref = await self._publish_component_image(source, "operator", registry_repo, tag, username, password)

        run_id = uuid.uuid4().hex
        k3s_state = dag.cache_volume(f"joulie-ci-k3s-state-{run_id}")
        k3s_out = dag.cache_volume(f"joulie-ci-k3s-out-{run_id}")

        k3s = (
            dag.container()
            .from_("rancher/k3s:v1.30.4-k3s1")
            .with_mounted_cache("/var/lib/rancher/k3s", k3s_state)
            .with_mounted_cache("/output", k3s_out)
            .with_exposed_port(6443)
            .with_exec(
                [
                    "server",
                    "--disable=traefik",
                    "--snapshotter=native",
                    "--write-kubeconfig=/output/kubeconfig.yaml",
                    "--write-kubeconfig-mode=644",
                ],
                use_entrypoint=True,
                insecure_root_capabilities=True,
            )
            .as_service()
        )

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
            .with_mounted_cache("/k3s-output", k3s_out)
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
