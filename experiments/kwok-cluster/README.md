# KWOK cluster of experiments 01 to 03

The cluster the benchmark experiments run on: kind with KWOK fake nodes, the
simulator in its HTTP mode, and Joulie with its agents in pool mode, one agent
pod serving many fake nodes. The agents read node power from the simulator and
send it their CPU and GPU caps over HTTP, so no Prometheus is needed. The
experiments' `scripts/03_install_components.sh` deploy these files; the
manifests here describe a heterogeneous inventory of 33 NVIDIA H100 NVL nodes
with 8 GPUs each and 8 CPU-only nodes (AMD EPYC 9375F, 9655 and 9965), which
`scripts/generate_heterogeneous_assets.py` writes from experiment 02's
inventory.

This directory was example 07 until #89. Its end-to-end script, `run-e2e.sh`,
stopped at `kubectl apply` of the node classes, a data file since #46, and was
removed with the smoke scripts of experiments 02 and 03 that ran it. To
learn Joulie, use the [examples](../../examples/README.md): they run on the
simulated cluster (`make simcluster-*`), where the real agent discovers and
caps emulated hardware through the same files and tools as on a real node.

## Files

- `manifests/00-kwok-nodes.yaml`: heterogeneous fake nodes (NVIDIA GPU + CPU-only)
- `manifests/01-kind-cluster.yaml`: kind cluster config (reused from benchmark experiment profile)
- `manifests/10-node-classes.yaml`: simulator hardware classes with GPU model caps
- `manifests/20-simulator.yaml`: simulator deployment using class config + GPU trace
- `manifests/30-joulie-values-pool.yaml`: Joulie Helm values for pool mode, GPU cap policy and simulator HTTP routing
- `manifests/50-workload-trace-configmap.yaml`: mixed CPU/GPU workload trace

## Bring it up by hand

On a kind cluster made from `manifests/01-kind-cluster.yaml`, with KWOK
installed, from this directory:

1. Apply the fake nodes and the simulator:

```bash
kubectl apply -f manifests/00-kwok-nodes.yaml
kubectl create namespace joulie-sim-demo
kubectl -n joulie-sim-demo create configmap joulie-simulator-node-classes \
  --from-file=node-classes.yaml=manifests/10-node-classes.yaml
kubectl apply -f manifests/50-workload-trace-configmap.yaml
kubectl apply -f manifests/20-simulator.yaml
```

2. Install Joulie in pool mode. `helm upgrade` never updates CRDs, so apply them first:

```bash
kubectl apply -f ../../charts/joulie/crds/
helm upgrade --install joulie ../../charts/joulie \
  -n joulie-system --create-namespace \
  -f manifests/30-joulie-values-pool.yaml
```

3. The values file routes everything to the simulator over HTTP, so the cluster needs no Prometheus: the pool agents read node power from `/otlp/telemetry/{node}` and send CPU and GPU controls to `/control/{node}` (`TELEMETRY_*`), and the controller manager reads measured node power from `/otlp/telemetry/{node}` (`NODE_POWER_SOURCE=http`, `NODE_POWER_HTTP_ENDPOINT`). With the chart's default, `NODE_POWER_SOURCE=prometheus`, every node would read 0 W here, with source `prometheus-no-data`.

4. Observe control + energy behavior:

```bash
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=200 | egrep 'gpu|dvfs|policy'
kubectl -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=200 | egrep 'gpu|control node=|job completed'
```

5. Compare profiles:

- performance GPU cap (`GPU_PERFORMANCE_CAP_PCT_OF_MAX=100`) baseline,
- eco GPU cap (`GPU_ECO_CAP_PCT_OF_MAX=60`) reduced power / longer GPU completion.
