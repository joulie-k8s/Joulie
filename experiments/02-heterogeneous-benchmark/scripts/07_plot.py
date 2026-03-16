#!/usr/bin/env python3
import os
import pathlib

import matplotlib.pyplot as plt
import pandas as pd

ROOT = pathlib.Path("experiments/02-heterogeneous-benchmark")
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results")))
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


def filter_stable_tradeoff_rows(df: pd.DataFrame, x_col: str, y_col: str):
    d = df.copy()
    ensure_numeric(d, [x_col, y_col])
    d = d.dropna(subset=[x_col, y_col, "baseline"]).copy()
    if "sample_quality" in d.columns:
        d = d[d["sample_quality"] == "stable"].copy()
    return d


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
    box = ax.boxplot(groups, tick_labels=baselines, patch_artist=True, showfliers=True)
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


def relative_to_baseline_a(df: pd.DataFrame) -> pd.DataFrame:
    needed = ["seed", "baseline", "energy_sim_kwh_est", "throughput_jobs_per_sim_hour", "wall_seconds"]
    d = df.dropna(subset=[c for c in needed if c in df.columns]).copy()
    if d.empty:
        return pd.DataFrame()
    a = d[d["baseline"] == "A"][
        ["seed", "energy_sim_kwh_est", "throughput_jobs_per_sim_hour", "wall_seconds"]
    ].rename(
        columns={
            "energy_sim_kwh_est": "energy_a",
            "throughput_jobs_per_sim_hour": "throughput_a",
            "wall_seconds": "wall_a",
        }
    )
    merged = d.merge(a, on="seed", how="inner")
    merged = merged[merged["baseline"] != "A"].copy()
    if merged.empty:
        return merged
    merged["energy_savings_pct"] = 100.0 * (1.0 - merged["energy_sim_kwh_est"] / merged["energy_a"])
    merged["throughput_slowdown_pct"] = 100.0 * (1.0 - merged["throughput_jobs_per_sim_hour"] / merged["throughput_a"])
    merged["makespan_increase_pct"] = 100.0 * (merged["wall_seconds"] / merged["wall_a"] - 1.0)
    return merged


