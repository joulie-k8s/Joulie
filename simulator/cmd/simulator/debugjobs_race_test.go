package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// The debug snapshot loop and /debug/jobs read the jobs (debugJobsPayload)
// while the workload loop writes them. Run with -race: the reader took no
// lock, a data race in both modes, until both sides held the jobs' lock.
func TestDebugJobsAndJobProgressRunTogether(t *testing.T) {
	s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
	pod := runningPodInfo{Namespace: "default", Name: "p", Node: "gpu-0", JobID: jobs[0].JobID, Running: true}
	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{pod}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()
	kube := k8sfake.NewSimpleClientset()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			s.injectTraceJobs(context.Background(), kube, time.Now().UTC())
			s.advanceJobProgress(context.Background(), kube, 0.001, time.Now().UTC())
		}
		close(done)
	}()
	for i := 0; i < 50; i++ {
		_ = s.debugJobsPayload()
	}
	<-done
}

func TestDebugJobsAndStandaloneRunTogether(t *testing.T) {
	dir := t.TempDir()
	inventory := filepath.Join(dir, "inventory.yaml")
	if err := os.WriteFile(inventory, []byte("nodes:\n  - node_name_prefix: sa\n    replicas: 2\n    cpu: Generic CPU\n    cpu_sockets: 2\n    cpu_cores: 32\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace.jsonl")
	if err := os.WriteFile(trace, []byte(`{"type":"job","jobId":"a","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"4"}},"work":{"cpuUnits":2000}}`+"\n"+
		`{"type":"job","jobId":"b","submitTimeOffsetSec":30,"podTemplate":{"requests":{"cpu":"4"}},"work":{"cpuUnits":2000}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"SIM_STANDALONE_INVENTORY": inventory,
		"SIM_STANDALONE_BASELINE":  "A",
		"SIM_SA_DT":                "1",
		"SIM_SA_TIMEOUT":           "600",
		"SIM_SA_WORKERS":           "1",
		"SIM_BASE_SPEED_PER_CORE":  "1",
	} {
		t.Setenv(k, v)
	}
	model := simModel{
		BaseIdleW: 80, PMaxW: 420, AlphaUtil: 1.15, BetaFreq: 1.35, FMinMHz: 1200, FMaxMHz: 3200,
		RaplHeadW: 5, DefaultCapW: 5000, RaplCapMinW: 80, RaplCapMaxW: 5000,
		DvfsRampMS: 500, CPUCapApplyTauMS: 250, CPUTelemetryWindowMS: 250,
		CPUAmbientTempC: 24, CPUThermalTauMS: 4000, CPUWattsPerDeltaC: 8,
		CPULowestNonlinearFreqMHz: 1800, CPUSockets: 2,
	}
	s := newSimulatorWithRegisterer(model, nil, nil, 0, prometheus.NewRegistry())
	if err := s.initWorkloadEngineWithOptions(trace, workloadOptions{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		runStandalone(s)
		close(done)
	}()
	for {
		_ = s.debugJobsPayload()
		select {
		case <-done:
			return
		default:
		}
	}
}
