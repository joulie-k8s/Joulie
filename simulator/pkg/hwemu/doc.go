// Package hwemu emulates the hardware interfaces of a bare-metal node: it
// renders the sysfs tree, /proc/cpuinfo and GPU tool state the agent reads
// from a Profile, and advances the counters in them as the node runs. The
// agent sees files and command output only, so an emulated node is
// indistinguishable from a real one at that boundary.
//
// # File ownership
//
// Every entry of a rendered tree has one owner (layout.Owner) and one kind
// (layout.Kind):
//
//   - R (layout.OwnerRender): written once by Render. A write to it is
//     reverted at the next Step, with a restored event; with
//     Options.RenderCheckInterval, at the first Step that reaches the
//     interval after the last check.
//   - S (layout.OwnerEmulator): emulator-owned, such as energy_uj and
//     scaling_cur_freq. Rewritten only when its content changes: in place,
//     in one write, under a write lease that proves nobody has the file
//     open, when the new value is at least as long as the file; otherwise
//     by temp file and rename. Either way a reader sees the old value or
//     the new one, whole. Temp files live in state/.tmp, never next to an
//     attribute, because filepath.Glob("*") matches dotfiles.
//   - A (layout.OwnerAgent): writable by the agent or a tool, such as a RAPL
//     limit or scaling_max_freq. Written in place, checked every Step (one
//     lstat, and a read when the file changed since its last read),
//     normalized in place, never renamed. An empty or unparsable read keeps
//     the previous value: the writer is between truncate and write. A value
//     the driver refuses, such as any write to a locked RAPL limit (ENODATA),
//     is reverted with a rejected event; Node.CheckWrite answers it with the
//     errno at the time of the write instead, and so does the live view of
//     a Node started with Options.Live (live.go, package hwfuse), which
//     serves every read and write of sys/ and state/ and reads back no file.
//   - L (layout.KindSymlink): relative inside sys/; absolute
//     layout.FakeSMIInContainer for the tool links in bin/.
//
// Modes mirror sysfs: 0444 read-only, 0644 for limits and enabled, 0400 for
// energy_uj (powercap_sys.c:225-245, 367-373, 467). The agent pod runs as
// root, so a non-root test gets EACCES on 0444 and 0400 files and must not
// depend on writing them.
//
// # Resuming a tree
//
// Render writes only into an absent or empty root. Ensure renders such a
// root too, and resumes any other: state/hwemu.json must name the same
// profile, node and digest of the profile's values (provenance and zero
// values excluded), or it returns ErrProfileChanged; every entry a fresh
// render has and the root lacks is moved in from that render, made in
// state/.tmp/ensure-*; an entry of another kind is ErrTreeConflict. Nothing
// the root has is replaced, so a simulator restart keeps every limit the
// agent wrote and every inode a pod has mounted, and a NewNode over the
// resumed tree continues its counters. With no agent mounting the node,
// removing its root recovers from either error.
//
// # Components
//
// State reports the node as telemetry names it: cpu<s> per package
// (package-<s>), memory<s> per DRAM channel (dram, parent cpu<s>), gpu<i> per
// GPU (its product), and the host, whose power is their sum plus the
// platform's otherW. Measured follows what a collector on the node could
// read: a package or DRAM zone with energy_uj, an NVIDIA GPU with
// total_energy_mj, an AMD GPU with power1_input or power1_average, a host
// that is not a hypervisor guest. Every event, a restored one too, names the
// component whose directory holds its file: a powercap zone, a cpufreq
// policy, an HSMP or amdgpu hwmon device, a GPU's PCI device or its NVML
// state. Any other file, such as cpuinfo or a control type's enabled, names
// none.
//
// # fakesmi
//
// A fake GPU tool writes nothing to stderr on a success path, because the
// agent parses stdout and stderr combined (OSCommandRunner.Run in
// cmd/agent). Every call returns well within the agent's 3 s command timeout
// (runCommand in cmd/agent).
//
// NVIDIA persistence mode lives in state/nvml/gpu<i>/persistence_mode (A),
// Enabled or Disabled, from gpus.persistenceMode and set by nvidia-smi -pm.
// While it is Disabled and the GPU has no load, which stands for no client
// holding it (assumed), each Step puts power_limit_mw back to the default,
// as a driver that unloads does, with a reset-persistence-off event.
//
// # Sources
//
// Kernel source citations in this package, its subpackages and its profiles
// (file.c:N, file.rst:N) are against Linux v7.3-rc5. rocm_smi.py citations
// are against python_smi_tools/rocm_smi.py of rocm_smi_lib at commit
// 323ab1105dce, the head of its develop branch; amd-smi citations name their
// ROCm release. D1 to D16 name known agent and simulator defects the
// emulator reproduces, and F1 to F5 the fixes that change them; both are
// listed at the top of cmd/agent/hwemu_test.go.
package hwemu
