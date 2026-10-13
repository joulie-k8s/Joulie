package workload_test

import (
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// The line is the legacy simulator's, byte for byte. What the collector's
// pattern could not read is written so it can: a negative elapsed time as
// 0, an empty id, node or class as "unknown", and whitespace inside one as
// '_', since the pattern reads each as one run of non-space characters.
func TestFormatJobCompletedLog(t *testing.T) {
	got := workload.FormatJobCompletedLog("wl-1-worker-01", "node-a", "performance", 12340*time.Millisecond)
	if want := "job completed id=wl-1-worker-01 node=node-a class=performance elapsed=12.3s"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	if got := workload.FormatJobCompletedLog("j", "n", "standard", -time.Second); got != "job completed id=j node=n class=standard elapsed=0.0s" {
		t.Errorf("negative elapsed gives %q", got)
	}
	cases := []struct{ id, node, class, want string }{
		{"j", "n", "", "job completed id=j node=n class=unknown elapsed=1.0s"},
		{"j", "", "standard", "job completed id=j node=unknown class=standard elapsed=1.0s"},
		{" ", "n", "standard", "job completed id=unknown node=n class=standard elapsed=1.0s"},
		{"a b", "n\tm", "eco  mode", "job completed id=a_b node=n_m class=eco_mode elapsed=1.0s"},
	}
	for _, c := range cases {
		if got := workload.FormatJobCompletedLog(c.id, c.node, c.class, time.Second); got != c.want {
			t.Errorf("FormatJobCompletedLog(%q, %q, %q) = %q, want %q", c.id, c.node, c.class, got, c.want)
		}
	}
}
