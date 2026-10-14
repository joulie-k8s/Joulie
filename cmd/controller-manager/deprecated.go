package main

// Compatibility shims, in two groups with their own removal release:
//   - the rename of the binary from "operator" to "controller manager":
//     legacyEnvAliases and the deprecated metric names, removed in the next
//     minor release;
//   - the OpenTelemetry inputs: inputEnvAliases, the legacy facility preset
//     and the legacy HTTP JSON body, added in v0.3.0 and removed in
//     inputAliasesRemovedIn.
//
// Delete each group's aliases, lookups and docs rows together.

import (
	"fmt"
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

// legacyEnvAliasesRemovedIn is when the OPERATOR_* names go.
const legacyEnvAliasesRemovedIn = "the next minor release"

// inputEnvAliases maps a facility query variable to the FACILITY_*_METRIC
// name it replaced. The old value is a bare metric name, which the facility
// reader aggregates (avg for a temperature, sum for a power), so it keeps
// meaning what it did.
var inputEnvAliases = map[string]string{
	"FACILITY_AMBIENT_TEMP_QUERY":  "FACILITY_AMBIENT_TEMP_METRIC",
	"FACILITY_IT_POWER_QUERY":      "FACILITY_IT_POWER_METRIC",
	"FACILITY_COOLING_POWER_QUERY": "FACILITY_COOLING_POWER_METRIC",
}

// inputAliasesRemovedIn is when inputEnvAliases, FACILITY_PRESET=legacy and
// the legacy HTTP JSON body go: otlpjson.LegacyRemovedIn, the release every
// component names (TestInputAliasesGoWithTheLegacyFormats). It is spelled
// as a literal because tests/docs reads it from this file to check the
// configuration reference.
const inputAliasesRemovedIn = "v0.4.0"

var (
	deprecatedEnvWarnMu   sync.Mutex
	deprecatedEnvWarnSeen = map[string]struct{}{}
)

// envOrDeprecated is envOrDefault for a variable listed in legacyEnvAliases
// or inputEnvAliases: the current name wins, the old name is used when the
// current one is unset, and def applies when neither is set. Reading the old
// name logs one warning per variable naming the replacement and the release
// that removes the old name.
func envOrDeprecated(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	old, removedIn, ok := deprecatedEnvAlias(key)
	if !ok {
		return def
	}
	v := strings.TrimSpace(os.Getenv(old))
	if v == "" {
		return def
	}
	warnDeprecatedOnce(old, fmt.Sprintf("%s is deprecated and will be removed in %s; set %s instead", old, removedIn, key))
	return v
}

// deprecatedEnvAlias returns the old name of key and the release that
// removes it.
func deprecatedEnvAlias(key string) (old, removedIn string, ok bool) {
	if old, ok := legacyEnvAliases[key]; ok {
		return old, legacyEnvAliasesRemovedIn, true
	}
	if old, ok := inputEnvAliases[key]; ok {
		return old, inputAliasesRemovedIn, true
	}
	return "", "", false
}

// warnDeprecatedOnce logs "warning: " + msg the first time it sees key.
func warnDeprecatedOnce(key, msg string) {
	deprecatedEnvWarnMu.Lock()
	_, seen := deprecatedEnvWarnSeen[key]
	deprecatedEnvWarnSeen[key] = struct{}{}
	deprecatedEnvWarnMu.Unlock()
	if !seen {
		log.Printf("warning: %s", msg)
	}
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
