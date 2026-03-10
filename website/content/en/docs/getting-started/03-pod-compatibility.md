+++
title = "Pod Compatibility for Joulie"
linkTitle = "Pod Compatibility"
slug = "workload-compatibility"
weight = 3
+++

Joulie uses Kubernetes scheduling constraints as the single source of truth for workload placement intent.

Power profile supply is exposed on node label:

- `joulie.io/power-profile=performance`
- `joulie.io/power-profile=draining-performance` (temporary transition state)
- `joulie.io/power-profile=eco`

Workload behavior:

- `performance` workload: require `joulie.io/power-profile=performance`
- `eco` workload: require `joulie.io/power-profile=eco`
- unconstrained workload: no power-profile affinity, can run on either profile

## Base Pod Spec

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

## Performance-only Pod (lines to add)

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
                operator: In
                values: ["performance"]
      containers:
      - name: app
        image: ghcr.io/example/app:latest
{{< /highlight >}}

## Eco-only Pod (lines to add)

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
                operator: In
                values: ["eco"]
      containers:
      - name: app
        image: ghcr.io/example/app:latest
{{< /highlight >}}

## Unconstrained Pod

Do not set power-profile affinity. Kubernetes can schedule it on either eco or performance nodes.

Note on transitions:

- during `performance -> eco` safeguard phase, operator may set node label to `draining-performance`;
- strict `performance` and strict `eco` affinity pods do not match that temporary label;
- this helps avoid admitting new strict placements while a node is draining.

Reference manifests:

- [Example 03 workloads](https://github.com/joulie-k8s/Joulie/blob/main/examples/03-workload-intent-classes/deployments.yaml)
