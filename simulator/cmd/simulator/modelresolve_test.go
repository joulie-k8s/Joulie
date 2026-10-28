package main

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
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
		maxW         float64
		catalogueKey string
	}{
		{cpu: "AMD EPYC 9654 96-Core Processor", maxW: 360, catalogueKey: "AMD_EPYC_9654"},
		{cpu: "INTEL XEON PLATINUM 8260", maxW: tdp8260, catalogueKey: "INTEL_XEON_PLATINUM_8260"},
	} {
		model, _ := s.resolveModelForNode(map[string]string{"joulie.io/hw.cpu-model": tc.cpu, "joulie.io/hw.cpu-sockets": "2"})
		if model.CPUModel != tc.catalogueKey {
			t.Fatalf("test setup: %q matched %q, want %s", tc.cpu, model.CPUModel, tc.catalogueKey)
		}
		if model.CPUSocketCapMaxW != tc.maxW {
			t.Errorf("%s socket cap maximum %v W, want %v W", tc.catalogueKey, model.CPUSocketCapMaxW, tc.maxW)
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
	t.Setenv("SIM_CPU_SOCKET_CAP_MAX_W", "300")
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, mrProxyCPULabels))
	if model.GPU.MaxWattsPerGPU != 500 || model.GPU.MinCapWattsPerGPU != 250 {
		t.Errorf("GPU caps %v to %v W, want 250 to 500 W from the environment", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	if model.PMaxW != 450 {
		t.Errorf("pMaxW %v W, want 450 W from the environment", model.PMaxW)
	}
	if model.CPUSocketCapMaxW != 300 {
		t.Errorf("socket cap maximum %v W, want 300 W from the environment", model.CPUSocketCapMaxW)
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
	s, _ := mrSimulator(t, nil)
	model, _ := s.resolveModelForNode(mrMergeLabels(mrH100SXMLabels, mrProxyCPULabels))
	if model.GPU.MinCapWattsPerGPU != 250 {
		t.Fatalf("GPU min %v W under a 250 W maximum, want it equal to the maximum", model.GPU.MinCapWattsPerGPU)
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
	if model.PMaxW != 420 || model.CPUSocketCapMaxW != 400 {
		t.Errorf("pMaxW %v W, socket cap maximum %v W; want 420 and 400 W", model.PMaxW, model.CPUSocketCapMaxW)
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

// A node whose only GPU facts are the AMD node labeller's labels resolves
// to the board the controller manager resolves it to: both read
// amd.com/gpu.product-name and beta.amd.com/gpu.product-name before
// amd.com/gpu.product and the family code, which names no board. The cases
// and boards are the controller manager's
// (TestGPULabelRegressionChartKeysAMDFamilyByProductName in
// cmd/controller-manager), whose family gpu:<key> is the catalogue key here.
func TestAMDLabelOnlyNodeResolvesLikeTheControllerManager(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"w7900", map[string]string{
			"amd.com/gpu.family":       "GC_11_0_0",
			"amd.com/gpu.product-name": "AMD_Radeon_PRO_W7900",
		}, "AMD_RADEON_PRO_W7900"},
		{"mi300x", map[string]string{
			"amd.com/gpu.family":            "AI",
			"amd.com/gpu.product-name":      "AMD_Instinct_MI300X_OAM",
			"beta.amd.com/gpu.family":       "AI",
			"beta.amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM",
		}, "AMD_INSTINCT_MI300X"},
		{"beta prefix only", map[string]string{
			"beta.amd.com/gpu.family":       "AI",
			"amd.com/gpu.family":            "AI",
			"beta.amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM",
		}, "AMD_INSTINCT_MI300X"},
		{"older amd.com/gpu.product", map[string]string{
			"amd.com/gpu.family":  "AI",
			"amd.com/gpu.product": "AMD_Instinct_MI300X",
		}, "AMD_INSTINCT_MI300X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := mrMergeLabels(tc.labels, map[string]string{"joulie.io/hw.gpu-count": "4"})
			model, class := s.resolveModelForNode(labels)
			if model.GPU.Product != tc.want {
				t.Fatalf("GPU %q (class %s), want the catalogue's %s, as the controller manager resolves it", model.GPU.Product, class, tc.want)
			}
		})
	}
}

// A node whose only GPU facts are the labels of Intel's NFD rules resolves
// to the catalogue entry gpu.intel.com/product names, as the controller
// manager reads that label before the AMD family code.
func TestIntelLabelOnlyNodeResolvesLikeTheControllerManager(t *testing.T) {
	clearSimEnv(t)
	s, _ := mrSimulator(t, nil)
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"flex 170", map[string]string{"gpu.intel.com/family": "Flex_Series", "gpu.intel.com/product": "Flex_170"}, "INTEL_DATA_CENTER_GPU_FLEX_170"},
		{"max 1550", map[string]string{"gpu.intel.com/family": "Max_Series", "gpu.intel.com/product": "Max_1550"}, "INTEL_DATA_CENTER_GPU_MAX_1550"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := mrMergeLabels(tc.labels, map[string]string{"joulie.io/hw.gpu-count": "4"})
			model, class := s.resolveModelForNode(labels)
			if model.GPU.Product != tc.want {
				t.Fatalf("GPU %q (class %s), want the catalogue's %s, as the controller manager resolves it", model.GPU.Product, class, tc.want)
			}
		})
	}
}

