# Joulie KWOK-based Workload + Power Simulator — Detailed Implementation Plan (for Codex)

This plan implements a **hybrid simulation** setup where:

- Kubernetes **API server + kube-scheduler are real** (placement and binding are real),
- Nodes and workload Pods can be **fake KWOK objects** (no real kubelet execution),
- Joulie **operator is real** (writes `NodePowerProfile`),
- Joulie **agent logic is real**, but in simulation it runs in a **pool/sharded mode** (one process hosts many *logical* “per-node agent loops”),
- Telemetry and actuation are routed via the existing **HTTP simulator interfaces** (`TelemetryProfile`),
- Workload is **batch** and completion time depends on DVFS/RAPL (and future GPU controls).

The plan is structured so you can scale to hundreds/thousands of simulated nodes without running thousands of containers, **while keeping the logical semantics “1 agent loop per node”** (required for real DVFS/RAPL/DCGM enforcement later).

---

## 0. Design constraints / decisions (do not deviate)

1. **Do not simulate the scheduler.** Keep kube-scheduler real.
2. **Do not require real workload containers to run.** Use KWOK Pods as API objects; simulator owns progress and completion.
3. Preserve the *logical* contract: **each node has an independent agent loop** that:
   - reads desired state (`NodePowerProfile`),
   - reads telemetry (`TelemetryProvider` HTTP),
   - sends control intents (`ControlProvider` HTTP),
   - reports/exports metrics.
4. Make the workload engine **trace-driven**:
   - “distribution generation” produces a trace,
   - “telemetry replay” produces a trace,
   - the simulator **consumes traces** to create pods and advance progress.
5. Keep CPU vs GPU as separate “device families” in APIs and internal models (CPU now, GPU later).

---

## 1. Deliverables

### 1.1 Code deliverables

- **Agent**: add **pool/sharded mode** that runs multiple node controllers in one pod (simulation), while keeping existing DaemonSet “single node” mode (real HW).
- **Simulator**:
  - add per-node **hardware profile** support (CPU vendor/cores, idle power, freq→power model parameters, DVFS/RAPL constraints),
  - replace simplistic power model with a parameterized model that responds to utilization + DVFS/RAPL,
  - implement **batch workload execution engine** with:
    - trace loading,
    - pod injection (create Pods at submit times),
    - per-pod progress and completion (slows down under caps/throttling),
    - utilization time series (optional) to support rich workloads/replay.
- **Example**: new `examples/simulator-kwok/` showing end-to-end setup:
  - create cluster + install KWOK (or use `kwokctl create cluster`),
  - create N fake nodes with desired allocatable + labels (including NFD-like labels),
  - install simulator + operator + agent(pool),
  - run batch workload trace and observe telemetry/control loop.

### 1.2 Docs deliverables

Update existing docs and add a new example README:

- `docs/simulator.md`: expand “Next iteration scope” into **exact steps + config** for KWOK mode, trace format, batch completion.
- `docs/telemetry.md`: document HTTP endpoints and payload schema updates (CPU and GPU-ready).
- `docs/daemonset.md`: document **agent pool mode** and when to use it (sim) vs daemonset (real).
- `docs/quickstart.md`: add a short “Simulator with KWOK” section pointing to the example.

---

## 2. System architecture and integration points

### 2.1 Components

1. **KWOK controller** maintains:
   - fake node heartbeats / Ready conditions,
   - fake pods in Running state once scheduled (as pure API objects).
   Reference: KWOK “Manage Nodes and Pods” guide.  
2. **kube-scheduler** binds Pods to Nodes (`spec.nodeName`).
3. **Joulie Operator**:
   - watches nodes/pods,
   - writes `NodePowerProfile` per node.
4. **Joulie Agent**:
   - reads `TelemetryProfile` for routing (host vs http),
   - reads node-scoped `NodePowerProfile`,
   - applies via `ControlProvider` (HTTP in simulation),
   - exports metrics.
5. **Simulator service**:
   - watches nodes/pods to infer workload allocation per node,
   - runs workload execution engine (batch progress),
   - computes telemetry trajectories per node,
   - provides HTTP endpoints used by agent.

### 2.2 Data/control flows (simulation)

- Scheduler → sets `pod.spec.nodeName`.
- Simulator → sees pods per node → updates internal node “load” and per-pod progress.
- Operator → writes `NodePowerProfile(nodeName=...)` based on policy/guards.
- Agent (pool) → per node:
  - `GET /telemetry/{node}` → normalized node telemetry snapshot,
  - compute desired DVFS/RAPL actions,
  - `POST /control/{node}` → simulator applies, affects next telemetry.
