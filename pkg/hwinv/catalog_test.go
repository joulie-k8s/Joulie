package hwinv

import "testing"

func TestDefaultCatalogMatchesSpreadsheetAliases(t *testing.T) {
	cat, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	if key, _, ok := cat.MatchCPU("AMD EPYC 9534 64-Core Processor"); !ok || key != "AMD_EPYC_9534" {
		t.Fatalf("cpu alias mismatch: key=%q ok=%v", key, ok)
	}
	if key, _, ok := cat.MatchGPU("AMD_Instinct_MI300X"); !ok || key != "AMD_INSTINCT_MI300X" {
		t.Fatalf("gpu alias mismatch: key=%q ok=%v", key, ok)
	}
}

func TestMatchNodeAllowsPartialRecognition(t *testing.T) {
	cat, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	match := cat.MatchNode(NodeDescriptor{
		CPUModelRaw: "AMD EPYC 9654 96-Core Processor",
		GPUModelRaw: "Unknown GPU",
		GPUCount:    4,
	})
	if match.CPUSpec == nil || match.CPUKey != "AMD_EPYC_9654" {
		t.Fatalf("expected cpu match, got %#v", match)
	}
	if match.GPUSpec != nil {
		t.Fatalf("expected gpu mismatch")
	}
	if len(match.Warnings) == 0 {
		t.Fatalf("expected warnings for unrecognized gpu")
	}
}
