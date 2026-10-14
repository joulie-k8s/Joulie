package telemetry

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// TM12: the Pusher stamps every point with the time it collects the
// export, as a periodic reader that collects and exports at once does, not
// with the time of the snapshot it reads. A point stamped with the snapshot
// time reached Prometheus up to one emulation tick plus the tick's own
// duration old; past 1.1 export intervals, Prometheus caps the rate()
// extrapolation beyond the newest sample at half an interval, and the
// facility IT power read 0.885 to 0.897 of the truth over a 30 s window.
// Counters still start at their ledger's start.
func TestTM12PusherStampsTheCollectionTime(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()
	snap := testSnapshot() // taken at testNow, long before the push
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(),
		Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := p.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	bodies := recv.got("/v1/metrics")
	if len(bodies) != 1 {
		t.Fatalf("%d requests, want 1", len(bodies))
	}
	d, err := otlpjson.DecodeMetricsStrict(bodies[0])
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]otlpjson.Uint64String{
		"intel-0":        testUnixNano(testNodeStart),
		"nvl-0":          testUnixNano(testNodeStart.Add(time.Minute)),
		FacilityInstance: testUnixNano(testFacStart),
	}
	lo, hi := otlpjson.Uint64String(before.UnixNano()), otlpjson.Uint64String(after.UnixNano())
	points := 0
	for id, rm := range testResourceByInstance(t, d) {
		for name, m := range testMetrics(t, rm) {
			for _, pt := range testPoints(m) {
				points++
				if pt.TimeUnixNano < lo || pt.TimeUnixNano > hi {
					t.Errorf("%s %s: timeUnixNano %d, want the push time, in [%d, %d] (the snapshot time is %d)",
						id, name, pt.TimeUnixNano, lo, hi, testUnixNano(snap.Time))
				}
				if m.Sum != nil && pt.StartTimeUnixNano != starts[id] {
					t.Errorf("%s %s: startTimeUnixNano %d, want the ledger start %d", id, name, pt.StartTimeUnixNano, starts[id])
				}
			}
		}
	}
	if points == 0 {
		t.Fatal("the export has no point")
	}
}