- Result: closed loop where caps/throttling change **power** and **completion time**.

---

## 3. KWOK cluster pattern (reference example design)

### 3.1 Core pattern

Run Joulie components (operator/agent/simulator) on **real nodes** (e.g., the kind control-plane node),
and schedule simulated workload onto **fake KWOK nodes**.

Use this rule:

- Fake nodes are tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
- Workload pods include toleration for that taint and a nodeSelector/affinity matching fake nodes (e.g., `type=kwok`).
- Joulie components **do not** tolerate the taint → they stay on the real node.

KWOK docs show this exact toleration/affinity approach.

### 3.2 Fake node resource advertising

Create `v1.Node` objects with:

- `metadata.labels`:
  - `type=kwok` (for affinity)
  - `joulie.io/managed=true` (so operator targets them)
  - NFD-like labels for heterogeneity (examples in §6.3)
- `status.capacity` + `status.allocatable`:
  - `cpu`, `memory`, `pods`
  - **extended resources** for future GPUs (example: `nvidia.com/gpu: "4"`)

Important: in KWOK, nodes are pure API objects; you can set allocatable/capacity directly.

---

## 4. Agent changes — “pool/sharded mode” while preserving per-node semantics

### 4.1 Current baseline (keep working)

- DaemonSet runs privileged, `NODE_NAME` from `spec.nodeName`.
- Agent reconciles a single node by reading `NodePowerProfile(spec.nodeName == NODE_NAME)`.

### 4.2 Refactor required (minimal, but structural)

Create a reusable internal controller type, e.g. `NodeController`, that encapsulates **exactly** the current single-node logic.

**Goal:** same per-node behavior in both modes.

#### 4.2.1 New internal structure

- `pkg/agent/controller/node_controller.go` (or similar):
  - `type NodeController struct { nodeName string; clients; telemetryProvider; controlProvider; desiredStateStore; metrics; ... }`
  - `Run(ctx)` loop with the current reconcile interval.
- `pkg/agent/controller/desired_state.go`:
  - index/cache `NodePowerProfile` by `spec.nodeName`.
- Keep current logic for parsing `TelemetryProfile` and `{node}` URL replacement.

#### 4.2.2 Two “wrappers” (entrypoints)

1) **DaemonSet wrapper** (real HW):
   - start exactly one `NodeController(nodeName=NODE_NAME)`.
2) **Pool wrapper** (simulation):
   - discover target nodes by selector (e.g., `joulie.io/managed=true`),
   - shard ownership across replicas,
   - for each owned node start one `NodeController(nodeName)`.

### 4.3 Pool mode sharding (avoid control flapping)

Implement deterministic sharding:

- Environment:
  - `AGENT_MODE=pool`
  - `POOL_NODE_SELECTOR` (label selector)
  - `POOL_SHARDS` (int k)
  - `POOL_SHARD_ID` (int 0..k-1) — use StatefulSet ordinal for stability.
- Ownership function:
  - `owns(node) := (fnv32(nodeName) % POOL_SHARDS) == POOL_SHARD_ID`

Lifecycle:

- Watch Nodes with selector.
- On add/update: if `owns(node)` start/keep controller; else stop if running.
- On delete: stop controller.

### 4.4 Pool mode deployment

Add Helm support:

- values:
  - `agent.mode: daemonset|pool`
  - `agent.pool.replicas: k`
  - `agent.pool.nodeSelector: "joulie.io/managed=true"`
- Templates:
  - keep existing `agent-daemonset.yaml`,
  - add `agent-deployment.yaml` (or StatefulSet if you want stable shard IDs),
  - pass env vars above.
- RBAC:
  - in pool mode, agent must read `nodes`, `nodepowerprofiles`, `telemetryprofiles`.
  - (No writes required in current design.)

### 4.5 Acceptance tests for agent changes

- Unit tests:
  - sharding stability: same nodeName → same shard
  - shard distribution sanity across many names
- Integration smoke test:
  - in KWOK example, set `k=2` replicas and ensure exactly one replica posts controls for each node.

---

## 5. Simulator enhancements (hardware + power + workload)

### 5.1 Hardware profile model

Add a per-node hardware profile store with:

- **Static** config (from file/ConfigMap):
  - `cpu.vendor` (string)
  - `cpu.cores` (int)
  - `freq.f_min_mhz`, `freq.f_max_mhz` (float)
  - `power.idle_w` (float)
  - `power.p_max_w` (float)
  - `power.alpha_util` (float)  # util nonlinearity
  - `power.beta_freq` (float)   # freq scaling nonlinearity
  - `rapl.cap_min_w`, `rapl.cap_max_w` (float)
  - `dvfs.ramp_ms` (int)
  - optional noise: `noise.stddev_w` (float)
