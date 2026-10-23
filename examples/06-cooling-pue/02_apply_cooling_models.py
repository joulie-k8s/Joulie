#!/usr/bin/env python3
"""Apply cooling models to IT power timeseries and compute PUE.

Takes timeseries CSV files (from the simulator or 01_generate_synthetic_timeseries.py)
and applies multiple cooling models to compute cooling power and PUE at each timestep.

Supports two types of cooling models:

1. **Built-in parametric models** (no external dependencies):
   - Air-cooled (traditional CRAH)
   - Hot/cold aisle containment
   - Direct liquid cooling (DLC / direct-to-chip)
   - Immersion cooling
   - Free cooling (air-side economizer, cold climate)

2. **FMU co-simulation** (requires fmpy):
   - Loads an FMI 2.0 co-simulation FMU exported from OpenModelica / Dymola
   - Steps through the timeseries, passing IT heat load + outdoor temp as inputs
   - Reads cooling power from FMU output variables
   - Example: LBL Buildings.Applications.DataCenters.DXCooled

Usage:
    # Built-in models only (no FMU needed):
    python 02_apply_cooling_models.py

    # With an FMU:
    python 02_apply_cooling_models.py --fmu path/to/datacenter.fmu

    # Custom input directory:
    python 02_apply_cooling_models.py --data-dir ./data
"""
import argparse
import csv
import json
import math
import pathlib
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field

import numpy as np
import pandas as pd

DOCKER_IMAGE = "openmodelica/openmodelica:v1.26.3-ompython"


# ---------------------------------------------------------------------------
# Built-in parametric cooling models
# ---------------------------------------------------------------------------

@dataclass
class CoolingModelResult:
    """Output of a cooling model for one timestep."""
    cooling_power_w: float
    pue: float
    cop: float  # Coefficient of Performance (cooling_capacity / electrical_input)
    indoor_temp_c: float  # Data center indoor temperature


class CoolingModel:
    """Base class for parametric cooling models."""

    name: str = "base"

    def step(self, it_power_w: float, outdoor_temp_c: float, dt_sec: float) -> CoolingModelResult:
        raise NotImplementedError


class AirCooledCRAH(CoolingModel):
    """Traditional air-cooled data center with CRAH (Computer Room Air Handler).

    High baseline overhead, COP degrades significantly at high outdoor temps.
    Typical PUE: 1.3 - 1.7
    """
    name = "Air-cooled (CRAH)"

    def __init__(self):
        self.base_pue = 1.30          # PUE at 15C outdoor
        self.temp_slope = 0.015       # PUE increase per degree above 15C
        self.load_slope = 0.08        # PUE increase at full load vs idle
        self.indoor_setpoint_c = 22.0
        self.cop_base = 3.5           # COP at 15C
        self.cop_temp_slope = -0.08   # COP decrease per degree

    def step(self, it_power_w, outdoor_temp_c, dt_sec):
        temp_delta = max(0, outdoor_temp_c - 15.0)
        pue = self.base_pue + self.temp_slope * temp_delta
        pue = max(1.0, min(3.0, pue))
        cooling_power = it_power_w * (pue - 1.0)
        cop = max(1.5, self.cop_base + self.cop_temp_slope * temp_delta)
        indoor = self.indoor_setpoint_c + 0.3 * temp_delta
        return CoolingModelResult(cooling_power, pue, cop, indoor)


class HotColdAisle(CoolingModel):
    """Hot/cold aisle containment with improved airflow management.

    Better than open CRAH due to reduced mixing losses.
    Typical PUE: 1.15 - 1.4
    """
    name = "Hot/cold aisle"

    def __init__(self):
        self.base_pue = 1.15
        self.temp_slope = 0.010
        self.indoor_setpoint_c = 24.0  # Can run warmer with containment
        self.cop_base = 4.5
        self.cop_temp_slope = -0.07

    def step(self, it_power_w, outdoor_temp_c, dt_sec):
        temp_delta = max(0, outdoor_temp_c - 15.0)
        pue = self.base_pue + self.temp_slope * temp_delta
        pue = max(1.0, min(2.5, pue))
        cooling_power = it_power_w * (pue - 1.0)
        cop = max(2.0, self.cop_base + self.cop_temp_slope * temp_delta)
        indoor = self.indoor_setpoint_c + 0.2 * temp_delta
        return CoolingModelResult(cooling_power, pue, cop, indoor)


