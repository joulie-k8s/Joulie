# Phase 2 — KWOK Benchmark + Policy v1 + Time Scaling + Plots (Implementation Plan for Codex)

This plan assumes Phase 1 is complete: KWOK nodes/pods + simulator HTTP telemetry/control + operator + agent(pool) are implemented (even if simplified). Phase 2 turns this into a **repeatable experiment harness** that produces **realistic statistics** (throughput vs energy tradeoffs) and plots.

**Goal (Phase 2):**

- Run controlled experiments on a small simulated cluster (default 5 KWOK nodes) with 10s/100s of batch pods.
- Add **time scaling** so experiments complete quickly.
- Replace dummy policy with **Policy v1**: target HP/LP mix + queue-aware adjustment + hysteresis + per-node cooldown.
- Compare baselines:
  - **A:** No Joulie / All HP (unconstrained)
  - **B:** Static partition (e.g., 60% HP / 40% LP)
  - **C:** Queue-aware (Policy v1) (dynamic HP fraction driven by pending performance pods)
- Export artifacts and generate plots (Pareto-ish view: energy vs makespan, plus class latencies, pending times, churn, saturation).

All experiment code/scripts must live under a new top-level folder: `experiments/`.

---

## 1) Repository structure to add

Create:

```
experiments/
  phase2_kwok_benchmark/
    README.md
    configs/
      cluster.yaml
      hardware_profiles.yaml
      workload_gen.yaml
      policy_static.yaml
      policy_queue_aware.yaml
      baselines.yaml
    manifests/
      kwok_nodes.yaml.tpl
      telemetryprofile.yaml
      workload_pod_template.yaml.tpl
    scripts/
      00_prereqs_check.sh
      01_create_cluster_kwokctl.sh
      02_apply_nodes.sh
      03_install_components.sh
      04_run_one.py
      05_sweep.py
      06_collect.py
      07_plot.py
      99_cleanup.sh
    results/
      .gitkeep
docs/
  experiments/
    phase2-kwok-benchmark.md
```

Add a top-level pointer in your main docs index (if you have one) linking to `docs/experiments/phase2-kwok-benchmark.md`.

---

## 2) Policy v1 implementation (operator side)

### 2.1 Overview

Replace the dummy policy (“swap profiles between two nodes every N minutes”) with **Policy v1** that computes a desired number of HP nodes and assigns specific nodes to HP/LP.

Policy v1 must support:

- **Base mix:** `hp_base_frac` (e.g., 0.60)
- **Queue-aware adjustment:** increase `hp_target` when there are pending performance pods
- **Hysteresis (global):** only apply target change if stable for `T_hold` simulated seconds
- **Cooldown (per node):** once a node switches profile, it cannot switch again for `T_cooldown` simulated seconds
- **Tick period:** policy reevaluates every `policy_tick_sim_s` (simulated seconds)

### 2.2 Inputs the policy must read

From Kubernetes API:

- Managed nodes: Nodes matching label selector `joulie.io/managed=true` (or existing selector used in your system)
- Pods to classify:
  - scheduling class, e.g. `performance|eco`
  - Pod Phase: Pending/Running
  - Node assignment: `spec.nodeName` (set by scheduler)

Compute:

- `N = number of managed nodes`
- `pending_perf = count(pods where phase=Pending AND intent=performance)`
- Optionally `pending_total`, `running_perf` (helpful but not mandatory)

### 2.3 Policy v1 algorithm (must implement exactly; make params configurable)

Let:

- `hp_base = round(N * hp_base_frac)`
- `hp_target_candidate = clamp(hp_min, hp_max, hp_base + ceil(pending_perf / perf_per_hp_node))`

Hysteresis:

- Track `candidate_since_ts` and `candidate_value`.
- Only update `hp_target_committed` when:
  - `candidate_value` has been unchanged for at least `T_hold_sim_s`.

Cooldown:

- Maintain `last_switch_sim_ts[nodeName]`.
- Node is eligible for switching only if:
  - `(now_sim_ts - last_switch_sim_ts[nodeName]) >= T_cooldown_sim_s`

Assignment step:

- Determine current HP set from existing `NodePowerProfile` (or other source of truth in operator).
- If `current_hp_count < hp_target_committed`, promote some LP nodes to HP.
- If `current_hp_count > hp_target_committed`, demote some HP nodes to LP.
- When choosing nodes to promote/demote:
  - **Must obey cooldown** (only switch eligible nodes).
  - Use a deterministic stable ordering for tie-breaks (e.g., lexical by nodeName).
  - Prefer minimal churn: do not switch nodes unnecessarily.

Minimal deterministic selection acceptable for Phase 2:

