// Package inputs reads the controller manager's measured inputs from
// Prometheus: node power, and the facility readings (outdoor ambient
// temperature, IT and cooling power, the ambient temperature of each cooling
// zone, rack power and node inlet temperature).
//
// A spec names a preset and the package builds the PromQL. The otel presets
// read the metrics of semconv.NodeInputs and semconv.FacilityInputs as a
// Prometheus OTLP receiver stores them, spelled by package promname under
// the receiver's translation strategy. The kepler, kepler-legacy and legacy
// presets read compatibility formats, and custom runs the adopter's own
// query. Every input is one grouped instant query per reconcile
// (GET <address>/api/v1/query), parsed into a map keyed by node, cooling
// zone or rack. Only the controller manager queries Prometheus; the
// scheduler extender reads NodeTwin.
//
// This file holds the package API: the specs, the query and result types and
// the errors. nodepower.go and facility.go build the queries, names.go spells
// them under a translation strategy, and prom.go runs them and reads the
// results.
package inputs

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/matbun/joulie/pkg/otel/promname"
)

// Rate windows. A window must cover at least four export intervals of the
// producer, or rate() reads too low.
const (
	// DefaultWindow is the rate window of PROMETHEUS_RATE_WINDOW unset.
	DefaultWindow = 5 * time.Minute
	// MinWindow is the shortest window PromDuration accepts.
	MinWindow = 10 * time.Second
)

// NodePlaceholder in a custom node power query stands for the node name:
// such a query runs once per node.
const NodePlaceholder = "{node}"

// ResourceAttributes says where the otel presets find the node identity,
// the resource attribute k8s.node.name.
type ResourceAttributes string

const (
	// ResourceAttributesAuto runs the labels form first and switches to the
	// target_info form when it returns nothing or a series lacks the node
	// label. It keeps the form that works until that form returns no data
	// or an error. The default.
	ResourceAttributesAuto ResourceAttributes = "auto"
	// ResourceAttributesLabels reads the node label on the data series,
	// which needs k8s.node.name in Prometheus' otlp.promote_resource_attributes.
	ResourceAttributesLabels ResourceAttributes = "labels"
	// ResourceAttributesTargetInfo joins every series with target_info on
	// job and instance to find the node, and needs no promotion.
	ResourceAttributesTargetInfo ResourceAttributes = "target_info"
)

// NodePowerPreset names the format the node power query reads.
type NodePowerPreset string

const (
	// NodePowerPresetOTel reads component energy and power (scope
	// components) or host energy and power (scope host), grouped by the
	// translated k8s.node.name. The default.
	NodePowerPresetOTel NodePowerPreset = "otel"
	// NodePowerPresetKepler reads Kepler 0.10 and later: CPU package and
	// GPU joules counters grouped by node_name. GPU series exist only from
	// Kepler 0.12 with its experimental GPU feature on. Scope host is
	// rejected.
	NodePowerPresetKepler NodePowerPreset = "kepler"
	// NodePowerPresetKeplerLegacy reads Kepler 0.7 to 0.9: package and GPU
	// joules under scope components, platform joules under scope host,
	// grouped by exported_instance.
	NodePowerPresetKeplerLegacy NodePowerPreset = "kepler-legacy"
	// NodePowerPresetCustom runs NodePowerSpec.CustomQuery.
	NodePowerPresetCustom NodePowerPreset = "custom"
)

// Scope is what measured node power covers.
type Scope string

const (
	// ScopeComponents is the CPU packages plus the GPUs, the boundary of the
	// twin's power budget (CPU TDP plus GPU TDP). The default.
	ScopeComponents Scope = "components"
	// ScopeHost is the whole host, which exceeds the budget under load.
	ScopeHost Scope = "host"
)

// FacilityInput names one facility reading. Its value, upper-cased, is the
// middle of the variable that overrides its query: FacilityInputITPower is
// FACILITY_IT_POWER_QUERY.
type FacilityInput string

const (
	// FacilityInputAmbientTemp is the outdoor ambient temperature, one
	// value in degrees Celsius.
	FacilityInputAmbientTemp FacilityInput = "ambient_temp"
	// FacilityInputITPower is the power of the IT equipment, one value in W.
	FacilityInputITPower FacilityInput = "it_power"
	// FacilityInputCoolingPower is the power of the cooling, one value in W.
	FacilityInputCoolingPower FacilityInput = "cooling_power"
	// FacilityInputZoneAmbientTemp is the ambient temperature of each
	// cooling zone, keyed by the joulie.io/cooling-zone node label value.
	FacilityInputZoneAmbientTemp FacilityInput = "zone_ambient_temp"
	// FacilityInputRackPower is the power of each rack, keyed by the
	// joulie.io/rack node label value.
	FacilityInputRackPower FacilityInput = "rack_power"
	// FacilityInputNodeInletTemp is the inlet temperature of each node,
	// keyed by node name.
	FacilityInputNodeInletTemp FacilityInput = "node_inlet_temp"
)

