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
- [Example: Workload Intent Classes](./examples/workload-intent-classes/README.md)

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
  - Computes the best “power state” for each node (for example `ActiveEco` or `ActivePerformance`).
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

Initial profile states:

- `ActivePerformance` (mapped to profile `performance`): unconstrained node behavior.
- `ActiveEco` (mapped to profile `eco`): constrained node behavior (power/frequency throttling when needed).

Initial policy implementation (simple rule-based):

- Example bootstrap scenario: two nodes, every minute swap states between nodes.

Policy inputs (now and future):

- Node metadata (location, reserved/immutable nodes).
- Time windows (peak-hour rules).
- Telemetry (PUE, temperatures, hotspots, energy mix).
- Future data-driven inputs (Prometheus analytics, AI inference via KServe, etc.).

Design goal: keep the policy engine extensible so custom policy modules can be added without changing the core operator loop.

## 2.4 Scheduling-aware, scheduler-agnostic control

Joulie can stay scheduler-agnostic while still influencing placement:

- operator publishes node power-state labels (supply side),
- workloads declare power intent classes via labels/affinity/taints-tolerations (demand side),
- default Kubernetes scheduler keeps making placement decisions.

Planned workload intent classes:

- `performance`: requires performance-capable placement.
- `eco`: requires eco placement.
- `flex`: prefers eco but can run on performance when needed.

The critical part is safe `ActivePerformance -> ActiveEco` transitions.
Downgrades must not violate running workload requirements.

Planned transition state machine:

1. `ActivePerformance`: node runs performance profile and accepts performance workloads.
2. `DrainingPerformance`: node keeps performance profile but stops accepting new performance workloads.
3. `ActiveEco`: when no performance-required pods remain (or a force rule applies), operator commits eco profile.

Responsibility split:

- operator/policy decides transitions, guardrails, timeouts, and optional eviction/drain strategy,
- agent applies requested profile and reports observed/applied/blocked state.

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

1. Operator assigns each managed node a state (`ActivePerformance` or `ActiveEco`), mapped to profile (`performance` or `eco`).
2. Operator writes one `NodePowerProfile` per managed node.
3. Operator also updates node label `joulie.io/power-profile` to drive scheduler-aware placement.
4. Agent reads local `NodePowerProfile` and applies (or simulates) actions.

Optional selector mode for experiments:

- operator can be bypassed and agent can evaluate `PowerPolicy` selectors directly.

See:

- [CRD and Policy Model](./docs/policy.md)
- [Operator Notes](./docs/operator.md)
- [Quickstart](./docs/quickstart.md)
