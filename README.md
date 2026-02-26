# Joulie - Node-level Power Orchestrator (PoC)

## Documentation

- [Docs index](./docs/README.md)
- [Quickstart](./docs/quickstart.md)
- [DaemonSet Configuration](./docs/daemonset.md)
- [CRD and Policy Model](./docs/policy.md)
- [Operator Notes](./docs/operator.md)
- [Metrics Reference](./docs/metrics.md)
- [Example: stress-ng throttling](./examples/stress-ng-throttling/README.md)
- [Example: Prometheus + Grafana](./examples/prometheus-grafana/README.md)
- [Example: Operator Configuration](./examples/operator-configuration/README.md)

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

## 2.3 Next step: central policy/operator loop

The next milestone is to move from agent-selected policies to a central control loop:

1. Every `X` minutes, the operator evaluates global cluster state.
2. It computes node-to-profile assignments.
3. It writes desired state for each node.
4. Agents enforce the assigned profile and report metrics.

Initial profiles:

- `performance` (HPC): unconstrained node behavior.
- `eco`: constrained node behavior (power/frequency throttling when needed).

Initial policy implementation (simple rule-based):

- Example bootstrap scenario: two nodes, every minute swap profiles between nodes.

Policy inputs (now and future):

- Node metadata (location, reserved/immutable nodes).
- Time windows (peak-hour rules).
- Telemetry (PUE, temperatures, hotspots, energy mix).
- Future data-driven inputs (Prometheus analytics, AI inference via KServe, etc.).

Design goal: keep the policy engine extensible so custom policy modules can be added without changing the core operator loop.

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

The implementation currently uses two CRDs:

- `PowerPolicy` (cluster-scoped): selector-based intent.
- `NodePowerProfile` (cluster-scoped): operator-assigned per-node desired state.

Current preferred flow is operator-driven:

1. Operator assigns each managed node a profile (`performance` or `eco`).
2. Operator writes one `NodePowerProfile` per managed node.
3. Agent reads local `NodePowerProfile` and applies (or simulates) actions.

Alternative selector-based mode:

- if no `NodePowerProfile` exists for a node, agent can still evaluate `PowerPolicy` selectors.

See:

- [CRD and Policy Model](./docs/policy.md)
- [Operator Notes](./docs/operator.md)
- [Quickstart](./docs/quickstart.md)
