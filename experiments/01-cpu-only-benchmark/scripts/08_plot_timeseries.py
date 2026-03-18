#!/usr/bin/env python3
"""Generate paper-style time-series power plots from simulator timeseries.csv files.

Produces multi-panel figures (like arXiv:2508.20016) with one line per baseline:
  - Cluster CPU utilization over time
  - Total IT power (kW)
  - PUE
  - Total facility power (kW)

Usage:
    python scripts/08_plot_timeseries.py                    # uses latest run
    RESULTS_DIR=runs/0002_.../results python scripts/08_plot_timeseries.py
"""
import os
import pathlib
import sys

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

ROOT = pathlib.Path(__file__).resolve().parents[1]
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "results"))).resolve()
PLOTS = RESULTS / "plots"
BASELINE_ORDER = ["A", "B", "C"]
BASELINE_COLORS = {"A": "#4c78a8", "B": "#f58518", "C": "#54a24b"}
BASELINE_LABELS = {
    "A": "A  (no Joulie)",
    "B": "B  (static partition)",
    "C": "C  (queue-aware)",
}

# Smoothing window in seconds for the rolling average.
SMOOTH_WINDOW_SEC = 10


def discover_timeseries(results_dir: pathlib.Path) -> dict[str, list[pathlib.Path]]:
    """Find timeseries.csv files grouped by baseline."""
    by_baseline: dict[str, list[pathlib.Path]] = {}
    for csv in sorted(results_dir.rglob("timeseries.csv")):
        # Infer baseline from parent directory name: ..._bA_s1/timeseries.csv
        parent = csv.parent.name
        baseline = None
        for part in parent.split("_"):
            if part.startswith("b") and len(part) == 2 and part[1].isalpha():
                baseline = part[1].upper()
                break
        if baseline is None:
            continue
        by_baseline.setdefault(baseline, []).append(csv)
    return by_baseline


def load_timeseries(paths: list[pathlib.Path], time_scale: float = 1.0) -> pd.DataFrame:
    """Load and concatenate timeseries CSVs for one baseline, averaging across seeds."""
    frames = []
    for i, p in enumerate(paths):
        try:
            df = pd.read_csv(p)
        except Exception as e:
            print(f"  warning: skipping {p}: {e}", file=sys.stderr)
            continue
        if df.empty or "elapsed_sec" not in df.columns:
            continue
        df["seed"] = i
        frames.append(df)
    if not frames:
        return pd.DataFrame()
    combined = pd.concat(frames, ignore_index=True)
    # Convert elapsed seconds to real wall-clock minutes for readability.
    combined["elapsed_sim_min"] = combined["elapsed_sec"] / 60.0
    return combined


def resample_and_average(df: pd.DataFrame, time_col: str, value_cols: list[str],
                         bin_width: float = 0.5) -> pd.DataFrame:
    """Bin by time, average across seeds, and apply rolling smoothing."""
    if df.empty:
        return df
    df = df.copy()
    df["_tbin"] = (df[time_col] / bin_width).round() * bin_width
    agg = {c: "mean" for c in value_cols}
    grouped = df.groupby("_tbin", as_index=False).agg(agg)
    grouped.rename(columns={"_tbin": time_col}, inplace=True)
    # Rolling smooth.
    window = max(1, int(SMOOTH_WINDOW_SEC / max(bin_width, 0.1)))
    for c in value_cols:
        grouped[c] = grouped[c].rolling(window, min_periods=1, center=True).mean()
    return grouped


def infer_time_scale(results_dir: pathlib.Path) -> float:
    """Try to read timeScale from a metadata.json in the results tree."""
    import json
    for meta in results_dir.rglob("metadata.json"):
        try:
            obj = json.loads(meta.read_text())
            ts = obj.get("timeScale")
            if ts and float(ts) > 0:
                return float(ts)
        except Exception:
            continue
    return 1.0