def plot_relative_tradeoff_scatter(df: pd.DataFrame):
    d = relative_to_baseline_a(df)
    if d.empty:
        return
    fig, ax = plt.subplots(figsize=(8.5, 5.5))
    for b in sorted(d["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = d[d["baseline"] == b]
        ax.scatter(
            grp["throughput_slowdown_pct"],
            grp["energy_savings_pct"],
            label=f"baseline {b} vs A",
            s=64,
            alpha=0.85,
            color=BASELINE_COLORS.get(b, None),
        )
    ax.axhline(0, color="#888888", linewidth=0.8)
    ax.axvline(0, color="#888888", linewidth=0.8)
    ax.set_xlabel("Throughput slowdown vs baseline A (%)")
    ax.set_ylabel("Energy savings vs baseline A (%)")
    ax.set_title("Energy Savings vs Throughput Slowdown")
    ax.grid(alpha=0.2)
    ax.legend()
    fig.tight_layout()
    fig.savefig(PLOTS / "relative_tradeoff_vs_a.png", dpi=170)
    plt.close(fig)


def plot_relative_tradeoff_bars(df: pd.DataFrame):
    d = relative_to_baseline_a(df)
    if d.empty:
        return
    summary = d.groupby("baseline", as_index=False).agg(
        energy_savings_pct=("energy_savings_pct", "mean"),
        throughput_slowdown_pct=("throughput_slowdown_pct", "mean"),
        makespan_increase_pct=("makespan_increase_pct", "mean"),
    )
    summary.sort_values("baseline", key=baseline_sort_key, inplace=True)

    fig, axes = plt.subplots(1, 3, figsize=(13.5, 4.5))
    metrics = [
        ("energy_savings_pct", "Mean Energy Savings vs A (%)"),
        ("throughput_slowdown_pct", "Mean Throughput Slowdown vs A (%)"),
        ("makespan_increase_pct", "Mean Makespan Increase vs A (%)"),
    ]
    for ax, (col, title) in zip(axes, metrics):
        xs = summary["baseline"].values
        vals = summary[col].values
        colors = [BASELINE_COLORS.get(x, "#888888") for x in xs]
        ax.bar(xs, vals, color=colors, alpha=0.82)
        ax.axhline(0, color="#888888", linewidth=0.8)
        ax.set_title(title)
        ax.set_xlabel("Baseline")
        ax.grid(axis="y", alpha=0.2)
    fig.suptitle("Relative Tradeoffs Against Baseline A")
    fig.tight_layout()
    fig.savefig(PLOTS / "relative_tradeoff_bars_vs_a.png", dpi=170)
    plt.close(fig)


def plot_workload_type_tradeoff():
    path = RESULTS / "workload_type_tradeoff_vs_a.csv"
    if not path.exists():
        return
    df = pd.read_csv(path)
    df = filter_stable_tradeoff_rows(df, "mean_slowdown_pct_vs_a", "mean_energy_savings_exposure_pct_vs_a")
    if df.empty:
        return
    fig, ax = plt.subplots(figsize=(10.5, 6))
    for baseline in sorted(df["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = df[df["baseline"] == baseline]
        ax.scatter(
            grp["mean_slowdown_pct_vs_a"],
            grp["mean_energy_savings_exposure_pct_vs_a"],
            s=70,
            alpha=0.85,
            color=BASELINE_COLORS.get(baseline, None),
            label=f"baseline {baseline}",
        )
        for _, row in grp.iterrows():
            ax.annotate(
                row["workload_type"],
                (row["mean_slowdown_pct_vs_a"], row["mean_energy_savings_exposure_pct_vs_a"]),
                textcoords="offset points",
                xytext=(5, 4),
                fontsize=8,
            )
    ax.axhline(0, color="#888888", linewidth=0.8)
    ax.axvline(0, color="#888888", linewidth=0.8)
    ax.set_xlabel("Mean slowdown vs baseline A (%)")
    ax.set_ylabel("Mean energy-savings exposure vs baseline A (%)")
    ax.set_title("Workload-Type Tradeoff: Slowdown vs Energy-Savings Exposure")
    ax.grid(alpha=0.2)
    ax.legend()
    fig.tight_layout()
    fig.savefig(PLOTS / "workload_type_tradeoff_vs_a.png", dpi=170)
    plt.close(fig)


def plot_workload_type_rankings():
    path = RESULTS / "workload_type_tradeoff_vs_a.csv"
    if not path.exists():
        return
    df = pd.read_csv(path)
    df = filter_stable_tradeoff_rows(df, "mean_slowdown_pct_vs_a", "mean_energy_savings_exposure_pct_vs_a")
    if df.empty:
        return
    for baseline in sorted(df["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = df[df["baseline"] == baseline].copy()
        if grp.empty:
            continue
        grp.sort_values("tradeoff_score", ascending=False, inplace=True)
        fig, axes = plt.subplots(1, 2, figsize=(14, 5.5))
        y = range(len(grp))
        color = BASELINE_COLORS.get(baseline, "#888888")
        axes[0].barh(y, grp["mean_energy_savings_exposure_pct_vs_a"], color=color, alpha=0.82)
        axes[0].set_yticks(list(y), grp["workload_type"])
        axes[0].invert_yaxis()
        axes[0].set_xlabel("Mean energy-savings exposure vs A (%)")
        axes[0].set_title(f"Baseline {baseline}: Workload Types Helped Most")
        axes[0].grid(axis="x", alpha=0.2)

        worst = grp.sort_values("mean_slowdown_pct_vs_a", ascending=False)
        y2 = range(len(worst))
        axes[1].barh(y2, worst["mean_slowdown_pct_vs_a"], color=color, alpha=0.82)
        axes[1].set_yticks(list(y2), worst["workload_type"])
        axes[1].invert_yaxis()
        axes[1].set_xlabel("Mean slowdown vs A (%)")
        axes[1].set_title(f"Baseline {baseline}: Workload Types Hurt Most")
        axes[1].grid(axis="x", alpha=0.2)

        fig.tight_layout()
        fig.savefig(PLOTS / f"workload_type_rankings_baseline_{baseline}.png", dpi=170)
        plt.close(fig)


def plot_hardware_family_tradeoff():
    path = RESULTS / "hardware_family_relative_to_a.csv"
    if not path.exists():
        return
    df = pd.read_csv(path)
    df = filter_stable_tradeoff_rows(df, "mean_slowdown_pct_vs_a", "mean_energy_savings_pct_vs_a")
    if df.empty:
        return
    fig, ax = plt.subplots(figsize=(10.5, 6))
    for baseline in sorted(df["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = df[df["baseline"] == baseline]
        ax.scatter(
            grp["mean_slowdown_pct_vs_a"],
            grp["mean_energy_savings_pct_vs_a"],
            s=78,
            alpha=0.85,
            color=BASELINE_COLORS.get(baseline, None),
            label=f"baseline {baseline}",
        )
        for _, row in grp.iterrows():
            ax.annotate(row["hardware_family"], (row["mean_slowdown_pct_vs_a"], row["mean_energy_savings_pct_vs_a"]), textcoords="offset points", xytext=(5, 4), fontsize=8)
    ax.axhline(0, color="#888888", linewidth=0.8)
    ax.axvline(0, color="#888888", linewidth=0.8)
    ax.set_xlabel("Mean slowdown vs baseline A (%)")
    ax.set_ylabel("Mean energy savings vs baseline A (%)")
    ax.set_title("Hardware Families Best Fit for Throttling")
    ax.grid(alpha=0.2)
    ax.legend()
    fig.tight_layout()
    fig.savefig(PLOTS / "hardware_family_tradeoff_vs_a.png", dpi=170)
    plt.close(fig)


def plot_hardware_family_rankings():
    path = RESULTS / "hardware_family_relative_to_a.csv"
    if not path.exists():
        return
    df = pd.read_csv(path)
    df = filter_stable_tradeoff_rows(df, "mean_slowdown_pct_vs_a", "mean_energy_savings_pct_vs_a")
    if df.empty:
        return
    for baseline in sorted(df["baseline"].unique(), key=lambda x: BASELINE_ORDER.index(x) if x in BASELINE_ORDER else 999):
        grp = df[df["baseline"] == baseline].copy()
        if grp.empty:
            continue
        fig, axes = plt.subplots(1, 2, figsize=(14, 5.5))

        helped = grp.sort_values("mean_energy_savings_pct_vs_a", ascending=False)
        y = range(len(helped))
        color = BASELINE_COLORS.get(baseline, "#888888")
        axes[0].barh(y, helped["mean_energy_savings_pct_vs_a"], color=color, alpha=0.82)
        axes[0].set_yticks(list(y), helped["hardware_family"])
        axes[0].invert_yaxis()
        axes[0].set_xlabel("Mean energy savings vs A (%)")
        axes[0].set_title(f"Baseline {baseline}: Hardware Families Helped Most")
        axes[0].grid(axis="x", alpha=0.2)

        hurt = grp.sort_values("mean_slowdown_pct_vs_a", ascending=False)
        y2 = range(len(hurt))
        axes[1].barh(y2, hurt["mean_slowdown_pct_vs_a"], color=color, alpha=0.82)
        axes[1].set_yticks(list(y2), hurt["hardware_family"])
        axes[1].invert_yaxis()
        axes[1].set_xlabel("Mean slowdown vs A (%)")
        axes[1].set_title(f"Baseline {baseline}: Hardware Families Hurt Most")
        axes[1].grid(axis="x", alpha=0.2)

        fig.tight_layout()
        fig.savefig(PLOTS / f"hardware_family_rankings_baseline_{baseline}.png", dpi=170)
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
    plot_relative_tradeoff_scatter(df)
    plot_relative_tradeoff_bars(df)
    plot_workload_type_tradeoff()
    plot_workload_type_rankings()
    plot_hardware_family_tradeoff()
    plot_hardware_family_rankings()

    print(f"plots written to {PLOTS}")


if __name__ == "__main__":
    main()
