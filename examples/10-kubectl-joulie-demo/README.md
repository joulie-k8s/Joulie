# Example: kubectl joulie plugin demo

End-to-end demo of the Joulie energy management system on a 6-node simulated
heterogeneous cluster. Uses KWOK fake nodes, the Joulie simulator for power
telemetry, and the full Joulie control loop (operator + agent + scheduler).
The `kubectl joulie status` plugin shows live cluster energy state.

**Cluster layout:**

- 2× NVIDIA H100-SXM GPU nodes (96 CPU, 8 GPU, 512 GiB)
- 2× AMD Instinct MI300X GPU nodes (128 CPU, 8 GPU, 768 GiB)
- 2× CPU-only compute nodes (64 CPU, 256 GiB)

## Prerequisites

- `kind`, `helm` v3, `kubectl`, Go toolchain
- kubectl-joulie plugin installed:

  ```bash
  make kubectl-plugin && install bin/kubectl-joulie ~/.local/bin/
  ```

## Quick start

```bash
# Generate workload trace (one-time)
make -C examples/10-kubectl-joulie-demo trace

# Run the demo
./examples/10-kubectl-joulie-demo/demo.sh my-demo-cluster
```

The script sets up the full infrastructure automatically, then guides you
through an interactive presentation.

## Demo flow

### Phase 1: Infrastructure setup (automated)

1. Create kind cluster + install KWOK controller
2. Apply KWOK stages + 6 fake nodes
3. Build all Joulie images (operator, agent, scheduler, simulator)
4. Install kube-prometheus-stack (Prometheus + Grafana)
5. Deploy simulator **without workload** (empty trace)
6. Install Joulie (operator + agent in pool mode + scheduler extender + dashboards)
7. Open Grafana via port-forward (`http://localhost:3300`, admin / joulie)

### Phase 2: Interactive demo (guided)

**Step 1 — Show idle cluster:**

```
kubectl get nodes -L nvidia.com/gpu.product,amd.com/gpu.product,joulie.io/power-profile
kubectl joulie status
```

All nodes visible, energy state shows idle power levels, 0% resource allocation.

**Step 2 — Launch workload:**

The script loads the trace into the simulator and restarts it. ~100 AI workload
pods appear across the cluster with realistic CPU, memory, and GPU requests.

**Step 3 — Watch live energy state:**

```
kubectl joulie status -W
```

Watch mode refreshes every 2 seconds — headroom drops, cooling stress rises,
caps get applied. Open Grafana to see the "Joulie Overview" dashboard.

**Step 4 — Reset:**

Stops the simulator, deletes all workload pods, and shows the cluster returning
to idle.

## kubectl joulie status columns

| Column | Meaning |
|--------|---------|
| CLASS | Operator-assigned power profile (eco / performance / draining) |
| HEADROOM | % of capped power budget remaining unused |
| COOLING | Thermal stress — fraction of physical cooling capacity in use |
| CPU% | CPU cores requested vs allocatable |
| MEM% | Memory requested vs allocatable |
| GPU% | GPUs requested vs allocatable (`-` for CPU-only nodes) |
| PODS | Running pods on this node |
| CPU CAP | Current CPU power cap percentage |
| GPU CAP | Current GPU power cap percentage (`-` for CPU-only nodes) |

## Architecture

```
                  ┌──────────────────────────────────────────────┐
                  │              Simulator                       │
                  │  GET /telemetry/{node} → packagePowerWatts   │
                  │  GET /api/v1/query     → facility metrics    │
                  │  GET /metrics          → Prometheus metrics  │
                  └──────┬──────────┬────────────┬──────────────┘
                         │          │            │
        ┌────────────────┘          │            └──────────┐
        │ HTTP (per-node power)     │ fake Prometheus       │ /metrics
        ▼                           ▼ (facility)            ▼
 ┌──────────────┐            ┌──────────────┐        ┌────────────┐
 │   Operator   │            │   Operator   │        │ Prometheus │
 │ resolveNode  │            │ facilityLoop │        │  (scrape)  │
 │ Power(http)  │            │ (prom query) │        └──────┬─────┘
 └──────┬───────┘            └──────┬───────┘               │
        │                           │                       ▼
        └───────────┬───────────────┘                ┌────────────┐
                    ▼                                │  Grafana   │
             ┌──────────────┐                        │ dashboards │
             │  twin.Compute │                       └────────────┘
             │  headroom     │
             │  cooling      │
             │  psu stress   │
             └──────┬───────┘
                    ▼
             ┌──────────────┐     ┌────────────┐
             │  NodeTwin CR │────▶│  Scheduler │
             │  (status)    │     │  Extender  │
             └──────────────┘     └────────────┘

        ┌──────────────────┐
        │      Agent       │◀── HTTP control ── Simulator
        │  (pool mode)     │    /control/{node}
        │  applies caps    │
        └──────────────────┘
```

## Files

| File | Purpose |
|------|---------|
| `demo.sh` | Full setup + interactive demo script |
| `Makefile` | Trace generation helper |
| `kind-cluster.yaml` | kind cluster configuration |
| `00-kwok-stages.yaml` | KWOK stages for node heartbeat + pod lifecycle |
| `01-kwok-nodes.yaml` | 6 fake KWOK nodes (2 NVIDIA, 2 AMD, 2 CPU) |
| `node-classes-data.yaml` | Simulator node class power models |
| `03-simulator-servicemonitor.yaml` | ServiceMonitor for simulator scraping |
| `04-joulie-servicemonitors.yaml` | ServiceMonitors for Joulie components |
| `prometheus-values.yaml` | kube-prometheus-stack Helm values |
| `sim-values.yaml` | Simulator Helm values |
| `joulie-values.yaml` | Joulie Helm values (pool mode + HTTP telemetry) |

## Teardown

```bash
kind delete cluster --name <cluster-name>
```
