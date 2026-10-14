package scenario_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// An inline job's profile.peerTransferIntensity is written to the trace
// under the key the simulator reads, an explicit 0 included, and left out
// when unset; a value outside 0..1 fails validation, naming the key.
func TestPeerTransferIntensityReachesTheTrace(t *testing.T) {
	s := validBase(t)
	p, zero := 0.3, 0.0
	s.Workload.Jobs[1].Profile.PeerTransferIntensity = &p
	s.Workload.Jobs[0].Profile.PeerTransferIntensity = &zero
	if err := scenario.Validate(s, validOpts); err != nil {
		t.Fatalf("peerTransferIntensity 0.3: %v", err)
	}
	b, err := scenario.RenderJobs(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"peerTransferIntensity":0.3`)) || !bytes.Contains(b, []byte(`"peerTransferIntensity":0`)) {
		t.Errorf("trace lacks the job's peerTransferIntensity:\n%s", b)
	}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	jobs, err := scenario.ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		got := j.WorkloadProfile.PeerTransferIntensity
		if want := map[string]float64{"cpu": 0, "gpu": 0.3}[j.WorkloadType]; got == nil || *got != want {
			t.Errorf("%s: peerTransferIntensity %v, want %v", j.JobID, got, want)
		}
	}

	unset, err := scenario.RenderJobs(validBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(unset, []byte("peerTransferIntensity")) {
		t.Errorf("an unset peerTransferIntensity is written:\n%s", unset)
	}

	bad := 1.5
	s.Workload.Jobs[1].Profile.PeerTransferIntensity = &bad
	if err := scenario.Validate(s, validOpts); err == nil || !strings.Contains(err.Error(), "profile.peerTransferIntensity 1.5 is not in 0..1") {
		t.Errorf("peerTransferIntensity 1.5: error %v, want one naming the key", err)
	}
}
