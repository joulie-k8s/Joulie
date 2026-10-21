"""Images, versions and settings the CI functions pin.

An image is pinned by digest so a run today and a run next month use the
same bits. The Go image's tag is the go.mod toolchain and the Dockerfiles
build from the same digest. Versions the repository already pins elsewhere
(Helm, Prometheus) are read from scripts/simcluster/versions.env at run time,
so there is one place to bump them.
"""

GOLANG_IMAGE = "golang:1.26.10@sha256:3745f1aa11822297b79113f5e84ce2bffeb039734571c39855809cfe9e5d7be6"
DISTROLESS_IMAGE = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
PYTHON_IMAGE = "python:3.12-bookworm@sha256:5560e9ab8709f459489e5b8aa696eda8a07ef821e14bb122be62d91234bfa98b"
NODE_IMAGE = "node:20-bookworm@sha256:8f693eaa7e0a8e71560c9a82b55fd54c2ae920a2ba5d2cde28bac7d1c01c9ba5"
REGISTRY_IMAGE = "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
CRANE_IMAGE = "gcr.io/go-containerregistry/crane@sha256:5ccb3ca884e3890d820fbf5ce4071fa3bb89469a72cc3f0ac7404a99c2a68788"
ALPINE_IMAGE = "alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"
K3S_IMAGE = "rancher/k3s:v1.34.1-k3s1"

# The docs site, as docs-preview.yml builds it.
HUGO_VERSION = "0.157.0"
HUGO_SHA256 = "5b2fdfe4646a48ee98107035024fd6640299bff31c4486c259eeeb9f4c822492"
DOCSY_VERSION = "v0.14.3"
DOCSY_DEPENDENCIES_VERSION = "v0.7.2"

VERSIONS_ENV = "scripts/simcluster/versions.env"

# The checks run as this user, not root, as on a GitHub runner: a test of a
# permission behaves the same, and the FUSE tests mount through fusermount3.
CI_UID = 1001

# The Prometheus OTLP receiver settings the live input tests run against:
# k8s.node.name promoted or not, under the default and a UTF-8 translation
# strategy (pkg/controller/inputs/live_test.go reads them from the server).
PROMETHEUS_OTLP_CONFIGS = {
    "promoted": "{promote_resource_attributes: [k8s.node.name, host.name], translation_strategy: UnderscoreEscapingWithSuffixes}",
    "unpromoted": "{translation_strategy: UnderscoreEscapingWithSuffixes}",
    "utf8": "{promote_resource_attributes: [k8s.node.name, host.name], translation_strategy: NoUTF8EscapingWithSuffixes}",
    "utf8-unpromoted": "{translation_strategy: NoUTF8EscapingWithSuffixes}",
}

# What is never sent to the engine: Git metadata (the checks make their own
# index), local state and build outputs. Everything tracked stays.
SOURCE_IGNORE = [
    ".git",
    ".claude/*",
    "!.claude/skills",
    "bin",
    "ci/sdk",
    "website/node_modules",
    "website/public",
    "website/resources",
    "**/__pycache__",
    "**/.pytest_cache",
    "**/.venv",
]

# The vulnerability scan. osv-scanner is the release binary, checked against
# the sha256 its release lists for osv-scanner_linux_amd64; govulncheck is
# installed with go install, which the Go checksum database verifies.
OSV_SCANNER_VERSION = "2.6.0"
OSV_SCANNER_SHA256 = "ca69b3d3cd08f889a49dc0a383122f71cc528b83803671df5fd874d97485b108"
GOVULNCHECK_VERSION = "v1.8.0"

# Every dependency manifest the scan reads; tests/contracts checks that it
# names each go.mod and requirements file of the repository, and that the
# PR gate's paths cover them. ci/pyproject.toml is not one: it lists no
# dependency, and osv-scanner has no extractor for a pyproject.toml without
# a lock file.
VULN_MANIFESTS = [
    "go.mod",
    "tests/promql/go.mod",
    "website/go.mod",
    "experiments/01-cpu-only-benchmark/requirements.txt",
    "experiments/02-heterogeneous-benchmark/requirements.txt",
    "experiments/03-homogeneous-h100-benchmark/requirements.txt",
]

# The Go modules govulncheck checks for reachable vulnerabilities: those with
# code. website/go.mod only pins Hugo modules.
GOVULNCHECK_MODULES = [".", "tests/promql"]
