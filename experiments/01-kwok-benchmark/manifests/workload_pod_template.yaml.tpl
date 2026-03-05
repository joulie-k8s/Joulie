apiVersion: v1
kind: Pod
metadata:
  name: {{NAME}}
  namespace: {{NAMESPACE}}
  labels:
    app.kubernetes.io/part-of: kwok-benchmark
  annotations:
    sim.joulie.io/jobId: {{JOB_ID}}
spec:
  restartPolicy: Never
  nodeSelector:
    type: kwok
  tolerations:
    - key: kwok.x-k8s.io/node
      operator: Equal
      value: fake
      effect: NoSchedule
  containers:
    - name: work
      image: registry.k8s.io/pause:3.9
      resources:
        requests:
          cpu: "{{CPU}}"
          memory: "{{MEMORY}}"
