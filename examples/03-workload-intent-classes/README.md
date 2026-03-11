# Example: Workload Scheduling Classes on 2 Virtualized Workers

This example demonstrates scheduler adaptation to Joulie profile flips even when RAPL/DVFS enforcement is unavailable.

You will run:

- operator policy loop that flips node profiles every `N` minutes,
- two Deployments with scheduling classes (`performance`, `eco`) expressed via node affinity,
- a recycler CronJob that periodically deletes pods so new pods are rescheduled against the latest node profile.

## 1) Preconditions

- two worker nodes labeled as managed:

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite
```

- kube-prometheus-stack installed (if not already present):

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install telemetry prometheus-community/kube-prometheus-stack -f values/prometheus-grafana.yaml
```

- operator reconcile interval configured (example `2m`):

```bash
kubectl -n joulie-system set env deploy/joulie-operator RECONCILE_INTERVAL=2m
kubectl -n joulie-system rollout status deploy/joulie-operator
```

Check operator output:

```bash
kubectl -n joulie-system logs deploy/joulie-operator --tail=200 | grep "assigned profiles"
kubectl -n joulie-system logs deploy/joulie-operator --tail=200 | grep "transition deferred"
kubectl get nodepowerprofiles -o wide
kubectl get nodes -L joulie.io/power-profile
```

## 2) Run the experiment (full sequence)

```bash
# label managed workers
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite

# fast operator loop for demo
kubectl -n joulie-system set env deploy/joulie-operator RECONCILE_INTERVAL=2m
kubectl -n joulie-system rollout status deploy/joulie-operator

# deploy workloads + recycler + operator ServiceMonitor
kubectl apply -f examples/03-workload-intent-classes/namespace.yaml
kubectl apply -f examples/03-workload-intent-classes/deployments.yaml
kubectl apply -f examples/03-workload-intent-classes/recycler.yaml
kubectl apply -f examples/03-workload-intent-classes/servicemonitor-operator.yaml

# sanity checks
kubectl get nodepowerprofiles -o wide
kubectl get nodes -L joulie.io/power-profile
kubectl -n joulie-intent-demo get pods -o wide
kubectl -n joulie-system logs deploy/joulie-operator --tail=200 | egrep "assigned profiles|transition deferred"
```

You can also watch the status in real time:

```bash
watch -n 5 'kubectl -n joulie-intent-demo get pods -o wide; echo; kubectl get nodes -L joulie.io/power-profile'
```

Note: `servicemonitor-operator.yaml` assumes Prometheus Operator release label `release: telemetry` in namespace `default`.

What each workload expresses (single source of truth is affinity):

- `performance`: recommended pattern is `joulie.io/power-profile NotIn ["eco"]`
- `eco`: hard requires `joulie.io/power-profile=eco`
- no power-profile affinity: implicit unconstrained/general scheduling class

## 3) Observe pod movement across nodes

```bash
watch -n 5 'kubectl -n joulie-intent-demo get pods -o wide; echo; kubectl get nodepowerprofiles -o wide; echo; kubectl get nodes -L joulie.io/power-profile'
```

You should see pods recreated roughly every minute by the recycler, then placed according to the profile labels currently set by the operator.
If a node is transitioning `ActivePerformance -> ActiveEco` and still has running `performance`-class workloads (classified from pod scheduling constraints), the operator marks the node as draining with `joulie.io/draining=true`.
Advanced eco-only constraints can use `joulie.io/draining=false` to avoid mid-transition nodes.

## 4) Grafana dashboard for this demo

Import dashboard JSON:

- [dashboard-workload-intents.json](./dashboard-workload-intents.json)

Suggested local access:

```bash
kubectl port-forward svc/telemetry-kube-prometheus-prometheus 9090:9090 1>/dev/null &
kubectl port-forward svc/telemetry-grafana 5000:80 1>/dev/null &
```

Dashboard highlights:

- `FSM State by Node (Operator Metric)`:
  - visualize `ActivePerformance` / `DrainingPerformance` / `ActiveEco` per node
- `State Transitions (5m)`:
  - `applied` transitions and `deferred` transitions from operator guard logic
- `Kubernetes Node Label Profile (kube_node_labels)`:
  - actual `joulie.io/power-profile` label value from Kubernetes
- `Kubernetes Draining Flag (kube_node_labels)`:
  - actual `joulie.io/draining` transition flag from Kubernetes
- `Pods by Node`:
  - shows how workload pods land on nodes as profiles flip
- `Pod Re-creations (5m)`:
  - confirms periodic recreation churn driving re-scheduling

Quick operator metric sanity check:

```bash
kubectl -n joulie-system port-forward svc/joulie-operator-metrics 18081:8081
curl -s localhost:18081/metrics | grep '^joulie_operator_' | head -n 30
```

## 5) Cleanup

```bash
kubectl delete namespace joulie-intent-demo
```
