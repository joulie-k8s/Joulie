#!/usr/bin/env python3
"""Draw the figures of REPORT.md and print its tables from one sweep.

Reads RESULTS_DIR, a sweep's results directory: one run directory per
baseline and seed (*_b<A|B|C>_s<seed>) with its timeseries.csv,
work_progress.csv, completion.json and metadata.json. Each run lasted the
arrival window and was stopped before the drain; every number here is taken
at regime, from R to W wall seconds after the start (run.measure_from_sec
and run.timeout of CONFIG, the sweep's benchmark config). Writes to OUT_DIR:
  - timeseries.png: IT power, CPU and GPU utilization, running pods (seed 1)
  - energy_comparison.png: energy per unit of work at regime, every seed
  - cumulative_energy.png: energy and work done over simulated time (seed 1)
  - utilization_summary.png: CPU and GPU utilization at regime (seed 1)
  - pue_analysis.png: IT and cooling power, PUE and ambient (seed 1)
  - facility_power_breakdown.png: average IT and cooling power at regime
and prints the report's tables as markdown, with every number used.

Energy and work come from the same polls of the simulator
(work_progress.csv): its cumulative IT energy, and the CPU and GPU work
units the jobs completed (total minus remaining over every job), both
interpolated at R and W. The decisive metric, chosen before any run, is
energy per unit of work: with GPU work in the trace, the energy of the
nodes with GPUs (energy_by_node.jsonl, nodes from sim_debug_nodes.json) per
GPU unit, with the energy of the CPU-only nodes per unit of the CPU-only
jobs' work beside it; without, all energy per CPU unit. Utilization is the simulator's
capacity-weighted cluster utilization. B and C are compared with A seed by
seed, on the same trace: each seed's change is listed, and with three seeds
or more also their mean and 95 % confidence interval (Student t).

At regime means after the ramp-up, which the prefill of the trace removes,
and before the drain, which the run never reaches. Each run's backlog, its
work in progress (the binding work of the jobs submitted so far, from its
trace.jsonl, less the work done) at R and at W, is listed: a policy that
slows jobs ends the window with more of it. The pending jobs (submitted, on
no node yet) of each run are listed too, in all, per pool and of performance
jobs; they include the few seconds of placement every job takes, so a
policy's cost to performance jobs is its difference against A. A baseline
run again on a seed it already ran (the sweep's repeat) is not averaged in:
its difference from the first run is the run-to-run noise. Each run's host
load (1-minute load average, completion.json) is listed next to it. Time is
simulated time: wall seconds times the run's time scale.

Usage:
    python experiments/kwok-cluster/report_figures.py RESULTS_DIR OUT_DIR "CPU-Only (40 nodes)" CONFIG

A run that failed the gates 04_run_one.py applied as it ended is refused
with the reason: completion.json lists in gate_failures a window its polls
do not cover for every run, and the mean queue over the window and the
utilization for baseline A, and in control_errors the control commands the
simulator refused. A run whose mean 1-minute host load exceeds 10 is refused
too, except the repeat of the first run, which is left out of the noise
table with the reason. Each run's performance node count comes from the
plans the controller manager logged. Energy and work are given in simulated
terms (wall values times the time scale), so energy per unit is the same in
either.
"""
import csv
import datetime as dt
import json
import math
import pathlib
import re
import statistics
import sys

import matplotlib
import yaml

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

NAMES = {"A": "A (No Management)", "B": "B (Static Partition)", "C": "C (Queue-Aware)"}
SHORT = {"A": "A (no mgmt)", "B": "B (static)", "C": "C (queue-aware)"}
COLORS = {"A": "#e74c3c", "B": "#3498db", "C": "#2ecc71"}
# Two-sided 95 % Student t quantiles by degrees of freedom.
T95 = {1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365, 8: 2.306, 9: 2.262}


def at(xs, ys, x):
    """ys linearly interpolated at x over the increasing xs."""
    for i in range(1, len(xs)):
        if xs[i] >= x:
            f = (x - xs[i - 1]) / ((xs[i] - xs[i - 1]) or 1)
            return ys[i - 1] + f * (ys[i] - ys[i - 1])
    return ys[-1]


def csvcols(path):
    rows = list(csv.DictReader(open(path)))
    return {k: [float(r[k]) for r in rows] for k in rows[0] if k != "timestamp_utc"}


# Repeats left out for their load, with the reason, for the noise section.
SKIPPED = []