// Every catalog CPU gets the socket cap range of hwinv's SocketCapRangeW,
// the range the agent resolves a percent cap against when it controls the
// node over HTTP (cmd/agent cpuCapRangeFromCatalog). A second derivation
// here would let the agent ask for a cap the modelled node clamps (#36).
func TestCatalogCPUSocketCapRangeIsTheAgentsRange(t *testing.T) {
	clearSimEnv(t)
	s, catalog := mrSimulator(t, nil)
	if len(catalog.CPUModels) == 0 {
		t.Fatal("test setup: the embedded catalogue has no CPU")
	}
	for key, spec := range catalog.CPUModels {
		_, maxW, ok := spec.Official.SocketCapRangeW()
		if !ok {
			t.Fatalf("%s: no socket cap range", key)
		}
		model, _ := s.resolveModelForNode(map[string]string{"joulie.io/hw.cpu-model": key, "joulie.io/hw.cpu-sockets": "2"})
		if model.CPUModel != key {
			t.Fatalf("test setup: %s matched %q", key, model.CPUModel)
		}
		if model.CPUSocketCapMaxW != maxW {
			t.Errorf("%s: socket cap maximum %v W, want hwinv's %v W", key, model.CPUSocketCapMaxW, maxW)
		}
	}
}

// The agent sends a catalog-resolved percent cap as the node's total, sockets
// x the socket range, and the simulator splits rapl.set_power_cap_watts
// evenly over the sockets: every cap of the agent's range lands on the same
// point of each socket's range, unclamped. The middle of the range tells a
// node total from a per-socket cap, which the socket maximum would clamp
// (#36).
func TestAgentCatalogCapIsTheNodeTotalTheSimulatorSplits(t *testing.T) {
	clearSimEnv(t)
	s, catalog := mrSimulator(t, nil)
	const node = "kwok-epyc-9654"
	model, _ := s.resolveModelForNode(map[string]string{"joulie.io/hw.cpu-model": "AMD-EPYC-9654-96-Core-Processor", "joulie.io/hw.cpu-sockets": "2"})
	s.mu.Lock()
	s.nodeModels[node] = model
	s.mu.Unlock()
	minW, maxW, _ := catalog.CPUModels["AMD_EPYC_9654"].Official.SocketCapRangeW()
	for _, perSocket := range []float64{minW, (minW + maxW) / 2, maxW} {
		total := 2 * perSocket
		w := httptest.NewRecorder()
		s.handleControl(w, httptest.NewRequest(http.MethodPost, "/control/"+node,
			strings.NewReader(fmt.Sprintf(`{"action":"rapl.set_power_cap_watts","capWatts":%v}`, total))))
		if w.Code != http.StatusOK {
			t.Fatalf("control status=%d body=%s", w.Code, w.Body)
		}
		st := s.getNode(node)
		s.mu.Lock()
		stepNode(st, model, 10, physMaxSubstep) // 10 s of simulated time
		target, sockets := st.TargetCapWatts, append([]cpuSocketState(nil), st.CPUSockets...)
		s.mu.Unlock()
		if target != total {
			t.Fatalf("node cap target %v W, want the %v W the agent sent", target, total)
		}
		if len(sockets) != 2 {
			t.Fatalf("%d sockets, want 2", len(sockets))
		}
		for _, sk := range sockets {
			if math.Abs(sk.CapWatts-perSocket) > 1e-3 {
				t.Errorf("socket %d cap %v W, want %v W: the node total %v W split over 2 sockets", sk.Index, sk.CapWatts, perSocket, total)
			}
		}
	}
}
