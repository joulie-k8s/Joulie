#!/usr/bin/env python3
"""
Generate KWOK Node manifests from cluster-nodes.yaml.
Outputs YAML to stdout for piping to kubectl apply.
"""
import sys
import yaml

def node_manifest(n):
    gpu_count = n.get("gpuCount", 0)
    cpu_cores = n.get("cpuCores", 1)
    memory = n.get("memory", "128Gi")
    gpu_model = n.get("gpuModel", "")
    cpu_model = n.get("cpuModel", "")

    capacity = {
        "cpu": str(cpu_cores),
        "memory": memory,
        "pods": "110",
    }
    if gpu_count > 0:
        capacity["nvidia.com/gpu"] = str(gpu_count)

    labels = {
        "beta.kubernetes.io/os": "linux",
        "kubernetes.io/arch": "amd64",
        "kubernetes.io/hostname": n["name"],
        "node-role.kubernetes.io/worker": "",
        "joulie.io/managed": "true",
        "joulie.io/power-profile": "performance",
    }
    if gpu_count > 0:
        labels["joulie.io/gpu"] = "true"
        labels["joulie.io/gpu-model"] = gpu_model.replace(" ", "-").lower()
    if cpu_model:
        labels["joulie.io/cpu-model"] = cpu_model.replace(" ", "-").lower()

    return {
        "apiVersion": "v1",
        "kind": "Node",
        "metadata": {
            "name": n["name"],
            "labels": labels,
            "annotations": {
                "kwok.x-k8s.io/node": "fake",
                "joulie.io/cpu-max-power-w": str(n.get("cpuMaxPowerW", 0)),
                "joulie.io/gpu-max-power-w": str(n.get("gpuCount", 0) * n.get("gpuMaxPowerPerUnit", 0)),
            },
        },
        "spec": {
            "taints": [],
        },
        "status": {
            "capacity": capacity,
            "allocatable": capacity,
            "conditions": [
                {"type": "Ready", "status": "True",
                 "reason": "KwokReady", "message": "kwok node ready"},
            ],
            "nodeInfo": {
                "architecture": "amd64",
                "operatingSystem": "linux",
                "kernelVersion": "5.15.0-kwok",
                "osImage": "Ubuntu 22.04.3 LTS",
                "containerRuntimeVersion": "containerd://1.7.0",
                "kubeletVersion": "v1.31.0",
                "kubeProxyVersion": "v1.31.0",
            },
        },
    }

def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "configs/cluster-nodes.yaml"
    with open(path) as f:
        data = yaml.safe_load(f)

    docs = [node_manifest(n) for n in data["nodes"]]
    print("---")
    print(yaml.dump_all(docs, default_flow_style=False, sort_keys=False))

if __name__ == "__main__":
    main()
