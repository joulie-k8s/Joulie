package hwemu

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
)

//go:embed profiles/*.yaml
var renderProfilesFS embed.FS

// BuiltinProfiles loads the generic profiles shipped in profiles/. It parses
// them on every call, so a caller may change the profiles it gets.
func BuiltinProfiles() (map[string]*Profile, error) {
	return renderLoadEmbedded(renderProfilesFS, "profiles")
}

// renderHostPattern matches what identifies one machine: a numbered node
// name, an IPv4 address or a dotted host name.
var renderHostPattern = regexp.MustCompile(`(?i)node-?[0-9]|\b[0-9]{1,3}(\.[0-9]{1,3}){3}\b|\b[a-z0-9-]+\.[a-z0-9-]+\.[a-z]{2,}\b`)

// renderLoadEmbedded is LoadProfiles plus the rule for profiles shipped in
// the repository: a measured source describes the machine by its hardware
// only, never by a node name, address or host name.
func renderLoadEmbedded(fsys fs.FS, dir string) (map[string]*Profile, error) {
	profiles, err := LoadProfiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, name := range renderSortedProfileNames(profiles) {
		p := profiles[name]
		for _, id := range renderSortedSourceIDs(p.Sources) {
			s := p.Sources[id]
			if s.Kind != renderKindMeasured {
				continue
			}
			if m := renderHostPattern.FindString(s.Ref + " " + s.Note); m != "" {
				errs = append(errs, fmt.Errorf("%s: measured source %q names a machine (%q); describe it by its hardware only", name, id, m))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return profiles, nil
}

func renderSortedProfileNames(m map[string]*Profile) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// renderSortedSourceIDs keeps error order stable.
func renderSortedSourceIDs(m map[string]Source) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
