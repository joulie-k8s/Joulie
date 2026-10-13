package clusterspec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// ProfileSource returns the hardware profiles by name. Every call returns
// profiles the caller may change: Expand calls it once per pool and applies
// the pool's overrides to the profile it gets.
type ProfileSource func() (map[string]*hwemu.Profile, error)

// Profiles is the profile source of the simulator and the CLI: the builtin
// profiles, plus the profiles in localDir/*.yaml when localDir is set. A
// local profile may not take a builtin's name, and extends only another
// local profile. A localDir without profiles adds none; a localDir that
// does not exist is an error. Each call parses the files again.
func Profiles(localDir string) ProfileSource {
	return func() (map[string]*hwemu.Profile, error) {
		profiles, err := hwemu.BuiltinProfiles()
		if err != nil {
			return nil, fmt.Errorf("builtin profiles: %w", err)
		}
		if localDir == "" {
			return profiles, nil
		}
		info, err := os.Stat(localDir)
		if err != nil {
			return nil, fmt.Errorf("local profiles: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("local profiles: %s is not a directory", localDir)
		}
		// The pattern is matched inside the directory, so a glob
		// metacharacter in localDir is a plain character of its name.
		fsys := os.DirFS(localDir)
		files, err := fs.Glob(fsys, "*.yaml")
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return profiles, nil
		}
		local, err := hwemu.LoadProfiles(fsys, ".")
		if err != nil {
			return nil, fmt.Errorf("local profiles in %s: %w", localDir, err)
		}
		names := make([]string, 0, len(local))
		for name := range local {
			names = append(names, name)
		}
		sort.Strings(names)
		var errs []error
		for _, name := range names {
			if _, ok := profiles[name]; ok {
				errs = append(errs, fmt.Errorf("local profile %s has the name of a builtin profile; rename it", filepath.Join(localDir, name+".yaml")))
				continue
			}
			profiles[name] = local[name]
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return profiles, nil
	}
}
