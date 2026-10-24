package inputs

import (
	"fmt"
	"strings"
)

// Queries returns the query of every input the spec reads, keyed by input.
// The otel preset reads every input but the carbon intensity; legacy reads
// the outdoor ambient temperature, IT power and cooling power, the three
// inputs it had before; custom reads only the overridden ones. The carbon
// intensity has a query only as an override, under any preset, and then its
// CarbonIntensity spec must pass Check. Zero fields take their defaults:
// the otel preset, the default translation strategy and DefaultWindow.
//
// Naming spells the otel names and, under every preset, the key labels of
// the zone, rack and node inputs. An override replaces its input's query: a
// bare metric name is aggregated by the input's key label, avg for a
// temperature and the carbon intensity and sum for a power; any other query
// is kept as written and must return one series, or one series per key
// carrying the key label (Query.GroupLabel); an empty one is no override.
//
// The otel node inlet query reads the node from the series' label where
// Prometheus promotes k8s.node.name and from target_info where it does not,
// so it reads every node under each PROMETHEUS_RESOURCE_ATTRIBUTES mode.
func (s FacilitySpec) Queries() (map[FacilityInput]Query, error) {
	preset := s.Preset
	switch preset {
	case "":
		preset = FacilityPresetOTel
	case FacilityPresetOTel, FacilityPresetLegacy, FacilityPresetCustom:
	default:
		return nil, fmt.Errorf("inputs: unknown facility preset %q, want %s, %s or %s",
			preset, FacilityPresetOTel, FacilityPresetLegacy, FacilityPresetCustom)
	}
	naming, err := parseNaming(s.Naming)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	w := s.Window
	if w == 0 {
		w = DefaultWindow
	}
	window, err := PromDuration(w)
	if err != nil {
		return nil, err
	}
	n := namesFor(naming)

	out := map[FacilityInput]Query{}
	switch preset {
	case FacilityPresetOTel:
		out[FacilityInputAmbientTemp] = Query{PromQL: wrapAggregation("avg", "", selector(n.facilityAmbient))}
		out[FacilityInputITPower] = Query{PromQL: energyOrPower("", n.facilityITEnergy, n.facilityITPower, window)}
		out[FacilityInputCoolingPower] = Query{PromQL: energyOrPower("", n.facilityCoolingEnergy, n.facilityCoolingPower, window)}
		out[FacilityInputZoneAmbientTemp] = Query{PromQL: wrapAggregation("avg", n.zone, selector(n.zoneAmbient)), GroupLabel: n.zone}
		out[FacilityInputRackPower] = Query{PromQL: energyOrPower(n.rack, n.rackEnergy, n.rackPower, window), GroupLabel: n.rack}
		// Two producers may report one node, and a node may have several
		// inlet sensors: the hottest one counts. The node is a resource
		// attribute and FacilitySpec has no resource attributes mode, so
		// the one query reads the node label where Prometheus promotes it
		// and target_info elsewhere.
		out[FacilityInputNodeInletTemp] = Query{PromQL: wrapAggregation("max", n.node, labelledOrJoined(n.hostAmbient, n.node)), GroupLabel: n.node}
	case FacilityPresetLegacy:
		out[FacilityInputAmbientTemp] = Query{PromQL: wrapAggregation("avg", "", legacyAmbientTemp)}
		out[FacilityInputITPower] = Query{PromQL: wrapAggregation("sum", "", legacyITPower)}
		out[FacilityInputCoolingPower] = Query{PromQL: wrapAggregation("sum", "", legacyCoolingPower)}
	}

	for input, q := range s.Overrides {
		if strings.TrimSpace(q) == "" {
			continue
		}
		key, op, ok := facilityInputShape(input, n)
		if !ok {
			return nil, fmt.Errorf("inputs: unknown facility input %q", input)
		}
		if input == FacilityInputCarbonIntensity {
			if err := s.CarbonIntensity.Check(); err != nil {
				return nil, fmt.Errorf("inputs: %w", err)
			}
		}
		if isBareMetricName(q) {
			q = wrapAggregation(op, key, q)
		}
		out[input] = Query{PromQL: q, GroupLabel: key}
	}
	return out, nil
}

// facilityInputShape returns the key label of input (empty for a single
// value) and the aggregation of a bare metric name: avg for a temperature,
// which several sensors report, and for the carbon intensity, which several
// replicas of an exporter may report; sum for a power, which adds up.
func facilityInputShape(input FacilityInput, n otelNames) (key, op string, ok bool) {
	switch input {
	case FacilityInputAmbientTemp, FacilityInputCarbonIntensity:
		return "", "avg", true
	case FacilityInputITPower, FacilityInputCoolingPower:
		return "", "sum", true
	case FacilityInputZoneAmbientTemp:
		return n.zone, "avg", true
	case FacilityInputRackPower:
		return n.rack, "sum", true
	case FacilityInputNodeInletTemp:
		return n.node, "avg", true
	}
	return "", "", false
}

// energyOrPower sums the rate of the energy counter by key, or the power
// gauge for a key without a counter.
func energyOrPower(key, energy, power, window string) string {
	e := wrapAggregation("sum", key, rate(selector(energy), window))
	p := wrapAggregation("sum", key, selector(power))
	if key == "" {
		return e + " or " + p
	}
	return e + " or on " + labelList(key) + " " + p
}
