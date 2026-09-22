---
name: joulie-dev
description: Use when changing, reviewing or debugging Joulie code (Go under cmd/, pkg/, api/, the Helm chart, the simulator, experiments), when a NodeTwin or NodeHardware object looks wrong on a cluster, or when planning a CRD, env var, metric or component change in this repository
---

# Developing Joulie

## Overview

What the docs describe is in `website/content/en/docs` (start with `architecture/_index.md`); this skill is the development practice that the docs do not carry: where the code entry points are, which rule guards which class of defect, and what a change must touch to be complete. Most defects so far were not logic errors. They were a component reading or writing something it did not own, a schema and its consumer drifting apart, or a hardware path no test could reach.

## Entry points

| Component | Start reading at |
|---|---|
| controller manager | `cmd/controller-manager/main.go` (`reconcileWithCatalog`, the policy loop), `twinstate.go` (`reconcileNodeTwin`, `nodeTwinStatusToMap`, `upsertNodeTwinSpec`), `reader.go` (cache options, pod transform), `deprecated.go` (aliases); logic in `pkg/controller/{policy,fsm,twin}` |
| agent | `cmd/agent/main.go` (`discoverHardware`, `reconcileOnce`, `applyRAPLPackageCap`, `updateNodeTwinControlStatus`, `upsertNodeHardwareStatus`, `cacheOptionsForNode`); `pkg/agent/dvfs` (`RAPLPackageZones`, `PowercapRoot`) |
| scheduler extender | `cmd/scheduler/main.go` (`filterNodes`, `scoreNode`, `twinStatusFromObject`, `hwInfoFromObject`); `pkg/scheduler/powerest` |
| kubectl plugin | `cmd/kubectl-joulie/main.go` (status columns) |
| shared | `api/v1alpha1` (CRD types, source of truth), `pkg/api` (in-memory structs, constants, `ObjectNameForNode`, field managers), `pkg/kube` (scheme, cache, manager), `pkg/hwinv` (catalog) |
| simulator | `simulator/cmd/simulator/main.go` (HTTP contract: `/telemetry/`, `/control/`, `/api/v1/query`) |

## Before reasoning about a twin value, find its source

Three kinds of numbers reach `NodeTwin.status` by three different roads, and a wrong diagnosis usually mixes them up:

- **Static hardware facts** (model, sockets, cap ranges): agent, through `NodeHardware`; the twin fills gaps from the catalog (`enrichHardwareFromCatalog`) and last resort 5 W per core. A TDP equal to `cores x 5` means both the CR and the catalog lacked the value.
- **Measured power**: only through `NODE_POWER_SOURCE` (`prometheus`, `http`; `static` means 0 W). It never travels through `NodeHardware`. A Prometheus query must return watts; a `*_joules_total` counter needs `rate()`.
- **Per-node RAPL readings** exist only on the node. A new per-node measurement means the agent exports it (metrics or the HTTP telemetry contract) and the controller manager queries it. There is no shortcut through a CR.

When a twin value looks wrong on a real cluster, walk the road backwards and ask for evidence at each step before reading code:

1. Which release runs there: `kubectl -n joulie-system get ds joulie-agent -o jsonpath='{.spec.template.spec.containers[0].image}'`. Older agents lack fallbacks that `main` has.
2. What the agent published: `kubectl get nodehardware <node> -o jsonpath='{.status.cpu}'` (`rawModel`, `sockets`, `capRange`) and `status.gpu`.
3. What the agent could read: inside the agent pod, `ls /host-sys/class/powercap` and `cat /host-sys/class/powercap/intel-rapl:*/name`; the node labels (`kubectl get node <node> -o jsonpath='{.metadata.labels}'`); `/proc/cpuinfo` model name.
4. Whether the catalog matched: `pkg/hwinv/assets/hardware.yaml` aliases against the raw model string.
5. Only then the code, and then a fixture-based regression test (section "Testing without hardware").

`pkg/agent/hardware` is a second discovery implementation that nothing imports; the live path is `discoverHardware` in `cmd/agent/main.go`. Do not diagnose or fix in the dead one.

## Ownership of shared objects

The per-field table lives in `architecture/_index.md` ("Who writes what"). What the developer must keep true:

