---
title: "Hardware"
linkTitle: "Hardware"
weight: 25
---

Joulie supports heterogeneous clusters with mixed CPU and GPU hardware. On real nodes, the agent discovers hardware capabilities and enforces power caps through RAPL (CPU) and NVML (GPU). In simulation, the same hardware characteristics are modeled parametrically so that benchmarks reflect realistic power behavior.

This section covers supported hardware, known caveats, and the physical power models used for both real enforcement and simulation.

Recommended reading order:

1. [CPU Support]({{< relref "/docs/hardware/cpus.md" >}})
2. [GPU Support]({{< relref "/docs/hardware/gpus.md" >}})
3. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})
