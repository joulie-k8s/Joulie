package governor

import joulie "github.com/matbun/joulie/pkg/api"

// presetsAMD lists the GPU presets the amdgpu device f can express, judging
// efficient at DefaultCeilingPctOfMax.
func presetsAMD(f AMDFacts) []string {
	var out []string
	for _, p := range joulie.GPUGovernorPresetValues {
		if chooseAMD(f, p, DefaultCeilingPctOfMax).ok {
			out = append(out, p)
		}
	}
	return out
}

// presetsNvidia lists the GPU presets the NVIDIA GPU f can express, judging
// efficient at DefaultCeilingPctOfMax.
func presetsNvidia(f NVFacts) []string {
	var out []string
	for _, p := range joulie.GPUGovernorPresetValues {
		if chooseNvidia(f, p, DefaultCeilingPctOfMax).result != joulie.GovernorResultUnsupported {
			out = append(out, p)
		}
	}
	return out
}
