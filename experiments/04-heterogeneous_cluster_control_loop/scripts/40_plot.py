#!/usr/bin/env python3
"""
Plot experiment 04 results from summary.csv.

Usage:
  python3 scripts/40_plot.py [results_dir]

Produces:
  results/plots/energy_vs_makespan.png
  results/plots/cooling_stress.png
  results/plots/edp_comparison.png
  results/plots/relative_improvement.png
"""
import sys
import csv
from pathlib import Path

try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
except ImportError:
    print("matplotlib not available. Install with: pip install matplotlib")
    sys.exit(1)

RESULTS_DIR = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).parent.parent / "results"
PLOTS_DIR = RESULTS_DIR / "plots"
PLOTS_DIR.mkdir(parents=True, exist_ok=True)

summary_path = RESULTS_DIR / "summary.csv"
if not summary_path.exists():
    print(f"summary.csv not found at {summary_path}. Run 30_collect.py first.")
    sys.exit(1)

with open(summary_path) as f:
    rows = list(csv.DictReader(f))

if not rows:
    print("No data in summary.csv")
    sys.exit(1)

scenarios = [r["scenario"].split(":")[0].strip() for r in rows]
energy = [float(r["energy_kwh"]) for r in rows]
makespan = [int(r["makespan_s"]) / 3600 for r in rows]  # hours
peak_cool = [float(r["peak_cooling_pct"]) for r in rows]
avg_cool = [float(r.get("avg_cooling_pct", 0)) for r in rows]
peak_psu = [float(r["peak_psu_pct"]) for r in rows]
edp = [float(r["edp"]) for r in rows]

colors = {"A": "#666666", "B": "#e67e22", "C": "#2980b9", "D": "#27ae60"}
bar_colors = [colors.get(s, "#999") for s in scenarios]

# --- Plot 1: Energy vs Makespan bar chart ---
fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(12, 5))
fig.suptitle("Experiment 04: Joulie Control Loop -- Energy & Makespan", fontsize=13)

bars1 = ax1.bar(scenarios, energy, color=bar_colors)
ax1.set_ylabel("Total Energy (kWh)")
ax1.set_title("Energy Consumption")
baseline_e = energy[0] if energy else 1
for bar, val in zip(bars1, energy):
    pct = (baseline_e - val) / baseline_e * 100
    label = f"{val:.0f}\n({pct:+.0f}%)" if pct != 0 else f"{val:.0f}"
    ax1.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 1,
             label, ha="center", va="bottom", fontsize=9)

bars2 = ax2.bar(scenarios, makespan, color=bar_colors)
ax2.set_ylabel("Makespan (hours)")
ax2.set_title("Total Makespan")
baseline_m = makespan[0] if makespan else 1
for bar, val in zip(bars2, makespan):
    pct = (val - baseline_m) / baseline_m * 100
    label = f"{val:.1f}h\n({pct:+.0f}%)" if pct != 0 else f"{val:.1f}h"
    ax2.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.02,
             label, ha="center", va="bottom", fontsize=9)

plt.tight_layout()
out = PLOTS_DIR / "energy_vs_makespan.png"
plt.savefig(out, dpi=150)
print(f"  Saved: {out}")
plt.close()

# --- Plot 2: Cooling stress comparison ---
fig, ax = plt.subplots(figsize=(10, 5))
x = range(len(scenarios))
w = 0.25
ax.bar([i - w for i in x], peak_cool, w, label="Peak Cooling Stress (%)", color="#e74c3c", alpha=0.85)
ax.bar([i for i in x], avg_cool, w, label="Avg Cooling Stress (%)", color="#f39c12", alpha=0.85)
ax.bar([i + w for i in x], peak_psu, w, label="Peak PSU Stress (%)", color="#8e44ad", alpha=0.85)
ax.set_xticks(list(x))
ax.set_xticklabels([r["scenario"] for r in rows], fontsize=8)
ax.set_ylabel("Stress (%)")
ax.set_title("Cooling and PSU Stress by Scenario")
ax.legend()
ax.set_ylim(0, max(max(peak_cool, default=10), max(peak_psu, default=10), max(avg_cool, default=10)) * 1.3 + 5)
plt.tight_layout()
out = PLOTS_DIR / "cooling_stress.png"
plt.savefig(out, dpi=150)
print(f"  Saved: {out}")
plt.close()

