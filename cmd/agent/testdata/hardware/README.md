# Hardware fixture corpus

Every directory here is one real machine, captured as the files and command
output the Joulie agent reads, plus the `NodeHardware` status the agent
publishes for it. `cmd/agent/fixture_corpus_test.go` replays each one through
the real discovery and publish path on every `go test ./...`.

**Contributions of new machines are wanted.** Three machines is not a corpus;
the list at the bottom of this page is short because every entry after the first
has to come from somebody else's hardware. If you own bare metal, a GPU node or an
unusual virtual machine, a capture takes a few minutes and makes your machine a
permanent regression test.

## Why the corpus exists

The agent reads hardware only through files and command output: the powercap
tree under `dvfs.PowercapRoot`, `/proc/cpuinfo` through `procCPUInfoPath`, the
cpufreq governor state and the amdgpu power cap attributes under
`dvfs.HostSysRoot`, and the stdout of `nvidia-smi`. Nothing else. A copy of those inputs therefore replays the
machine exactly, with no hardware and no cluster.

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
- when the capture holds governor state, its CPU governor, energy-performance
  preference, boost, driver mode and frequency range are published as the node
  runs them, and the presets it accepts follow from them;
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
  amdgpu-hwmon/             the power cap of each amdgpu device, if any
    gpu0/power1_cap                      the cap in force, in microwatts
    gpu0/power1_cap_min
    gpu0/power1_cap_max
    gpu0/power1_cap_default
    ...                                  one directory per device
  node-labels.json          the node's labels as a JSON object, may be {}
  machine.yaml              hand written metadata
  expected.json             generated golden: the published NodeHardware status
  README.md                 optional notes that do not fit in machine.yaml
  governor/                 the frequency governor state, if any
    cpufreq/policy0/scaling_governor     powersave
    cpufreq/policy0/scaling_available_governors
    cpufreq/policy0/energy_performance_preference
    cpufreq/policy0/energy_performance_available_preferences
    cpufreq/policy0/boost
    cpufreq/policy0/cpuinfo_min_freq
    cpufreq/policy0/cpuinfo_max_freq
    ...                                  one directory per policy
    cpufreq/boost                        the global boost switch
    intel_pstate/status                  active or passive
    intel_pstate/no_turbo
    amd_pstate/status
    cpuidle/state0/name                  the idle states of CPU 0
    amdgpu/gpu0/power_dpm_force_performance_level
    amdgpu/gpu0/pp_dpm_sclk              and mclk, fclk, socclk, pcie, device
    nvidia-smi-clocks.txt                nvidia-smi -q -d CLOCK,SUPPORTED_CLOCKS,PERFORMANCE
