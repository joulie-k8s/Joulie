package main

import (
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// SIM_WORKLOAD_TICK is the period of the legacy workload loop; a v1 run
// ticks on the emulator's tick and never reads it. Before the fix it was
// missing from workloadLegacyOnlyEnv, so under v1 it was dropped without the
// warning every other legacy-only variable gets.
func TestWorkloadTickWarnsUnderV1(t *testing.T) {
	_, warnings, err := loadWorkloadOptions(envMap(map[string]string{
		envHwemuRoot:     "/emu",
		workload.EnvTick: "2s",
	}), true)
	if err != nil {
		t.Fatalf("loadWorkloadOptions: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], workload.EnvTick) {
		t.Fatalf("warnings = %q, want one naming %s, which the v1 engine ignores", warnings, workload.EnvTick)
	}

	// The legacy engine reads it, so it warns nothing there.
	_, warnings, err = loadWorkloadOptions(envMap(map[string]string{workload.EnvTick: "2s"}), false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("legacy: err %v, warnings %q; want neither", err, warnings)
	}
}
