# Joulie Documentation

This directory contains the minimal docs for the current PoC.

## What exists today

- `PowerPolicy` CRD (cluster-scoped)
- `joulie-agent` DaemonSet (node-local enforcer)
- NFD-based hardware discovery in agent
- AMD and Intel CPU backend hooks via Linux `powercap` sysfs path
- GPU detection only (enforcement is not implemented yet)

## Read this first

- [Quickstart](./quickstart.md)
- [DaemonSet Configuration](./daemonset.md)
- [CRD and Policy Model](./policy.md)
- [Operator Notes](./operator.md)
- [Example: stress-ng throttling](../examples/stress-ng-throttling/README.md)

## Current naming

The implementation currently uses `PowerPolicy` (`powerpolicies.joulie.io`).
If older text refers to `PowerState`, treat it as the same conceptual API for now.
