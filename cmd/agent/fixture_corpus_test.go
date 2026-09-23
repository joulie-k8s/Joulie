package main

// Hardware fixture corpus.
//
// The agent reads hardware only through files and command output, so a captured
// machine replays exactly. Each directory under testdata/hardware is one real
// machine: its powercap tree, its /proc/cpuinfo, the stdout of the GPU queries
// the agent runs, its node labels, hand written metadata, and the golden
// NodeHardware status the agent publishes for it.
//
// Regenerate every golden after reviewing a new capture:
//
//	go test ./cmd/agent/ -run Corpus -update
//
// expected.json is generated, never hand edited. A bug report from an adopter
// becomes a test by adding their dump here and running that command.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/agent/dvfs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

// updateCorpusGolden rewrites every expected.json from the fixture inputs.
var updateCorpusGolden = flag.Bool("update", false,
	"rewrite testdata/hardware/*/expected.json from the fixture inputs")

const (
	corpusDir     = "../../testdata/hardware"
	corpusCatalog = "../../pkg/hwinv/assets/hardware.yaml"
)

// corpusMachine is machine.yaml. sigs.k8s.io/yaml routes YAML through JSON, so
// the json tags are the field names in the file.
type corpusMachine struct {
	Description string `json:"description"`
	Source      string `json:"source"`
	CapturedAt  string `json:"capturedAt"`
	Allocatable struct {
		CPU    string            `json:"cpu"`
		Memory string            `json:"memory"`
		GPU    map[string]string `json:"gpu"`
	} `json:"allocatable"`
	// WriteRejectingZones lists powercap zone directories whose
	// constraint_0_power_limit_uw rejects writes on the real machine, for
	// example a disabled DRAM zone. The test replays that by turning the
	// limit file into a directory, which is how the existing RAPL tests
	// simulate a zone that refuses a write.
	WriteRejectingZones []string `json:"writeRejectingZones"`
}

// corpusCommandRunner answers the GPU queries from the captured stdout in the
// fixture and fails every other command, the way an absent tool does.
type corpusCommandRunner struct {
	nvidiaQuery string
	hasNvidia   bool
	rocmQuery   string
	hasRocm     bool
}

func (r corpusCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if len(args) > 0 {
		key += " " + strings.Join(args, " ")
	}
	switch key {
	// detectGPUVendor probes with -L. The fixture does not store its output
	// because it is a list of GPU UUIDs and nothing reads it; the agent only
	// checks that the command succeeds.
	case "nvidia-smi -L":
		if r.hasNvidia {
			return nil, nil
		}
	case "nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits":
		if r.hasNvidia {
			return []byte(r.nvidiaQuery), nil
		}
	case "rocm-smi --showproductname":
		if r.hasRocm {
			return nil, nil
		}
	case "rocm-smi --showpowercap --showproductname --json":
		if r.hasRocm {
			return []byte(r.rocmQuery), nil
		}
	}
	return nil, fmt.Errorf("command not present on this machine: %s", key)
}

// TestHardwareFixtureCorpus replays every captured machine through the real
// discovery and publish path and compares the published NodeHardware status
// with the golden.
func TestHardwareFixtureCorpus(t *testing.T) {
	// The agent loads the hardware catalog once per process from
	// HARDWARE_CATALOG_PATH, so the golden reflects the catalog the chart
	// ships rather than an empty one.
	t.Setenv("HARDWARE_CATALOG_PATH", corpusCatalog)

	for _, machine := range corpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(corpusDir, machine)
			meta := readCorpusMachine(t, dir)
			hideHostGPUProbe(t, dir)

			pointAgentAtFixture(t, dir, copyCorpusPowercap(t, dir))
			node := corpusNode(t, machine, dir, meta)

			hw := discoverHardware(context.Background(), node)
			got := publishedNodeHardwareStatus(t, node.Name, hw)

			golden := filepath.Join(dir, "expected.json")
			if *updateCorpusGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s", golden)
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run: go test ./cmd/agent/ -run Corpus -update)", err)
			}
			if string(got) != string(want) {
				t.Fatalf("NodeHardware status for %s does not match the golden.\ngot:\n%s\nwant:\n%s\n"+
					"If the change is intended, review it and run: go test ./cmd/agent/ -run Corpus -update",
					machine, got, want)
			}
		})
	}
}

