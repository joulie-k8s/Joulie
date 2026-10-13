package hwemu

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderCorpusRoot is the hardware corpus the agent's tests replay, or
// JOULIE_HARDWARE_CORPUS when set, as the corpus test honours it.
func renderCorpusRoot() string {
	if v := os.Getenv("JOULIE_HARDWARE_CORPUS"); v != "" {
		return v
	}
	return filepath.Join("..", "..", "..", "cmd", "agent", "testdata", "hardware")
}

// renderCorpusMachines lists the machines of the corpus, skipping
// directories that start with _.
func renderCorpusMachines(t testing.TB) []string {
	t.Helper()
	entries, err := os.ReadDir(renderCorpusRoot())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no machines in %s", renderCorpusRoot())
	}
	return out
}

// H21: FromCorpus maps every corpus machine, and the rendered tree replays
// the capture: cpuinfo is byte-identical; every captured powercap file reads
// the same through class/powercap, the captured energy_uj included, and no
// zone gains or loses a file; writeRejectingZones reject writes with EISDIR;
// the cpufreq driver reads back from policy0; the captured governor state
// reads back byte for byte (renderCheckCorpusGovernor); dev/nvidiactl exists
// exactly when nvidia-smi.txt does; the NVML state holds the captured values;
// and the Node carries the captured labels and allocatable.
func TestH21FromCorpusReplaysEveryMachine(t *testing.T) {
	t.Parallel()
	for _, machine := range renderCorpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(renderCorpusRoot(), machine)
			p, err := FromCorpus(dir)
			if err != nil {
				t.Fatal(err)
			}
			tree := renderTestTree(t, p)
			var meta renderCorpusMachine
			b, err := os.ReadFile(filepath.Join(dir, "machine.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(b, &meta); err != nil {
				t.Fatal(err)
			}

			want, _ := os.ReadFile(filepath.Join(dir, "cpuinfo"))
			if got := renderReadFile(t, tree, layout.CPUInfoFile); got != string(want) {
				t.Errorf("cpuinfo differs from the capture")
			}

			renderCheckCorpusPowercap(t, dir, tree, meta.WriteRejectingZones)

			driver, err := os.ReadFile(filepath.Join(dir, "cpufreq-driver"))
			policy0 := filepath.Join(tree.SysRoot, layout.PolicyDir(0), "scaling_driver")
			_, govErr := os.Stat(filepath.Join(dir, renderCorpusGovernorDir, "cpufreq"))
			if err == nil {
				if got, _ := os.ReadFile(policy0); string(got) != string(driver) {
					t.Errorf("policy0/scaling_driver = %q, want the captured %q", got, driver)
				}
				m, _ := filepath.Glob(filepath.Join(tree.SysRoot, "devices/system/cpu/cpu*/cpufreq/scaling_max_freq"))
				if govErr != nil && len(m) != 0 {
					t.Errorf("a driver-only capture renders scaling_max_freq, which the agent would write")
				}
				renderCheckCorpusGovernor(t, dir, tree, p)
			} else if _, err := os.Stat(filepath.Join(tree.SysRoot, "devices/system/cpu/cpufreq")); err == nil {
				t.Errorf("no cpufreq-driver captured, but cpufreq is rendered")
			}

			nvidia, nvErr := os.ReadFile(filepath.Join(dir, "nvidia-smi.txt"))
			_, devErr := os.Stat(filepath.Join(tree.Root, layout.DevNvidiactl))
			if (nvErr == nil) != (devErr == nil) {
				t.Errorf("nvidia-smi.txt present = %v, dev/nvidiactl present = %v", nvErr == nil, devErr == nil)
			}
			if nvErr == nil {
				renderCheckCorpusNVML(t, tree, string(nvidia))
			}

			var labels map[string]string
			lb, _ := os.ReadFile(filepath.Join(dir, "node-labels.json"))
			if err := json.Unmarshal(lb, &labels); err != nil {
				t.Fatal(err)
			}
			node := p.NodeObject("corpus-" + machine)
			for k, v := range labels {
				if k != "kubernetes.io/hostname" && node.Labels[k] != v {
					t.Errorf("label %s = %q, want the captured %q", k, node.Labels[k], v)
				}
			}
			for name, raw := range map[corev1.ResourceName]string{corev1.ResourceCPU: meta.Allocatable.CPU, corev1.ResourceMemory: meta.Allocatable.Memory} {
				want := resource.MustParse(raw)
				if got := node.Status.Allocatable[name]; got.Cmp(want) != 0 {
					t.Errorf("allocatable %s = %s, want %s", name, got.String(), raw)
				}
			}
			for name, raw := range meta.Allocatable.GPU {
				want := resource.MustParse(raw)
				if got := node.Status.Allocatable[corev1.ResourceName(name)]; got.Cmp(want) != 0 {
					t.Errorf("allocatable %s = %s, want %s", name, got.String(), raw)
				}
			}
		})
	}

	if _, err := FromCorpus(filepath.Join(renderCorpusRoot(), "_template")); err == nil {
		t.Errorf("FromCorpus accepted _template, which is not a machine")
	}
}

