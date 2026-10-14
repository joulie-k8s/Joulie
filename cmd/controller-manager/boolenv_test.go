package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// A boolean variable takes true, 1, yes or on and false, 0, no or off, in
// any case; empty keeps the default, and any other value keeps it with one
// warning that names the variable, however often the variable is read.
// CPU_GOVERNOR_ON_GPU_NODES follows the same rule as every other.
func TestBoolEnvSpellings(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, tc := range []struct {
		raw       string
		def, want bool
		warns     bool
	}{
		{"true", false, true, false}, {"1", false, true, false}, {"yes", false, true, false}, {"on", false, true, false},
		{" ON ", false, true, false}, {"Yes", false, true, false}, {"TRUE", false, true, false},
		{"false", true, false, false}, {"0", true, false, false}, {"no", true, false, false}, {"off", true, false, false},
		{"OFF", true, false, false}, {"", true, true, false}, {"", false, false, false},
		{"t", false, false, true}, {"enabled", true, true, true}, {"2", false, false, true},
	} {
		key := "JOULIE_TEST_BOOL_" + strings.ToUpper(strings.TrimSpace(tc.raw))
		t.Setenv(key, tc.raw)
		buf.Reset()
		for i := 0; i < 2; i++ {
			if got := boolEnv(key, tc.def); got != tc.want {
				t.Errorf("%s=%q default %v: %v, want %v", key, tc.raw, tc.def, got, tc.want)
			}
		}
		if n := strings.Count(buf.String(), key); (n == 1) != tc.warns || n > 1 {
			t.Errorf("%s=%q: %d warnings naming it, want %v once: %q", key, tc.raw, n, tc.warns, buf.String())
		}
	}
	for raw, want := range map[string]bool{"on": true, "off": false, "maybe": false} {
		setGovernorEnv(t, map[string]string{"CPU_GOVERNOR_ON_GPU_NODES": raw})
		if got := loadGovernorConfig().cpuOnGPUNodes; got != want {
			t.Errorf("CPU_GOVERNOR_ON_GPU_NODES=%q: %v, want %v", raw, got, want)
		}
	}
}