# --- Plot 3: EDP comparison ---
fig, ax = plt.subplots(figsize=(8, 5))
bars = ax.bar(scenarios, edp, color=bar_colors)
ax.set_ylabel("Energy-Delay Product (J*s)")
ax.set_title("Energy-Delay Product (EDP) by Scenario\n(lower is better)")
ax.ticklabel_format(style="scientific", axis="y", scilimits=(0, 0))
baseline_edp = edp[0] if edp else 1
for bar, val in zip(bars, edp):
    pct = (baseline_edp - val) / baseline_edp * 100
    label = f"{pct:+.0f}%" if pct != 0 else "baseline"
    ax.text(bar.get_x() + bar.get_width()/2, bar.get_height(),
             label, ha="center", va="bottom", fontsize=9)
plt.tight_layout()
out = PLOTS_DIR / "edp_comparison.png"
plt.savefig(out, dpi=150)
print(f"  Saved: {out}")
plt.close()

# --- Plot 4: Relative improvement vs baseline A ---
if len(rows) > 1:
    fig, ax = plt.subplots(figsize=(10, 6))

    metrics_to_plot = []
    for i, r in enumerate(rows[1:], 1):
        s = scenarios[i]
        e_save = (baseline_e - energy[i]) / baseline_e * 100
        m_delta = (makespan[i] - baseline_m) / baseline_m * 100
        c_save = (peak_cool[0] - peak_cool[i]) / peak_cool[0] * 100 if peak_cool[0] > 0 else 0
        edp_save = (edp[0] - edp[i]) / edp[0] * 100 if edp[0] > 0 else 0
        metrics_to_plot.append({
            "scenario": s,
            "energy_savings": e_save,
            "makespan_delta": -m_delta,  # positive = improvement
            "cooling_savings": c_save,
            "edp_savings": edp_save,
        })

    group_labels = [m["scenario"] for m in metrics_to_plot]
    x = range(len(group_labels))
    w = 0.2

    e_vals = [m["energy_savings"] for m in metrics_to_plot]
    m_vals = [m["makespan_delta"] for m in metrics_to_plot]
    c_vals = [m["cooling_savings"] for m in metrics_to_plot]
    edp_vals = [m["edp_savings"] for m in metrics_to_plot]

    ax.bar([i - 1.5*w for i in x], e_vals, w, label="Energy savings (%)", color="#27ae60", alpha=0.85)
    ax.bar([i - 0.5*w for i in x], m_vals, w, label="Makespan improvement (%)", color="#2980b9", alpha=0.85)
    ax.bar([i + 0.5*w for i in x], c_vals, w, label="Cooling stress reduction (%)", color="#e74c3c", alpha=0.85)
    ax.bar([i + 1.5*w for i in x], edp_vals, w, label="EDP improvement (%)", color="#8e44ad", alpha=0.85)

    ax.set_xticks(list(x))
    ax.set_xticklabels(group_labels)
    ax.set_ylabel("Improvement vs Baseline A (%)")
    ax.set_title("Relative Improvement vs Baseline A (Scenario with No Joulie)")
    ax.axhline(0, color="#888888", linewidth=0.8)
    ax.legend(loc="best", fontsize=8)
    ax.grid(axis="y", alpha=0.2)
    plt.tight_layout()
    out = PLOTS_DIR / "relative_improvement.png"
    plt.savefig(out, dpi=150)
    print(f"  Saved: {out}")
    plt.close()

print(f"\nAll plots saved to: {PLOTS_DIR}")
