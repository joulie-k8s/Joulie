package main

// Compatibility shims for the rename of the binary from "operator" to
// "controller manager". Everything in this file is kept for one minor release
// after the rename shipped; delete the file, the legacyEnvAliases lookups and
// the deprecated metric names together.

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// legacyEnvAliases maps a current environment variable to the OPERATOR_*
// spelling it replaced. envOrDeprecated reads the old name when the new one is
// unset and warns once per variable.
var legacyEnvAliases = map[string]string{
	"NODE_POWER_SOURCE":             "OPERATOR_NODE_POWER_SOURCE",
	"NODE_POWER_HTTP_ENDPOINT":      "OPERATOR_NODE_POWER_HTTP_ENDPOINT",
	"NODE_POWER_PROMETHEUS_ADDRESS": "OPERATOR_NODE_POWER_PROMETHEUS_ADDRESS",
	"NODE_POWER_PROMETHEUS_QUERY":   "OPERATOR_NODE_POWER_PROMETHEUS_QUERY",
}

var (
	deprecatedEnvWarnMu   sync.Mutex
	deprecatedEnvWarnSeen = map[string]struct{}{}
)

// envOrDeprecated is envOrDefault for a variable listed in legacyEnvAliases:
// the current name wins, the old name is used when the current one is unset,
// and def applies when neither is set. Reading the old name logs one warning
// per variable naming the replacement.
func envOrDeprecated(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	old, ok := legacyEnvAliases[key]
	if !ok {
		return def
	}
	v := strings.TrimSpace(os.Getenv(old))
	if v == "" {
		return def
	}
	deprecatedEnvWarnMu.Lock()
	_, seen := deprecatedEnvWarnSeen[old]
	deprecatedEnvWarnSeen[old] = struct{}{}
	deprecatedEnvWarnMu.Unlock()
	if !seen {
		log.Printf("warning: %s is deprecated and will be removed in the next minor release; set %s instead", old, key)
	}
	return v
}

// dualGaugeVec writes every sample to the current metric and to its
// deprecated joulie_operator_* alias, so dashboards keep working for one
// release while they migrate to the joulie_policy_* names.
type dualGaugeVec struct {
	cur *prometheus.GaugeVec
	old *prometheus.GaugeVec
}

func newDualGaugeVec(name, oldName, help string, labels []string) dualGaugeVec {
	return dualGaugeVec{
		cur: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels),
		old: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: oldName, Help: "Deprecated, use " + name + "."}, labels),
	}
}

// Set records v under the given label values on both metrics.
func (d dualGaugeVec) Set(v float64, labelValues ...string) {
	d.cur.WithLabelValues(labelValues...).Set(v)
	d.old.WithLabelValues(labelValues...).Set(v)
}

func (d dualGaugeVec) collectors() []prometheus.Collector {
	return []prometheus.Collector{d.cur, d.old}
}

// dualCounterVec is dualGaugeVec for counters.
type dualCounterVec struct {
	cur *prometheus.CounterVec
	old *prometheus.CounterVec
}

func newDualCounterVec(name, oldName, help string, labels []string) dualCounterVec {
	return dualCounterVec{
		cur: prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels),
		old: prometheus.NewCounterVec(prometheus.CounterOpts{Name: oldName, Help: "Deprecated, use " + name + "."}, labels),
	}
}

// Inc increments the given label values on both metrics.
func (d dualCounterVec) Inc(labelValues ...string) {
	d.cur.WithLabelValues(labelValues...).Inc()
	d.old.WithLabelValues(labelValues...).Inc()
}

func (d dualCounterVec) collectors() []prometheus.Collector {
	return []prometheus.Collector{d.cur, d.old}
}
