package hwinv

import (
	"strings"
	"testing"
)

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

// A catalogue mounted through HARDWARE_CATALOG_PATH that predates the removal
// of computeDensity and performanceHints must keep loading and matching:
// adopters copied the shipped file, keys included.
func TestCatalogWithRemovedKeysStillLoads(t *testing.T) {
	legacy := []byte(`catalogVersion: v1
cpuModels:
  AMD_EPYC_9654:
    aliases: ["AMD EPYC 9654 96-Core Processor"]
    provenance: exact+official
    computeDensity: 710.4
    performanceHints: {spec: 1.0}
    official: {vendor: amd, baseGHz: 2.4, boostGHz: 3.7, tdpW: 360}
gpuModels:
  NVIDIA_H100_NVL:
    aliases: ["NVIDIA H100 NVL"]
    provenance: official
    computeDensity: 400
    official: {vendor: nvidia, maxBoardPowerW: 400, minBoardPowerW: 200}
`)
	cat, err := loadCatalogBytes(legacy)
	if err != nil {
		t.Fatalf("a catalogue carrying computeDensity no longer loads: %v", err)
	}
	if key, spec, ok := cat.MatchCPU("AMD EPYC 9654 96-Core Processor"); !ok || key != "AMD_EPYC_9654" || spec.Official.TDPW != 360 {
		t.Fatalf("cpu match key=%q ok=%v spec=%+v", key, ok, spec)
	}
	if key, _, ok := cat.MatchGPU("NVIDIA H100 NVL"); !ok || key != "NVIDIA_H100_NVL" {
		t.Fatalf("gpu match key=%q ok=%v", key, ok)
	}
}

// D14: the AMD_EPYC_9655 node2S curve carried SPEC result 01464, which is a
// 2x EPYC 9965 Dell R7725 run. The only public 2x EPYC 9655 curves are 01457
// and 01458; the catalogue uses 01457.
func TestCatalogEPYC9655CurveIsSPECResult01457(t *testing.T) {
	// SPECpower_ssj2008 result 01457, Active Idle then 10% to 100%.
	assertSPECCurve(t, "AMD_EPYC_9655", "01457", []float64{126, 273, 329, 383, 433, 482, 531, 579, 625, 675, 751})
}

func TestCatalogEPYC9654And9965CurvesAreTheirCitedSPECResults(t *testing.T) {
	assertSPECCurve(t, "AMD_EPYC_9654", "01203", []float64{128, 257, 300, 340, 378, 410, 442, 498, 577, 697, 817})
	assertSPECCurve(t, "AMD_EPYC_9965", "01522", []float64{157, 265, 314, 362, 412, 461, 503, 544, 587, 661, 800})
}

// assertSPECCurve checks that key's node2S curve is SPECpower_ssj2008 result
// id point for point and that the YAML comment above it cites that result's
// .txt, which SPEC fair use requires.
func assertSPECCurve(t *testing.T, key, id string, want []float64) {
	t.Helper()
	cat, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	spec, ok := cat.CPUModels[key]
	if !ok || spec.MeasuredCurves == nil || spec.MeasuredCurves.Node2S == nil {
		t.Fatalf("%s has no node2S curve: %+v", key, spec)
	}
	got := spec.MeasuredCurves.Node2S.Points
	if len(got) != len(want) {
		t.Fatalf("D14: %s node2S has %d points, SPEC result %s has %d", key, len(got), id, len(want))
	}
	for i, p := range got {
		if p.LoadPct != float64(10*i) || p.PowerW != want[i] {
			t.Fatalf("D14: %s node2S point %d = {loadPct: %v, powerW: %v}, SPEC result %s gives {loadPct: %d, powerW: %v}",
				key, i, p.LoadPct, p.PowerW, id, 10*i, want[i])
		}
	}
	comment := yamlCommentAbove(t, key, "node2S:")
	for _, cite := range []string{"result " + id, "-" + id + ".txt"} {
		if !strings.Contains(comment, cite) {
			t.Errorf("D14: the comment above %s node2S does not cite %q:\n%s", key, cite, comment)
		}
	}
}

// yamlCommentAbove returns the comment lines directly above the first line
// starting with field inside the block of the two-space-indented model key
// in the embedded catalogue. The typed structs drop comments, so provenance
// can only be checked on the raw YAML.
func yamlCommentAbove(t *testing.T, key, field string) string {
	t.Helper()
	lines := strings.Split(string(defaultCatalogYAML), "\n")
	in := false
	for i, line := range lines {
		if line == "  "+key+":" {
			in = true
			continue
		}
		if in && len(line) > 2 && line[2] != ' ' {
			break
		}
		if !in || !strings.HasPrefix(strings.TrimSpace(line), field) {
			continue
		}
		var comment []string
		for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "#"); j-- {
			comment = append([]string{strings.TrimSpace(lines[j])}, comment...)
		}
		return strings.Join(comment, "\n")
	}
	t.Fatalf("%s has no %q line in the embedded catalogue", key, field)
	return ""
}