- Promote: choose eligible LP nodes with smallest `nodeName` first.
- Demote: choose eligible HP nodes with largest `nodeName` first.
(Keep it simple and deterministic; optimize later.)

### 2.4 Operator configuration

Add a policy config type that can be selected by a ConfigMap or Helm values:

Example Helm values:

```yaml
operator:
  policy:
    type: queue_aware_v1   # or "static_partition"
    timeScale: 60          # see §3
    tickSimSeconds: 20
    queueAwareV1:
      hpBaseFrac: 0.60
      hpMin: 1
      hpMax: 5
      perfPerHpNode: 10
      hysteresisHoldSimSeconds: 60
      nodeCooldownSimSeconds: 300
```

For baseline B, static partition config:

```yaml
operator:
  policy:
    type: static_partition
    timeScale: 60
    tickSimSeconds: 20
    staticPartition:
      hpFrac: 0.60
      hysteresisHoldSimSeconds: 60
      nodeCooldownSimSeconds: 300
```

For baseline A (no Joulie), the experiment harness should either:

- not install operator/agent at all, OR
- install operator/agent but set policy to `all_hp` (no constraints), OR
- keep profiles always HP and no controls applied.

Choose the simplest **reproducible** approach:

- For baseline A: do not deploy operator+agent; run simulator alone (telemetry still available but no controls posted).

### 2.5 Operator metrics/logging required (for plots)

Add Prometheus metrics (or structured logs) for:

- `joulie_policy_hp_target_committed{run_id}` (gauge)
- `joulie_policy_pending_perf{run_id}` (gauge)
- `joulie_policy_switch_total{run_id,from,to}` (counter)
- `joulie_policy_node_switch_total{run_id,node,from,to}` (counter) [optional; might be high-cardinality]
- `joulie_policy_tick_duration_seconds` (histogram)

Also emit structured logs per tick containing:

- run_id, now_sim_ts, pending_perf, hp_target_candidate, hp_target_committed, switched_nodes

---

## 3) Time scaling (simulated time compression)

### 3.1 Requirements

Introduce a consistent **timeScale** across simulator, operator policy loop, and agent loops so the experiment finishes quickly while preserving “control loop cadence” in simulated time.

Definition:

- `timeScale = S` means: `1 wall second = S simulated seconds`.

### 3.2 Simulator changes

Simulator must:

- accept `SIM_TIME_SCALE` (float/int) env var, default 1
- maintain `sim_now` that advances by `dt_wall * SIM_TIME_SCALE`
- all internal integrals (energy) must use **simulated time**, not wall time
- expose `sim_now` in telemetry payload for debugging

### 3.3 Operator changes

Operator must:

- accept same `TIME_SCALE` env var (or Helm value), default 1
- for periodic actions (policy tick, hysteresis hold, cooldown), treat configured values as **sim seconds**
- implement tick scheduling in wall time as:
  - `tick_wall = tick_sim / timeScale`
  - but clamp to a safe minimum wall tick (e.g., >= 200ms) to avoid API spam

### 3.4 Agent changes

Agent pool mode must:

- accept `TIME_SCALE`
- scale reconcile interval similarly:
  - `reconcile_wall = reconcile_sim / timeScale` (clamp >= 200ms)
- still run per-node logical controllers with independent timers

### 3.5 Experiment-level safety

Add in experiment config:

- `minWallTickMs` for operator and agent; enforce it.

---

## 4) Benchmark experiment design (KWOK, 5 nodes, ~200 pods)

### 4.1 Cluster

Use KWOK-based cluster:

- default: `kwokctl create cluster --name joulie-phase2`
- one real node (control-plane) runs operator/agent/simulator
- fake nodes host scheduled workload pods (as API objects)

Enforce separation:

- fake nodes tainted `kwok.x-k8s.io/node=fake:NoSchedule`
- workload pods tolerate this taint and select fake nodes (via nodeSelector `type=kwok`)
- Joulie components do **not** tolerate the taint (stay on real node)

### 4.2 Fake nodes (5 nodes by default)

Create 5 Nodes with:

- labels:
  - `type=kwok`
  - `joulie.io/managed=true`
  - optionally `joulie.io/node-class=classA|classB` for heterogeneity
- allocatable:
  - cpu: e.g. 32 cores
  - memory: e.g. 128Gi
  - pods: e.g. 110
- (future-proof) optional extended resources:
  - `nvidia.com/gpu: "0"` now; allow config to set >0

Implement node manifests as templates:

- `manifests/kwok_nodes.yaml.tpl` rendered by script with N and per-node params from config.

### 4.3 Workload (batch)

Use trace-driven workload:

