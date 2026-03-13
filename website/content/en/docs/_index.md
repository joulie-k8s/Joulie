---
title: Documentation
linkTitle: Docs
menu:
  main:
    weight: 10
---

If you are completely new, the smoothest path is:

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}})
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}})
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}})
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}})

Core mental model used across all pages:

- operator decides cluster-level node states,
- agent discovers node hardware and capability,
- agent enforces node-level controls,
- scheduler consumes node supply labels + workload constraints.

## Section guide

- [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
  - concepts, install, runtime modes, workload compatibility
- [Architecture]({{< relref "/docs/architecture/_index.md" >}})
  - operator/agent roles, CRDs, policy model, telemetry/control interfaces
- [Hardware]({{< relref "/docs/hardware/_index.md" >}})
  - CPU and GPU support model, heterogeneity strategy, runtime caveats
- [Simulator]({{< relref "/docs/simulator/_index.md" >}})
  - digital-twin behavior, algorithms, runtime flow
- [Experiments]({{< relref "/docs/experiments/_index.md" >}})
  - benchmark design and measured outcomes

## What to expect

- **Clear control-loop model**: operator decides, agent enforces.
- **Kubernetes-native contracts**: CRDs + scheduling constraints as intent/supply language.
- **Practical path to adoption**: quickstart first, then progressive deep dives.
