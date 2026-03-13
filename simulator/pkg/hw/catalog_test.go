package hw

import (
	"path/filepath"
	"testing"
)

func TestLoadCatalog(t *testing.T) {
	path := filepath.Join("..", "..", "catalog", "hardware.yaml")
	c, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("LoadCatalog error: %v", err)
	}
	if c == nil {
		t.Fatalf("expected catalog")
	}
	if _, ok := c.CPUModels["AMD_EPYC_9654"]; !ok {
		t.Fatalf("missing CPU model AMD_EPYC_9654")
	}
	if _, ok := c.GPUModels["NVIDIA_H100_NVL"]; !ok {
		t.Fatalf("missing GPU model NVIDIA_H100_NVL")
	}
	if len(c.CPUModels["AMD_EPYC_9654"].Aliases) == 0 {
		t.Fatalf("expected cpu aliases")
	}
}
