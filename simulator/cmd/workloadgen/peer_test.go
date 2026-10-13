package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// -peer-transfer-intensity writes workloadProfile.peerTransferIntensity on
// every job that requests two or more GPUs, in both modes, and on no other
// job or workload record; everything else in the trace stays as without the
// flag. Without it no record carries the key.
func TestWorkloadgenPeerTransferIntensityOnMultiGPUJobs(t *testing.T) {
	for _, mode := range traceModes {
		t.Run(mode.name, func(t *testing.T) {
			plain := runWorkloadgen(t, mode.args...)
			if bytes.Contains(plain, []byte("peerTransferIntensity")) {
				t.Fatal("a trace without -peer-transfer-intensity carries peerTransferIntensity")
			}
			base := traceRecords(t, plain)
			with := traceRecords(t, runWorkloadgen(t, append([]string{"-peer-transfer-intensity", "0.3"}, mode.args...)...))
			if len(with) != len(base) {
				t.Fatalf("%d records with the flag, %d without", len(with), len(base))
			}
			multi := 0
			for i, r := range with {
				prof, _ := r["workloadProfile"].(map[string]any)
				if prof == nil {
					prof, _ = r["sharedIntensityProfile"].(map[string]any)
				}
				got, has := prof["peerTransferIntensity"]
				if r["type"] != workload.TraceRecordJob {
					if has {
						t.Fatalf("record %d (%v) carries peerTransferIntensity", i+1, r["type"])
					}
					continue
				}
				if gpus := jobGPUs(r); gpus >= 2 {
					multi++
					if got != 0.3 {
						t.Fatalf("job %v requests %d GPUs: peerTransferIntensity %v, want 0.3", r["jobId"], gpus, got)
					}
				} else if has {
					t.Fatalf("job %v requests %d GPUs and carries peerTransferIntensity %v", r["jobId"], gpus, got)
				}
				delete(prof, "peerTransferIntensity")
				if !bytes.Equal(mustJSON(t, r), mustJSON(t, base[i])) {
					t.Fatalf("record %d differs beyond peerTransferIntensity:\n with    %s\n without %s", i+1, mustJSON(t, r), mustJSON(t, base[i]))
				}
			}
			if multi == 0 {
				t.Fatal("no job requests two or more GPUs; the flag went unchecked")
			}
		})
	}
}

// The flag is v3 only, like the sensitivity flags, so a v2 pin never moves,
// and it takes a share in [0,1].
func TestWorkloadgenRejectsInvalidPeerTransferIntensity(t *testing.T) {
	for _, args := range [][]string{
		{"-schema-version", "v2", "-peer-transfer-intensity", "0.3"},
		{"-peer-transfer-intensity", "1.5"},
		{"-peer-transfer-intensity", "-0.1"},
		{"-peer-transfer-intensity", "NaN"},
	} {
		out := filepath.Join(t.TempDir(), "trace.jsonl")
		if err := run(append(args, "-jobs", "1", "-out", out)); err == nil {
			t.Errorf("%v: no error", args)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%v: wrote a trace", args)
		}
	}
}

// mustJSON is v encoded, with map keys sorted.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jobGPUs is the GPU count a job record requests.
func jobGPUs(r map[string]any) int {
	tpl, _ := r["podTemplate"].(map[string]any)
	req, _ := tpl["requests"].(map[string]any)
	for _, k := range []string{"nvidia.com/gpu", "amd.com/gpu"} {
		if s, ok := req[k].(string); ok {
			n, _ := strconv.Atoi(s)
			return n
		}
	}
	return 0
}