// FacilityInputs returns every facility input, in the order declared above.
func FacilityInputs() []FacilityInput {
	return []FacilityInput{
		FacilityInputAmbientTemp, FacilityInputITPower, FacilityInputCoolingPower,
		FacilityInputZoneAmbientTemp, FacilityInputRackPower, FacilityInputNodeInletTemp,
	}
}

// FacilityPreset names the format the facility queries read.
type FacilityPreset string

const (
	// FacilityPresetOTel reads Joulie's facility, cooling zone and rack
	// metrics and the host ambient temperature as node inlet. The default.
	FacilityPresetOTel FacilityPreset = "otel"
	// FacilityPresetLegacy reads the datacenter_* names the controller
	// manager read before the otel preset existed. Deprecated, kept for one
	// release.
	FacilityPresetLegacy FacilityPreset = "legacy"
	// FacilityPresetCustom has no queries of its own: only the inputs in
	// FacilitySpec.Overrides are read.
	FacilityPresetCustom FacilityPreset = "custom"
)

// NodePowerSpec selects the node power query.
type NodePowerSpec struct {
	Preset NodePowerPreset
	Scope  Scope
	// Naming is the receiver's translation strategy. It spells the otel
	// preset's names and the custom preset's default node label;
	// ResourceAttributes applies to the otel preset only.
	Naming             promname.Strategy
	ResourceAttributes ResourceAttributes
	// Window is the rate() window, at least MinWindow.
	Window time.Duration
	// NodeLabel overrides the preset's grouping label: the translated
	// k8s.node.name, node_name or exported_instance. Empty keeps it.
	NodeLabel string
	// CustomQuery is the query of the custom preset. With NodePlaceholder
	// it runs once per node; without, it is grouped by the node label.
	CustomQuery string
}

// Query is one PromQL instant query. GroupLabel is the label that keys its
// result: the node label for node power, the zone, rack or node label for
// the keyed facility inputs, and empty for a single value. PerNode marks a
// query holding NodePlaceholder, which the caller fills in and runs once per
// node, summing every series it returns.
type Query struct {
	PromQL, GroupLabel string
	PerNode            bool
}

// FacilitySpec selects the facility queries.
type FacilitySpec struct {
	Preset FacilityPreset
	// Naming is the receiver's translation strategy. It spells the otel
	// preset's names and, under every preset, the key labels of the zone,
	// rack and node inputs.
	Naming promname.Strategy
	// Window is the rate() window, at least MinWindow.
	Window time.Duration
	// Overrides replaces the query of an input. A bare metric name is
	// wrapped as avg(...) for a temperature and sum(...) for a power.
	Overrides map[FacilityInput]string
}

// Client runs instant queries against the Prometheus HTTP API.
type Client struct {
	// Address is the base URL, such as http://prometheus-operated.monitoring:9090.
	Address string
	// HTTP is the client that sends the requests; nil means
	// http.DefaultClient.
	HTTP *http.Client
}

// Result is the answer to one query. Type is "vector" or "scalar"; a scalar
// is one Sample without labels.
type Result struct {
	Type     string
	Samples  []Sample
	Warnings []string
}

// Sample is one series of a result. Value is what Prometheus returned,
// possibly NaN or infinite; the readers of Result reject such values.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Issues counts what a reader found wrong in a result, for logs and hints.
type Issues struct {
	// Series is the number of samples in the result.
	Series int
	// MissingLabel counts samples without the grouping label.
	MissingLabel int
	// NonFinite counts NaN and infinite values.
	NonFinite int
	// Duplicates counts samples whose key an earlier sample already had.
	Duplicates int
}

// Error is a query Prometheus failed: the HTTP status, its errorType (such
// as bad_data, timeout or execution) and its error. For a body that is not
// JSON, Type is empty and Message holds the start of the body.
type Error struct {
	Status        int
	Type, Message string
}

func (e *Error) Error() string {
	if e.Type == "" {
		return fmt.Sprintf("prometheus: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("prometheus: HTTP %d %s: %s", e.Status, e.Type, e.Message)
}

// ErrNoData is returned when a query succeeds without a sample.
var ErrNoData = errors.New("inputs: query returned no data")
