package smi

import "testing"

// HWEMU_UNPRIVILEGED turns the tools into a user without root; unset or
// unparsable means root, as inside an emulated agent pod.
func TestEnvFromOSReadsUnprivileged(t *testing.T) {
	for v, want := range map[string]bool{"": false, "1": true, "true": true, "0": false, "false": false, "yes": false} {
		t.Setenv("HWEMU_UNPRIVILEGED", v)
		if got := EnvFromOS().Unprivileged; got != want {
			t.Errorf("HWEMU_UNPRIVILEGED=%q: Unprivileged=%v, want %v", v, got, want)
		}
	}
}
