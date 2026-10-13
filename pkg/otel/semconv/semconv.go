// Package semconv is the one table of OpenTelemetry names Joulie writes and
// reads: metrics, attributes and events. The simulator emits these names and
// the controller manager queries them, so each name is spelled once, here,
// and every other package refers to the exported values. The Prometheus
// spelling of a name is computed from the table (package promname), never
// written by hand.
//
// Names follow the semantic conventions v1.44.0 (SchemaURL). Every hw.*
// metric and attribute is at Development stability there and may be renamed
// upstream without a deprecation; the pin turns such a rename into a
// deliberate change of this table. Names Joulie defines itself use the
// application prefix joulie., as naming.md ("Recommendations for application
// developers") advises for names no convention covers.
//
// The table is append-only, one file per stage. A file declares its names and
// registers them at package initialisation: metrics with Register, events
// with RegisterEvent, attributes with registerAttributes in its init. Each of
// them panics on a duplicate, so two files cannot claim one name.
package semconv

import (
	"fmt"
	"sort"
)

// SchemaURL is the semantic conventions version every resource and scope
// Joulie emits declares as its schemaUrl.
const SchemaURL = "https://opentelemetry.io/schemas/1.44.0"

// Kind is the instrument of a metric. The zero Kind is invalid.
type Kind int

const (
	// Counter is a monotonic Sum, exported with cumulative temporality.
	Counter Kind = iota + 1
	// Gauge is a Gauge.
	Gauge
	// UpDownCounter is a non-monotonic Sum.
	UpDownCounter
	// Histogram is an explicit-bucket Histogram.
	Histogram
)

func (k Kind) String() string {
	switch k {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case UpDownCounter:
		return "updowncounter"
	case Histogram:
		return "histogram"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// ValueType is the type of a metric's points: asDouble or asInt in OTLP. The
// zero ValueType is invalid.
type ValueType int

const (
	Double ValueType = iota + 1
	Int
)

func (v ValueType) String() string {
	switch v {
	case Double:
		return "double"
	case Int:
		return "int"
	}
	return fmt.Sprintf("ValueType(%d)", int(v))
}

// Owners of a name: the semantic conventions or Joulie.
const (
	OwnerSemconv = "semconv"
	OwnerJoulie  = "joulie"
)

// StabilityDevelopment is the stability of every name in the table: the
// semantic conventions mark all hw.* names Development, and Joulie's own
// names carry no stronger promise.
const StabilityDevelopment = "development"

// Metric is one metric of the table. Unit is a UCUM unit as the semantic
// conventions write it (J, W, Cel, Hz, 1, s, or an annotation such as
// {job}). Attributes lists the point attributes the metric may carry; a
// producer sets a subset of them and never a resource attribute.
type Metric struct {
	Name, Unit, Brief, Stability, Owner string
	Kind                                Kind
	Value                               ValueType
	Attributes                          []string
}

// HasAttribute reports whether a point of m may carry attribute a.
func (m Metric) HasAttribute(a string) bool {
	for _, x := range m.Attributes {
		if x == a {
			return true
		}
	}
	return false
}

// Event is one event: an OTel LogRecord whose eventName is Name. Attributes
// lists the attributes its records may carry.
type Event struct {
	Name, Brief    string
	SeverityNumber int
	SeverityText   string
	Attributes     []string
}

// HasAttribute reports whether a record of e may carry attribute a.
func (e Event) HasAttribute(a string) bool {
	for _, x := range e.Attributes {
		if x == a {
			return true
		}
	}
	return false
}

// Severities of Joulie's events and their short names (logs data model,
// "Field: SeverityNumber"): INFO for normal operation, WARN for a condition
// the reader should look at, ERROR for a failure.
const (
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17

	SeverityTextInfo  = "INFO"
	SeverityTextWarn  = "WARN"
	SeverityTextError = "ERROR"
)

// The registry is written only during package initialisation, so the
// accessors need no lock.
var (
	registryMetrics    = map[string]Metric{}
	registryEvents     = map[string]Event{}
	registryAttributes = map[string]bool{}
)

// Register adds m to the table and returns it, for use in a package-level
// variable. The table keeps its own copy of m.Attributes, so a later write to
// the argument or to the result never reaches it. It panics on a duplicate or
// empty name and on an invalid Kind, ValueType or Owner. It is meant for
// package initialisation and is not safe for concurrent use.
func Register(m Metric) Metric {
	if m.Name == "" {
		panic("semconv: metric with an empty name")
	}
	if _, dup := registryMetrics[m.Name]; dup {
		panic(fmt.Sprintf("semconv: metric %q registered twice", m.Name))
	}
	if m.Kind < Counter || m.Kind > Histogram {
		panic(fmt.Sprintf("semconv: metric %q has invalid kind %v", m.Name, m.Kind))
	}
	if m.Value != Double && m.Value != Int {
		panic(fmt.Sprintf("semconv: metric %q has invalid value type %v", m.Name, m.Value))
	}
	if m.Owner != OwnerSemconv && m.Owner != OwnerJoulie {
		panic(fmt.Sprintf("semconv: metric %q has owner %q, want %q or %q", m.Name, m.Owner, OwnerSemconv, OwnerJoulie))
	}
	m.Attributes = append([]string(nil), m.Attributes...)
	registryMetrics[m.Name] = m
	m.Attributes = append([]string(nil), m.Attributes...)
	return m
}

// RegisterEvent adds e to the table and returns it, for use in a
// package-level variable. The table keeps its own copy of e.Attributes, as
// Register does. It panics on a duplicate or empty name. It is meant for
// package initialisation and is not safe for concurrent use.
func RegisterEvent(e Event) Event {
	if e.Name == "" {
		panic("semconv: event with an empty name")
	}
	if _, dup := registryEvents[e.Name]; dup {
		panic(fmt.Sprintf("semconv: event %q registered twice", e.Name))
	}
	e.Attributes = append([]string(nil), e.Attributes...)
	registryEvents[e.Name] = e
	e.Attributes = append([]string(nil), e.Attributes...)
	return e
}

// registerAttributes adds attribute names to the table. Each file calls it
// from its init with every attribute constant it declares. It panics on a
// duplicate or empty name, and then adds none of the names.
func registerAttributes(names ...string) {
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			panic("semconv: attribute with an empty name")
		}
		if registryAttributes[n] || seen[n] {
			panic(fmt.Sprintf("semconv: attribute %q registered twice", n))
		}
		seen[n] = true
	}
	for _, n := range names {
		registryAttributes[n] = true
	}
}

