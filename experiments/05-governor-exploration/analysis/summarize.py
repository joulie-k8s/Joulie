#!/usr/bin/env python3
"""Summarise a govsweep run of experiment 05 as Markdown.

Usage: summarize.py RUN_DIR

Reads every *-cpu.csv and *-gpu.csv in RUN_DIR (govsweep's output) and
prints, for each grid, the energy to solution and the time of every arm
against the grid's baseline arm, per workload kind, as medians and 95th
percentiles over profiles, loads, classes and seeds, then the acceptance
rule of the experiment's README applied to the Tier A half of it.

Baselines: CPU, the performance preset with boost on (race to idle at the
reference frequency); GPU, auto with no cap, in the same profile, workload,
transfer and sensitivity variant. Only cells that ran (status ok) are
compared; a cell whose arm is unsupported on its profile is counted apart.
Standard library only.
"""
import csv
import glob
import math
import os
import statistics
import sys
from collections import defaultdict

CPU_BASE = "performance-boost-on"  # or "performance" in a grid without boost arms
GPU_BASE_PRESET, GPU_BASE_CAP = "auto", "100"
PERF_P95, ECO_P95 = 5.0, 20.0  # percent slowdown the acceptance rule allows


def pct(xs, q):
    if not xs:
        return float("nan")
    s = sorted(xs)
    i = max(0, min(len(s) - 1, math.ceil(q / 100 * len(s)) - 1))
    return s[i]


def fmt(v, digits=1):
    if v is None or (isinstance(v, float) and math.isnan(v)):
        return "n/a"
    return f"{v:+.{digits}f}"


def load(run, grid):
    rows = []
    for p in sorted(glob.glob(os.path.join(run, f"*-{grid}.csv"))):
        with open(p, newline="") as f:
            rows.extend(csv.DictReader(f))
    return rows


def cpu_kind(r):
    if r["load"] == "steady":
        return f"steady {r['pct']} %"
    return r["load"]


def gpu_kind(r):
    if r["load"] == "idle":
        return "idle"
    t = r["transfer"]
    if t == "none":
        return f"{r['gpus']} GPU, no transfer"
    return f"{r['gpus']} GPU, {t} {r['transfer_intensity']}"


def deltas(rows, key, arm_of, base_arm):
    """Energy and time of each arm against the baseline cell with the same key."""
    base = {}
    for r in rows:
        if r["status"] == "ok" and arm_of(r) == base_arm:
            base[key(r)] = r
    out = defaultdict(lambda: {"e": [], "t": [], "n": 0})
    for r in rows:
        # A timeout ran to the limit and its time is a lower bound; it is
        # left out, and listed by the caller.
        if r["status"] != "ok":
            continue
        b = base.get(key(r))
        if b is None:
            continue
        be, bt = float(b["host_j"]), float(b["time_s"])
        if be <= 0 or bt <= 0:
            continue
        d = out[arm_of(r)]
        d["e"].append(100 * (float(r["host_j"]) / be - 1))
        d["t"].append(100 * (float(r["time_s"]) / bt - 1))
        d["n"] += 1
    return out


def table(title, groups, order=None):
    print(f"\n#### {title}\n")
    print("| arm | cells | energy median % | energy p95 % | time median % | time p95 % |")
    print("|---|---|---|---|---|---|")
    for arm in order or sorted(groups):
        d = groups.get(arm)
        if not d or not d["n"]:
            continue
        print(f"| {arm} | {d['n']} | {fmt(statistics.median(d['e']))} | {fmt(pct(d['e'], 95))} | "
              f"{fmt(statistics.median(d['t']))} | {fmt(pct(d['t'], 95))} |")


def cpu_base(rows):
    """The CPU baseline arm: performance with boost on, or plain performance in
    a grid whose arms leave boost alone."""
    arms = {r["arm"] for r in rows}
    return CPU_BASE if CPU_BASE in arms else "performance"


