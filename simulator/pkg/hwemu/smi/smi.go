// Package smi emulates the GPU tools the agent runs, nvidia-smi, rocm-smi and
// amd-smi, over the files of an emulated node. It is a library so that tests
// run the tools in process through Runner; the fakesmi command wraps Main for
// agent pods, where bin/<tool> links point at it.
//
// Each tool is a pinned variant, named per tool in state/tools.json and
// written from the tool's own source:
//
//   - nvidia-smi default: the nvidia-smi documentation (RETURN VALUE codes,
//     query fields, -pl in watts). Message text the documentation does not fix
//     is assumed, and marked so where it is written.
//   - rocm-smi current: rocm_smi.py of rocm_smi_lib at 323ab1105dce, the head
//     of its develop branch, whose flags and exit logic hold from rocm-5.4.0
//     on. It has no --showpowercap.
//   - rocm-smi legacy-agent-contract: current plus --showpowercap, the flag
//     the agent asks for (listAmdDevices in cmd/agent). No release has it; the
//     variant is assumed and lives only until the agent stops asking.
//   - amd-smi 7.1 and 7.2: amdsmi_cli of rocm-7.1.0 and rocm-7.2.0, whose set
//     syntax and JSON shape differ.
//
// A tool writes only agent-owned (A) attributes, in place, as package hwemu's
// ownership contract requires; the emulator reads them back on its next step.
// A success path writes nothing to stderr, because the agent parses stdout and
// stderr combined (OSCommandRunner.Run in cmd/agent). Kernel citations
// (file.c:N) are against Linux v7.3-rc5.
//
// The tools read the node's state from two places:
//
//   - NVIDIA: state/nvml/gpu<i>/, one directory per GPU with one file per
//     value in mW, mJ or percent; nvFields maps each query field to its file.
//     A field the device does not support holds the token nvidia-smi prints,
//     such as [N/A], in place of a number. A missing file fails the call with
//     exit 255: a device answers every field with a value or a token, so a
//     missing file is state nobody wrote. A node without state/nvml has no
//     NVIDIA driver.
//   - AMD: the amdgpu PCI and hwmon attributes under sys/. What rocm-smi and
//     amd-smi learn from KFD or from the binary gpu_metrics file has no text
//     attribute, so it lives in state/amdgpu/card<N>/: gfx_version (the KFD
//     target, such as gfx942) and energy_count (the energy accumulator,
//     non-zero on a primary and 0 on a secondary die). Both are optional; an
//     absent file is a query that fails, as on hardware, and an absent
//     energy_count means a primary. The tools print each device's own power
//     reading and never add dies, so a multi-die package's power belongs on
//     its primary's hwmon, and a secondary die has no power reading.
package smi

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// Tool names, as the agent runs them and as bin/ links name them.
const (
	toolNvidiaSMI = "nvidia-smi"
	toolRocmSMI   = "rocm-smi"
	toolAMDSMI    = "amd-smi"
	toolFakeSMI   = "fakesmi"
)

// exitNotInstalled is the shell's code for a command it cannot find. A real
// node gives exec ENOENT instead; the agent treats any error the same way.
const exitNotInstalled = 127

// call is one run of a fake tool.
type call struct {
	tool    string
	variant string
	args    []string
	env     Env
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
}

// Main runs one fake tool and returns its exit code. The tool is
// filepath.Base(argv[0]), or argv[1] when argv[0] is fakesmi itself, so one
// binary serves every bin/<tool> link. A tool that tools.json does not list
// exits 127 with "fakesmi: <tool> is not installed on this emulated node".
func Main(argv []string, env Env, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "fakesmi: no program name")
		return 2
	}
	tool, args := filepath.Base(argv[0]), argv[1:]
	if tool == toolFakeSMI {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "usage: fakesmi nvidia-smi|rocm-smi|amd-smi [ARG ...]")
			return 2
		}
		tool, args = filepath.Base(args[0]), args[1:]
	}
	m, err := readManifest(env)
	if err != nil {
		fmt.Fprintf(stderr, "fakesmi: %v\n", err)
		return exitNotInstalled
	}
	entry, ok := m.Tools[tool]
	if !ok {
		fmt.Fprintf(stderr, "fakesmi: %s is not installed on this emulated node\n", tool)
		return exitNotInstalled
	}
	c := &call{tool: tool, variant: entry.Variant, args: args, env: env, stdin: stdin, stdout: stdout, stderr: stderr}
	switch tool {
	case toolNvidiaSMI:
		return runNvidiaSMI(c)
	case toolRocmSMI:
		return runRocmSMI(c)
	case toolAMDSMI:
		return runAMDSMI(c)
	}
	fmt.Fprintf(stderr, "fakesmi: %s is listed in %s but not emulated\n", tool, layout.ToolsFile)
	return exitNotInstalled
}

// readManifest reads state/tools.json. A missing manifest is an error rather
// than an empty one: every rendered node has one (L03), so its absence means
// HWEMU_STATE_ROOT points at the wrong directory.
func readManifest(env Env) (layout.ToolsManifest, error) {
	var m layout.ToolsManifest
	p := filepath.Join(env.StateRoot, layout.ToolsFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return m, fmt.Errorf("read tools manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse tools manifest %s: %w", p, err)
	}
	if m.SchemaVersion != 1 {
		return m, fmt.Errorf("tools manifest %s: schemaVersion %d, want 1", p, m.SchemaVersion)
	}
	return m, nil
}

// unknownVariant rejects a variant tools.json names but fakesmi does not
// emulate.
func (c *call) unknownVariant() int {
	fmt.Fprintf(c.stderr, "fakesmi: %s variant %q is not emulated\n", c.tool, c.variant)
	return 2
}

// notEmulated rejects a use of a real tool that fakesmi does not model, such
// as an option no agent path needs. It says so instead of imitating output it
// has no source for.
func (c *call) notEmulated(what string) int {
	fmt.Fprintf(c.stderr, "fakesmi: %s %s is not emulated\n", c.tool, what)
	return 2
}
