# CI Integration Tests (Dagger + k3s)

This folder contains a Dagger-based integration test harness for Joulie.

It starts a lightweight **k3s** cluster as a Dagger service, installs Joulie via Helm from the local repo, and runs integration tests focused on:

- FSM transitions and node labels (`joulie.io/power-profile`, `joulie.io/draining`)
- scheduling behavior under affinity constraints
- classification-driven draining behavior
- TelemetryProfile HTTP routing smoke test

## Layout

- `dagger.json`: Dagger module definition (Python SDK)
- `src/main/__init__.py`: Dagger pipeline entrypoint
- `scripts/run-integration.sh`: in-container bootstrap and test launcher
- `tests/integration_runner.py`: integration runner (kubectl/helm driven)
- `examples.sh`: local command examples

## Run locally

Prerequisites:

- Docker or Podman runtime
- `dagger` CLI

From repo root:

```bash
./ci/examples.sh
```

Or directly:

```bash
dagger -m ./ci call integration --source=.
```

From within `ci/`:

```bash
dagger call integration --source=..
```

## What the suite validates

Implemented tests currently cover:

- k3s bootstrap and Helm install smoke
- operator/agent readiness
- TelemetryProfile HTTP GET/POST plumbing against in-cluster mock service
- FSM behavior for:
  - perf -> eco with perf pod present (`draining=true`)
  - draining clear when perf pod is gone (`draining=false`)
  - eco -> perf (`draining=false`)
  - legacy `draining-performance` migration handling
- scheduler-level checks for:
  - perf pods (`NotIn ["eco"]`)
  - eco-only pods with `draining=false`

On failure, the runner dumps cluster state and controller logs for debugging.
