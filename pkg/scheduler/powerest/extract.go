package powerest

import (
	"math"
	"strings"
)

// gpuResourceKeys are the well-known extended resource names for GPU devices.
var gpuResourceKeys = []string{
	"nvidia.com/gpu",
	"amd.com/gpu",
	"gpu.intel.com/i915",
}

// ExtractPodDemand builds a PodDemand from a parsed pod spec represented as
// nested maps (the format the scheduler extender receives from kube-scheduler).
// It prefers requests over limits, with conservative fallbacks.
func ExtractPodDemand(pod map[string]interface{}, workloadClass string) PodDemand {
	d := PodDemand{WorkloadClass: workloadClass}

	spec, _ := pod["spec"].(map[string]interface{})
	if spec == nil {
		d.CPUCores = fallbackCPUCores(workloadClass)
		return d
	}

	containers, _ := spec["containers"].([]interface{})
	for _, c := range containers {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		res, _ := cm["resources"].(map[string]interface{})
		if res == nil {
			continue
		}
		d.CPUCores += extractQuantityCores(res, "cpu")
		d.MemoryBytes += extractQuantityBytes(res, "memory")
		gpus, vendor := extractGPU(res)
		d.GPUCount += gpus
		if vendor != "" {
			d.GPUVendor = vendor
		}
	}

	// Init containers can also request GPUs; take the max with regular containers.
	initContainers, _ := spec["initContainers"].([]interface{})
	for _, c := range initContainers {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		res, _ := cm["resources"].(map[string]interface{})
		if res == nil {
			continue
		}
		gpus, vendor := extractGPU(res)
		if gpus > d.GPUCount {
			d.GPUCount = gpus
		}
		if vendor != "" && d.GPUVendor == "" {
			d.GPUVendor = vendor
		}
	}

	if d.CPUCores <= 0 {
		d.CPUCores = fallbackCPUCores(workloadClass)
	}

	return d
}

// fallbackCPUCores returns a conservative CPU estimate when no requests/limits
// are specified.
func fallbackCPUCores(workloadClass string) float64 {
	if workloadClass == "performance" {
		return 0.5
	}
	return 0.25
}

// extractQuantityCores extracts CPU cores from requests or limits.
// Handles both numeric (float64) and string ("500m", "2") formats.
func extractQuantityCores(resources map[string]interface{}, key string) float64 {
	for _, section := range []string{"requests", "limits"} {
		sec, _ := resources[section].(map[string]interface{})
		if sec == nil {
			continue
		}
		raw, ok := sec[key]
		if !ok {
			continue
		}
		v := parseQuantity(raw)
		if v > 0 {
			return v
		}
	}
	return 0
}

// extractQuantityBytes extracts memory bytes from requests or limits.
func extractQuantityBytes(resources map[string]interface{}, key string) float64 {
	for _, section := range []string{"requests", "limits"} {
		sec, _ := resources[section].(map[string]interface{})
		if sec == nil {
			continue
		}
		raw, ok := sec[key]
		if !ok {
			continue
		}
		v := parseMemoryQuantity(raw)
		if v > 0 {
			return v
		}
	}
	return 0
}

// extractGPU returns (count, vendor) from resource requests/limits.
func extractGPU(resources map[string]interface{}) (int, string) {
	for _, section := range []string{"requests", "limits"} {
		sec, _ := resources[section].(map[string]interface{})
		if sec == nil {
			continue
		}
		for _, key := range gpuResourceKeys {
			raw, ok := sec[key]
			if !ok {
				continue
			}
			count := int(parseQuantity(raw))
			if count > 0 {
				return count, vendorFromKey(key)
			}
		}
	}
	return 0, ""
}

// vendorFromKey maps a GPU resource key to a vendor string.
func vendorFromKey(key string) string {
	switch {
	case strings.HasPrefix(key, "nvidia.com"):
		return "nvidia"
	case strings.HasPrefix(key, "amd.com"):
		return "amd"
	case strings.HasPrefix(key, "gpu.intel.com"):
		return "intel"
	default:
		return ""
	}
}

// parseQuantity parses a Kubernetes resource quantity (e.g. "500m", "2", 1.5).
func parseQuantity(raw interface{}) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		return parseQuantityString(v)
	default:
		return 0
	}
}

// parseQuantityString parses a Kubernetes quantity string like "500m", "2", "1.5".
func parseQuantityString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "m") {
		s = strings.TrimSuffix(s, "m")
		var v float64
		if _, err := parseFloat(s, &v); err {
			return 0
		}
		return v / 1000.0
	}
	var v float64
	if _, err := parseFloat(s, &v); err {
		return 0
	}
	return v
}

// parseMemoryQuantity parses memory quantities like "128Mi", "1Gi", "1073741824".
func parseMemoryQuantity(raw interface{}) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		return parseMemoryString(v)
	default:
		return 0
	}
}

func parseMemoryString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	multipliers := []struct {
		suffix string
		mult   float64
	}{
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Mi", 1024 * 1024},
		{"Ki", 1024},
		{"T", 1e12},
		{"G", 1e9},
		{"M", 1e6},
		{"K", 1e3},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suffix) {
			numStr := strings.TrimSuffix(s, m.suffix)
			var v float64
			if _, err := parseFloat(numStr, &v); err {
				return 0
			}
			return v * m.mult
		}
	}
	// Plain number = bytes
	var v float64
	if _, err := parseFloat(s, &v); err {
		return 0
	}
	return v
}

// parseFloat is a helper that avoids importing strconv in the hot path.
// Returns (true, false) on success, (false, true) on error.
func parseFloat(s string, out *float64) (bool, bool) {
	// Simple float parser for positive numbers.
	if s == "" {
		return false, true
	}
	var result float64
	var decimal bool
	var divisor float64 = 1
	for _, c := range s {
		if c == '.' {
			if decimal {
				return false, true
			}
			decimal = true
			continue
		}
		if c < '0' || c > '9' {
			return false, true
		}
		if decimal {
			divisor *= 10
			result += float64(c-'0') / divisor
		} else {
			result = result*10 + float64(c-'0')
		}
	}
	*out = result
	return true, false
}

// clamp restricts v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