- Every write carries a field manager from `pkg/api` (`FieldManagerControllerManager`, `FieldManagerAgent`).
- The controller manager never writes `status.controlStatus`; the agent never writes anything else in `NodeTwin`. Guards: `TestNodeTwinStatusMapNeverContainsControlStatus`, `TestControlStatusPatchTouchesOnlyItsOwnSubtree`.
- Object names come only from `pkg/api.ObjectNameForNode`. A second implementation is how a node gets two objects. Guard: `TestObjectNameSharedWithOperator`, `TestUpsertNodeTwinStatusUsesSanitizedObjectName`.
- A new field gets an owner in that docs table in the same PR.

## Rules and the guard behind each

| Rule | Why | Guard |
|---|---|---|
| Read CRs through `api/v1alpha1` types, never `unstructured` with a `float64` assertion | the API server returns whole numbers as `int64`; the assertion silently drops them | `TestTwinStatusFromObjectAcceptsWholeNumbers`, `TestFetchNodeHardwareKeepsWholeNumberWatts` |
| Never edit CRD YAML; edit the Go types and run `make generate manifests` | two copies are shipped (`config/crd/bases`, `charts/joulie/crds`) and drift silently | `TestCRDCopiesAreIdentical`, `make verify-manifests` in CI |
| Every value written to an enum field is listed in `pkg/api` | the schema rejects unknown values and the write fails quietly | `TestNodeTwinSchemaAcceptsEveryPowerSourceValue` |
| Select RAPL zones by their `name` file (`package-*`), never by directory pattern | `intel-rapl:N:0` is the DRAM zone: tiny maximum, rejects writes | `TestReadRAPLPackageCapRangeIgnoresDRAMSubZones`, `TestRAPLCapFilesTargetsPackageZonesOnly` |
| Socket count and CPU model fall back to `/proc/cpuinfo`, then RAPL zones; labels are optional | NFD does not publish `cpu-sockets` or `cpu-model.name` | `TestDiscoverCPUSocketsFallsBackToPackageZoneCount`, `TestDiscoverCPURawModelFallsBackToCPUInfo` |
| Skip a write when nothing changed; refresh at most every 5 minutes | a write bumps `resourceVersion` and wakes every watcher | `TestControlStatusWriteSkippedWhenUnchanged`, `TestNodeHardwareStatusWriteSkippedWhenUnchanged` |
| A DaemonSet caches only its own node's objects | otherwise memory grows with the cluster on every node | `TestDaemonsetCacheSelectsOnlyThisNodeByName` |
| The pod cache keeps only what `pkg/controller/fsm` reads | pods dominate memory at scale | `TestPodTransformKeepsOnlyFSMFields` |
| A missing CRD at startup is retried, not fatal | install order is not guaranteed | agent `startReader`, scheduler `startReaderInBackground` |
| Renamed or deprecated names get one release of aliases | adopters configure env vars, metrics and Helm keys | `cmd/controller-manager/deprecated.go` and its tests |
| A confirmed bug ships with a test that fails on the old code | the only proof that the cause was the cause | project rule |

## Checklists

**Add or change a CRD field**
1. `api/v1alpha1/*_types.go` with markers (enum, min, max); the `pkg/api` mirror if in-memory code uses it; `pkg/controller/twin` output struct if the twin computes it.
2. Name the owner and find the source of the value (section above). Writer: `nodeTwinStatusToMap` or the agent status writers. Readers: `twinStatusFromObject`/`hwInfoFromObject` in the scheduler, columns in `cmd/kubectl-joulie`.
3. `make generate manifests`; both CRD copies change identically; `make verify-manifests` passes on a clean tree.
4. Tests: `api/v1alpha1/roundtrip_test.go`, the owner's unit test, `tests/contracts` for an enum or a cross-component key.
5. Docs: `architecture/digital-twin.md` and `architecture/scheduler.md` for twin fields, the ownership table, fixtures in `config/samples` and `examples/09-scheduler-steering`.

**Add or rename an env var or Helm value**
1. Read it in the binary's `main.go`; on rename keep the old name in `deprecated.go` with a warning.
2. `charts/joulie/values.yaml` and its template; `values/joulie.yaml`; experiment installers (`experiments/*/scripts/03_install_components.sh`, `05_sweep.py`); `ci/tests/integration_runner.py` if CI sets it.
3. `getting-started/05-configuration-reference.md`, the table of that component.

