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

func TestMatchCPUAcceptsProcCPUInfoStrings(t *testing.T) {
	cat, err := LoadCatalog("")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	tests := []struct {
		raw     string
		wantKey string
		wantOK  bool
	}{
		// Exactly what /proc/cpuinfo reports on Intel: a frequency suffix the
		// catalog aliases do not carry.
		{"Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz", "INTEL_XEON_GOLD_6530", true},
		{"Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz\t", "INTEL_XEON_GOLD_6530", true},
		// AMD reports no frequency suffix and already matched.
		{"AMD EPYC 9654 96-Core Processor", "AMD_EPYC_9654", true},
		{"INTEL XEON GOLD 6530", "INTEL_XEON_GOLD_6530", true},
		// A model that is genuinely absent must stay unmatched.
		{"Intel(R) Xeon(R) Gold 6252 CPU @ 2.10GHz", "", false},
	}
	for _, tc := range tests {
		key, _, ok := cat.MatchCPU(tc.raw)
		if ok != tc.wantOK || key != tc.wantKey {
			t.Fatalf("MatchCPU(%q)=(%q,%v) want=(%q,%v)", tc.raw, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}
