#!/usr/bin/env python3
"""
Generate test workload pod manifests with joulie.io/workload-class annotations.

Reads benchmark.yaml for workload ratios and job count.
Outputs YAML to stdout for piping to kubectl apply.

Usage:
  python3 scripts/25_generate_workloads.py [--config configs/benchmark.yaml] [--jobs 50]
"""
import argparse
import math
import random
import sys
from pathlib import Path

import yaml


def generate_pods(jobs, perf_ratio, standard_ratio, best_effort_ratio, gpu_ratio, seed=42):
    """Generate pod manifests with workload-class annotations."""
    rng = random.Random(seed)

    n_perf = int(math.ceil(jobs * perf_ratio))
    n_best = int(math.ceil(jobs * best_effort_ratio))
    n_standard = jobs - n_perf - n_best

    classes = (
        ["performance"] * n_perf
        + ["standard"] * n_standard
        + ["best-effort"] * n_best
    )
    rng.shuffle(classes)

    n_gpu = int(math.ceil(jobs * gpu_ratio))
    gpu_flags = [True] * n_gpu + [False] * (jobs - n_gpu)
    rng.shuffle(gpu_flags)

    pods = []
    for i, (wclass, needs_gpu) in enumerate(zip(classes, gpu_flags)):
        name = f"bench-{wclass}-{'gpu' if needs_gpu else 'cpu'}-{i:04d}"
        cpu_req = rng.choice(["500m", "1", "2", "4"])
        mem_req = rng.choice(["256Mi", "512Mi", "1Gi", "2Gi"])

        resources = {"requests": {"cpu": cpu_req, "memory": mem_req}}
        if needs_gpu:
            resources["requests"]["nvidia.com/gpu"] = "1"
            resources["limits"] = {"nvidia.com/gpu": "1"}

        pod = {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "name": name,
                "namespace": "default",
                "labels": {
                    "app.kubernetes.io/part-of": "joulie-exp03-benchmark",
                    "joulie.io/workload-class": wclass,
                },
                "annotations": {
                    "joulie.io/workload-class": wclass,
                },
            },
            "spec": {
                "restartPolicy": "Never",
                "containers": [
                    {
                        "name": "workload",
                        "image": "busybox:1.36",
                        "command": ["sh", "-c", f"echo 'workload {name} class={wclass}'; sleep {rng.randint(10, 120)}"],
                        "resources": resources,
                    }
                ],
            },
        }
        pods.append(pod)

    return pods


def main():
    parser = argparse.ArgumentParser(description="Generate benchmark workload pods")
    parser.add_argument("--config", default="", help="Path to benchmark.yaml")
    parser.add_argument("--jobs", type=int, default=0, help="Override job count")
    parser.add_argument("--seed", type=int, default=42, help="Random seed")
    args = parser.parse_args()

    # Defaults
    jobs = 50
    perf_ratio = 0.30
    standard_ratio = 0.50
    best_effort_ratio = 0.20
    gpu_ratio = 0.40

    if args.config:
        cfg_path = Path(args.config)
        if cfg_path.exists():
            with open(cfg_path) as f:
                cfg = yaml.safe_load(f)
            run_cfg = cfg.get("run", {})
            wl_cfg = cfg.get("workload", {})
            jobs = run_cfg.get("jobs", jobs)
            perf_ratio = wl_cfg.get("performance_ratio", perf_ratio)
            standard_ratio = wl_cfg.get("standard_ratio", standard_ratio)
            best_effort_ratio = wl_cfg.get("best_effort_ratio", best_effort_ratio)
            gpu_ratio = wl_cfg.get("gpu_ratio", gpu_ratio)

    if args.jobs > 0:
        jobs = args.jobs

    pods = generate_pods(jobs, perf_ratio, standard_ratio, best_effort_ratio, gpu_ratio, seed=args.seed)

    print("---")
    print(yaml.dump_all(pods, default_flow_style=False, sort_keys=False))


if __name__ == "__main__":
    main()
