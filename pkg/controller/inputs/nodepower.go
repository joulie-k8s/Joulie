package inputs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Query returns the node power query. fallback is set only under
// ResourceAttributesAuto, where primary is the labels form and fallback the
// target_info form. A combination no preset serves, such as the kepler
// preset with scope host, is an error.
//
// Zero fields take their defaults: the otel preset (custom when CustomQuery
// is set), scope components, the default translation strategy, auto and
// DefaultWindow. A preset other than custom together with CustomQuery is an
// error, so a query is never silently ignored.
func (s NodePowerSpec) Query() (primary, fallback Query, err error) {
	s, err = s.withDefaults()
	if err != nil {
		return Query{}, Query{}, err
	}
	window, err := PromDuration(s.Window)
	if err != nil {
		return Query{}, Query{}, err
	}
	switch s.Preset {
	case NodePowerPresetOTel:
		n := namesFor(s.Naming)
		node := n.node
		if s.NodeLabel != "" {
			node = s.NodeLabel
		}
		labels := Query{PromQL: otelNodePower(n, node, s.Scope, window, false), GroupLabel: node}
		targetInfo := Query{PromQL: otelNodePower(n, node, s.Scope, window, true), GroupLabel: node}
		switch s.ResourceAttributes {
		case ResourceAttributesLabels:
			return labels, Query{}, nil
		case ResourceAttributesTargetInfo:
			return targetInfo, Query{}, nil
		}
		return labels, targetInfo, nil
	case NodePowerPresetKepler:
		node := keplerNodeLabel
		if s.NodeLabel != "" {
			node = s.NodeLabel
		}
		return Query{PromQL: keplerComponents(node, keplerCPUJoules, []matcher{{keplerZoneLabel, "=", keplerPackageZone}}, window), GroupLabel: node}, Query{}, nil
	case NodePowerPresetKeplerLegacy:
		node := keplerLegacyNodeLabel
		if s.NodeLabel != "" {
			node = s.NodeLabel
		}
		if s.Scope == ScopeHost {
			q := fmt.Sprintf("sum by %s (%s)", labelList(node), rate(selector(keplerLegacyPlatform), window))
			return Query{PromQL: q, GroupLabel: node}, Query{}, nil
		}
		return Query{PromQL: keplerComponents(node, keplerLegacyPackage, nil, window), GroupLabel: node}, Query{}, nil
	}
	// Custom. With the placeholder the caller runs the query once per node
	// and sums every series; without it, the query is summed by the node
	// label, so a node with several series is read whole, never by its
	// first series.
	if strings.Contains(s.CustomQuery, NodePlaceholder) {
		return Query{PromQL: s.CustomQuery, PerNode: true}, Query{}, nil
	}
	node := s.NodeLabel
	if node == "" {
		node = namesFor(s.Naming).node
	}
	return Query{PromQL: wrapAggregation("sum", node, s.CustomQuery), GroupLabel: node}, Query{}, nil
}

