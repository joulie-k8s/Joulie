package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

// testNode names the emulated node of the sweep; it is no real machine.
const testNode = "sweep-node-0"

// writeSweep writes a sweep of amd-epyc-2s-energy-only under the
// performance governor, with the analytic model of its own facts in place
// of its catalog curve, so the fit has them to give back, to
// dir/sweep.jsonl and returns its path: two
// frequency ceilings and three loads, 22 s each, sampled every 2 s and
// exported as the simulator exports a node, plus the CPU utilization of the
// whole host and the setpoints.
func writeSweep(t *testing.T, dir string) string {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["amd-epyc-2s-energy-only"]
	p.CPUFreq.Governor.V = "performance"
	p.Physics.CPU.Model = "analytic"
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: testNode, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_760_000_000, 0)
	n, err := hwemu.NewNode(p, tree, hwemu.Options{Start: start, RenderCheckInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	policies, err := filepath.Glob(filepath.Join(tree.SysRoot, filepath.FromSlash(filepath.Dir(layout.PolicyDir(0))), "policy*", "scaling_max_freq"))
	if err != nil || len(policies) == 0 {
		t.Fatalf("no cpufreq policies: %v", err)
	}
	res := telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "hwcalib-test"}
	var buf bytes.Buffer
	now := start
	for _, khz := range []string{"2400000", "1500000"} {
		for _, f := range policies {
			if err := os.WriteFile(f, []byte(khz+"\n"), 0); err != nil {
				t.Fatal(err)
			}
		}
		for _, u := range []float64{0, 0.5, 1} {
			n.SetLoad(hwemu.Load{CPUUtil: []float64{u}})
			hz := map[string]float64{"2400000": 2.4e9, "1500000": 1.5e9}[khz]
			for k := 0; k < 11; k++ {
				now = now.Add(2 * time.Second)
				if err := n.AdvanceTo(now); err != nil {
					t.Fatal(err)
				}
				st := n.State()
				snap := emulate.NodeSnapshot{Name: testNode, StartTime: start,
					Host: emulate.HostReading{Measured: true, PowerW: st.Host.PowerW, EnergyJ: st.Host.EnergyJ, OtherW: st.Host.OtherW, InletC: 24}}
				snap.Components = append(snap.Components, st.Components...)
				md, _ := telemetry.NodeRequest(snap, "hwcalib-test", now, res)
				md.ResourceMetrics[0].ScopeMetrics = append(md.ResourceMetrics[0].ScopeMetrics, otlpjson.ScopeMetrics{
					Scope: otlpjson.Scope{Name: "hwcalib-test"}, SchemaURL: semconv.SchemaURL,
					Metrics: []otlpjson.Metric{
						otlpjson.GaugeMetric(semconv.SystemCPUUtilization,
							otlpjson.DoublePoint(time.Time{}, now, 1-u, otlpjson.String(semconv.AttrCPUMode, semconv.CPUModeIdle))),
						otlpjson.GaugeMetric(semconv.JoulieCalibrationSetpoint,
							otlpjson.DoublePoint(time.Time{}, now, hz, otlpjson.String(semconv.AttrCalibrationKnob, semconv.KnobCPUFrequencyMax)),
							otlpjson.DoublePoint(time.Time{}, now, u, otlpjson.String(semconv.AttrCalibrationKnob, semconv.KnobCPUUtilization))),
					},
				})
				b, err := json.Marshal(md)
				if err != nil {
					t.Fatal(err)
				}
				buf.Write(append(b, '\n'))
			}
		}
	}
	path := filepath.Join(dir, "sweep.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// hwcalibMainEnv set to 1 makes the test binary run hwcalib itself, for the
// tests that signal a running sweep.
const hwcalibMainEnv = "HWCALIB_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(hwcalibMainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

// noEnv is a getenv with nothing set.
func noEnv(string) string { return "" }

// env is a getenv with the given variables set.
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func runHwcalib(stdin []byte, getenv func(string) string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, bytes.NewReader(stdin), &stdout, &stderr, getenv)
	return code, stdout.String(), stderr.String()
}

// CB10: hwcalib fit end to end: the sweep in, a report out, a local profile
// written that loads with its fitted facts; a second run does not replace
// it; SIM_PROFILES_DIR is the default directory and standard input a valid
// input; repository paths, usage errors and an unknown profile fail with
// their own exit status.
func TestCB10FitEndToEnd(t *testing.T) {
	sweepPath := writeSweep(t, t.TempDir())
	out := t.TempDir()

	code, stdout, stderr := runHwcalib(nil, noEnv, "fit", "-profile", "amd-epyc-2s-energy-only", "-node", testNode,
		"-out-dir", out, "-name", "epyc-calibrated", sweepPath)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"node " + testNode, "physics.cpu.alphaUtil", "cpu-ols", "physics.cpu.betaFreq", "not fitted: no hw.energy or hw.power of a memory component", "wrote " + filepath.Join(out, "epyc-calibrated.yaml")} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	profiles, local, err := hwemu.LoadProfileSet(out)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["epyc-calibrated"]
	if len(local) != 1 || p == nil {
		t.Fatalf("local profiles %v", local)
	}
	cpu := p.Physics.CPU
	if cpu.IdlePkgW.V != 40 || cpu.MaxPkgW.V != 360 || cpu.AlphaUtil.V != 1.15 || cpu.BetaFreq.V != 1.35 || !strings.HasPrefix(cpu.AlphaUtil.Src, "measured:sweep#") {
		t.Errorf("fitted cpu physics idle %v max %v alpha %v (%s) beta %v", cpu.IdlePkgW.V, cpu.MaxPkgW.V, cpu.AlphaUtil.V, cpu.AlphaUtil.Src, cpu.BetaFreq.V)
	}
	if src := p.Sources["sweep"]; src.Kind != "measured" || !strings.Contains(src.Ref, "sweep.jsonl") || !strings.Contains(src.Ref, testNode) {
		t.Errorf("sweep source %+v", src)
	}

	t.Run("an existing profile is not replaced", func(t *testing.T) {
		code, _, stderr := runHwcalib(nil, noEnv, "fit", "-profile", "amd-epyc-2s-energy-only", "-out-dir", out, "-name", "epyc-calibrated", sweepPath)
		if code != 1 || !strings.Contains(stderr, "exists") {
			t.Errorf("exit %d, stderr %q; want 1 and a refusal", code, stderr)
		}
	})

	t.Run("SIM_PROFILES_DIR and standard input", func(t *testing.T) {
		dir := t.TempDir()
		in, err := os.ReadFile(sweepPath)
		if err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runHwcalib(in, env(map[string]string{envProfilesDir: " " + dir + " "}), "fit", "-profile", "amd-epyc-2s-energy-only", "-")
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "amd-epyc-2s-energy-only-measured.yaml")); err != nil {
			t.Errorf("default name in SIM_PROFILES_DIR: %v", err)
		} else if !strings.Contains(string(b), "ref: hwcalib fit of standard input") {
			t.Errorf("the sweep source does not name standard input:\n%s", b)
		}
		// The measured profile is now a local base for the next fit.
		code, stdout, stderr = runHwcalib(nil, env(map[string]string{envProfilesDir: dir}), "fit", "-profile", "amd-epyc-2s-energy-only-measured", "-name", "second", sweepPath)
		if code != 0 {
			t.Errorf("fit against a local base: exit %d\n%s\n%s", code, stdout, stderr)
		}
	})

	t.Run("a local base outside the output directory", func(t *testing.T) {
		code, _, stderr := runHwcalib(nil, env(map[string]string{envProfilesDir: out}), "fit", "-profile", "epyc-calibrated", "-out-dir", t.TempDir(), sweepPath)
		if code != 1 || !strings.Contains(stderr, "removed") {
			t.Errorf("exit %d, stderr %q; want 1 and the written file removed", code, stderr)
		}
	})

	t.Run("repository paths", func(t *testing.T) {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..", "..")
		for _, dir := range []string{root, filepath.Join(root, "simulator", "pkg", "hwemu", "profiles")} {
			// The input does not exist: the refusal comes before any work.
			code, _, stderr := runHwcalib(nil, noEnv, "fit", "-profile", "amd-epyc-2s-energy-only", "-out-dir", dir, filepath.Join(t.TempDir(), "absent.jsonl"))
			if code != 1 || !strings.Contains(stderr, "inside the repository") {
				t.Errorf("-out-dir %s: exit %d, stderr %q; want 1 and a refusal", dir, code, stderr)
			}
			code, _, stderr = runHwcalib(nil, env(map[string]string{envProfilesDir: dir}), "fit", "-profile", "amd-epyc-2s-energy-only", sweepPath)
			if code != 1 || !strings.Contains(stderr, "inside the repository") {
				t.Errorf("%s=%s: exit %d, stderr %q; want 1 and a refusal", envProfilesDir, dir, code, stderr)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			getenv func(string) string
			args   []string
			code   int
			want   string
		}{
			{"no command", noEnv, nil, 2, "usage"},
			{"unknown command", noEnv, []string{"calibrate"}, 2, "unknown command"},
			{"no profile", noEnv, []string{"fit", "-out-dir", out, sweepPath}, 2, "-profile is required"},
			{"no input", noEnv, []string{"fit", "-profile", "x", "-out-dir", out}, 2, "one input file"},
			{"bad format", noEnv, []string{"fit", "-profile", "x", "-format", "xml", "-out-dir", out, sweepPath}, 2, "neither otlp nor csv"},
			{"no output directory", noEnv, []string{"fit", "-profile", "x", sweepPath}, 2, envProfilesDir + " is unset"},
			{"unknown profile", noEnv, []string{"fit", "-profile", "no-such-profile", "-out-dir", t.TempDir(), sweepPath}, 1, "no profile \"no-such-profile\""},
			{"unknown node", noEnv, []string{"fit", "-profile", "amd-epyc-2s-energy-only", "-node", "other", "-out-dir", t.TempDir(), sweepPath}, 1, "\"other\""},
			{"csv that is not csv", noEnv, []string{"fit", "-profile", "amd-epyc-2s-energy-only", "-format", "csv", "-out-dir", t.TempDir(), sweepPath}, 1, "csv"},
			{"sweep with no load", noEnv, []string{"sweep", "-node", testNode, "-util", ""}, 2, "-util is empty"},
			{"sweep limits without write-limits", noEnv, []string{"sweep", "-node", testNode, "-freq-max-mhz", "2000"}, 2, "write-limits"},
			{"sweep over an existing file", noEnv, []string{"sweep", "-node", testNode, "-out", sweepPath}, 1, "exists"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, _, stderr := runHwcalib(nil, tc.getenv, tc.args...)
				if code != tc.code || !strings.Contains(stderr, tc.want) {
					t.Errorf("exit %d, stderr %q; want %d and %q", code, stderr, tc.code, tc.want)
				}
			})
		}
	})
}

