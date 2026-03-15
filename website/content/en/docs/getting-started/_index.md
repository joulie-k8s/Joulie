---
title: "Getting Started"
linkTitle: "Getting Started"
weight: 10
---

Start here if you are new to Joulie.

This section is ordered intentionally:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}}) - what Joulie is, how the control loop works
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}) - install and verify
3. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}}) - workload class annotations and scheduling behavior
4. [WorkloadProfile]({{< relref "/docs/getting-started/04-workload-profiles.md" >}}) - automatic workload classification, Kepler integration, classification reasons
5. [Agent Runtime Modes]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}}) - DaemonSet vs pool mode
6. [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}}) - all environment variables

By the end, you should understand:

- what operator, agent, and scheduler extender each do,
- how workload placement intent is expressed via pod annotations,
- how the classifier automatically builds WorkloadProfiles from live metrics,
- how to configure and run Joulie in real-node and simulator workflows.