// withDefaults fills the zero fields and rejects unknown values and
// combinations no preset serves.
func (s NodePowerSpec) withDefaults() (NodePowerSpec, error) {
	if s.Preset == "" {
		s.Preset = NodePowerPresetOTel
		if s.CustomQuery != "" {
			s.Preset = NodePowerPresetCustom
		}
	}
	switch s.Preset {
	case NodePowerPresetOTel, NodePowerPresetKepler, NodePowerPresetKeplerLegacy:
		if s.CustomQuery != "" {
			return s, fmt.Errorf("inputs: node power preset %q does not run a custom query; use preset %q or leave the preset unset", s.Preset, NodePowerPresetCustom)
		}
	case NodePowerPresetCustom:
		if strings.TrimSpace(s.CustomQuery) == "" {
			return s, fmt.Errorf("inputs: node power preset %q needs a query", NodePowerPresetCustom)
		}
	default:
		return s, fmt.Errorf("inputs: unknown node power preset %q, want %s, %s, %s or %s",
			s.Preset, NodePowerPresetOTel, NodePowerPresetKepler, NodePowerPresetKeplerLegacy, NodePowerPresetCustom)
	}
	switch s.Scope {
	case "":
		s.Scope = ScopeComponents
	case ScopeComponents, ScopeHost:
	default:
		return s, fmt.Errorf("inputs: unknown node power scope %q, want %s or %s", s.Scope, ScopeComponents, ScopeHost)
	}
	// Kepler 0.10 and later have no host series Joulie can read the same
	// way on every platform: kepler_platform_watts is a BMC gauge whose
	// meaning depends on the source.
	if s.Preset == NodePowerPresetKepler && s.Scope == ScopeHost {
		return s, fmt.Errorf("inputs: node power preset %q has no host scope; use scope %s, or preset %s for Kepler 0.7 to 0.9",
			NodePowerPresetKepler, ScopeComponents, NodePowerPresetKeplerLegacy)
	}
	switch s.ResourceAttributes {
	case "":
		s.ResourceAttributes = ResourceAttributesAuto
	case ResourceAttributesAuto, ResourceAttributesLabels, ResourceAttributesTargetInfo:
	default:
		return s, fmt.Errorf("inputs: unknown resource attributes mode %q, want %s, %s or %s",
			s.ResourceAttributes, ResourceAttributesAuto, ResourceAttributesLabels, ResourceAttributesTargetInfo)
	}
	naming, err := parseNaming(s.Naming)
	if err != nil {
		return s, fmt.Errorf("inputs: %w", err)
	}
	s.Naming = naming
	if s.Window == 0 {
		s.Window = DefaultWindow
	}
	return s, nil
}

// otelNodePower is the otel preset's query. Scope components sums the cpu
// and gpu components of each node: the rate of each energy counter, or the
// power gauge of a component without one, deduplicated per (node, hw.type,
// hw.id) by max, since two producers may report one component. hw.type is
// part of the key because hw.id is unique only within a type on some
// producers. Scope host reads the host energy, or its power gauge. In the
// target_info form every selector is joined with target_info for the node.
func otelNodePower(n otelNames, node string, scope Scope, window string, targetInfo bool) string {
	join := func(x string) string {
		if targetInfo {
			return joinTargetInfo(x, node)
		}
		return x
	}
	if scope == ScopeHost {
		key := labelList(node)
		return fmt.Sprintf("max by %s (%s) or on %s max by %s (%s)",
			key, join(rate(selector(n.hostEnergy), window)), key, key, join(selector(n.hostPower)))
	}
	components := matcher{n.hwType, "=~", semconv.HwTypeCPU + "|" + semconv.HwTypeGPU}
	key := labelList(node, n.hwType, n.hwID)
	return fmt.Sprintf("sum by %s (max by %s (%s) or on %s max by %s (%s))",
		labelList(node),
		key, join(rate(selector(n.energy, components), window)),
		key, key, join(selector(n.power, components)))
}

// keplerComponents sums the CPU package and GPU energy rates of each node.
// Each branch of the "or" gets its own componentLabel value first, or the
// GPU series of a node whose label set equals its CPU series' would be
// dropped.
func keplerComponents(node, cpuMetric string, cpuMatchers []matcher, window string) string {
	branch := func(sel, component string) string {
		return fmt.Sprintf("label_replace(%s, %s, %s, \"\", \"\")", rate(sel, window), strconv.Quote(componentLabel), strconv.Quote(component))
	}
	return fmt.Sprintf("sum by %s (%s or %s)", labelList(node),
		branch(selector(cpuMetric, cpuMatchers...), semconv.HwTypeCPU),
		branch(selector(keplerGPUJoules), semconv.HwTypeGPU))
}

// wrapAggregation writes op by (label) (q), or op(q) without a label. The
// closing parenthesis goes on a line of its own when q may end in a PromQL
// comment.
func wrapAggregation(op, label, q string) string {
	end := ")"
	if strings.Contains(q, "#") {
		end = "\n)"
	}
	if label == "" {
		return op + "(" + q + end
	}
	return fmt.Sprintf("%s by %s (%s%s", op, labelList(label), q, end)
}
