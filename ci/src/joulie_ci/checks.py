"""Containers the checks run in, and the helpers they share."""

from __future__ import annotations

import dagger
from dagger import dag

from . import literals

_HOME = "/home/ci"


async def versions_env(source: dagger.Directory) -> dict[str, str]:
    """The KEY=value lines of scripts/simcluster/versions.env."""
    text = await source.file(literals.VERSIONS_ENV).contents()
    out = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        out[key.strip()] = value.strip().strip('"')
    return out


def go_container(
    source: dagger.Directory,
    packages: tuple[str, ...] = (),
    setup: tuple[str, ...] = (),
) -> dagger.Container:
    """The Go image with the repository at /src.

    Debian packages and the setup commands are installed as root; the
    checks then run as the CI user, with the Go module and build caches as
    cache volumes. /src is a Git repository with every file staged and no
    commit: make verify-manifests diffs against that index, verify-fmt
    lists it, and code that asks for HEAD gets none, as it tolerates.
    """
    uid = str(literals.CI_UID)
    owner = f"{uid}:{uid}"
    c = dag.container().from_(literals.GOLANG_IMAGE)
    if packages:
        c = c.with_exec(
            ["sh", "-c", "apt-get update -qq && apt-get install -y -qq --no-install-recommends " + " ".join(packages)]
        )
    for command in setup:
        c = c.with_exec(["sh", "-c", command])
    # The caches mount below directories the CI user owns, so it can create
    # what Go keeps next to them (pkg/sumdb beside pkg/mod).
    return (
        c.with_exec(["useradd", "--create-home", "--home-dir", _HOME, "--uid", uid, "ci"])
        .with_exec(["install", "-d", "-o", uid, "-g", uid, f"{_HOME}/go", f"{_HOME}/go/pkg", f"{_HOME}/.cache"])
        .with_env_variable("HOME", _HOME)
        .with_env_variable("GOPATH", f"{_HOME}/go")
        .with_env_variable("GOMODCACHE", f"{_HOME}/go/pkg/mod")
        .with_env_variable("GOCACHE", f"{_HOME}/.cache/go-build")
        .with_env_variable("GOTOOLCHAIN", "local")
        .with_mounted_cache(f"{_HOME}/go/pkg/mod", dag.cache_volume("joulie-ci-go-mod"), owner=owner)
        .with_mounted_cache(f"{_HOME}/.cache/go-build", dag.cache_volume("joulie-ci-go-build"), owner=owner)
        .with_directory("/src", source, owner=owner)
        .with_workdir("/src")
        .with_user(uid)
        .with_exec(["sh", "-c", "git init -q && git add -A"])
    )


def helm_setup(version: str) -> str:
    """The command that installs Helm version, checked against the
    checksum get.helm.sh publishes next to it."""
    tarball = f"helm-{version}-linux-amd64.tar.gz"
    return (
        f"cd /tmp && curl -fsSLO https://get.helm.sh/{tarball}"
        f" && curl -fsSL https://get.helm.sh/{tarball}.sha256sum | sha256sum -c -"
        f" && tar -xzf {tarball} && install linux-amd64/helm /usr/local/bin/helm"
    )


async def run(container: dagger.Container, script: str, privileged: bool = False) -> str:
    """Runs script with bash -euo pipefail and returns what it printed.

    privileged runs it in a container that may mount (it has /dev/fuse), but
    without the ambient capabilities Dagger then hands even a non-root user:
    setpriv clears them, so the CI user reads and writes only what a runner's
    user may, and a setuid fusermount3 still mounts.
    """
    argv = ["bash", "-euo", "pipefail", "-c", script]
    if privileged:
        argv = ["setpriv", "--inh-caps=-all", "--ambient-caps=-all", "--"] + argv
    return await container.with_exec(argv, insecure_root_capabilities=privileged).stdout()


def failure_text(err: BaseException, lines: int = 40) -> str:
    """The last lines a failed check printed, or its error."""
    if isinstance(err, dagger.ExecError):
        text = (err.stdout or "") + (err.stderr or "")
        tail = text.strip().splitlines()[-lines:]
        return f"exit code {err.exit_code}\n" + "\n".join(tail)
    return str(err)


def osv_scanner_setup(version: str, sha256: str) -> str:
    """The command that installs the osv-scanner release binary, checked
    against its sha256."""
    url = f"https://github.com/google/osv-scanner/releases/download/v{version}/osv-scanner_linux_amd64"
    return (
        f"curl -fsSL -o /usr/local/bin/osv-scanner {url}"
        f" && echo '{sha256}  /usr/local/bin/osv-scanner' | sha256sum -c -"
        " && chmod 0755 /usr/local/bin/osv-scanner"
    )
