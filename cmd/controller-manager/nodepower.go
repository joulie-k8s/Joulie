package main

// Measured node power. NODE_POWER_SOURCE picks the source: prometheus runs
// the query of a preset from package inputs, http reads the telemetry
// endpoint of each node (nodepower_http.go), static measures nothing.
//
// The prometheus source runs one grouped query per reconcile and reads every
// node from its answer, keyed by the node label. A custom query holding
// {node} runs once per node instead, and every series it returns counts:
// a node is never read from its first series alone.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"k8s.io/client-go/dynamic"
)

// The node power settings, set at start by inputConfig.install.
// resolveNodePower reads only these, so it works without a setup call: the
// zero values are the static source, and under the prometheus source the
// otel preset, or the custom one when nodePowerPromQuery is set.
var (
	nodePowerSource       string // "prometheus", "http", or "static"
	nodePowerHTTPEndpoint string // e.g. "http://sim:18080/telemetry/{node}"
	nodePowerHTTPClient   = &http.Client{Timeout: 5 * time.Second}

	// nodePowerPromAddress is the Prometheus the node power query goes to.
	nodePowerPromAddress string
	// nodePowerPromQuery is the custom preset's query, in watts. With
	// {node} it runs once per node and its series are summed; without, it
	// is summed by the node label.
	nodePowerPromQuery string
	// nodePowerSpec holds the other prometheus settings: preset, scope,
	// translation strategy, resource attributes mode, rate window and node
	// label. Its CustomQuery is ignored: nodePowerPromQuery is the query.
	nodePowerSpec inputs.NodePowerSpec
)

// resolveNodePower returns the best available measured power for a node.
//
// The source is selected via NODE_POWER_SOURCE:
//   - "prometheus": the node's value in the answer to the node power query.
//   - "http": queries an HTTP telemetry endpoint (e.g. simulator).
//   - "static" (default): returns 0.
func resolveNodePower(ctx context.Context, _ dynamic.Interface, nodeName string, hw joulie.NodeHardware) (float64, string) {
	switch nodePowerSource {
	case "prometheus":
		if nodePowerPromAddress != "" {
			if power, ok := prometheusNodePower(ctx, nodeName); ok && power > 0 {
				return power, joulie.PowerSourcePrometheus
			}
		}
		// Fallback: utilization-based estimation is not yet implemented.
		// When available, it will query CPU/GPU utilization metrics from
		// Prometheus and estimate power using the hardware TDP curve.
		return 0, joulie.PowerSourcePrometheusNoData

	case "http":
		if nodePowerHTTPEndpoint != "" {
			power, err := queryNodePowerHTTP(ctx, nodeName)
			if err != nil {
				log.Printf("[twin] http node power for %s: %v", nodeName, err)
				return 0, joulie.PowerSourceHTTPError
			}
			return power, joulie.PowerSourceHTTP
		}
		return 0, joulie.PowerSourceHTTPNoEndpoint

	default:
		return 0, joulie.PowerSourceStatic
	}
}

// prometheusNodePower reads the node from Prometheus. ok is false when the
// query failed or did not name the node.
func prometheusNodePower(ctx context.Context, nodeName string) (float64, bool) {
	spec := nodePowerSpec
	spec.CustomQuery = nodePowerPromQuery
	primary, fallback, err := spec.Query()
	if err != nil {
		nodePowerHintOnce("spec", fmt.Sprintf("node power query: %v", err))
		return 0, false
	}
	c := inputs.Client{Address: nodePowerPromAddress, HTTP: nodePowerHTTPClient}
	if primary.PerNode {
		return perNodePower(ctx, c, primary, nodeName)
	}
	values := roundNodePower(ctx, func() map[string]float64 {
		return groupedNodePower(ctx, c, spec, primary, fallback)
	})
	v, ok := values[nodeName]
	return v, ok
}

// nodePowerRoundKey is the context key of a nodePowerRound.
type nodePowerRoundKey struct{}

