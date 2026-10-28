"""An accounting split of B's and C's change in energy per unit of work
against A, by node family. It says where the energy went, not why: a cap's
effect and a placement's are only separated by a run with the caps off.

For each seed, on one time base (the whole run, 0 to 900 s, since the work
done per family is known only at the end): each family's cumulative energy
at 900 s (energy_by_node.jsonl) and its work done (sim_debug_jobs.json,
total - remaining). A family is a node name without its index
(kwok-h100-nvl-3 is kwok-h100-nvl). The work is GPU work when the sweep has
GPU jobs, and then only the families that did GPU work count; otherwise it
is CPU work.

Each family's energy splits into idle energy, its nodes' model idle power
(sim_debug_nodes.json: the CPU's measured curve at 0 % load, or BaseIdleW
for a CPU without a curve, plus the GPUs' IdleWattsPerGPU) times the elapsed
time, and dynamic energy, the rest. With s a family's share of the
work and d its dynamic energy per unit, the change in J per unit is exactly
  idle term       (idle energy per unit of all the work, B minus A)
+ placement       sum of (s_B - s_A) x d_A over the families
+ each family's own term, s_B x (d_B - d_A).
Pricing moved work at the average cost, idle included, would credit a
family that gains work with a saving that is only its idle power spread
over more work.

It also prints each family's share of compute-bound work and, for CPU work,
how often an eco cap binds: the agent's control calls on a family at its
eco cap (below the highest cap the family gets, the performance cap) in
simulator.log in which the node draws at least 97 % of the cap.

Usage, from the repository root:
    python experiments/kwok-cluster/decompose_energy.py RESULTS_DIR
"""
import collections
import glob
import json
import re
import sys


def family(node):
    return re.sub(r"-\d+$", "", node or "")


def run(d, gpu):
    polls = [json.loads(l) for l in open(d + "/energy_by_node.jsonl")]
    last = min(polls, key=lambda p: abs(p["elapsed_sec"] - 900))
    models = {n["node"]: n.get("model") or {} for n in json.load(open(d + "/sim_debug_nodes.json"))["nodes"]}
    energy, idle = collections.Counter(), collections.Counter()
    for node, joules in last["byNodeJoules"].items():
        m = models.get(node, {})
        g = m.get("GPU") or {}
        energy[family(node)] += joules
        curve = m.get("CPUPowerCurve") or []
        cpu_idle = curve[0]["PowerW"] if curve else m.get("BaseIdleW", 0)
        idle[family(node)] += (cpu_idle + (g.get("Count") or 0) * (g.get("IdleWattsPerGPU") or 0)) * last["elapsed_sec"]
    kind = "gpu" if gpu else "cpu"
    cls = {}
    for line in open(d + "/trace.jsonl"):
        r = json.loads(line)
        if r.get("type") == "job":
            cls[r["jobId"]] = (r.get("workloadClass") or {}).get(kind, "")
    work, compute = collections.Counter(), collections.Counter()
    for j in json.load(open(d + "/sim_debug_jobs.json"))["jobs"]:
        total = j.get(f"{kind}UnitsTotal") or 0
        if j.get("submitted") and total:
            done = total - j[f"{kind}UnitsRemaining"]
            work[family(j.get("node"))] += done
            if "compute" in cls.get(j["jobId"], ""):
                compute[family(j.get("node"))] += done
    calls = []
    for line in open(d + "/simulator.log"):
        m = re.search(r"control node=(\S+) action=rapl.set_power_cap_watts capW=([\d.]+) throttlePct=\d+ powerW=([\d.]+)", line)
        if m and float(m.group(2)) < 5000:
            calls.append((family(m.group(1)), float(m.group(2)), float(m.group(3))))
    top = collections.defaultdict(float)
    for f, cap, _ in calls:
        top[f] = max(top[f], cap)
    bind, capped = collections.Counter(), collections.Counter()
    for f, cap, power in calls:
        if cap < 0.99 * top[f]:
            capped[f] += 1
            bind[f] += power >= 0.97 * cap
    return energy, idle, work, compute, bind, capped


def main(base):
    first = sorted(glob.glob(f"{base}/*_bA_s1"))[0]
    gpu = any((j.get("gpuUnitsTotal") or 0) > 0 for j in json.load(open(first + "/sim_debug_jobs.json"))["jobs"])
    seeds = sorted({re.search(r"_s(\d+)$", d).group(1) for d in glob.glob(f"{base}/*_b?_s*")})
    for seed in seeds:
        res = {b: run(sorted(glob.glob(f"{base}/*_b{b}_s{seed}"))[0], gpu) for b in "ABC"}
        ea, ia, wa, ca = res["A"][:4]
        fams = sorted(f for f in wa if wa[f] > 0)
        ta = sum(wa[f] for f in fams)
        da = {f: (ea[f] - ia[f]) / wa[f] for f in fams}
        JA = sum(ea[f] for f in fams) / ta
        print(f"seed {seed}, {'GPU' if gpu else 'CPU'} work, 0 to 900 s: A J per unit {JA:.5f}; per family "
              + ", ".join(f"{f} {ea[f] / wa[f]:.4g} (idle share {ia[f] / ea[f]:.2f}, dynamic {da[f]:.4g}, work share {wa[f] / ta:.3f}, compute-bound {ca[f] / wa[f]:.2f})" for f in fams))
        for b in "BC":
            eb, ib, wb, cb, bind, capped = res[b]
            tb = sum(wb[f] for f in fams)
            JB = sum(eb[f] for f in fams) / tb
            idle_term = sum(ib[f] for f in fams) / tb - sum(ia[f] for f in fams) / ta
            mix = sum((wb[f] / tb - wa[f] / ta) * da[f] for f in fams)
            own = {f: (wb[f] / tb) * ((eb[f] - ib[f]) / wb[f] - da[f]) for f in fams}
            line = (f"  {b}: J per unit {JB:.5f} ({100 * (JB / JA - 1):+.2f} %) = idle {100 * idle_term / JA:+.2f} pp + placement {100 * mix / JA:+.2f} pp"
                    + "".join(f" + {f} own {100 * own[f] / JA:+.2f} pp" for f in fams)
                    + " | work share, compute-bound " + ", ".join(f"{f} {wb[f] / tb:.3f} {cb[f] / wb[f]:.2f}" for f in fams))
            if not gpu:
                line += " | eco cap binding (power >= 0.97 cap) " + ", ".join(f"{f} {bind[f]}/{capped[f]}" for f in fams)
            print(line)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
