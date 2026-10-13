package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/hw"
)

// Labels of the nodes these tests resolve, spelled as the catalogue aliases.
var (
	mrH100SXMLabels = map[string]string{
		"joulie.io/hw.gpu-model": "NVIDIA H100 80GB HBM3",
		"joulie.io/hw.gpu-count": "8",
	}
	mrProxyCPULabels = map[string]string{
		"joulie.io/hw.cpu-model":   "AMD EPYC 9534 64-Core Processor",
		"joulie.io/hw.cpu-sockets": "2",
	}
)

// mrSimulator returns a simulator on the base model from the environment and
// the embedded catalogue, as main builds it.
func mrSimulator(t *testing.T, classes []simNodeClass) (*simulator, *hw.Catalog) {
	t.Helper()
	catalog, err := hw.LoadCatalog("")
	if err != nil {
		t.Fatalf("embedded catalogue: %v", err)
	}
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, classes, 0, prometheus.NewRegistry())
	s.catalog = catalog
	return s, catalog
}

func mrMergeLabels(sets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, set := range sets {
		for k, v := range set {
			out[k] = v
		}
	}
	return out
}

func mrWriteClasses(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "classes.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// With no SIM_GPU_* power variable set, the GPU board power range of a
// catalogue GPU comes from the catalogue. The old code always took the
// environment defaults, 300 and 80 W, so an H100 SXM capped at 300 W ran
// below its 700 W board power and could be capped down to 80 W.
func TestCatalogGPUCapsReachTheModelWhenEnvIsUnset(t *testing.T) {
	clearSimEnv(t)
	s, catalog := mrSimulator(t, nil)
	spec := catalog.GPUModels["NVIDIA_H100_SXM"].Official
	if spec.MaxBoardPowerW != 700 || spec.MinBoardPowerW <= 80 {
		t.Fatalf("test setup: catalogue H100 SXM board power %v to %v W, want 700 W and a minimum above 80 W", spec.MinBoardPowerW, spec.MaxBoardPowerW)
	}
	model, _ := s.resolveModelForNode(mrH100SXMLabels)
	if model.GPU.MaxWattsPerGPU != 700 || model.GPU.MinCapWattsPerGPU != spec.MinBoardPowerW {
		t.Fatalf("H100 SXM with the environment unset: GPU caps %v to %v W, want %v to 700 W from the catalogue", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU, spec.MinBoardPowerW)
	}
}

// The socket cap range of a catalogue CPU is its TDP and the low end of its
// cTDP range, or 0.55 x TDP without one. The old code kept 120 to 400 W.
func TestCatalogCPUSocketCapsReachTheModelWhenEnvIsUnset(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	tdp8260 := 165.0 // no cTDP range in the catalogue
	for _, tc := range []struct {
		cpu          string
		minW, maxW   float64
		catalogueKey string
	}{
		{cpu: "AMD EPYC 9654 96-Core Processor", minW: 320, maxW: 360, catalogueKey: "AMD_EPYC_9654"},
		{cpu: "INTEL XEON PLATINUM 8260", minW: 0.55 * tdp8260, maxW: tdp8260, catalogueKey: "INTEL_XEON_PLATINUM_8260"},
	} {
		model, _ := s.resolveModelForNode(map[string]string{"joulie.io/hw.cpu-model": tc.cpu, "joulie.io/hw.cpu-sockets": "2"})
		if model.CPUModel != tc.catalogueKey {
			t.Fatalf("test setup: %q matched %q, want %s", tc.cpu, model.CPUModel, tc.catalogueKey)
		}
		if model.CPUSocketCapMinW != tc.minW || model.CPUSocketCapMaxW != tc.maxW {
			t.Errorf("%s socket caps %v to %v W, want %v to %v W", tc.catalogueKey, model.CPUSocketCapMinW, model.CPUSocketCapMaxW, tc.minW, tc.maxW)
		}
	}
}

// A variable that is set wins over the catalogue, for every field the
// catalogue can fill.
func TestExplicitEnvOverridesCatalog(t *testing.T) {
	clearSimEnv(t)
	t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "500")
	t.Setenv("SIM_GPU_MIN_CAP_WATTS_PER_GPU", "250")
	t.Setenv("SIM_P_MAX_W", "450")
	t.Setenv("SIM_CPU_SOCKET_CAP_MIN_W", "150")
	t.Setenv("SIM_CPU_SOCKET_CAP_MAX_W", "300")
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, mrProxyCPULabels))
	if model.GPU.MaxWattsPerGPU != 500 || model.GPU.MinCapWattsPerGPU != 250 {
		t.Errorf("GPU caps %v to %v W, want 250 to 500 W from the environment", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	if model.PMaxW != 450 {
		t.Errorf("pMaxW %v W, want 450 W from the environment", model.PMaxW)
	}
	if model.CPUSocketCapMinW != 150 || model.CPUSocketCapMaxW != 300 {
		t.Errorf("socket caps %v to %v W, want 150 to 300 W from the environment", model.CPUSocketCapMinW, model.CPUSocketCapMaxW)
	}
}

// A node class key wins over the catalogue and over the environment; a key
// the class leaves out still comes from the catalogue.
func TestClassOverridesCatalog(t *testing.T) {
	clearSimEnv(t)
	resolve := func() (simModel, *hw.Catalog) {
		t.Helper()
		classes, err := loadNodeClasses(mrWriteClasses(t, `
classes:
  - name: tuned
    matchLabels: {pool: tuned}
    model:
      pMaxW: 500
      cpuSocketCapMaxW: 250
      gpu:
        maxWattsPerGpu: 600
`))
		if err != nil || len(classes) != 1 {
			t.Fatalf("loadNodeClasses: %d classes, err %v", len(classes), err)
		}
		s, catalog := mrSimulator(t, classes)
		model, _ := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, mrProxyCPULabels, map[string]string{"pool": "tuned"}))
		return model, catalog
	}
	model, catalog := resolve()
	if model.GPU.MaxWattsPerGPU != 600 {
		t.Errorf("GPU max %v W, want 600 W from the class over 700 W from the catalogue", model.GPU.MaxWattsPerGPU)
	}
	if want := catalog.GPUModels["NVIDIA_H100_SXM"].Official.MinBoardPowerW; model.GPU.MinCapWattsPerGPU != want {
		t.Errorf("GPU min %v W, want %v W from the catalogue: the class does not set it", model.GPU.MinCapWattsPerGPU, want)
	}
	if model.PMaxW != 500 || model.CPUSocketCapMaxW != 250 {
		t.Errorf("pMaxW %v W and socket max %v W, want 500 and 250 W from the class", model.PMaxW, model.CPUSocketCapMaxW)
	}
	// 9534: the cTDP low end, 240 W, is below the class maximum of 250 W.
	if model.CPUSocketCapMinW != 240 {
		t.Errorf("socket min %v W, want 240 W, the catalogue cTDP low end", model.CPUSocketCapMinW)
	}

	t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "500")
	if model, _ := resolve(); model.GPU.MaxWattsPerGPU != 600 {
		t.Errorf("GPU max %v W with SIM_GPU_MAX_WATTS_PER_GPU=500, want 600 W from the class", model.GPU.MaxWattsPerGPU)
	}
}

