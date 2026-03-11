#!/usr/bin/env bash
set -euo pipefail

# Run integration tests locally.
# Requires: dagger CLI + Docker/Podman runtime.

# From repo root:
dagger -m ./ci call integration \
  --source=. \
  --username env:CERN_REGISTRY_USER \
  --password env:CERN_REGISTRY_PASSWORD

# From within ci/ (matches the itwinai pattern with parent context):
# dagger call integration --source=.. --username env:CERN_REGISTRY_USER --password env:CERN_REGISTRY_PASSWORD
