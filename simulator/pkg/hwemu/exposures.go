package hwemu

import "sort"

// Exposures lists, sorted, the GPU exposure modes (gpus.exposure) a profile
// of vendor may set, nil for a vendor hwemu does not render.
func Exposures(vendor string) []string {
	var out []string
	for mode, ok := range renderExposures[vendor] {
		if ok {
			out = append(out, mode)
		}
	}
	sort.Strings(out)
	return out
}
