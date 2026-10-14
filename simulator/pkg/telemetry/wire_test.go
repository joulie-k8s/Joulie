package telemetry

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// wireUpdate rewrites testdata/wire.golden.txt from the encoders:
//
//	go test ./simulator/pkg/telemetry/ -run TM15 -update
var wireUpdate = flag.Bool("update", false, "rewrite simulator/pkg/telemetry/testdata/wire.golden.txt")

// TM15: the bytes the simulator puts on the wire, one export per line: the
// metrics of the governor snapshot (every instrument, value type and
// attribute shape the encoder writes), the same with values OTLP JSON
// cannot carry (left out and counted), one node alone at another time, and
// the logs of every event kind. The golden holds the encoding to the byte,
// whichever builder writes it.
func TestTM15WireOutputGolden(t *testing.T) {
	var buf bytes.Buffer
	line := func(what string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		fmt.Fprintf(&buf, "%s\t%s\n", what, b)
	}
	r := testResource()

	d, dropped := MetricsRequest(testGovernorSnapshot(), r)
	line(fmt.Sprintf("metrics dropped=%d", dropped), d)

	bad := testGovernorSnapshot()
	bad.Nodes[0].Host.PowerW = math.NaN()
	bad.Nodes[0].Components[0].EnergyJ = math.Inf(1)
	bad.Nodes[1].Host.EnergyJ = 1e30
	bad.Nodes[1].Components[0].EnergyJ = -2.5
	bad.Facility.PUE = math.Inf(-1)
	d, dropped = MetricsRequest(bad, r)
	line(fmt.Sprintf("non-finite dropped=%d", dropped), d)

	s := testGovernorSnapshot()
	n, dropped := NodeRequest(s.Nodes[1], s.Cluster, testNow.Add(1500*time.Millisecond), r)
	line(fmt.Sprintf("node dropped=%d", dropped), n)
	n, dropped = NodeRequest(s.Nodes[2], s.Cluster, testNow, r)
	line(fmt.Sprintf("guest dropped=%d", dropped), n)

	recs := append(HardwareEvents(testEventSnapshot()),
		NodeMismatch("intel-0", semconv.ErrorTypeProfileMismatch, "intel-xeon-2s-rapl", "amd-epyc-2s-hsmp", testNow),
		NodeMismatch("vm-0", semconv.ErrorTypeNodeMissing, "vm-no-powercap", "", testNow),
		EventRecord{Event: semconv.EventSimNodeReady, Time: time.Time{}})
	line("logs", logsRequest(recs, "smoke", testNow.Add(2*time.Second), r))

	path := filepath.Join("testdata", "wire.golden.txt")
	if *wireUpdate {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	got := bytes.Split(buf.Bytes(), []byte("\n"))
	wl := bytes.Split(want, []byte("\n"))
	for i := 0; i < len(got) || i < len(wl); i++ {
		var g, w []byte
		if i < len(got) {
			g = got[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("%s line %d differs:\n got  %s\n want %s", path, i+1, g, w)
		}
	}
}
