# Cooling System Impact on Data Center Energy Efficiency

## A Comparative Analysis of Cooling Technologies with Joulie Energy-Aware Scheduling

### 1. Introduction

This report evaluates how different cooling system technologies interact with
energy-aware workload scheduling to determine total data center facility energy
consumption. We simulate a heterogeneous GPU cluster (8 CPU-only + 33 GPU nodes,
~200 GPUs) over 60 days under three scheduling baselines and six cooling models,
including a physics-based Functional Mock-up Unit (FMU) compiled from a Modelica
model adapted from the Lawrence Berkeley National Laboratory (LBL) Buildings
library.

The key question: **does the choice of cooling system amplify or diminish the
energy savings achieved by Joulie's energy-aware scheduling?**

### 2. Experimental Setup

#### 2.1 Cluster Configuration

| Component | Count | Idle Power | Peak Power |
|---|---|---|---|
| CPU nodes (EPYC 9965/9375F/9655) | 8 | 90 W | 700 W |
| GPU nodes (8x H100 NVL per node) | 33 | 600 W | 5,200 W |
| **Cluster total** | **41** | **20.5 kW** | **177.2 kW** |

#### 2.2 Workload Profile (60 days, 5-minute resolution)

The synthetic workload models realistic data center operations:

- **Diurnal cycle**: peak utilization at ~14:00 (60-95%), trough at ~04:00
  (25-40%), reflecting business-hours-heavy inference and batch scheduling
- **Weekend dip**: batch/HPC workloads drop ~30% on weekends
- **GPU training jobs**: Poisson-distributed arrivals (~2/day), 1-8 hour
  duration, 15-35% cluster impact with 10-minute ramp-up
- **Inference bursts**: sharp spikes (~8/day), 5-45 minute duration
- **Batch HPC**: moderate jobs (~5/day), 30 min to 3 hours
- **Noise**: Gaussian perturbation on load, CPU/GPU utilization

#### 2.3 Weather Profile

Outdoor temperature follows a spring-to-early-summer progression (March-May):

- **Seasonal trend**: 15°C baseline rising to 25°C over 60 days
- **Diurnal swing**: ±7°C (peak at 15:00, trough at 05:00)
- **Weather systems**: 3-5 day cycles (±3°C)
- **Noise**: ±1.2°C Gaussian

#### 2.4 Scheduling Baselines

| Baseline | Strategy | IT Energy (60 days) |
|---|---|---|
| A — No Joulie | All nodes run uncapped | 173,035 kWh |
| B — Static partition | 30% performance / 70% eco-mode (65% CPU cap, 70% GPU cap) | 139,480 kWh (−19.4%) |
| C — Queue-aware | Dynamic 15-40% perf fraction based on queue depth, tighter caps at low load | 132,333 kWh (−23.5%) |

#### 2.5 Cooling Models

Five parametric models and one physics-based FMU model:

1. **Air-cooled (CRAH)** — Traditional computer room air handler. High baseline
   overhead, COP degrades with outdoor temperature. PUE = 1.30 + 0.015 per °C
   above 15°C.

2. **Hot/cold aisle** — Containment with improved airflow management. PUE =
   1.15 + 0.010 per °C above 15°C.

3. **Direct liquid cooling (DLC)** — 75% of heat removed by liquid loop (COP
   ~20), 25% residual air cooling. Nearly outdoor-temp independent for the
   liquid fraction.

4. **Immersion cooling** — Servers submerged in dielectric fluid. COP ~25,
   minimal temperature sensitivity.

5. **Free cooling (economizer)** — Air-side economizer. 100% free cooling below
   18°C, 100% mechanical above 28°C, linear blend in between.

