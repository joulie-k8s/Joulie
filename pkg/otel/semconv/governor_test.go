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

// GV05: the governor stage registers hw.gpu.io, joulie.hw.gpu.speed,
// joulie.hw.gpu.speed.limit and joulie.hw.governor with units By, Hz, Hz
// and 1, the instruments and attributes the design names, and its five
// attributes; the Prometheus rows are checked by PN01 and PN02 in package
// promname against its golden table.
func TestGV05GovernorNamesFrozen(t *testing.T) {
	frozen := []struct {
		v     Metric
		name  string
		unit  string
		owner string
		kind  Kind
		attrs []string
	}{
		{HwGPUIO, "hw.gpu.io", "By", OwnerSemconv, Counter,
			[]string{"hw.id", "hw.name", "hw.vendor", "hw.model", "network.io.direction", "joulie.hw.gpu.io.path"}},
		{JoulieHwGPUSpeed, "joulie.hw.gpu.speed", "Hz", OwnerJoulie, Gauge,
			[]string{"hw.id", "hw.vendor", "hw.model", "joulie.hw.clock.domain"}},
		{JoulieHwGPUSpeedLimit, "joulie.hw.gpu.speed.limit", "Hz", OwnerJoulie, Gauge,
			[]string{"hw.id", "hw.vendor", "hw.model", "joulie.hw.clock.domain", "hw.limit_type"}},
		{JoulieHwGovernor, "joulie.hw.governor", "1", OwnerJoulie, UpDownCounter,
			[]string{"hw.id", "hw.type", "joulie.hw.governor.preset", "joulie.hw.governor.native"}},
	}
	registered := indexMetrics()
	for _, f := range frozen {
		if f.v.Name != f.name || f.v.Unit != f.unit || f.v.Owner != f.owner || f.v.Kind != f.kind || f.v.Value != Int {
			t.Errorf("%s: got (%q, %q, %q, %v, %v), want (%q, %q, %v, int)", f.name, f.v.Name, f.v.Unit, f.v.Owner, f.v.Kind, f.v.Value, f.unit, f.owner, f.kind)
		}
		if !reflect.DeepEqual(sortedCopy(f.v.Attributes), sortedCopy(f.attrs)) {
			t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
		}
		if got, ok := registered[f.name]; !ok || !reflect.DeepEqual(got, f.v) {
			t.Errorf("%s: registered %+v (%v), exported variable %+v", f.name, got, ok, f.v)
		}
	}

	attrs := map[string]bool{}
	for _, a := range Attributes() {
		attrs[a] = true
	}
	for got, want := range map[string]string{
		AttrNetworkIODirection: "network.io.direction", AttrJoulieGPUIOPath: "joulie.hw.gpu.io.path",
		AttrJoulieClockDomain: "joulie.hw.clock.domain", AttrJoulieGovernorPreset: "joulie.hw.governor.preset",
		AttrJoulieGovernorNative: "joulie.hw.governor.native",
	} {
		if got != want {
			t.Errorf("attribute constant %q, want %q", got, want)
		}
		if !attrs[got] {
			t.Errorf("attribute %q is not registered", got)
		}
	}
	for name, pair := range map[string][2][]string{
		"GPUIOPaths":   {GPUIOPaths(), {"peer", "host", "nic"}},
		"ClockDomains": {ClockDomains(), {"graphics", "memory", "fabric", "soc"}},
		"directions":   {{NetworkIODirectionReceive, NetworkIODirectionTransmit}, {"receive", "transmit"}},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
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

	for got, want := range map[string]string{ErrorTypeEINVAL: "einval", ErrorTypeEOPNOTSUPP: "eopnotsupp", ErrorTypeEBUSY: "ebusy"} {
		if got != want {
			t.Errorf("error.type %q, want %q", got, want)
		}
	}
}
