package hwemu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"
)

// Local profiles live outside the repository, in the directory the simulator
// reads from SIM_PROFILES_DIR, and are the only profiles whose measured
// sources may name the machine they were measured on. They are loaded with the builtins from one in-memory file
// system, so extends resolves across both by the same merge rules. That file
// system is an fstest.MapFS: testing/fstest does not import package testing,
// and only its MapFS is used.

// LoadProfileSet loads the builtin profiles plus the local ones, the *.yaml
// files directly in dir, and returns every profile by name and the local
// names, sorted, or nil when there are none. An empty dir, or a directory with
// no *.yaml in it, gives the builtins only. Hidden entries (a leading dot) are
// skipped, since no profile name has a dot: an editor's lock link may point
// nowhere. A *.yml file or a suffix spelled in another case (*.YAML), a dir
// that does not exist and one that is not a directory are errors, never a
// silent fallback. A local profile may extend a builtin or another local
// profile, but may not take a builtin's name. A measured source that names a
// machine is rejected in the builtins and allowed in local profiles. Every
// call parses afresh, so a caller may change the profiles it gets.
func LoadProfileSet(dir string) (profiles map[string]*Profile, local []string, err error) {
	return renderLoadProfileSet(renderProfilesFS, "profiles", dir)
}

// renderLoadProfileSet is LoadProfileSet with the builtins read from
// builtinDir/*.yaml of builtin.
func renderLoadProfileSet(builtin fs.FS, builtinDir, dir string) (map[string]*Profile, []string, error) {
	if dir == "" {
		profiles, err := renderLoadEmbedded(builtin, builtinDir)
		if err != nil {
			return nil, nil, err
		}
		return profiles, nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("local profiles: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("local profiles: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("local profiles: %w", err)
	}

	// The set keeps the builtins' paths, so an error in a builtin reads as
	// it does from BuiltinProfiles.
	set := fstest.MapFS{}
	files, err := fs.Glob(builtin, path.Join(builtinDir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	builtinFile := map[string]string{}
	for _, f := range files {
		b, err := fs.ReadFile(builtin, f)
		if err != nil {
			return nil, nil, err
		}
		builtinFile[strings.TrimSuffix(path.Base(f), ".yaml")] = f
		set[f] = &fstest.MapFile{Data: b}
	}

	// Each local file is parsed on its own first, so that every error about
	// one file names it on disk rather than its copy in the set.
	var errs []error
	localFile := map[string]string{}
	extends := map[string]string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && (strings.EqualFold(ext, ".yaml") || strings.EqualFold(ext, ".yml")) {
			errs = append(errs, fmt.Errorf("%s: a local profile file ends in .yaml; rename it to %s.yaml", filepath.Join(dir, e.Name()), strings.TrimSuffix(e.Name(), ext)))
			continue
		}
		if ext != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		file := filepath.Join(dir, e.Name())
		localFile[name] = file
		b, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tree, err := renderParseFile(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			continue
		}
		if got, _ := tree["name"].(string); got != name {
			errs = append(errs, fmt.Errorf("%s: name is %q, want %q, the file name without .yaml", file, got, name))
			continue
		}
		if bf, ok := builtinFile[name]; ok {
			errs = append(errs, fmt.Errorf("%s: the builtin profile %s has the name %q; give the local profile another name and extend the builtin to build on it", file, bf, name))
			continue
		}
		extends[name], _ = tree["extends"].(string)
		set[path.Join(builtinDir, e.Name())] = &fstest.MapFile{Data: b}
	}
	local := make([]string, 0, len(localFile))
	for name := range localFile {
		local = append(local, name)
	}
	sort.Strings(local)
	for _, name := range local {
		base := extends[name]
		if _, ok := builtinFile[base]; ok || base == "" {
			continue
		}
		if _, ok := localFile[base]; !ok {
			errs = append(errs, fmt.Errorf("%s extends %q, which is neither a builtin profile nor a profile in %s", localFile[name], base, dir))
		}
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	if len(local) == 0 {
		local = nil
	}

	profiles, err := LoadProfiles(set, builtinDir)
	if err != nil {
		return nil, nil, fmt.Errorf("local profiles in %s: %w", dir, err)
	}
	names := make([]string, 0, len(builtinFile))
	for name := range builtinFile {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := renderRejectMachineNames(profiles, names); err != nil {
		return nil, nil, err
	}
	return profiles, local, nil
}
