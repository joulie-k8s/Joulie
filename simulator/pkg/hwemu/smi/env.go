package smi

import "os"

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
type Env struct {
	SysRoot   string
	StateRoot string
	RootFS    string
}

// EnvFromOS reads HWEMU_SYS_ROOT, HWEMU_STATE_ROOT and HWEMU_ROOTFS. An unset
// or empty variable gives the pod mount point, and / for the root file
// system, so fakesmi needs no environment inside an emulated agent pod and
// checks the requirements against the pod's own files.
func EnvFromOS() Env {
	e := Env{SysRoot: os.Getenv("HWEMU_SYS_ROOT"), StateRoot: os.Getenv("HWEMU_STATE_ROOT"), RootFS: os.Getenv("HWEMU_ROOTFS")}
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