// renderCheckCorpusPowercap compares the capture's flat powercap tree with
// the rendered class/powercap view of it.
func renderCheckCorpusPowercap(t *testing.T, dir string, tree *Tree, rejecting []string) {
	t.Helper()
	captured := filepath.Join(dir, "powercap")
	zones, err := os.ReadDir(captured)
	if os.IsNotExist(err) {
		if m, _ := filepath.Glob(filepath.Join(tree.SysRoot, "class/powercap/*")); len(m) != 0 {
			t.Errorf("no powercap captured, but %v is rendered", m)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	rejects := map[string]bool{}
	for _, z := range rejecting {
		rejects[z] = true
	}
	class := filepath.Join(tree.SysRoot, "class", "powercap")
	for _, z := range zones {
		files, err := os.ReadDir(filepath.Join(captured, z.Name()))
		if err != nil {
			t.Fatal(err)
		}
		capturedFiles := map[string]bool{}
		for _, f := range files {
			capturedFiles[f.Name()] = true
			got := filepath.Join(class, z.Name(), f.Name())
			if rejects[z.Name()] && f.Name() == "constraint_0_power_limit_uw" {
				if !renderIsEISDIR(got) {
					t.Errorf("%s/%s accepts writes; machine.yaml lists the zone as rejecting", z.Name(), f.Name())
				}
				continue
			}
			want, _ := os.ReadFile(filepath.Join(captured, z.Name(), f.Name()))
			if b, err := os.ReadFile(got); err != nil || string(b) != string(want) {
				t.Errorf("%s/%s = %q (%v), want the captured %q", z.Name(), f.Name(), b, err, want)
			}
		}
		rendered, err := os.ReadDir(filepath.Join(class, z.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range rendered {
			if strings.HasPrefix(f.Name(), z.Name()+":") {
				continue // a child zone directory
			}
			if !capturedFiles[f.Name()] {
				t.Errorf("%s/%s is rendered but not captured", z.Name(), f.Name())
			}
		}
	}
}

// renderCheckCorpusGovernor compares the captured governor state with the
// rendered tree, byte for byte: every attribute of every captured policy,
// the status file of a driver with modes, and the boost file where the
// driver keeps boost. A boost file the driver does not use is left out, as
// the agent leaves it out (cpuBoostScope in cmd/agent).
func renderCheckCorpusGovernor(t *testing.T, dir string, tree *Tree, p *Profile) {
	t.Helper()
	root := filepath.Join(dir, renderCorpusGovernorDir)
	if _, err := os.Stat(filepath.Join(root, "cpufreq")); err != nil {
		return
	}
	f := p.CPUFreq
	if f == nil || f.Minimal {
		t.Fatalf("governor/cpufreq is captured, but the profile's cpufreq is %+v", f)
	}
	driver := f.Driver.V
	compared := 0
	_ = filepath.WalkDir(root, func(fp string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, fp)
		rel = filepath.ToSlash(rel)
		var sys string
		switch dirName, name := path.Split(rel); {
		case strings.HasPrefix(rel, "cpufreq/policy"):
			if name == "boost" && f.BoostScope.V != cpuGovBoostPerCPU {
				return nil
			}
			id, err := strconv.Atoi(strings.TrimPrefix(path.Base(dirName), "policy"))
			if err != nil {
				t.Fatalf("governor/%s: %v", rel, err)
			}
			sys = path.Join(layout.PolicyDir(id), name)
		case rel == "cpufreq/boost":
			if f.BoostScope.V != cpuGovBoostGlobal {
				return nil
			}
			sys = path.Join(path.Dir(layout.PolicyDir(0)), "boost")
		case strings.HasPrefix(rel, "intel_pstate/"):
			if !cpuGovIntel(driver) || name == "no_turbo" && f.BoostScope.V != cpuGovBoostNoTurbo {
				return nil
			}
			sys = path.Join(layout.IntelPstateDir(), name)
		case rel == "amd_pstate/status":
			if !strings.HasPrefix(driver, "amd-pstate") {
				return nil
			}
			sys = path.Join(layout.AMDPstateDir(), name)
		default:
			return nil // cpuidle, amdgpu and the NVIDIA clocks are not mapped
		}
		want, err := os.ReadFile(fp)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(tree.SysRoot, filepath.FromSlash(sys))); err != nil || string(got) != string(want) {
			t.Errorf("%s = %q (%v), want the captured governor/%s %q", sys, got, err, rel, want)
		}
		compared++
		return nil
	})
	if compared == 0 {
		t.Errorf("governor/cpufreq is captured, but no governor file was compared")
	}
}

// renderCorpusTestMachine writes a capture into a temporary directory named
// test-machine and returns it.
func renderCorpusTestMachine(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "test-machine")
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// renderCorpusGovernorCapture is a two CPU capture of one driver: the
// cpuinfo with flags, and both policies in the same governor state, which
// policy holds as attribute files.
func renderCorpusGovernorCapture(driver, flags string, policy map[string]string, global map[string]string) map[string]string {
	block := func(id int) string {
		return fmt.Sprintf("processor\t: %d\nvendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 143\nmodel name\t: Neutral Test CPU\n"+
			"stepping\t: 8\nphysical id\t: 0\nsiblings\t: 2\ncore id\t\t: %d\ncpu cores\t: 2\nflags\t\t: %s\n\n", id, id, flags)
	}
	files := map[string]string{
		"machine.yaml":     "description: test machine\nallocatable:\n  cpu: \"2\"\n  memory: 1024Ki\nwriteRejectingZones: []\n",
		"cpuinfo":          block(0) + block(1),
		"node-labels.json": "{}",
		"cpufreq-driver":   driver + "\n",
	}
	for _, pol := range []string{"policy0", "policy1"} {
		for name, body := range policy {
			files["governor/cpufreq/"+pol+"/"+name] = body
		}
	}
	for name, body := range global {
		files["governor/"+name] = body
	}
	return files
}

// GF02: FromCorpus renders the captured governor state instead of the
// scaling driver only: the governor, the available governors, the
// energy-performance preference pair, the boost state where the driver keeps
// it and the driver mode reach the profile's facts, and the rendered tree
// reads them back byte for byte. Under performance amd-pstate-epp renders the
// forced preference the capture shows.
func TestGF02FromCorpusRendersGovernorAndEPP(t *testing.T) {
	t.Parallel()
	freq := map[string]string{"cpuinfo_min_freq": "400000\n", "cpuinfo_max_freq": "1500000\n"}
	with := func(m map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range freq {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	const prefs = "default performance balance_performance balance_power power \n"
	cases := []struct {
		name          string
		files         map[string]string
		governor, epp string
		boost         int
		scope, mode   string
		hwp           bool
	}{
		{
			name: "amd-pstate-epp powersave",
			files: renderCorpusGovernorCapture("amd-pstate-epp", "fpu cppc", with(nil,
				"scaling_governor", "powersave\n", "scaling_available_governors", "performance powersave\n",
				"energy_performance_preference", "balance_power\n", "energy_performance_available_preferences", prefs,
				"boost", "0\n"), map[string]string{"amd_pstate/status": "active\n"}),
			governor: "powersave", epp: "balance_power", boost: 0, scope: cpuGovBoostPerCPU, mode: "active",
		},
		{
			name: "amd-pstate-epp performance",
			files: renderCorpusGovernorCapture("amd-pstate-epp", "fpu cppc", with(nil,
				"scaling_governor", "performance\n", "scaling_available_governors", "performance powersave\n",
				"energy_performance_preference", "performance\n", "energy_performance_available_preferences", "performance\n",
				"boost", "1\n"), map[string]string{"amd_pstate/status": "active\n"}),
			governor: "performance", boost: 1, scope: cpuGovBoostPerCPU, mode: "active",
		},
		{
			name: "intel_cpufreq schedutil with HWP",
			files: renderCorpusGovernorCapture("intel_cpufreq", "fpu hwp hwp_epp", with(nil,
				"scaling_governor", "schedutil\n", "scaling_available_governors", "conservative ondemand userspace powersave performance schedutil \n",
				"energy_performance_preference", "balance_performance\n", "energy_performance_available_preferences", prefs),
				map[string]string{"intel_pstate/status": "passive\n", "intel_pstate/no_turbo": "1\n"}),
			governor: "schedutil", epp: "balance_performance", boost: 0, scope: cpuGovBoostNoTurbo, mode: "passive", hwp: true,
		},
		{
			name: "acpi-cpufreq with the global boost",
			files: renderCorpusGovernorCapture("acpi-cpufreq", "fpu", with(nil,
				"scaling_governor", "ondemand\n", "scaling_available_governors", "conservative ondemand userspace powersave performance schedutil \n"),
				map[string]string{"cpufreq/boost": "1\n"}),
			governor: "ondemand", boost: 1, scope: cpuGovBoostGlobal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := renderCorpusTestMachine(t, tc.files)
			p, err := FromCorpus(dir)
			if err != nil {
				t.Fatal(err)
			}
			f := p.CPUFreq
			if f.Minimal || f.Governor.V != tc.governor || f.Boost.V != tc.boost || f.BoostScope.V != tc.scope ||
				f.DriverMode.V != tc.mode || f.HWP.V != tc.hwp || f.CPUInfoMinKHz.V != 400000 || f.CPUInfoMaxKHz.V != 1500000 {
				t.Errorf("cpufreq = %+v, want governor %s, boost %d in %s, mode %q, hwp %v, 400000 to 1500000 kHz",
					f, tc.governor, tc.boost, tc.scope, tc.mode, tc.hwp)
			}
			gotEPP := ""
			if f.EPP != nil {
				gotEPP = f.EPP.Default.V
			}
			if gotEPP != tc.epp {
				t.Errorf("epp.default = %q, want %q", gotEPP, tc.epp)
			}
			tree := renderTestTree(t, p)
			renderCheckCorpusGovernor(t, dir, tree, p)
			for _, pol := range []int{0, 1} {
				if got := renderReadFile(t, tree, "sys/"+layout.PolicyDir(pol)+"/scaling_governor"); got != tc.governor+"\n" {
					t.Errorf("policy%d/scaling_governor = %q, want %q", pol, got, tc.governor)
				}
			}
		})
	}
}

// GF02: a capture the profile cannot hold is an error rather than a profile
// that renders something else: policies in different states, a driver whose
// status file is missing, a boost file the driver keeps that is missing,
// policies the cpuinfo does not render, and governor state without the
// scaling driver.
func TestGF02FromCorpusRejectsGovernorStateItCannotRender(t *testing.T) {
	t.Parallel()
	good := func() map[string]string {
		return renderCorpusGovernorCapture("amd-pstate-epp", "fpu cppc", map[string]string{
			"scaling_governor": "powersave\n", "scaling_available_governors": "performance powersave\n",
			"energy_performance_preference": "balance_performance\n", "energy_performance_available_preferences": "default performance balance_performance balance_power power \n",
			"boost": "1\n", "cpuinfo_min_freq": "400000\n", "cpuinfo_max_freq": "1500000\n",
		}, map[string]string{"amd_pstate/status": "active\n"})
	}
	if _, err := FromCorpus(renderCorpusTestMachine(t, good())); err != nil {
		t.Fatalf("the good capture: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(files map[string]string)
		want   string
	}{
		{"policies differ", func(f map[string]string) { f["governor/cpufreq/policy1/scaling_governor"] = "performance\n" }, "one state for all policies"},
		{"no status file", func(f map[string]string) { delete(f, "governor/amd_pstate/status") }, "keeps its mode"},
		{"no policy boost", func(f map[string]string) {
			delete(f, "governor/cpufreq/policy0/boost")
			delete(f, "governor/cpufreq/policy1/boost")
		}, "keeps boost"},
		{"no preference pair", func(f map[string]string) {
			for _, pol := range []string{"policy0", "policy1"} {
				delete(f, "governor/cpufreq/"+pol+"/energy_performance_preference")
				delete(f, "governor/cpufreq/"+pol+"/energy_performance_available_preferences")
			}
		}, "preference pair"},
		{"policy the cpuinfo lacks", func(f map[string]string) {
			for k, v := range f {
				if strings.HasPrefix(k, "governor/cpufreq/policy1/") {
					f[strings.Replace(k, "policy1", "policy5", 1)] = v
					delete(f, k)
				}
			}
		}, "neither per CPU nor per core"},
		{"no scaling driver", func(f map[string]string) { delete(f, "cpufreq-driver") }, "no cpufreq-driver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := good()
			tc.mutate(files)
			if _, err := FromCorpus(renderCorpusTestMachine(t, files)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one that says %q", err, tc.want)
			}
		})
	}
}

// GF04: a capture that holds the scaling driver only, as every capture did
// before the script copied governor state, still loads as the same minimal
// cpufreq, scaling_driver alone: every such machine of the corpus, and a
// synthetic one whose governor/ holds no policy.
func TestGF04DriverOnlyCorpusEntriesLoadUnchanged(t *testing.T) {
	t.Parallel()
	minimal := func(driver string) *CPUFreqSpec {
		return &CPUFreqSpec{
			Driver:         Fact[string]{V: driver, Src: "corpus:corpus#cpufreq-driver"},
			PolicyGrouping: Fact[string]{V: "per-cpu", Src: "assumed:the capture records the scaling driver only"},
			Minimal:        true,
		}
	}
	check := func(t *testing.T, dir string) {
		t.Helper()
		driver, err := os.ReadFile(filepath.Join(dir, "cpufreq-driver"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := FromCorpus(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := minimal(strings.TrimSpace(string(driver))); !reflect.DeepEqual(p.CPUFreq, want) {
			t.Errorf("cpufreq = %+v, want the minimal %+v", p.CPUFreq, want)
		}
		tree := renderTestTree(t, p)
		entries, err := os.ReadDir(filepath.Join(tree.SysRoot, layout.PolicyDir(0)))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "scaling_driver" {
			t.Errorf("policy0 holds %v, want scaling_driver alone", entries)
		}
	}
	ran := 0
	for _, machine := range renderCorpusMachines(t) {
		dir := filepath.Join(renderCorpusRoot(), machine)
		_, driverErr := os.Stat(filepath.Join(dir, "cpufreq-driver"))
		_, govErr := os.Stat(filepath.Join(dir, renderCorpusGovernorDir, "cpufreq"))
		if driverErr != nil || govErr == nil {
			continue
		}
		ran++
		t.Run(machine, func(t *testing.T) { check(t, dir) })
	}
	if ran == 0 && os.Getenv("JOULIE_HARDWARE_CORPUS") == "" {
		t.Errorf("no driver-only machine in %s; the repository's corpus has one", renderCorpusRoot())
	}
	t.Run("governor without policies", func(t *testing.T) {
		files := renderCorpusGovernorCapture("intel_pstate", "fpu", nil, map[string]string{"intel_pstate/status": "active\n", "cpuidle/state0/name": "POLL\n"})
		check(t, renderCorpusTestMachine(t, files))
	})
}

// renderCheckCorpusNVML compares the NVML state with the captured query
// output: index, min, max, limit, draw, name.
func renderCheckCorpusNVML(t *testing.T, tree *Tree, text string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.Split(line, ", ")
		mw := func(field int) string {
			v, err := strconv.ParseFloat(parts[field], 64)
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%d\n", int64(math.Round(v*1000)))
		}
		for file, want := range map[string]string{
			"power_min_limit_mw": mw(1),
			"power_max_limit_mw": mw(2),
			"power_limit_mw":     mw(3),
			"name":               strings.Join(parts[5:], ", ") + "\n",
		} {
			if got := renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLGPUDir(i), file)); got != want {
				t.Errorf("gpu%d %s = %q, want %q", i, file, got, want)
			}
		}
		if i == 0 {
			if got := renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLGPUDir(0), "power_draw_mw")); got != mw(4) {
				t.Errorf("gpu0 power_draw_mw = %q, want %q", got, mw(4))
			}
		}
	}
}

// H21: an AMD capture becomes amdgpu hwmon devices with the captured limits
// and the legacy-agent-contract rocm-smi, a zone listed as rejecting that the
// tree lacks is an error, and GPUs that disagree on their limits are an
// error rather than a silently averaged profile.
func TestH21FromCorpusAMDAndErrors(t *testing.T) {
	t.Parallel()
	machine := func(t *testing.T, files map[string]string) string {
		dir := filepath.Join(t.TempDir(), "amd-test-machine")
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cpuinfo := "processor\t: 0\nvendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 17\nmodel name\t: AMD EPYC 9654 96-Core Processor\nstepping\t: 1\nphysical id\t: 0\ncore id\t\t: 0\ncpu cores\t: 1\nsiblings\t: 1\n\n"
	card := `{"Max Graphics Package Power (W)": "750.0", "Min Graphics Package Power (W)": "0.0", "Current Power Cap (W)": "600.0", "Average Graphics Package Power (W)": "131.0", "Card Series": "AMD Instinct MI300X", "Card SKU": "X0000000"}`
	files := map[string]string{
		"machine.yaml":     "description: test AMD machine\nallocatable:\n  cpu: \"1\"\n  memory: 1024Ki\n  gpu:\n    amd.com/gpu: \"2\"\nwriteRejectingZones: []\n",
		"cpuinfo":          cpuinfo,
		"node-labels.json": `{"feature.node.kubernetes.io/pci-1200_1002.present": "true"}`,
		"rocm-smi.txt":     `{"card0": ` + card + `, "card1": ` + card + `}`,
	}
	p, err := FromCorpus(machine(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if p.GPUs.PCI.Class.V != "0x120000" {
		t.Errorf("gpu class = %q, want 0x120000 from the captured NFD label", p.GPUs.PCI.Class.V)
	}
	tree := renderTestTree(t, p)
	if got := renderReadFile(t, tree, "state/tools.json"); got != `{"schemaVersion":1,"tools":{"rocm-smi":{"variant":"legacy-agent-contract"}}}`+"\n" {
		t.Errorf("tools.json = %s", got)
	}
	for i := 0; i < 2; i++ {
		link, _ := layout.HwmonClassLink(i+2, layout.PCIDevDir(0x10+i))
		for file, want := range map[string]string{"power1_cap": "600000000\n", "power1_cap_max": "750000000\n", "power1_cap_min": "0\n", "power1_average": "131000000\n"} {
			if got := renderReadFile(t, tree, "sys/"+link+"/"+file); got != want {
				t.Errorf("card%d %s = %q, want %q", i, file, got, want)
			}
		}
	}
	if got := renderReadFile(t, tree, "sys/"+layout.PCIDevDir(0x10)+"/vbios_version"); !strings.Contains(got, "-X0000000-") {
		t.Errorf("vbios_version = %q, want the captured Card SKU as its middle segment", got)
	}
	if got := renderQty(p.NodeObject("n").Status.Allocatable, "amd.com/gpu"); got != "2" {
		t.Errorf("allocatable amd.com/gpu = %q, want 2", got)
	}

	bad := map[string]string{}
	for k, v := range files {
		bad[k] = v
	}
	bad["machine.yaml"] = strings.Replace(files["machine.yaml"], "writeRejectingZones: []", "writeRejectingZones: [\"intel-rapl:0:0\"]", 1)
	if _, err := FromCorpus(machine(t, bad)); err == nil {
		t.Errorf("a rejecting zone without a powercap tree was accepted")
	}
	bad["machine.yaml"] = files["machine.yaml"]
	bad["rocm-smi.txt"] = `{"card0": ` + card + `, "card1": ` + strings.Replace(card, `"750.0"`, `"500.0"`, 1) + `}`
	if _, err := FromCorpus(machine(t, bad)); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("err = %v, want GPUs with different limits rejected", err)
	}
}
