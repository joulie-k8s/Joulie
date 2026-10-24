package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/controller/policy"
)

// facilitySnapshot is one poll's facility readings: the outdoor ambient
// temperature, IT and cooling power, the grid carbon intensity, and the
// ambient temperature of each cooling zone, the power of each rack and the
// inlet temperature of each node. A reading the poll could not get is
// unknown (a false flag, or no map entry), never the value of an earlier
// poll.
type facilitySnapshot struct {
	ambientC      float64
	ambientKnown  bool
	itPowerW      float64
	itKnown       bool
	coolingPowerW float64
	coolingKnown  bool
	carbon        inputs.CarbonIntensity // gCO2eq/kWh, with its signal
	carbonKnown   bool
	zoneAmbientC  map[string]float64 // joulie.io/cooling-zone value -> ambient temp
	rackPowerW    map[string]float64 // joulie.io/rack value -> measured power draw
	nodeInletC    map[string]float64 // node name -> inlet temp
}

// forPolicy is the snapshot as the policy reads it. The maps are copies, so
// a policy never shares them with the twins of its reconcile.
func (s facilitySnapshot) forPolicy() policy.Facility {
	c := s.clone()
	return policy.Facility{
		CarbonIntensity: c.carbon, CarbonIntensityKnown: c.carbonKnown,
		AmbientC: c.ambientC, AmbientKnown: c.ambientKnown,
		ITPowerW: c.itPowerW, ITPowerKnown: c.itKnown,
		CoolingPowerW: c.coolingPowerW, CoolingPowerKnown: c.coolingKnown,
		ZoneAmbientC: c.zoneAmbientC, RackPowerW: c.rackPowerW, NodeInletC: c.nodeInletC,
	}
}

// clone copies the maps, so a reconcile never shares them with the poller.
func (s facilitySnapshot) clone() facilitySnapshot {
	s.zoneAmbientC = cloneFloatMap(s.zoneAmbientC)
	s.rackPowerW = cloneFloatMap(s.rackPowerW)
	s.nodeInletC = cloneFloatMap(s.nodeInletC)
	return s
}

func cloneFloatMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// facilityMetrics holds the readings of the last facility poll. They feed
// the twin: the ambient temperatures its cooling stress, the IT and rack
// power its PSU stress. The policy gets all of them, the carbon intensity
// included (facilitySnapshot.forPolicy).
type facilityMetrics struct {
	mu          sync.RWMutex
	readings    facilitySnapshot
	lastUpdated time.Time
}

// store replaces every reading with those of one poll.
func (f *facilityMetrics) store(s facilitySnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings = s.clone()
	f.lastUpdated = time.Now()
}

// snapshot returns a copy of the readings.
func (f *facilityMetrics) snapshot() facilitySnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.readings.clone()
}

// twinFacility is the facility snapshot the policy and the twin read during
// a reconcile. reconcileWithCatalogAndFacility stores it before every
// reconcile.
var (
	twinFacilityMu sync.RWMutex
	twinFacility   facilitySnapshot
)

func setTwinFacility(s facilitySnapshot) {
	twinFacilityMu.Lock()
	defer twinFacilityMu.Unlock()
	twinFacility = s
}

func currentTwinFacility() facilitySnapshot {
	twinFacilityMu.RLock()
	defer twinFacilityMu.RUnlock()
	return twinFacility
}

// facilityConfig is the facility input: ENABLE_FACILITY_METRICS,
// FACILITY_PROMETHEUS_ADDRESS, FACILITY_POLL_INTERVAL, the queries of
// FACILITY_PRESET with the FACILITY_*_QUERY overrides, and the unit and
// signal of the carbon intensity query (spec.CarbonIntensity).
type facilityConfig struct {
	enabled           bool
	prometheusAddress string
	pollInterval      time.Duration
	spec              inputs.FacilitySpec
}

// defaultFacilityConfig is the configuration with no variable set.
func defaultFacilityConfig() facilityConfig {
	return facilityConfig{
		enabled:           false,
		prometheusAddress: defaultFacilityPrometheusAddress,
		pollInterval:      30 * time.Second,
		spec:              inputs.FacilitySpec{Preset: inputs.FacilityPresetOTel, Window: inputs.DefaultWindow},
	}
}

// defaultFacilityPrometheusAddress is FACILITY_PROMETHEUS_ADDRESS when
// neither it nor PROMETHEUS_ADDRESS is set.
const defaultFacilityPrometheusAddress = "http://prometheus-operated.monitoring:9090"

