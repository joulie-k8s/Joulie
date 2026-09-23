# Hardware fixture corpus

Every directory here is one real machine, captured as the files and command
output the Joulie agent reads, plus the `NodeHardware` status the agent
publishes for it. `cmd/agent/fixture_corpus_test.go` replays each one through
the real discovery and publish path on every `go test ./...`.

**Contributions of new machines are wanted.** Two machines is not a corpus; the
list at the bottom of this page is short because every entry after the first has
to come from somebody else's hardware. If you own bare metal, a GPU node or an
unusual virtual machine, a capture takes a few minutes and makes your machine a
permanent regression test.

## Why the corpus exists

The agent reads hardware only through files and command output: the powercap
tree under `dvfs.PowercapRoot`, `/proc/cpuinfo` through `procCPUInfoPath`, and
the stdout of `nvidia-smi` and `rocm-smi`. Nothing else. A copy of those inputs
therefore replays the machine exactly, with no hardware and no cluster.

That matters because the defects this project keeps hitting are not logic
errors. They are a machine reporting something no developer thought to invent:
NFD publishing neither `cpu-sockets` nor `cpu-model.name`, so the twin computed
a node TDP of `maxWattsPerSocket` times zero. A hypervisor giving every vCPU its
own `physical id`, so the agent read 28 sockets. A DRAM sub-zone sitting at
`intel-rapl:0:0` with a 47 W maximum and a limit file that rejects writes, one
directory away from the package zone the agent is allowed to cap.

None of those were found by reasoning. They were found by a machine. A hand
written sample proves the parser handles your sample; a capture proves it
handles a computer that exists.

## What a capture proves

Adding your machine asserts, forever after:

- the agent reads its CPU model and its socket count correctly, from whichever
  source that machine actually offers;
- its RAPL zones are classified correctly, so a power cap lands on the package
  zones and on nothing else;
- its GPUs are discovered with the right count, vendor and cap range;
- the `NodeHardware` status published for it does not change silently. If a
  future refactor changes one number for your machine, the golden diff says so
  in the pull request that caused it.

It costs you one capture and costs the project nothing per run: the whole corpus
replays in well under a second.

## Publication

**Captures are published as part of this repository.** They are committed to
git, mirrored by every fork and clone, and stay in the history permanently. By
contributing one you agree to that, and you confirm you are entitled to publish
what the capture contains.

That is why this page is strict about identifying data, and why
`cmd/agent/corpus_validate_test.go` checks every file of every capture on every
test run rather than leaving it to a reviewer's eye. Nothing in a capture is
needed to describe hardware except hardware.

## Layout

```
cmd/agent/testdata/hardware/<machine>/
  powercap/                 copy of /sys/class/powercap, one directory per zone
    intel-rapl:0/name                    package-0
    intel-rapl:0/constraint_0_max_power_uw
    intel-rapl:0/constraint_0_power_limit_uw
    intel-rapl:0/energy_uj
    intel-rapl:0:0/name                  dram
    ...
  cpuinfo                   copy of /proc/cpuinfo
  cpufreq-driver            the active cpufreq scaling driver, if any
  nvidia-smi.txt            stdout of the agent's nvidia-smi query, if any
  rocm-smi.txt              stdout of the agent's rocm-smi query, if any
  node-labels.json          the node's labels as a JSON object, may be {}
  machine.yaml              hand written metadata
  expected.json             generated golden: the published NodeHardware status
  README.md                 optional notes that do not fit in machine.yaml
```

`machine.yaml`, `cpuinfo`, `node-labels.json` and `expected.json` are required.
Everything else is optional, and any other file name is rejected: a forgotten
tarball or an editor backup fails the validation test rather than reaching a
reviewer.

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

### The template

`_template/` is a complete capture with placeholder content, one comment per
key, ready to copy:

```sh
cp -r cmd/agent/testdata/hardware/_template cmd/agent/testdata/hardware/<machine>
rm cmd/agent/testdata/hardware/<machine>/README.md
```

Directories whose name starts with an underscore are not machines. Both the
corpus test and the validation test skip them, and
`TestCorpusListingsSkipUnderscoreDirectories` asserts that they do.

### machine.yaml

`_template/machine.yaml` documents every key. In short:

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
`allocatableGPUCount` reads the GPU resources, so both feed the golden. Take the
values from the kubelet, not from the capture host:

```sh
kubectl get node <node> -o jsonpath='{.status.allocatable}'
```

`writeRejectingZones` names the zones whose `constraint_0_power_limit_uw`
refuses a write on the real machine, typically a disabled DRAM zone.
`TestHardwareFixtureCorpusRAPLWrites` replays that by turning the limit file
into a directory in a temporary copy of the tree, then checks that
`applyRAPLPackageCap` wrote every package zone it should and reported exactly
the failures it should. For a machine whose only rejecting zones are DRAM
sub-zones, the assertion is that nothing changes at all: zones are selected by
their `name` file, never by a directory pattern.

## Adding a machine

All you need is the node and a shell. A Go checkout is optional: it buys you
step 5, and a maintainer can do that step for you after your pull request is
open. Nothing registers a machine anywhere. The tests list the corpus directory
and run whatever is in it, so a capture is added by adding a directory and
removed by deleting one.