6. **FMU (DXCooledAirsideEconomizer)** — Physics-based model adapted from
   [LBL Buildings Library v12.1.0](https://simulationresearch.lbl.gov/modelica/releases/v12.1.0/help/Buildings_Applications_DataCenters_DXCooled_Examples.html).
   Variable-speed DX compressor (4 stages, COP nominal = 3.0), airside
   economizer with three cooling modes, fan affinity laws, and room thermal
   mass. Compiled to an FMI 2.0 co-simulation FMU using the
   `openmodelica/openmodelica:v1.26.3-ompython` Docker container.

### 3. Results

#### 3.1 Mean PUE by Cooling System

| Cooling Model | Baseline A | Baseline B | Baseline C |
|---|---|---|---|
| Air-cooled (CRAH) | 1.387 | 1.387 | 1.387 |
| Hot/cold aisle | 1.208 | 1.208 | 1.208 |
| DLC | 1.070 | 1.070 | 1.070 |
| Immersion | 1.060 | 1.060 | 1.060 |
| Free cooling | 1.147 | 1.147 | 1.148 |
| FMU (DX+economizer) | 1.271 | 1.265 | 1.270 |

The parametric models produce identical PUE across baselines because they depend
only on outdoor temperature. The FMU model shows slight PUE variation because
its thermal dynamics (room mass, compressor part-load behavior) respond
differently to the load profiles of each baseline.

#### 3.2 Total Facility Energy (60 days)

| Cooling Model | Baseline A (MWh) | Baseline B (MWh) | Baseline C (MWh) | B savings vs A | C savings vs A |
|---|---|---|---|---|---|
| Air-cooled (CRAH) | 240.9 | 194.1 | 184.1 | 19.4% | 23.6% |
| Hot/cold aisle | 209.6 | 168.9 | 160.2 | 19.4% | 23.6% |
| DLC | 185.2 | 149.3 | 141.6 | 19.4% | 23.5% |
| Immersion | 183.5 | 147.9 | 140.3 | 19.4% | 23.5% |
| Free cooling | 200.1 | 161.2 | 152.7 | 19.4% | 23.7% |
| FMU (DX+economizer) | 219.8 | 176.6 | 168.1 | 19.6% | 23.5% |

#### 3.3 Cooling Energy Overhead

| Cooling Model | Overhead (% of IT energy) |
|---|---|
| Air-cooled (CRAH) | 39.2% |
| FMU (DX+economizer) | 27.0% |
| Hot/cold aisle | 21.2% |
| Free cooling | 15.5% |
| DLC | 7.0% |
| Immersion | 6.0% |

The FMU model's 27% overhead is realistic for a DX-cooled data center with
partial economizer capability — it sits between traditional CRAH (39%) and
modern hot/cold aisle (21%).

#### 3.4 Key Observations

**1. Scheduling savings are approximately constant across cooling technologies.**
Baselines B and C achieve ~19.4% and ~23.5% facility energy savings regardless
of cooling system. This is because the parametric models apply a multiplicative
PUE factor: reducing IT power by X% reduces facility power by approximately X%.

**2. The FMU model shows the largest relative savings from scheduling (19.6% for
B).** The DX compressor's part-load COP improvement means that reducing IT power
has a compounding effect — lower heat load lets the compressor run at a more
efficient operating point.

**3. Absolute cooling energy savings scale with cooling overhead.** Switching
from CRAH to immersion cooling saves 57,473 kWh over 60 days under Baseline A
— more than the 40,701 kWh saved by switching from Baseline A to B with CRAH.
This suggests that **cooling system selection can have a larger impact than
scheduling optimization** for facilities with inefficient cooling.

**4. Free cooling effectiveness varies with season.** The 60-day simulation
shows PUE rising from ~1.05 in early spring (outdoor temps <18°C, full free
cooling) to ~1.30 in late spring (>28°C, full mechanical). This seasonal
variation is clearly visible in the time-series plots and demonstrates why
static PUE assumptions are insufficient for energy planning.

**5. The FMU captures thermal dynamics invisible to parametric models.** The
room thermal mass (50×40×3 m room, ~5 MJ/K effective capacitance) smooths
transient load spikes. When a GPU training job starts suddenly, the FMU shows
cooling power ramping up over ~15 minutes rather than instantly, which is
physically accurate.

### 4. FMU Model Details

The physics-based model is adapted from the LBL Buildings Library
`DXCooledAirsideEconomizer` example. The original model is a self-contained
Modelica simulation with its own weather reader and fixed heat load. We adapted
it for FMU co-simulation by:

1. Replacing the TMY3 weather reader with an external `T_outdoor` input (K)
2. Replacing the fixed `QRooInt_flow = 500 kW` with a `Q_IT` input (W)
3. Adding `P_cooling`, `T_indoor`, and `COP` outputs
4. Preserving the core physics: three cooling modes (free/partial/full
   mechanical), airside economizer mixing, variable-speed DX compressor with
   temperature-dependent COP, fan affinity laws (P ~ speed^3), and room
   thermal mass dynamics

The model is compiled to an FMI 2.0 co-simulation FMU using OpenModelica inside
a Docker container (`openmodelica/openmodelica:v1.26.3-ompython`), requiring no
local Modelica toolchain installation. The FMU is then executed via Docker to
handle glibc compatibility, with input/output data exchanged via CSV files.

### 5. Limitations

- **Synthetic data**: The workload and weather profiles are generated, not
  measured. Real data center loads have more complex patterns (maintenance
  windows, capacity planning events, correlated failures).
- **Simplified parametric models**: The five built-in models use static
  equations without thermal dynamics, humidity effects, or equipment aging.
- **FMU simplification**: The adapted FMU preserves the key physics but
  simplifies the air mixing and compressor models compared to the full Buildings
  library implementation, which includes detailed DX coil performance curves,
  wet-bulb temperature effects, and refrigerant dynamics.
- **Single climate**: Results use one spring-to-summer temperature profile.
  Arctic, tropical, and desert climates would show very different economizer
  and CRAH behavior.

### 6. Conclusions

1. **Energy-aware scheduling (Joulie) saves 19-24% of total facility energy**,
   and this benefit is consistent across all cooling technologies tested.

2. **Cooling system selection matters as much as scheduling**: the gap between
   CRAH (PUE 1.39) and immersion cooling (PUE 1.06) represents a 33% overhead
   reduction, comparable to the scheduling savings.

3. **Physics-based FMU models capture effects invisible to parametric models**:
   thermal inertia, economizer mode switching, and part-load COP variation.
   These are important for accurate energy forecasting and capacity planning.

4. **The combination of efficient cooling + smart scheduling yields the best
   results**: Baseline C with immersion cooling uses 140,299 kWh over 60 days,
   versus 240,924 kWh for Baseline A with CRAH — a **41.8% total reduction**.

### 7. Reproducing This Analysis

```bash
cd examples/08-fmu-cooling-pue

# Generate 60-day synthetic data
python 01_generate_synthetic_timeseries.py

# Build the FMU (requires Docker)
python 04_build_fmu.py --model-file cooling_models/DXCooledAirsideEconomizer.mo

# Apply all cooling models + FMU
python 02_apply_cooling_models.py --fmu cooling_models/DXCooledAirsideEconomizer.fmu

# Generate plots
python 03_plot_pue_comparison.py
```

All plots are written to `plots/`. Summary statistics are in
`data/cooling_model_summary.csv`.
