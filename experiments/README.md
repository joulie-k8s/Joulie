# Experiments

This directory contains benchmark/experiment harnesses.

- [01-cpu-only-benchmark](./01-cpu-only-benchmark/README.md): reproducible baseline sweep (A/B/C) over kind+KWOK with simulator + optional Joulie control.
- [05-governor-exploration](./05-governor-exploration/README.md): CPU and GPU governor presets as an energy dimension: an offline sweep of every preset on emulated nodes (`simulator/cmd/govsweep`) and one arm per run on a simulated cluster.
