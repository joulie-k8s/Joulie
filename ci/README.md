# CI (Dagger)

Every job a pull request runs is a function of this Dagger module, and the workflows run them with `dagger call`. So a job run locally is the job CI runs: the same images, pinned by digest, the same tools and the same commands.

| Function | CI job | What it runs |
|---|---|---|
| `test` | `ci.yml` test | `go mod tidy -diff` for both modules, `make verify-manifests`, `make verify-fmt`, `go build ./...`, `go vet ./...`, `go test ./...` with the FUSE tests required |
| `race` | `ci.yml` race | `go test -race ./cmd/...` and `./simulator/cmd/simulator/` |
| `envtest` | `ci.yml` envtest | `make test-envtest` |
| `promql` | `ci.yml` promql | `make test-promql` |
| `prometheus-otlp-live --config=<c>` | `ci.yml` prometheus-otlp-live | the live input tests against the pinned Prometheus, `<c>` one of `promoted`, `unpromoted`, `utf8`, `utf8-unpromoted` |
| `helm` | `ci.yml` helm | `helm lint` of both charts and `scripts/verify-chart-renders.sh`, with `HELM_VERSION` of `scripts/simcluster/versions.env` |
| `experiments` | `ci.yml` experiments | the experiments' requirements and `make test-experiments` on Python 3.12 |
| `docs` | `docs-preview.yml` build | the website with Hugo extended, Docsy and PostCSS; returns the built site |
| `integration` | `integration-tests.yml` | the 2-node k3s integration suite, below |
| `all` | | every function above at once, `prometheus-otlp-live` in each configuration; one PASS or FAIL line per check |
| `vulnscan` | `vulnscan.yml`, when a manifest changes | osv-scanner over every dependency manifest and govulncheck over the Go modules; fails on a vulnerability the code reaches |
| `vulnscan-report` | `vulnscan-nightly.yml` | the same scan's SARIF and JSON files, findings or not |
| `vulnfix --report=<dir>` | `vulnscan-nightly.yml` | the fixes of a `vulnscan-report` applied by `scripts/vulnfix.sh`: the changed files and the pull request body |

The simulated cluster smoke (`simcluster.yml`) is not a function: it runs k3s through the host's Docker, which does not nest inside Dagger here, so it stays `make simcluster-smoke` on the runner and on a laptop. The gh-pages deploys and the preview comments stay in the workflows, which hold the token they need.

## Run locally

Needs the `dagger` CLI, at the module's `engineVersion` (`dagger.json`), and Docker. From the repository root:

```bash
make ci-local                          # dagger -m ./ci call all --source=.
dagger -m ./ci call test               # one job; --source defaults to the repository root
dagger -m ./ci call prometheus-otlp-live --config=utf8
dagger -m ./ci call docs export --path=/tmp/site
```

The checks run as a non-root user, as on a GitHub runner, with the Go module and build caches in Dagger cache volumes. `test` runs in a container that may mount FUSE (`/dev/fuse`), with no capabilities left to that user. The source is uploaded without `.git`, `bin/`, local state and build outputs (`SOURCE_IGNORE` in `src/joulie_ci/literals.py`); the checks that read Git get a fresh index of every file.

## Vulnerability scan

`vulnscan` runs `ci/scripts/vulnscan.sh`: osv-scanner reads every manifest of `VULN_MANIFESTS` in `src/joulie_ci/literals.py`, with call analysis for Go, so an advisory whose code is never called is listed but does not fail; govulncheck checks the Go modules of `GOVULNCHECK_MODULES` for vulnerabilities their code reaches, the standard library's included. Both scanners are pinned there, osv-scanner by version and sha256. It is not in `all`: an advisory published today fails it whatever the change, so the pull request gate runs it only when a manifest changes, and the nightly scan opens a pull request with the fixes.

```bash
dagger -m ./ci call vulnscan                                   # the summary; fails on a finding
dagger -m ./ci call vulnscan-report export --path=/tmp/vulnscan  # osv.json, osv.sarif, govulncheck-*.json and .sarif
dagger -m ./ci call vulnfix --report=/tmp/vulnscan export --path=/tmp/vulnfix  # report.md and changes/
```

An advisory that cannot be fixed yet goes in `osv-scanner.toml` at the repository root, under `[[IgnoredVulns]]` with its `id`, a `reason` and an `ignoreUntil` date; govulncheck findings with the same `id` are ignored too. A standard library finding needs a Go release: bump the `toolchain` line of `go.mod` and the golang image digests.

