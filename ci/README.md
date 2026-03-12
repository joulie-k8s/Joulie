# CI Integration Tests (Dagger + k3s)

This folder contains a Dagger-based integration test harness for Joulie.

It starts a lightweight **k3s** cluster as a Dagger service using the Daggerverse k3s module (`github.com/marcosnils/daggerverse/k3s`), installs Joulie via Helm from the local repo, and runs integration tests focused on:

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
- `CERN_REGISTRY_USER` and `CERN_REGISTRY_PASSWORD` exported in your shell

From repo root:

```bash
./ci/examples.sh
```

Or directly:

```bash
dagger -m ./ci call integration \
  --source=. \
  --username env:CERN_REGISTRY_USER \
  --password env:CERN_REGISTRY_PASSWORD
```

From within `ci/`:

```bash
dagger call integration --source=.. --username env:CERN_REGISTRY_USER --password env:CERN_REGISTRY_PASSWORD
```

The pipeline builds `agent` and `operator` images from this repo and publishes them
to the CERN registry with a `dev-*` tag, then installs Helm using those exact tags.
`latest` is never used by integration tests.

## What the suite validates

Implemented tests currently cover:

- k3s bootstrap and Helm install smoke
- operator/agent readiness
- TelemetryProfile HTTP GET/POST plumbing against in-cluster mock service
- FSM behavior for:
  - baseline reconciliation to `performance` (`draining=false`)
  - perf -> eco with perf pod present (`draining=true`)
  - draining clear when perf pod is gone (`draining=false`)
  - eco -> perf (`draining=false`)
  - desired `eco` + perf-constrained pod remains unschedulable (affinity mismatch), preserving eco state
  - idempotency check (labels stable, no unexpected node resourceVersion churn)
- scheduler-level checks for:
  - perf pods (`NotIn ["eco"]`) on both unlabeled and `performance` nodes
  - perf pods (`NotIn ["eco"]`) blocked on `eco` nodes
  - eco-only pods (`In ["eco"]`) run on eco and are blocked on performance
  - eco-only pods with `draining=false`
- exhaustive classification matrix (`IT-CLS-*`) including:
  - required affinity / nodeSelector variants
  - OR-term semantics
  - preference-only vs required constraints
  - invalid/edge cases (e.g. empty values)
  - `spec.nodeName` fallback for perf-intent cases that are unschedulable in single-node eco setups

## Runtime notes

- Profile transitions in tests are driven via operator env (`STATIC_HP_FRAC`) updates.
- A rollout is triggered only when the requested value actually changes; no-op updates are skipped.
- Classification matrix cases that rely on perf intent under eco use `spec.nodeName` when needed so classification can still be validated.

On failure, the runner dumps cluster state and controller logs for debugging.
