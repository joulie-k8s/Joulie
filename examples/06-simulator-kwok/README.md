# Example: KWOK Simulator (Workload + Power + Agent Pool)

This example runs:

- real Kubernetes API + scheduler,
- real infra nodes (kind worker),
- fake KWOK nodes/pods,
- real Joulie controller manager,
- Joulie agent in `pool` mode,
- Joulie simulator with HTTP telemetry/control and trace-driven batch workload.

## Prerequisites

- `kubectl`, `helm`, `docker`
- `kind`
- `jq` (to resolve latest KWOK release)
- Joulie images available in your registry

## 1. Create mixed kind cluster (real nodes)

```bash
kind create cluster --name joulie-kwok --config manifests/01-kind-cluster.yaml
kubectl cluster-info --context kind-joulie-kwok
kubectl get nodes -o wide
```

You should see at least one real schedulable worker node.

## 2. Install KWOK controllers in-cluster

```bash
# https://kwok.sigs.k8s.io/docs/user/kwok-in-cluster/
KWOK_VER=$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | jq -r .tag_name)
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"
kubectl -n kube-system get deploy kwok-controller
```

## 3. Create fake nodes

```bash
kubectl apply -f manifests/00-kwok-nodes.yaml
kubectl get nodes -l type=kwok
```

Nodes are tainted with `kwok.x-k8s.io/node=fake:NoSchedule` and labeled `joulie.io/managed=true`.
This keeps fake-workload placement explicit while infra pods stay on real nodes.

## 4. Install simulator

```bash
kubectl apply -f manifests/10-simulator.yaml
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
```

## 5. Install Joulie in pool mode

```bash
export TAG=dev0.0.5
helm upgrade --install joulie ../../charts/joulie \
  -n joulie-system --create-namespace \
  --set agent.image.tag="$TAG" \
  --set controllerManager.image.tag="$TAG" \
  -f manifests/15-joulie-values-pool.yaml
```

`helm upgrade` never updates CRDs: on a cluster where Joulie was installed before, run `kubectl apply -f ../../charts/joulie/crds/` first.

## 6. Telemetry and control go to the simulator

`manifests/15-joulie-values-pool.yaml` routes both components to the simulator over HTTP, so this example needs no Prometheus:

- the pool agents read each node's power from `/otlp/telemetry/{node}` and send CPU controls to `/control/{node}` (`TELEMETRY_CPU_*`);
- the controller manager reads measured node power from the same `/otlp/telemetry/{node}` (`NODE_POWER_SOURCE=http`, `NODE_POWER_HTTP_ENDPOINT`). With the chart's default, `NODE_POWER_SOURCE=prometheus`, every node would read 0 W here, with source `prometheus-no-data`.

## 7. Load trace workload

```bash
kubectl apply -f manifests/30-workload-trace-configmap.yaml
kubectl rollout restart deploy/joulie-telemetry-sim -n joulie-sim-demo
```

The simulator reads `SIM_WORKLOAD_TRACE_PATH=/etc/joulie-sim-trace/trace.jsonl` and creates pods.

## 8. Verify closed loop

```bash
kubectl get nodes -L type,joulie.io/managed
kubectl get pods -A -l app.kubernetes.io/part-of=joulie-sim-workload -o wide
kubectl get nodetwins
kubectl -n joulie-system logs -l app.kubernetes.io/name=joulie-agent --tail=200 | egrep 'controller started|dvfs-control|applied policy'
kubectl -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=200 | egrep 'control node=|job completed'
```

Optional direct debug:

```bash
kubectl -n joulie-sim-demo port-forward deploy/joulie-telemetry-sim 18080:18080
curl -s localhost:18080/debug/nodes | jq
curl -s localhost:18080/debug/events | jq
```

## Expected behavior

- infra pods (`joulie-telemetry-sim`, controller manager, agent pool) schedule on real kind node(s),
- fake workload pods are scheduled on fake KWOK nodes,
- controller manager writes `NodeTwin` for managed fake nodes,
- exactly one pool shard controls each node,
- simulator power/freq telemetry changes with controls,
- job completion slows under stronger caps/throttling.

## Troubleshooting

- `joulie-telemetry-sim` Pending with `untolerated taint kwok.x-k8s.io/node=fake`:
  - your cluster has no schedulable real node.
  - recreate cluster with `manifests/01-kind-cluster.yaml` so infra has a real worker.