- 200 jobs/pods total (configurable)
- split into classes: e.g. 30% performance, 70% eco
- requests distribution:
  - cpu request in {1,2,4,8,16} with weights
  - memory request correlated with cpu request
- work budgets heavy-tailed (log-normal)
- utilization profile: start with constant 0.7–1.0 of request, plus optional burst templates

Important: keep requests vs utilization distinct.

Implementation components:

- `workloadgen` produces a trace for a given seed and config.
- simulator injects pods based on trace submit times OR experiment script injects pods (choose one; prefer simulator injection if already supported).

---

## 5) Experiment harness implementation under `experiments/`

### 5.1 Core principles

- Every run is reproducible:
  - fixed seed
  - saved configs
  - saved trace file
  - saved git commit hash
- Every run produces artifacts:
  - `run_summary.json`
  - `jobs.csv` (per job: submit, start, completion, class, requests, work units, node assigned)
  - `nodes_timeseries.csv` (per node per sample: sim_ts, power, util, freq_scale, profile)
  - `policy_timeseries.csv` (sim_ts, pending_perf, hp_target_committed, switches)
  - `run.log` (kubectl logs snapshot)
- Plot generation is automatic and saved as PNG (and optionally PDF).

### 5.2 Run orchestration scripts

#### 5.2.1 `scripts/00_prereqs_check.sh`

- Check required CLIs are installed:
  - `kubectl`, `helm`, `kwokctl` (or `kind` + `kwok` if you support both)
  - `python3` with `matplotlib` and `pandas` (or provide a venv setup)
- Print versions.

#### 5.2.2 `scripts/01_create_cluster_kwokctl.sh`

- Create / reset cluster with deterministic name, e.g. `joulie-phase2`
- Configure kubeconfig context
- Optionally `--delete` existing cluster first

#### 5.2.3 `scripts/02_apply_nodes.sh`

- Render and apply fake node manifests based on `configs/cluster.yaml`
- Wait until nodes are Ready (KWOK sets conditions)
- Validate allocatable resources are visible via `kubectl get nodes -o json`

#### 5.2.4 `scripts/03_install_components.sh`

Installs components for each baseline:

- Baseline A: simulator only
- Baseline B/C: simulator + operator + agent(pool)

Use Helm values files:

- `configs/hardware_profiles.yaml` -> simulator values
- `configs/policy_static.yaml` / `configs/policy_queue_aware.yaml` -> operator values
- `configs/baselines.yaml` selects which values to apply

Ensure TelemetryProfile points agents to simulator endpoints with `{node}` substitution.

#### 5.2.5 `scripts/04_run_one.py` (main single-run driver)

Responsibilities:

1) Load run config and set `run_id` (timestamp + seed + baseline).
2) Generate workload trace using `workloadgen` (or built-in generator):
   - output: `results/<run_id>/trace.jsonl`
3) Apply workload start:
   - either: mount trace into simulator and trigger “start” endpoint
   - or: inject pods according to trace submit times (if simulator does not inject)
4) Wait for completion:
   - completion condition: simulator reports `completed_jobs == total_jobs` via HTTP endpoint `/results/status` OR presence of a “done” ConfigMap/CR created by simulator.
   - include timeout in *wall time*.
5) Collect artifacts:
   - call simulator `/results/export` to download `run_summary.json`, `jobs.csv`, time series files
   - dump operator/agent logs
   - record cluster objects snapshots:
     - `kubectl get pods -A -o json` (optional)
     - `kubectl get nodepowerprofiles -A -o yaml`
6) Write a `metadata.json`:
   - git commit hash
   - baseline
   - seed
   - timeScale
   - policy params
   - trace hash (sha256)

#### 5.2.6 `scripts/05_sweep.py`

- Runs a grid:
  - baselines: A/B/C
  - seeds: 10 seeds (configurable)
- For each run, call `04_run_one.py` with parameters.
- Ensure clean state between runs:
  - delete workload pods namespace(s)
  - reset operator policy state (restart operator pod) to clear hysteresis history OR implement “run_id” namespacing in operator to reset automatically.
  - reset simulator state (restart simulator or call `/reset?run_id=...` endpoint).

Prefer explicit `/reset` endpoints in simulator for speed (no need to redeploy).

#### 5.2.7 `scripts/06_collect.py`

- Aggregates per-run summaries into a single table `results/phase2_summary.csv`:
  - makespan (sim seconds)
  - total energy (J or kWh) (sim integrated)
  - energy per job (kWh/job)
  - mean/p95 completion per class
  - mean/p95 pending per class (especially performance)
  - switch count (churn)
  - saturation ratio (cap saturated time fraction)
- Computes confidence intervals across seeds.

