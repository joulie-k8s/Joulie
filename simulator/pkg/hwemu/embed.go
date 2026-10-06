package hwemu

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
)

//go:embed profiles/*.yaml
var renderProfilesFS embed.FS

// BuiltinProfiles loads the generic profiles shipped in profiles/. It parses
// them on every call, so a caller may change the profiles it gets.
func BuiltinProfiles() (map[string]*Profile, error) {
	return renderLoadEmbedded(renderProfilesFS, "profiles")
}

// renderLoadEmbedded is LoadProfiles plus the rule for profiles shipped in
// the repository: no measured: provenance and no measured source, because
// measurements of one site's machines belong only in local profiles.
func renderLoadEmbedded(fsys fs.FS, dir string) (map[string]*Profile, error) {
	profiles, err := LoadProfiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, name := range renderSortedProfileNames(profiles) {
		p := profiles[name]
		for id, s := range p.Sources {
			if s.Kind == renderKindMeasured {
				errs = append(errs, fmt.Errorf("%s: source %q is measured; measured sources belong only in local profiles", name, id))
			}
		}
		renderWalkFacts(reflect.ValueOf(p).Elem(), "", func(at string, f renderFact) {
			if strings.HasPrefix(f.renderSource(), renderKindMeasured+":") {
				errs = append(errs, fmt.Errorf("%s: %s: measured: provenance belongs only in local profiles", name, at))
			}
		})
		for g, lg := range p.Node.Labels {
			if strings.HasPrefix(lg.Src, renderKindMeasured+":") {
				errs = append(errs, fmt.Errorf("%s: node.labels.%s: measured: provenance belongs only in local profiles", name, g))
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
