+++
title = "Pod Compatibility for Joulie"
linkTitle = "Pod Compatibility"
slug = "workload-compatibility"
weight = 3
+++

Joulie uses a single pod annotation to express workload placement intent:

```
joulie.io/workload-class: performance | standard
```

The scheduler extender reads this annotation and steers pods accordingly. No node affinity rules are needed.

## Workload classes

| Class | Behavior |
|-------|----------|
| `performance` | Must run on full-power nodes. The extender hard-rejects eco nodes. |
| `standard` | Default. Can run on any node. Adaptive scoring steers toward eco when performance nodes are congested. |

If no annotation is present, the extender treats it as `standard`.

## Performance pod

Add the `joulie.io/workload-class: performance` annotation. The scheduler extender will reject eco nodes for this pod.

{{< highlight yaml "linenos=table,hl_lines=6" >}}
apiVersion: v1
kind: Pod
metadata:
  name: my-critical-service
  annotations:
    joulie.io/workload-class: performance
spec:
  containers:
  - name: app
    image: ghcr.io/example/app:latest
{{< /highlight >}}

## Standard pod (default)

No annotation is required. The extender scores performance nodes higher but does not reject eco nodes.

{{< highlight yaml "linenos=table" >}}
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
spec:
  containers:
  - name: app
    image: ghcr.io/example/app:latest
{{< /highlight >}}

You can also be explicit:

{{< highlight yaml "linenos=table,hl_lines=6" >}}
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
  annotations:
    joulie.io/workload-class: standard
spec:
  containers:
  - name: app
    image: ghcr.io/example/app:latest
{{< /highlight >}}

## GPU resource requests

GPU scheduling resources (`nvidia.com/gpu`, `amd.com/gpu`) are independent from Joulie workload classes.

- Request GPU resources as usual in pod/container resources.
- Set `joulie.io/workload-class` to express your placement intent.
- Joulie GPU capping is node-level (not per-container GPU slicing).

Example: a performance GPU inference pod:

{{< highlight yaml "linenos=table,hl_lines=6" >}}
apiVersion: v1
kind: Pod
metadata:
  name: gpu-inference
  annotations:
    joulie.io/workload-class: performance
spec:
  containers:
  - name: inference
    image: ghcr.io/example/inference:latest
    resources:
      limits:
        nvidia.com/gpu: "1"
{{< /highlight >}}

