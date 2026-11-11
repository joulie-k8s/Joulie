package main

// The metrics of the twins' health, which the alerts of the chart's
// PrometheusRule (charts/joulie/alerts/joulie-alerts.yaml) read: how many
// managed nodes got each measured power source in the last reconcile, and
// when the controller manager last wrote each managed node's NodeTwin
// status. The power source gauge has no node label, as
// joulie_twin_nodes_agent_reporting; the nodes are the NodeTwins whose
// status.powerMeasurement.source it counts.

import (
	"sync"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	twinNodesPowerSource = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joulie_twin_nodes_power_source",
		Help: "Managed nodes by the measured power source of their NodeTwin status in the last reconcile (status.powerMeasurement.source): prometheus or http with a reading, prometheus-no-data, http-error or http-no-endpoint without one, static when nothing is measured.",
	}, []string{"source"})

	twinStatusUpdated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joulie_twin_status_updated_timestamp_seconds",
		Help: "Unix time of the NodeTwin status the controller manager last wrote for each managed node (status.lastUpdated), or of the reconcile that first planned the node while none was written.",
	}, []string{"node"})
)

// recordPowerSources sets joulie_twin_nodes_power_source from the power
// sources of one reconcile, one per managed node. Every source in
// pkg/api.PowerSourceValues is set, 0 included, so an alert on the sources
// without a reading has a value to compare from the first reconcile on.
func recordPowerSources(sources []string) {
	counts := map[string]float64{}
	for _, s := range sources {
		counts[s]++
	}
	for _, s := range joulie.PowerSourceValues {
		twinNodesPowerSource.WithLabelValues(s).Set(counts[s])
	}
}

// twinFreshness keeps joulie_twin_status_updated_timestamp_seconds: one
// series per planned node, holding the time of its last NodeTwin status
// write. A node planned before any write holds the time it was first
// planned, so a twin the controller manager never manages to write ages
// like one it stopped writing, and a node that leaves the plan loses its
// series, so it never reads as stale.
type twinFreshness struct {
	mu      sync.Mutex
	written map[string]time.Time
}

var twinStatusFreshness = &twinFreshness{written: map[string]time.Time{}}

// plan starts the nodes of a reconcile's plan that have no series at now,
// and removes the series of every node not in nodes.
func (f *twinFreshness) plan(nodes []string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		keep[n] = true
		if _, ok := f.written[n]; !ok {
			f.written[n] = now
			twinStatusUpdated.WithLabelValues(n).Set(float64(now.Unix()))
		}
	}
	for n := range f.written {
		if !keep[n] {
			delete(f.written, n)
			twinStatusUpdated.DeleteLabelValues(n)
		}
	}
}

// wrote records a NodeTwin status write for node whose lastUpdated is at.
func (f *twinFreshness) wrote(node string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written[node] = at
	twinStatusUpdated.WithLabelValues(node).Set(float64(at.Unix()))
}