def load(results):
    SKIPPED.clear()
    runs, repeats = {}, {}
    for d in sorted(results.glob("*_b?_s*")):
        m = re.search(r"_b([ABC])_s(\d+)$", d.name)
        if not m or not d.is_dir():
            continue
        is_repeat = (m.group(1), int(m.group(2))) in runs
        target = repeats if is_repeat else runs
        done = json.load(open(d / "completion.json"))
        # A repeat only estimates noise: one that failed the load gate alone
        # is left out, and the report says why.
        others = [g for g in (done.get("gate_failures") or []) if not g.startswith("mean host load")]
        if is_repeat and done["host_load1_mean"] > done["host_cpus"] and not others and not done.get("control_errors"):
            SKIPPED.append(f"the repeat of {SHORT[m.group(1)]} seed {m.group(2)} ({d.name}) ran at a mean host load of {done['host_load1_mean']:.1f}, above the {done['host_cpus']} CPUs, and is left out")
            continue
        if done.get("control_errors") or done.get("gate_failures"):
            sys.exit(f"{d.name} failed the gates: {done.get('gate_failures') or ''} {done.get('control_errors') or ''} refused controls")
        # The load gate of 04_run_one.py: any other run above it is refused.
        if done["host_load1_mean"] > done["host_cpus"]:
            sys.exit(f"{d.name}: mean host load {done['host_load1_mean']:.1f} exceeds the {done['host_cpus']} CPUs: run it again")
        scale = float(json.load(open(d / "metadata.json")).get("timeScale", 1.0))
        ts = csvcols(d / "timeseries.csv")
        ts["sim_h"] = [t * scale / 3600.0 for t in ts["elapsed_sec"]]
        jobs = [json.loads(l) for l in open(d / "trace.jsonl") if '"type":"job"' in l]
        nodes = json.load(open(d / "sim_debug_nodes.json"))
        nodes = nodes if isinstance(nodes, list) else nodes.get("nodes", [])
        gpu_nodes = {n["node"] for n in nodes if ((n.get("model") or {}).get("GPU") or {}).get("Count", 0) > 0}
        # Where each finished job ran (simulator.log "job completed id=...
        # node=..."): a job without GPUs must not run on a GPU node, or the
        # energy split by node kind measures placement, not caps.
        gpu_jobs = {j["jobId"] for j in jobs if any(k.endswith("gpu") for k in ((j.get("podTemplate") or {}).get("requests") or {}))}
        ran_on = dict(re.search(r"job completed id=(\S+) node=(\S+)", l).groups() for l in open(d / "simulator.log") if "job completed id=" in l) if (d / "simulator.log").exists() else {}
        cpu_jobs_on_gpu_nodes = sum(1 for j, n in ran_on.items() if j not in gpu_jobs and n in gpu_nodes)
        polls = [json.loads(l) for l in open(d / "energy_by_node.jsonl")]
        by_node = {"elapsed_sec": [p["elapsed_sec"] for p in polls],
                   "gpu_nodes_j": [sum(v for k, v in p["byNodeJoules"].items() if k in gpu_nodes) for p in polls],
                   "cpu_nodes_j": [sum(v for k, v in p["byNodeJoules"].items() if k not in gpu_nodes) for p in polls]}
        target[(m.group(1), int(m.group(2)))] = {
            "ts": ts, "work": csvcols(d / "work_progress.csv"), "scale": scale, "jobs": jobs, "by_node": by_node,
            "plans": plans(d),
            "finished": len(ran_on), "cpu_jobs_on_gpu_nodes": cpu_jobs_on_gpu_nodes,
            "load_mean": done["host_load1_mean"], "load_max": done["host_load1_max"], "cpus": done["host_cpus"],
        }
    return runs, repeats


def mean(v):
    return sum(v) / len(v)


