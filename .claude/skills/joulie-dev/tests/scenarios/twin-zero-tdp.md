# Diagnose a twin with zero TDP on bare metal

Tests whether the assistant traces a twin value back to its source, asks for
node-side evidence before reading code, finds the live discovery code, and
proposes a fixture-based regression test.

## Prompt

You are a contributor to this Go repository (a Kubernetes energy-management project). READ-ONLY task: do not create, edit or delete any file, do not run go build or go test.

A user running Joulie on a bare-metal node reports this NodeTwin status excerpt:

```
powerMeasurement:
  cpuCappedPowerW: 0
  cpuTdpW: 0
  measuredNodePowerW: 312.4
  nodeCappedPowerW: 0
  source: prometheus
predictedCoolingStressScore: 0
predictedPowerHeadroomScore: 100
```

The node is busy, yet headroom reads 100% and cooling stress 0%. Their NodeHardware object shows `status.cpu.sockets: 0` and no `capRange`.

Write the diagnosis plan you would follow before touching code: the single most likely cause, the exact commands you would ask the user to run on the node or cluster to confirm it and in which order, which files in this repository implement the path, and how you would turn the confirmed cause into a regression test that runs in CI without hardware. Be specific to this repository. Max 40 lines.

## Expect

- image|version|release
- kubectl get nodehardware
- powercap|intel-rapl
- cpuinfo|cpu-sockets|feature\.node\.kubernetes\.io
- cmd/agent/main\.go
- twin_test\.go|twinstate_test\.go|main_test\.go|raplFixture|withProcCPUInfo

## Baseline

Without the skill (sonnet, read-only, 2026-09-22) the assistant:

- named "the agent never wrote NodeHardware.status.cpu" as the cause and asked for pod status, logs and an RBAC check, without asking which agent version runs, whether `/host-sys/class/powercap` is mounted, or whether the node carries NFD socket labels;
- pointed at `pkg/agent/hardware/discovery.go`, which nothing imports; the live discovery is in `cmd/agent/main.go`;
- proposed adding warning and condition fields as the regression test instead of replaying the node's files through the existing fixture hooks.

With the skill (same model, same day) the answer walked the road backwards in the skill's order (agent image, node labels, `/proc/cpuinfo`, `/host-sys/class/powercap`, catalog match), named `cmd/agent/main.go` and `pkg/agent/dvfs` as the live path, and found that `enrichHardwareFromCatalog` heals a zero socket count only in the per-core fallback branch, so a catalog match with `sockets: 0` still yields a zero TDP. It proposed a pure unit test in `pkg/controller/twin/twin_test.go` rather than a fixture tree, which is the right tool for a branching bug. All expectations matched.
