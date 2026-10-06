package smi

import "os"

// Mount points of an emulated agent pod (layout package doc): the node's sys/
// at /host-sys, its state/ at /emu/state.
const (
	defaultSysRoot   = "/host-sys"
	defaultStateRoot = "/emu/state"
)

// Env locates the emulated node a tool acts on. SysRoot is the node's sys/
// directory, where the amdgpu attributes live; StateRoot is its state/
// directory, which holds tools.json and the NVML state.
type Env struct {
	SysRoot   string
	StateRoot string
}

// EnvFromOS reads HWEMU_SYS_ROOT and HWEMU_STATE_ROOT. An unset or empty
// variable gives the pod mount point, so fakesmi needs no environment inside
// an emulated agent pod.
func EnvFromOS() Env {
	e := Env{SysRoot: os.Getenv("HWEMU_SYS_ROOT"), StateRoot: os.Getenv("HWEMU_STATE_ROOT")}
	if e.SysRoot == "" {
		e.SysRoot = defaultSysRoot
	}
	if e.StateRoot == "" {
		e.StateRoot = defaultStateRoot
	}
	return e
}
