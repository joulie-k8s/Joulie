# Add a field to NodeTwin.status

Tests whether the assistant knows the generated-API workflow, asks where a
measurement comes from, and names the documentation and readers that a twin
field touches.

## Prompt

You are a contributor to this Go repository (a Kubernetes energy-management project). READ-ONLY task: do not create, edit or delete any file, do not run go build or go test. Produce the complete plan for this change request, as you would before opening a PR:

"Add an optional field `dramPowerW` (float, watts drawn by the DRAM RAPL zones) under `NodeTwin.status.powerMeasurement`. The twin controller writes it; `kubectl joulie status` shows it in a new column; the scheduler extender ignores it."

List every file you would change or add (with paths), every command you would run and in which order, every test you would add or adapt, and every documentation page you would update. State which component owns the new field, where the DRAM watts value can come from at runtime, and how you make sure the CRD schema accepts it. Be specific to this repository. Max 60 lines.

## Expect

- api/v1alpha1/nodetwin_types\.go
- make generate manifests|make manifests
- verify-manifests
- charts/joulie/crds
- kubectl-joulie
- twinStatusFromObject|cmd/scheduler
- digital-twin\.md
- (agent|telemetry|NODE_POWER_SOURCE|prometheus|http).*(dram|DRAM)|(dram|DRAM).*(agent|telemetry|NODE_POWER_SOURCE|prometheus|http)
- roundtrip_test|round.?trip

## Baseline

Without the skill (sonnet, read-only, 2026-09-22) the assistant found the typed API, the regeneration step and both CRD copies, but:

- invented a per-node metric for DRAM watts instead of asking how a per-node measurement reaches the controller manager (through `NODE_POWER_SOURCE`, never through `NodeHardware`), and did not notice that DRAM zones are excluded from RAPL discovery by design;
- was unsure of the make targets and never mentioned `make verify-manifests` or that CI enforces it;
- pointed at "grep *.md" for documentation instead of `digital-twin.md` and `scheduler.md`;
- assumed an envtest-based round trip exists; the repository has none.

With the skill (same model, same day) the answer traced DRAM watts to the agent as a per-node RAPL reading that the controller manager must query, named `make generate manifests` then `make verify-manifests`, both CRD copies, the kubectl column, the scheduler conversion function, `digital-twin.md`, the round-trip test and a `raplFixture` based agent test. All expectations matched.