def plot_timeseries_panels(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Generate a 4-panel time-series figure like arXiv:2508.20016."""
    panels = [
        ("cluster_cpu_util", "Cluster CPU Utilization", "", lambda x: x * 100, "%"),
        ("it_power_w", "Total IT Power", "", lambda x: x / 1000, "kW"),
        ("pue", "Power Usage Effectiveness (PUE)", "", lambda x: x, ""),
        ("facility_power_w", "Total Facility Power", "", lambda x: x / 1000, "kW"),
    ]
    fig, axes = plt.subplots(len(panels), 1, figsize=(12, 10), sharex=True)
    time_col = "elapsed_sim_min"

    for ax, (col, title, _ylabel, transform, unit) in zip(axes, panels):
        for baseline in BASELINE_ORDER:
            df = by_baseline.get(baseline)
            if df is None or df.empty or col not in df.columns:
                continue
            label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
            color = BASELINE_COLORS.get(baseline, None)
            vals = transform(df[col])
            ax.plot(df[time_col], vals, label=label, color=color, linewidth=1.2, alpha=0.9)
        ylabel = f"{title}"
        if unit:
            ylabel += f" ({unit})"
        ax.set_ylabel(ylabel, fontsize=10)
        ax.grid(alpha=0.15)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Wall-clock time (minutes)", fontsize=10)
    fig.suptitle("Cluster Power Profile Over Time", fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(out_dir / "timeseries_power_profile.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_power_profile.png'}")


def plot_gpu_power_panel(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """GPU-specific power panel (only if GPU data is present)."""
    has_gpu = any(
        df is not None and not df.empty and "gpu_power_w" in df.columns and df["gpu_power_w"].max() > 0
        for df in by_baseline.values()
    )
    if not has_gpu:
        return

    panels = [
        ("cluster_gpu_util", "Cluster GPU Utilization", lambda x: x * 100, "%"),
        ("gpu_power_w", "Total GPU Power", lambda x: x / 1000, "kW"),
        ("cpu_power_w", "Total CPU Power", lambda x: x / 1000, "kW"),
        ("facility_power_w", "Total Facility Power", lambda x: x / 1000, "kW"),
    ]
    fig, axes = plt.subplots(len(panels), 1, figsize=(12, 10), sharex=True)
    time_col = "elapsed_sim_min"

    for ax, (col, title, transform, unit) in zip(axes, panels):
        for baseline in BASELINE_ORDER:
            df = by_baseline.get(baseline)
            if df is None or df.empty or col not in df.columns:
                continue
            label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
            color = BASELINE_COLORS.get(baseline, None)
            vals = transform(df[col])
            ax.plot(df[time_col], vals, label=label, color=color, linewidth=1.2, alpha=0.9)
        ylabel = f"{title}"
        if unit:
            ylabel += f" ({unit})"
        ax.set_ylabel(ylabel, fontsize=10)
        ax.grid(alpha=0.15)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Wall-clock time (minutes)", fontsize=10)
    fig.suptitle("CPU + GPU Power Breakdown Over Time", fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(out_dir / "timeseries_gpu_power.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_gpu_power.png'}")


def plot_energy_cumulative(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Cumulative energy consumption over time."""
    fig, ax = plt.subplots(figsize=(12, 5))
    time_col = "elapsed_sim_min"

    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "energy_cumulative_j" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        kwh = df["energy_cumulative_j"] / 3.6e6
        ax.plot(df[time_col], kwh, label=label, color=color, linewidth=1.5, alpha=0.9)

    ax.set_xlabel("Wall-clock time (minutes)", fontsize=10)
    ax.set_ylabel("Cumulative Energy (kWh)", fontsize=10)
    ax.set_title("Cumulative Energy Consumption Over Time", fontsize=13)
    ax.grid(alpha=0.15)
    ax.legend(fontsize=9)
    fig.tight_layout()
    fig.savefig(out_dir / "timeseries_cumulative_energy.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_cumulative_energy.png'}")


def plot_cooling_ambient(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Ambient temperature and cooling power over time."""
    panels = [
        ("ambient_temp_c", "Ambient Temperature", lambda x: x, "°C"),
        ("cooling_power_w", "Cooling Power", lambda x: x / 1000, "kW"),
        ("pue", "PUE", lambda x: x, ""),
    ]
    fig, axes = plt.subplots(len(panels), 1, figsize=(12, 8), sharex=True)
    time_col = "elapsed_sim_min"

    for ax, (col, title, transform, unit) in zip(axes, panels):
        for baseline in BASELINE_ORDER:
            df = by_baseline.get(baseline)
            if df is None or df.empty or col not in df.columns:
                continue
            label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
            color = BASELINE_COLORS.get(baseline, None)
            vals = transform(df[col])
            ax.plot(df[time_col], vals, label=label, color=color, linewidth=1.2, alpha=0.9)
        ylabel = f"{title}"
        if unit:
            ylabel += f" ({unit})"
        ax.set_ylabel(ylabel, fontsize=10)
        ax.grid(alpha=0.15)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Wall-clock time (minutes)", fontsize=10)
    fig.suptitle("Cooling and Ambient Conditions Over Time", fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(out_dir / "timeseries_cooling.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_cooling.png'}")


def export_fmu_timeseries(by_baseline: dict[str, pd.DataFrame], time_scale: float, out_dir: pathlib.Path):
    """Export per-baseline CSV files in the format expected by the FMU cooling model.

    Columns: timestamp_utc, elapsed_sec, it_power_w, cpu_power_w, gpu_power_w,
             pue, cooling_power_w, facility_power_w, ambient_temp_c,
             cluster_cpu_util, cluster_gpu_util, nodes_active, pods_running,
             energy_cumulative_j, sim_elapsed_sec, sim_hour
    """
    fmu_dir = out_dir / "fmu_input"
    fmu_dir.mkdir(parents=True, exist_ok=True)
    for baseline, df in by_baseline.items():
        if df.empty:
            continue
        out = df.copy()
        # Reconstruct elapsed_sec from the binned minutes column.
        out["elapsed_sec"] = out["elapsed_sim_min"] * 60.0
        # Simulated elapsed seconds and hour-of-day.
        out["sim_elapsed_sec"] = out["elapsed_sec"] * time_scale
        out["sim_hour"] = (out["sim_elapsed_sec"] % 86400) / 3600.0
        # Synthetic UTC timestamp starting at midnight.
        out["timestamp_utc"] = pd.to_datetime(out["sim_elapsed_sec"], unit="s", origin="2026-01-01")
        cols = [
            "timestamp_utc", "elapsed_sec", "sim_elapsed_sec", "sim_hour",
            "it_power_w", "cpu_power_w", "gpu_power_w",
            "pue", "cooling_power_w", "facility_power_w", "ambient_temp_c",
            "cluster_cpu_util", "cluster_gpu_util", "nodes_active", "pods_running",
            "energy_cumulative_j",
        ]
        present = [c for c in cols if c in out.columns]
        csv_path = fmu_dir / f"timeseries_baseline_{baseline}.csv"
        out[present].to_csv(csv_path, index=False)
        print(f"  wrote FMU input: {csv_path}  ({len(out)} rows)")


def main():
    print(f"results dir: {RESULTS}")
    ts_map = discover_timeseries(RESULTS)
    if not ts_map:
        print("no timeseries.csv files found — run experiments with updated simulator first",
              file=sys.stderr)
        sys.exit(1)

    time_scale = infer_time_scale(RESULTS)
    print(f"time_scale={time_scale}")

    value_cols = [
        "it_power_w", "cpu_power_w", "gpu_power_w", "pue",
        "cooling_power_w", "facility_power_w", "ambient_temp_c",
        "cluster_cpu_util", "cluster_gpu_util", "pods_running",
        "nodes_active", "energy_cumulative_j",
    ]

    by_baseline: dict[str, pd.DataFrame] = {}
    for baseline in BASELINE_ORDER:
        paths = ts_map.get(baseline, [])
        if not paths:
            print(f"  baseline {baseline}: no data")
            continue
        print(f"  baseline {baseline}: {len(paths)} seed(s)")
        raw = load_timeseries(paths, time_scale)
        if raw.empty:
            continue
        present_cols = [c for c in value_cols if c in raw.columns]
        resampled = resample_and_average(raw, "elapsed_sim_min", present_cols, bin_width=0.25)
        by_baseline[baseline] = resampled

    if not by_baseline:
        print("no valid timeseries data to plot", file=sys.stderr)
        sys.exit(1)

    PLOTS.mkdir(parents=True, exist_ok=True)
    plot_timeseries_panels(by_baseline, PLOTS)
    plot_gpu_power_panel(by_baseline, PLOTS)
    plot_energy_cumulative(by_baseline, PLOTS)
    plot_cooling_ambient(by_baseline, PLOTS)
    export_fmu_timeseries(by_baseline, time_scale, RESULTS)
    print(f"all timeseries plots written to {PLOTS}")


if __name__ == "__main__":
    main()
