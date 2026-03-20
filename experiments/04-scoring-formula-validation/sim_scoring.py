#!/usr/bin/env python3
"""
Scheduling formula validation simulation.

Compares the OLD twin-based scoring formula (headroom/coolingStress/psuStress
all derived from cap-based power) against a NEW formula using measured node
power, marginal estimation, and power-trend backfilling.

Usage:
    python sim_scoring.py [--steps 1000] [--seed 42] [--outdir ./results]
"""

import argparse
import math
import os
from dataclasses import dataclass, field
from typing import Optional

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

# ---------------------------------------------------------------------------
# Constants (from pkg/scheduler/powerest/types.go DefaultCoefficients)
# ---------------------------------------------------------------------------

CPU_UTIL_COEFF = 0.7
GPU_UTIL_COEFF_STD = 0.65
GPU_UTIL_COEFF_PERF = 0.85
IDLE_GPU_WATTS_PER_DEVICE = 60.0
IDLE_GPU_PENALTY_CAP = 300.0
REFERENCE_NODE_POWER_W = 4000.0
REFERENCE_RACK_CAPACITY_W = 50000.0

# Trend window: how many past readings to use for slope estimation
TREND_WINDOW = 5

# ---------------------------------------------------------------------------
# Data model
# ---------------------------------------------------------------------------


@dataclass
class NodeSpec:
    name: str
    cpu_cores: int
    cpu_max_watts: float  # total CPU max (e.g. 2 sockets x 350W)
    has_gpu: bool
    gpu_count: int
    gpu_max_watts_per_gpu: float
    profile: str = "performance"  # "performance" or "eco"

    @property
    def peak_power_w(self) -> float:
        return self.cpu_max_watts + self.gpu_count * self.gpu_max_watts_per_gpu


@dataclass
class Pod:
    cpu_cores: float
    gpu_count: int
    workload_class: str  # "standard" or "performance"
    remaining_steps: int
    cpu_utilization: float = 0.7  # fraction of requested cores actually used
    gpu_utilization: float = 0.7


@dataclass
class NodeState:
    spec: NodeSpec
    pods: list = field(default_factory=list)
    power_history: list = field(default_factory=list)
    measured_power_w: float = 0.0

    @property
    def allocated_cpu(self) -> float:
        return sum(p.cpu_cores for p in self.pods)

    @property
    def allocated_gpu(self) -> int:
        return sum(p.gpu_count for p in self.pods)


# ---------------------------------------------------------------------------
# Power model
# ---------------------------------------------------------------------------


def compute_node_power(node: NodeState) -> float:
    """Compute node power from actual pod utilizations."""
    spec = node.spec
    # CPU: sum of per-pod contributions
    cpu_power = 0.0
    for p in node.pods:
        core_share = p.cpu_cores / max(1, spec.cpu_cores)
        cpu_power += spec.cpu_max_watts * CPU_UTIL_COEFF * core_share * p.cpu_utilization

    # Add idle CPU baseline (10% of max when any pod runs, 5% otherwise)
    if node.pods:
        cpu_power += spec.cpu_max_watts * 0.10
    else:
        cpu_power += spec.cpu_max_watts * 0.05

    # GPU
    gpu_power = 0.0
    if spec.has_gpu:
        active_gpus = 0
        for p in node.pods:
            if p.gpu_count > 0:
                coeff = GPU_UTIL_COEFF_PERF if p.workload_class == "performance" else GPU_UTIL_COEFF_STD
                gpu_power += p.gpu_count * spec.gpu_max_watts_per_gpu * coeff * p.gpu_utilization
                active_gpus += p.gpu_count
        idle_gpus = max(0, spec.gpu_count - active_gpus)
        gpu_power += idle_gpus * IDLE_GPU_WATTS_PER_DEVICE

    return cpu_power + gpu_power


