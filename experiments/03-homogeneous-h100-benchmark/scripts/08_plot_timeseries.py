#!/usr/bin/env python3
"""Generate paper-style time-series power plots from simulator timeseries.csv files.

Produces multi-panel figures (inspired by exp-04 sim_24h_pue.py) with one line
per baseline:
  - Job arrival rate over time
  - Active jobs (pods_running) over time
  - Total IT power (kW)
  - PUE (if FMU post-processing was applied)
  - Facility power (if available)

All x-axes are in *simulated time* (hours) so that day/night cycles and
multi-day patterns are clearly visible.

Usage:
    python scripts/08_plot_timeseries.py                    # uses latest run
    RESULTS_DIR=runs/0002_.../results python scripts/08_plot_timeseries.py
"""
import json
import os
import pathlib
import sys

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.dates as mdates
import numpy as np
import pandas as pd
from datetime import datetime, timedelta

ROOT = pathlib.Path(__file__).resolve().parents[1]
RESULTS = pathlib.Path(os.environ.get("RESULTS_DIR", str(ROOT / "runs" / "latest" / "results"))).resolve()
PLOTS = RESULTS / "plots"
BASELINE_ORDER = ["A", "B", "C"]
BASELINE_COLORS = {"A": "#d62728", "B": "#f58518", "C": "#54a24b"}
BASELINE_LABELS = {
    "A": "A  (no Joulie)",
    "B": "B  (static partition)",
    "C": "C  (queue-aware)",
}

# Smoothing window in simulated hours for the rolling average.
SMOOTH_WINDOW_SIM_HOURS = 0.5


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
    # Convert wall-clock elapsed seconds to simulated hours.
    combined["elapsed_sim_hours"] = combined["elapsed_sec"] * time_scale / 3600.0
    return combined


def resample_and_average(df: pd.DataFrame, time_col: str, value_cols: list[str],
                         bin_width: float = 0.1) -> pd.DataFrame:
    """Bin by time (sim-hours), average across seeds, and apply rolling smoothing."""
    if df.empty:
        return df
    df = df.copy()
    df["_tbin"] = (df[time_col] / bin_width).round() * bin_width
    agg = {c: "mean" for c in value_cols}
    grouped = df.groupby("_tbin", as_index=False).agg(agg)
    grouped.rename(columns={"_tbin": time_col}, inplace=True)
    # Rolling smooth in sim-hours.
    window = max(1, int(SMOOTH_WINDOW_SIM_HOURS / max(bin_width, 0.01)))
    for c in value_cols:
        grouped[c] = grouped[c].rolling(window, min_periods=1, center=True).mean()
    return grouped


def infer_time_scale(results_dir: pathlib.Path) -> float:
    """Try to read timeScale from a metadata.json in the results tree."""
    for meta in results_dir.rglob("metadata.json"):
        try:
            obj = json.loads(meta.read_text())
            ts = obj.get("timeScale")
            if ts and float(ts) > 0:
                return float(ts)
        except Exception:
            continue
    return 1.0


def _add_night_shading(ax, max_hours: float):
    """Add night-time shading (22:00-06:00) for each 24-hour day in the plot."""
    labeled = False
    day = 0
    while day * 24 < max_hours:
        # Night window: [day*24+22, day*24+30] (22:00 to 06:00 next day)
        night_start = day * 24 + 22
        night_end = day * 24 + 30  # 06:00 next day
        if night_start < max_hours:
            label = "Night (22-06)" if not labeled else None
            ax.axvspan(night_start, min(night_end, max_hours),
                       alpha=0.06, color="navy", label=label)
            labeled = True
        # Also shade the first night of day 0 (00:00-06:00)
        if day == 0:
            ax.axvspan(0, min(6, max_hours), alpha=0.06, color="navy",
                       label="Night (22-06)" if not labeled else None)
            labeled = True
        day += 1


def _add_day_boundaries(ax, max_hours: float):
    """Add vertical dashed lines at 24-hour boundaries."""
    day = 1
    while day * 24 < max_hours:
        ax.axvline(day * 24, color="gray", linestyle="--", linewidth=0.5, alpha=0.4)
        day += 1


# ---------------------------------------------------------------------------
# Job arrival rate from trace files
# ---------------------------------------------------------------------------

