#!/usr/bin/env python3
"""Generate paper-style PUE comparison plots across cooling models and baselines.

Reads the enriched timeseries CSVs produced by 02_apply_cooling_models.py and
generates multi-panel time-series figures comparing:
  - PUE over time for different cooling systems (one panel per baseline)
  - Facility power over time for different cooling systems
  - Cooling power breakdown
  - Summary bar charts (mean PUE, total facility energy)

Output: plots/*.png

Usage:
    python 03_plot_pue_comparison.py
    python 03_plot_pue_comparison.py --data-dir ./data
"""
import argparse
import pathlib
import re
import sys

import matplotlib.pyplot as plt
import matplotlib.ticker as mticker
import numpy as np
import pandas as pd

BASELINE_ORDER = ["A", "B", "C"]
BASELINE_LABELS = {
    "A": "A (no Joulie)",
    "B": "B (static partition)",
    "C": "C (queue-aware)",
}

# Distinct colors for cooling models
COOLING_COLORS = {
    "air-cooled_crah": "#d62728",       # red
    "hot_cold_aisle": "#ff7f0e",         # orange
    "direct_liquid_cooling_dlc": "#2ca02c",  # green
    "immersion_cooling": "#1f77b4",      # blue
    "free_cooling_economizer": "#9467bd", # purple
}
COOLING_LINESTYLES = {
    "air-cooled_crah": "-",
    "hot_cold_aisle": "--",
    "direct_liquid_cooling_dlc": "-.",
    "immersion_cooling": ":",
    "free_cooling_economizer": (0, (3, 1, 1, 1)),
}

SMOOTH_WINDOW = 60  # rolling window size for smoothing (~5h at 5-min resolution)


def discover_enriched_files(data_dir: pathlib.Path) -> dict[str, dict[str, pathlib.Path]]:
    """Find enriched timeseries files: {baseline: {cooling_tag: path}}."""
    result = {}
    pattern = re.compile(r"timeseries_baseline_([ABC])_cooling_(.+)\.csv")
    for p in sorted(data_dir.glob("timeseries_baseline_*_cooling_*.csv")):
        m = pattern.match(p.name)
        if m:
            baseline, cooling_tag = m.group(1), m.group(2)
            result.setdefault(baseline, {})[cooling_tag] = p
    return result


def load_and_smooth(path: pathlib.Path, time_scale: float) -> pd.DataFrame:
    df = pd.read_csv(path)
    if "elapsed_sec" in df.columns:
        df["elapsed_sim_min"] = df["elapsed_sec"] * time_scale / 60.0
        df["elapsed_sim_days"] = df["elapsed_sim_min"] / 60.0 / 24.0
    smooth_cols = ["it_power_w", "cpu_power_w", "gpu_power_w", "pue", "cop",
                   "cooling_power_w", "facility_power_w", "indoor_temp_c",
                   "cluster_cpu_util", "cluster_gpu_util"]
    for c in smooth_cols:
        if c in df.columns:
            df[c] = df[c].rolling(SMOOTH_WINDOW, min_periods=1, center=True).mean()
    return df


def get_time_axis(df: pd.DataFrame) -> tuple:
    """Return (x_values, x_label) choosing days or minutes based on duration."""
    max_min = df["elapsed_sim_min"].max()
    if max_min > 2880:  # > 2 days: use days
        return df["elapsed_sim_days"], "Time (days)"
    elif max_min > 300:  # > 5 hours: use hours
        return df["elapsed_sim_min"] / 60.0, "Time (hours)"
    else:
        return df["elapsed_sim_min"], "Simulated Time (minutes)"


def cooling_label(tag: str) -> str:
    """Convert cooling tag back to display name."""
    labels = {
        "air-cooled_crah": "Air-cooled (CRAH)",
        "hot_cold_aisle": "Hot/cold aisle",
        "direct_liquid_cooling_dlc": "Direct liquid cooling",
        "immersion_cooling": "Immersion cooling",
        "free_cooling_economizer": "Free cooling (economizer)",
    }
    return labels.get(tag, tag.replace("_", " ").title())