def cpu_section(rows):
    if not rows:
        return {}
    base = cpu_base(rows)
    print("## E-G1: CPU presets\n")
    print(f"Baseline: `{base}` on the same profile, load, class and seed. "
          "Energy is the host's energy to solution (idle: over the same window); time is the job's makespan.")
    arm = lambda r: r["arm"]
    key = lambda r: (r["profile"], r["sens"], r["load"], r["pct"], r["class"], r["seed"])
    arms = sorted({r["arm"] for r in rows})
    order = [a for a in ["default", base] if a in arms] + [a for a in arms if a not in ("default", base)]
    base_rows = [r for r in rows if not r["sens"]]
    table("All loads and profiles", deltas(base_rows, key, arm, base), order)
    if "default" in arms:
        table("All loads and profiles, against the profile's own governor (`default`, nothing written)",
              deltas(base_rows, key, arm, "default"), order)
    for kind in sorted({cpu_kind(r) for r in base_rows}, key=lambda k: (k[0] != "i", k)):
        table(f"Load: {kind}", deltas([r for r in base_rows if cpu_kind(r) == kind], key, arm, base), order)
    for prof in sorted({r["profile"] for r in base_rows}):
        sub = [r for r in base_rows if r["profile"] == prof and r["load"] == "steady"]
        natives = sorted({f"{r['arm']}: {r['native']} ({r['result'] or 'nothing written'})" for r in sub})
        table(f"Profile {prof}, steady loads ({'; '.join(natives)})", deltas(sub, key, arm, base), order)
    bursty = [r for r in base_rows if r["load"] == "bursty" and r["status"] == "ok"]
    if bursty:
        print("\n#### Bursty load: burst latency (ms, p95 over bursts, median over profiles and seeds)\n")
        print("| arm | p50 ms | p95 ms |")
        print("|---|---|---|")
        by = defaultdict(lambda: ([], []))
        for r in bursty:
            by[r["arm"]][0].append(float(r["burst_p50_ms"]))
            by[r["arm"]][1].append(float(r["burst_p95_ms"]))
        for a in order:
            if a in by:
                print(f"| {a} | {statistics.median(by[a][0]):.0f} | {statistics.median(by[a][1]):.0f} |")
    return deltas(base_rows, key, arm, base)


def gpu_section(rows):
    if not rows:
        return {}
    print("\n## E-G2: GPU presets\n")
    print(f"Baseline: `{GPU_BASE_PRESET}` with no cap on the same profile, workload, transfer, receiver gate and seed.")
    def arm(r):
        a = r["preset"] + (r["ceiling_pct"] if r["ceiling_pct"] else "")
        return f"{a} cap{r['cap_pct']}"
    key = lambda r: (r["profile"], r["sens"], r["gate"], r["load"], r["class"], r["gpus"], r["transfer"], r["transfer_intensity"], r["seed"])
    base_arm = f"{GPU_BASE_PRESET} cap{GPU_BASE_CAP}"
    base_rows = [r for r in rows if not r["sens"]]
    arms = sorted({arm(r) for r in base_rows}, key=lambda a: (a.split()[1], a))
    unsupported = defaultdict(set)
    for r in base_rows:
        if r["status"] == "unsupported":
            unsupported[r["profile"]].add(r["preset"] + (r["ceiling_pct"] or ""))
    if unsupported:
        print("\nArms a profile cannot express (pkg/governor gives unsupported; nothing ran):\n")
        for p in sorted(unsupported):
            print(f"- {p}: {', '.join(sorted(unsupported[p]))}")
    timeouts = defaultdict(int)
    for r in base_rows:
        if r["status"] == "timeout":
            timeouts[(r["profile"], arm(r))] += 1
    if timeouts:
        print("\nCells that did not finish within the limit, left out of the tables:\n")
        for (p, a), n in sorted(timeouts.items()):
            print(f"- {p}, {a}: {n}")
    table("All workloads and profiles", deltas(base_rows, key, arm, base_arm), arms)
    for cls in sorted({r["class"] for r in base_rows if r["class"]}):
        table(f"Class {cls}", deltas([r for r in base_rows if r["class"] == cls], key, arm, base_arm), arms)
    for kind in ("idle", "gather"):
        sub = [r for r in base_rows if (r["load"] == "idle" if kind == "idle" else r["transfer"] == kind)]
        if sub:
            table(f"{'Idle node' if kind == 'idle' else 'Gather jobs (an idle receiver)'}", deltas(sub, key, arm, base_arm), arms)
    for prof in sorted({r["profile"] for r in base_rows}):
        sub = [r for r in base_rows if r["profile"] == prof and r["load"] == "job"]
        table(f"Profile {prof}, jobs", deltas(sub, key, arm, base_arm), arms)
    return deltas(base_rows, key, arm, base_arm)