class DirectLiquidCooling(CoolingModel):
    """Direct liquid cooling (DLC) / direct-to-chip.

    Liquid loops remove heat directly from CPUs/GPUs. Very efficient,
    nearly independent of outdoor temperature for the liquid loop.
    Typical PUE: 1.03 - 1.15

    Models two heat paths:
      - Liquid loop (70-80% of IT heat): very efficient, low sensitivity to outdoor temp
      - Residual air cooling (20-30%): for DIMMs, storage, fans
    """
    name = "Direct liquid cooling (DLC)"

    def __init__(self):
        self.liquid_fraction = 0.75    # fraction of IT heat removed by liquid
        self.liquid_cop = 20.0         # pumps are very efficient
        self.air_base_pue = 1.10       # residual air PUE
        self.air_temp_slope = 0.005
        self.indoor_setpoint_c = 27.0  # Can run hot, liquid handles most heat
        self.cop_base = 12.0
        self.cop_temp_slope = -0.04

    def step(self, it_power_w, outdoor_temp_c, dt_sec):
        temp_delta = max(0, outdoor_temp_c - 15.0)
        liquid_heat = it_power_w * self.liquid_fraction
        air_heat = it_power_w * (1.0 - self.liquid_fraction)

        liquid_cooling_power = liquid_heat / self.liquid_cop
        air_pue = self.air_base_pue + self.air_temp_slope * temp_delta
        air_cooling_power = air_heat * (air_pue - 1.0)

        cooling_power = liquid_cooling_power + air_cooling_power
        pue = (it_power_w + cooling_power) / max(it_power_w, 1.0)
        cop = max(3.0, self.cop_base + self.cop_temp_slope * temp_delta)
        indoor = self.indoor_setpoint_c + 0.1 * temp_delta
        return CoolingModelResult(cooling_power, pue, cop, indoor)


class ImmersionCooling(CoolingModel):
    """Single-phase or two-phase immersion cooling.

    Entire servers submerged in dielectric fluid. Extremely efficient,
    almost no fans, minimal outdoor temperature sensitivity.
    Typical PUE: 1.02 - 1.06
    """
    name = "Immersion cooling"

    def __init__(self):
        self.cop = 25.0               # pumps + heat exchanger
        self.overhead_fraction = 0.02  # lighting, networking, controls
        self.indoor_setpoint_c = 35.0  # fluid temp is higher but contained
        self.cop_temp_slope = -0.02

    def step(self, it_power_w, outdoor_temp_c, dt_sec):
        temp_delta = max(0, outdoor_temp_c - 15.0)
        cop = max(8.0, self.cop + self.cop_temp_slope * temp_delta)
        cooling_power = it_power_w / cop
        overhead = it_power_w * self.overhead_fraction
        total_cooling = cooling_power + overhead
        pue = (it_power_w + total_cooling) / max(it_power_w, 1.0)
        indoor = self.indoor_setpoint_c + 0.05 * temp_delta
        return CoolingModelResult(total_cooling, pue, cop, indoor)