def plans(run_dir):
    """The performance node count of each plan the controller manager logged
    ("assigned profiles ... plan=<node>=<profile>(...),..."), as
    [(elapsed_sec, count)], its wall clock mapped onto the run's through the
    first row of timeseries.csv; empty when it logged none."""
    log = run_dir / "controller-manager.log"
    if not log.exists():
        return []
    first = next(csv.DictReader(open(run_dir / "timeseries.csv")))
    t0 = dt.datetime.strptime(first["timestamp_utc"], "%Y-%m-%dT%H:%M:%SZ") - dt.timedelta(seconds=float(first["elapsed_sec"]))
    out = []
    for line in open(log):
        m = re.match(r"(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) .*assigned profiles .*plan=(.*)$", line)
        if m:
            t = (dt.datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S") - t0).total_seconds()
            out.append((t, m.group(2).count("=performance(")))
    return out


def nodes_cell(v):
    return "none" if math.isnan(v) else f"{v:.1f}"


def held_mean(events, R, W):
    """Time-weighted mean from R to W of a step function given as
    [(time, value)], each value held until the next; nan before the first."""
    events = sorted(events)
    if not events or events[0][0] > R:
        return float("nan")
    total = 0.0
    for (t, v), nxt in zip(events, events[1:] + [(W, None)]):
        a, b = max(t, R), min(nxt[0], W)
        if b > a:
            total += v * (b - a)
    return total / (W - R)


def ci95(v):
    """Half width of the 95 % confidence interval of the mean of v."""
    return T95.get(len(v) - 1, 1.96) * statistics.stdev(v) / math.sqrt(len(v))


def wip(r, gpu_work):
    """The run's work in progress at each poll: the binding work of the jobs
    submitted by then (trace offsets) less the work done."""
    key, col = ("gpuUnits", "gpu_units_done") if gpu_work else ("cpuUnits", "cpu_units_done")
    sub = sorted((float(j["submitTimeOffsetSec"]), float(j["work"].get(key) or 0)) for j in r["jobs"])
    out, acc, i = [], 0.0, 0
    for t, done in zip(r["work"]["elapsed_sec"], r["work"][col]):
        while i < len(sub) and sub[i][0] <= t:
            acc += sub[i][1]
            i += 1
        out.append(acc - done)
    return out


def run_metrics(r, R, W, gpu_work):
    ts, wk, scale = r["ts"], r["work"], r["scale"]
    t, wt = ts["elapsed_sec"], wk["elapsed_sec"]

    def span(col):
        return at(wt, wk[col], W) - at(wt, wk[col], R)

    e_j = span("energy_j")
    backlog = wip(r, gpu_work)
    bn, bt = r["by_node"], r["by_node"]["elapsed_sec"]
    e_gpu_nodes = at(bt, bn["gpu_nodes_j"], W) - at(bt, bn["gpu_nodes_j"], R)
    e_cpu_nodes = at(bt, bn["cpu_nodes_j"], W) - at(bt, bn["cpu_nodes_j"], R)
    cpu_only = span("cpu_only_units_done")
    cpu_work, gpu_work_done = span("cpu_units_done"), span("gpu_units_done")
    work = gpu_work_done if gpu_work else cpu_work
    win = [i for i, x in enumerate(t) if R <= x <= W]
    return {
        "energy_kwh": e_j * scale / 3.6e6,
        "it_w": e_j / (W - R),
        "cpu_work": cpu_work * scale,
        "gpu_work": gpu_work_done * scale,
        "work": work * scale,
        "j_per_unit": (e_gpu_nodes if gpu_work else e_j) / work if work > 0 else float("nan"),
        "cpu_node_j_per_unit": e_cpu_nodes / cpu_only if gpu_work and cpu_only > 0 else float("nan"),
        "cpu_only_work": cpu_only * scale,
        "pending_perf": mean([p for p, x in zip(wk["pending_perf"], wt) if R <= x <= W]),
        "pending_8gpu": mean([p for p, x in zip(wk["pending_8gpu"], wt) if R <= x <= W]),
        "pending_pools": " / ".join(f"{mean([p for p, x in zip(wk[c], wt) if R <= x <= W]):.1f}" for c in ("pending_nvidia", "pending_amd", "pending_cpu")),
        "cool_w": mean([ts["cooling_power_w"][i] for i in win]),
        "pue": mean([ts["pue"][i] for i in win]),
        "cpu_util": 100 * mean([ts["cluster_cpu_util"][i] for i in win]),
        "gpu_util": 100 * mean([ts["cluster_gpu_util"][i] for i in win]),
        "pods_avg": mean([ts["pods_running"][i] for i in win]),
        "perf_nodes": held_mean(r["plans"], R, W),
        "finished": r["finished"], "cpu_jobs_on_gpu_nodes": r["cpu_jobs_on_gpu_nodes"],
        "ambient_min": min(ts["ambient_temp_c"][i] for i in win),
        "ambient_max": max(ts["ambient_temp_c"][i] for i in win),
        "load_mean": r["load_mean"], "load_max": r["load_max"],
        "backlog_start": at(wt, backlog, R) * scale,
        "backlog_end": at(wt, backlog, W) * scale,
        "pending_avg": mean([p for p, x in zip(wk["jobs_pending"], wt) if R <= x <= W]),
        "pending_share": 100 * mean([1.0 if p > 0 else 0.0 for p, x in zip(wk["jobs_pending"], wt) if R <= x <= W]),
    }


def figures(runs, m, out, title, R, W, gpu_work):
    baselines = sorted({b for b, _ in runs})
    seeds = sorted({s for _, s in runs})
    first = {b: runs[(b, seeds[0])] for b in baselines}
    scale = first[baselines[0]]["scale"]
    rh, wh = R * scale / 3600.0, W * scale / 3600.0
    gpu = any(max(r["ts"]["cluster_gpu_util"]) > 0 for r in first.values())

    def shade(ax):
        ax.axvspan(rh, wh, color="grey", alpha=0.08, lw=0)

    panels = [("it_power_w", "IT Power (kW)", 1e-3), ("cluster_cpu_util", "CPU Utilization (%)", 100)]
    if gpu:
        panels.append(("cluster_gpu_util", "GPU Utilization (%)", 100))
    panels.append(("pods_running", "Running Pods", 1))
    fig, axes = plt.subplots(len(panels), 1, figsize=(14, 3.3 * len(panels)), sharex=True)
    for ax, (k, label, f) in zip(axes, panels):
        for b in baselines:
            ts = first[b]["ts"]
            ax.plot(ts["sim_h"], [v * f for v in ts[k]], color=COLORS[b], lw=0.9, label=NAMES[b])
        shade(ax)
        ax.set_ylabel(label)
        ax.grid(alpha=0.3)
    axes[0].legend(loc="upper left", fontsize=9)
    axes[-1].set_xlabel(f"Simulated time (h), seed {seeds[0]}; shaded: at regime, measured")
    fig.suptitle(f"{title} - KWOK Experiment", fontweight="bold")
    fig.tight_layout()
    fig.savefig(out / "timeseries.png", dpi=150)
    plt.close(fig)

    unit = "GPU" if gpu_work else "CPU"
    fig, ax = plt.subplots(figsize=(8, 5))
    for i, b in enumerate(baselines):
        vals = [m[(b, s)]["j_per_unit"] for s in seeds]
        ax.bar(i, mean(vals), color=COLORS[b], width=0.55)
        ax.scatter([i] * len(vals), vals, color="black", s=14, zorder=3)
        ax.text(i, mean(vals), f"{mean(vals):.2f} J", ha="center", va="bottom", fontweight="bold")
    ax.set_xticks(range(len(baselines)), [NAMES[b] for b in baselines])
    ax.set_ylabel(f"{'GPU-node energy' if gpu_work else 'IT energy'} per {unit} work unit (J)")
    ax.set_title(f"{title} - Energy per Unit of Work at Regime ({len(seeds)} seeds)")
    ax.grid(axis="y", alpha=0.3)
    fig.tight_layout()
    fig.savefig(out / "energy_comparison.png", dpi=150)
    plt.close(fig)

    col = "gpu_units_done" if gpu_work else "cpu_units_done"
    fig, axes = plt.subplots(2, 1, figsize=(10, 7), sharex=True)
    for b in baselines:
        ts, wk = first[b]["ts"], first[b]["work"]
        axes[0].plot(ts["sim_h"], [j * scale / 3.6e6 for j in ts["energy_cumulative_j"]], color=COLORS[b], lw=1.5, label=NAMES[b])
        axes[1].plot([t * scale / 3600.0 for t in wk["elapsed_sec"]], [v * scale for v in wk[col]], color=COLORS[b], lw=1.5)
    for ax in axes:
        shade(ax)
        ax.grid(alpha=0.3)
    axes[0].set_ylabel("Cumulative IT energy (kWh)")
    axes[1].set_ylabel(f"{unit} work units done (simulated)")
    axes[1].set_xlabel(f"Simulated time (h), seed {seeds[0]}")
    axes[0].legend()
    fig.suptitle(f"{title} - Energy and Work (KWOK)", fontweight="bold")
    fig.tight_layout()
    fig.savefig(out / "cumulative_energy.png", dpi=150)
    plt.close(fig)

    kinds = [("cluster_cpu_util", "CPU")] + ([("cluster_gpu_util", "GPU")] if gpu else [])
    fig, axes = plt.subplots(1, len(kinds), figsize=(7 * len(kinds), 4.5), squeeze=False)
    for ax, (k, label) in zip(axes[0], kinds):
        for b in baselines:
            ts = first[b]["ts"]
            v = [100 * x for x, t in zip(ts[k], ts["elapsed_sec"]) if R <= t <= W]
            ax.hist(v, bins=50, range=(0, 100), alpha=0.45, color=COLORS[b], label=NAMES[b])
        ax.set_title(f"{label} Utilization at Regime, seed {seeds[0]}")
        ax.set_xlabel(f"{label} Utilization (%)")
        ax.set_ylabel("Samples")
        ax.grid(alpha=0.3)
        ax.legend(fontsize=9)
    fig.suptitle(f"{title} - KWOK", fontweight="bold")
    fig.tight_layout()
    fig.savefig(out / "utilization_summary.png", dpi=150)
    plt.close(fig)

    fig, axes = plt.subplots(3, 1, figsize=(14, 9), sharex=True)
    for b in baselines:
        ts = first[b]["ts"]
        axes[0].plot(ts["sim_h"], [v / 1e3 for v in ts["it_power_w"]], color=COLORS[b], lw=0.9, label=NAMES[b])
        axes[0].plot(ts["sim_h"], [v / 1e3 for v in ts["cooling_power_w"]], color=COLORS[b], lw=0.9, ls="--")
        axes[1].plot(ts["sim_h"], ts["pue"], color=COLORS[b], lw=0.9)
        axes[2].plot(ts["sim_h"], ts["ambient_temp_c"], color=COLORS[b], lw=0.9)
    for ax in axes:
        shade(ax)
        ax.grid(alpha=0.3)
    axes[0].set_ylabel("Power (kW)\nsolid IT, dashed cooling")
    axes[1].set_ylabel("PUE")
    axes[2].set_ylabel("Ambient (°C)")
    axes[2].set_xlabel(f"Simulated time (h), seed {seeds[0]}")
    axes[0].legend(fontsize=9)
    fig.suptitle(f"{title} - IT Power, Cooling and PUE (KWOK)", fontweight="bold")
    fig.tight_layout()
    fig.savefig(out / "pue_analysis.png", dpi=150)
    plt.close(fig)

    fig, ax = plt.subplots(figsize=(8, 5))
    it = [mean([m[(b, s)]["it_w"] for s in seeds]) for b in baselines]
    cool = [mean([m[(b, s)]["cool_w"] for s in seeds]) for b in baselines]
    x = [NAMES[b] for b in baselines]
    ax.bar(x, it, color=[COLORS[b] for b in baselines], width=0.5, label="IT Power")
    ax.bar(x, cool, bottom=it, color=[COLORS[b] for b in baselines], alpha=0.5, width=0.5, label="Cooling Power")
    for i in range(len(baselines)):
        ax.text(i, it[i] + cool[i], f"{it[i] + cool[i]:,.0f} W", ha="center", va="bottom", fontweight="bold")
    ax.set_ylabel("Average power at regime (W)")
    ax.set_title(f"{title} - Facility Power Breakdown ({len(seeds)} seeds)")
    ax.grid(axis="y", alpha=0.3)
    ax.legend()
    fig.tight_layout()
    fig.savefig(out / "facility_power_breakdown.png", dpi=150)
    plt.close(fig)


def tables(runs, m, repeats, R, W, gpu_work):
    baselines = sorted({b for b, _ in runs})
    seeds = sorted({s for _, s in runs})
    unit = "GPU" if gpu_work else "CPU"
    cpus = max(r["cpus"] for r in runs.values())
    print(f"At regime, from {R:.0f} to {W:.0f} wall seconds, per run ({len(seeds)} seeds):\n")
    print(f"| Baseline | Seed | IT energy (kWh, simulated) | Avg IT power (W) | CPU work units (simulated) | GPU work units (simulated) | J per {unit} unit | CPU util (%) | GPU util (%) | Avg pods | Performance nodes (mean) | Backlog at start / end ({unit} units, simulated) | Pending jobs avg / polls with any (%) | Pending avg NVIDIA / AMD / CPU | Pending performance jobs avg | Pending 8-GPU jobs avg | Jobs finished / of them without GPUs on GPU nodes | Host load mean / max ({cpus} CPUs) |")
    print("|---|---:|" + "---:|" * 16)
    for b in baselines:
        for s in seeds:
            x = m[(b, s)]
            print(f"| {SHORT[b]} | {s} | {x['energy_kwh']:,.1f} | {x['it_w']:,.0f} | {x['cpu_work']:,.0f} | {x['gpu_work']:,.0f} | {x['j_per_unit']:.3f} | {x['cpu_util']:.1f} | {x['gpu_util']:.1f} | {x['pods_avg']:.1f} | {nodes_cell(x['perf_nodes'])} | {x['backlog_start']:,.0f} / {x['backlog_end']:,.0f} | {x['pending_avg']:.1f} / {x['pending_share']:.0f} | {x['pending_pools']} | {x['pending_perf']:.1f} | {x['pending_8gpu']:.1f} | {x['finished']} / {x['cpu_jobs_on_gpu_nodes']} | {x['load_mean']:.1f} / {x['load_max']:.1f} |")

    print("\nPer baseline, mean over seeds:\n")
    print("| Baseline | Avg IT power (W) | J per unit | Avg PUE | Avg cooling (W) | CPU util (%) | GPU util (%) | Performance nodes | Pending performance jobs | Ambient (°C) |")
    print("|---|" + "---:|" * 9)
    for b in baselines:
        xs = [m[(b, s)] for s in seeds]
        a = {k: mean([x[k] for x in xs]) for k, v in xs[0].items() if isinstance(v, (int, float))}
        amb = f"{min(x['ambient_min'] for x in xs):.1f} to {max(x['ambient_max'] for x in xs):.1f}"
        print(f"| {SHORT[b]} | {a['it_w']:,.0f} | {a['j_per_unit']:.3f} | {a['pue']:.3f} | {a['cool_w']:,.0f} | {a['cpu_util']:.1f} | {a['gpu_util']:.1f} | {nodes_cell(a['perf_nodes'])} | {a['pending_perf']:.1f} | {amb} |")

    paired = len(seeds) >= 3
    how = ", ".join(f"seed {s}" for s in seeds) + (", mean and 95 % CI" if paired else "")
    print(f"\nChange against A, paired by seed ({how}):\n")
    keys = [("it_w", "Avg IT power"), ("cpu_work", "CPU work done")] + ([("gpu_work", "GPU work done")] if gpu_work else [])
    keys += [("j_per_unit", "GPU-node energy per GPU unit" if gpu_work else "Energy per CPU unit")]
    keys += [("cpu_node_j_per_unit", "CPU-only-node energy per CPU-only unit")] if gpu_work else []
    keys += [("backlog_end", "Backlog at end")]
    print("| Baseline | " + " | ".join(f"{label} (%)" for _, label in keys) + " |")
    print("|---|" + "---:|" * len(keys))
    for b in baselines:
        if b == "A":
            continue
        cells = []
        for k, _ in keys:
            d = [100 * (m[(b, s)][k] / m[("A", s)][k] - 1) for s in seeds]
            cell = " / ".join(f"{v:+.1f}" for v in d)
            if paired:
                cell += f" (mean {mean(d):+.1f} ± {ci95(d):.1f})"
            cells.append(cell)
        print(f"| {SHORT[b]} | " + " | ".join(cells) + " |")
    for note in SKIPPED:
        print(f"\nRun-to-run noise: {note}.")
    if repeats:
        print("\nRun-to-run noise, the same baseline and seed run twice (second run against the first), with the host load of each, since the host is shared:\n")
        print("| Baseline | Seed | " + " | ".join(f"{label} (%)" for _, label in keys) + f" | Host load mean, first / second ({cpus} CPUs) |")
        print("|---|---:|" + "---:|" * (len(keys) + 1))
        for (b, s), x in sorted(repeats.items()):
            print(f"| {SHORT[b]} | {s} | " + " | ".join(f"{100 * (x[k] / m[(b, s)][k] - 1):+.1f}" for k, _ in keys) + f" | {m[(b, s)]['load_mean']:.1f} / {x['load_mean']:.1f} |")


def main():
    if len(sys.argv) != 5:
        sys.exit(__doc__)
    results, out, title = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
    run_cfg = yaml.safe_load(open(sys.argv[4]))["run"]
    R, W = float(run_cfg["measure_from_sec"]), float(run_cfg["timeout"])
    out.mkdir(parents=True, exist_ok=True)
    runs, repeats = load(results)
    if not runs:
        sys.exit(f"{results}: no run directories")
    gpu_work = any(max(r["work"]["gpu_units_done"]) > 0 for r in runs.values())
    m = {k: run_metrics(r, R, W, gpu_work) for k, r in runs.items()}
    rm = {k: run_metrics(r, R, W, gpu_work) for k, r in repeats.items()}
    figures(runs, m, out, title, R, W, gpu_work)
    tables(runs, m, rm, R, W, gpu_work)


if __name__ == "__main__":
    main()
