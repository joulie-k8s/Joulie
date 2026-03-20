#!/usr/bin/env python3
"""Multi-seed validation: 2500-node H100 cluster.
Run BASELINE vs JOULIE across 8 seeds to confirm consistency."""

import numpy as np
import pandas as pd
from sim_24h_pue import (
    generate_workload, run_simulation,
    DT_SEC, N_NODES,
)

SEEDS = [42, 123, 456, 789, 1337, 2024, 9999, 31415]
SIM_HOURS = 48


def main():
    print(f"Multi-seed validation: {len(SEEDS)} seeds, {SIM_HOURS}h each, {N_NODES} nodes (IT-level only)\n", flush=True)

    rows = []
    for seed in SEEDS:
        rng_wl = np.random.default_rng(seed)
        jobs = generate_workload(SIM_HOURS, rng_wl)
        n_jobs = len(jobs)

        results = {}
        for name in ["BASELINE", "JOULIE"]:
            rng_sim = np.random.default_rng(seed + 1)
            df = run_simulation(jobs, name, SIM_HOURS, rng_sim)
            deriv = np.diff(df["it_power_kw"].values)
            dropped = int(df["dropped"].sum())
            it_kwh = df["it_power_kw"].sum() * DT_SEC / 3600
            results[name] = {
                "avg_kw": df["it_power_kw"].mean(),
                "peak_kw": df["it_power_kw"].max(),
                "std_kw": df["it_power_kw"].std(),
                "dropped": dropped,
                "it_kwh": it_kwh,
                "kwh_per_job": it_kwh / max(1, n_jobs - dropped),
                "deriv_std": deriv.std(),
                "p99_ramp": np.percentile(np.abs(deriv), 99),
            }

        b = results["BASELINE"]
        j = results["JOULIE"]
        row = {
            "seed": seed,
            "n_jobs": n_jobs,
            "it_kwh_delta": (j["it_kwh"] - b["it_kwh"]) / b["it_kwh"] * 100,
            "peak_delta": (j["peak_kw"] - b["peak_kw"]) / b["peak_kw"] * 100,
            "std_delta": (j["std_kw"] - b["std_kw"]) / b["std_kw"] * 100,
            "drop_delta": j["dropped"] - b["dropped"],
            "kwh_job_delta": (j["kwh_per_job"] - b["kwh_per_job"]) / b["kwh_per_job"] * 100,
            "deriv_std_delta": (j["deriv_std"] - b["deriv_std"]) / b["deriv_std"] * 100,
            "p99_delta": (j["p99_ramp"] - b["p99_ramp"]) / b["p99_ramp"] * 100,
        }
        rows.append(row)
        print(f"  seed={seed:5d}  jobs={n_jobs:5d}  "
              f"IT_kWh={row['it_kwh_delta']:+5.1f}%  "
              f"peak={row['peak_delta']:+5.1f}%  "
              f"std={row['std_delta']:+5.1f}%  "
              f"drops={row['drop_delta']:+6d}  "
              f"kWh/job={row['kwh_job_delta']:+5.2f}%  "
              f"ramp_std={row['deriv_std_delta']:+5.1f}%  "
              f"p99={row['p99_delta']:+5.1f}%", flush=True)

    df = pd.DataFrame(rows)
    print(f"\n{'='*70}")
    print("AGGREGATE (mean +/- std across seeds)")
    print(f"{'='*70}")
    for col, label in [
        ("it_kwh_delta", "IT energy (%)"),
        ("peak_delta", "Peak IT power (%)"),
        ("std_delta", "Power std (%)"),
        ("kwh_job_delta", "kWh/job (%)"),
        ("deriv_std_delta", "Ramp rate std (%)"),
        ("p99_delta", "P99 ramp (%)"),
    ]:
        vals = df[col]
        wins = (vals < 0).sum()
        print(f"  {label:25s}: {vals.mean():+6.2f}% +/- {vals.std():5.2f}%  "
              f"(wins: {wins}/{len(SEEDS)}, range: [{vals.min():+.1f}%, {vals.max():+.1f}%])")

    drop_delta = df["drop_delta"]
    print(f"  {'Drop delta (absolute)':25s}: {drop_delta.mean():+6.1f} +/- {drop_delta.std():5.1f}  "
          f"(wins: {(drop_delta < 0).sum()}/{len(SEEDS)})")


if __name__ == "__main__":
    main()
