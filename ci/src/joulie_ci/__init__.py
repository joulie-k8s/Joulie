from __future__ import annotations

import uuid

import dagger
from dagger import dag, function, object_type


@object_type
class JoulieCi:
    @function
    async def integration(self, source: dagger.Directory) -> str:
        """
        Run Joulie integration tests on a lightweight k3s cluster started as a Dagger service.
        """
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
            .with_workdir("/src")
            .with_exec(["bash", "ci/scripts/run-integration.sh"])
        )

        return await client.stdout()
