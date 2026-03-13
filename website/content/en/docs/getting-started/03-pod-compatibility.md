+++
title = "Pod Compatibility for Joulie"
linkTitle = "Pod Compatibility"
slug = "workload-compatibility"
weight = 3
+++

Joulie uses Kubernetes scheduling constraints as the single source of truth for workload placement intent.

Power profile supply is exposed on node label:

- `joulie.io/power-profile=performance`
- `joulie.io/power-profile=eco`
- `joulie.io/draining=true|false` (independent transition flag)

Workload behavior:

- `performance` workload (recommended): require `joulie.io/power-profile NotIn ["eco"]`
- `eco` workload: require `joulie.io/power-profile=eco`
- unconstrained workload: no power-profile affinity, can run on either profile

## Best-effort Pod (unconstrained, starting point)

This is the default and recommended starting spec.
Do not set power-profile affinity: Kubernetes can schedule the pod on either eco or performance nodes.

{{< highlight yaml "linenos=table" >}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-workload
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-workload
  template:
    metadata:
      labels:
        app: my-workload
    spec:
      containers:
      - name: app
        image: ghcr.io/example/app:latest
{{< /highlight >}}

## Performance Pod (recommended, lines to add)

{{< highlight yaml "linenos=table,hl_lines=15-22" >}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-workload
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-workload
  template:
    metadata:
      labels:
        app: my-workload
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: joulie.io/power-profile
                operator: NotIn
                values: ["eco"]
      containers:
      - name: app
        image: ghcr.io/example/app:latest
{{< /highlight >}}

Why this is recommended:

- it avoids eco nodes while still allowing high-performance nodes which are not managed by Joulie;
- explicitly requiring `performance` can exclude unlabeled nodes that are still valid for performance workloads and are managed by Joulie.

## Eco-only Pod (advanced, lines to add)

This is an advanced/rare pattern.
Use it only when you explicitly need jobs to run on eco supply.
In most cases, users should either:

- use `NotIn ["eco"]` for performance-sensitive pods, or
- keep pods unconstrained (best-effort) and let Joulie manage power behavior.

If you choose eco-only, adding `joulie.io/draining=false` avoids nodes in transition from performance to eco, which are labelled with `joulie.io/power-profile=eco` but still have a performance power profile (`DrainingPerformance` state).

{{< highlight yaml "linenos=table,hl_lines=15-25" >}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-workload
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-workload
  template:
    metadata:
      labels:
        app: my-workload
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: joulie.io/power-profile
                operator: In
                values: ["eco"]
              - key: joulie.io/draining
                operator: In
                values: ["false"]
      containers:
      - name: app
        image: ghcr.io/example/app:latest
{{< /highlight >}}

Reference manifests:

- [Example 03 workloads](https://github.com/joulie-k8s/Joulie/blob/main/examples/03-workload-intent-classes/deployments.yaml)

## GPU resource requests

GPU scheduling resources (`nvidia.com/gpu`, `amd.com/gpu`) are independent from Joulie power-profile labels.

- request GPU resources as usual in pod/container resources,
- keep Joulie intent guidance based on power-profile constraints (`NotIn ["eco"]` for performance-sensitive workloads),
- remember Joulie GPU capping is node-level (not per-container GPU slicing).