// A sweep that has written limits and gets SIGHUP, as when the SSH session
// it runs in drops, puts back every file it wrote and exits 1 rather than
// die with the node still capped; under nohup, where SIGHUP is ignored, the
// hangup leaves it running. The sweep runs as its own process over an
// emulated tree, with a load of 0, so no CPU of this machine is kept busy.
func TestSweepPutsBackOnHangup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("signals a child process the Linux way")
	}
	for _, nohup := range []bool{false, true} {
		name := "hangup"
		if nohup {
			name = "nohup"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			profiles, err := hwemu.BuiltinProfiles()
			if err != nil {
				t.Fatal(err)
			}
			tree, err := hwemu.Render(profiles["intel-xeon-2s-rapl"], filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: testNode, Seed: 1})
			if err != nil {
				t.Fatal(err)
			}
			proc := t.TempDir()
			if err := os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu  2 0 0 2 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0\ncpu1 1 0 0 1 0 0 0 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, p := range []string{"devices/system/cpu/cpufreq/policy*/scaling_max_freq", "devices/virtual/powercap/intel-rapl/intel-rapl:*/constraint_0_power_limit_uw"} {
				m, err := filepath.Glob(filepath.Join(tree.SysRoot, filepath.FromSlash(p)))
				if err != nil || len(m) == 0 {
					t.Fatalf("no %s: %v", p, err)
				}
				files = append(files, m...)
			}
			read := func() string {
				var sb strings.Builder
				for _, f := range files {
					b, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					sb.WriteString(f + "=" + string(b))
				}
				return sb.String()
			}
			before := read()

			out := filepath.Join(t.TempDir(), "sweep.jsonl")
			args := []string{os.Args[0], "sweep", "-node", testNode, "-sys-root", tree.SysRoot, "-proc-root", proc,
				"-util", "0", "-write-limits", "-freq-max-mhz", "1500", "-power-limit-w", "100", "-hold", "1h", "-period", "1h", "-out", out}
			if nohup {
				// An ignored signal stays ignored across exec, as nohup does it.
				args = append([]string{"/bin/sh", "-c", `trap '' HUP; exec "$@"`, "sh"}, args...)
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Env = append(os.Environ(), hwcalibMainEnv+"=1")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() {
				cmd.Process.Kill()
				<-done
			}()
			// The signals are handled before the first file is written.
			for deadline := time.Now().Add(30 * time.Second); read() == before; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the sweep wrote no limit; stderr %q", stderr.String())
				}
			}
			if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
				t.Fatal(err)
			}
			if nohup {
				select {
				case err := <-done:
					done <- err
					t.Fatalf("the sweep ended on a hangup under nohup: %v; stderr %q", err, stderr.String())
				case <-time.After(500 * time.Millisecond):
				}
				if read() == before {
					t.Fatal("the limits were put back while the sweep went on")
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var err2 error
			select {
			case err2 = <-done:
				done <- err2
			case <-time.After(30 * time.Second):
				t.Fatal("the sweep did not end")
			}
			var ee *exec.ExitError
			if !errors.As(err2, &ee) || ee.ExitCode() != 1 {
				t.Errorf("sweep ended with %v, want exit status 1; stderr %q", err2, stderr.String())
			}
			if after := read(); after != before {
				t.Errorf("files not put back:\nbefore %s\nafter  %s", before, after)
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s with no sample was not removed: %v", out, err)
			}
		})
	}
}
