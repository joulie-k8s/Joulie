// Package compat serves the emulated nodes in the formats of real exporters,
// so the Joulie input presets written for them can run without the
// hardware: Kepler v0.7.12 (kepler-0.7, read by the kepler-legacy preset),
// Kepler v0.12.0 (kepler-0.12, read by the kepler preset), dcgm-exporter
// 4.8.4 (dcgm-4.8.4, an NVIDIA GPU source for a Collector bridge) and ROCm
// device-metrics-exporter v1.5.2 (amd-dme-1.5.2, an AMD GPU source for a
// Collector bridge). The simulator's own telemetry is OpenTelemetry; these
// exporters are opt-in.
//
// Every exporter is computed from the emulate.Snapshot of each tick, which
// holds the counters the agent and the fake tools read, and is served in
// the Prometheus text format at /exporters/<name>/metrics for every
// emulated node at once. The names, labels and constant labels are pinned
// in upstream.go with the upstream file and line they reproduce; where an
// exporter approximates its upstream, the comment says assumed:.
//
// Like a real exporter, an exporter's own counters start at zero when the
// simulator starts. Kepler v0.12.0 cpu and gpu totals, the DCGM energy and
// the AMD GPU energy are hardware counters upstream, so they report the
// snapshot's ledger and device counters, which continue across a restart.
// A node without package energy, such as a virtual machine, gets no Kepler
// series.
package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// The exporter names SIM_EXPORTERS takes.
const (
	Kepler07  = "kepler-0.7"
	Kepler012 = "kepler-0.12"
	DCGM484   = "dcgm-4.8.4"
	AMDDME152 = "amd-dme-1.5.2"
)

// PathPrefix and PathSuffix frame an exporter's path:
// /exporters/<name>/metrics.
const (
	PathPrefix = "/exporters/"
	PathSuffix = "/metrics"
)

// Options configure a Set.
type Options struct {
	// DCGMHostnameLabel names the dcgm-4.8.4 label that holds the node
	// name: "hostname", as dcgm-exporter writes it from 4.8.3 on and the
	// default when empty, or "Hostname", as releases up to 4.8.2 wrote it.
	DCGMHostnameLabel string
}

// Names returns every exporter name, in a fixed order.
func Names() []string {
	return []string{Kepler07, Kepler012, DCGM484, AMDDME152}
}

// Path returns the path an exporter is served at.
func Path(name string) string {
	return PathPrefix + name + PathSuffix
}

// exporter is one compatibility format: observe computes its series from a
// snapshot, and the Collector half serves the series of the last one.
type exporter interface {
	prometheus.Collector
	observe(snap *emulate.Snapshot)
}

// Set is the enabled exporters. Observe and the handler may run on
// different goroutines; a scrape sees the series of the last observed
// snapshot.
type Set struct {
	mu        sync.Mutex // serialises Observe
	enabled   []string
	exporters map[string]exporter
	registry  map[string]*prometheus.Registry
	handlers  map[string]http.Handler
}

// NewSet enables the named exporters. Names are trimmed, and empty and
// repeated names are skipped, so a comma-separated SIM_EXPORTERS can be
// split and passed as it is. An unknown name or an unknown DCGM hostname
// label is an error that lists the valid values.
func NewSet(names []string, o Options) (*Set, error) {
	hostLabel := o.DCGMHostnameLabel
	if hostLabel == "" {
		hostLabel = dcgmLabelHostname
	}
	var errs []error
	if hostLabel != dcgmLabelHostname && hostLabel != dcgmLabelHostnameLegacy {
		errs = append(errs, fmt.Errorf("compat: DCGM hostname label %q is not %s or %s",
			hostLabel, dcgmLabelHostname, dcgmLabelHostnameLegacy))
	}
	s := &Set{
		exporters: map[string]exporter{},
		registry:  map[string]*prometheus.Registry{},
		handlers:  map[string]http.Handler{},
	}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || s.exporters[name] != nil {
			continue
		}
		var e exporter
		switch name {
		case Kepler07:
			e = newKepler07()
		case Kepler012:
			e = newKepler012()
		case DCGM484:
			e = newDCGM(hostLabel)
		case AMDDME152:
			e = newAMDDME()
		default:
			errs = append(errs, fmt.Errorf("compat: unknown exporter %q; the exporters are %s",
				name, strings.Join(Names(), ", ")))
			continue
		}
		// An exporter describes nothing, which makes it an unchecked
		// collector: its label sets follow the nodes of the snapshot.
		reg := prometheus.NewRegistry()
		if err := reg.Register(e); err != nil {
			errs = append(errs, fmt.Errorf("compat: %s: %w", name, err))
			continue
		}
		s.exporters[name] = e
		s.registry[name] = reg
		s.handlers[name] = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, name := range Names() {
		if s.exporters[name] != nil {
			s.enabled = append(s.enabled, name)
		}
	}
	return s, nil
}