// facilityMetricsLoop polls Prometheus for data-center-level metrics: once
// at start, so the first reconciles already see them, then every
// pollInterval.
//
// These metrics enable the twin to compute PUE from real facility data
// rather than heuristic estimates. The scheduler then uses PUE to weight
// marginal power costs: a pod on a node with PUE 1.8 costs more than
// one with PUE 1.2.
//
// The default preset, otel, reads Joulie's facility, cooling zone and rack
// metrics and the host ambient temperature (package inputs builds the
// queries); FACILITY_*_QUERY overrides one input.
//
// PUE = (IT power + cooling power) / IT power
//
// References:
//   - Dayarathna et al. (2016). "Data Center Energy Consumption Modeling: A Survey"
//   - Kaup et al. (2014). "Measuring and Modeling the Power Consumption of OpenFlow Switches"
//   - The Green Grid. "PUE: A Comprehensive Examination of the Metric"
func facilityMetricsLoop(ctx context.Context, fm *facilityMetrics, cfg facilityConfig) {
	if !cfg.enabled {
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	log.Printf("[facility] started: interval=%s prometheus=%s preset=%s", cfg.pollInterval, cfg.prometheusAddress, facilityPresetName(cfg.spec))
	fetchFacilityMetrics(ctx, client, fm, cfg)

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchFacilityMetrics(ctx, client, fm, cfg)
		}
	}
}

// facilityPresetName is the preset label of the facility gauges.
func facilityPresetName(spec inputs.FacilitySpec) string {
	if spec.Preset == "" {
		return string(inputs.FacilityPresetOTel)
	}
	return string(spec.Preset)
}

// fetchFacilityMetrics runs one grouped query per facility input and stores
// what they read in fm, replacing the previous poll. An input whose query
// fails is unknown until a later poll reads it.
func fetchFacilityMetrics(ctx context.Context, client *http.Client, fm *facilityMetrics, cfg facilityConfig) {
	queries, err := cfg.spec.Queries()
	if err != nil {
		log.Printf("[facility] %v", err)
		return
	}
	preset := facilityPresetName(cfg.spec)
	c := inputs.Client{Address: cfg.prometheusAddress, HTTP: client}

	var s facilitySnapshot
	for _, input := range inputs.FacilityInputs() {
		q, ok := queries[input]
		if !ok {
			continue
		}
		if q.GroupLabel == "" {
			v, err := queryPrometheusScalar(ctx, client, cfg.prometheusAddress, q.PromQL)
			if err == nil && input == inputs.FacilityInputCarbonIntensity {
				s.carbon, err = cfg.spec.CarbonIntensity.Read(v)
			}
			known, result := err == nil, facilitySingleResult(err)
			recordInputQuery(string(input), result)
			noteFacilityResult(input, preset, q, result, err)
			switch input {
			case inputs.FacilityInputAmbientTemp:
				s.ambientC, s.ambientKnown = v, known
			case inputs.FacilityInputITPower:
				s.itPowerW, s.itKnown = v, known
			case inputs.FacilityInputCoolingPower:
				s.coolingPowerW, s.coolingKnown = v, known
			case inputs.FacilityInputCarbonIntensity:
				s.carbonKnown = known
			default:
				// An override of a keyed input without its key label lands
				// here; Queries always gives keyed inputs a GroupLabel.
			}
			continue
		}
		res, err := c.Query(ctx, q.PromQL)
		m, result, err := readFacilityKeyed(res, err, q.GroupLabel)
		recordInputQuery(string(input), result)
		noteFacilityResult(input, preset, q, result, err)
		switch input {
		case inputs.FacilityInputZoneAmbientTemp:
			s.zoneAmbientC = m
		case inputs.FacilityInputRackPower:
			s.rackPowerW = m
		case inputs.FacilityInputNodeInletTemp:
			s.nodeInletC = m
		}
	}

	fm.store(s)
	setFacilityGauge(twinFacilityAmbientTemp, preset, s.ambientC, s.ambientKnown)
	setFacilityGauge(twinFacilityITPower, preset, s.itPowerW, s.itKnown)
	setFacilityGauge(twinFacilityCoolingPower, preset, s.coolingPowerW, s.coolingKnown)
	setCarbonIntensityGauge(s.carbon, s.carbonKnown)

	if s.itKnown && s.coolingKnown && s.itPowerW > 0 {
		pue := (s.itPowerW + s.coolingPowerW) / s.itPowerW
		log.Printf("[facility] ambient=%.1fC itPower=%.0fW cooling=%.0fW pue=%.3f",
			s.ambientC, s.itPowerW, s.coolingPowerW, pue)
	}
	if len(s.zoneAmbientC) > 0 || len(s.rackPowerW) > 0 || len(s.nodeInletC) > 0 {
		log.Printf("[facility] topology: zones=%d racks=%d inlets=%d", len(s.zoneAmbientC), len(s.rackPowerW), len(s.nodeInletC))
	}
}

