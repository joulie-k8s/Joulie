package hwemu

import (
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The NVML state holds what the fake nvidia-smi reports: limits in mW, the
// default limit as the starting limit, idle power, a GPU- uuid that differs
// per node and per GPU, and, for a field gpus.fieldSupport marks
// unsupported, the token in place of the value (D16).
func TestH02NVMLState(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "nvidia-h100-nvl-8gpu")
	tree := renderTestTree(t, p)
	gpu := func(i int, file string) string {
		return renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLGPUDir(i), file))
	}
	for file, want := range map[string]string{
		"name":               "NVIDIA H100 NVL\n",
		"power_min_limit_mw": "200000\n",
		"power_max_limit_mw": "400000\n",
		// The driver default is 310 W; the profile's currentW starts the
		// limit at the 400 W maximum an admin set.
		"power_default_limit_mw":  "310000\n",
		"power_limit_mw":          "400000\n",
		"enforced_power_limit_mw": "400000\n",
		"power_draw_mw":           "58600\n",
		"total_energy_mj":         "0\n",
		"utilization_gpu_pct":     "0\n",
		"pci_bus_id":              "00000000:17:00.0\n",
	} {
		if got := gpu(7, file); got != want {
			t.Errorf("gpu7 %s = %q, want %q", file, got, want)
		}
	}
	uuid := regexp.MustCompile(`^GPU-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n$`)
	if u0, u1 := gpu(0, "uuid"), gpu(1, "uuid"); !uuid.MatchString(u0) || u0 == u1 {
		t.Errorf("uuids %q %q: want distinct GPU-8-4-4-4-12 ids", u0, u1)
	}
	if renderGPUUUID("node-a", 0) == renderGPUUUID("node-b", 0) {
		t.Errorf("two nodes share a GPU uuid")
	}
	if !strings.HasSuffix(renderReadFile(t, tree, path.Join(layout.StateDir, layout.NVMLDir, "driver_version")), "\n") {
		t.Errorf("driver_version has no newline")
	}

	p.GPUs.FieldSupport = map[string]string{"power.min_limit": "[N/A]", "power.max_limit": "[N/A]"}
	tree = renderTestTree(t, p)
	if got := gpu(3, "power_min_limit_mw"); got != "[N/A]\n" {
		t.Errorf("unsupported power.min_limit = %q, want the token", got)
	}
	if got := gpu(3, "power_limit_mw"); got != "400000\n" {
		t.Errorf("a supported field changed: power_limit_mw = %q", got)
	}
	p.GPUs.FieldSupport = map[string]string{"power.minimum": "[N/A]"}
	if err := p.Validate(); err == nil {
		t.Errorf("a fieldSupport key that is no query field was accepted")
	}
}
