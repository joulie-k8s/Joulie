# 02 - Heterogeneous Benchmark

This experiment prepares a heterogeneous simulated cluster from the spreadsheet inventory in `tmp/Joulie heterogeneous cluster.xlsx`.

It uses the new hardware-discovery/inventory path:
- agent publishes `NodeHardware`
- operator resolves discovered hardware against the inventory
- simulator uses the same CPU/GPU inventory for node composition and fallback modeling

## Generate assets

```bash
experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh
```

This refreshes:
- `examples/07 - simulator-gpu-powercaps/manifests/00-kwok-nodes.yaml`
- `examples/07 - simulator-gpu-powercaps/manifests/10-node-classes.yaml`
- `simulator/catalog/hardware.generated.yaml`

## Notes

- The first heterogeneous policy version reasons on CPU and GPU density only.
- Unknown hardware uses per-device fallback.
- This experiment is intended to be the benchmark consumer of the shared hardware inventory and physical model.
