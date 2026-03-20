# Integration Tests

Integration tests run against a single-node k3s cluster in Dagger CI.

## Test IDs

| ID | Name | Description |
|----|------|-------------|
| IT-ARCH-01 | Smoke install | Install full stack, assert CRDs registered |
| IT-HW-01 | NodeHardware publish | Agent publishes NodeHardware |
| IT-TWIN-01 | NodeTwinState writing | Operator writes NodeTwinState |
| IT-SCHED-01 | Scheduler filter | Eco node filter for performance pods |
| IT-SCHED-02 | Scheduler scoring | NodeTwinState influences score |
| IT-FSM-01 | FSM still works | Existing profile FSM transitions |
| IT-SIM-01 | Simulator run | Short end-to-end simulator run |

## Running locally

```bash
# Install Dagger CLI
brew install dagger/tap/dagger  # or see https://docs.dagger.io

# Run all integration tests
cd ci && dagger call integration

# Run a specific test (if supported)
cd ci && dagger call integration --test IT-TWIN-01
```

## Design notes

- Tests run against single-node k3s (no multi-node)
- Scheduler scoring tests use synthetic NodeTwinState fixtures
- Agent hardware discovery uses simulated mode in CI (no real RAPL/GPU)
