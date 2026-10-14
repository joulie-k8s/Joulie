package semconv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// joulieExtensions lists the Joulie attributes a semconv metric carries on
// top of its definition. The conventions let an instrumentation add
// attributes; each addition is named here so SV02 still rejects any other.
var joulieExtensions = map[string][]string{
	// Where the bytes go: a peer GPU, the host or a NIC. The conventions
	// split hw.gpu.io by direction only.
	"hw.gpu.io": {AttrJoulieGPUIOPath},
}

// GV05: the governor stage's metrics, attributes and values are pinned in
// frozenMetrics, frozenAttributes and frozenValues (SV01); the clock domains
// keep their order, and the Prometheus rows are checked by PN01 and PN02 in
// package promname against its golden table.
func TestGV05GovernorNamesFrozen(t *testing.T) {
	if got, want := ClockDomains(), []string{"graphics", "memory", "fabric", "soc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ClockDomains() = %v, want %v", got, want)
	}
	// The Prometheus spellings under the default strategy, as the golden
	// table of package promname pins them (PN01 checks every row against
	// the translator; this checks the rows the design names are there).
	golden, err := os.ReadFile(filepath.Join("..", "promname", "testdata", "prometheus-names.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	prom := map[string]string{}
	for _, line := range strings.Split(string(golden), "\n") {
		if f := strings.Split(line, "\t"); len(f) >= 5 && f[0] == "metric" {
			prom[f[1]] = f[4]
		}
	}
	for name, want := range map[string]string{
		"hw.gpu.io": "hw_gpu_io_bytes_total", "joulie.hw.gpu.speed": "joulie_hw_gpu_speed_hertz",
		"joulie.hw.gpu.speed.limit": "joulie_hw_gpu_speed_limit_hertz", "joulie.hw.governor": "joulie_hw_governor",
	} {
		if got := prom[name]; got != want {
			t.Errorf("golden Prometheus name of %s is %q, want %q", name, got, want)
		}
	}
}