- **Mapping**:
  - `SIM_NODE_CLASS_CONFIG` already exists; extend it so classes can inject a full hardware profile or overrides.

Implementation:

- `simulator/pkg/hw/profile.go`: structs + validation + defaults.
- Support two ways to assign profiles:
  1) explicit nodeName mapping,
  2) class mapping via `matchLabels` (preferred for dynamic names).

### 5.2 Node state model

Maintain internal `NodeState` per simulated node:

- `current_time`
- `dvfs.throttle_pct` or `dvfs.target_freq` (store both if needed)
- `rapl.cap_watts`
- `observed`:
  - `cpu.util` (effective)
  - `cpu.freq_scale` (0..1) (derived from throttle/cap + ramp dynamics)
  - `cpu.power_watts`
- `pods_running` (from API)
- `workload` aggregate vectors (CPU/GPU utilization)
- debug: last control action, last telemetry snapshot, last update latency

### 5.3 Power model (CPU now, GPU-ready)

Replace current simplistic model with:

- Inputs:
  - `u ∈ [0,1]` effective CPU utilization,
  - `s = f/f_max ∈ [f_min/f_max, 1]` effective frequency scaling,
  - `P_idle`, `P_max`, `alpha`, `beta`.
- Base estimate:
  - `P_est = P_idle + (P_max - P_idle) * (u^alpha) * (s^beta)`
- Apply RAPL cap enforcement:
  - If `P_est > P_cap`, reduce `s` until `P_est(s) <= P_cap` (bounded by `f_min`).
  - Track `cap_saturated=true` if cannot reach cap even at min freq.
- Apply DVFS ramp dynamics:
  - throttle changes do not apply instantaneously; ramp to target over `dvfs.ramp_ms`.

GPU-ready:

- Extend telemetry schema to include `gpu.*` fields, but keep them zero/default until implemented.
- Keep internal model extensible: `DeviceState` map keyed by family `cpu|gpu`.

### 5.4 HTTP API schema (extend, remain backward compatible)

Current minimal accepted telemetry JSON forms include:

- `{ "packagePowerWatts": 245.3 }` or `{ "cpu": { "packagePowerWatts": 245.3 } }`

Update simulator responses to always include the structured form, but keep the top-level shortcut for backward compatibility.

**Telemetry response (suggested):**

```json
{
  "node": "kwok-node-0",
  "ts": "2026-03-04T00:00:00Z",
  "cpu": {
    "packagePowerWatts": 210.5,
    "utilization": 0.74,
    "freqScale": 0.62,
    "throttlePct": 35,
    "raplCapWatts": 180,
    "capSaturated": false
  },
  "gpu": {
    "present": false,
    "powerWatts": 0,
    "utilization": 0
  },
  "pods": {
    "running": 12,
    "byIntentClass": {"performance": 4, "eco": 6, "flex": 2}
  }
}
```

**Control request (keep current):**

```json
{
  "node": "kwok-node-0",
  "action": "rapl.set_power_cap_watts | dvfs.set_throttle_pct",
  "capWatts": 120.0,
  "throttlePct": 20,
  "ts": "..."
}
```

Return:

- `applied|blocked|error`
- observed post-state (best-effort).

### 5.5 Workload execution engine (batch B)

#### 5.5.1 Key principle

Requests drive scheduling; utilization drives power/progress.

So each job needs:

- resource requests (cpu/mem/gpu),
- utilization profile (optional time-varying),
- a work budget (CPU/GPU work units),
- sensitivity/intensity parameters (CPU-bound vs IO-bound etc).

#### 5.5.2 Workload Trace format (stable contract)

Define a trace schema versioned, consumed by simulator.

**Preferred initial format:** JSONL (easy in Go), with explicit schema version.
(Optionally add Parquet later behind an interface.)

**Records:**

1) `Job` record:

```json
{
  "type": "job",
  "schemaVersion": "v1",
  "jobId": "job-000123",
  "submitTimeOffsetSec": 12.5,
  "namespace": "default",
  "podTemplate": {
    "labels": { "joulie.io/workload-intent-class": "performance" },
    "requests": { "cpu": "4", "memory": "8Gi", "nvidia.com/gpu": "1" }
  },
  "work": { "cpuUnits": 5000, "gpuUnits": 0 },
  "sensitivity": { "cpu": 1.0, "gpu": 1.0, "other": 0.2 }
}
```

1) `UtilizationSegment` record (optional, piecewise):

