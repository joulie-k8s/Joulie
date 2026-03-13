---
title: "Architecture"
linkTitle: "Architecture"
weight: 20
---

Architecture explains how Joulie turns policy intent into node-level enforcement.

If you are new, first read:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}})
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}})

## Core story

1. **Operator** computes desired node power state.
2. Desired state is published in `NodePowerProfile`.
3. **Agent** reads desired state and applies controls through configured backends.
4. Telemetry and status feed the next reconcile step.

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

## Read in this order

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
4. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
5. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
6. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})
7. [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}})
