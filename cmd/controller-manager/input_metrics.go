package main

// Metrics of the measured inputs: how each Prometheus query of the node
// power and facility inputs ended, and the facility readings the twin uses.
// Each name equals the default Prometheus translation of an OpenTelemetry
// name (joulie.twin.input.queries is a counter of unit {query}, the facility
// gauges carry units Cel and W), so a dashboard reads them the same way
// whether Prometheus scrapes them or receives them over OTLP.

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Values of the result label of joulie_twin_input_queries_total.
const (
	// inputResultSuccess: every series was read.
	inputResultSuccess = "success"
	// inputResultPartial: some series were skipped, for a missing key
	// label or a value that is not finite.
	inputResultPartial = "partial"
	// inputResultNoData: the query returned no usable series.
	inputResultNoData = "no_data"
	// inputResultError: Prometheus failed the query, could not be reached,
	// or returned a result the reader refuses (duplicate keys, several
	// series for a single value).
	inputResultError = "error"
)

// inputNodePower is the input label value of the node power queries. The
// facility inputs use their inputs.FacilityInput value.
const inputNodePower = "node_power"

var (
	twinInputQueries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "joulie_twin_input_queries_total",
		Help: "Prometheus queries of the controller manager's measured inputs, by input (node_power or a facility input) and result (success, partial, no_data, error).",
	}, []string{"input", "result"})

	twinFacilityAmbientTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joulie_twin_facility_ambient_temperature_celsius",
		Help: "Outdoor ambient temperature the facility input last read, in degrees Celsius, by FACILITY_PRESET. Absent while unknown.",
	}, []string{"preset"})
	twinFacilityITPower = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joulie_twin_facility_it_power_watts",
		Help: "IT power the facility input last read, in watts, by FACILITY_PRESET. Absent while unknown.",
	}, []string{"preset"})
	twinFacilityCoolingPower = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joulie_twin_facility_cooling_power_watts",
		Help: "Cooling power the facility input last read, in watts, by FACILITY_PRESET. Absent while unknown.",
	}, []string{"preset"})
)

// inputMetricCollectors returns every input metric, for registerMetrics.
func inputMetricCollectors() []prometheus.Collector {
	return []prometheus.Collector{twinInputQueries, twinFacilityAmbientTemp, twinFacilityITPower, twinFacilityCoolingPower}
}

// recordInputQuery counts one query of input that ended in result.
func recordInputQuery(input, result string) {
	twinInputQueries.WithLabelValues(input, result).Inc()
}

// setFacilityGauge sets g for preset to v when known, and removes the series
// when not, so a stale reading never outlives the poll that lost it.
func setFacilityGauge(g *prometheus.GaugeVec, preset string, v float64, known bool) {
	if known {
		g.WithLabelValues(preset).Set(v)
		return
	}
	g.DeleteLabelValues(preset)
}