// Metrics returns every registered metric, sorted by name. The result is a
// copy.
func Metrics() []Metric {
	out := make([]Metric, 0, len(registryMetrics))
	for _, m := range registryMetrics {
		m.Attributes = append([]string(nil), m.Attributes...)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Events returns every registered event, sorted by name. The result is a
// copy.
func Events() []Event {
	out := make([]Event, 0, len(registryEvents))
	for _, e := range registryEvents {
		e.Attributes = append([]string(nil), e.Attributes...)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Attributes returns every registered attribute name, resource and point
// attributes alike, sorted.
func Attributes() []string {
	out := make([]string, 0, len(registryAttributes))
	for a := range registryAttributes {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Attributes from the semantic conventions v1.44.0 registry.
const (
	AttrHwID        = "hw.id"
	AttrHwType      = "hw.type"
	AttrHwName      = "hw.name"
	AttrHwParent    = "hw.parent"
	AttrHwVendor    = "hw.vendor"
	AttrHwModel     = "hw.model"
	AttrHwLimitType = "hw.limit_type"
	AttrHwGPUTask   = "hw.gpu.task"

	AttrK8sClusterName   = "k8s.cluster.name"
	AttrK8sNodeName      = "k8s.node.name"
	AttrK8sNamespaceName = "k8s.namespace.name"
	AttrK8sPodName       = "k8s.pod.name"
	AttrK8sPodUID        = "k8s.pod.uid"
	AttrK8sJobName       = "k8s.job.name"

	AttrHostName = "host.name"
	AttrHostID   = "host.id"

	AttrServiceName       = "service.name"
	AttrServiceNamespace  = "service.namespace"
	AttrServiceInstanceID = "service.instance.id"
	AttrServiceVersion    = "service.version"

	AttrErrorType = "error.type"
)

// Attributes Joulie defines.
const (
	// AttrJoulieLimitState says which limit a joulie.hw.power.limit point
	// is: LimitStateRequested or LimitStateEnforced.
	AttrJoulieLimitState = "joulie.hw.limit.state"
	// AttrJoulieCoolingZoneName is the cooling zone of a zone metric, the
	// value of the joulie.io/cooling-zone node label.
	AttrJoulieCoolingZoneName = "joulie.cooling_zone.name"
	// AttrJoulieRackName is the rack of a rack metric, the value of the
	// joulie.io/rack node label.
	AttrJoulieRackName = "joulie.rack.name"
	// AttrJoulieWorkloadClass is the joulie.io/workload-class of a pod.
	AttrJoulieWorkloadClass = "joulie.workload.class"
)

// Attribute values.
const (
	// hw.type of a CPU package, its DRAM zone and a GPU. Whole-host
	// readings are hw.host.* metrics and carry no hw.type.
	HwTypeCPU    = "cpu"
	HwTypeMemory = "memory"
	HwTypeGPU    = "gpu"
	// HwIDHost is the hw.id of hw.host.* points.
	HwIDHost = "host"
	// hw.limit_type of hw.cpu.speed.limit: the hardware maximum and the
	// maximum in effect after limits.
	LimitTypeMax       = "max"
	LimitTypeThrottled = "throttled"
	// joulie.hw.limit.state of joulie.hw.power.limit: the limit as written
	// and the limit the hardware enforces.
	LimitStateRequested = "requested"
	LimitStateEnforced  = "enforced"
	// GPUTaskGeneral is the hw.gpu.task of whole-GPU utilization.
	GPUTaskGeneral = "general"
)

func init() {
	registerAttributes(
		AttrHwID, AttrHwType, AttrHwName, AttrHwParent, AttrHwVendor, AttrHwModel, AttrHwLimitType, AttrHwGPUTask,
		AttrK8sClusterName, AttrK8sNodeName, AttrK8sNamespaceName, AttrK8sPodName, AttrK8sPodUID, AttrK8sJobName,
		AttrHostName, AttrHostID,
		AttrServiceName, AttrServiceNamespace, AttrServiceInstanceID, AttrServiceVersion,
		AttrErrorType,
		AttrJoulieLimitState, AttrJoulieCoolingZoneName, AttrJoulieRackName, AttrJoulieWorkloadClass,
	)
}

// Hardware metrics from the semantic conventions v1.44.0
// (model/hardware/*.yaml); Brief is the convention's brief. Whole-host
// energy is reported only as hw.host.*, never as hw.energy (the MUST in
// docs/hardware/host.md), so the two never overlap in a sum.
var (
	HwHostEnergy = Register(Metric{
		Name: "hw.host.energy", Unit: "J", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Counter, Value: Int, Attributes: []string{AttrHwID},
		Brief: "Total energy consumed by the entire physical host, in joules.",
	})
	HwHostPower = Register(Metric{
		Name: "hw.host.power", Unit: "W", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrHwID},
		Brief: "Instantaneous power consumed by the entire physical host in Watts (hw.host.energy is preferred).",
	})
	HwHostAmbientTemperature = Register(Metric{
		Name: "hw.host.ambient_temperature", Unit: "Cel", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrHwID},
		Brief: "Ambient (external) temperature of the physical host.",
	})
	HwEnergy = Register(Metric{
		Name: "hw.energy", Unit: "J", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double, Attributes: []string{AttrHwID, AttrHwType, AttrHwName, AttrHwParent},
		Brief: "Energy consumed by the component.",
	})
	HwPower = Register(Metric{
		Name: "hw.power", Unit: "W", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrHwID, AttrHwType, AttrHwName, AttrHwParent},
		Brief: "Instantaneous power consumed by the component.",
	})
	HwCPUSpeed = Register(Metric{
		Name: "hw.cpu.speed", Unit: "Hz", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Int, Attributes: []string{AttrHwID, AttrHwName, AttrHwVendor, AttrHwModel},
		Brief: "CPU current frequency.",
	})
	HwCPUSpeedLimit = Register(Metric{
		Name: "hw.cpu.speed.limit", Unit: "Hz", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Int, Attributes: []string{AttrHwID, AttrHwName, AttrHwVendor, AttrHwModel, AttrHwLimitType},
		Brief: "CPU maximum frequency.",
	})
	HwGPUUtilization = Register(Metric{
		Name: "hw.gpu.utilization", Unit: "1", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrHwID, AttrHwName, AttrHwVendor, AttrHwModel, AttrHwGPUTask},
		Brief: "Fraction of time spent in a specific task.",
	})
)

// Metrics Joulie defines: component power limits, and the facility, cooling
// zone and rack readings no convention covers. Facility metrics are emitted
// on a resource of their own with no point attributes.
var (
	JoulieHwPowerLimit = Register(Metric{
		Name: "joulie.hw.power.limit", Unit: "W", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrHwID, AttrHwType, AttrJoulieLimitState},
		Brief: "Power limit of a component: the limit as written, and the limit the hardware enforces.",
	})
	JoulieFacilityAmbientTemperature = Register(Metric{
		Name: "joulie.facility.ambient_temperature", Unit: "Cel", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double,
		Brief: "Outdoor ambient temperature at the facility.",
	})
	JoulieFacilityITEnergy = Register(Metric{
		Name: "joulie.facility.it.energy", Unit: "J", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double,
		Brief: "Energy consumed by the facility's IT equipment.",
	})
	JoulieFacilityITPower = Register(Metric{
		Name: "joulie.facility.it.power", Unit: "W", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double,
		Brief: "Instantaneous power consumed by the facility's IT equipment.",
	})
	JoulieFacilityCoolingEnergy = Register(Metric{
		Name: "joulie.facility.cooling.energy", Unit: "J", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double,
		Brief: "Energy consumed by the facility's cooling.",
	})
	JoulieFacilityCoolingPower = Register(Metric{
		Name: "joulie.facility.cooling.power", Unit: "W", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double,
		Brief: "Instantaneous power consumed by the facility's cooling.",
	})
	JoulieFacilityPUE = Register(Metric{
		Name: "joulie.facility.pue", Unit: "1", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double,
		Brief: "Power usage effectiveness: facility power over IT power.",
	})
	JoulieCoolingZoneAmbientTemperature = Register(Metric{
		Name: "joulie.cooling_zone.ambient_temperature", Unit: "Cel", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrJoulieCoolingZoneName},
		Brief: "Ambient temperature of a cooling zone, the inlet temperature of its nodes.",
	})
	JoulieRackEnergy = Register(Metric{
		Name: "joulie.rack.energy", Unit: "J", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double, Attributes: []string{AttrJoulieRackName},
		Brief: "Energy consumed by the hosts of a rack.",
	})
	JoulieRackPower = Register(Metric{
		Name: "joulie.rack.power", Unit: "W", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrJoulieRackName},
		Brief: "Instantaneous power consumed by the hosts of a rack.",
	})
)
