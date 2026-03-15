#!/usr/bin/env python3
"""
Collect and aggregate results from experiment 04 scenario runs.

Reads per-scenario JSON files from results/ (produced by the Go simulation
or the cluster-based benchmark) and produces summary.csv.

Usage:
  python3 scripts/30_collect.py [results_dir]
"""
import sys
import json
import csv
from pathlib import Path

RESULTS_DIR = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).parent.parent / "results"

SCENARIOS = ["A", "B", "C"]
FIELDS = [
    "scenario", "energy_kwh", "makespan_s", "peak_cooling_pct",
    "avg_cooling_pct", "peak_psu_pct", "eco_pct",
    "p50_completion_s", "p95_completion_s", "edp",
    "rescheduled_jobs", "reschedule_overhead_s",
]


def load_scenario_simulation(scenario):
    """Load from Go simulation output (scenario_X_metrics.json)."""
    path = RESULTS_DIR / f"scenario_{scenario}_metrics.json"
    if not path.exists():
        return None
    with open(path) as f:
        d = json.load(f)
    return {
        "scenario": d.get("Scenario", scenario),
        "energy_kwh": round(d.get("TotalEnergyKWh", 0), 2),
        "makespan_s": round(d.get("MakespanS", 0)),
        "peak_cooling_pct": round(d.get("PeakCoolingStress", 0), 1),
        "avg_cooling_pct": round(d.get("AvgCoolingStress", 0), 1),
        "peak_psu_pct": round(d.get("PeakPSUStress", 0), 1),
        "eco_pct": round(d.get("EcoPct", 0), 1),
        "p50_completion_s": round(d.get("P50CompletionS", 0)),
        "p95_completion_s": round(d.get("P95CompletionS", 0)),
        "edp": f"{d.get('EnergyDelayProduct', 0):.2e}",
        "rescheduled_jobs": d.get("RescheduledJobs", 0),
        "reschedule_overhead_s": round(d.get("TotalReschedulingOverheadS", 0)),
    }


def load_scenario_cluster(scenario):
    """Load from cluster-based benchmark (scenario_X/metrics.json)."""
    path = RESULTS_DIR / f"scenario_{scenario}" / "metrics.json"
    if not path.exists():
        return None
    with open(path) as f:
        d = json.load(f)
    return {
        "scenario": f"{scenario}: cluster-based",
        "energy_kwh": round(d.get("energy_kwh", 0), 2),
        "makespan_s": round(d.get("makespan_s", 0)),
        "peak_cooling_pct": round(d.get("peak_cooling_pct", 0), 1),
        "avg_cooling_pct": round(d.get("avg_cooling_pct", 0), 1),
        "peak_psu_pct": round(d.get("peak_psu_pct", 0), 1),
        "eco_pct": round(d.get("eco_pct", 0), 1),
        "p50_completion_s": round(d.get("p50_completion_s", 0)),
        "p95_completion_s": round(d.get("p95_completion_s", 0)),
        "edp": f"{d.get('edp', 0):.2e}",
        "rescheduled_jobs": d.get("rescheduled_jobs", 0),
        "reschedule_overhead_s": round(d.get("reschedule_overhead_s", 0)),
    }


def load_scenario(scenario):
    """Try simulation output first, then cluster-based output."""
    row = load_scenario_simulation(scenario)
    if row is not None:
        return row
    row = load_scenario_cluster(scenario)
    if row is not None:
        return row
    print(f"  WARNING: no results found for scenario {scenario}, skipping.")
    return None


def main():
    print(f"Collecting from: {RESULTS_DIR}")
    rows = []
    for sc in SCENARIOS:
        row = load_scenario(sc)
        if row:
            rows.append(row)

    if not rows:
        print("No results found. Run the simulation or cluster benchmark first:")
        print("  go run ./experiments/04-heterogeneous_cluster_control_loop/")
        print("  # or")
        print("  bash experiments/04-heterogeneous_cluster_control_loop/scripts/20_run_scenarios.sh")
        sys.exit(1)

    out_path = RESULTS_DIR / "summary.csv"
    with open(out_path, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=FIELDS)
        w.writeheader()
        w.writerows(rows)

    print(f"\nSummary written to: {out_path}")
    print()
    # Print table
    header = f"{'Scenario':<45} {'Energy(kWh)':>12} {'Makespan(s)':>12} {'PeakCool(%)':>12} {'EcoPct(%)':>10} {'EDP':>12}"
    print(header)
    print("-" * len(header))
    baseline_e = rows[0]["energy_kwh"] if rows else 1
    baseline_edp = float(rows[0]["edp"]) if rows else 1
    for r in rows:
        savings = (baseline_e - r["energy_kwh"]) / baseline_e * 100 if baseline_e > 0 else 0
        print(f"{r['scenario']:<45} {r['energy_kwh']:>12.2f} {r['makespan_s']:>12} "
              f"{r['peak_cooling_pct']:>12.1f} {r['eco_pct']:>10.1f}  ({savings:+.0f}% energy)")


if __name__ == "__main__":
    main()