def compute_arrival_rate(results_dir: pathlib.Path, time_scale: float,
                         bin_width_h: float = 0.5) -> tuple[np.ndarray, np.ndarray] | None:
    """Compute job arrival rate (jobs/sim-hour) from trace JSONL files.

    Returns (bin_centers_sim_hours, rate_jobs_per_sim_hour) or None.
    """
    # Find trace files
    traces = sorted(results_dir.rglob("*.jsonl"))
    if not traces:
        # Also check parent directories
        traces = sorted(results_dir.parent.rglob("traces/*.jsonl"))
    if not traces:
        return None

    # Use canonical trace (not baseline-A stripped version)
    trace_path = None
    for t in traces:
        if "canonical" in t.name:
            trace_path = t
            break
    if trace_path is None:
        trace_path = traces[0]

    offsets = []
    with open(trace_path) as f:
        for line in f:
            try:
                rec = json.loads(line)
                if rec.get("type") == "job" or rec.get("kind") == "job":
                    offsets.append(float(rec.get("submitTimeOffsetSec", 0)))
            except Exception:
                continue
    if not offsets:
        return None

    offsets.sort()
    # Convert wall-clock offsets to simulated hours
    sim_hours = np.array([o * time_scale / 3600.0 for o in offsets])
    max_h = sim_hours.max() + bin_width_h
    bins = np.arange(0, max_h + bin_width_h, bin_width_h)
    counts, edges = np.histogram(sim_hours, bins=bins)
    centers = (edges[:-1] + edges[1:]) / 2
    rate = counts / bin_width_h
    return centers, rate


# ---------------------------------------------------------------------------
# Main 3-panel plot: Job Arrivals + Active Jobs + IT Power (exp-04 style)
# ---------------------------------------------------------------------------

def plot_main_panels(by_baseline: dict[str, pd.DataFrame],
                     arrival_data: tuple[np.ndarray, np.ndarray] | None,
                     out_dir: pathlib.Path, sim_hours: float = 48.0):
    """Generate the primary 3-panel figure: arrivals, active jobs, IT power."""
    n_panels = 3
    fig, axes = plt.subplots(n_panels, 1, figsize=(16, 12), sharex=True)
    time_col = "elapsed_sim_hours"

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=sim_hours
    )

    # --- Panel 0: Job Arrival Rate ---
    ax = axes[0]
    if arrival_data is not None:
        centers, rate = arrival_data
        # Clip to max_hours
        mask = centers <= max_hours
        ax.bar(centers[mask], rate[mask], width=0.45, color="#4c78a8",
               alpha=0.7, edgecolor="white", linewidth=0.3, label="Job arrivals")
    ax.set_ylabel("Jobs / sim-hour", fontsize=11)
    ax.set_title("Job Arrival Rate", fontsize=12, fontweight="bold")
    ax.grid(alpha=0.15, axis="y")
    _add_night_shading(ax, max_hours)
    _add_day_boundaries(ax, max_hours)
    ax.legend(loc="upper right", fontsize=9, framealpha=0.7)

    # --- Panel 1: Active Jobs ---
    ax = axes[1]
    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "pods_running" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        ax.plot(df[time_col], df["pods_running"], label=label,
                color=color, linewidth=1.0, alpha=0.85)
    ax.set_ylabel("Active Jobs", fontsize=11)
    ax.set_title("Active Jobs Over Time", fontsize=12, fontweight="bold")
    ax.grid(alpha=0.15)
    _add_night_shading(ax, max_hours)
    _add_day_boundaries(ax, max_hours)
    ax.legend(loc="upper right", fontsize=9, framealpha=0.7)

    # --- Panel 2: IT Power ---
    ax = axes[2]
    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "it_power_w" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        it_kw = df["it_power_w"] / 1000.0
        ax.plot(df[time_col], it_kw, label=label,
                color=color, linewidth=1.0, alpha=0.85)
    ax.set_ylabel("IT Power (kW)", fontsize=11)
    ax.set_title("Total IT Power", fontsize=12, fontweight="bold")
    ax.set_xlabel("Simulated Time (hours)", fontsize=11)
    ax.grid(alpha=0.15)
    _add_night_shading(ax, max_hours)
    _add_day_boundaries(ax, max_hours)
    ax.legend(loc="upper right", fontsize=9, framealpha=0.7)

    fig.suptitle(
        f"Datacenter Simulation: {max_hours:.0f}h — Job Arrivals, Active Jobs & IT Power\n"
        f"Baselines: A (no Joulie), B (static partition), C (queue-aware)",
        fontsize=13, fontweight="bold", y=0.98,
    )
    fig.tight_layout(rect=[0, 0, 1, 0.95])
    path = out_dir / "timeseries_arrivals_jobs_power.png"
    fig.savefig(path, dpi=200)
    plt.close(fig)
    print(f"  wrote {path}")


# ---------------------------------------------------------------------------
# Full power profile panels (original 4-panel + FMU metrics)
# ---------------------------------------------------------------------------