1. **Capture it.** On the node, or in a debug pod that sees the host `/sys`:

   ```sh
   sudo sh scripts/collect-hardware-fixture.sh --name <machine> --out /tmp/fixtures
   ```

   `scripts/collect-hardware-fixture.sh --help` documents the `kubectl` route as
   well. The script writes the layout above, redacts what it can, and tars the
   directory. It never writes `expected.json`.

2. **Review the output, file by file, before it leaves the machine.** The
   redaction in the script is a first pass, not a guarantee: it cannot know that
   a node label carries your team's name or that a rack position is in there.
   Open every file. `node-labels.json` is where identifying data hides most
   often.

3. **Unpack it into the corpus:**

   ```sh
   tar -xf <machine>.tar -C cmd/agent/testdata/hardware/
   ```

4. **Fill in `machine.yaml`.** Copy `_template/machine.yaml` over the skeleton
   the script wrote if you prefer its comments. `description` and `source` are
   prose a stranger has to be able to act on; the validation test rejects a
   `TODO` left in either.

5. **Optional, and only if you have Go: pin the output.**

   ```sh
   go test ./cmd/agent/ -run Corpus -update
   ```

   Then read what changed under the corpus directory. That is the agent's own
   conclusion about your machine. If a number in it is wrong, the fixture or the
   agent is wrong; say so in the pull request rather than editing the golden.

   Skipping this step does not hold the machine back. Without `expected.json`
   the corpus test still replays your capture through the real discovery path on
   every `go test ./cmd/agent/`, which is the half that catches a parser falling
   over on your hardware, and it reports the machine as skipped rather than
   failing the build. The golden adds the other half, that the output stops
   changing.

6. **Run the validation test:**

   ```sh
   go test ./cmd/agent/ -run TestHardwareCorpusIsValid -v
   ```

   It checks the file layout, the metadata, the parsers, and scans every file
   for identifying data. Each failure names the file, the line and what to
   change.

7. **Run the whole package and open the pull request:**

   ```sh
   go test ./cmd/agent/...
   ```

   Use the checklist template by appending `?template=hardware-fixture.md` to
   the pull request URL:

   ```
   https://github.com/joulie-k8s/Joulie/compare/main...<your-branch>?template=hardware-fixture.md
   ```

## What is never accepted

**Anything identifying.** `cmd/agent/corpus_validate_test.go` fails the build on
a MAC address, an IPv4 or IPv6 address, a UUID of any kind including a GPU or
MIG UUID, a serial number, an asset tag, a hostname or FQDN, a username or home
directory path, and a cloud instance or provider id, in any file of any capture.
It uses the same patterns the capture script redacts with, so the two cannot
drift apart.

The check is a backstop, not a reviewer. It cannot recognise a bare hostname
with no dots in it, a project code name, or a rack label. That is step 2 above,
and it is yours.

Things that look identifying and are not, and which the test is careful to let
through: powercap zone names such as `intel-rapl:0:0`, driver versions such as
`535.183.01`, kernel versions, `bogomips`, CPU flag lists and CPU model strings
full of numbers.

**Anything hand edited into `expected.json`.** It is the output of the agent's
own status writer, serialised as indented JSON with `updatedAt` removed. A
golden that was adjusted until it looked right is worse than no golden: it
asserts the bug. Generate it with `-update` and nothing else.

**A capture that was hand written rather than captured.** The one exception in
the corpus, `xeon-4socket-no-labels`, says so in its `source` and explains
exactly what was reconstructed and from what. If you have to do the same, say so
there.

**A machine that proves nothing new.** `description` has to answer "which
discovery path does this exercise that no other corpus machine does". A second
machine of a kind already represented is a cost with no benefit.

## Portability

The validation test reads the corpus root from the `JOULIE_HARDWARE_CORPUS`
environment variable and falls back to this directory, so the corpus can later
move to a repository of its own without the test being rewritten:

```sh
JOULIE_HARDWARE_CORPUS=/path/to/corpus go test ./cmd/agent/ -run TestHardwareCorpusIsValid
```

## The one input the corpus cannot replay

`detectGPUVendor` stats `/dev/nvidiactl` through a literal path, with no
variable to redirect the way `procCPUInfoPath` and `dvfs.PowercapRoot` can be
redirected. On a host that has an NVIDIA driver installed, a machine captured
without a GPU would be discovered as vendor `nvidia`, so its golden would
describe the host rather than the fixture. The test skips such a machine on
such a host and says so. To run or regenerate it there, either use a host
without an NVIDIA driver, or run the command inside a clean `/dev`:

```sh
bwrap --bind / / --dev /dev --chdir "$PWD" go test ./cmd/agent/ -run Corpus -update
```

Making that path a package variable would remove the caveat, and is the only
non-test change this corpus would need.

## The machines

| Machine | What it is | Why it is here |
|---|---|---|
| `xeon-4socket-no-labels` | Bare metal 4 socket Intel Xeon Gold 6252, 192 logical CPUs, 165 W per package zone, DRAM sub-zones at 47.25 W, no GPU | NFD publishes neither `cpu-sockets` nor `cpu-model.name`, so sockets and model both come from `/proc/cpuinfo`. The DRAM sub-zones are the trap that zone selection by name exists to avoid. |
| `vm-no-rapl` | VM with one NVIDIA Tesla T4, no powercap tree at all | The hypervisor gives every vCPU its own `physical id`, so the agent reads 28 sockets. No RAPL means no CPU cap range and no CPU control, and the GPU path has to carry the node on its own. |