// nodePowerRound is one reconcile's answer to the grouped node power query,
// shared by every node of that reconcile.
type nodePowerRound struct {
	once   sync.Once
	values map[string]float64
}

// withNodePowerRound returns a context under which every resolveNodePower
// shares one grouped query. reconcileWithCatalog starts one per reconcile;
// a call outside a round runs its own query, so it never reads a value
// from an earlier reconcile.
func withNodePowerRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, nodePowerRoundKey{}, &nodePowerRound{})
}

// roundNodePower returns the round's values, running run on the first call
// of the round, or run's values when ctx carries no round.
func roundNodePower(ctx context.Context, run func() map[string]float64) map[string]float64 {
	r, ok := ctx.Value(nodePowerRoundKey{}).(*nodePowerRound)
	if !ok {
		return run()
	}
	r.once.Do(func() { r.values = run() })
	return r.values
}

// perNodePower runs q, which holds {node}, for one node and sums every
// series it returns: a query that splits a node by component, socket or
// producer still reads the whole node.
func perNodePower(ctx context.Context, c inputs.Client, q inputs.Query, nodeName string) (float64, bool) {
	res, err := c.Query(ctx, strings.ReplaceAll(q.PromQL, inputs.NodePlaceholder, nodeName))
	if err != nil {
		recordInputQuery(inputNodePower, inputResultError)
		log.Printf("[twin] prometheus node power for %s: %v", nodeName, err)
		return 0, false
	}
	logQueryWarnings(res.Warnings)
	finite := 0
	for _, s := range res.Samples {
		if !math.IsNaN(s.Value) && !math.IsInf(s.Value, 0) {
			finite++
		}
	}
	switch {
	case finite == 0:
		recordInputQuery(inputNodePower, inputResultNoData)
		return 0, false
	case finite < len(res.Samples):
		recordInputQuery(inputNodePower, inputResultPartial)
	default:
		recordInputQuery(inputNodePower, inputResultSuccess)
	}
	if len(res.Samples) > 1 {
		nodePowerHintOnce("series:"+nodeName, fmt.Sprintf(
			"node power query for %s returned %d series; their sum is the node's power, so aggregate the query if two of them measure the same component",
			nodeName, len(res.Samples)))
	}
	return res.Sum(), true
}

// nodePowerRead is the outcome of one grouped node power query.
type nodePowerRead struct {
	values map[string]float64
	issues inputs.Issues
	err    error
}

// result is the result label of joulie_twin_input_queries_total.
func (r nodePowerRead) result() string {
	switch {
	case r.err != nil:
		return inputResultError
	case len(r.values) == 0:
		return inputResultNoData
	case r.issues.MissingLabel > 0 || r.issues.NonFinite > 0:
		return inputResultPartial
	}
	return inputResultSuccess
}

// runGroupedNodePower runs q and reads its answer by q.GroupLabel.
func runGroupedNodePower(ctx context.Context, c inputs.Client, q inputs.Query) nodePowerRead {
	res, err := c.Query(ctx, q.PromQL)
	if err != nil {
		r := nodePowerRead{err: err}
		recordInputQuery(inputNodePower, r.result())
		log.Printf("[twin] node power query: %v", err)
		return r
	}
	logQueryWarnings(res.Warnings)
	values, iss, err := res.ByLabel(q.GroupLabel)
	r := nodePowerRead{values: values, issues: iss, err: err}
	recordInputQuery(inputNodePower, r.result())
	if err != nil {
		log.Printf("[twin] node power query: %v", err)
	}
	return r
}

// The form PROMETHEUS_RESOURCE_ATTRIBUTES=auto reads first: the one that
// worked last, the labels form before any query.
var (
	nodePowerAutoMu   sync.Mutex
	nodePowerAutoForm inputs.ResourceAttributes
)

