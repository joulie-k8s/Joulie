package inputs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The Kepler names the kepler and kepler-legacy presets read. The
// simulator's compatibility exporters (simulator/pkg/compat) write the same
// constants, so a reader and its writer cannot drift apart.
//
// Kepler 0.10 and later label node series with node_name and RAPL zones with
// zone. A zone is named after the RAPL name file with a trailing socket index
// stripped, so package-0 and package-1 are one zone "package" summed over the
// sockets, while a die zone such as package-0-die-1 keeps its name and is a
// zone of its own (prometheus/procfs sysfs/class_powercap.go,
// getIndexAndName). KeplerPackageZones matches both. kepler_node_cpu_joules_total
// is the RAPL counter as the kernel shows it and wraps at
// max_energy_range_uj; the active and idle counters accumulate Kepler's own
// wrap-corrected increments and never wrap, so the kepler preset reads those.
//
// Kepler 0.7 to 0.9 label node series with instance, which Prometheus
// renames exported_instance when it scrapes with honor_labels false. Both
// name the GPU counter the same.
const (
	KeplerCPUJoules       = "kepler_node_cpu_joules_total"
	KeplerCPUActiveJoules = "kepler_node_cpu_active_joules_total"
	KeplerCPUIdleJoules   = "kepler_node_cpu_idle_joules_total"
	KeplerCPUWatts        = "kepler_node_cpu_watts"
	KeplerGPUJoules       = "kepler_node_gpu_joules_total"
	KeplerNodeLabel       = "node_name"
	KeplerZoneLabel       = "zone"
	KeplerPackageZones    = `package(-[0-9]+-die-[0-9]+)?`

	KeplerLegacyPackageJoules  = "kepler_node_package_joules_total"
	KeplerLegacyPlatformJoules = "kepler_node_platform_joules_total"
	KeplerLegacyNodeLabel      = "exported_instance"
)

// The names the legacy facility preset reads, the datacenter_* metrics the
// controller manager read before the otel facility preset existed.
const (
	legacyAmbientTemp  = "datacenter_ambient_temperature_celsius"
	legacyITPower      = "datacenter_total_it_power_watts"
	legacyCoolingPower = "datacenter_cooling_power_watts"
)

// componentLabel is the label the Kepler presets add to each branch of an
// "or" with label_replace. Kepler's CPU and GPU series of a node can carry
// equal label sets once rate() drops the metric name, and "or" then keeps
// only the left one.
const componentLabel = "joulie_component"

var (
	legacyLabelRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	legacyMetricRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
)

// isBareMetricName reports whether q is a metric name alone, the form of
// the FACILITY_*_METRIC variables.
func isBareMetricName(q string) bool { return legacyMetricRE.MatchString(q) }

// labelName writes a label name as PromQL needs it in by, on, group_left and
// a matcher: as it is when it is a legacy name, quoted otherwise, as the
// UTF-8 translation strategies store "k8s.node.name".
func labelName(name string) string {
	if legacyLabelRE.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}

// labelList writes the parenthesised label list of by, on and group_left.
func labelList(names ...string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = labelName(n)
	}
	return "(" + strings.Join(out, ", ") + ")"
}

// matcher is one label matcher of a selector, such as hw_type=~"cpu|gpu".
type matcher struct {
	label, op, value string
}