// Enabled returns the enabled exporter names, in the order of Names.
func (s *Set) Enabled() []string {
	return append([]string(nil), s.enabled...)
}

// Observe computes every enabled exporter from snap. A nil snapshot is
// ignored. Snapshots are expected in time order; a node whose snapshot time
// did not advance keeps the series it has.
func (s *Set) Observe(snap *emulate.Snapshot) {
	if s == nil || snap == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.enabled {
		s.exporters[name].observe(snap)
	}
}

// Handler serves /exporters/<name>/metrics for each enabled exporter and
// answers 404 to any other path, a known but disabled exporter included. A
// nil Set answers 404 to every path.
func (s *Set) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, PathPrefix)
		if ok {
			name, ok = strings.CutSuffix(name, PathSuffix)
		}
		var h http.Handler
		if s != nil {
			h = s.handlers[name]
		}
		if !ok || h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// compatPoint is one series of a published view. Its label values are
// never modified once a view holds them, so successive views share them.
type compatPoint struct {
	desc   *prometheus.Desc
	kind   prometheus.ValueType
	value  float64
	labels []string
}

// compatPublished is the Collector half of an exporter: the series of the
// last observed snapshot, turned into metrics when a scrape asks, so a
// tick costs one slice and not a metric per series. Describe sends
// nothing.
type compatPublished struct {
	mu     sync.RWMutex
	points []compatPoint
}

func (p *compatPublished) Describe(chan<- *prometheus.Desc) {}

func (p *compatPublished) Collect(ch chan<- prometheus.Metric) {
	p.mu.RLock()
	points := p.points
	p.mu.RUnlock()
	for _, pt := range points {
		m, err := prometheus.NewConstMetric(pt.desc, pt.kind, pt.value, pt.labels...)
		if err != nil {
			m = prometheus.NewInvalidMetric(pt.desc, err)
		}
		ch <- m
	}
}

// publish replaces the served view. The slice is never written again.
func (p *compatPublished) publish(points []compatPoint) {
	p.mu.Lock()
	p.points = points
	p.mu.Unlock()
}

// compatLabelValues returns old when it holds exactly values, else a copy
// of values, so a series keeps one label slice while its labels stay the
// same.
func compatLabelValues(old []string, values ...string) []string {
	if len(old) == len(values) {
		same := true
		for i := range values {
			if old[i] != values[i] {
				same = false
				break
			}
		}
		if same {
			return old
		}
	}
	return append([]string(nil), values...)
}

// compatIsNVIDIA reports whether a GPU component is an NVIDIA device, by its
// vendor name or PCI vendor id.
func compatIsNVIDIA(c *emulate.ComponentReading) bool {
	v := strings.ToLower(c.Vendor)
	return strings.Contains(v, "nvidia") || v == "0x10de"
}

// compatGPUProduct is a GPU's product name, as NVML reports it.
func compatGPUProduct(c *emulate.ComponentReading) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Model
}

// compatHasPackageEnergy reports whether a collector on the node could read
// CPU package energy. Without it the Kepler exporters emit nothing for the
// node: Kepler v0.12.0 finds no RAPL zone and does not start, and the model
// estimates Kepler v0.7.12 would publish instead are not reproduced
// (follow-up S2-F5).
func compatHasPackageEnergy(n *emulate.NodeSnapshot) bool {
	for i := range n.Components {
		if c := &n.Components[i]; c.Type == semconv.HwTypeCPU && c.Measured {
			return true
		}
	}
	return false
}
