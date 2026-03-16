apiVersion: v1
kind: Node
metadata:
  name: kwok-node-{{INDEX}}
  annotations:
    kwok.x-k8s.io/node: fake
  labels:
    type: kwok
    joulie.io/managed: "true"
    feature.node.kubernetes.io/cpu-model.vendor_id: {{CPU_VENDOR_ID}}
    feature.node.kubernetes.io/cpu-vendor: {{CPU_VENDOR}}
spec:
  taints:
    - key: kwok.x-k8s.io/node
      value: fake
      effect: NoSchedule
status:
  allocatable:
    cpu: "{{CPU}}"
    memory: "{{MEMORY}}"
    pods: "{{PODS}}"
  capacity:
    cpu: "{{CPU}}"
    memory: "{{MEMORY}}"
    pods: "{{PODS}}"