def estimate_marginal_delta(spec: NodeSpec, pod: Pod) -> float:
    """Estimate incremental watts if pod is placed on this node."""
    core_share = min(1.0, pod.cpu_cores / max(1, spec.cpu_cores))
    cpu_delta = spec.cpu_max_watts * CPU_UTIL_COEFF * core_share

    gpu_delta = 0.0
    if pod.gpu_count > 0 and spec.has_gpu:
        coeff = GPU_UTIL_COEFF_PERF if pod.workload_class == "performance" else GPU_UTIL_COEFF_STD
        gpu_delta = pod.gpu_count * spec.gpu_max_watts_per_gpu * coeff

    return cpu_delta + gpu_delta


# ---------------------------------------------------------------------------
# Trend computation
# ---------------------------------------------------------------------------


def compute_power_trend(history: list) -> float:
    """
    Compute the slope of recent power readings (watts per step).
    Positive = rising, negative = falling.
    """
    if len(history) < 2:
        return 0.0
    window = history[-TREND_WINDOW:]
    n = len(window)
    xs = np.arange(n, dtype=float)
    ys = np.array(window, dtype=float)
    # Simple linear regression slope
    x_mean = xs.mean()
    y_mean = ys.mean()
    denom = ((xs - x_mean) ** 2).sum()
    if denom < 1e-9:
        return 0.0
    return float(((xs - x_mean) * (ys - y_mean)).sum() / denom)


# ---------------------------------------------------------------------------
# Scoring functions
# ---------------------------------------------------------------------------


def can_fit(node: NodeState, pod: Pod) -> bool:
    """Check if node has capacity for the pod."""
    if node.allocated_cpu + pod.cpu_cores > node.spec.cpu_cores:
        return False
    if pod.gpu_count > 0:
        if not node.spec.has_gpu:
            return False
        if node.allocated_gpu + pod.gpu_count > node.spec.gpu_count:
            return False
    return True