class FreeCooling(CoolingModel):
    """Air-side economizer (free cooling) for cold climates.

    Uses outdoor air directly when cold enough. Excellent PUE in winter,
    degrades to mechanical cooling in summer.
    Typical PUE: 1.02 (cold) - 1.35 (hot)
    """
    name = "Free cooling (economizer)"

    def __init__(self):
        self.free_threshold_c = 18.0   # below this: 100% free cooling
        self.mech_threshold_c = 28.0   # above this: 100% mechanical
        self.free_pue = 1.02           # fans only
        self.mech_base_pue = 1.25      # mechanical cooling base
        self.mech_temp_slope = 0.012
        self.indoor_setpoint_c = 24.0

    def step(self, it_power_w, outdoor_temp_c, dt_sec):
        if outdoor_temp_c <= self.free_threshold_c:
            free_frac = 1.0
        elif outdoor_temp_c >= self.mech_threshold_c:
            free_frac = 0.0
        else:
            free_frac = 1.0 - (outdoor_temp_c - self.free_threshold_c) / (self.mech_threshold_c - self.free_threshold_c)

        free_power = it_power_w * (self.free_pue - 1.0) * free_frac
        mech_temp_delta = max(0, outdoor_temp_c - 15.0)
        mech_pue = self.mech_base_pue + self.mech_temp_slope * mech_temp_delta
        mech_power = it_power_w * (mech_pue - 1.0) * (1.0 - free_frac)

        cooling_power = free_power + mech_power
        pue = (it_power_w + cooling_power) / max(it_power_w, 1.0)
        cop = max(2.0, it_power_w / max(cooling_power, 1.0))
        indoor = self.indoor_setpoint_c + 0.2 * max(0, outdoor_temp_c - self.free_threshold_c)
        return CoolingModelResult(cooling_power, pue, cop, indoor)


# ---------------------------------------------------------------------------
# FMU co-simulation wrapper
# ---------------------------------------------------------------------------

class DockerFMUCoolingModel(CoolingModel):
    """Runs an FMI 2.0 co-simulation FMU via Docker for glibc compatibility.

    Instead of stepping the FMU per-timestep from Python (which requires
    matching glibc), this model runs the entire timeseries through the FMU
    inside a Docker container and reads back the results.

    The FMU should expose:
      Inputs:  Q_IT (W), T_outdoor (K)
      Outputs: P_cooling (W), T_indoor (K), COP
    """
    name = "FMU model"

    def __init__(self, fmu_path: str):
        self.fmu_path = pathlib.Path(fmu_path).resolve()
        self.name = f"FMU ({self.fmu_path.stem})"
        if not self.fmu_path.exists():
            print(f"ERROR: FMU file not found: {self.fmu_path}", file=sys.stderr)
            sys.exit(1)