// groupedNodePower runs the grouped node power query and returns watts by
// node. Under auto, primary is the labels form and fallback the target_info
// form: the form that worked last runs first, and the other runs when it
// fails (see autoFormWorks). The other form is then kept while it works,
// and every change is logged.
func groupedNodePower(ctx context.Context, c inputs.Client, spec inputs.NodePowerSpec, primary, fallback inputs.Query) map[string]float64 {
	if fallback.PromQL == "" {
		r := runGroupedNodePower(ctx, c, primary)
		hintNodePower(spec, spec.ResourceAttributes, false, primary, r)
		return r.values
	}

	nodePowerAutoMu.Lock()
	first := nodePowerAutoForm
	nodePowerAutoMu.Unlock()
	if first == "" {
		first = inputs.ResourceAttributesLabels
	}
	second, firstQ, secondQ := inputs.ResourceAttributesTargetInfo, primary, fallback
	if first == inputs.ResourceAttributesTargetInfo {
		second, firstQ, secondQ = inputs.ResourceAttributesLabels, fallback, primary
	}

	r1 := runGroupedNodePower(ctx, c, firstQ)
	if autoFormWorks(first, r1) {
		setNodePowerAutoForm(first)
		return r1.values
	}
	r2 := runGroupedNodePower(ctx, c, secondQ)
	if autoFormWorks(second, r2) {
		setNodePowerAutoForm(second)
		return r2.values
	}
	hintNodePower(spec, first, true, firstQ, r1)
	hintNodePower(spec, second, true, secondQ, r2)
	if len(r2.values) > len(r1.values) {
		return r2.values
	}
	return r1.values
}

// autoFormWorks reports whether auto keeps reading form: it answered
// without an error and with data, and in the labels form every series
// carried the node label. A labels form whose series lack the label means
// Prometheus does not promote k8s.node.name, which target_info solves.
func autoFormWorks(form inputs.ResourceAttributes, r nodePowerRead) bool {
	if r.err != nil || len(r.values) == 0 {
		return false
	}
	return form != inputs.ResourceAttributesLabels || r.issues.MissingLabel == 0
}

// setNodePowerAutoForm records the form auto reads and logs a change,
// including the first form that works.
func setNodePowerAutoForm(form inputs.ResourceAttributes) {
	nodePowerAutoMu.Lock()
	prev := nodePowerAutoForm
	nodePowerAutoForm = form
	nodePowerAutoMu.Unlock()
	if prev == form {
		return
	}
	if form == inputs.ResourceAttributesTargetInfo {
		log.Printf("[twin] node power: PROMETHEUS_RESOURCE_ATTRIBUTES=auto reads the node from target_info, since the data series do not carry it (Prometheus does not promote %s)", semconv.AttrK8sNodeName)
		return
	}
	log.Printf("[twin] node power: PROMETHEUS_RESOURCE_ATTRIBUTES=auto reads the node label on the data series")
}

// hintNodePower logs, once per kind, what to change when a grouped query
// read no node or not every node. auto marks a form auto tried.
func hintNodePower(spec inputs.NodePowerSpec, form inputs.ResourceAttributes, auto bool, q inputs.Query, r nodePowerRead) {
	preset := spec.Preset
	if preset == "" {
		preset = inputs.NodePowerPresetOTel
		if spec.CustomQuery != "" {
			preset = inputs.NodePowerPresetCustom
		}
	}
	naming := spec.Naming
	if naming == "" {
		naming = promname.Default
	}
	label := q.GroupLabel
	var perr *inputs.Error
	switch {
	case errors.As(r.err, &perr) && (perr.Status == http.StatusUnprocessableEntity ||
		strings.Contains(perr.Message, "many-to-many") || strings.Contains(perr.Message, "duplicate series")):
		nodePowerHintOnce("join", fmt.Sprintf(
			"node power: Prometheus could not join with target_info (%v); two nodes share one job and instance, so give every producer its own %s",
			r.err, semconv.AttrServiceInstanceID))
	case r.err != nil && r.issues.Duplicates > 0:
		nodePowerHintOnce("duplicates", fmt.Sprintf(
			"node power: several series name one node (%v); Prometheus stores two producers as one target when they share %s",
			r.err, semconv.AttrServiceInstanceID))
	case r.err != nil:
		// Already logged; an unreachable Prometheus needs no hint.
	case r.issues.MissingLabel > 0:
		nodePowerHintOnce(fmt.Sprintf("label:%s:%s", preset, form), missingNodeLabelHint(preset, form, auto, label, r.issues.MissingLabel))
	case r.issues.Series == 0:
		nodePowerHintOnce(fmt.Sprintf("nodata:%s", preset), noNodePowerHint(preset, naming))
	}
}

