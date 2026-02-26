# Example: Workload Intent Classes on 2 Virtualized Workers

This example demonstrates scheduler adaptation to Joulie profile flips even when RAPL/DVFS enforcement is unavailable.

You will run:

- operator policy loop that flips node profiles every `N` minutes,
- three Deployments with workload intent classes (`performance`, `eco`, `flex`),
- a recycler CronJob that periodically deletes pods so new pods are rescheduled against the latest node profile.

## 1) Preconditions

- two worker nodes labeled as managed:

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite
```

- operator reconcile interval configured (example `1m`):

```bash
kubectl -n joulie-system set env deploy/joulie-operator RECONCILE_INTERVAL=1m
kubectl -n joulie-system rollout status deploy/joulie-operator
```

Check operator output:

```bash
kubectl -n joulie-system logs deploy/joulie-operator --tail=200 | grep "assigned profiles"
kubectl -n joulie-system logs deploy/joulie-operator --tail=200 | grep "transition deferred"
kubectl get nodepowerprofiles -o wide
kubectl get nodes -L joulie.io/power-profile
```

## 2) Deploy intent-class workloads (as Deployments)

```bash
kubectl apply -f examples/workload-intent-classes/namespace.yaml
kubectl apply -f examples/workload-intent-classes/deployments.yaml
kubectl apply -f examples/workload-intent-classes/recycler.yaml
kubectl apply -f examples/workload-intent-classes/servicemonitor-operator.yaml
```

Note: `servicemonitor-operator.yaml` assumes Prometheus Operator release label `release: telemetry` in namespace `default`.

What each workload expresses:

- `performance`: hard requires `joulie.io/power-profile=performance`
- `eco`: hard requires `joulie.io/power-profile=eco`
- `flex`: soft prefers `eco`, can run on `performance`

## 3) Observe pod movement across nodes

```bash
watch -n 5 'kubectl -n joulie-intent-demo get pods -o wide; echo; kubectl get nodepowerprofiles -o wide; echo; kubectl get nodes -L joulie.io/power-profile'
```

You should see pods recreated roughly every minute by the recycler, then placed according to the profile labels currently set by the operator.
If a node is transitioning `ActivePerformance -> ActiveEco` and still has running `performance` intent workloads, the operator now defers the downgrade until those pods terminate.
During defer, the node label is set to `joulie.io/power-profile=draining-performance`, so new `performance` pods stop landing there.

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
- `Operator Profile vs Node Label (Side-by-side)`:
  - compare operator-exported profile signal with actual `joulie.io/power-profile` node label value
- `Pods by Intent Class and Node`:
  - shows how `performance`, `eco`, `flex` workloads land on nodes
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
