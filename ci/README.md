# CI Integration Tests (Dagger + k3s)

This folder contains a Dagger-based integration test harness for Joulie.

It starts a lightweight custom **2-node k3s** cluster (server + worker) as Dagger services, installs Joulie via Helm from the local repo, and runs integration tests focused on:

- FSM transitions and node labels (`joulie.io/power-profile`) with draining state tracked in `NodeTwinState.schedulableClass`
- scheduler extender behavior driven by `joulie.io/workload-class` pod annotations
- workload-class classification and draining behavior (via NodeTwinState)
- TelemetryProfile HTTP routing smoke test (CPU + GPU control paths)

## Layout

- `dagger.json`: Dagger module definition (Python SDK)
- `src/joulie_ci/__init__.py`: Dagger pipeline entrypoint
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

## Scope Selection

Integration scope is controlled by the Dagger argument `--it-scope` (forwarded to runner env `IT_SCOPE`):

- `all` (default; `full` alias): run the full integration suite.
- `gpu-only`: run only boot/install + telemetry/GPU smoke checks.

Examples:

```bash
dagger -m ./ci call integration \
  --source=. \
  --it-scope all \
  --username env:CERN_REGISTRY_USER \
  --password env:CERN_REGISTRY_PASSWORD
```

```bash
dagger -m ./ci call integration \
  --source=. \
  --it-scope gpu-only \
  --username env:CERN_REGISTRY_USER \
  --password env:CERN_REGISTRY_PASSWORD
```

## Test list (one line each)

Always executed:

- `IT-BOOT-01 / IT-HELM-01` (`test_boot_and_install`): waits for a ready node, installs Joulie via Helm, verifies CRDs, creates `joulie-it`, and installs shared HTTP mock + TelemetryProfile.
- `IT-TP-01` (`test_telemetry_http`): validates telemetry/control HTTP plumbing by asserting mock GET/POST counters increase; on non-GPU nodes it validates graceful GPU-control degradation instead of hard failure.

Executed in full scope (`IT_SCOPE=all` or `full`):

- `IT-CLS-*` (`test_classification_matrix`): validates workload-class annotation-based classification (`performance`, `standard`, `best-effort`, unset) and expected draining/eco behavior.
- `IT-FSM-*` (`test_fsm_and_labels`): verifies main FSM transitions (`performance` <-> `eco`) and `draining` behavior with performance and best-effort workloads.
- `IT-FSM-07` (`test_fsm_toggle_under_eco`): keeps node in eco, creates a performance-class pod, and verifies the scheduler extender rejects it while node remains eco/non-draining.
- `IT-FSM-05` (`test_fsm_idempotency`): checks steady-state idempotency (no label flapping and no unexpected node resourceVersion churn).
- `IT-SCH-*` (`test_scheduling`): validates scheduler extender outcomes for `performance`, `standard`, and `best-effort` workload classes on performance/eco nodes.

Current execution order in `integration_runner.py` is:

- `all/full` (default): `IT-BOOT-01/IT-HELM-01` -> `IT-TP-01` -> `IT-CLS-*` -> `IT-FSM-*` -> `IT-FSM-07` -> `IT-FSM-05` -> `IT-SCH-*`
- `gpu-only`: `IT-BOOT-01/IT-HELM-01` -> `IT-TP-01`

## Runtime notes

- Profile transitions in tests are driven via operator env (`STATIC_HP_FRAC`) updates.
- A rollout is triggered only when the requested value actually changes; no-op updates are skipped.
- Classification matrix cases that rely on perf intent under eco use `spec.nodeName` when needed so classification can still be validated.
- Non-GPU CI nodes are expected; GPU telemetry/control checks are validated in graceful-degradation mode when allocatable GPU resources are absent.

On failure, the runner dumps cluster state and controller logs for debugging.
