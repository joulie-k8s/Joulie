# FMU Cooling + PUE Post-Processing

Apply different cooling system models to Joulie simulator IT power traces and
compute realistic PUE curves. Supports both simple parametric models and a
physics-based Modelica FMU compiled via Docker (no local OpenModelica needed).

## Quick Start (parametric models only — no external dependencies)

```bash
# 1. Generate synthetic timeseries (60 days, 5-min resolution)
python 01_generate_synthetic_timeseries.py

# 2. Apply built-in parametric cooling models
python 02_apply_cooling_models.py

# 3. Generate plots
python 03_plot_pue_comparison.py
```

## With OpenModelica FMU (physics-based cooling)

The FMU is built and run inside a Docker container — no local OpenModelica
installation required. The only prerequisites are Docker and Python.

### Prerequisites

```bash
# Docker (required for FMU build and execution)
docker pull openmodelica/openmodelica:v1.26.3-ompython

# Python dependencies
pip install numpy pandas matplotlib fmpy
```

### Build and Run

```bash
# 1. Compile the Modelica cooling model into an FMU (uses Docker)
python 04_build_fmu.py --model-file cooling_models/DXCooledAirsideEconomizer.mo

# 2. Apply ALL cooling models (parametric + FMU) — FMU runs inside Docker
python 02_apply_cooling_models.py --fmu cooling_models/DXCooledAirsideEconomizer.fmu

# 3. Generate plots
python 03_plot_pue_comparison.py
```

You can also build a custom Modelica model:

```bash
python 04_build_fmu.py --model-file path/to/your_model.mo
python 04_build_fmu.py --model-file your_model.mo --install-buildings  # if it uses LBL Buildings library
```

## The DXCooledAirsideEconomizer Model

The FMU model in `cooling_models/DXCooledAirsideEconomizer.mo` is adapted from
the LBL Buildings Library example:

> **Buildings.Applications.DataCenters.DXCooled.Examples.DXCooledAirsideEconomizer**
> https://simulationresearch.lbl.gov/modelica/releases/v12.1.0/help/Buildings_Applications_DataCenters_DXCooled_Examples.html

The original example is a self-contained simulation with a TMY3 weather reader,
fixed 500 kW IT load, variable-speed DX compressor (4 stages), airside
economizer with mixing box, and PID-controlled fan speed. It cannot be used
directly as a co-simulation FMU because it has no external inputs.

### Adaptations for FMU co-simulation

We replaced the fixed internal components with external `RealInput`/`RealOutput`
connectors while preserving the key physics:

| Original (Buildings library) | Adapted (FMU wrapper) |
|---|---|
| `weaDat` TMY3 file reader | `T_outdoor` input (K) — driven by our timeseries |
| `QRooInt_flow = 500000` (fixed) | `Q_IT` input (W) — driven by IT power trace |
| Internal `PHVAC` computation | `P_cooling` output (W) — compressor + fan |
| `roo.TRooAir` internal variable | `T_indoor` output (K) |
| No COP output | `COP` output — system-level COP |

### Physics preserved from the original

- **Three cooling modes**: free cooling (outdoor air only), partial mechanical
  (economizer + DX), full mechanical (DX compressor only) — switching based on
  outdoor temperature vs supply air setpoint
- **Airside economizer**: outdoor air fraction varies from 5% (minimum fresh air)
  to 100% (full free cooling), with mixing temperature computation
- **Variable-speed DX compressor**: COP degrades with outdoor temperature
  (condenser effect) and improves at part load (cycling gain), matching the
  4-stage performance curves from the Buildings example (COP_nominal = 3.0)
- **Fan affinity laws**: fan power scales with speed cubed (`P ∝ speed³`)
- **Room thermal mass**: 50×40×3 m data center room with `der(T_indoor)` ODE,
  so temperature responds dynamically to load transients (not instantaneous)

### Parameters (from Buildings example)

| Parameter | Value | Source |
|---|---|---|
| Room dimensions | 50 × 40 × 3 m | `Buildings.Examples.ChillerPlant.BaseClasses.SimplifiedRoom` |
| COP nominal | 3.0 | DX coil performance data (all 4 stages) |
| Nominal airflow | 82.84 kg/s | Computed from `QRooC_flow_nominal / (cp * ΔT)` |
| Fan pressure rise | 500 Pa | `per(pressure(..., dp=500*{2,0}))` |
| Economizer threshold | 13-18°C | `CoolingMode` controller `dT=1`, supply setpoint 18°C |
| Minimum fan speed | 0.2 (20%) | `minSpeFan` parameter |

## Using real experiment data

Point the scripts at your experiment results:

```bash
python 02_apply_cooling_models.py --data-dir ../../experiments/02-heterogeneous-benchmark/runs/latest/results/
python 03_plot_pue_comparison.py --data-dir ../../experiments/02-heterogeneous-benchmark/runs/latest/results/
```

## Cooling Models

| Model | Type | Mean PUE | Description |
|---|---|---|---|
| Air-cooled (CRAH) | Parametric | ~1.39 | Traditional computer room air handler |
| Hot/cold aisle | Parametric | ~1.21 | Containment with improved airflow |
| Direct liquid cooling | Parametric | ~1.07 | Direct-to-chip liquid loops |
| Immersion cooling | Parametric | ~1.06 | Servers submerged in dielectric fluid |
| Free cooling | Parametric | ~1.15 | Air-side economizer, climate-dependent |
| DXCooledAirsideEconomizer | FMU | ~1.27 | Physics-based, adapted from LBL Buildings library |

## Report

See [REPORT.md](REPORT.md) for a detailed analysis of the results, including
mean PUE comparison, facility energy savings across baselines and cooling
systems, and discussion of how the FMU model captures thermal dynamics invisible
to parametric models.

## Architecture

```
01_generate_synthetic_timeseries.py  →  data/timeseries_baseline_{A,B,C}.csv
                                              ↓
02_apply_cooling_models.py           →  data/timeseries_baseline_*_cooling_*.csv
  (parametric models run in Python)     data/cooling_model_summary.csv
  (FMU model runs inside Docker)
                                              ↓
03_plot_pue_comparison.py            →  plots/*.png

04_build_fmu.py                      →  cooling_models/*.fmu
  (compiles .mo → .fmu via Docker)
```