// D14: every GPU minimum cap is the value its public source supports. The
// controller clamps eco caps to it, so a floor no source backs moves caps.
func TestCatalogGPUMinimumsHavePublicSources(t *testing.T) {
	cat, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	// cite must appear in the YAML comment above minBoardPowerW.
	want := map[string]struct {
		minW   float64
		source string
		cite   string
	}{
		// MLPerf Inference v4.0, NVIDIA DGX-H100 MaxQ: the lowest vendor-set
		// limit (power_limit = 300), not a datasheet minimum.
		"NVIDIA_H100_SXM": {300, "MLPerf Inference v4.0 DGX-H100 MaxQ", "MLPerf"},
		// arXiv 2603.16164: limits below 400 W were not reliably enforced.
		"AMD_INSTINCT_MI300X": {400, "arXiv 2603.16164", "arXiv 2603.16164"},
		// No public minimum: the H100 PCIe product brief gives 200 W.
		"NVIDIA_H100_NVL": {200, "H100 PCIe product brief PB-11133-001", "PB-11133-001"},
		// No public minimum: the L40S product brief says "TBD".
		"NVIDIA_L40S": {200, "L40S product brief PB-11470-001 (TBD)", "PB-11470-001"},
		// No public minimum: AMD publishes only TBP 295 W.
		"AMD_RADEON_PRO_W7900": {200, "none published (TBP 295 W only)", "295 W"},
	}
	for key, gpu := range cat.GPUModels {
		w, ok := want[key]
		if !ok {
			t.Errorf("D14: GPU %q has no audited minimum cap source; add one here", key)
			continue
		}
		if gpu.Official.MinBoardPowerW != w.minW {
			t.Errorf("D14: %s minBoardPowerW = %v, want %v (%s)", key, gpu.Official.MinBoardPowerW, w.minW, w.source)
		}
		if comment := yamlCommentAbove(t, key, "minBoardPowerW:"); !strings.Contains(comment, w.cite) {
			t.Errorf("D14: the comment above %s minBoardPowerW does not cite %q (%s):\n%s", key, w.cite, w.source, comment)
		}
	}
	for key := range want {
		if _, ok := cat.GPUModels[key]; !ok {
			t.Errorf("GPU %q is audited here but missing from the catalogue", key)
		}
	}
}

// A label-safe spelling of an Intel part, as the KWOK node generator writes it
// into joulie.io/hw.cpu-model, matches the same entry as the cpuinfo name:
// "(R)" becomes "-R-" there, which used to leave a stray "r" token.
func TestMatchCPULabelSafeIntelSpelling(t *testing.T) {
	c, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"INTEL(R) XEON(R) GOLD 6530", "INTEL-R-XEON-R-GOLD-6530", "INTEL-R--XEON-R--GOLD-6530", "Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz"} {
		if key, _, ok := c.MatchCPU(raw); !ok || key != "INTEL_XEON_GOLD_6530" {
			t.Errorf("MatchCPU(%q) = %q, %v; want INTEL_XEON_GOLD_6530", raw, key, ok)
		}
	}
}

// FC07: the 8260 matches its catalogue entry in the full cpuinfo spelling and
// in its label-safe form, which the KWOK node generator (labelSafe in the
// simulator) writes into joulie.io/hw.cpu-model. That form turns
// " CPU @ 2.40GHz" into "-CPU---2.40GHz": the suffix pattern, anchored on
// "@", used to miss it, so the node fell back to the built-in model.
func TestFC07LabelSafeCPUInfoSpellingMatchesThe8260(t *testing.T) {
	c, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz",
		"Intel-R--Xeon-R--Platinum-8260-CPU---2.40GHz",
		"INTEL-R-XEON-R-PLATINUM-8260-CPU-2.40GHZ",
		"Intel-R--Xeon-R--Platinum-8260-2.40GHz",
	} {
		if key, _, ok := c.MatchCPU(raw); !ok || key != "INTEL_XEON_PLATINUM_8260" {
			t.Errorf("MatchCPU(%q) = %q, %v; want INTEL_XEON_PLATINUM_8260", raw, key, ok)
		}
	}
	// A clock token alone is not a model, and a part the catalogue lacks
	// stays unmatched in either spelling.
	for _, raw := range []string{"2.40GHz", "Intel-R--Xeon-R--Gold-6252-CPU---2.10GHz"} {
		if key, _, ok := c.MatchCPU(raw); ok {
			t.Errorf("MatchCPU(%q) = %q; want no match", raw, key)
		}
	}
}