def plot_pue_by_cooling_per_baseline(files: dict, out_dir: pathlib.Path, time_scale: float):
    """One figure per baseline: PUE time-series for each cooling model."""
    for baseline in BASELINE_ORDER:
        if baseline not in files:
            continue
        cooling_files = files[baseline]

        fig, axes = plt.subplots(3, 1, figsize=(13, 9), sharex=True)
        ax_pue, ax_cooling, ax_facility = axes

        x_label = None
        for tag, path in sorted(cooling_files.items()):
            df = load_and_smooth(path, time_scale)
            if df.empty:
                continue
            x, x_label = get_time_axis(df)
            color = COOLING_COLORS.get(tag, None)
            ls = COOLING_LINESTYLES.get(tag, "-")
            label = cooling_label(tag)

            ax_pue.plot(x, df["pue"], label=label, color=color, linestyle=ls, linewidth=1.0)
            ax_cooling.plot(x, df["cooling_power_w"] / 1000, label=label, color=color, linestyle=ls, linewidth=1.0)
            ax_facility.plot(x, df["facility_power_w"] / 1000, label=label, color=color, linestyle=ls, linewidth=1.0)

        # Also plot IT power as dashed gray on facility panel
        first_path = next(iter(cooling_files.values()))
        df0 = load_and_smooth(first_path, time_scale)
        if not df0.empty:
            x0, _ = get_time_axis(df0)
            ax_facility.plot(x0, df0["it_power_w"] / 1000,
                             label="IT power only", color="#888888", linestyle="--", linewidth=0.8, alpha=0.7)

        ax_pue.set_ylabel("PUE", fontsize=10)
        ax_pue.legend(fontsize=8, loc="upper right", ncol=2, framealpha=0.8)
        ax_pue.grid(alpha=0.15)

        ax_cooling.set_ylabel("Cooling Power (kW)", fontsize=10)
        ax_cooling.grid(alpha=0.15)

        ax_facility.set_ylabel("Total Facility Power (kW)", fontsize=10)
        ax_facility.set_xlabel(x_label or "Time", fontsize=10)
        ax_facility.legend(fontsize=8, loc="upper right", ncol=2, framealpha=0.8)
        ax_facility.grid(alpha=0.15)

        bl = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        fig.suptitle(f"Cooling System Comparison — Baseline {bl}", fontsize=13, y=0.98)
        fig.tight_layout(rect=[0, 0, 1, 0.96])
        fig.savefig(out_dir / f"pue_cooling_comparison_baseline_{baseline}.png", dpi=200)
        plt.close(fig)
        print(f"  wrote pue_cooling_comparison_baseline_{baseline}.png")


def plot_pue_by_baseline_per_cooling(files: dict, out_dir: pathlib.Path, time_scale: float):
    """One figure per cooling model: PUE for each baseline (A/B/C)."""
    # Collect all cooling tags
    all_tags = set()
    for bf in files.values():
        all_tags.update(bf.keys())

    baseline_colors = {"A": "#4c78a8", "B": "#f58518", "C": "#54a24b"}

    for tag in sorted(all_tags):
        fig, axes = plt.subplots(3, 1, figsize=(13, 9), sharex=True)
        ax_pue, ax_facility, ax_util = axes

        x_label = None
        for baseline in BASELINE_ORDER:
            if baseline not in files or tag not in files[baseline]:
                continue
            df = load_and_smooth(files[baseline][tag], time_scale)
            if df.empty:
                continue
            x, x_label = get_time_axis(df)
            color = baseline_colors.get(baseline)
            label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")

            ax_pue.plot(x, df["pue"], label=label, color=color, linewidth=1.0)
            ax_facility.plot(x, df["facility_power_w"] / 1000, label=label, color=color, linewidth=1.0)
            if "cluster_cpu_util" in df.columns:
                ax_util.plot(x, df["cluster_cpu_util"] * 100, label=f"{label} (CPU)", color=color, linewidth=0.8)
            if "cluster_gpu_util" in df.columns and df["cluster_gpu_util"].max() > 0.01:
                ax_util.plot(x, df["cluster_gpu_util"] * 100, label=f"{label} (GPU)", color=color, linewidth=0.8, linestyle="--")

        ax_pue.set_ylabel("PUE", fontsize=10)
        ax_pue.legend(fontsize=8, loc="upper right", framealpha=0.8)
        ax_pue.grid(alpha=0.15)

        ax_facility.set_ylabel("Total Facility Power (kW)", fontsize=10)
        ax_facility.legend(fontsize=8, loc="upper right", framealpha=0.8)
        ax_facility.grid(alpha=0.15)

        ax_util.set_ylabel("Cluster Utilization (%)", fontsize=10)
        ax_util.set_xlabel(x_label or "Time", fontsize=10)
        ax_util.legend(fontsize=8, loc="upper right", ncol=2, framealpha=0.8)
        ax_util.grid(alpha=0.15)

        fig.suptitle(f"Baseline Comparison — {cooling_label(tag)}", fontsize=13, y=0.98)
        fig.tight_layout(rect=[0, 0, 1, 0.96])
        fig.savefig(out_dir / f"baseline_comparison_{tag}.png", dpi=200)
        plt.close(fig)
        print(f"  wrote baseline_comparison_{tag}.png")


