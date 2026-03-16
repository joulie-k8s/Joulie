#!/usr/bin/env python3
import os
import pathlib

import matplotlib.pyplot as plt
import pandas as pd

ROOT = pathlib.Path("experiments/01-kwok-benchmark")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results"))).resolve()
PLOTS = RESULTS / "plots"
BASELINE_ORDER = ["A", "B", "C"]
BASELINE_COLORS = {"A": "#4c78a8", "B": "#f58518", "C": "#54a24b"}


def baseline_sort_key(values):
    order_map = {b: i for i, b in enumerate(BASELINE_ORDER)}
    return [order_map.get(v, 999) for v in values]


def ensure_numeric(df: pd.DataFrame, cols):
    for c in cols:
        if c in df.columns:
            df[c] = pd.to_numeric(df[c], errors="coerce")


def filter_completed_runs(df: pd.DataFrame) -> pd.DataFrame:
    if "run_completed" not in df.columns:
        return df.copy()
    vals = (
        df["run_completed"]
        .astype(str)
        .str.strip()
        .str.lower()
        .isin({"true", "1", "yes"})
    )
    return df[vals].copy()


def pareto_frontier(df: pd.DataFrame, x_col: str, y_col: str):
    # Minimize x (energy), maximize y (throughput).
    pts = df.dropna(subset=[x_col, y_col]).sort_values([x_col, y_col], ascending=[True, False])
    out = []
    best_y = None
    for _, row in pts.iterrows():
        y = row[y_col]
        if best_y is None or y > best_y:
            out.append(row)
            best_y = y
    if not out:
        return pd.DataFrame(columns=df.columns)
    return pd.DataFrame(out)


