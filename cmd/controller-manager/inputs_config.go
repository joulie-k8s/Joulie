package main

// The configuration of the measured inputs: node power and the facility
// metrics. loadInputConfig reads and checks every variable once at start, so
// a value no preset serves stops the controller manager with a message
// instead of reading nothing for ever.

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
)

// Values of NODE_POWER_SOURCE.
const (
	nodePowerSourceStatic     = "static"
	nodePowerSourcePrometheus = "prometheus"
	nodePowerSourceHTTP       = "http"
)

// inputConfig is the measured-input configuration.
type inputConfig struct {
	// prometheusAddress is PROMETHEUS_ADDRESS, the default of both
	// Prometheus addresses below.
	prometheusAddress string
	nodePower         nodePowerConfig
	facility          facilityConfig
}

// nodePowerConfig is the node power input.
type nodePowerConfig struct {
	source       string // static, prometheus or http
	httpEndpoint string
	promAddress  string
	promQuery    string
	// spec has its Preset resolved and no CustomQuery: promQuery is the
	// query of the custom preset.
	spec inputs.NodePowerSpec
}

// query returns the node power query and, under auto, its target_info form.
func (c nodePowerConfig) query() (primary, fallback inputs.Query, err error) {
	spec := c.spec
	spec.CustomQuery = c.promQuery
	return spec.Query()
}

// loadInputConfig reads the input variables. An invalid value is an error,
// whatever the source, except an unknown NODE_POWER_SOURCE, which means
// static with a warning as it always did.
//
// Shared by both inputs: PROMETHEUS_ADDRESS, PROMETHEUS_NAME_TRANSLATION
// (the receiver's translation strategy, default UnderscoreEscapingWithSuffixes),
// PROMETHEUS_RATE_WINDOW (default 5m, at least 10s).
//
// Node power: NODE_POWER_SOURCE, NODE_POWER_PRESET (otel, kepler,
// kepler-legacy, custom; unset is otel, or custom when a query is set),
// NODE_POWER_SCOPE (components, host), NODE_POWER_NODE_LABEL,
// PROMETHEUS_RESOURCE_ATTRIBUTES (auto, labels, target_info),
// NODE_POWER_PROMETHEUS_ADDRESS, NODE_POWER_PROMETHEUS_QUERY,
// NODE_POWER_HTTP_ENDPOINT.
//
// Facility: ENABLE_FACILITY_METRICS, FACILITY_PROMETHEUS_ADDRESS,
// FACILITY_POLL_INTERVAL, FACILITY_PRESET (otel, legacy, custom) and
// FACILITY_<INPUT>_QUERY for each input of inputs.FacilityInputs.
func loadInputConfig() (inputConfig, error) {
	var cfg inputConfig
	cfg.prometheusAddress = envOrDefault("PROMETHEUS_ADDRESS", "")

	naming := promname.Default
	if v := envOrDefault("PROMETHEUS_NAME_TRANSLATION", ""); v != "" {
		s, err := promname.ParseStrategy(v)
		if err != nil {
			return cfg, fmt.Errorf("PROMETHEUS_NAME_TRANSLATION: %w", err)
		}
		naming = s
	}
	window := inputs.DefaultWindow
	if v := envOrDefault("PROMETHEUS_RATE_WINDOW", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("PROMETHEUS_RATE_WINDOW: %w", err)
		}
		window = d
	}
	if _, err := inputs.PromDuration(window); err != nil {
		return cfg, fmt.Errorf("PROMETHEUS_RATE_WINDOW: %w", err)
	}

	np := &cfg.nodePower
	np.source = strings.ToLower(envOrDeprecated("NODE_POWER_SOURCE", nodePowerSourceStatic))
	switch np.source {
	case nodePowerSourceStatic, nodePowerSourcePrometheus, nodePowerSourceHTTP:
	default:
		log.Printf("warning: NODE_POWER_SOURCE=%q is not %s, %s or %s; reading no measured power (%s)",
			np.source, nodePowerSourceStatic, nodePowerSourcePrometheus, nodePowerSourceHTTP, nodePowerSourceStatic)
		np.source = nodePowerSourceStatic
	}
	np.httpEndpoint = envOrDeprecated("NODE_POWER_HTTP_ENDPOINT", "")
	np.promAddress = envOrDeprecated("NODE_POWER_PROMETHEUS_ADDRESS", cfg.prometheusAddress)
	np.promQuery = envOrDeprecated("NODE_POWER_PROMETHEUS_QUERY", "")
	preset := inputs.NodePowerPreset(strings.ToLower(envOrDefault("NODE_POWER_PRESET", "")))
	if preset == "" {
		preset = inputs.NodePowerPresetOTel
		if np.promQuery != "" {
			preset = inputs.NodePowerPresetCustom
		}
	}
	np.spec = inputs.NodePowerSpec{
		Preset:             preset,
		Scope:              inputs.Scope(strings.ToLower(envOrDefault("NODE_POWER_SCOPE", string(inputs.ScopeComponents)))),
		Naming:             naming,
		ResourceAttributes: inputs.ResourceAttributes(strings.ToLower(envOrDefault("PROMETHEUS_RESOURCE_ATTRIBUTES", string(inputs.ResourceAttributesAuto)))),
		Window:             window,
		NodeLabel:          envOrDefault("NODE_POWER_NODE_LABEL", ""),
	}
	if _, _, err := np.query(); err != nil {
		return cfg, fmt.Errorf("node power input (NODE_POWER_PRESET, NODE_POWER_SCOPE, PROMETHEUS_RESOURCE_ATTRIBUTES, NODE_POWER_PROMETHEUS_QUERY): %w", err)
	}

	fc := defaultFacilityConfig()
	fc.enabled = boolEnv("ENABLE_FACILITY_METRICS", false)
	fc.prometheusAddress = envOrDefault("FACILITY_PROMETHEUS_ADDRESS", firstNonEmpty(cfg.prometheusAddress, defaultFacilityPrometheusAddress))
	fc.pollInterval = durationEnv("FACILITY_POLL_INTERVAL", fc.pollInterval)
	if fc.pollInterval <= 0 {
		return cfg, fmt.Errorf("FACILITY_POLL_INTERVAL: %s is not a positive duration", fc.pollInterval)
	}
	fpreset := inputs.FacilityPreset(strings.ToLower(envOrDefault("FACILITY_PRESET", string(inputs.FacilityPresetOTel))))
	if fpreset == inputs.FacilityPresetLegacy {
		warnDeprecatedOnce("FACILITY_PRESET=legacy", fmt.Sprintf(
			"FACILITY_PRESET=legacy (the datacenter_* metrics) is deprecated and will be removed in %s; publish the facility metrics under their OpenTelemetry names and use FACILITY_PRESET=otel, or set FACILITY_*_QUERY",
			inputAliasesRemovedIn))
	}
	overrides := map[inputs.FacilityInput]string{}
	for _, input := range inputs.FacilityInputs() {
		if v := envOrDeprecated(facilityQueryEnv(input), ""); v != "" {
			overrides[input] = v
		}
	}
	fc.spec = inputs.FacilitySpec{Preset: fpreset, Naming: naming, Window: window, Overrides: overrides}
	if _, err := fc.spec.Queries(); err != nil {
		return cfg, fmt.Errorf("facility input (FACILITY_PRESET, FACILITY_*_QUERY): %w", err)
	}
	cfg.facility = fc
	return cfg, nil
}

