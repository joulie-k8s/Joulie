package hw

import (
	"path/filepath"
	"testing"
)

func TestLoadCatalog(t *testing.T) {
	// The catalog exists once, embedded in pkg/hwinv. Naming the file it is
	// embedded from is what a mounted override looks like to the simulator;
	// an unreadable path would fall back to the embed and assert nothing, so
	// TestLoadCatalogWithoutAPathUsesTheEmbeddedCatalog covers that side.
	path := filepath.Join("..", "..", "..", "pkg", "hwinv", "assets", "hardware.yaml")
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

// SIM_HARDWARE_CATALOG_PATH defaults to empty, which is how the simulator asks
// for the catalog embedded in pkg/hwinv. If that ever stopped resolving, the
// simulator would run with no hardware data and model every node as generic.
func TestLoadCatalogWithoutAPathUsesTheEmbeddedCatalog(t *testing.T) {
	c, err := LoadCatalog("")
	if err != nil {
		t.Fatalf("LoadCatalog(\"\") error: %v", err)
	}
	if c == nil || len(c.CPUModels) == 0 || len(c.GPUModels) == 0 {
		t.Fatalf("an empty path must yield the embedded catalog, got %+v", c)
	}
}
