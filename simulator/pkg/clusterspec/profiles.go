package clusterspec

import (
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// ProfileSource returns the hardware profiles by name. Every call returns
// profiles the caller may change: Expand calls it once per pool and applies
// the pool's overrides to the profile it gets.
type ProfileSource func() (map[string]*hwemu.Profile, error)

// Profiles is the profile source of the simulator and the CLIs: the builtin
// profiles plus the local ones in localDir, loaded by hwemu.LoadProfileSet,
// so a local profile extends a builtin or another local profile by the same
// rules wherever profiles are read. Each call parses the files again.
func Profiles(localDir string) ProfileSource {
	return func() (map[string]*hwemu.Profile, error) {
		profiles, _, err := hwemu.LoadProfileSet(localDir)
		return profiles, err
	}
}
