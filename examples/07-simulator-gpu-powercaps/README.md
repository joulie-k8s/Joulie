# Example: Simulator GPU Power Caps (Heterogeneous)

This example validates GPU capping end-to-end in simulator mode on a heterogeneous virtual cluster of KWOK nodes:

- 33 NVIDIA H100 NVL nodes, 8 GPUs each,
- 8 CPU-only nodes (AMD EPYC 9375F, 9655 and 9965).

It demonstrates that tighter GPU caps reduce simulated GPU power and increase completion time for GPU jobs.

The example uses three contracts with separate roles:

- node labels: simulated hardware identity bootstrap
- `NodeTwin`: desired caps/profile
- Helm values (`manifests/30-joulie-values-pool.yaml`): the agents' telemetry and control, and the controller manager's node power, routed to the simulator over HTTP

`NodeHardware` is published automatically by the agent for observability and controller manager planning.
You do not need to create it by hand for this example.

## Important validation note

GPU support has been validated in simulator mode only (no bare-metal GPU access yet).
The host code paths are designed for NVIDIA/AMD bare metal, but this example is simulator-first.

## Files

- `manifests/00-kwok-nodes.yaml`: heterogeneous fake nodes (NVIDIA GPU + CPU-only)
- `manifests/01-kind-cluster.yaml`: kind cluster config (reused from benchmark experiment profile)
- `manifests/10-node-classes.yaml`: simulator hardware classes with GPU model caps
- `manifests/20-simulator.yaml`: simulator deployment using class config + GPU trace
- `manifests/30-joulie-values-pool.yaml`: Joulie Helm values for pool mode, GPU cap policy and simulator HTTP routing
- `manifests/50-workload-trace-configmap.yaml`: mixed CPU/GPU workload trace

## Run

Single command (recommended for debug-heavy runs):

```bash
./run-e2e.sh
```

The script creates/reuses a kind+KWOK cluster, builds and loads local `agent`/`controller-manager`/`simulator` images, deploys the full example, validates GPU control behavior, and writes a full artifact bundle under `tmp/gpu-e2e-*` (logs, events, pod/node descriptions, CRs, simulator debug endpoints, metrics).

By default, the script first uses `manifests/01-kind-cluster.yaml`; if cluster creation fails on the host runtime, it retries with kind defaults, and if that still fails it reuses an already-running healthy kind cluster context when available.
Set `KIND_FALLBACK_NO_CONFIG=false` to disable the default-config retry.
Set `KIND_REUSE_EXISTING_ON_CREATE_FAILURE=false` to disable automatic reuse of existing kind clusters.

1. Create a kind cluster using this example config (same profile used in benchmark experiment):

```bash
kind create cluster --name joulie-gpu-e2e --config manifests/01-kind-cluster.yaml
kubectl config use-context kind-joulie-gpu-e2e
```

2. Install KWOK:

```bash
KWOK_VER=$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | jq -r .tag_name)
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"
kubectl -n kube-system rollout status deploy/kwok-controller
```

3. (Optional) Use the one-command runner with a specific cluster/config:

```bash
CLUSTER_NAME=joulie-gpu-e2e \
KIND_CLUSTER_CONFIG="$(pwd)/manifests/01-kind-cluster.yaml" \
./run-e2e.sh
```

4. Manual flow: apply fake nodes and simulator stack:

```bash
kubectl apply -f manifests/00-kwok-nodes.yaml
kubectl apply -f manifests/10-node-classes.yaml
kubectl apply -f manifests/50-workload-trace-configmap.yaml
kubectl apply -f manifests/20-simulator.yaml
```

5. Install Joulie in pool mode. `helm upgrade` never updates CRDs, so apply them first:

```bash
kubectl apply -f ../../charts/joulie/crds/
helm upgrade --install joulie ../../charts/joulie \
  -n joulie-system --create-namespace \
  -f manifests/30-joulie-values-pool.yaml
```

6. The values file routes everything to the simulator over HTTP, so the example needs no Prometheus: the pool agents read node power from `/otlp/telemetry/{node}` and send CPU and GPU controls to `/control/{node}` (`TELEMETRY_*`), and the controller manager reads measured node power from `/otlp/telemetry/{node}` (`NODE_POWER_SOURCE=http`, `NODE_POWER_HTTP_ENDPOINT`). With the chart's default, `NODE_POWER_SOURCE=prometheus`, every node would read 0 W here, with source `prometheus-no-data`.

7. Observe control + energy behavior:

```bash
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=200 | egrep 'gpu|dvfs|policy'
kubectl -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=200 | egrep 'gpu|control node=|job completed'
```

8. Compare profiles:

- performance GPU cap (`GPU_PERFORMANCE_CAP_PCT_OF_MAX=100`) baseline,
- eco GPU cap (`GPU_ECO_CAP_PCT_OF_MAX=60`) reduced power / longer GPU completion.