// A catalogue minimum above a maximum set by hand is lowered to that
// maximum, so it never sits above it.
func TestCatalogMinimumNeverExceedsAnExplicitMaximum(t *testing.T) {
	clearSimEnv(t)
	t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "250")
	t.Setenv("SIM_CPU_SOCKET_CAP_MAX_W", "200")
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, mrProxyCPULabels))
	if model.GPU.MinCapWattsPerGPU != 250 || model.CPUSocketCapMinW != 200 {
		t.Fatalf("GPU min %v W under a 250 W maximum, socket min %v W under a 200 W maximum; want each equal to its maximum", model.GPU.MinCapWattsPerGPU, model.CPUSocketCapMinW)
	}
}

// Hardware the catalogue does not know keeps the built-in constants.
func TestUnmatchedGPUKeepsTheOldDefaults(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	model, class := s.resolveModelForNode(map[string]string{
		"joulie.io/hw.gpu-model":   "Generic PCIe Accelerator",
		"joulie.io/hw.gpu-count":   "4",
		"joulie.io/hw.cpu-model":   "Generic Server CPU",
		"joulie.io/hw.cpu-sockets": "2",
	})
	if class != "default" {
		t.Fatalf("test setup: unknown hardware resolved to class %q, want default", class)
	}
	if model.GPU.MaxWattsPerGPU != 300 || model.GPU.MinCapWattsPerGPU != 80 {
		t.Errorf("GPU caps %v to %v W, want 80 to 300 W", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	if model.PMaxW != 420 || model.CPUSocketCapMinW != 120 || model.CPUSocketCapMaxW != 400 {
		t.Errorf("pMaxW %v W, socket caps %v to %v W; want 420 W and 120 to 400 W", model.PMaxW, model.CPUSocketCapMinW, model.CPUSocketCapMaxW)
	}
}

// node2S curves were measured on two sockets and scale by sockets/2. The old
// code gave a one-socket node the two-socket curve.
func TestNode2SCurveScalesWithSocketCount(t *testing.T) {
	clearSimEnv(t)
	s, catalog := mrSimulator(t, nil)
	points := catalog.CPUModels["AMD_EPYC_9654"].MeasuredCurves.Node2S.Points
	for _, sockets := range []string{"1", "2", "4"} {
		model, _ := s.resolveModelForNode(map[string]string{
			"joulie.io/hw.cpu-model":   "AMD EPYC 9654 96-Core Processor",
			"joulie.io/hw.cpu-sockets": sockets,
		})
		scale := map[string]float64{"1": 0.5, "2": 1, "4": 2}[sockets]
		if len(model.CPUPowerCurve) != len(points) {
			t.Fatalf("%s sockets: %d curve points, want %d", sockets, len(model.CPUPowerCurve), len(points))
		}
		for i, p := range points {
			got := model.CPUPowerCurve[i]
			if got.LoadPct != p.LoadPct || got.PowerW != p.PowerW*scale {
				t.Fatalf("%s sockets: point %d is %v W at %v %%, want %v W (node2S x %v)", sockets, i, got.PowerW, got.LoadPct, p.PowerW*scale, scale)
			}
		}
	}
}

// A catalogue CPU without a curve of its own peaks at sockets x TDP: 560 W
// for 2x EPYC 9534 (280 W), where the old code kept 420 W.
func TestProxyCPUPeakFollowsTDP(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(mrProxyCPULabels)
	if len(model.CPUPowerCurve) != 0 || model.CPUProxyFrom == "" {
		t.Fatalf("test setup: 9534 has %d curve points and proxy %q, want no curve and a proxy family", len(model.CPUPowerCurve), model.CPUProxyFrom)
	}
	if model.PMaxW != 560 {
		t.Fatalf("2x EPYC 9534 pMaxW %v W, want 560 W (2 x 280 W TDP)", model.PMaxW)
	}
	oneSocket, _ := s.resolveModelForNode(map[string]string{
		"joulie.io/hw.cpu-model":   "AMD EPYC 9534 64-Core Processor",
		"joulie.io/hw.cpu-sockets": "1",
	})
	if oneSocket.PMaxW != 280 {
		t.Fatalf("1x EPYC 9534 pMaxW %v W, want 280 W", oneSocket.PMaxW)
	}
}

// The removed knobs no longer change the model. Each variable and each class
// key logs one warning, however often the base model and the classes are
// built, and a class file that still carries them loads.
func TestRemovedModelKnobsWarnOnce(t *testing.T) {
	clearSimEnv(t)
	removedKnobWarnMu.Lock()
	removedKnobWarnSeen = map[string]struct{}{}
	removedKnobWarnMu.Unlock()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for _, k := range []string{"SIM_POD_W", "SIM_DVFS_DROP_W_PER_PCT", "SIM_GPU_ALPHA_UTIL", "SIM_GPU_BETA_CAP"} {
		t.Setenv(k, "1")
	}
	path := mrWriteClasses(t, `
classes:
  - name: old
    matchLabels: {pool: old}
    model:
      podW: 110
      dvfsDropWPerPct: 1.6
      noiseStddevW: 2
      gpu:
        count: 2
        powerModel: {alphaUtil: 1.0, betaCap: 1.0}
  - name: older
    matchLabels: {pool: older}
    model:
      podW: 120
`)
	for i := 0; i < 2; i++ {
		_ = defaultBaseModelFromEnv()
		classes, err := loadNodeClasses(path)
		if err != nil || len(classes) != 2 {
			t.Fatalf("class file with removed keys: %d classes, err %v; want 2 and no error", len(classes), err)
		}
	}
	out := buf.String()
	for _, want := range []string{
		"SIM_POD_W was removed",
		"SIM_DVFS_DROP_W_PER_PCT was removed",
		"SIM_GPU_ALPHA_UTIL was removed",
		"SIM_GPU_BETA_CAP was removed",
		"key model.podW was removed",
		"key model.dvfsDropWPerPct was removed",
		"key model.noiseStddevW was removed",
		"key model.gpu.powerModel was removed",
	} {
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("%q logged %d times, want once; log:\n%s", want, n, out)
		}
	}
}