def run_fmu_on_timeseries(
    fmu_path: pathlib.Path,
    df: pd.DataFrame,
    time_scale: float,
) -> pd.DataFrame:
    """Run an FMU over a timeseries DataFrame using Docker.

    Writes input CSV, runs fmpy inside Docker, reads output CSV.
    Returns DataFrame with cooling_power_w, pue, cop, indoor_temp_c, facility_power_w columns added.
    """
    fmu_path = fmu_path.resolve()

    with tempfile.TemporaryDirectory(prefix="joulie-fmu-run-") as tmpdir:
        work_dir = pathlib.Path(tmpdir)

        # Write input CSV for the Docker script
        input_path = work_dir / "fmu_input.csv"
        df[["elapsed_sec", "it_power_w", "ambient_temp_c"]].to_csv(input_path, index=False)

        # Copy FMU to work dir
        import shutil
        shutil.copy2(fmu_path, work_dir / fmu_path.name)

        # Write the runner script (avoid f-string backslash issues)
        runner_lines = [
            '#!/usr/bin/env python3',
            'import csv, sys',
            'import numpy as np',
            'from fmpy import read_model_description, simulate_fmu',
            '',
            f'fmu = "{fmu_path.name}"',
            'md = read_model_description(fmu)',
            '',
            'rows = []',
            'with open("fmu_input.csv") as f:',
            '    reader = csv.DictReader(f)',
            '    for r in reader:',
            '        rows.append(r)',
            '',
            'n = len(rows)',
            f'time_scale = {time_scale}',
            '',
            'time_arr = np.zeros(n)',
            'q_it_arr = np.zeros(n)',
            't_out_arr = np.zeros(n)',
            '',
            'for i, r in enumerate(rows):',
            '    time_arr[i] = float(r["elapsed_sec"]) * time_scale',
            '    q_it_arr[i] = float(r["it_power_w"])',
            '    t_out_arr[i] = float(r["ambient_temp_c"]) + 273.15',
            '',
            'if n > 1:',
            '    step_size = time_arr[1] - time_arr[0]',
            'else:',
            '    step_size = 60.0',
            '',
            'stop_time = time_arr[-1] if n > 0 else 86400',
            '',
            'dtype = [("time", np.float64), ("Q_IT", np.float64), ("T_outdoor", np.float64)]',
            'signals = np.array(list(zip(time_arr, q_it_arr, t_out_arr)), dtype=dtype)',
            '',
            'print(f"Running FMU: {n} steps, step_size={step_size:.0f}s, stop={stop_time:.0f}s", file=sys.stderr)',
            '',
            'result = simulate_fmu(',
            '    fmu,',
            '    stop_time=stop_time,',
            '    step_size=step_size,',
            '    input=signals,',
            '    output=["P_cooling", "T_indoor", "COP"],',
            ')',
            '',
            'time_col = result["time"]',
            'pcool_col = result["P_cooling"]',
            'tind_col = result["T_indoor"]',
            'cop_col = result["COP"]',
            '',
            'with open("fmu_output.csv", "w", newline="") as f:',
            '    w = csv.writer(f)',
            '    w.writerow(["time_s", "p_cooling_w", "t_indoor_k", "cop"])',
            '    for i in range(len(result)):',
            '        w.writerow([',
            '            round(time_col[i], 1),',
            '            round(pcool_col[i], 1),',
            '            round(tind_col[i], 3),',
            '            round(cop_col[i], 4),',
            '        ])',
            '',
            'print(f"Wrote {len(result)} rows to fmu_output.csv", file=sys.stderr)',
        ]
        (work_dir / "run_fmu.py").write_text("\n".join(runner_lines) + "\n")

        # Run inside Docker
        result = subprocess.run(
            [
                "docker", "run", "--rm",
                "-v", f"{work_dir}:/work",
                "-w", "/work",
                DOCKER_IMAGE,
                "bash", "-c",
                "pip install fmpy numpy 2>/dev/null | tail -1 && python3 run_fmu.py",
            ],
            capture_output=True, text=True, timeout=600,
        )

        if result.stderr:
            for line in result.stderr.strip().split("\n"):
                print(f"    [FMU] {line}")

        output_path = work_dir / "fmu_output.csv"
        if not output_path.exists():
            print(f"ERROR: FMU Docker run failed. stdout: {result.stdout[:500]}", file=sys.stderr)
            sys.exit(1)

        # Read FMU output
        fmu_df = pd.read_csv(output_path)

    # Merge FMU results back into the original DataFrame
    enriched = df.copy()

    # The FMU may produce a different number of timesteps; interpolate to match input
    fmu_time = fmu_df["time_s"].values
    input_time = df["elapsed_sec"].values * time_scale

    p_cooling = np.interp(input_time, fmu_time, fmu_df["p_cooling_w"].values)
    t_indoor_k = np.interp(input_time, fmu_time, fmu_df["t_indoor_k"].values)
    cop_vals = np.interp(input_time, fmu_time, fmu_df["cop"].values)

    p_cooling = np.maximum(0, p_cooling)
    t_indoor_c = t_indoor_k - 273.15

    it_power = enriched["it_power_w"].values
    enriched["cooling_power_w"] = np.round(p_cooling, 1)
    enriched["pue"] = np.round((it_power + p_cooling) / np.maximum(it_power, 1.0), 4)
    enriched["cop"] = np.round(cop_vals, 2)
    enriched["indoor_temp_c"] = np.round(t_indoor_c, 2)
    enriched["facility_power_w"] = np.round(it_power + p_cooling, 1)

    # Cumulative cooling energy
    dt = time_scale  # seconds per wall-clock second
    if len(df) > 1:
        dt_wall = df["elapsed_sec"].iloc[1] - df["elapsed_sec"].iloc[0]
    else:
        dt_wall = 1.0
    cooling_energy = np.cumsum(p_cooling * dt_wall * time_scale)
    enriched["cooling_energy_cumulative_j"] = np.round(cooling_energy, 1)

    return enriched


