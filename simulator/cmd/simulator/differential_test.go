package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// diffTestInputs is the work of the differential runs: a class and a memory
// intensity both sides read, so the activity factor and the GPU memory
// weight take part.
var diffTestInputs = diffInputs{cpuClass: "cpu.compute_bound", gpuClass: "gpu.compute_bound", memoryIntensity: 0.2}

// Standalone runs a profile node through the legacy model the profile
// adapter derives and stepNode; emulate mode runs the same profile as an
// hwemu node. Both must draw the same power and energy for the same load,
// caps and frequency limits, or a scenario's numbers depend on the mode it
// ran in. One subtest per builtin profile the adapter maps.
func TestDifferentialStandaloneStepMatchesEmulatedNode(t *testing.T) {
	clearSimEnv(t)
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		if _, rejected := profilesNotMappable[name]; !rejected {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			results := diffRun(t, name, diffTestInputs, diffSegments, nil)
			for _, r := range results {
				t.Logf("%-28s CPU %8.2f W (standalone %8.2f), GPU %8.2f W (%8.2f); freq scale %.4f (%.4f), throttle %d%%",
					r.seg.name, r.emu.cpuW, r.fine.cpuW, r.emu.gpuW, r.fine.gpuW, r.emuFreqScale, r.legacyFreqScale, r.throttlePct)
			}
			if bad := diffCompare(results); len(bad) > 0 {
				t.Errorf("standalone and emulated %s disagree:\n%s", name, strings.Join(bad, "\n"))
			}
		})
	}
}

// The comparator must see a model that drifted by little: the legacy idle
// power raised by 2 % is outside the idle segment's tolerance.
func TestDifferentialComparatorRejectsAPerturbedModel(t *testing.T) {
	clearSimEnv(t)
	idleOnly := func(p *hwemu.Profile) []diffSegment { return diffSegments(p)[:1] }
	results := diffRun(t, "intel-xeon-2s-rapl", diffTestInputs, idleOnly, func(m *simModel) { m.BaseIdleW *= 1.02 })
	bad := diffCompare(results)
	if len(bad) == 0 {
		t.Fatalf("the comparator accepted idle power 2 %% high: emulated %.2f W, standalone %.2f W", results[0].emu.cpuW, results[0].fine.cpuW)
	}
	for _, line := range bad {
		if !strings.HasPrefix(line, "cpu idle: CPU") {
			t.Errorf("mismatch outside the CPU idle readings: %s", line)
		}
	}
}