// missingNodeLabelHint says how to give the node power series their node.
func missingNodeLabelHint(preset inputs.NodePowerPreset, form inputs.ResourceAttributes, auto bool, label string, n int) string {
	switch {
	case preset == inputs.NodePowerPresetOTel && form == inputs.ResourceAttributesTargetInfo:
		return fmt.Sprintf("node power: %d series found no %s on target_info: every producer must set the resource attributes %s and its own %s",
			n, label, semconv.AttrK8sNodeName, semconv.AttrServiceInstanceID)
	case preset == inputs.NodePowerPresetOTel:
		h := fmt.Sprintf("node power: %d series lack the label %s: add %s to Prometheus' otlp.promote_resource_attributes",
			n, label, semconv.AttrK8sNodeName)
		if !auto {
			h += ", or set PROMETHEUS_RESOURCE_ATTRIBUTES=auto or target_info"
		}
		return h
	case preset == inputs.NodePowerPresetCustom:
		return fmt.Sprintf("node power: the custom query is summed by the label %s, which %d of its series lack: set NODE_POWER_NODE_LABEL to the label that names the node, or put %s in the query to run it once per node",
			label, n, inputs.NodePlaceholder)
	}
	return fmt.Sprintf("node power: %d series lack the label %s: set NODE_POWER_NODE_LABEL to the label that names the node (Prometheus renames Kepler's instance label to exported_instance only when it scrapes with honor_labels: false)",
		n, label)
}

// noNodePowerHint says where node power series come from under preset.
func noNodePowerHint(preset inputs.NodePowerPreset, naming promname.Strategy) string {
	switch preset {
	case inputs.NodePowerPresetKepler:
		return "node power: no Kepler series (kepler_node_cpu_joules_total, Kepler 0.10 and later); Kepler 0.7 to 0.9 is read with NODE_POWER_PRESET=kepler-legacy"
	case inputs.NodePowerPresetKeplerLegacy:
		return "node power: no Kepler 0.7 to 0.9 series (kepler_node_package_joules_total); Kepler 0.10 and later is read with NODE_POWER_PRESET=kepler"
	case inputs.NodePowerPresetCustom:
		return "node power: the custom query (NODE_POWER_PROMETHEUS_QUERY) returned no series"
	}
	return fmt.Sprintf("node power: no series; producers push OTLP to Prometheus' /api/v1/otlp/v1/metrics, which needs --web.enable-otlp-receiver, and PROMETHEUS_NAME_TRANSLATION (%s) must equal Prometheus' otlp.translation_strategy",
		naming)
}

var (
	nodePowerHintsMu   sync.Mutex
	nodePowerHintsSeen = map[string]struct{}{}
)

// nodePowerHintOnce logs msg the first time it sees key.
func nodePowerHintOnce(key, msg string) {
	nodePowerHintsMu.Lock()
	_, seen := nodePowerHintsSeen[key]
	nodePowerHintsSeen[key] = struct{}{}
	nodePowerHintsMu.Unlock()
	if !seen {
		log.Printf("[twin] hint: %s", msg)
	}
}

// logQueryWarnings logs each Prometheus warning once.
func logQueryWarnings(warnings []string) {
	for _, w := range warnings {
		nodePowerHintOnce("warning:"+w, "node power query: Prometheus warns: "+w)
	}
}
