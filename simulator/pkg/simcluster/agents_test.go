package simcluster

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

const testRoot = "/var/lib/joulie-hwemu"

var testAgentsGolden = filepath.Join("testdata", "agents-smoke.golden.yaml")

// testGolden compares got with the golden file, or rewrites it under
// -update.
func testGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the output (go test -update rewrites it):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// testRequirementTools is the GPU tools section of a profile with the
// requirements S3-WP5 gives the builtin profiles, in its JSON form.
const testRequirementTools = `[
  {"name": "nvidia-smi", "variant": "v", "requires": [
    {"paths": ["/lib64/ld-linux-x86-64.so.2"], "exit": 127},
    {"paths": ["/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "/usr/lib64/libnvidia-ml.so.1"], "exit": 12}]},
  {"name": "rocm-smi", "requires": [{"paths": ["/usr/bin/env"]}, {"paths": ["/usr/bin/python3", "/usr/local/bin/python3"]}]},
  {"name": "amd-smi", "requires": [{"paths": ["/usr/bin/env"]}, {"paths": ["/usr/bin/python3", "/usr/local/bin/python3"]}]},
  {"name": "plain"}
]`

// G01: DevNvidiactl is set on the agents of NVIDIA Nodes only.
func TestG01DevNvidiactlOnNVIDIAOnly(t *testing.T) {
	agents, err := EmulatedAgents(testExpand(t, testSmokeSpec), Harness{}, testRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"nvl-0": true, "sxm-nopm-0": true}
	if len(agents) != 8 {
		t.Fatalf("%d agents, want one per smoke Node (8)", len(agents))
	}
	for _, a := range agents {
		if a.DevNvidiactl != want[a.Name] {
			t.Errorf("agent %s (%s): devNvidiactl %v, want %v", a.Name, a.Profile, a.DevNvidiactl, want[a.Name])
		}
	}
}

