package main

// The formats of /telemetry/{node}. The route answers by default with
// the OTLP JSON of /otlp/telemetry/{node}, the format the agent's and the
// controller manager's HTTP power inputs read. ?format=legacy keeps the
// legacy packagePowerWatts JSON for one release, until
// otlpjson.LegacyRemovedIn.

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
)

// telemetryFormat is the document /telemetry/{node} answers with.
type telemetryFormat int

const (
	// telemetryFormatOTLP is one OTLP JSON ExportMetricsServiceRequest, the
	// answer of /otlp/telemetry/{node}.
	telemetryFormatOTLP telemetryFormat = iota
	// telemetryFormatLegacy is the legacy packagePowerWatts JSON, deprecated.
	telemetryFormatLegacy
)

// legacyTelemetryWarned is set once the process has logged that
// ?format=legacy is deprecated.
var legacyTelemetryWarned atomic.Bool

// parseTelemetryFormat reads the format query parameter of
// /telemetry/{node}: absent, empty or otlp is OTLP and legacy the legacy
// JSON. Any other value is an error, so a misspelt format is refused rather
// than answered with a document the client did not ask for.
func parseTelemetryFormat(r *http.Request) (telemetryFormat, error) {
	switch f := r.URL.Query().Get("format"); f {
	case "", "otlp":
		return telemetryFormatOTLP, nil
	case "legacy":
		return telemetryFormatLegacy, nil
	default:
		return 0, fmt.Errorf("unknown format %q: want otlp (the default) or legacy", f)
	}
}

// warnLegacyTelemetry logs, once per process, that the legacy JSON is
// deprecated and when it goes.
func warnLegacyTelemetry() {
	if legacyTelemetryWarned.CompareAndSwap(false, true) {
		log.Printf("warning: GET /telemetry/{node}?format=legacy is deprecated and removed in %s; /telemetry/{node} serves the OTLP JSON of /otlp/telemetry/{node} by default, which the agent's and the controller manager's http power sources read", otlpjson.LegacyRemovedIn)
	}
}
