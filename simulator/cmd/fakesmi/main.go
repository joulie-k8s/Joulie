// Command fakesmi is nvidia-smi, rocm-smi and amd-smi for an emulated node.
// One copy serves every node: each node's bin/<tool> link points at it, and it
// runs as the tool its name says, over the node files at HWEMU_SYS_ROOT and
// HWEMU_STATE_ROOT (package smi). Before it reads any GPU state, it looks for
// what the real tool needs to run (the requires of its tools.json entry: the
// ELF loader, the NVML library, a script's interpreter) under HWEMU_ROOTFS,
// default /, and exits as the real tool does when one is missing. Build it
// with CGO_ENABLED=0, so it runs in the agent's distroless static image.
package main

import (
	"os"

	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

func main() {
	os.Exit(smi.Main(os.Args, smi.EnvFromOS(), os.Stdin, os.Stdout, os.Stderr))
}
