# 02 Workload classes and the scheduler extender

One pod annotation tells Joulie what a workload needs:

```yaml
metadata:
  annotations:
    joulie.io/workload-class: performance   # or standard, the default
```

The scheduler extender reads it. A `performance` pod never lands on an eco
node, whose caps would slow it; a `standard` pod may run anywhere, and the
extender's scores steer it towards the nodes where it costs the least power
headroom. This tutorial runs one Deployment of each class and shows where they
land and why.

Needs the cluster of [01 Simulated cluster](../01-simulated-cluster/README.md),
with `KUBECONFIG` set as there. Its scheduler already calls the extender; on a
real cluster, the
[scheduler page](https://joulie-k8s.github.io/Joulie/main/docs/architecture/scheduler/)
shows the kube-scheduler configuration.

## 1. Run both classes

[`workloads.yaml`](workloads.yaml) holds two Deployments of six pods that
differ only in the annotation:

```bash
kubectl get nodes -L joulie.io/power-profile
kubectl apply -f examples/02-workload-classes/workloads.yaml
kubectl -n joulie-tutorial get pods -o custom-columns=POD:.metadata.name,CLASS:.metadata.annotations.joulie\\.io/workload-class,NODE:.spec.nodeName
```

<!-- tutorials: check
kubectl -n joulie-tutorial wait --for=condition=Ready pod --all --timeout=180s
eco=" $(kubectl get nodes -l joulie.io/power-profile=eco -o jsonpath='{.items[*].metadata.name}') "
for node in $(kubectl -n joulie-tutorial get pods -l app=performance -o jsonpath='{.items[*].spec.nodeName}'); do
  [[ $eco != *" $node "* ]] || { echo "a performance pod runs on the eco node $node" >&2; exit 1; }
done
-->

```text
POD                            CLASS         NODE
performance-7498875957-8pzc9   performance   xeon-0
performance-7498875957-bn5wd   performance   xeon-0
...
standard-7f66876fc-22sf4       standard      epyc-1
standard-7f66876fc-2bwj6       standard      epyc-1
...
```

Every performance pod runs on a performance node. The standard pods went to
`epyc-1`, an eco node: they do not need full power, and putting them there
keeps the performance nodes free for the work that does. Which nodes you get
depends on the profiles the policy chose, in the first command's output, and
on the load at that moment, but no performance pod lands on an eco node.

`kubectl joulie pods` (see 01) shows the same placement, and flags a
performance pod on an eco node, which happens only if its node turned eco
after the pod started: the policy then keeps the node in performance, draining,
until such pods end.

## 2. Why: the extender's filter and scores

For each pod the scheduler asks the extender twice: which nodes are allowed
(filter), and how good each allowed node is (prioritize). The extender exports
both as metrics, which the simulated cluster's Prometheus scrapes:

```bash
q() { kubectl get --raw "/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy/api/v1/query?query=$1" |
  jq -r '.data.result[] | "\(.metric.node // "") \(.metric.workload_class) \(.value[1])"'; }
q 'sum%20by%20(workload_class)%20(joulie_scheduler_filter_requests_total)'
q 'joulie_scheduler_final_node_score' | sort -k2,2 -k3,3nr
```

<!-- tutorials: check
eventually '[ -n "$(q "sum%20by%20(workload_class)%20(joulie_scheduler_filter_requests_total)" | awk "\$1 == \"performance\" && \$2 > 0")" ]'
eventually '[ -n "$(q joulie_scheduler_final_node_score | awk "\$2 == \"standard\"")" ]'
-->

```text
 standard 13
 performance 9
xeon-0 performance 53.213882955412274
epyc-0 performance 47.23273904201457
h100-0 performance 46.95875172230502
l40s-0 performance 30.674416622227046
h100-1 standard 71.97452293373433
h100-0 standard 65.7040956960054
l40s-0 standard 61.68591573926636
epyc-1 standard 55.65615182652117
xeon-2 standard 50.593560856731756
...
```

The performance class has scores only for performance nodes: the filter
removed the eco ones before scoring. The standard class has a score for every
node, eco ones included, and an eco node gets a bonus of 10 for a standard
pod. The rest of the score is the power headroom the node would have left
under its cap once the pod runs, its cooling stress from the `NodeTwin`, and
its power trend. The scores move with the load; the extender logs the marginal
power it expects the pod to add on each node:

```bash
kubectl -n joulie-system logs deploy/joulie-scheduler-extender --tail=5
# marginal: node=xeon-0 pod_cpu=4.00 pod_gpu=0 delta=12.3W [cpu=12.3W(4.00 cores/128, coeff=0.70) ...]
```

The [energy-aware scheduling page](https://joulie-k8s.github.io/Joulie/main/docs/architecture/energy-aware-scheduling/)
gives the formula. [03 Grafana dashboard](../03-grafana-dashboard/README.md)
plots the same metrics.

## Clean up

```bash
kubectl delete -f examples/02-workload-classes/workloads.yaml
```

## On a real cluster

Drop the `nodeSelector` and the toleration from `workloads.yaml`: they place
the pods on the simulated cluster's fake Nodes. Install the chart with
`schedulerExtender.enabled=true` and configure kube-scheduler to call it; the
[pod compatibility page](https://joulie-k8s.github.io/Joulie/main/docs/getting-started/workload-compatibility/)
covers GPU requests and pods without the annotation.