def plot_summary_bars(data_dir: pathlib.Path, out_dir: pathlib.Path):
    """Bar charts from cooling_model_summary.csv."""
    summary_path = data_dir / "cooling_model_summary.csv"
    if not summary_path.exists():
        return
    df = pd.read_csv(summary_path)

    baseline_colors = {"A": "#4c78a8", "B": "#f58518", "C": "#54a24b"}

    # --- Plot 1: Mean PUE by cooling model, grouped by baseline ---
    cooling_models = df["cooling_model"].unique()
    fig, ax = plt.subplots(figsize=(12, 5.5))
    x = np.arange(len(cooling_models))
    width = 0.25
    for i, baseline in enumerate(BASELINE_ORDER):
        bd = df[df["baseline"] == baseline]
        if bd.empty:
            continue
        vals = []
        for cm in cooling_models:
            row = bd[bd["cooling_model"] == cm]
            vals.append(row["mean_pue"].values[0] if not row.empty else 0)
        ax.bar(x + i * width, vals, width, label=BASELINE_LABELS.get(baseline, baseline),
               color=baseline_colors.get(baseline), alpha=0.85)

    ax.set_xticks(x + width)
    ax.set_xticklabels(cooling_models, rotation=20, ha="right", fontsize=9)
    ax.set_ylabel("Mean PUE", fontsize=10)
    ax.set_title("Mean PUE by Cooling System and Scheduling Baseline", fontsize=12)
    ax.legend(fontsize=9)
    ax.grid(axis="y", alpha=0.15)
    ax.set_ylim(bottom=1.0)
    fig.tight_layout()
    fig.savefig(out_dir / "summary_mean_pue.png", dpi=200)
    plt.close(fig)
    print(f"  wrote summary_mean_pue.png")

    # --- Plot 2: Total facility energy by cooling model, grouped by baseline ---
    fig, ax = plt.subplots(figsize=(12, 5.5))
    for i, baseline in enumerate(BASELINE_ORDER):
        bd = df[df["baseline"] == baseline]
        if bd.empty:
            continue
        vals = []
        for cm in cooling_models:
            row = bd[bd["cooling_model"] == cm]
            vals.append(row["facility_energy_kwh"].values[0] if not row.empty else 0)
        ax.bar(x + i * width, vals, width, label=BASELINE_LABELS.get(baseline, baseline),
               color=baseline_colors.get(baseline), alpha=0.85)

    ax.set_xticks(x + width)
    ax.set_xticklabels(cooling_models, rotation=20, ha="right", fontsize=9)
    ax.set_ylabel("Total Facility Energy (kWh)", fontsize=10)
    ax.set_title("Total Facility Energy by Cooling System and Scheduling Baseline", fontsize=12)
    ax.legend(fontsize=9)
    ax.grid(axis="y", alpha=0.15)
    fig.tight_layout()
    fig.savefig(out_dir / "summary_facility_energy.png", dpi=200)
    plt.close(fig)
    print(f"  wrote summary_facility_energy.png")

    # --- Plot 3: Cooling overhead % by cooling model ---
    fig, ax = plt.subplots(figsize=(12, 5.5))
    for i, baseline in enumerate(BASELINE_ORDER):
        bd = df[df["baseline"] == baseline]
        if bd.empty:
            continue
        vals = []
        for cm in cooling_models:
            row = bd[bd["cooling_model"] == cm]
            vals.append(row["cooling_overhead_pct"].values[0] if not row.empty else 0)
        ax.bar(x + i * width, vals, width, label=BASELINE_LABELS.get(baseline, baseline),
               color=baseline_colors.get(baseline), alpha=0.85)

    ax.set_xticks(x + width)
    ax.set_xticklabels(cooling_models, rotation=20, ha="right", fontsize=9)
    ax.set_ylabel("Cooling Overhead (%)", fontsize=10)
    ax.set_title("Cooling Energy as % of IT Energy", fontsize=12)
    ax.legend(fontsize=9)
    ax.grid(axis="y", alpha=0.15)
    fig.tight_layout()
    fig.savefig(out_dir / "summary_cooling_overhead.png", dpi=200)
    plt.close(fig)
    print(f"  wrote summary_cooling_overhead.png")

    # --- Plot 4: Combined energy savings heatmap ---
    # Rows: cooling models, Columns: baselines B and C
    # Values: % facility energy savings vs baseline A with same cooling
    pivot = df.pivot_table(index="cooling_model", columns="baseline", values="facility_energy_kwh")
    if "A" in pivot.columns:
        for b in ["B", "C"]:
            if b in pivot.columns:
                pivot[f"savings_{b}_pct"] = 100.0 * (1.0 - pivot[b] / pivot["A"])

        savings_cols = [c for c in pivot.columns if c.startswith("savings_")]
        if savings_cols:
            fig, ax = plt.subplots(figsize=(8, 5))
            savings_data = pivot[savings_cols].copy()
            savings_data.columns = [c.replace("savings_", "Baseline ").replace("_pct", "") for c in savings_cols]

            im = ax.imshow(savings_data.values, cmap="YlGn", aspect="auto", vmin=0)
            ax.set_xticks(range(len(savings_data.columns)))
            ax.set_xticklabels(savings_data.columns, fontsize=10)
            ax.set_yticks(range(len(savings_data.index)))
            ax.set_yticklabels(savings_data.index, fontsize=9)

            for i in range(len(savings_data.index)):
                for j in range(len(savings_data.columns)):
                    val = savings_data.values[i, j]
                    ax.text(j, i, f"{val:.1f}%", ha="center", va="center", fontsize=10,
                            color="white" if val > 15 else "black")

            ax.set_title("Facility Energy Savings vs Baseline A (%)", fontsize=12)
            fig.colorbar(im, ax=ax, label="Savings (%)", shrink=0.8)
            fig.tight_layout()
            fig.savefig(out_dir / "summary_savings_heatmap.png", dpi=200)
            plt.close(fig)
            print(f"  wrote summary_savings_heatmap.png")