def plot_timeseries_panels(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Generate a multi-panel time-series figure: CPU util, IT power, PUE, facility power."""
    panels = [
        ("cluster_cpu_util", "Cluster CPU Utilization", "", lambda x: x * 100, "%"),
        ("it_power_w", "Total IT Power", "", lambda x: x / 1000, "kW"),
        ("pue", "Power Usage Effectiveness (PUE)", "", lambda x: x, ""),
        ("facility_power_w", "Total Facility Power", "", lambda x: x / 1000, "kW"),
    ]
    fig, axes = plt.subplots(len(panels), 1, figsize=(14, 11), sharex=True)
    time_col = "elapsed_sim_hours"

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=24
    )

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
        _add_night_shading(ax, max_hours)
        _add_day_boundaries(ax, max_hours)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Simulated Time (hours)", fontsize=10)
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
    fig, axes = plt.subplots(len(panels), 1, figsize=(14, 11), sharex=True)
    time_col = "elapsed_sim_hours"

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=24
    )

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
        _add_night_shading(ax, max_hours)
        _add_day_boundaries(ax, max_hours)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Simulated Time (hours)", fontsize=10)
    fig.suptitle("CPU + GPU Power Breakdown Over Time", fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(out_dir / "timeseries_gpu_power.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_gpu_power.png'}")


def plot_energy_cumulative(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Cumulative energy consumption over time."""
    fig, ax = plt.subplots(figsize=(14, 5))
    time_col = "elapsed_sim_hours"

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=24
    )

    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "energy_cumulative_j" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        kwh = df["energy_cumulative_j"] / 3.6e6
        ax.plot(df[time_col], kwh, label=label, color=color, linewidth=1.5, alpha=0.9)

    _add_night_shading(ax, max_hours)
    _add_day_boundaries(ax, max_hours)
    ax.set_xlabel("Simulated Time (hours)", fontsize=10)
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
        ("ambient_temp_c", "Ambient Temperature", lambda x: x, "\u00b0C"),
        ("cooling_power_w", "Cooling Power", lambda x: x / 1000, "kW"),
        ("pue", "PUE", lambda x: x, ""),
    ]
    fig, axes = plt.subplots(len(panels), 1, figsize=(14, 8), sharex=True)
    time_col = "elapsed_sim_hours"

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=24
    )

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
        _add_night_shading(ax, max_hours)
        _add_day_boundaries(ax, max_hours)
        ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    axes[-1].set_xlabel("Simulated Time (hours)", fontsize=10)
    fig.suptitle("Cooling and Ambient Conditions Over Time", fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(out_dir / "timeseries_cooling.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'timeseries_cooling.png'}")


def plot_pue_analysis(by_baseline: dict[str, pd.DataFrame], out_dir: pathlib.Path):
    """Dedicated PUE analysis: PUE over time, PUE vs ambient, PUE distribution."""
    time_col = "elapsed_sim_hours"
    has_pue = any(
        df is not None and not df.empty and "pue" in df.columns
        for df in by_baseline.values()
    )
    if not has_pue:
        return

    fig, axes = plt.subplots(3, 1, figsize=(14, 10))

    max_hours = max(
        (df[time_col].max() for df in by_baseline.values() if not df.empty),
        default=24
    )

    # Panel 1: PUE over simulated time
    ax = axes[0]
    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "pue" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        ax.plot(df[time_col], df["pue"], label=label, color=color, linewidth=1.2, alpha=0.9)
    _add_night_shading(ax, max_hours)
    _add_day_boundaries(ax, max_hours)
    ax.set_ylabel("PUE", fontsize=10)
    ax.set_xlabel("Simulated Time (hours)", fontsize=10)
    ax.set_title("PUE Over Time", fontsize=11)
    ax.grid(alpha=0.15)
    ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    # Panel 2: PUE vs ambient temperature (scatter)
    ax = axes[1]
    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "pue" not in df.columns or "ambient_temp_c" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        ax.scatter(df["ambient_temp_c"], df["pue"], label=label, color=color,
                   alpha=0.3, s=8, edgecolors="none")
    ax.set_xlabel("Ambient Temperature (\u00b0C)", fontsize=10)
    ax.set_ylabel("PUE", fontsize=10)
    ax.set_title("PUE vs Ambient Temperature", fontsize=11)
    ax.grid(alpha=0.15)
    ax.legend(loc="upper left", fontsize=8, framealpha=0.7)

    # Panel 3: PUE distribution (histogram)
    ax = axes[2]
    for baseline in BASELINE_ORDER:
        df = by_baseline.get(baseline)
        if df is None or df.empty or "pue" not in df.columns:
            continue
        label = BASELINE_LABELS.get(baseline, f"Baseline {baseline}")
        color = BASELINE_COLORS.get(baseline, None)
        pue_vals = df["pue"].dropna()
        ax.hist(pue_vals, bins=40, alpha=0.5, color=color, label=label, edgecolor="white", linewidth=0.3)
    ax.set_xlabel("PUE", fontsize=10)
    ax.set_ylabel("Frequency", fontsize=10)
    ax.set_title("PUE Distribution by Baseline", fontsize=11)
    ax.grid(alpha=0.15, axis="y")
    ax.legend(loc="upper right", fontsize=8, framealpha=0.7)

    fig.suptitle("PUE Analysis", fontsize=13, y=0.99)
    fig.tight_layout(rect=[0, 0, 1, 0.97])
    fig.savefig(out_dir / "pue_analysis.png", dpi=200)
    plt.close(fig)
    print(f"  wrote {out_dir / 'pue_analysis.png'}")


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
        # Reconstruct elapsed_sec from the sim-hours column.
        out["elapsed_sec"] = out["elapsed_sim_hours"] * 3600.0 / time_scale
        # Simulated elapsed seconds and hour-of-day.
        out["sim_elapsed_sec"] = out["elapsed_sim_hours"] * 3600.0
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


def print_summary(by_baseline: dict[str, pd.DataFrame], time_scale: float):
    """Print a comparison summary table like exp-04."""
    print(f"\n{'='*80}")
    print(f"TIMESERIES SUMMARY (per-baseline)")
    print(f"{'='*80}")
    print(f"  {'Metric':30s}", end="")
    for b in BASELINE_ORDER:
        if b in by_baseline:
            lbl = BASELINE_LABELS.get(b, b)
            print(f" {lbl:>16s}", end="")
    print()
    print(f"  {'-'*30}", end="")
    for b in BASELINE_ORDER:
        if b in by_baseline:
            print(f" {'-'*16}", end="")
    print()

    metrics = [
        ("Avg IT power (kW)", "it_power_w", "mean", lambda x: x / 1000),
        ("Peak IT power (kW)", "it_power_w", "max", lambda x: x / 1000),
        ("IT power std (kW)", "it_power_w", "std", lambda x: x / 1000),
        ("Avg active jobs", "pods_running", "mean", lambda x: x),
        ("Peak active jobs", "pods_running", "max", lambda x: x),
        ("Avg PUE", "pue", "mean", lambda x: x),
        ("Peak PUE", "pue", "max", lambda x: x),
        ("Avg facility power (kW)", "facility_power_w", "mean", lambda x: x / 1000),
    ]
    for label, col, agg, transform in metrics:
        print(f"  {label:30s}", end="")
        for b in BASELINE_ORDER:
            df = by_baseline.get(b)
            if df is None or df.empty or col not in df.columns:
                continue
            val = transform(getattr(df[col], agg)())
            print(f" {val:16.2f}", end="")
        print()

    # Energy totals (integrate IT power over sim-time)
    print()
    for b in BASELINE_ORDER:
        df = by_baseline.get(b)
        if df is None or df.empty or "it_power_w" not in df.columns:
            continue
        # Each row covers bin_width sim-hours; approximate with dt
        dt_sim_hours = df["elapsed_sim_hours"].diff().median()
        if pd.isna(dt_sim_hours) or dt_sim_hours <= 0:
            dt_sim_hours = 0.1
        it_energy_kwh = (df["it_power_w"] / 1000.0 * dt_sim_hours).sum()
        lbl = BASELINE_LABELS.get(b, b)
        print(f"  Total IT energy ({lbl}): {it_energy_kwh:.1f} kWh")
    print()


def main():
    print(f"results dir: {RESULTS}")
    ts_map = discover_timeseries(RESULTS)
    if not ts_map:
        print("no timeseries.csv files found -- run experiments with updated simulator first",
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
        resampled = resample_and_average(raw, "elapsed_sim_hours", present_cols, bin_width=0.1)
        by_baseline[baseline] = resampled

    if not by_baseline:
        print("no valid timeseries data to plot", file=sys.stderr)
        sys.exit(1)

    PLOTS.mkdir(parents=True, exist_ok=True)

    # Compute arrival rate from trace files
    arrival_data = compute_arrival_rate(RESULTS, time_scale)
    if arrival_data is None:
        # Try parent directory (traces may be alongside results)
        arrival_data = compute_arrival_rate(RESULTS.parent, time_scale)

    # Primary plot: arrivals + active jobs + IT power (exp-04 style)
    plot_main_panels(by_baseline, arrival_data, PLOTS)

    # Additional detail plots
    plot_timeseries_panels(by_baseline, PLOTS)
    plot_gpu_power_panel(by_baseline, PLOTS)
    plot_energy_cumulative(by_baseline, PLOTS)
    plot_cooling_ambient(by_baseline, PLOTS)
    plot_pue_analysis(by_baseline, PLOTS)

    # Export FMU-compatible timeseries for PUE co-simulation
    export_fmu_timeseries(by_baseline, time_scale, RESULTS)

    # Print summary table
    print_summary(by_baseline, time_scale)

    print(f"all timeseries plots written to {PLOTS}")


if __name__ == "__main__":
    main()