// selector writes the instant vector selector of metric with ms. A metric
// name outside the legacy character set goes inside the braces, quoted, and
// so does a label name: {"a.b_total", "c.d"=~"x|y"}.
func selector(metric string, ms ...matcher) string {
	parts := make([]string, 0, len(ms)+1)
	quoted := !legacyMetricRE.MatchString(metric)
	if quoted {
		parts = append(parts, strconv.Quote(metric))
	}
	for _, m := range ms {
		parts = append(parts, labelName(m.label)+m.op+strconv.Quote(m.value))
	}
	if !quoted {
		if len(parts) == 0 {
			return metric
		}
		return metric + "{" + strings.Join(parts, ", ") + "}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// rate writes rate(sel[window]).
func rate(sel, window string) string {
	return fmt.Sprintf("rate(%s[%s])", sel, window)
}

// joinTargetInfo attaches nodeLabel from target_info to every series of x,
// matching on job and instance, which the receiver derives from
// service.namespace, service.name and service.instance.id. The max by
// collapses target_info series of one target that differ in another
// attribute, as they do for a while after a producer restarts with a new
// service.version; a plain join fails on them as many-to-many. The "and"
// keeps only the target_info series of targets x has: Prometheus checks
// every right-hand series for duplicates, so another producer in the same
// Prometheus whose job and instance span two nodes (an application without
// service.instance.id, a pod rescheduled within the lookback) would
// otherwise fail the join for every node. x is an instant vector selector
// or a rate of one.
func joinTargetInfo(x, nodeLabel string) string {
	on := labelList(promname.LabelJob, promname.LabelInstance)
	return fmt.Sprintf("%s * on %s group_left %s max by %s (%s and on %s %s)",
		x, on, labelList(nodeLabel),
		labelList(promname.LabelJob, promname.LabelInstance, nodeLabel), promname.TargetInfo, on, x)
}

// labelledOrJoined selects metric for every node: as stored where the
// series carries nodeLabel, which Prometheus adds when it promotes
// k8s.node.name, and joined with target_info for it where the series does
// not. The two selectors are disjoint, so each series counts once under
// either receiver setting, and only series without the label reach the
// join, which fails when two nodes share one job and instance of the
// metric.
func labelledOrJoined(metric, nodeLabel string) string {
	return selector(metric, matcher{nodeLabel, "!=", ""}) + " or " +
		joinTargetInfo(selector(metric, matcher{nodeLabel, "=", ""}), nodeLabel)
}

// otelNames is the Prometheus spelling of the names the otel presets read,
// under one translation strategy.
type otelNames struct {
	node, hwType, hwID, zone, rack string

	energy, power, hostEnergy, hostPower, hostAmbient string

	facilityAmbient, facilityITEnergy, facilityITPower string
	facilityCoolingEnergy, facilityCoolingPower        string
	zoneAmbient, rackEnergy, rackPower                 string
}

func namesFor(s promname.Strategy) otelNames {
	return otelNames{
		node:   s.LabelName(semconv.AttrK8sNodeName),
		hwType: s.LabelName(semconv.AttrHwType),
		hwID:   s.LabelName(semconv.AttrHwID),
		zone:   s.LabelName(semconv.AttrJoulieCoolingZoneName),
		rack:   s.LabelName(semconv.AttrJoulieRackName),

		energy:      s.MetricName(semconv.HwEnergy),
		power:       s.MetricName(semconv.HwPower),
		hostEnergy:  s.MetricName(semconv.HwHostEnergy),
		hostPower:   s.MetricName(semconv.HwHostPower),
		hostAmbient: s.MetricName(semconv.HwHostAmbientTemperature),

		facilityAmbient:       s.MetricName(semconv.JoulieFacilityAmbientTemperature),
		facilityITEnergy:      s.MetricName(semconv.JoulieFacilityITEnergy),
		facilityITPower:       s.MetricName(semconv.JoulieFacilityITPower),
		facilityCoolingEnergy: s.MetricName(semconv.JoulieFacilityCoolingEnergy),
		facilityCoolingPower:  s.MetricName(semconv.JoulieFacilityCoolingPower),
		zoneAmbient:           s.MetricName(semconv.JoulieCoolingZoneAmbientTemperature),
		rackEnergy:            s.MetricName(semconv.JoulieRackEnergy),
		rackPower:             s.MetricName(semconv.JoulieRackPower),
	}
}

// parseNaming returns the canonical spelling of s; empty means the default.
func parseNaming(s promname.Strategy) (promname.Strategy, error) {
	if s == "" {
		return promname.Default, nil
	}
	return promname.ParseStrategy(string(s))
}

// FacilityMetricNames returns every metric name a query of input may
// select, under every preset and translation strategy, without duplicates:
// the otel names under each strategy in promname.Strategies order, then the
// legacy name. The simulator's fake Prometheus resolves a query by these
// exact names. An unknown input has none.
func FacilityMetricNames(input FacilityInput) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, s := range promname.Strategies() {
		n := namesFor(s)
		switch input {
		case FacilityInputAmbientTemp:
			add(n.facilityAmbient)
		case FacilityInputITPower:
			add(n.facilityITEnergy)
			add(n.facilityITPower)
		case FacilityInputCoolingPower:
			add(n.facilityCoolingEnergy)
			add(n.facilityCoolingPower)
		case FacilityInputZoneAmbientTemp:
			add(n.zoneAmbient)
		case FacilityInputRackPower:
			add(n.rackEnergy)
			add(n.rackPower)
		case FacilityInputNodeInletTemp:
			add(n.hostAmbient)
		}
	}
	switch input {
	case FacilityInputAmbientTemp:
		add(legacyAmbientTemp)
	case FacilityInputITPower:
		add(legacyITPower)
	case FacilityInputCoolingPower:
		add(legacyCoolingPower)
	}
	return out
}
