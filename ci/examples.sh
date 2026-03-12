#!/usr/bin/env bash
set -euo pipefail

# Run integration tests locally.
# Requires: dagger CLI + Docker/Podman runtime.
# Also requires: CERN_REGISTRY_USER + CERN_REGISTRY_PASSWORD exported.

: "${CERN_REGISTRY_USER:?CERN_REGISTRY_USER is required}"
: "${CERN_REGISTRY_PASSWORD:?CERN_REGISTRY_PASSWORD is required}"

# From repo root:
dagger -m ./ci call integration \
  --source=. \
  --username env:CERN_REGISTRY_USER \
  --password env:CERN_REGISTRY_PASSWORD

# From within ci/ (matches the itwinai pattern with parent context):
# dagger call integration \
#   --source=.. \
#   --username env:CERN_REGISTRY_USER \
#   --password env:CERN_REGISTRY_PASSWORD
#
# Optional overrides:
# --registry-repo registry.cern.ch/mbunino/joulie
# --tag dev-my-debug-tag
