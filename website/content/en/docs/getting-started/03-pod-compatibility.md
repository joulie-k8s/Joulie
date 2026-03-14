+++
title = "Pod Compatibility for Joulie"
linkTitle = "Pod Compatibility"
slug = "workload-compatibility"
weight = 3
+++

Joulie uses a single pod annotation to express workload placement intent:

```
joulie.io/workload-class: performance | standard | best-effort
```

The scheduler extender reads this annotation and steers pods accordingly. No node affinity rules are needed.

## Workload classes

| Class | Behavior |
|-------|----------|
| `performance` | Must run on full-power nodes. The extender hard-rejects eco nodes. |
| `standard` | Default. Prefers performance nodes, tolerates eco. |
| `best-effort` | Prefers eco nodes, leaves performance capacity free for critical workloads. |

If no annotation is present and no `WorkloadProfile` matches the pod, the extender treats it as `standard`.

## Performance pod

Add the `joulie.io/workload-class: performance` annotation. The scheduler extender will reject eco nodes for this pod.

{{< highlight yaml "linenos=table,hl_lines=6-7" >}}
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

{{< highlight yaml "linenos=table,hl_lines=6-7" >}}
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

## Best-effort pod

Best-effort pods are steered toward eco nodes, freeing performance capacity for critical workloads.

{{< highlight yaml "linenos=table,hl_lines=6-7" >}}
apiVersion: v1
kind: Pod
metadata:
  name: my-batch-job
  annotations:
    joulie.io/workload-class: best-effort
spec:
  containers:
  - name: app
    image: ghcr.io/example/batch:latest
{{< /highlight >}}

## GPU resource requests

GPU scheduling resources (`nvidia.com/gpu`, `amd.com/gpu`) are independent from Joulie workload classes.

- Request GPU resources as usual in pod/container resources.
- Set `joulie.io/workload-class` to express your placement intent.
- Joulie GPU capping is node-level (not per-container GPU slicing).

Example: a performance GPU inference pod:

{{< highlight yaml "linenos=table,hl_lines=6-8" >}}
apiVersion: v1
kind: Pod
metadata:
  name: gpu-inference
  annotations:
    joulie.io/workload-class: performance
    joulie.io/gpu-sensitivity: high
spec:
  containers:
  - name: inference
    image: ghcr.io/example/inference:latest
    resources:
      limits:
        nvidia.com/gpu: "1"
{{< /highlight >}}

## Sensitivity annotations

For finer control, add sensitivity annotations so the extender can prefer nodes with more headroom:

| Annotation | Values | Effect |
|---|---|---|
| `joulie.io/workload-class` | `performance`, `standard`, `best-effort` | Controls eco/performance placement |
| `joulie.io/cpu-sensitivity` | `high`, `medium`, `low` | Scales penalty on capped CPU nodes |
| `joulie.io/gpu-sensitivity` | `high`, `medium`, `low` | Scales penalty on capped GPU nodes |

All annotations are optional. If omitted and no `WorkloadProfile` matches the pod, the extender scores neutrally.

## WorkloadProfile-based scheduling

For teams that prefer not to annotate individual pods, create a `WorkloadProfile` with a `podSelector` matching your workload's labels. The extender will use the profile's fields to drive filter and score logic automatically.

See [WorkloadProfile Guide]({{< relref "/docs/getting-started/04-workload-profiles.md" >}}) for details.
