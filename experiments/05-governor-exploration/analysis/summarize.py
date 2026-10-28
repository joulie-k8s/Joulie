#!/usr/bin/env python3
"""Summarise a govsweep run of experiment 05 as Markdown.

Usage: summarize.py RUN_DIR

Reads every *-cpu.csv and *-gpu.csv in RUN_DIR (govsweep's output). For a
Tier A run (configs/tier-a.yaml) it prints the energy to solution and the
time of every arm against the grid's baseline arm, per workload kind, as
medians and 95th percentiles over profiles, loads, classes and seeds, then
the acceptance rule of the experiment's README, judged per profile. For a
Q5 run (sensitivity variants, configs/tier-a-sensitivity.yaml) it prints
the acceptance at every value of each assumed fact and the burst latency;
for a Q3 run (the receiver gate forced on, configs/tier-a-gate.yaml), the
gate's cost and what sustained buys back.

Baselines: CPU, the performance preset with boost on (race to idle at the
reference frequency); GPU, auto with no cap, in the same profile, workload,
transfer and sensitivity variant. Only cells that ran (status ok) are
compared, timed-out cells aside in the acceptance; a cell whose arm or cap
the profile cannot take is counted apart.
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
    if r["load"] == "bursty":
        return f"bursty {r['class']}"
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
    for cls in sorted({r["class"] for r in bursty}):
        # A bursty cell's time is the fixed window of its bursts; what an arm
        # costs it is the burst latency, resolved to the burst step.
        sub = [r for r in bursty if r["class"] == cls]
        ref = {(r["profile"], r["seed"]): float(r["burst_p95_ms"]) for r in sub if r["arm"] == base}
        print(f"\n#### Bursty load, {cls}: burst latency (p95 over bursts; median over profiles and seeds)\n")
        print(f"| arm | p50 ms | p95 ms | p95 against `{base}` % |")
        print("|---|---|---|---|")
        by = defaultdict(lambda: ([], [], []))
        for r in sub:
            by[r["arm"]][0].append(float(r["burst_p50_ms"]))
            by[r["arm"]][1].append(float(r["burst_p95_ms"]))
            b = ref.get((r["profile"], r["seed"]))
            if b:
                by[r["arm"]][2].append(100 * (float(r["burst_p95_ms"]) / b - 1))
        for a in order:
            if a in by:
                p50, p95, d = by[a]
                print(f"| {a} | {statistics.median(p50):.0f} | {statistics.median(p95):.0f} | {fmt(statistics.median(d)) if d else 'n/a'} |")
    return deltas(base_rows, key, arm, base)


def gpu_arm(r):
    a = r["preset"] + (r["ceiling_pct"] if r["ceiling_pct"] else "")
    return f"{a} cap{r['cap_pct']}"


def acceptance_deltas(rows, key, arm_of, base_arm, loads=None):
    """The deltas the acceptance rule judges, by (arm, profile), one per cell:
    job cells only, unless loads names others (an idle cell has no job to
    slow down, and a bursty cell's time is a fixed window in every arm; both
    are reported apart), each cell's deltas averaged over its seeds (the
    emulator is deterministic, so seeds repeat a cell rather than sample it),
    and a timed-out cell kept with the slowdown at the limit, a lower bound,
    and counted as censored. key leaves out the seed and includes the profile."""
    judged = (lambda r: r["load"] in loads) if loads else (lambda r: r["load"] not in ("idle", "bursty"))
    base = defaultdict(list)
    for r in rows:
        if judged(r) and r["status"] == "ok" and arm_of(r) == base_arm:
            base[key(r)].append(r)
    cells = defaultdict(lambda: {"e": [], "t": [], "censored": False})
    for r in rows:
        if not judged(r) or r["status"] not in ("ok", "timeout"):
            continue
        bs = [b for b in base.get(key(r), []) if b["seed"] == r["seed"]]
        if not bs:
            continue
        be, bt = float(bs[0]["host_j"]), float(bs[0]["time_s"])
        if be <= 0 or bt <= 0:
            continue
        c = cells[(arm_of(r), key(r))]
        c["e"].append(100 * (float(r["host_j"]) / be - 1))
        c["t"].append(100 * (float(r["time_s"]) / bt - 1))
        c["censored"] |= r["status"] == "timeout"
    out = defaultdict(lambda: {"e": [], "t": [], "n": 0, "censored": 0})
    for (a, k), c in cells.items():
        d = out[(a, k[0])]
        d["e"].append(statistics.mean(c["e"]))
        d["t"].append(statistics.mean(c["t"]))
        d["n"] += 1
        d["censored"] += c["censored"]
    return out


def gpu_section(rows):
    if not rows:
        return {}
    print("\n## E-G2: GPU presets\n")
    print(f"Baseline: `{GPU_BASE_PRESET}` with no cap on the same profile, workload, transfer, receiver gate and seed.")
    arm = gpu_arm
    key = lambda r: (r["profile"], r["sens"], r["gate"], r["load"], r["class"], r["gpus"], r["transfer"], r["transfer_intensity"], r["seed"])
    base_arm = f"{GPU_BASE_PRESET} cap{GPU_BASE_CAP}"
    base_rows = [r for r in rows if not r["sens"]]
    arms = sorted({arm(r) for r in base_rows}, key=lambda a: (a.split()[1], a))
    unsupported = defaultdict(set)
    for r in base_rows:
        if r["status"] == "unsupported":
            unsupported[r["profile"]].add(arm(r))
    if unsupported:
        print("\nArms a profile cannot express (pkg/governor gives unsupported, or the cap is outside the board's range; nothing ran):\n")
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


CANDIDATES = (
    # (name, class, arms in order of preference, limit, baseline arm)
    ("CPU performance (boost on)", "performance", ("performance-boost-on", "performance"), PERF_P95, True),
    ("CPU efficiency", "eco", ("efficiency-boost-on", "efficiency"), ECO_P95, False),
    ("CPU balanced", "eco", ("balanced-boost-on", "balanced"), ECO_P95, False),
    ("CPU efficiency, boost off (CPU_GOVERNOR_ECO_BOOST=off)", "eco", ("efficiency-boost-off",), ECO_P95, False),
    ("GPU efficient 75 %", "eco", ("efficient75 cap100",), ECO_P95, False),
    ("GPU efficient 80 %", "eco", ("efficient80 cap100",), ECO_P95, False),
    ("GPU efficient 90 %", "eco", ("efficient90 cap100",), ECO_P95, False),
    ("GPU efficient 75 % with a 60 % cap", "eco", ("efficient75 cap60",), ECO_P95, False),
    ("GPU sustained", "performance", ("sustained cap100",), PERF_P95, False),
)


def burst_changes(cpu):
    """Each arm's burst p95 change against the baseline arm, by (arm,
    profile, class), median over seeds."""
    rows = [r for r in cpu if r["load"] == "bursty" and r["status"] == "ok" and not r["sens"]]
    if not rows:
        return {}
    base = cpu_base(rows)
    ref = {(r["profile"], r["class"], r["seed"]): float(r["burst_p95_ms"]) for r in rows if r["arm"] == base}
    out = defaultdict(list)
    for r in rows:
        b = ref.get((r["profile"], r["class"], r["seed"]))
        if b:
            out[(r["arm"], r["profile"], r["class"])].append(100 * (float(r["burst_p95_ms"]) / b - 1))
    return {k: statistics.median(v) for k, v in out.items()}


def acceptance(cpu_d, gpu_d, cpu_idle=None, gpu_idle=None, burst=None):
    """Judges each candidate on each profile (a default applies to every
    node, and the profiles answer differently). d and idle map (arm,
    profile) to deltas (acceptance_deltas)."""
    print("\n## Acceptance rule, Tier A half\n")
    print(f"A candidate default passes Tier A on a profile when its median energy change there is negative and its p95 "
          f"slowdown is at most {PERF_P95:.0f} % (performance class) or {ECO_P95:.0f} % (eco class). Tier B and Q5 decide the rest. "
          "The rule judges job cells only, one value per cell (its seeds averaged); idle cells, with no job to slow down, "
          "are reported apart, and bursty cells, whose time is a fixed window in every arm, are left to their burst latency in E-G1. A timed-out cell counts with its "
          "slowdown at the limit, a lower bound, so with any such cell a pass is not established. "
          "Idle energy is reported for the GPU candidates only: CPU idle power is the same under every preset by "
          "assumption (`idleGovernorDeltaW` is 0 in every profile), and Q5 varies it. "
          "Tier B has measured no energy or slowdown yet (Q4), so this rule adopts no default: a pass here is the Tier A half. "
          "The burst column is each CPU candidate's burst p95 against the baseline, per burst class; the rule does not "
          "count it yet. The time p95 is the nearest-rank 95th percentile over the profile's cells (the 19th of 20, "
          "the 46th of 48), so the worst cell is shown beside it.\n")
    burst = burst or {}
    classes = sorted({k[2] for k in burst})
    print("| candidate | class | profile | cells | timed out | energy median % | time p95 % | time worst cell % | idle energy median % | "
          f"burst p95 % ({', '.join(classes) or 'none'}) | Tier A |")
    print("|---|---|---|---:|---:|---|---|---|---|---|---|")
    tally = []
    for name, cls, arms, limit, is_base in CANDIDATES:
        src, idle = (cpu_d, cpu_idle or {}) if name.startswith("CPU") else (gpu_d, gpu_idle or {})
        arm = next((a for a in arms if any(k[0] == a for k in src)), None)
        if arm is None:
            continue
        verdicts = []
        for prof in sorted({k[1] for k in src if k[0] == arm}):
            d = src[(arm, prof)]
            if not d["n"]:
                continue
            e, t = statistics.median(d["e"]), pct(d["t"], 95)
            ok = "baseline" if is_base else ("pass" if e < 0 and t <= limit else "fail")
            # With censored cells the p95 is a lower bound: a pass is not established.
            if d["censored"] and ok == "pass":
                ok = "undecided (timed-out cells)"
            i = idle.get((arm, prof))
            icell = fmt(statistics.median(i["e"])) if i and i["n"] and name.startswith("GPU") else "n/a"
            tcell = (">= " if d["censored"] else "") + fmt(t)
            bs = [burst.get((arm, prof, c)) for c in classes]
            bcell = ", ".join(fmt(b) if b is not None else "n/a" for b in bs) if name.startswith("CPU") and classes else "n/a"
            wcell = (">= " if d["censored"] else "") + fmt(max(d["t"]))
            print(f"| {name} | {cls} | {prof} | {d['n']} | {d['censored']} | {fmt(e)} | {tcell} | {wcell} | {icell} | {bcell} | {ok} |")
            verdicts.append(ok)
        if not is_base and verdicts:
            tally.append((name, sum(v == "pass" for v in verdicts), len(verdicts)))
    if tally:
        print("\n| candidate | profiles where it passes |")
        print("|---|---|")
        for name, k, n in tally:
            print(f"| {name} | {k} of {n} |")


def sign_check(cpu, gpu):
    """Q5's half of the acceptance rule: a candidate's energy change on a
    profile, judged as the acceptance judges it, must stay negative and its
    p95 slowdown within the candidate's limit at every value of every
    assumed fact. Its worst cell and its idle energy are reported beside."""
    sens = sorted({r["sens"] for r in cpu + gpu if r["sens"]})
    if not sens:
        return
    cpu_key = lambda r: (r["profile"], r["load"], r["pct"], r["class"])
    gpu_key = lambda r: (r["profile"], r["gate"], r["load"], r["class"], r["gpus"], r["transfer"], r["transfer_intensity"])
    gpu_base = f"{GPU_BASE_PRESET} cap{GPU_BASE_CAP}"
    by = defaultdict(lambda: defaultdict(list))  # (candidate, profile) -> fact -> [(value, energy, p95, idle)]
    limits = {}
    for v in sens:
        fact, value = v.split("=", 1)
        sub_cpu, sub_gpu = [r for r in cpu if r["sens"] == v], [r for r in gpu if r["sens"] == v]
        d = {"CPU": acceptance_deltas(sub_cpu, cpu_key, lambda r: r["arm"], cpu_base(sub_cpu)) if sub_cpu else {},
             "GPU": acceptance_deltas(sub_gpu, gpu_key, gpu_arm, gpu_base) if sub_gpu else {}}
        idle = {"CPU": acceptance_deltas(sub_cpu, cpu_key, lambda r: r["arm"], cpu_base(sub_cpu), loads=("idle",)) if sub_cpu else {},
                "GPU": acceptance_deltas(sub_gpu, gpu_key, gpu_arm, gpu_base, loads=("idle",)) if sub_gpu else {}}
        for name, _, arms, limit, is_base in CANDIDATES:
            grid = name[:3]
            arm = next((a for a in arms if any(k[0] == a for k in d[grid])), None)
            if is_base or arm is None:
                continue
            limits[name] = limit
            for (a, prof), x in d[grid].items():
                if a == arm and x["n"]:
                    i = idle[grid].get((a, prof))
                    by[(name, prof)][fact].append((value, statistics.median(x["e"]), pct(x["t"], 95),
                                                   statistics.median(i["e"]) if i and i["n"] else None, max(x["t"])))
    if not by:
        return
    print("\n## Q5: the acceptance at every value of each assumed fact\n")
    print("Each candidate's energy change and p95 slowdown on each profile, judged as the acceptance judges it (job "
          "cells, one value per cell), at every value of each assumed fact, one fact at a time. Q5 holds when, at every "
          "value, the energy change stays negative and the p95 slowdown within the candidate's limit. Idle energy is "
          "the median over the idle cells.\n")
    print("| candidate | profile | energy not negative at | worst energy % | p95 over the limit at | worst p95 % | worst cell % | idle energy % (lowest to highest) | Q5 |")
    print("|---|---|---|---|---|---|---|---|---|")
    for (name, prof), facts in sorted(by.items()):
        rows = [(f, v, e, t, i) for f, vals in facts.items() for v, e, t, i, _ in vals]
        wc = max(((w, f, v) for f, vals in facts.items() for v, _, _, _, w in vals), key=lambda x: x[0])
        e_bad = sorted({f"{f}={v}" for f, v, e, _, _ in rows if e >= 0})
        t_bad = sorted({f"{f}={v}" for f, v, _, t, _ in rows if t > limits[name]})
        we = max(rows, key=lambda x: x[2])
        wt = max(rows, key=lambda x: x[3])
        idles = [i for *_, i in rows if i is not None]
        icell = f"{fmt(min(idles))} to {fmt(max(idles))}" if idles else "n/a"
        print(f"| {name} | {prof} | {', '.join(e_bad) or 'none'} | {fmt(we[2])} at {we[0]}={we[1]} | "
              f"{', '.join(t_bad) or 'none'} | {fmt(wt[3])} at {wt[0]}={wt[1]} | {fmt(wc[0])} at {wc[1]}={wc[2]} | {icell} | "
              f"{'fails' if e_bad or t_bad else 'holds'} |")
    if any(f == "freqRampMS" for facts in by.values() for f in facts):
        print("\nThe steady and job cells step the physics every substepMS (100 ms in configs/tier-a-sensitivity.yaml), "
              "and a frequency ramp no longer than one substep completes within it (phys.FirstOrderToward), so "
              "`freqRampMS` 5 and 50 give the same steady results; the bursts, stepped at 1 ms, tell them apart.")
    missing = [name for name, *_, is_base in CANDIDATES if not is_base and name not in limits]
    if missing:
        print(f"\nNot run in Q5 (their arms are not in this grid), so not established: {'; '.join(missing)}.")


def burst_q5(cpu):
    """Q5 for the bursty cells: each arm's burst p95, and its change against
    the baseline arm on the same profile, at every value of each CPU fact;
    medians over profiles."""
    rows = [r for r in cpu if r["load"] == "bursty" and r["status"] == "ok"]
    if not rows or not any(r["sens"] for r in rows):
        return
    base = cpu_base(rows)
    arms = [base] + sorted({r["arm"] for r in rows} - {base})
    ref = {(r["sens"], r["class"], r["profile"]): float(r["burst_p95_ms"]) for r in rows if r["arm"] == base}
    by = defaultdict(lambda: ([], []))
    for r in rows:
        b = ref.get((r["sens"], r["class"], r["profile"]))
        x = by[(r["sens"] or "profile values", r["class"], r["arm"])]
        x[0].append(float(r["burst_p95_ms"]))
        if b:
            x[1].append(100 * (float(r["burst_p95_ms"]) / b - 1))
    print("\n## Q5: burst latency at every value of each CPU fact\n")
    print(f"Burst p95 in ms, and its change against `{base}` on the same profile in %; medians over profiles.\n")
    print("| variant | class | " + " | ".join(f"`{a}`" for a in arms) + " |")
    print("|---|---|" + "---|" * len(arms))
    for v, cls in sorted({(k[0], k[1]) for k in by}):
        cells = []
        for a in arms:
            ms, d = by.get((v, cls, a), ([], []))
            cells.append(f"{statistics.median(ms):.0f} ({fmt(statistics.median(d))})" if ms and d else "n/a")
        print(f"| {v} | {cls} | " + " | ".join(cells) + " |")


def gate_section(gpu):
    """Q3: under a forced receiver gate, what the gate costs a gather job under
    auto, and what sustained buys back, per profile and slowFraction."""
    rows = [r for r in gpu if r["status"] == "ok" and r["transfer"] == "gather" and r["cap_pct"] == "100"]
    if not any(r["gate"] == "on" for r in rows):
        return
    cannot = {(r["profile"], r["preset"]) for r in gpu if r["status"] == "unsupported"}
    print("\n## Q3: the receiver gate on gather jobs\n")
    print("A job of four GPUs gathers into an idle one. `auto` with the gate on against the gate off (the profile's), "
          "and `sustained` against `auto` with the gate on; medians over classes and seeds. The repository's "
          "`slowFraction` is 1.0, which makes the gate neutral.\n")
    print("| slowFraction | profile | `auto`, gate on against off: energy %, time %, kJ | `sustained` against `auto`, gate on: energy %, time %, kJ |")
    print("|---|---|---|---|")
    key = lambda r: (r["profile"], r["class"], r["gpus"], r["transfer_intensity"], r["seed"], r["sens"])
    cell = {(key(r), r["preset"], r["gate"]): r for r in rows}
    out = defaultdict(lambda: ([], [], [], [], [], []))
    for (k, preset, gate), r in cell.items():
        if preset != "auto" or gate != "on":
            continue
        off, sus = cell.get((k, "auto", "profile")), cell.get((k, "sustained", "on"))
        o = out[(k[5].split("=", 1)[1] if k[5] else "1.0 (profile)", k[0])]
        if off:
            o[0].append(100 * (float(r["host_j"]) / float(off["host_j"]) - 1))
            o[1].append(100 * (float(r["time_s"]) / float(off["time_s"]) - 1))
            o[2].append((float(r["host_j"]) - float(off["host_j"])) / 1000)
        if sus:
            o[3].append(100 * (float(sus["host_j"]) / float(r["host_j"]) - 1))
            o[4].append(100 * (float(sus["time_s"]) / float(r["time_s"]) - 1))
            o[5].append((float(sus["host_j"]) - float(r["host_j"])) / 1000)
    med = lambda xs, f=fmt: f(statistics.median(xs)) if xs else "n/a"
    kj = lambda v: f"{v:+.1f}"
    for (sf, prof), o in sorted(out.items()):
        sus = "the profile cannot express `sustained`" if (prof, "sustained") in cannot else f"{med(o[3])}, {med(o[4])}, {med(o[5], kj)}"
        print(f"| {sf} | {prof} | {med(o[0])}, {med(o[1])}, {med(o[2], kj)} | {sus} |")

def main():
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    run = sys.argv[1]
    cpu, gpu = load(run, "cpu"), load(run, "gpu")
    if not cpu and not gpu:
        print(f"summarize.py: no *-cpu.csv or *-gpu.csv in {run}", file=sys.stderr)
        return 1
    # A run is Tier A, Q5 (sensitivity variants) or Q3 (the receiver gate
    # forced on), and gets only its own sections: the acceptance belongs to
    # Tier A's cells.
    kind = ("Q3, the receiver gate" if any(r["gate"] == "on" for r in gpu) else
            "Q5, sensitivity" if any(r["sens"] for r in cpu + gpu) else "Tier A")
    print(f"# Experiment 05, {kind} summary\n\nRun: `{os.path.basename(os.path.abspath(run))}`, "
          f"{len(cpu)} CPU cells and {len(gpu)} GPU cells.\n")
    if kind.startswith("Q3"):
        gate_section(gpu)
    elif any(r["sens"] for r in cpu + gpu):
        sign_check(cpu, gpu)
        burst_q5(cpu)
    else:
        cpu_section(cpu)
        gpu_section(gpu)
        cpu_key = lambda r: (r["profile"], r["load"], r["pct"], r["class"])
        gpu_key = lambda r: (r["profile"], r["gate"], r["load"], r["class"], r["gpus"], r["transfer"], r["transfer_intensity"])
        cpu_arm, gpu_base = lambda r: r["arm"], f"{GPU_BASE_PRESET} cap{GPU_BASE_CAP}"
        acceptance(acceptance_deltas(cpu, cpu_key, cpu_arm, cpu_base(cpu)) if cpu else {},
                   acceptance_deltas(gpu, gpu_key, gpu_arm, gpu_base) if gpu else {},
                   acceptance_deltas(cpu, cpu_key, cpu_arm, cpu_base(cpu), loads=("idle",)) if cpu else {},
                   acceptance_deltas(gpu, gpu_key, gpu_arm, gpu_base, loads=("idle",)) if gpu else {},
                   burst_changes(cpu))
    return 0


if __name__ == "__main__":
    sys.exit(main())