// TestHardwareFixtureCorpusRAPLWrites checks, for every machine that declares
// zones whose limit file rejects writes, that applyRAPLPackageCap writes the
// package zones it should and reports exactly the failures it should. The DRAM
// sub-zones are the trap: they are selected by no correct implementation, so
// making their limit file unwritable must change nothing.
func TestHardwareFixtureCorpusRAPLWrites(t *testing.T) {
	t.Setenv("HARDWARE_CATALOG_PATH", corpusCatalog)

	const capWatts = 120.0
	const wantPayload = "120000000"

	for _, machine := range corpusMachines(t) {
		dir := filepath.Join(corpusDir, machine)
		meta := readCorpusMachine(t, dir)
		if len(meta.WriteRejectingZones) == 0 {
			continue
		}
		t.Run(machine, func(t *testing.T) {
			root := copyCorpusPowercap(t, dir)
			if root == "" {
				t.Fatalf("machine.yaml lists writeRejectingZones but %s has no powercap tree", dir)
			}
			for _, zone := range meta.WriteRejectingZones {
				limit := filepath.Join(root, zone, "constraint_0_power_limit_uw")
				if _, err := os.Stat(limit); err != nil {
					t.Fatalf("writeRejectingZones names %q, which has no limit file: %v", zone, err)
				}
				// A directory in place of the file is how a zone that
				// refuses a write behaves from the agent's side.
				if err := os.Remove(limit); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(limit, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pointAgentAtFixture(t, dir, root)

			rejecting := map[string]bool{}
			for _, z := range meta.WriteRejectingZones {
				rejecting[z] = true
			}
			var wantWritten, wantFailed []string
			for _, zone := range corpusPackageZones(t, root) {
				if rejecting[zone] {
					wantFailed = append(wantFailed, zone)
					continue
				}
				wantWritten = append(wantWritten, zone)
			}

			node := corpusNode(t, machine, dir, meta)
			hw := discoverHardware(context.Background(), node)

			applied, count, err := applyRAPLPackageCap(hw, capWatts)
			if len(wantWritten) == 0 {
				if err == nil {
					t.Fatalf("every package zone rejects the write, want an error, got applied=%v count=%d", applied, count)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !applied || count != len(wantWritten) {
				t.Fatalf("applied=%v count=%d want applied=true count=%d (failures expected on %v)",
					applied, count, len(wantWritten), wantFailed)
			}
			for _, zone := range wantWritten {
				b, err := os.ReadFile(filepath.Join(root, zone, "constraint_0_power_limit_uw"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(string(b)) != wantPayload {
					t.Fatalf("%s limit=%q want=%q", zone, strings.TrimSpace(string(b)), wantPayload)
				}
			}
			// Nothing outside the package zones may be touched.
			for _, zone := range corpusZones(t, root) {
				if rejecting[zone] || containsString(wantWritten, zone) {
					continue
				}
				limit := filepath.Join(root, zone, "constraint_0_power_limit_uw")
				b, err := os.ReadFile(limit)
				if err != nil {
					continue
				}
				original, err := os.ReadFile(filepath.Join(dir, "powercap", zone, "constraint_0_power_limit_uw"))
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != string(original) {
					t.Fatalf("%s limit changed to %q, want the captured %q: only package-* zones may be written",
						zone, strings.TrimSpace(string(b)), strings.TrimSpace(string(original)))
				}
			}
		})
	}
}

// publishedNodeHardwareStatus runs the real status writer against a fake API
// server and returns the status it stored, as indented JSON without the
// timestamp.
func publishedNodeHardwareStatus(t *testing.T, nodeName string, hw HardwareInfo) []byte {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeHardwareGVR: "NodeHardwareList"})
	name := sanitizeNodeObjectName(nodeName)
	// The writer skips a write whose payload is unchanged; make the subtest
	// independent of whatever ran before it in this process.
	lastWrites.Delete("nodeHardware|" + name)

	if err := upsertNodeHardwareStatus(context.Background(), dyn, nodeName, hw); err != nil {
		t.Fatal(err)
	}
	obj, err := dyn.Resource(nodeHardwareGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, ok := obj.Object["status"].(map[string]any)
	if !ok {
		t.Fatalf("no status on the published NodeHardware for %s", nodeName)
	}
	// updatedAt is wall clock, not a hardware fact.
	delete(status, "updatedAt")
	out, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// pointAgentAtFixture redirects every host path the agent reads at this
// machine's capture, for the duration of the test.
func pointAgentAtFixture(t *testing.T, dir, powercapRoot string) {
	t.Helper()
	if powercapRoot == "" {
		// No powercap tree in the capture: point at a path that does not
		// exist, so the test never reads the sysfs of the machine it runs on.
		powercapRoot = filepath.Join(t.TempDir(), "absent-powercap")
	}
	oldRoot := dvfs.PowercapRoot
	dvfs.PowercapRoot = powercapRoot
	t.Cleanup(func() { dvfs.PowercapRoot = oldRoot })

	oldCPUInfo := procCPUInfoPath
	procCPUInfoPath = filepath.Join(dir, "cpuinfo")
	t.Cleanup(func() { procCPUInfoPath = oldCPUInfo })

	// cpufreq-driver is optional in a capture; when it is missing, point at a
	// path that does not exist so the driver family comes out empty rather
	// than describing the host running the test.
	oldDriver := cpufreqDriverPath
	cpufreqDriverPath = filepath.Join(dir, "cpufreq-driver")
	if _, err := os.Stat(cpufreqDriverPath); err != nil {
		cpufreqDriverPath = filepath.Join(t.TempDir(), "absent-scaling-driver")
	}
	t.Cleanup(func() { cpufreqDriverPath = oldDriver })

	runner := corpusCommandRunner{}
	if b, err := os.ReadFile(filepath.Join(dir, "nvidia-smi.txt")); err == nil {
		runner.hasNvidia = true
		runner.nvidiaQuery = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "rocm-smi.txt")); err == nil {
		runner.hasRocm = true
		runner.rocmQuery = string(b)
	}
	oldRunner := commandRunner
	commandRunner = runner
	t.Cleanup(func() { commandRunner = oldRunner })
}

// corpusNode builds the Node object the agent would see: the captured labels
// plus the allocatable from machine.yaml.
func corpusNode(t *testing.T, machine, dir string, meta corpusMachine) *corev1.Node {
	t.Helper()
	nodeLabels := map[string]string{}
	b, err := os.ReadFile(filepath.Join(dir, "node-labels.json"))
	if err != nil {
		t.Fatalf("node-labels.json: %v", err)
	}
	if err := json.Unmarshal(b, &nodeLabels); err != nil {
		t.Fatalf("node-labels.json: %v", err)
	}

	resources := corev1.ResourceList{}
	if meta.Allocatable.CPU != "" {
		resources[corev1.ResourceCPU] = resource.MustParse(meta.Allocatable.CPU)
	}
	if meta.Allocatable.Memory != "" {
		resources[corev1.ResourceMemory] = resource.MustParse(meta.Allocatable.Memory)
	}
	for name, qty := range meta.Allocatable.GPU {
		resources[corev1.ResourceName(name)] = resource.MustParse(qty)
	}

	node := &corev1.Node{}
	node.Name = "corpus-" + machine
	node.Labels = nodeLabels
	node.Status.Capacity = resources
	node.Status.Allocatable = resources.DeepCopy()
	return node
}

// copyCorpusPowercap copies the captured powercap tree into a writable temp
// directory and returns its path, or "" when the machine has no RAPL. A copy
// is required because applyRAPLPackageCap writes into it.
func copyCorpusPowercap(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "powercap")
	if _, err := os.Stat(src); err != nil {
		return ""
	}
	dst := filepath.Join(t.TempDir(), "powercap")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range entries {
		if !zone.IsDir() {
			continue
		}
		zoneDst := filepath.Join(dst, zone.Name())
		if err := os.MkdirAll(zoneDst, 0o755); err != nil {
			t.Fatal(err)
		}
		files, err := os.ReadDir(filepath.Join(src, zone.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, zone.Name(), f.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(zoneDst, f.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dst
}

func corpusZones(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// corpusPackageZones lists the zones a correct implementation selects: the
// ones whose name file says package-*, never the ones whose directory merely
// looks like a socket.
func corpusPackageZones(t *testing.T, root string) []string {
	t.Helper()
	out := []string{}
	for _, zone := range corpusZones(t, root) {
		b, err := os.ReadFile(filepath.Join(root, zone, "name"))
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(b)), "package-") {
			out = append(out, zone)
		}
	}
	return out
}

func corpusMachines(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		// A directory whose name starts with an underscore is not a machine:
		// _template is the contributor's starting point and is full of
		// placeholders. corpus_validate_test.go skips them too, and
		// TestCorpusListingsSkipUnderscoreDirectories asserts both do.
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no machines in %s", corpusDir)
	}
	return out
}

func readCorpusMachine(t *testing.T, dir string) corpusMachine {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "machine.yaml"))
	if err != nil {
		t.Fatalf("machine.yaml: %v", err)
	}
	var meta corpusMachine
	if err := yaml.Unmarshal(b, &meta); err != nil {
		t.Fatalf("machine.yaml: %v", err)
	}
	return meta
}

// hideHostGPUProbe points the NVIDIA driver probe at a path inside the
// fixture. detectGPUVendor stats that path to decide the vendor, so without
// this a machine captured with no GPU would be discovered as "nvidia" on any
// host that has a driver loaded, and its golden would describe the host
// instead of the fixture. A machine that carries GPU query output gets the
// path pointed at its own marker file, so the probe answers the way it did on
// the captured node.
func hideHostGPUProbe(t *testing.T, dir string) {
	t.Helper()
	previous := nvidiaControlDevicePath
	t.Cleanup(func() { nvidiaControlDevicePath = previous })

	if _, err := os.Stat(filepath.Join(dir, "nvidia-smi.txt")); err == nil {
		marker := filepath.Join(t.TempDir(), "nvidiactl")
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		nvidiaControlDevicePath = marker
		return
	}
	nvidiaControlDevicePath = filepath.Join(t.TempDir(), "absent-nvidiactl")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
