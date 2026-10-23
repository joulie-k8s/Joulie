---
title: "Getting Started"
linkTitle: "Getting Started"
weight: 10
---

Start here if you are new to Joulie.

**Prerequisites**: Kubernetes 1.26+, Helm 3+, and cluster-admin access. For GPU support, nodes must have NVIDIA drivers with NVML available.

This section is ordered intentionally:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}}) - what Joulie is, how the control loop works
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}) - install and verify
3. [Tutorials]({{< relref "/docs/getting-started/04-tutorials.md" >}}) - six guided tours, the first three on a simulated cluster with only Docker
4. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}}) - workload class annotations and scheduling behavior
5. [Agent Runtime Modes]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}}) - DaemonSet, pool and emulated modes
6. [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}}) - all environment variables
7. [Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}}) - what to check before upgrading an installation

By the end, you should understand:

- what controller manager, agent, and scheduler extender each do,
- how workload placement intent is expressed via pod annotations,
- how to configure and run Joulie in real-node and simulator workflows.

Once installed, use `kubectl joulie` (the kubectl plugin) for a per-node overview of power, power profiles, applied caps and digital twin scores, and `kubectl joulie top` for the live dashboard.