# ---------------------------------------------------------------------------
# Main processing pipeline
# ---------------------------------------------------------------------------

def apply_cooling_model(
    df: pd.DataFrame,
    model: CoolingModel,
    time_scale: float = 1.0,
) -> pd.DataFrame:
    """Apply a cooling model to a timeseries DataFrame, returning enriched copy."""
    result = df.copy()
    cooling_powers = []
    pues = []
    cops = []
    indoor_temps = []
    facility_powers = []
    cooling_energy_j = 0.0
    cooling_energies = []

    dt = 1.0  # default timestep
    if len(df) > 1:
        dt = df["elapsed_sec"].iloc[1] - df["elapsed_sec"].iloc[0]

    for _, row in df.iterrows():
        it_power = row["it_power_w"]
        outdoor_temp = row["ambient_temp_c"]

        r = model.step(it_power, outdoor_temp, dt)
        cooling_powers.append(r.cooling_power_w)
        pues.append(r.pue)
        cops.append(r.cop)
        indoor_temps.append(r.indoor_temp_c)
        facility_powers.append(it_power + r.cooling_power_w)

        cooling_energy_j += r.cooling_power_w * dt * time_scale
        cooling_energies.append(cooling_energy_j)

    result["cooling_power_w"] = cooling_powers
    result["pue"] = pues
    result["cop"] = cops
    result["indoor_temp_c"] = indoor_temps
    result["facility_power_w"] = facility_powers
    result["cooling_energy_cumulative_j"] = cooling_energies

    return result