#### 5.2.8 `scripts/07_plot.py`

Generate plots (PNG) into `results/plots/`:

1) **Energy vs makespan scatter** (each point = seed/run, colored by baseline)
2) **Completion time distribution** per class (boxplot or violin) comparing baselines
3) **Pending time p95** for performance pods across baselines
4) **Node profile time series** (for a representative run): hp_target + number of HP nodes vs time
5) **Power time series** (aggregate cluster power vs time) for a representative run
6) **Churn** (bar: number of switches per baseline)

Rules:

- Use matplotlib only (no seaborn).
- Do not hardcode colors (use defaults).

---

## 6) Simulator: experiment-facing endpoints / artifacts (must implement if missing)

To make experiment scripts robust, implement or confirm these simulator endpoints:

- `POST /run/reset`:
  - clears internal state, sets new `run_id`, resets sim clock, clears completed jobs
- `POST /run/load-trace`:
  - accepts trace JSONL upload (or points to mounted file path)
- `POST /run/start`:
  - begins injecting pods / begins execution engine
- `GET /run/status`:
  - returns `{ run_id, sim_now, jobs_total, jobs_completed, jobs_running, jobs_pending, done }`
- `GET /run/export`:
  - downloads a tar/zip containing:
    - `run_summary.json`
    - `jobs.csv`
    - `nodes_timeseries.csv`
    - `policy_timeseries.csv` (if simulator also records observed policy; otherwise collected from operator)
    - `events.log` (optional)

If you already store these as files in a volume, `/run/export` can just stream them.

**Important:** energy integration must be done over **simulated time**.

---

## 7) README and docs (must be written)

### 7.1 `experiments/phase2_kwok_benchmark/README.md`

Must include:

- What the experiment measures (makespan, energy, class latency, pending)
- Baselines A/B/C definitions
- How to run:
  - create cluster
  - apply nodes
  - install components
  - run sweep
  - generate plots
- Where results are stored

Include a “quick run” snippet:

- `./scripts/00_prereqs_check.sh`
- `./scripts/01_create_cluster_kwokctl.sh`
- `./scripts/02_apply_nodes.sh`
- `python3 ./scripts/05_sweep.py --seeds 10`
- `python3 ./scripts/06_collect.py`
- `python3 ./scripts/07_plot.py`

### 7.2 `docs/experiments/phase2-kwok-benchmark.md`

Dedicated doc describing:

- the motivation and assumptions (real scheduler, fake nodes/pods, batch progress, time scaling)
- policy v1 in plain language (target mix, queue-aware, hysteresis, cooldown)
- metrics definitions
- baseline comparisons
- embed or reference the generated plots (images under `experiments/.../results/plots/` or copied into `docs/assets/`)

---

## 8) Run reproducibility + correctness checks

Add checks after each run:

- All jobs completed (`jobs_completed == jobs_total`)
- No job has negative remaining work
- Total energy >= idle energy baseline (sanity)
- In baselines B/C, policy churn is non-zero (at least sometimes), unless workload is trivial
- For baseline A (no Joulie), no control actions were applied (agent absent)

Also:

- Save `kubectl get events -A` snapshot for debugging.

---

## 9) Implementation order (Codex should follow)

1) Implement **Policy v1** in operator + configs + metrics/logs.
2) Implement **timeScale** propagation + clamping in operator and agent (simulator likely already has it; ensure consistent).
3) Implement simulator experiment endpoints (`/run/*`) and export artifacts if missing.
4) Build `experiments/phase2_kwok_benchmark/` skeleton and scripts.
5) Ensure one run works end-to-end for baseline A.
6) Enable baseline B (static partition) and verify.
7) Enable baseline C (queue-aware) and verify.
8) Implement sweep + aggregation + plotting scripts.
9) Write README + docs page and reference generated plots.

---

## 10) Definition of done (Phase 2)

- Running `python3 experiments/phase2_kwok_benchmark/scripts/05_sweep.py --seeds 10` completes successfully and produces:
  - per-run artifacts under `experiments/phase2_kwok_benchmark/results/<run_id>/...`
  - aggregated summary CSV
  - plots under `experiments/phase2_kwok_benchmark/results/plots/`
- Docs page exists at `docs/experiments/phase2-kwok-benchmark.md` and explains:
  - setup, baselines, metrics, and includes the plots.
- Policy v1 behavior observable:
  - hp_target changes with pending performance pods
  - hysteresis + cooldown prevent flapping
- Comparison is meaningful:
  - baseline A has best makespan but highest energy
  - baseline B reduces energy but hurts perf latency/pending
  - baseline C recovers perf latency while keeping much of the energy savings
