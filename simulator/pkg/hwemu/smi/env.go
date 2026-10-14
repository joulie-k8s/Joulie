package smi

import (
	"os"
	"strconv"
)

// Mount points of an emulated agent pod (layout package doc): the node's sys/
// at /host-sys, its state/ at /emu/state.
const (
	defaultSysRoot   = "/host-sys"
	defaultStateRoot = "/emu/state"
	defaultRootFS    = "/"
)

// Env locates the emulated node a tool acts on. SysRoot is the node's sys/
// directory, where the amdgpu attributes live; StateRoot is its state/
// directory, which holds tools.json and the NVML state.
//
// RootFS is the file system the real tool would run in, where the
// requirements of its tools.json entry (the ELF loader, the NVML library,
// a script's interpreter) are looked up. Empty skips every check, so a test
// that runs the tools in process sees no file of the host.
//
// Unprivileged runs the tools as a user without root would: a command that
// needs root fails, or does nothing, as the real tool does without it.
type Env struct {
	SysRoot      string
	StateRoot    string
	RootFS       string
	Unprivileged bool
}

// EnvFromOS reads HWEMU_SYS_ROOT, HWEMU_STATE_ROOT, HWEMU_ROOTFS and
// HWEMU_UNPRIVILEGED. An unset or empty variable gives the pod mount point,
// and / for the root file system, so fakesmi needs no environment inside an
// emulated agent pod and checks the requirements against the pod's own
// files. HWEMU_UNPRIVILEGED takes the words strconv.ParseBool takes, such as
// 1 or true; anything else, unset included, means root.
func EnvFromOS() Env {
	e := Env{SysRoot: os.Getenv("HWEMU_SYS_ROOT"), StateRoot: os.Getenv("HWEMU_STATE_ROOT"), RootFS: os.Getenv("HWEMU_ROOTFS")}
	e.Unprivileged, _ = strconv.ParseBool(os.Getenv("HWEMU_UNPRIVILEGED"))
	if e.SysRoot == "" {
		e.SysRoot = defaultSysRoot
	}
	if e.StateRoot == "" {
		e.StateRoot = defaultStateRoot
	}
	if e.RootFS == "" {
		e.RootFS = defaultRootFS
	}
	return e
}