def main():
    ap = argparse.ArgumentParser(description="Apply cooling models to IT power timeseries")
    ap.add_argument("--data-dir", default="", help="Directory containing timeseries CSV files")
    ap.add_argument("--fmu", default="", help="Path to FMI 2.0 co-simulation FMU file")
    ap.add_argument("--baseline", default="", help="Process only this baseline (A, B, or C)")
    args = ap.parse_args()

    data_dir = pathlib.Path(args.data_dir) if args.data_dir else pathlib.Path(__file__).resolve().parent / "data"
    out_dir = data_dir

    # Discover input files
    baselines = ["A", "B", "C"] if not args.baseline else [args.baseline.upper()]
    input_files = {}
    for b in baselines:
        p = data_dir / f"timeseries_baseline_{b}.csv"
        if p.exists():
            input_files[b] = p
    if not input_files:
        print(f"no timeseries CSV files found in {data_dir}", file=sys.stderr)
        sys.exit(1)

    # Load metadata for time_scale
    time_scale = 60.0
    meta_path = data_dir / "metadata.json"
    if meta_path.exists():
        meta = json.loads(meta_path.read_text())
        time_scale = meta.get("time_scale", time_scale)

    # Set up cooling models
    models: list[CoolingModel] = [
        AirCooledCRAH(),
        HotColdAisle(),
        DirectLiquidCooling(),
        ImmersionCooling(),
        FreeCooling(),
    ]
    fmu_model = None
    if args.fmu:
        fmu_path = pathlib.Path(args.fmu).resolve()
        if not fmu_path.exists():
            print(f"FMU file not found: {fmu_path}", file=sys.stderr)
            sys.exit(1)
        fmu_model = DockerFMUCoolingModel(str(fmu_path))
        print(f"FMU loaded: {fmu_model.name} (will run via Docker)")

    # Process each baseline with each cooling model
    summary_rows = []
    for baseline, csv_path in sorted(input_files.items()):
        print(f"\n=== Baseline {baseline} ({csv_path.name}) ===")
        df = pd.read_csv(csv_path)
        print(f"  {len(df)} timesteps, IT power range: {df['it_power_w'].min()/1000:.1f} - {df['it_power_w'].max()/1000:.1f} kW")

        # Parametric models
        for model in models:
            model_tag = model.name.lower().replace(" ", "_").replace("(", "").replace(")", "").replace("/", "_")
            print(f"  applying: {model.name}...", end=" ", flush=True)

            enriched = apply_cooling_model(df, model, time_scale)

            out_path = out_dir / f"timeseries_baseline_{baseline}_cooling_{model_tag}.csv"
            enriched.to_csv(out_path, index=False)

            mean_pue = enriched["pue"].mean()
            mean_cop = enriched["cop"].mean()
            total_it_kwh = enriched["energy_cumulative_j"].iloc[-1] / 3.6e6
            total_cooling_kwh = enriched["cooling_energy_cumulative_j"].iloc[-1] / 3.6e6
            total_facility_kwh = total_it_kwh + total_cooling_kwh

            summary_rows.append({
                "baseline": baseline,
                "cooling_model": model.name,
                "cooling_tag": model_tag,
                "mean_pue": round(mean_pue, 4),
                "mean_cop": round(mean_cop, 2),
                "it_energy_kwh": round(total_it_kwh, 2),
                "cooling_energy_kwh": round(total_cooling_kwh, 2),
                "facility_energy_kwh": round(total_facility_kwh, 2),
                "cooling_overhead_pct": round(100 * total_cooling_kwh / max(total_it_kwh, 0.001), 1),
            })
            print(f"PUE={mean_pue:.3f}, COP={mean_cop:.1f}, facility={total_facility_kwh:.1f} kWh -> {out_path.name}")

        # FMU model (runs entire timeseries via Docker)
        if fmu_model is not None:
            model_tag = fmu_model.name.lower().replace(" ", "_").replace("(", "").replace(")", "").replace("/", "_")
            print(f"  applying: {fmu_model.name} (via Docker)...", flush=True)

            enriched = run_fmu_on_timeseries(fmu_model.fmu_path, df, time_scale)

            out_path = out_dir / f"timeseries_baseline_{baseline}_cooling_{model_tag}.csv"
            enriched.to_csv(out_path, index=False)

            mean_pue = enriched["pue"].mean()
            mean_cop = enriched["cop"].mean()
            total_it_kwh = enriched["energy_cumulative_j"].iloc[-1] / 3.6e6
            total_cooling_kwh = enriched["cooling_energy_cumulative_j"].iloc[-1] / 3.6e6
            total_facility_kwh = total_it_kwh + total_cooling_kwh

            summary_rows.append({
                "baseline": baseline,
                "cooling_model": fmu_model.name,
                "cooling_tag": model_tag,
                "mean_pue": round(mean_pue, 4),
                "mean_cop": round(mean_cop, 2),
                "it_energy_kwh": round(total_it_kwh, 2),
                "cooling_energy_kwh": round(total_cooling_kwh, 2),
                "facility_energy_kwh": round(total_facility_kwh, 2),
                "cooling_overhead_pct": round(100 * total_cooling_kwh / max(total_it_kwh, 0.001), 1),
            })
            print(f"    PUE={mean_pue:.3f}, COP={mean_cop:.1f}, facility={total_facility_kwh:.1f} kWh -> {out_path.name}")

    # Write summary
    summary_df = pd.DataFrame(summary_rows)
    summary_path = out_dir / "cooling_model_summary.csv"
    summary_df.to_csv(summary_path, index=False)
    print(f"\nsummary -> {summary_path}")
    print(summary_df.to_string(index=False))


if __name__ == "__main__":
    main()