```

`machine.yaml`, `cpuinfo`, `node-labels.json` and `expected.json` are required.
Everything else is optional, and any other file name is rejected: a forgotten
tarball or an editor backup fails the validation test rather than reaching a
reviewer.

`powercap/` is absent when the node has no RAPL. `nvidia-smi.txt` is absent
when the tool is not installed, and the test's command stub then fails the
NVIDIA commands the way a missing binary does. Every `rocm-smi` command fails
the same way: the agent reads and caps AMD GPUs through sysfs.

The captured NVIDIA query is the exact one `cmd/agent/main.go` runs:

```
nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits
```

Where the driver gives no value, nvidia-smi prints a bracketed token in its
place, such as `[N/A]` or `[Not Supported]`; keep it as printed. The validation
test accepts any bracketed token in the four power columns, the agent reads one
in a limit column as a GPU without a settable power limit, and
`hwemu.FromCorpus` renders it for the emulated tool to print back. A capture
whose `power.limit` and `power.max_limit` are both tokens, as on a vGPU guest,
has no limit to emulate: `TestHwemuA01` skips it, and the flat replay still
checks its golden.

**Wanted: a GPU whose power limit cannot be set.** The corpus has no capture
with tokens in its limits yet. A vGPU guest, or a GeForce laptop at driver R530
or later, is the likeliest to print them. Along with the capture, put in the
pull request the output of these two commands, with `<W>` inside the range the
GPU lists, or any value when it lists none:

```sh
sudo nvidia-smi -i 0 -pl <W>; echo $?
sudo nvidia-smi -i 0 -pl <W> --error-on-warning; echo $?
```

They settle how nvidia-smi reports a refused limit, and which exit code each
form returns. If the first one sets the limit, put the captured `power.limit`
back with `sudo nvidia-smi -i 0 -pl <captured limit>`.

The same goes for the clock locks on a GPU that refuses them, such as `-lmc`
on a GPU without a run-time memory lock or `-lgc` on one older than Volta.
With `<min>,<max>` two clocks that `nvidia-smi -q -d SUPPORTED_CLOCKS` lists,
put in the pull request the output and exit code of:

```sh
sudo nvidia-smi -i 0 -lgc <min>,<max>; echo $?
sudo nvidia-smi -i 0 -lgc <min>,<max> --error-on-warning; echo $?
sudo nvidia-smi -i 0 -lmc <min>,<max>; echo $?
sudo nvidia-smi -i 0 -lmc <min>,<max> --error-on-warning; echo $?
```

A lock that is taken stays until reset, so run these afterwards, and add
their output and exit codes too:

```sh
sudo nvidia-smi -i 0 -rgc; echo $?
sudo nvidia-smi -i 0 -rmc; echo $?
```

`nvidia-smi -L`, which `detectGPUVendor` uses only as a presence probe, is not
captured: its output is a list of GPU UUIDs and nothing reads it. The stub
answers it with success whenever the machine has `nvidia-smi.txt`.

`amdgpu-hwmon/` holds, for each amdgpu device, the four power cap attributes
of its amdgpu hwmon device, copied verbatim from sysfs: whole microwatts, or
the empty line the driver prints when the firmware query fails. The devices
are numbered `gpu0`, `gpu1` in bus id order, the order the agent lists them
in, so the bus id never leaves the machine. Both replays read it:
`pointAgentAtFixture` lays each device out under a `bus/pci/devices` entry of
the replayed `/sys`, and `hwemu.FromCorpus` renders amdgpu devices with the
same attributes, which holds one value for all devices, so devices that differ
are an error there.

### Governor state

`governor/` holds what the governor discovery reads, each file under the
name sysfs gives it, and only the files the capture script copies; any other
file there is rejected. Sysfs files are copied verbatim. The amdgpu devices are
numbered `gpu0`, `gpu1` in bus id order, the order the agent lists them in, so
the bus id itself never leaves the machine; `nvidia-smi-clocks.txt` goes
through the same redaction as every other command output, bus ids included.
Before it writes the tar, the script's identifier check reads every file under
`governor/` and stops the capture when a PCI bus id, a serial number or a host
name is still in one. Run it again on a capture you edited by hand:

```sh
sh scripts/collect-hardware-fixture.sh --check cmd/agent/testdata/hardware/<machine> --node <node>
```

A capture with `governor/cpufreq/` needs `cpufreq-driver` beside it, and every
captured policy needs its `scaling_governor`. The policies must all be in one
state, because an emulated node holds one state for all of them, and must be
the CPUs of the cpuinfo, one policy per CPU or per core.

What each replay does with it:

- `pointAgentAtFixture` builds a `/sys` tree from `cpufreq/`,
  `intel_pstate/`, `amd_pstate/` and `cpuidle/`, writes `cpufreq-driver` into
  every policy as its `scaling_driver`, and points `dvfs.HostSysRoot` at it.
- `hwemu.FromCorpus` renders the captured governor, available governors,
  preference pair, boost, driver mode and frequency range, and
  `TestH21FromCorpusReplaysEveryMachine` reads them back byte for byte. A
  capture with `cpufreq-driver` alone still renders `scaling_driver` alone.
- `amdgpu/` and `nvidia-smi-clocks.txt` are recorded for review and for
  calibrating hardware profiles, and neither replay reads them yet: an
  emulated amdgpu or NVIDIA governor needs facts a capture does not show, such
  as the levels a family accepts and the clocks a profile level pins, and both
  replays must publish the same golden (`TestHwemuA01`).
- `base_frequency`, which the agent reads as the nominal frequency, is not
  captured: hwemu renders no such file, so a captured one would make the two
  replays differ.

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
   well. The script writes the layout above, redacts what it can, checks the
   governor files for identifiers, and tars the directory. It never writes
   `expected.json`.

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
a MAC address, an IPv4 or IPv6 address, a PCI bus id, a UUID of any kind
including a GPU or MIG UUID, a serial number, an asset tag, a hostname or FQDN,
a username or home directory path, and a cloud instance or provider id, in any
file of any capture.
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

## Host paths and the variables that redirect them

Every host path the agent reads is a package variable, so a test points it at
a capture instead of at the machine the test runs on:

| Variable | Production value | Redirected by |
|---|---|---|
| `dvfs.PowercapRoot` | `/host-sys/class/powercap` | `pointAgentAtFixture`, at a copy of the capture's `powercap/` |
| `procCPUInfoPath` | `/proc/cpuinfo` | `pointAgentAtFixture`, at the capture's `cpuinfo` |
| `cpufreqDriverPath` | `/host-sys/devices/system/cpu/cpufreq/policy0/scaling_driver` | `pointAgentAtFixture`, at `cpufreq-driver`, or at an absent path when the capture has none |
| `nvidiaControlDevicePath` | `/dev/nvidiactl` | `hideHostGPUProbe`, at a marker file when the capture has `nvidia-smi.txt`, otherwise at an absent path |
| `dvfs.HostSysRoot` | `/host-sys` | `pointAgentAtFixture`, at a tree built from the capture's `governor/`, empty when the capture has none |

`detectGPUVendor` stats `nvidiaControlDevicePath` to decide whether the NVIDIA
driver is loaded. Without `hideHostGPUProbe`, a machine captured without a GPU
would be discovered as vendor `nvidia` on any host with a driver, and its golden
would describe that host instead of the capture. `TestHardwareFixtureCorpus`
redirects it for every machine, so the goldens replay and regenerate the same
way on any host.

`dvfs.HostSysRoot` is the root of the `/host-sys` paths that the governor
discovery and the DVFS loop read: the cpufreq policies, `intel_pstate/`,
`amd_pstate/` and `bus/pci/devices`, the cpufreq globs of `dvfs.CPUFreqList`
and the `devices/virtual/powercap` globs of `dvfs.EnergyFiles`. The corpus tests
point it at a tree of the capture's own governor state, so a machine whose
`/host-sys` is mounted, an agent image or a runner, never leaks its governor
into a golden; `TestCorpusReplayIgnoresTheHostSysfs` replays every machine with
such a tree present. A test that drives the DVFS loop over a whole `/sys` tree
points it at that tree, and `dvfs.PowercapRoot` at the tree's `class/powercap`.

## The machines

| Machine | What it is | Why it is here |
|---|---|---|
| `xeon-4socket-no-labels` | Bare metal 4 socket Intel Xeon Gold 6252, 192 logical CPUs, 165 W per package zone, DRAM sub-zones at 47.25 W, no GPU | NFD publishes neither `cpu-sockets` nor `cpu-model.name`, so sockets and model both come from `/proc/cpuinfo`. The DRAM sub-zones are the trap that zone selection by name exists to avoid. |
| `xeon-4socket-nfd-labels` | Bare metal 4 socket Intel Xeon Platinum 8260, 192 logical CPUs, 165 W per package zone, DRAM sub-zones at 47.25 W, no GPU | A real capture, contributed by the node's owner, of the same machine class the row above approximates by hand. NFD publishes 69 labels here and still neither `cpu-sockets` nor `cpu-model.name`, which is the evidence that those two cannot be depended on. It is also the only machine with a cpufreq driver (`intel_pstate`) and no governor state, and the only one with a `pci-0300` display device that belongs to no GPU vendor. |
| `vm-no-rapl` | VM with one NVIDIA Tesla T4, no powercap tree at all | The hypervisor gives every vCPU its own `physical id`, so the agent reads 28 sockets. No RAPL means no CPU cap range and no CPU control, and the GPU path has to carry the node on its own. |
| `epyc-amd-pstate-epp` | Hand written one socket AMD EPYC, four logical CPUs on `amd-pstate-epp` (active), governor `powersave`, preference `balance_power`, boost off in every policy, no powercap tree, no GPU | The machine with captured governor state: the governor, the preference pair, the per-policy boost, the driver mode and the frequency range reach `cpu.governor` and `cpu.landmarks` from `governor/`, and `hwemu.FromCorpus` renders the same state. The preference and boost differ from the hwemu defaults, so the emulated replay shows that both were mapped. Written by hand, with neutral values, until a real capture with governor state is contributed. |
