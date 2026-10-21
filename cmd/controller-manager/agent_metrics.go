package main

// The metric of the AgentReporting condition: how many managed nodes an
// agent reports for, and why the others have none, so an alert needs no
// NodeTwin read. The node itself is on its NodeTwin's condition; the gauge
// carries no node label, so its size does not grow with the cluster.

import (
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var twinNodesAgentReporting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "joulie_twin_nodes_agent_reporting",
	Help: "Managed nodes by their NodeTwin's AgentReporting condition, as of the last reconcile: status true (an agent publishes the node's NodeHardware) or false (the caps planned there are not enforced), and the condition's reason.",
}, []string{"status", "reason"})

// agentReportingSeries are the status and reason pairs the condition takes.
// Each is set on every reconcile, 0 included, so an alert on the false
// series has a value to compare from the first reconcile on.
var agentReportingSeries = []struct{ status, reason string }{
	{"true", joulie.ReasonNodeHardwareCurrent},
	{"false", joulie.ReasonNoNodeHardware},
	{"false", joulie.ReasonNodeHardwareStale},
}

// recordAgentReporting sets joulie_twin_nodes_agent_reporting from the
// AgentReporting conditions of one reconcile, one per managed node. A node
// whose NodeHardware could not be read has no condition and is not counted.
func recordAgentReporting(conditions []metav1.Condition) {
	counts := map[[2]string]float64{}
	for _, c := range conditions {
		counts[[2]string{strings.ToLower(string(c.Status)), c.Reason}]++
	}
	for _, s := range agentReportingSeries {
		twinNodesAgentReporting.WithLabelValues(s.status, s.reason).Set(counts[[2]string{s.status, s.reason}])
	}
}
