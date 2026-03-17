#!/usr/bin/env bash
set -euo pipefail

# Run integration tests locally.
# Requires: dagger CLI + Docker/Podman runtime.

# From repo root:
dagger -m ./ci call integration \
  --source=.

# From within ci/ (matches the itwinai pattern with parent context):
# dagger call integration \
#   --source=..
#
# Optional overrides:
# --registry-repo joulie-registry.local:5000/mbunino/joulie
# --tag dev-my-debug-tag

# For a plain output:
dagger call --progress plain integration --source=..