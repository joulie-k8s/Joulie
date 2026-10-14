package main

import "testing"

// A node whose only GPU facts are GPU feature discovery's labels resolves to
// the catalogue model of its product whatever the exposure: the suffixes
// -MIG-<profile> (single MIG strategy), -SHARED (time-slicing without
// renaming) and -MIG-INVALID are not part of the model, as the controller
// manager and the agent read them (pkg/api.GPUProductBase).
func TestGFDProductSuffixesResolveTheCatalogueModel(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	for _, product := range []string{
		"NVIDIA-H100-NVL",
		"NVIDIA-H100-NVL-MIG-1g.12gb",
		"NVIDIA-H100-NVL-SHARED",
		"NVIDIA-H100-NVL-MIG-1g.12gb-SHARED",
		"NVIDIA-H100-NVL-MIG-INVALID",
	} {
		t.Run(product, func(t *testing.T) {
			model, class := s.resolveModelForNode(map[string]string{
				"nvidia.com/gpu.product": product,
				"joulie.io/hw.gpu-count": "8",
			})
			if model.GPU.Product != "NVIDIA_H100_NVL" {
				t.Fatalf("GPU %q (class %s), want the catalogue's NVIDIA_H100_NVL", model.GPU.Product, class)
			}
		})
	}
}
