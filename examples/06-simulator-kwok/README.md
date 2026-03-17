# Example: KWOK Simulator (Workload + Power + Agent Pool)

This example runs:

- real Kubernetes API + scheduler,
- real infra nodes (kind worker),
- fake KWOK nodes/pods,
- real Joulie operator,
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
  --set operator.image.tag="$TAG" \
  -f manifests/15-joulie-values-pool.yaml
```

## 6. Route telemetry/control to simulator

Configure agent env vars to route telemetry and control to the simulator over HTTP:

```bash
kubectl -n joulie-system set env daemonset/joulie-agent \
  JOULIE_TELEMETRY_SOURCE_TYPE=http \
  JOULIE_TELEMETRY_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/telemetry/{node} \
  JOULIE_CONTROL_TYPE=http \
  JOULIE_CONTROL_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node} \
  JOULIE_CONTROL_MODE=dvfs
```

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

- infra pods (`joulie-simulator`, operator, agent pool) schedule on real kind node(s),
- fake workload pods are scheduled on fake KWOK nodes,
- operator writes `NodeTwin` for managed fake nodes,
- exactly one pool shard controls each node,
- simulator power/freq telemetry changes with controls,
- job completion slows under stronger caps/throttling.

## Troubleshooting

- `joulie-telemetry-sim` Pending with `untolerated taint kwok.x-k8s.io/node=fake`:
  - your cluster has no schedulable real node.
  - recreate cluster with `manifests/01-kind-cluster.yaml` so infra has a real worker.