```json
{
  "type": "util",
  "schemaVersion": "v1",
  "jobId": "job-000123",
  "tOffsetSec": 0,
  "cpuUtilFracOfRequest": 0.9,
  "gpuUtilFracOfRequest": 0.0
}
```

If no util segments exist, default to `cpuUtilFracOfRequest=1` and `gpuUtilFracOfRequest=0` for CPU-only jobs.

#### 5.5.3 Simulator workload modules

Implement in simulator:

- `TraceLoader`:
  - reads trace from mounted file path env `SIM_WORKLOAD_TRACE_PATH`,
  - validates schema version,
  - materializes job list + per-job utilization segments.
- `WorkloadInjector` (optional, but recommended for reproducibility):
  - creates Pods in Kubernetes at `submitTimeOffsetSec` relative to `SIM_START_TIME` (or simulator start).
  - Pods are created with:
    - nodeSelector / affinity to fake nodes,
    - toleration for kwok taint,
    - labels for intent class,
    - annotations for work units and jobId.
- `ExecutionEngine`:
  - watches pods and maps them to jobs by annotation `sim.joulie.io/jobId`,
  - tracks per-job remaining work,
  - every tick computes per-node effective speed and advances job progress,
  - on completion: delete pod (initially) and emit completion metrics/events.

#### 5.5.4 Progress model (batch completion depends on control)

Per node per tick `dt`:

1) Determine node effective `s_cpu` (0..1) from DVFS/RAPL.
2) Compute each job’s effective CPU “throughput”:
   - `throughput = requestedCores * cpuUtilFracOfRequest * (baseSpeedPerCore) * (1 - (1 - s_cpu) * sensitivity.cpu)`
   - choose `baseSpeedPerCore=1 workUnit/sec` for normalized units.
3) Decrement `remainingCpuUnits -= throughput * dt`.

GPU future:

- same but `s_gpu` and `gpuUnits`.

Important: include fairness if multiple jobs share cores:

- simplest: proportional share based on `cpu request` (or equal share).
- implement a clear deterministic rule and document it.

#### 5.5.5 Pod lifecycle semantics

Start with **delete-on-complete**:

- avoids fighting pod status subresource ownership.
- operator guardrail sees the pod is gone → safe to downgrade.

Optionally later:

- patch `status.phase=Succeeded` for Job compatibility (requires more RBAC and careful ownership).

### 5.6 Simulator watches cluster state

Simulator already watches pods/nodes for `runningPods`. Extend it to:

- group pods per node,
- compute per-node aggregate utilization vectors from job profiles,
- compute by intent class counts (for debug/metrics).

### 5.7 Simulator metrics

Add Prometheus metrics needed to validate batch behavior:

- `joulie_sim_job_submitted_total{class}`
- `joulie_sim_job_completed_total{class,node}`
- `joulie_sim_job_completion_seconds_bucket{class}` histogram
- `joulie_sim_node_cpu_power_watts{node}`
- `joulie_sim_node_cpu_util{node}`
- `joulie_sim_node_freq_scale{node}`
- `joulie_sim_node_rapl_cap_watts{node}`
- `joulie_sim_control_actions_total{node,action,result}`

---

## 6. Workload richness: distributions + telemetry replay (trace producers)

### 6.1 Generator (distributions → trace)

Implement a separate binary or a simulator subcommand:

- `simulator/cmd/workloadgen/main.go` (recommended) that outputs trace JSONL.

Inputs:

- CLI flags / config file describing distributions:
  - inter-arrival distribution,
  - request sizes (CPU/mem/GPU),
  - work units distribution (log-normal recommended),
  - utilization shape templates (constant, ramp, bursty, periodic),
  - correlations (e.g., longer jobs → bursty util; GPU jobs → larger CPU request).

Output:

- stable trace JSONL consumed by simulator.

### 6.2 Replay (telemetry → trace)

Implement a second tool:

- `simulator/cmd/traceextract/main.go`

Inputs:

- telemetry source:
  - Prometheus (pod metrics if available),
  - or offline exported telemetry files.
Outputs:
- same trace schema.

Two replay levels:

1) Pod-level replay (preferred): needs pod start/end + per-pod utilization.
2) Node-level replay (fallback): derive aggregate load and synthesize per-pod profiles.

Keep placement decision with real scheduler by default (do not pin nodeName unless explicitly requested).

### 6.3 Node heterogeneity labels (NFD-like)

For realism, set labels on fake nodes similar to NFD output (agent already looks at some keys):

- CPU vendor:
  - `feature.node.kubernetes.io/cpu-vendor=GenuineIntel|AuthenticAMD`
  - `feature.node.kubernetes.io/cpu-model.vendor_id=...` (fallback)