// facilitySingleResult is the result label of a single-value read by
// queryPrometheusScalar.
func facilitySingleResult(err error) string {
	switch {
	case err == nil:
		return inputResultSuccess
	case errors.Is(err, inputs.ErrNoData):
		return inputResultNoData
	}
	return inputResultError
}

// readFacilityKeyed reads a keyed input: one series per key label value.
func readFacilityKeyed(res inputs.Result, err error, label string) (map[string]float64, string, error) {
	if err != nil {
		return nil, inputResultError, err
	}
	m, iss, err := res.ByLabel(label)
	switch {
	case err != nil:
		return nil, inputResultError, err
	case len(m) == 0:
		if iss.Series > 0 {
			return nil, inputResultNoData, fmt.Errorf("%d series, none with a finite value and the label %q", iss.Series, label)
		}
		return nil, inputResultNoData, inputs.ErrNoData
	case iss.MissingLabel > 0 || iss.NonFinite > 0:
		return m, inputResultPartial, fmt.Errorf("skipped %d series without the label %q and %d not finite", iss.MissingLabel, label, iss.NonFinite)
	}
	return m, inputResultSuccess, nil
}

var (
	facilityResultMu   sync.Mutex
	facilityLastResult = map[inputs.FacilityInput]string{}
)

// noteFacilityResult logs an input whose result changed since the previous
// poll, so a facility without zone sensors logs that once and not every 30
// seconds. The first poll counts as following a success.
func noteFacilityResult(input inputs.FacilityInput, preset string, q inputs.Query, result string, err error) {
	facilityResultMu.Lock()
	prev, seen := facilityLastResult[input]
	facilityLastResult[input] = result
	facilityResultMu.Unlock()
	if !seen {
		prev = inputResultSuccess
	}
	if result == prev {
		return
	}
	if result == inputResultSuccess {
		log.Printf("[facility] %s: reading again", input)
		return
	}
	log.Printf("[facility] %s: %s from %q: %v%s", input, result, q.PromQL, err, facilityHint(input, preset, err))
}

// facilityHint is a hint for a failed facility query, naming the variable
// that overrides it, and, when the otel preset found nothing for an input
// the legacy preset also reads, FACILITY_PRESET=legacy.
func facilityHint(input inputs.FacilityInput, preset string, err error) string {
	env := facilityQueryEnv(input)
	if input == inputs.FacilityInputCarbonIntensity {
		// No preset reads it: the query is the operator's own.
		return fmt.Sprintf(" (%s sets the query; it must return one series, in the unit of FACILITY_CARBON_INTENSITY_UNIT)", env)
	}
	if errors.Is(err, inputs.ErrNoData) && preset == string(inputs.FacilityPresetOTel) {
		legacy := ""
		if q, _ := (inputs.FacilitySpec{Preset: inputs.FacilityPresetLegacy}).Queries(); q[input].PromQL != "" {
			legacy = fmt.Sprintf("; a Prometheus that holds the datacenter_* metrics is read with FACILITY_PRESET=%s", inputs.FacilityPresetLegacy)
		}
		return fmt.Sprintf(" (producers that push OTLP need Prometheus' --web.enable-otlp-receiver, and PROMETHEUS_NAME_TRANSLATION must equal its otlp.translation_strategy; %s overrides the query%s)", env, legacy)
	}
	return fmt.Sprintf(" (%s overrides the query)", env)
}

// queryPrometheusScalar runs an instant query that must return exactly one
// series with a finite value, or a scalar. No series is inputs.ErrNoData;
// several series, a value that is not finite, or a failed query (an
// *inputs.Error carrying Prometheus' errorType and message) are errors.
// fetchFacilityMetrics reads every single-value input through it.
func queryPrometheusScalar(ctx context.Context, client *http.Client, address, query string) (float64, error) {
	res, err := inputs.Client{Address: address, HTTP: client}.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	return res.Single()
}
