package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// envProfilesDir names the directory of local hwemu profiles, the only place
// a measured source may name the machine it was measured on.
// loadProfileSetFromEnv is its only reader.
const envProfilesDir = "SIM_PROFILES_DIR"

var logLocalProfilesOnce sync.Once

// loadProfileSetFromEnv returns the builtin hwemu profiles plus the local
// ones in SIM_PROFILES_DIR (builtins only when it is unset), parsed afresh on
// every call. It has the shape of clusterspec.ProfileSource, so a cluster
// spec can expand each pool from its own copy. The local names are logged on
// the first successful call only, since a spec calls it once per pool.
func loadProfileSetFromEnv() (map[string]*hwemu.Profile, error) {
	dir := strings.TrimSpace(os.Getenv(envProfilesDir))
	profiles, local, err := hwemu.LoadProfileSet(dir)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", envProfilesDir, dir, err)
	}
	logLocalProfilesOnce.Do(func() {
		if len(local) > 0 {
			log.Printf("local profiles loaded dir=%s profiles=%s", dir, strings.Join(local, ","))
		}
	})
	return profiles, nil
}
