package main

// Metrics of the measured inputs: how each Prometheus query of the node
// power and facility inputs ended, and the facility readings the twin and
// the policy use. Each name equals the default Prometheus translation of an
// OpenTelemetry name (joulie.twin.input.queries is a counter of unit
// {query}, the facility gauges carry units Cel and W), so a dashboard reads
// them the same way whether Prometheus scrapes them or receives them over
// OTLP. The grid carbon intensity is in the semconv table, and its names come
// from it.

import (
	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
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

	// gridCarbonIntensity is semconv.JoulieGridCarbonIntensity, a cluster
	// metric: joulie_grid_carbon_intensity_grams_per_kW_h{joulie_grid_carbon_intensity_signal}.
	gridCarbonIntensity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: promname.Default.MetricName(semconv.JoulieGridCarbonIntensity),
		Help: "Grid carbon intensity the facility input last read (FACILITY_CARBON_INTENSITY_QUERY), in gCO2eq/kWh, by signal (marginal or average). Absent while unknown.",
	}, []string{promname.Default.LabelName(semconv.AttrJoulieGridCarbonIntensitySignal)})
)

// inputMetricCollectors returns every input metric, for registerMetrics.
func inputMetricCollectors() []prometheus.Collector {
	return []prometheus.Collector{twinInputQueries, twinFacilityAmbientTemp, twinFacilityITPower, twinFacilityCoolingPower, gridCarbonIntensity}
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

// setCarbonIntensityGauge exports c under its signal when known, and no
// series when not; a series of another signal never outlives the poll that
// replaced it.
func setCarbonIntensityGauge(c inputs.CarbonIntensity, known bool) {
	gridCarbonIntensity.Reset()
	if known {
		gridCarbonIntensity.WithLabelValues(string(c.Signal)).Set(c.GramsPerKWh)
	}
}