## Layout

- `dagger.json`: the module (Python SDK) and its engine version
- `src/joulie_ci/main.py`: the functions
- `src/joulie_ci/checks.py`: the containers the checks run in
- `src/joulie_ci/literals.py`: pinned images, versions and settings
- `scripts/run-integration.sh`, `tests/integration_runner.py`: the integration run
- `scripts/vulnscan.sh`: the vulnerability scan; the fixer is `scripts/vulnfix.sh` at the repository root
- `examples.sh`: example calls

`tests/contracts` checks that every pull request job calls one function of this module at its engine version, and that `all` runs it.

## Integration (Dagger + k3s)

`integration` starts a lightweight custom **2-node k3s** cluster (server + worker) as Dagger services, installs Joulie via Helm from the local repo, and runs integration tests focused on:

- FSM transitions and node labels (`joulie.io/power-profile`) with draining state tracked in `NodeTwinState.schedulableClass`
- scheduler extender behavior driven by `joulie.io/workload-class` pod annotations
- workload-class classification and draining behavior (via NodeTwinState)
- agent telemetry/control HTTP routing smoke test (CPU + GPU control paths)

The pipeline builds the Joulie images from this repo, publishes them to a
throwaway in-pipeline registry with a `dev-*` tag, then installs Helm using
those exact tags. `latest` is never used by integration tests.

## Scope Selection

Integration scope is controlled by the Dagger argument `--it-scope` (forwarded to runner env `IT_SCOPE`):

- `all` (default; `full` alias): run the full integration suite.
- `gpu-only`: run only boot/install + telemetry/GPU smoke checks.

Examples:

```bash
dagger -m ./ci call integration \
  --source=. \
  --it-scope all
```

```bash
dagger -m ./ci call integration \
  --source=. \
  --it-scope gpu-only
```

## Test list (one line each)

Always executed:

- `IT-BOOT-01 / IT-HELM-01` (`test_boot_and_install`): waits for a ready node, installs Joulie via Helm, verifies CRDs, creates `joulie-it`, and installs shared HTTP mock + configures agent telemetry env vars.
- `IT-TP-01` (`test_telemetry_http`): validates telemetry/control HTTP plumbing by asserting mock GET/POST counters increase; on non-GPU nodes it validates graceful GPU-control degradation instead of hard failure.

Executed in full scope (`IT_SCOPE=all` or `full`):

- `IT-CLS-*` (`test_classification_matrix`): validates workload-class annotation-based classification (`performance`, `standard`, unset) and expected draining/eco behavior.
- `IT-FSM-*` (`test_fsm_and_labels`): verifies main FSM transitions (`performance` <-> `eco`) and `draining` behavior with performance and standard workloads.
- `IT-FSM-07` (`test_fsm_toggle_under_eco`): keeps node in eco, creates a performance-class pod, and verifies the scheduler extender rejects it while node remains eco/non-draining.
- `IT-FSM-05` (`test_fsm_idempotency`): checks steady-state idempotency (no label flapping and no unexpected node resourceVersion churn).
- `IT-SCH-*` (`test_scheduling`): validates scheduler extender outcomes for `performance` and `standard` workload classes on performance/eco nodes.

Current execution order in `integration_runner.py` is:

- `all/full` (default): `IT-BOOT-01/IT-HELM-01` -> `IT-TP-01` -> `IT-CLS-*` -> `IT-FSM-*` -> `IT-FSM-07` -> `IT-FSM-05` -> `IT-SCH-*`
- `gpu-only`: `IT-BOOT-01/IT-HELM-01` -> `IT-TP-01`

## Runtime notes

- Profile transitions in tests are driven via controller manager env (`STATIC_HP_FRAC`) updates.
- A rollout is triggered only when the requested value actually changes; no-op updates are skipped.
- Classification matrix cases that rely on perf intent under eco use `spec.nodeName` when needed so classification can still be validated.
- Non-GPU CI nodes are expected; GPU telemetry/control checks are validated in graceful-degradation mode when the node has no GPU resource and no GPU count label (the rule of `pkg/api.GPUResourceKind`, mirrored by `node_object_has_gpu`).

On failure, the runner dumps cluster state and controller logs for debugging.