- GPU presence hints (for future):
  - `feature.node.kubernetes.io/pci-10de.present=true` (NVIDIA)
  - `feature.node.kubernetes.io/pci-1002.present=true` (AMD)

Also keep a simpler simulator label namespace for matching:

- `joulie.io/node-class=<class>` (optional) to bind to class profiles.

---

## 7. GPU future-proofing (what to implement now vs later)

### 7.1 Implement now (no real GPU yet)

- KWOK nodes can advertise `status.allocatable["nvidia.com/gpu"]=N` (extended resource).
- Workload trace supports GPU requests and GPU work units (even if `gpuUnits=0` initially).
- Simulator telemetry schema includes `gpu` object (present=false by default).
- Control API accepts future actions but returns `blocked` if unsupported:
  - `gpu.set_power_cap_watts`
  - `gpu.set_clock_policy` (optional)

### 7.2 Implement later (GPU simulation)

- Add `GpuModel`:
  - power model responding to utilization + cap,
  - progress slowdown for GPU work units,
  - per-node GPU count and type.
- Add replay extraction for GPU utilization if available.

---

## 8. Example under `examples/` (must be added)

Create: `examples/simulator-kwok/`

### 8.1 Contents

- `README.md` with:
  1) prerequisites (docker, kubectl, helm, kwokctl),
  2) create cluster (kwokctl or kind + install kwok),
  3) create fake nodes with allocatable + labels + taints,
  4) install Joulie + simulator (helm values),
  5) create workload trace + start simulation,
  6) verify:
     - pods scheduled to fake nodes,
     - operator writes NodePowerProfiles,
     - agent pool posts controls,
     - simulator power changes and job completion time changes when caps change.

- `manifests/`:
  - `00-kwok-nodes.yaml` (N fake nodes; include cpu/mem/pods + optional GPU)
  - `10-simulator-values.yaml` or `10-simulator.yaml` (deployment/service/RBAC)
  - `20-telemetryprofile-simulator.yaml` (HTTP endpoints with `{node}`)
  - `30-workload-trace-configmap.yaml` (trace JSONL as ConfigMap) or mount file
  - `40-start-workload.yaml` (optional if simulator injects pods; otherwise a driver manifest)

### 8.2 KWOK setup commands (in README)

Use KWOK docs guidance:

- create cluster (via `kwokctl create cluster ...`)
- apply Node objects (fake nodes)
- deploy workload with tolerations/affinity

Make sure README explains the taint/toleration split so Joulie components do not land on fake nodes.

---

## 9. Docs updates (must be done as part of this work)

Update the following files:

- `docs/simulator.md`
  - add KWOK-based flow (fake nodes, taints/tolerations),
  - document hardware profiles schema,
  - document workload trace format + batch progress model,
  - document completion semantics.
- `docs/telemetry.md`
  - document the updated telemetry JSON schema (cpu.*and gpu.* stubs),
  - document control request/response semantics including `applied/blocked/error`.
- `docs/daemonset.md`
  - add agent pool mode:
    - when to use (KWOK/simulation),
    - required env vars,
    - sharding behavior.
- `docs/quickstart.md`
  - add a short “Simulator (KWOK)” section linking to `examples/simulator-kwok/README.md`.

---

## 10. Implementation order (recommended)

1) **Agent refactor**: extract `NodeController`, keep DaemonSet mode working.
2) Add **agent pool wrapper** + helm template + sharding.
3) Simulator: implement **hardware profile** parsing + node class mapping.
4) Simulator: implement **new power model** + DVFS/RAPL coupling + metrics.
5) Implement **workload trace schema** + loader.
6) Implement **workload injector** (create Pods from trace) + tolerations/affinity for fake nodes.
7) Implement **batch execution engine** (progress + completion).
8) Build the **examples/simulator-kwok/** end-to-end demo.
9) Update docs.

---

## 11. Acceptance criteria (definition of done)

### Functional

- You can create 50+ fake nodes with KWOK, each advertising distinct allocatable CPU/mem (+ optional GPUs).
- Operator writes `NodePowerProfile` for those nodes (based on selector `joulie.io/managed=true`).
- Agent pool mode runs **one logical controller per node** and posts controls to `/control/{node}`.
- Simulator telemetry changes in response to controls (power, freqScale/throttle, cap).
- Batch jobs complete, and **completion time increases** when DVFS/RAPL constrains the node.

### Quality / maintainability

- DaemonSet mode remains intact for real clusters.
- Pool mode is isolated to wrapper code; core per-node logic is shared.
- Workload trace format is versioned and documented.
- Example under `examples/` is reproducible and documented.