def main():
    ap = argparse.ArgumentParser(description="Plot PUE comparison across cooling models")
    ap.add_argument("--data-dir", default="", help="Directory with enriched CSVs")
    args = ap.parse_args()

    data_dir = pathlib.Path(args.data_dir) if args.data_dir else pathlib.Path(__file__).resolve().parent / "data"
    out_dir = pathlib.Path(__file__).resolve().parent / "plots"
    out_dir.mkdir(parents=True, exist_ok=True)

    # Load time_scale
    time_scale = 60.0
    meta_path = data_dir / "metadata.json"
    if meta_path.exists():
        import json
        meta = json.loads(meta_path.read_text())
        time_scale = meta.get("time_scale", time_scale)

    files = discover_enriched_files(data_dir)
    if not files:
        print(f"no enriched timeseries files found in {data_dir}", file=sys.stderr)
        print("run 02_apply_cooling_models.py first", file=sys.stderr)
        sys.exit(1)

    n_baselines = len(files)
    n_cooling = sum(len(v) for v in files.values())
    print(f"found {n_baselines} baselines, {n_cooling} enriched files total\n")

    print("--- Per-baseline cooling comparison ---")
    plot_pue_by_cooling_per_baseline(files, out_dir, time_scale)

    print("\n--- Per-cooling-model baseline comparison ---")
    plot_pue_by_baseline_per_cooling(files, out_dir, time_scale)

    print("\n--- Summary bar charts ---")
    plot_summary_bars(data_dir, out_dir)

    print(f"\nall plots written to {out_dir}")


if __name__ == "__main__":
    main()