def sensitivity_section(cpu, gpu):
    sens = sorted({r["sens"] for r in cpu + gpu if r["sens"]})
    if not sens:
        return
    print("\n## Q5: sensitivity\n")
    print("Median energy change against the baseline arm in the same variant; "
          "a conclusion holds only if its sign is the same at every value of a fact.\n")
    print("| variant | grid | arm | energy median % | time median % |")
    print("|---|---|---|---|---|")
    for s in sens:
        for grid, rows in (("cpu", cpu), ("gpu", gpu)):
            sub = [r for r in rows if r["sens"] == s]
            if not sub:
                continue
            if grid == "cpu":
                d = deltas(sub, lambda r: (r["profile"], r["load"], r["pct"], r["class"], r["seed"]), lambda r: r["arm"], cpu_base(sub))
            else:
                arm = lambda r: f"{r['preset']}{r['ceiling_pct']} cap{r['cap_pct']} gate-{r['gate']}"
                key = lambda r: (r["profile"], r["load"], r["class"], r["gpus"], r["transfer"], r["transfer_intensity"], r["seed"], r["gate"])
                base = {}
                for r in sub:
                    if r["status"] == "ok" and r["preset"] == "auto" and r["cap_pct"] == "100":
                        base[key(r)] = r
                d = defaultdict(lambda: {"e": [], "t": [], "n": 0})
                for r in sub:
                    b = base.get(key(r))
                    if r["status"] != "ok" or b is None:
                        continue
                    x = d[arm(r)]
                    x["e"].append(100 * (float(r["host_j"]) / float(b["host_j"]) - 1))
                    x["t"].append(100 * (float(r["time_s"]) / float(b["time_s"]) - 1))
                    x["n"] += 1
            for a in sorted(d):
                if d[a]["n"]:
                    print(f"| {s} | {grid} | {a} | {fmt(statistics.median(d[a]['e']))} | {fmt(statistics.median(d[a]['t']))} |")


def acceptance(cpu_d, gpu_d):
    print("\n## Acceptance rule, Tier A half\n")
    print(f"A candidate default passes Tier A when its median energy change is negative and its p95 slowdown is at "
          f"most {PERF_P95:.0f} % (performance class) or {ECO_P95:.0f} % (eco class). Tier B and Q5 decide the rest.\n")
    print("| candidate | class | energy median % | time p95 % | Tier A |")
    print("|---|---|---|---|---|")
    # The last field marks the baseline arm itself, which is not judged; a
    # candidate that changes nothing fails, since it lowers no energy.
    for name, cls, d, limit, is_base in (
        ("CPU performance (boost on)", "performance", cpu_d.get("performance-boost-on") or cpu_d.get("performance"), PERF_P95, True),
        ("CPU efficiency", "eco", cpu_d.get("efficiency-boost-on") or cpu_d.get("efficiency"), ECO_P95, False),
        ("CPU balanced", "eco", cpu_d.get("balanced-boost-on") or cpu_d.get("balanced"), ECO_P95, False),
        ("CPU efficiency, boost off (CPU_GOVERNOR_ECO_BOOST=off)", "eco", cpu_d.get("efficiency-boost-off"), ECO_P95, False),
        ("GPU efficient 75 %", "eco", gpu_d.get("efficient75 cap100"), ECO_P95, False),
        ("GPU efficient 80 %", "eco", gpu_d.get("efficient80 cap100"), ECO_P95, False),
        ("GPU efficient 90 %", "eco", gpu_d.get("efficient90 cap100"), ECO_P95, False),
        ("GPU efficient 75 % with a 60 % cap", "eco", gpu_d.get("efficient75 cap60"), ECO_P95, False),
        ("GPU sustained", "performance", gpu_d.get("sustained cap100"), PERF_P95, False),
    ):
        if not d or not d["n"]:
            continue
        e, t = statistics.median(d["e"]), pct(d["t"], 95)
        ok = "baseline" if is_base else ("pass" if e < 0 and t <= limit else "fail")
        print(f"| {name} | {cls} | {fmt(e)} | {fmt(t)} | {ok} |")


def main():
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    run = sys.argv[1]
    cpu, gpu = load(run, "cpu"), load(run, "gpu")
    if not cpu and not gpu:
        print(f"summarize.py: no *-cpu.csv or *-gpu.csv in {run}", file=sys.stderr)
        return 1
    print(f"# Experiment 05, Tier A summary\n\nRun: `{os.path.basename(os.path.abspath(run))}`, "
          f"{len(cpu)} CPU cells and {len(gpu)} GPU cells.\n")
    cpu_d = cpu_section(cpu)
    gpu_d = gpu_section(gpu)
    sensitivity_section(cpu, gpu)
    acceptance(cpu_d, gpu_d)
    return 0


if __name__ == "__main__":
    sys.exit(main())
