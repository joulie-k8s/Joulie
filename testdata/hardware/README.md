# Hardware fixture corpus

The Joulie agent reads hardware only through files and command output: the
powercap tree under `dvfs.PowercapRoot`, `/proc/cpuinfo` through
`procCPUInfoPath`, and the stdout of `nvidia-smi` and `rocm-smi`. A copy of
those inputs therefore replays a machine exactly, with no hardware and no
cluster. Each directory here is one real machine, and
`cmd/agent/fixture_corpus_test.go` runs the real discovery and publish path
against every one of them.

The point of the corpus is that a bug report from an adopter becomes a test.
Ask them for a dump, add it here, and the machine is regression tested forever
after.

## Layout

```
testdata/hardware/<machine>/
  powercap/                 copy of /sys/class/powercap, one directory per zone
    intel-rapl:0/name                    package-0
    intel-rapl:0/constraint_0_max_power_uw
    intel-rapl:0/constraint_0_power_limit_uw
    intel-rapl:0/energy_uj
    intel-rapl:0:0/name                  dram
    ...
  cpuinfo                   copy of /proc/cpuinfo
  nvidia-smi.txt            stdout of the agent's nvidia-smi query, if any
  rocm-smi.txt              stdout of the agent's rocm-smi query, if any
  node-labels.json          the node's labels as a JSON object, may be {}
  machine.yaml              hand written metadata
  expected.json             generated golden: the published NodeHardware status
```

`powercap/` is absent when the node has no RAPL. `nvidia-smi.txt` and
`rocm-smi.txt` are absent when the tool is not installed, and the test's command
stub then fails those commands the way a missing binary does.

The captured GPU queries are the exact ones `cmd/agent/main.go` runs:

```
nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits
rocm-smi --showpowercap --showproductname --json
```

`nvidia-smi -L` and `rocm-smi --showproductname`, which `detectGPUVendor` uses
only as a presence probe, are not captured: their output is a list of GPU UUIDs
and nothing reads it. The stub answers them with success whenever the machine
has the matching query file.

### machine.yaml

```yaml
description: what kind of machine this is, and why it is worth keeping
source: where the dump came from, and who reviewed it
capturedAt: "2026-09-23T07:26:32Z"
allocatable:            # what goes on the fake Node object
  cpu: "192"
  memory: "394993664Ki"
  gpu:
    nvidia.com/gpu: "1"
writeRejectingZones:    # zone directories whose limit file rejects writes
  - "intel-rapl:0:0"
```

`allocatable` matters: `cpuCoresFromNode` reads the CPU capacity and
`allocatableGPUCount` reads the GPU resources, so both feed the golden.

`writeRejectingZones` names the zones whose `constraint_0_power_limit_uw`
refuses a write on the real machine, typically a disabled DRAM zone.
`TestHardwareFixtureCorpusRAPLWrites` replays that by turning the limit file
into a directory in a temporary copy of the tree, then checks that
`applyRAPLPackageCap` wrote every package zone it should and reported exactly
the failures it should. For a machine whose only rejecting zones are DRAM
sub-zones, the assertion is that nothing changes at all: zones are selected by
their `name` file, never by a directory pattern.

## Adding a machine

1. Capture it. On the node, or in a debug pod that sees the host `/sys`:

   ```
   sudo sh hack/collect-hardware-fixture.sh --name <machine> --out /tmp/fixtures
   ```

   `hack/collect-hardware-fixture.sh --help` documents the `kubectl` route as
   well. The script writes the layout above and tars it. It never writes
   `expected.json`.

2. Unpack it here and read every file:

   ```
   tar -xf <machine>.tar -C testdata/hardware/
   ```

3. Complete `machine.yaml`: `description`, `source`, the allocatable the
   kubelet actually reports, and any `writeRejectingZones`.

4. Generate the golden and review the diff:

   ```
   go test ./cmd/agent/ -run Corpus -update
   git diff testdata/hardware
   ```

5. Check it passes without the flag, then commit:

   ```
   go test ./cmd/agent/...
   ```

## What must never be committed

The script redacts these before writing anything, but review the files anyway:

- GPU UUIDs and MIG UUIDs, and any other UUID
- serial numbers and asset tags
- MAC addresses
- IPv4 and IPv6 addresses
- the machine's hostname in every spelling, short, fully qualified and the
  domain on its own, including inside `node-labels.json`

Node labels are the easiest place for a site name or a team name to slip
through. Read `node-labels.json` line by line before adding it.

## expected.json is generated

It is the output of the agent's own status writer, serialised as indented JSON
with `updatedAt` removed. Never hand edit it. If it looks wrong, the fixture or
the code is wrong; fix that and regenerate with `-update`.

## The one input the corpus cannot replay

`detectGPUVendor` stats `/dev/nvidiactl` through a literal path, with no
variable to redirect the way `procCPUInfoPath` and `dvfs.PowercapRoot` can be
redirected. On a host that has an NVIDIA driver installed, a machine captured
without a GPU would be discovered as vendor `nvidia`, so its golden would
describe the host rather than the fixture. The test skips such a machine on
such a host and says so. To run or regenerate it there, either use a host
without an NVIDIA driver, or run the command inside a clean `/dev`:

```
bwrap --bind / / --dev /dev --chdir "$PWD" go test ./cmd/agent/ -run Corpus -update
```

Making that path a package variable would remove the caveat, and is the only
non-test change this corpus would need.

## The machines

| Machine | What it is | Why it is here |
|---|---|---|
| `xeon-4socket-no-labels` | Bare metal 4 socket Intel Xeon Gold 6252, 192 logical CPUs, 165 W per package zone, DRAM sub-zones at 47.25 W, no GPU | NFD publishes neither `cpu-sockets` nor `cpu-model.name`, so sockets and model both come from `/proc/cpuinfo`. The DRAM sub-zones are the trap that zone selection by name exists to avoid. |
| `vm-no-rapl` | VM with one NVIDIA Tesla T4, no powercap tree at all | The hypervisor gives every vCPU its own `physical id`, so the agent reads 28 sockets. No RAPL means no CPU cap range and no CPU control, and the GPU path has to carry the node on its own. |
