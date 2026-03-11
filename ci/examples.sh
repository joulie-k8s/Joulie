#!/usr/bin/env bash
set -euo pipefail

# Run integration tests locally.
# Requires: dagger CLI + Docker/Podman runtime.

# From repo root:
dagger -m ./ci call integration --source=.

# From within ci/ (matches the itwinai pattern with parent context):
# dagger call integration --source=..
