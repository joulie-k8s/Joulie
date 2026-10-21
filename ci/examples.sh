#!/usr/bin/env bash
set -euo pipefail

# Example calls of the CI functions, from the repository root.
# Requires: the dagger CLI at ci/dagger.json's engineVersion, and Docker.

# Every check a pull request runs, at once (make ci-local):
dagger -m ./ci call all --source=.

# One job; --source defaults to the repository root:
# dagger -m ./ci call test
# dagger -m ./ci call race
# dagger -m ./ci call envtest
# dagger -m ./ci call promql
# dagger -m ./ci call prometheus-otlp-live --config=utf8-unpromoted
# dagger -m ./ci call helm
# dagger -m ./ci call experiments
# dagger -m ./ci call docs export --path=/tmp/site

# The integration suite on 2-node k3s:
# dagger -m ./ci call integration --source=.
# Optional overrides:
# --registry-repo joulie-registry.local:5000/joulie
# --tag dev-my-debug-tag
# --it-scope gpu-only

# A shell in the container of a failed step, to look around:
# dagger -m ./ci call --interactive test
