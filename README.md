# Joulie - Node-level Power Orchestrator (PoC)

## Documentation

- [Docs index](./docs/README.md)
- [Quickstart](./docs/quickstart.md)
- [DaemonSet Configuration](./docs/daemonset.md)
- [CRD and Policy Model](./docs/policy.md)
- [Operator Notes](./docs/operator.md)
- [Example: stress-ng throttling](./examples/stress-ng-throttling/README.md)
- [Example: Prometheus + Grafana](./examples/prometheus-grafana/README.md)

## 1. Motivation

Modern data-centres are heterogeneous (Intel/AMD CPUs, NVIDIA/AMD/Intel GPUs) and operate under thermal and power constraints that vary across time and space (rack placement, hot spots, HVAC behaviour, weather, energy mix, peak hours). Even without per-core or per-pod control, applying coarse, node-level power actions can:

- reduce power peaks and thermal hot spots,
- smooth utilization/power over time,
- improve operational predictability and potentially PUE,
- provide a foundation for future finer-grained control (e.g., KPM-like pools, DRA, per-device slicing).

Joulie targets a Kubernetes-native PoC that is **simple by design**:

- **Node-level actions only** (no pools, no per-core policies).
- A **central policy engine** decides node states based on telemetry + topology.
- A **daemon agent** enforces settings on each node (CPU and later GPU).
- A **simulation mode** allows policy development without real telemetry.

## 2. High-level architecture

Joulie has two planes:

### 2.1 Infrastructure plane (Kubernetes-native)

- **Operator / Controller (Deployment)**
  - Maintains desired power state for each node.
  - Reconciles CRDs and pushes desired state to agents.
  - Maintains inventory of node capabilities (CPU vendor, GPU vendor, supported knobs).

- **Node Agent (DaemonSet)**
  - Runs on every node.
  - Enforces node-level settings (CPU/GPU) via host interfaces (sysfs, vendor tools).
  - Reports observed state and enforcement status (metrics + CR status).

### 2.2 Policy plane (central intelligence)

- **Policy Engine**
  - Reads telemetry (real or simulated) and datacentre topology.
  - Computes the best “power state” for each node (e.g. ECO/BALANCED/PERF).
  - Produces desired state updates (CRDs or direct operator API).

The policy engine may run:

- inside the operator (simple PoC), or
- as a separate component talking to the operator (cleaner separation).

## 3. Design principles (PoC constraints)

- **Coarse control first**: node-wide power actions only.
- **Heterogeneity-aware**: same abstract “state” maps to vendor-specific commands.
- **Safety-first**: never lock the node; support rollback; validate knob availability.
- **Extensible by plugins**: connectors for telemetry and devices; policy modules.
- **Sim-first**: a simulator should reproduce inputs (telemetry/topology) and allow closed-loop tests.

## 4. Node-level actions (what the agent can do)

### 4.1 CPU actions (initial target)

Abstract actions:

- Set CPU governor / EPP (if supported).
- Set min/max frequency bounds (if supported).
- Set package power cap (RAPL for Intel; AMD equivalents via appropriate interface/tooling).
- Optional: set “turbo/boost” toggles (if safe/available).

Enforcement model:

- Apply to **all CPUs** or “all but excluded CPUs”.
- Optional exclusions (future work): kubelet reserved CPUs and/or infra safety set.

### 4.2 GPU actions (future target)

Abstract actions:

- Set GPU power limit (vendor APIs: NVML/DCGM for NVIDIA; ROCm SMI for AMD; Intel equivalents).
- Optional: set application clocks / frequency caps (where supported).

Note: GPU support will be implemented behind a device plugin interface so that CPU-only clusters can run the full PoC.

## 5. Kubernetes API model (CRDs)

Keep the CRD surface minimal. The core idea is: one CRD representing the **desired node power state**.

### 5.1 `PowerState` (core CRD)

Represents desired state for a node (or a group of nodes, see selectors).

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: PowerState
metadata:
  name: global-default
  namespace: joulie-system
spec:
  selector:
    matchLabels:
      joulie.io/managed: "true"
  state: balanced
  gpu:
    enabled: false