// facilityQueryEnv is the variable that overrides the query of input:
// FACILITY_IT_POWER_QUERY for inputs.FacilityInputITPower.
func facilityQueryEnv(input inputs.FacilityInput) string {
	return "FACILITY_" + strings.ToUpper(string(input)) + "_QUERY"
}

// install sets the variables resolveNodePower reads.
func (c inputConfig) install() {
	nodePowerSource = c.nodePower.source
	nodePowerHTTPEndpoint = c.nodePower.httpEndpoint
	nodePowerPromAddress = c.nodePower.promAddress
	nodePowerPromQuery = c.nodePower.promQuery
	nodePowerSpec = c.nodePower.spec
}

// logSummary logs one line per input: where it reads from and the query it
// runs.
func (c inputConfig) logSummary() {
	np := c.nodePower
	switch np.source {
	case nodePowerSourceHTTP:
		if np.httpEndpoint == "" {
			log.Printf("input node_power: source=http without NODE_POWER_HTTP_ENDPOINT; no measured power")
		} else {
			log.Printf("input node_power: source=http endpoint=%s", np.httpEndpoint)
		}
	case nodePowerSourcePrometheus:
		primary, fallback, _ := np.query()
		line := fmt.Sprintf("input node_power: source=prometheus address=%s preset=%s scope=%s naming=%s resource_attributes=%s window=%s query=%q",
			np.promAddress, np.spec.Preset, np.spec.Scope, np.spec.Naming, np.spec.ResourceAttributes, np.spec.Window, primary.PromQL)
		if fallback.PromQL != "" {
			line += fmt.Sprintf(" target_info_query=%q", fallback.PromQL)
		}
		if primary.PerNode {
			line += " per_node=true"
		}
		if np.promAddress == "" {
			line += "; no address (set PROMETHEUS_ADDRESS or NODE_POWER_PROMETHEUS_ADDRESS), so no measured power"
		}
		log.Print(line)
	default:
		log.Printf("input node_power: source=static (no measured power)")
	}

	fc := c.facility
	if !fc.enabled {
		log.Printf("input facility: disabled (ENABLE_FACILITY_METRICS=false)")
		return
	}
	queries, _ := fc.spec.Queries()
	if len(queries) == 0 {
		log.Printf("input facility: preset=%s reads nothing; set FACILITY_*_QUERY", facilityPresetName(fc.spec))
		return
	}
	for _, input := range inputs.FacilityInputs() {
		if q, ok := queries[input]; ok {
			log.Printf("input facility %s: address=%s preset=%s interval=%s query=%q",
				input, fc.prometheusAddress, facilityPresetName(fc.spec), fc.pollInterval, q.PromQL)
		}
	}
}