// G02: under ToolDeliveryMounted the driver stub is mounted read-only at
// the first path of each tool requirement, once per path; under
// ToolDeliveryNone, or without requirements, nothing is mounted.
func TestG02StubMountsPerRequirement(t *testing.T) {
	tools, err := agentToolsFromJSON([]byte(testRequirementTools))
	if err != nil {
		t.Fatal(err)
	}
	vols, mounts := agentToolMounts(tools, ToolDeliveryMounted, testRoot)
	file := corev1.HostPathFile
	wantVols := []corev1.Volume{{Name: DriverStubVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: testRoot + "/lib/driver-stub", Type: &file}}}}
	if !reflect.DeepEqual(vols, wantVols) {
		t.Errorf("volumes %+v, want %+v", vols, wantVols)
	}
	var paths []string
	for _, m := range mounts {
		if m.Name != DriverStubVolume || !m.ReadOnly {
			t.Errorf("mount %+v is not a read-only mount of %s", m, DriverStubVolume)
		}
		paths = append(paths, m.MountPath)
	}
	wantPaths := []string{"/lib64/ld-linux-x86-64.so.2", "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "/usr/bin/env", "/usr/bin/python3"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("mount paths %v, want %v", paths, wantPaths)
	}

	if vols, mounts := agentToolMounts(tools, ToolDeliveryNone, testRoot); vols != nil || mounts != nil {
		t.Errorf("tool delivery none mounts %v %v, want nothing", vols, mounts)
	}
	if vols, mounts := agentToolMounts(tools[3:], ToolDeliveryMounted, testRoot); vols != nil || mounts != nil {
		t.Errorf("a tool without requirements mounts %v %v, want nothing", vols, mounts)
	}

	// The chart reads these keys (S3-WP0's EmulatedAgent contract).
	b, err := json.Marshal(EmulatedAgent{Name: "n", Profile: "p", ExtraVolumes: vols, ExtraVolumeMounts: mounts})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"extraVolumes":[{"name":"driver-stub","hostPath":{"path":"/var/lib/joulie-hwemu/lib/driver-stub","type":"File"}}]`, `"extraVolumeMounts":[{"name":"driver-stub","readOnly":true,"mountPath":"/lib64/ld-linux-x86-64.so.2"}`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("agent JSON %s lacks %s", b, key)
		}
	}

	for _, bad := range []string{`[{"name": "x", "requires": [{"paths": []}]}]`, `[{"name": "x", "requires": [{"paths": ["lib/x"]}]}]`} {
		if _, err := agentToolsFromJSON([]byte(bad)); err == nil {
			t.Errorf("tools %s: no error for a requirement without an absolute first path", bad)
		}
	}

	// Every builtin GPU profile's tools decode; a pool under none gets no
	// mount whatever its profile has.
	members := testExpand(t, testSmokeSpec)
	agents, err := EmulatedAgents(members, Harness{Pools: map[string]HarnessPool{"nvl": {ToolDelivery: ToolDeliveryNone}}}, testRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == "nvl-0" && (a.ExtraVolumes != nil || a.ExtraVolumeMounts != nil) {
			t.Errorf("nvl-0 under tool delivery none mounts %v %v", a.ExtraVolumes, a.ExtraVolumeMounts)
		}
	}
	if _, err := EmulatedAgents(members, Harness{}, "var/lib/joulie-hwemu"); err == nil {
		t.Error("a relative hwemu root is accepted")
	}
}

// G03: the agents come sorted by Node name, whatever the members' order.
func TestG03AgentsSortedByName(t *testing.T) {
	members := testExpand(t, testSmokeSpec)
	reversed := make([]clusterspec.Member, len(members))
	for i, m := range members {
		reversed[len(members)-1-i] = m
	}
	agents, err := EmulatedAgents(reversed, Harness{}, testRoot)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(agents); i++ {
		if agents[i-1].Name >= agents[i].Name {
			t.Fatalf("agents out of order: %s before %s", agents[i-1].Name, agents[i].Name)
		}
	}
}

// G04: the agents values file of the smoke spec under the chart's real
// DaemonSet render, as the simcluster command prints it.
func TestG04SmokeAgentsGolden(t *testing.T) {
	testGolden(t, testAgentsGolden, testSmokeAgentsValues(t))
}

func testSmokeAgentsValues(t *testing.T) []byte {
	t.Helper()
	s, err := clusterspec.LoadFile(testSmokeSpec)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	h, err := SpecHarness(s)
	if err != nil {
		t.Fatal(err)
	}
	agents, _, err := Agents(members, testRenderPlacement(t), h, testRoot)
	if err != nil {
		t.Fatal(err)
	}
	b, err := AgentsValues(s.Metadata.Name, agents)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ParseHarness and SpecHarness: the section is strict, and every pool it
// lists must be a pool of the spec.
func TestParseHarness(t *testing.T) {
	for _, raw := range []string{"", "null", " null ", "{}", `{"pools": {}}`} {
		h, err := ParseHarness(json.RawMessage(raw))
		if err != nil || len(h.Pools) != 0 {
			t.Errorf("harness %q: %+v, %v; want the zero Harness", raw, h, err)
		}
	}
	h, err := ParseHarness(json.RawMessage(`{"pools": {"a": {"toolDelivery": "none"}, "b": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.ToolDelivery("a") != ToolDeliveryNone || h.ToolDelivery("b") != ToolDeliveryMounted || h.ToolDelivery("c") != ToolDeliveryMounted {
		t.Errorf("tool deliveries %s %s %s, want none mounted mounted", h.ToolDelivery("a"), h.ToolDelivery("b"), h.ToolDelivery("c"))
	}
	for raw, want := range map[string]string{
		`{"pool": {}}`:                                 "unknown field",
		`{"pools": {"a": {"delivery": "none"}}}`:       "unknown field",
		`{"pools": {"a": {"toolDelivery": "copied"}}}`: `"copied" is not mounted or none`,
		`[]`: "cannot unmarshal",
	} {
		if _, err := ParseHarness(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("harness %s: error %v, want one containing %q", raw, err, want)
		}
	}

	s := &clusterspec.Spec{Metadata: clusterspec.Metadata{Name: "x"}, Spec: clusterspec.SpecBody{
		Pools:   []clusterspec.Pool{{Name: "a"}},
		Harness: json.RawMessage(`{"pools": {"a": {}, "z": {"toolDelivery": "none"}}}`),
	}}
	if _, err := SpecHarness(s); err == nil || !strings.Contains(err.Error(), "spec.harness.pools.z names no pool of spec x") {
		t.Errorf("SpecHarness: error %v, want one naming pool z", err)
	}
}
