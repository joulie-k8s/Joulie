---
title: "Hardware Modeling"
weight: 35
---

This simulator section now treats hardware modeling as a shared hardware concept rather than a simulator-only detail.

The canonical page is:

- [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})

Use that page for:

- CPU and GPU model provenance
- physical assumptions behind caps and slowdown
- heterogeneous-node semantics
- current limitations and calibration status

From the simulator point of view, the important relationship is simple:

- the simulator implements the modeling assumptions documented there
- the agent relies on the same hardware assumptions when interpreting caps and backend limits
- simulator runtime pages describe how those models are exercised in experiments

For simulator-specific flow, continue with:

- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