**Add or rename a metric**
1. `joulie_<subsystem>_*` with subsystem in `policy`, `twin`, `agent`, `scheduler`; on rename emit both names for one release (`newDualGaugeVec`).
2. `charts/joulie/dashboards/joulie-overview.json`, example dashboards, `architecture/metrics.md`.

**Rename a component or object**: inventory with `git grep -n -i <old>` across code, `Makefile`, `Dockerfile`, workflows, `ci/`, chart, `values/`, docs, examples, experiments; choose the alias for each user-facing surface; note the removal release next to each alias.

**Before calling a change ready (a PR about to merge)**: the code is one part of the repository; the rest must still be true afterwards. Walk every consumer of what changed and bring it along in the same PR:

1. Inventory: `git grep -n -i` for each renamed or removed name, env var, value key, metric, CRD field or file path across `docs`, `examples`, `experiments`, `charts`, `values`, `ci`, `simulator` and the README.
2. Docs: pages that describe the changed behaviour, the configuration reference, the ownership table, `architecture/metrics.md` for metrics.
3. Examples: each example that exercises the changed surface is updated and still renders or applies (`make test-examples` for manifests, `helm template` for values files).
4. Experiments: installers, sweep configs and scripts that set the changed env vars or names.
5. When an example or experiment cannot be ported (a breaking change, a removed feature): remove the example rather than leave it broken, and in the experiment's `README.md` and `REPORT.md` state the last Joulie version it was run with (`Results were produced with Joulie vX.Y.Z; the experiment has not been ported to later versions because ...`) so the numbers remain interpretable. Update the docs pages that pointed at it.
6. Only then say it is ready, with the commands you ran.

**Release**: tag `vX.Y.Z` on `main`; the release workflow builds images, the kubectl plugin and both charts; verify each image with `docker manifest inspect` and the chart with `helm template` before telling adopters.

## Testing without hardware

The agent reads files and command output, nothing else, so a captured tree is a faithful replay. Point `dvfs.PowercapRoot` at a fixture powercap tree, `procCPUInfoPath` at a fixture cpuinfo, and feed captured `nvidia-smi` output through the fake command runner (`cmd/agent/main_test.go`: `raplFixture`, `withProcCPUInfo`, `fakeCommandRunner`). Simulate a zone that rejects writes by making its limit file a directory. Real dumps are obtainable by asking adopters and colleagues; a corpus is tracked in issue #53.

| Layer | Command | Proves |
|---|---|---|
| unit, fake clients and fixtures | `go test ./...`, `go test -race ./cmd/...` | logic, parsing, ownership shapes, cache scoping |
| contracts | `go test ./tests/contracts/...` | keys, enums and formats agree across components |
| chart | `helm lint charts/joulie`, `helm template joulie charts/joulie -f values/joulie.yaml` | the chart renders, one Deployment per component |
| integration | `cd ci && dagger call integration --source=..` (2-node k3s, HTTP telemetry mock) | install, labels, draining, twin writes, scheduler filter and score |
| scale | `experiments/*` on KWOK | policy behaviour at hundreds of nodes, not in CI |

There is no envtest; schema acceptance is proven by the generated CRD plus the round-trip test. `ci.yml` runs only for PRs against `main`; a stacked PR gets only the Dagger job.

## Keeping this skill true

This file is a set of claims about the repository, and the repository moves. When first-hand evidence contradicts a claim here, a command you ran or a test you watched fail, not a memory, a doc page or another agent's report, the skill is wrong, not the evidence:

1. Say so to the user before continuing: which claim, the command and output that contradicts it, and the correction you intend.
2. Update the skill in the same change as the code, so the two cannot drift apart again. A pointer that moved gets the new path; a rule that turned out false gets removed or rewritten with its new guard.
3. If the correction changes what an assistant should do, add or adjust a scenario under `tests/scenarios` so the behaviour is tested, not just described.

Secondhand evidence is a reason to check, never a reason to edit. Silent edits are worse than stale text: the user has to be able to see what changed and why.

## Common mistakes

- Reasoning about a twin value without first finding which road produced it.
- Running `/usr/bin/go` on the main dev box; use `$HOME/.local/go/bin/go`.
- Producing a patch from a tree with intent-to-add entries: renames become additions and the old files survive. Look for `deleted file` lines before applying.
- Testing a socket-count or RAPL fallback against the host's own `/proc/cpuinfo` or `/sys`; isolate with the fixture variables.
- Recording a decision only in the PR. The configuration reference, the ownership table and this skill are what the next person reads.
