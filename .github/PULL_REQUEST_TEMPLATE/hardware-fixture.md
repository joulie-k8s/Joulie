---
name: Hardware fixture
about: Contribute a captured machine to cmd/agent/testdata/hardware
labels: hardware-fixture
---

<!--
Thank you for contributing a machine. This template is for a pull request that
adds or updates a directory under cmd/agent/testdata/hardware. Anything else belongs in
the ordinary pull request description.

Read cmd/agent/testdata/hardware/README.md first. It explains what a capture proves, the
exact steps, and what is never accepted.

Captures are published as part of this repository: committed to git, mirrored by
every fork, permanent in the history. Please make sure you are entitled to
publish what this capture contains.
-->

## The machine

Fill this in even where `machine.yaml` already says it. A reviewer reads this
first.

- **Corpus directory:** `cmd/agent/testdata/hardware/<machine>`
- **Vendor and model:** <!-- e.g. Intel Xeon Gold 6252, AMD EPYC 9654 -->
- **Sockets and logical CPUs:** <!-- e.g. 4 sockets, 192 logical CPUs -->
- **GPU:** <!-- vendor, model and count, or "none" -->
- **Kernel:** <!-- uname -r -->
- **Bare metal or virtual:** <!-- and the hypervisor, if virtual -->
- **RAPL:** <!-- package zones, their maximum, any DRAM or psys sub-zones, or "no powercap tree" -->
- **Kubernetes distribution and NFD:** <!-- if it is a cluster node; which labels NFD publishes -->

## What it proves

<!--
Which discovery path does this machine exercise that no machine already in the
corpus does? "Another Xeon" is not a reason to add a fixture. If it came from a
bug report, link the issue.
-->

## Checklist

- [ ] I ran `scripts/collect-hardware-fixture.sh` to produce the capture. Nothing
      in it was hand written. (If something had to be, it says so in
      `machine.yaml` under `source`.)
- [ ] I opened and read **every file** in the capture, including
      `node-labels.json` line by line.
- [ ] The capture contains **nothing identifying**: no hostname or FQDN, no MAC
      address, no IP address, no UUID of any kind including GPU and MIG UUIDs,
      no serial number or asset tag, no username or home directory path, no
      cloud instance or provider id, no site, team, rack or tenant name.
- [ ] `machine.yaml` is complete: `description` says what the machine is and
      what it proves, `source` says where the dump came from and who reviewed
      it, `capturedAt` is the capture date, `allocatable` is what the **kubelet**
      reports, and `writeRejectingZones` lists every zone whose
      `constraint_0_power_limit_uw` refuses a write (or is `[]`).
- [ ] `expected.json` was generated with
      `go test ./cmd/agent/ -run Corpus -update` and **not hand edited**. I read
      the resulting diff and it describes my machine correctly.
- [ ] `go test ./cmd/agent/...` passes on my checkout.
- [ ] `go test ./cmd/agent/ -run TestHardwareCorpusIsValid -v` passes.
- [ ] I am entitled to publish this capture, and I understand it becomes a
      permanent, public part of this repository.

## Anything surprising in the golden

<!--
If a number in expected.json looks wrong to you, say so here rather than
editing the file. A golden that was adjusted until it looked right asserts the
bug. A wrong number is a finding, and it is usually the reason the fixture is
worth having.
-->