def score_old(node: NodeState, pod: Pod, cluster_total_power: float) -> float:
    """
    OLD formula from cmd/scheduler/main.go:552.
    headroom, coolingStress, psuStress all derived from cap-based estimate.
    """
    spec = node.spec
    cpu_cap_pct = 100.0
    gpu_cap_pct = 100.0

    # computeCoolingStress (twin.go:172-180): cap-based node power estimate
    node_power_w = spec.cpu_max_watts * (cpu_cap_pct / 100.0)
    if spec.has_gpu:
        node_power_w += spec.gpu_max_watts_per_gpu * spec.gpu_count * (gpu_cap_pct / 100.0)
    cooling_stress = min(100.0, (node_power_w / REFERENCE_NODE_POWER_W) * 80.0)

    # computePowerHeadroom (twin.go:159-168)
    cap_factor = (cpu_cap_pct + gpu_cap_pct) / 200.0
    cooling_factor = 1.0 - cooling_stress / 100.0
    headroom = max(0.0, min(100.0, cap_factor * cooling_factor * 100.0))

    # computePSUStress (twin.go:187-199)
    psu_stress = min(100.0, max(0.0, (cluster_total_power / REFERENCE_RACK_CAPACITY_W) * 100.0))

    base = headroom * 0.4 + (100 - cooling_stress) * 0.3 + (100 - psu_stress) * 0.3

    # Marginal penalty (model.go:98-112)
    delta_w = estimate_marginal_delta(spec, pod)
    marginal_penalty = min(20.0, max(0.0, delta_w / 20.0))
    cooling_delta = min(100.0, max(0.0, (delta_w / REFERENCE_NODE_POWER_W) * 80.0))
    cooling_delta_penalty = min(20.0, max(0.0, cooling_delta * 0.6))
    psu_delta = min(100.0, max(0.0, (delta_w / REFERENCE_RACK_CAPACITY_W) * 100.0))
    psu_delta_penalty = min(15.0, max(0.0, psu_delta * 0.4))

    # Idle GPU waste
    idle_gpu_penalty = 0.0
    if pod.gpu_count == 0 and spec.has_gpu:
        waste = min(spec.gpu_count * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
        idle_gpu_penalty = min(20.0, max(0.0, waste / 10.0))

    score = base - marginal_penalty - cooling_delta_penalty - psu_delta_penalty - idle_gpu_penalty
    return max(0.0, min(100.0, score))


def score_new(node: NodeState, pod: Pod) -> float:
    """
    NEW formula: measured power headroom + marginal penalty + trend signal.
    """
    spec = node.spec

    # Power headroom from measured power
    power_headroom = spec.peak_power_w - node.measured_power_w
    headroom_pct = max(0.0, (power_headroom / spec.peak_power_w) * 100.0)

    # Concave mapping: sqrt gives diminishing returns for extra headroom
    score = 10.0 * math.sqrt(headroom_pct)  # 0-100

    # Marginal power penalty (same logic as Go)
    delta_w = estimate_marginal_delta(spec, pod)
    marginal_penalty = min(20.0, max(0.0, delta_w / 20.0))

    # Idle GPU waste (same as old formula)
    idle_gpu_penalty = 0.0
    if pod.gpu_count == 0 and spec.has_gpu:
        waste = min(spec.gpu_count * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
        idle_gpu_penalty = min(20.0, max(0.0, waste / 10.0))

    # Trend: rising -> penalty, falling -> bonus (backfill)
    trend = compute_power_trend(node.power_history)
    # Normalize: trend is watts/step. A 100W/step rise is significant.
    # Penalty range: [-8, +15] — stronger penalty for spikes than bonus for dips
    trend_penalty = max(-8.0, min(15.0, trend / 40.0))

    score = score - marginal_penalty - idle_gpu_penalty - trend_penalty
    return max(0.0, min(100.0, score))


def score_stable(node: NodeState, pod: Pod, cluster_nodes: list) -> float:
    """
    STABLE formula: explicitly targets equal power utilization across nodes
    of the same hardware class, and penalizes temporal instability.

    Three components:
    1. Balance: how far is this node from the average utilization of its class?
       Nodes below average get a bonus, nodes above get a penalty.
    2. Marginal: what will this pod add? Prefer nodes where the post-placement
       power stays closest to the class average.
    3. Trend: penalize rising nodes (spike), reward falling nodes (backfill dip).
    """
    spec = node.spec

    # --- 1. Balance score: target equal utilization across same-class nodes ---
    # Compute average power utilization for nodes of the same type (CPU vs GPU)
    same_class = [n for n in cluster_nodes if n.spec.has_gpu == spec.has_gpu]
    if same_class:
        class_avg_util = np.mean([n.measured_power_w / n.spec.peak_power_w for n in same_class])
    else:
        class_avg_util = 0.0

    node_util = node.measured_power_w / spec.peak_power_w

    # How much room does this node have relative to the class average?
    # Negative deviation = node is below average = good target for new work
    deviation = node_util - class_avg_util  # -1 to +1

    # Score: 50 at average, 100 if maximally below average, 0 if maximally above
    balance_score = 50.0 - deviation * 100.0
    balance_score = max(0.0, min(100.0, balance_score))

    # --- 2. Post-placement projection: will placing here keep us balanced? ---
    delta_w = estimate_marginal_delta(spec, pod)
    projected_util = (node.measured_power_w + delta_w) / spec.peak_power_w
    projected_deviation = abs(projected_util - class_avg_util)

    # Penalty for how far from average we'd be after placement (0-20)
    projection_penalty = min(20.0, projected_deviation * 40.0)

    # --- 3. Trend: rising -> penalty, falling -> bonus (backfill dips) ---
    trend = compute_power_trend(node.power_history)
    # Stronger trend weight than NEW formula — stability is the explicit goal
    # Penalty range: [-12, +20]
    trend_penalty = max(-12.0, min(20.0, trend / 25.0))

    # --- 4. Idle GPU waste (keep CPU pods off GPU nodes) ---
    idle_gpu_penalty = 0.0
    if pod.gpu_count == 0 and spec.has_gpu:
        waste = min(spec.gpu_count * IDLE_GPU_WATTS_PER_DEVICE, IDLE_GPU_PENALTY_CAP)
        idle_gpu_penalty = min(20.0, max(0.0, waste / 10.0))

    score = balance_score - projection_penalty - trend_penalty - idle_gpu_penalty
    return max(0.0, min(100.0, score))


# ---------------------------------------------------------------------------
# Workload generators
# ---------------------------------------------------------------------------


def _make_pod(rng: np.random.Generator, gpu_prob: float = 0.1) -> Pod:
    """Generate a random pod."""
    is_gpu = rng.random() < gpu_prob
    if is_gpu:
        return Pod(
            cpu_cores=rng.choice([2, 4, 8]),
            gpu_count=rng.choice([1, 2, 4]),
            workload_class=rng.choice(["performance", "standard"], p=[0.6, 0.4]),
            remaining_steps=int(rng.integers(20, 80)),
            cpu_utilization=float(rng.uniform(0.2, 0.5)),
            gpu_utilization=float(rng.uniform(0.4, 0.95)),
        )
    else:
        return Pod(
            cpu_cores=rng.choice([1, 2, 4, 8, 16]),
            gpu_count=0,
            workload_class=rng.choice(["standard", "performance"], p=[0.7, 0.3]),
            remaining_steps=int(rng.integers(5, 40)),
            cpu_utilization=float(rng.uniform(0.3, 0.9)),
            gpu_utilization=0.0,
        )


def gen_steady(steps: int, rng: np.random.Generator) -> dict:
    """Uniform 2-4 pods per step."""
    trace = {}
    for t in range(steps):
        count = int(rng.integers(2, 5))
        trace[t] = [_make_pod(rng, gpu_prob=0.1) for _ in range(count)]
    return trace


def gen_burst(steps: int, rng: np.random.Generator) -> dict:
    """Background of 1 pod/step with 3 large bursts."""
    trace = {}
    burst_times = [int(steps * 0.2), int(steps * 0.5), int(steps * 0.8)]
    for t in range(steps):
        count = 1
        if t in burst_times:
            count = int(rng.integers(25, 40))
        trace[t] = [_make_pod(rng, gpu_prob=0.15) for _ in range(count)]
    return trace


def gen_wave(steps: int, rng: np.random.Generator) -> dict:
    """Sinusoidal arrival rate (day/night cycle)."""
    trace = {}
    for t in range(steps):
        rate = 3 + 2.5 * math.sin(2 * math.pi * t / 200)
        count = max(0, int(round(rate + rng.normal(0, 0.5))))
        trace[t] = [_make_pod(rng, gpu_prob=0.1) for _ in range(count)]
    return trace


def gen_mixed(steps: int, rng: np.random.Generator) -> dict:
    """Mixed workloads: small CPU, medium CPU, large GPU jobs."""
    trace = {}
    for t in range(steps):
        count = int(rng.integers(2, 5))
        pods = []
        for _ in range(count):
            r = rng.random()
            if r < 0.5:  # small CPU
                pods.append(Pod(cpu_cores=2, gpu_count=0, workload_class="standard",
                                remaining_steps=int(rng.integers(3, 10)),
                                cpu_utilization=float(rng.uniform(0.4, 0.8))))
            elif r < 0.8:  # medium CPU
                pods.append(Pod(cpu_cores=8, gpu_count=0, workload_class="standard",
                                remaining_steps=int(rng.integers(10, 30)),
                                cpu_utilization=float(rng.uniform(0.5, 0.9))))
            else:  # large GPU
                pods.append(Pod(cpu_cores=4, gpu_count=2,
                                workload_class=rng.choice(["performance", "standard"]),
                                remaining_steps=int(rng.integers(30, 80)),
                                cpu_utilization=float(rng.uniform(0.2, 0.5)),
                                gpu_utilization=float(rng.uniform(0.5, 0.95))))
        trace[t] = pods
    return trace


SCENARIOS = {
    "steady": gen_steady,
    "burst": gen_burst,
    "wave": gen_wave,
    "mixed": gen_mixed,
}

# ---------------------------------------------------------------------------
# Cluster setup
# ---------------------------------------------------------------------------


def init_cluster() -> list:
    """Create a 16-node cluster: 10 CPU-only + 6 GPU."""
    nodes = []
    for i in range(10):
        spec = NodeSpec(
            name=f"cpu-{i:02d}", cpu_cores=96,
            cpu_max_watts=700.0,  # 2x350W sockets
            has_gpu=False, gpu_count=0, gpu_max_watts_per_gpu=0.0,
        )
        nodes.append(NodeState(spec=spec))
    for i in range(6):
        spec = NodeSpec(
            name=f"gpu-{i:02d}", cpu_cores=96,
            cpu_max_watts=700.0,
            has_gpu=True, gpu_count=8, gpu_max_watts_per_gpu=350.0,
        )
        nodes.append(NodeState(spec=spec))
    return nodes


# ---------------------------------------------------------------------------
# Simulation
# ---------------------------------------------------------------------------


def run_simulation(
    trace: dict,
    score_fn,
    steps: int,
    seed: int,
) -> pd.DataFrame:
    """Run the scheduling simulation and return a timeseries DataFrame."""
    cluster = init_cluster()
    records = []

    for t in range(steps):
        # 1. Tick: decrement remaining steps, remove completed pods
        for node in cluster:
            for p in node.pods:
                p.remaining_steps -= 1
            node.pods = [p for p in node.pods if p.remaining_steps > 0]

        # 2. Update measured power (before scheduling, so scores see current state)
        for node in cluster:
            node.measured_power_w = compute_node_power(node)
            node.power_history.append(node.measured_power_w)

        # 3. Compute cluster total power (needed for old formula)
        cluster_total_power = sum(n.measured_power_w for n in cluster)

        # 4. Schedule arriving pods
        new_pods = trace.get(t, [])
        scheduled = 0
        dropped = 0
        for pod in new_pods:
            # Score all nodes that can fit the pod
            candidates = []
            for node in cluster:
                if can_fit(node, pod):
                    if score_fn == score_old:
                        s = score_fn(node, pod, cluster_total_power)
                    elif score_fn == score_stable:
                        s = score_fn(node, pod, cluster)
                    else:
                        s = score_fn(node, pod)
                    candidates.append((s, node))
            if candidates:
                # Pick highest score (break ties randomly-ish via order)
                candidates.sort(key=lambda x: x[0], reverse=True)
                best_node = candidates[0][1]
                best_node.pods.append(pod)
                # Update power immediately so next pod in this step sees the change
                best_node.measured_power_w = compute_node_power(best_node)
                best_node.power_history[-1] = best_node.measured_power_w
                scheduled += 1
            else:
                dropped += 1

        # 5. Record timeseries
        cluster_total_power = sum(n.measured_power_w for n in cluster)
        node_powers = {n.spec.name: n.measured_power_w for n in cluster}
        node_pods = {n.spec.name: len(n.pods) for n in cluster}
        max_node_power = max(n.measured_power_w for n in cluster)

        rec = {
            "step": t,
            "total_power_w": cluster_total_power,
            "max_node_power_w": max_node_power,
            "total_pods": sum(len(n.pods) for n in cluster),
            "scheduled": scheduled,
            "dropped": dropped,
        }
        for name, pw in node_powers.items():
            rec[f"power_{name}"] = pw
        for name, pc in node_pods.items():
            rec[f"pods_{name}"] = pc
        records.append(rec)

    return pd.DataFrame(records)


# ---------------------------------------------------------------------------
# Metrics
# ---------------------------------------------------------------------------


def compute_metrics(df: pd.DataFrame, cluster_size: int = 16) -> dict:
    """Compute summary metrics from simulation timeseries."""
    total = df["total_power_w"]
    diffs = np.diff(total.values)

    # Per-node time-averaged power
    power_cols = [c for c in df.columns if c.startswith("power_")]
    node_avg_powers = df[power_cols].mean().values

    # Gini coefficient
    sorted_p = np.sort(node_avg_powers)
    n = len(sorted_p)
    index = np.arange(1, n + 1)
    gini = (2 * (index * sorted_p).sum() / (n * sorted_p.sum()) - (n + 1) / n) if sorted_p.sum() > 0 else 0

    # Per-node temporal stability: avg of each node's power std over time
    node_power_stds = df[power_cols].std().values  # std per node over time
    avg_node_power_std = node_power_stds.mean() / 1000  # kW

    # Cross-node balance at each timestep: avg std across nodes per step
    per_step_node_std = df[power_cols].std(axis=1).values  # std across nodes at each step
    avg_cross_node_std = per_step_node_std.mean() / 1000  # kW

    return {
        "total_power_std_kw": total.std() / 1000,
        "peak_to_avg_ratio": total.max() / max(1, total.mean()),
        "max_ramp_up_kw": diffs.max() / 1000 if len(diffs) > 0 else 0,
        "max_ramp_down_kw": abs(diffs.min()) / 1000 if len(diffs) > 0 else 0,
        "power_derivative_std_kw": diffs.std() / 1000 if len(diffs) > 0 else 0,
        "gini_coefficient": gini,
        "avg_node_temporal_std_kw": avg_node_power_std,
        "avg_cross_node_std_kw": avg_cross_node_std,
        "total_energy_kwh": total.sum() / 1000 / 3600,  # assuming 1 step = 1 second
        "avg_total_power_kw": total.mean() / 1000,
        "peak_total_power_kw": total.max() / 1000,
        "avg_dropped_pods": df["dropped"].mean(),
    }


# ---------------------------------------------------------------------------
# Plotting
# ---------------------------------------------------------------------------


def plot_timeseries_comparison(
    results: dict,
    scenario: str,
    outdir: str,
):
    """Plot total and max-node power timeseries for old vs new."""
    fig, axes = plt.subplots(2, 1, figsize=(14, 8), sharex=True)
    fig.suptitle(f"Scenario: {scenario}", fontsize=14, fontweight="bold")

    colors = {"OLD": "#d62728", "NEW": "#1f77b4", "STABLE": "#2ca02c"}

    for formula_name, df in results.items():
        c = colors[formula_name]
        axes[0].plot(df["step"], df["total_power_w"] / 1000, label=formula_name,
                     color=c, alpha=0.8, linewidth=0.8)
        axes[1].plot(df["step"], df["max_node_power_w"] / 1000, label=formula_name,
                     color=c, alpha=0.8, linewidth=0.8)

    axes[0].set_ylabel("Total Cluster Power (kW)")
    axes[0].legend()
    axes[0].grid(alpha=0.3)
    axes[1].set_ylabel("Max Single-Node Power (kW)")
    axes[1].set_xlabel("Timestep")
    axes[1].legend()
    axes[1].grid(alpha=0.3)

    plt.tight_layout()
    path = os.path.join(outdir, f"timeseries_{scenario}.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"  Saved {path}")


def plot_node_power_heatmap(
    results: dict,
    scenario: str,
    outdir: str,
):
    """Side-by-side heatmaps of per-node power over time."""
    n_formulas = len(results)
    fig, axes = plt.subplots(1, n_formulas, figsize=(7 * n_formulas, 6))
    fig.suptitle(f"Per-Node Power Heatmap: {scenario}", fontsize=14, fontweight="bold")

    for idx, (formula_name, df) in enumerate(results.items()):
        power_cols = sorted([c for c in df.columns if c.startswith("power_")])
        matrix = df[power_cols].values.T / 1000  # nodes x steps, in kW
        im = axes[idx].imshow(matrix, aspect="auto", cmap="hot", interpolation="nearest")
        axes[idx].set_title(formula_name)
        axes[idx].set_ylabel("Node")
        axes[idx].set_xlabel("Timestep")
        axes[idx].set_yticks(range(len(power_cols)))
        axes[idx].set_yticklabels([c.replace("power_", "") for c in power_cols], fontsize=6)
        plt.colorbar(im, ax=axes[idx], label="kW")

    plt.tight_layout()
    path = os.path.join(outdir, f"heatmap_{scenario}.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"  Saved {path}")


def plot_power_derivative(
    results: dict,
    scenario: str,
    outdir: str,
):
    """Plot the power derivative (rate of change) for old vs new."""
    fig, ax = plt.subplots(1, 1, figsize=(14, 4))
    fig.suptitle(f"Power Derivative (dP/dt): {scenario}", fontsize=14, fontweight="bold")

    colors = {"OLD": "#d62728", "NEW": "#1f77b4", "STABLE": "#2ca02c"}
    for formula_name, df in results.items():
        diffs = np.diff(df["total_power_w"].values) / 1000
        ax.plot(df["step"].values[1:], diffs, label=formula_name,
                color=colors[formula_name], alpha=0.6, linewidth=0.6)

    ax.axhline(0, color="gray", linewidth=0.5)
    ax.set_ylabel("dP/dt (kW/step)")
    ax.set_xlabel("Timestep")
    ax.legend()
    ax.grid(alpha=0.3)

    plt.tight_layout()
    path = os.path.join(outdir, f"derivative_{scenario}.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"  Saved {path}")


def plot_summary_bars(all_metrics: dict, outdir: str):
    """Bar chart comparing OLD vs NEW vs STABLE across all scenarios and metrics."""
    metrics_to_plot = [
        ("total_power_std_kw", "Cluster Power\nStd Dev (kW)"),
        ("peak_to_avg_ratio", "Peak/Avg Ratio"),
        ("avg_node_temporal_std_kw", "Avg Node\nTemporal Std (kW)"),
        ("avg_cross_node_std_kw", "Cross-Node\nImbalance (kW)"),
        ("power_derivative_std_kw", "Derivative\nStd (kW)"),
        ("gini_coefficient", "Gini\n(node balance)"),
    ]

    formula_names = ["OLD", "NEW", "STABLE"]
    colors = {"OLD": "#d62728", "NEW": "#1f77b4", "STABLE": "#2ca02c"}
    scenarios = list(all_metrics.keys())
    n_metrics = len(metrics_to_plot)
    fig, axes = plt.subplots(1, n_metrics, figsize=(3.5 * n_metrics, 5))
    fig.suptitle("Summary: OLD vs NEW vs STABLE Scoring Formula", fontsize=14, fontweight="bold")

    x = np.arange(len(scenarios))
    n_formulas = len(formula_names)
    width = 0.8 / n_formulas

    for i, (metric_key, metric_label) in enumerate(metrics_to_plot):
        ax = axes[i]
        for j, fname in enumerate(formula_names):
            vals = [all_metrics[s][fname][metric_key] for s in scenarios]
            offset = (j - (n_formulas - 1) / 2) * width
            ax.bar(x + offset, vals, width, label=fname, color=colors[fname], alpha=0.8)
        ax.set_title(metric_label, fontsize=9)
        ax.set_xticks(x)
        ax.set_xticklabels(scenarios, rotation=45, ha="right", fontsize=8)
        if i == 0:
            ax.legend(fontsize=7)
        ax.grid(axis="y", alpha=0.3)

    plt.tight_layout()
    path = os.path.join(outdir, "summary_bars.png")
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"  Saved {path}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    parser = argparse.ArgumentParser(description="Scheduling formula validation")
    parser.add_argument("--steps", type=int, default=1000, help="Simulation steps")
    parser.add_argument("--seed", type=int, default=42, help="RNG seed")
    parser.add_argument("--outdir", type=str, default="./results", help="Output directory")
    args = parser.parse_args()

    os.makedirs(args.outdir, exist_ok=True)

    formulas = {"OLD": score_old, "NEW": score_new, "STABLE": score_stable}
    all_metrics = {}
    all_results = {}

    for scenario_name, gen_fn in SCENARIOS.items():
        print(f"\n{'='*60}")
        print(f"Scenario: {scenario_name}")
        print(f"{'='*60}")

        scenario_results = {}
        scenario_metrics = {}

        for formula_name, score_fn in formulas.items():
            # Use same seed for both formulas so they get identical workload
            rng = np.random.default_rng(args.seed)
            trace = gen_fn(args.steps, rng)

            print(f"  Running {formula_name}...")
            df = run_simulation(trace, score_fn, args.steps, args.seed)
            metrics = compute_metrics(df)

            scenario_results[formula_name] = df
            scenario_metrics[formula_name] = metrics

            print(f"    Avg power:       {metrics['avg_total_power_kw']:.1f} kW")
            print(f"    Power std:       {metrics['total_power_std_kw']:.2f} kW")
            print(f"    Peak/avg:        {metrics['peak_to_avg_ratio']:.3f}")
            print(f"    Max ramp:        {metrics['max_ramp_up_kw']:.2f} kW")
            print(f"    Deriv std:       {metrics['power_derivative_std_kw']:.3f} kW")
            print(f"    Gini:            {metrics['gini_coefficient']:.3f}")
            print(f"    Node temporal:   {metrics['avg_node_temporal_std_kw']:.3f} kW")
            print(f"    Cross-node std:  {metrics['avg_cross_node_std_kw']:.3f} kW")
            print(f"    Dropped:         {metrics['avg_dropped_pods']:.2f} pods/step avg")

        all_metrics[scenario_name] = scenario_metrics
        all_results[scenario_name] = scenario_results

        # Per-scenario plots
        plot_timeseries_comparison(scenario_results, scenario_name, args.outdir)
        plot_node_power_heatmap(scenario_results, scenario_name, args.outdir)
        plot_power_derivative(scenario_results, scenario_name, args.outdir)

    # Summary plots
    plot_summary_bars(all_metrics, args.outdir)

    # Summary table
    print(f"\n{'='*60}")
    print("SUMMARY TABLE")
    print(f"{'='*60}")
    rows = []
    for scenario in SCENARIOS:
        for formula in ["OLD", "NEW", "STABLE"]:
            m = all_metrics[scenario][formula]
            rows.append({
                "scenario": scenario,
                "formula": formula,
                **m,
            })
    summary_df = pd.DataFrame(rows)
    print(summary_df.to_string(index=False, float_format="%.3f"))
    summary_path = os.path.join(args.outdir, "summary.csv")
    summary_df.to_csv(summary_path, index=False)
    print(f"\nSaved {summary_path}")

    # Improvement table
    compare_keys = [
        "total_power_std_kw", "peak_to_avg_ratio", "max_ramp_up_kw",
        "power_derivative_std_kw", "gini_coefficient",
        "avg_node_temporal_std_kw", "avg_cross_node_std_kw",
    ]
    for compare_name in ["NEW", "STABLE"]:
        print(f"\n{'='*60}")
        print(f"IMPROVEMENT ({compare_name} vs OLD, negative = better for {compare_name})")
        print(f"{'='*60}")
        for scenario in SCENARIOS:
            old_m = all_metrics[scenario]["OLD"]
            cmp_m = all_metrics[scenario][compare_name]
            print(f"\n  {scenario}:")
            for key in compare_keys:
                old_v = old_m[key]
                cmp_v = cmp_m[key]
                if old_v > 0:
                    pct = (cmp_v - old_v) / old_v * 100
                    print(f"    {key:30s}: {old_v:8.3f} -> {cmp_v:8.3f}  ({pct:+.1f}%)")
                else:
                    print(f"    {key:30s}: {old_v:8.3f} -> {cmp_v:8.3f}")


if __name__ == "__main__":
    main()
