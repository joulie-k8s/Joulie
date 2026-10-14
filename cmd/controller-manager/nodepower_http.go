package main

// Measured node power from an HTTP telemetry endpoint
// (NODE_POWER_SOURCE=http). NODE_POWER_HTTP_ENDPOINT names it, with {node}
// replaced by the node name, and each answer is one OTLP JSON
// ExportMetricsServiceRequest, such as the simulator's
// /otlp/telemetry/{node}. The node is read from it with the rules of the
// prometheus source (otlpjson.PowerReader): energy first, as a rate, the
// power gauge as the fallback, one value per component, under
// NODE_POWER_SCOPE. Any other JSON object is the legacy packagePowerWatts
// JSON, read until otlpjson.LegacyRemovedIn with one deprecation warning per
// endpoint (otlpjson.ReadPower).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// nodePowerHTTPKey identifies the readers that share energy points: one
// endpoint read with one rate window.
type nodePowerHTTPKey struct {
	endpoint string
	window   time.Duration
}

// nodePowerHTTPReaders holds an *otlpjson.PowerReader per nodePowerHTTPKey.
// A reader keeps the previous energy points of every node between
// reconciles, so a rate needs two reconciles no further apart than the rate
// window (PROMETHEUS_RATE_WINDOW).
var nodePowerHTTPReaders sync.Map

// nodePowerHTTPReader returns the reader of endpoint and window.
func nodePowerHTTPReader(endpoint string, window time.Duration) *otlpjson.PowerReader {
	k := nodePowerHTTPKey{endpoint: endpoint, window: window}
	if r, ok := nodePowerHTTPReaders.Load(k); ok {
		return r.(*otlpjson.PowerReader)
	}
	r, _ := nodePowerHTTPReaders.LoadOrStore(k, &otlpjson.PowerReader{MaxInterval: window})
	return r.(*otlpjson.PowerReader)
}

// nodePowerHTTPSelector picks what the power of nodeName is made of under
// scope: by default the CPU packages and the GPUs (hw.energy and hw.power
// with hw.type cpu or gpu), the boundary of the twin's budget, and under
// host the whole host (hw.host.energy and hw.host.power).
func nodePowerHTTPSelector(nodeName string, scope inputs.Scope) otlpjson.Selector {
	if scope == inputs.ScopeHost {
		return otlpjson.Selector{Node: nodeName, Host: true}
	}
	return otlpjson.Selector{Node: nodeName, HwTypes: []string{semconv.HwTypeCPU, semconv.HwTypeGPU}}
}

// queryNodePowerHTTP fetches the power of nodeName from the HTTP telemetry
// endpoint, selected by nodePowerSpec.Scope. It returns 0 and no error when
// the answer holds no reading of the node: an OTLP body that selects
// nothing, or that has an energy series without a power gauge still warming
// up, or a legacy body without a positive packagePowerWatts.
//
// The legacy body's packagePowerWatts is read whatever NODE_POWER_SCOPE
// says, and holds what its producer put there: the simulator puts every CPU
// package and GPU of the node there (a modelled node's power, for a node
// outside the cluster spec), and the packages alone under cpu.
func queryNodePowerHTTP(ctx context.Context, nodeName string) (float64, error) {
	url := strings.ReplaceAll(nodePowerHTTPEndpoint, "{node}", nodeName)
	body, err := otlpjson.FetchTelemetry(ctx, nodePowerHTTPClient, url)
	if err != nil {
		return 0, err
	}
	r := nodePowerHTTPReader(nodePowerHTTPEndpoint, nodePowerSpec.Window)
	w, ok, legacy, err := otlpjson.ReadPower(body, r, nodePowerHTTPSelector(nodeName, nodePowerSpec.Scope))
	if legacy {
		warnDeprecatedOnce("legacy-http-json:"+nodePowerHTTPEndpoint, fmt.Sprintf(
			"NODE_POWER_HTTP_ENDPOINT %s answered with the legacy packagePowerWatts JSON, whose power is whatever its producer put in packagePowerWatts, whatever NODE_POWER_SCOPE says; it is deprecated and will be removed in %s: serve one OTLP JSON metrics export with %s or %s points of %s %s and %s, or %s or %s points for NODE_POWER_SCOPE=%s",
			nodePowerHTTPEndpoint, otlpjson.LegacyRemovedIn, semconv.HwEnergy.Name, semconv.HwPower.Name, semconv.AttrHwType,
			semconv.HwTypeCPU, semconv.HwTypeGPU, semconv.HwHostEnergy.Name, semconv.HwHostPower.Name, inputs.ScopeHost))
	}
	if err != nil {
		return 0, fmt.Errorf("telemetry from %s: %w", url, err)
	}
	if !ok {
		return 0, nil
	}
	return w, nil
}
