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
//     reverted at the next Step, with a restored event.
//   - S (layout.OwnerEmulator): emulator-owned, such as energy_uj and
//     scaling_cur_freq. Rewritten by temp file and rename, only when its
//     content changes. Temp files live in state/.tmp, never next to an
//     attribute, because filepath.Glob("*") matches dotfiles.
//   - A (layout.OwnerAgent): writable by the agent or a tool, such as a RAPL
//     limit or scaling_max_freq. Written in place, read back every Step,
//     normalized in place, never renamed. An empty or unparsable read keeps
//     the previous value: the writer is between truncate and write.
//   - D (layout.KindRejectingDir): a directory in place of an attribute that
//     rejects writes, so os.WriteFile fails with EISDIR, as in the hardware
//     corpus (TestHardwareFixtureCorpusRAPLWrites in cmd/agent).
//   - L (layout.KindSymlink): relative inside sys/; absolute
//     layout.FakeSMIInContainer for the tool links in bin/.
//
// Modes mirror sysfs: 0444 read-only, 0644 for limits and enabled, 0400 for
// energy_uj (powercap_sys.c:225-245, 367-373, 467). The agent pod runs as
// root, so a non-root test gets EACCES on 0444 and 0400 files and must not
// depend on writing them.
//
// # fakesmi
//
// A fake GPU tool writes nothing to stderr on a success path, because the
// agent parses stdout and stderr combined (OSCommandRunner.Run in
// cmd/agent). Every call returns well within the agent's 3 s command timeout
// (runCommand in cmd/agent).
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