def plot_runtime_distribution(df: pd.DataFrame):
    metric = "wall_seconds"
    if metric not in df.columns:
        return
    d = df.dropna(subset=[metric, "baseline"]).copy()
    if d.empty:
        return

    baselines = sorted(d["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999)
    groups = [d[d["baseline"] == b][metric].values for b in baselines]

    fig, ax = plt.subplots(figsize=(8, 5))
    box = ax.boxplot(groups, labels=baselines, patch_artist=True, showfliers=True)
    for patch, b in zip(box["boxes"], baselines):
        patch.set_facecolor(BASELINE_COLORS.get(b, "#cccccc"))
        patch.set_alpha(0.45)

    for i, b in enumerate(baselines, start=1):
        g = d[d["baseline"] == b][metric].reset_index(drop=True)
        if g.empty:
            continue
        offsets = [((j % 5) - 2) * 0.035 for j in range(len(g))]
        x = [i + off for off in offsets]
        ax.scatter(x, g.values, s=28, alpha=0.75, color=BASELINE_COLORS.get(b, "#333333"))

    ax.set_xlabel("Baseline")
    ax.set_ylabel("Wall seconds (makespan proxy)")
    ax.set_title("Runtime Distribution by Baseline")
    ax.grid(axis="y", alpha=0.2)
    fig.tight_layout()
    fig.savefig(PLOTS / "runtime_distribution.png", dpi=170)
    plt.close(fig)


def plot_throughput_vs_energy(df: pd.DataFrame):
    x_col = "energy_sim_kwh_est"
    y_col = "throughput_jobs_per_sim_hour"
    d = df.dropna(subset=[x_col, y_col, "baseline"]).copy()
    if d.empty:
        return

    fig, ax = plt.subplots(figsize=(8, 5))
    for b in sorted(d["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = d[d["baseline"] == b]
        ax.scatter(
            grp[x_col],
            grp[y_col],
            label=f"baseline {b}",
            s=56,
            alpha=0.85,
            color=BASELINE_COLORS.get(b, None),
        )

    frontier = pareto_frontier(d, x_col=x_col, y_col=y_col)
    if not frontier.empty:
        ax.plot(frontier[x_col], frontier[y_col], linestyle="--", linewidth=1.4, color="#222222", label="Pareto frontier")

    ax.set_xlabel("Estimated Energy (kWh, simulated time)")
    ax.set_ylabel("Throughput (jobs / simulated hour)")
    ax.set_title("Throughput vs Energy Tradeoff")
    ax.grid(alpha=0.2)
    ax.legend()
    fig.tight_layout()
    fig.savefig(PLOTS / "throughput_vs_energy.png", dpi=170)
    plt.close(fig)


def plot_energy_vs_makespan(df: pd.DataFrame):
    x_col = "energy_sim_kwh_est"
    y_col = "wall_seconds"
    d = df.dropna(subset=[x_col, y_col, "baseline"]).copy()
    if d.empty:
        return

    fig, ax = plt.subplots(figsize=(8, 5))
    for b in sorted(d["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = d[d["baseline"] == b]
        ax.scatter(
            grp[x_col],
            grp[y_col],
            label=f"baseline {b}",
            s=52,
            alpha=0.75,
            color=BASELINE_COLORS.get(b, None),
        )

    means = d.groupby("baseline", as_index=False).agg(
        energy_mean=(x_col, "mean"),
        makespan_mean=(y_col, "mean"),
    )
    means.sort_values("baseline", key=baseline_sort_key, inplace=True)
    for _, row in means.iterrows():
        b = row["baseline"]
        ax.scatter(
            [row["energy_mean"]],
            [row["makespan_mean"]],
            marker="*",
            s=220,
            color=BASELINE_COLORS.get(b, "#111111"),
            edgecolor="#111111",
            linewidth=0.9,
        )
        ax.annotate(
            f"{b} mean",
            (row["energy_mean"], row["makespan_mean"]),
            textcoords="offset points",
            xytext=(6, 6),
            fontsize=9,
        )

    ax.set_xlabel("Estimated Energy (kWh, simulated time)")
    ax.set_ylabel("Wall seconds (makespan proxy)")
    ax.set_title("Energy vs Makespan")
    ax.grid(alpha=0.2)
    ax.legend()
    fig.tight_layout()
    fig.savefig(PLOTS / "energy_vs_makespan.png", dpi=170)
    plt.close(fig)


def plot_baseline_summary_bars(df: pd.DataFrame):
    cols = ["energy_sim_kwh_est", "throughput_jobs_per_sim_hour", "wall_seconds"]
    needed = [c for c in cols if c in df.columns]
    if not needed:
        return
    d = df.dropna(subset=["baseline"]).copy()
    if d.empty:
        return
    summary = d.groupby("baseline", as_index=False).agg(
        energy_kwh=("energy_sim_kwh_est", "mean"),
        throughput=("throughput_jobs_per_sim_hour", "mean"),
        makespan=("wall_seconds", "mean"),
    )
    summary.sort_values("baseline", key=baseline_sort_key, inplace=True)

    fig, axes = plt.subplots(1, 3, figsize=(13, 4.5))
    metrics = [
        ("energy_kwh", "Mean Energy (kWh)"),
        ("throughput", "Mean Throughput (jobs/sim hour)"),
        ("makespan", "Mean Makespan (s)"),
    ]
    for ax, (col, title) in zip(axes, metrics):
        vals = summary[col].values
        xs = summary["baseline"].values
        colors = [BASELINE_COLORS.get(x, "#888888") for x in xs]
        ax.bar(xs, vals, color=colors, alpha=0.8)
        ax.set_title(title)
        ax.set_xlabel("Baseline")
        ax.grid(axis="y", alpha=0.2)
    fig.suptitle("Baseline Mean Metrics")
    fig.tight_layout()
    fig.savefig(PLOTS / "baseline_means.png", dpi=170)
    plt.close(fig)


def plot_completion_summary(df: pd.DataFrame):
    if "baseline" not in df.columns or "run_completed" not in df.columns:
        return
    d = df.dropna(subset=["baseline"]).copy()
    if d.empty:
        return
    completed_mask = (
        d["run_completed"].astype(str).str.strip().str.lower().isin({"true", "1", "yes"})
    )
    d["completed_count"] = completed_mask.astype(int)
    summary = d.groupby("baseline", as_index=False).agg(
        runs_total=("baseline", "count"),
        runs_completed=("completed_count", "sum"),
    )
    if summary.empty:
        return
    summary["runs_incomplete"] = summary["runs_total"] - summary["runs_completed"]
    summary.sort_values("baseline", key=baseline_sort_key, inplace=True)

    fig, axes = plt.subplots(1, 2, figsize=(11, 4.5))
    xs = summary["baseline"].tolist()
    colors = [BASELINE_COLORS.get(x, "#888888") for x in xs]

    axes[0].bar(xs, summary["runs_completed"], color=colors, alpha=0.85, label="Completed")
    axes[0].bar(
        xs,
        summary["runs_incomplete"],
        bottom=summary["runs_completed"],
        color="#d9d9d9",
        alpha=0.9,
        label="Incomplete",
    )
    axes[0].set_title("Run Completion Counts")
    axes[0].set_xlabel("Baseline")
    axes[0].set_ylabel("Runs")
    axes[0].grid(axis="y", alpha=0.2)
    axes[0].legend()

    completion_rate = 100.0 * summary["runs_completed"] / summary["runs_total"]
    axes[1].bar(xs, completion_rate, color=colors, alpha=0.85)
    axes[1].set_title("Completion Rate")
    axes[1].set_xlabel("Baseline")
    axes[1].set_ylabel("Completed runs (%)")
    axes[1].set_ylim(0, 100)
    axes[1].grid(axis="y", alpha=0.2)

    fig.suptitle("Run Robustness by Baseline")
    fig.tight_layout()
    fig.savefig(PLOTS / "completion_summary.png", dpi=170)
    plt.close(fig)


def main():
    summary = RESULTS / "summary.csv"
    if not summary.exists():
        raise SystemExit("run collect first: scripts/06_collect.py")
    PLOTS.mkdir(parents=True, exist_ok=True)

    df_all = pd.read_csv(summary)
    ensure_numeric(
        df_all,
        [
            "wall_seconds",
            "jobs_total",
            "sim_seconds",
            "throughput_jobs_per_wall_sec",
            "throughput_jobs_per_sim_sec",
            "throughput_jobs_per_sim_hour",
            "energy_sim_joules_est",
            "energy_sim_kwh_est",
            "avg_cluster_power_w_est",
        ],
    )
    plot_completion_summary(df_all)
    df = filter_completed_runs(df_all)

    plot_runtime_distribution(df)
    plot_throughput_vs_energy(df)
    plot_energy_vs_makespan(df)
    plot_baseline_summary_bars(df)

    print(f"plots written to {PLOTS}")


if __name__ == "__main__":
    main()
