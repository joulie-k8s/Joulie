#!/usr/bin/env python3
"""Run the DXCooledAirsideEconomizer FMU and verify it works."""
import numpy as np
from fmpy import read_model_description, simulate_fmu

FMU = "DXCooledAirsideEconomizer.fmu"

md = read_model_description(FMU)
print(f"Variables: {len(md.modelVariables)}")

# Show inputs and outputs
for v in md.modelVariables:
    if v.causality in ("input", "output"):
        print(f"  {v.causality}: {v.name}")

# Create input signal: ramp IT load from 100kW to 500kW, outdoor temp 15-30C
n_steps = 1440  # 24h at 1-min intervals
time = np.linspace(0, 86400, n_steps + 1)

# IT load: ramp up then steady
q_it = np.where(time < 7200, 100000 + (time / 7200) * 400000, 500000)

# Outdoor temp: diurnal sine
t_out = 293.15 + 5 * np.sin(2 * np.pi * time / 86400)

# Build input array
dtype = [("time", np.float64), ("Q_IT", np.float64), ("T_outdoor", np.float64)]
signals = np.array(list(zip(time, q_it, t_out)), dtype=dtype)

print(f"\nRunning FMU for 24h ({n_steps} steps)...")
result = simulate_fmu(
    FMU,
    stop_time=86400,
    step_size=60,
    input=signals,
    output=["P_cooling", "T_indoor", "COP"],
)

p_cool = result["P_cooling"]
t_indoor = result["T_indoor"]
cop = result["COP"]

print(f"Simulated {len(result)} timesteps")
print(f"P_cooling:  min={p_cool.min():.0f} W, max={p_cool.max():.0f} W, mean={p_cool.mean():.0f} W")
print(f"T_indoor:   min={t_indoor.min()-273.15:.1f} C, max={t_indoor.max()-273.15:.1f} C")
print(f"COP:        min={cop.min():.2f}, max={cop.max():.2f}, mean={cop.mean():.2f}")

# Compute PUE at steady state (after ramp)
mask = result["time"] > 7200
if mask.any():
    q_it_ss = 500000
    pue = (q_it_ss + p_cool[mask]) / q_it_ss
    print(f"\nSteady-state PUE: min={pue.min():.3f}, max={pue.max():.3f}, mean={pue.mean():.3f}")

print("\nFMU co-simulation works!")
