# Benchmark Report Assets

This folder contains the minimal artifacts needed by `REPORT.md`, extracted from the local `results/` directory so the report can be published without committing full run outputs.

Included:

- `data/summary.csv` (aggregated metrics table used in the report)
- `plots/runtime_distribution.png`
- `plots/energy_vs_makespan.png`
- `plots/baseline_means.png`

Not included:

- per-run raw logs and pod snapshots
- full `results/traces/`
- `throughput_vs_energy.png` (intentionally excluded from report commentary)
