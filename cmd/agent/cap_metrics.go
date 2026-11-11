package main

// The metrics of what the agent reports in the API: the result of each cap
// as it writes it to status.controlStatus, and the status writes the API
// server refused. A write the API server refuses leaves the NodeTwin and
// NodeHardware showing an older state, so neither can be read from them;
// the alerts of the chart's PrometheusRule (charts/joulie/alerts) read these.

import (
	"slices"
	"sync"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus"
)

// Values of the object label of joulie_agent_status_write_errors_total.
const (
	statusObjectNodeTwin     = "NodeTwin"
	statusObjectNodeHardware = "NodeHardware"
)

var (
	capMetricsOnce          sync.Once
	capResultMetric         *prometheus.GaugeVec
	statusWriteErrorsMetric *prometheus.CounterVec
)

// capMetrics registers the cap result and status write metrics once per
// process.
func capMetrics() (*prometheus.GaugeVec, *prometheus.CounterVec) {
	capMetricsOnce.Do(func() {
		capResultMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_agent_cap_result",
			Help: "Result of the last CPU or GPU cap per component, as in status.controlStatus (1), others 0",
		}, []string{"node", "component", "result"})
		statusWriteErrorsMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_agent_status_write_errors_total",
			Help: "Status writes of the agent that failed, per object (NodeTwin controlStatus or NodeHardware status)",
		}, []string{"node", "object"})
		prometheus.MustRegister(capResultMetric, statusWriteErrorsMetric)
	})
	return capResultMetric, statusWriteErrorsMetric
}

// recordCapResult sets joulie_agent_cap_result of node's component (cpu or
// gpu) to result. Every result in pkg/api.CapResultValues is set, 0
// included, so an alert has a value to compare from the first reconcile on.
func recordCapResult(node, component, result string) {
	g, _ := capMetrics()
	for _, r := range joulie.CapResultValues {
		v := 0.0
		if r == result {
			v = 1
		}
		g.WithLabelValues(node, component, r).Set(v)
	}
	if !slices.Contains(joulie.CapResultValues, result) {
		g.WithLabelValues(node, component, result).Set(1)
	}
}

// recordStatusWriteError counts a failed write of object for node.
func recordStatusWriteError(node, object string) {
	_, c := capMetrics()
	c.WithLabelValues(node, object).Inc()
}

// forgetNodeCapMetrics removes the series of a node the agent no longer
// controls (a pool agent's node that left its shard or selector), so a
// cap result of a node nobody controls never fires an alert.
func forgetNodeCapMetrics(node string) {
	g, c := capMetrics()
	g.DeletePartialMatch(prometheus.Labels{"node": node})
	c.DeletePartialMatch(prometheus.Labels{"node": node})
}
