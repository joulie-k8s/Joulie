package emulate

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// emulatorMetrics are the emulator's own Prometheus metrics. They are
// registered with Config.Registerer when it is set, and unregistered by
// Close, so a second Emulator can register them again.
type emulatorMetrics struct {
	reg          prometheus.Registerer
	nodes        prometheus.Gauge
	tickDuration prometheus.Histogram
	overruns     prometheus.Counter
	stepErrors   *prometheus.CounterVec
}

func newEmulatorMetrics(reg prometheus.Registerer, nodes []string) (*emulatorMetrics, error) {
	m := &emulatorMetrics{
		reg: reg,
		nodes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "joulie_sim_emulate_nodes",
			Help: "Emulated nodes the simulator steps on each tick",
		}),
		tickDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "joulie_sim_emulate_tick_duration_seconds",
			Help:    "Time one emulator tick takes to step every emulated node and publish the snapshot",
			Buckets: prometheus.DefBuckets,
		}),
		overruns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "joulie_sim_emulate_tick_overruns_total",
			Help: "Emulator ticks that took longer than the tick interval",
		}),
		stepErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_emulate_node_step_errors_total",
			Help: "Emulated node steps that failed, by node",
		}, []string{"node"}),
	}
	m.nodes.Set(float64(len(nodes)))
	// Every node's series exists from the start, so rate() sees the first
	// error as an increase from 0.
	for _, n := range nodes {
		m.stepErrors.WithLabelValues(n)
	}
	if reg == nil {
		return m, nil
	}
	var registered []prometheus.Collector
	for _, c := range m.collectors() {
		if err := reg.Register(c); err != nil {
			for _, r := range registered {
				reg.Unregister(r)
			}
			return nil, fmt.Errorf("emulate: register metrics: %w", err)
		}
		registered = append(registered, c)
	}
	return m, nil
}

func (m *emulatorMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.nodes, m.tickDuration, m.overruns, m.stepErrors}
}

func (m *emulatorMetrics) unregister() {
	if m.reg == nil {
		return
	}
	for _, c := range m.collectors() {
		m.reg.Unregister(c)
	}
	m.reg = nil
}
