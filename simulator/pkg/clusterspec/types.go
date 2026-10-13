// Package clusterspec reads the spec of a simulated cluster
// (apiVersion sim.joulie.io/v1alpha1, kind ClusterSpec): pools of nodes, each
// with a hardware profile, a count, labels, taints, a rack and a cooling
// zone, and the facility around them. Expanding a spec gives one Member per
// node, carrying its profile and the Kubernetes Node object to create; it is
// the only place fake Nodes are rendered, for the simulator and the harness
// alike.
//
// The Go structs mirror the YAML: every JSON tag equals the YAML key, and a
// spec is decoded strictly, so an unknown key is an error. spec.harness is
// the exception: it is kept as raw JSON for the harness and never read here.
package clusterspec

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// APIVersion and Kind of a cluster spec document.
const (
	APIVersion = "sim.joulie.io/v1alpha1"
	Kind       = "ClusterSpec"
)

// Labels, annotations and taints a spec puts on its Nodes.
const (
	// LabelCluster is the spec name on every Node of the spec. It selects
	// the spec's Nodes for workload placement and for cleanup.
	LabelCluster = "sim.joulie.io/cluster"
	// LabelPool is the pool name on every Node of the spec.
	LabelPool = "sim.joulie.io/pool"
	// AnnotationProfile names the hardware profile a Node was built from.
	AnnotationProfile = "sim.joulie.io/profile"
	// LabelRack and LabelCoolingZone carry a pool's rack and cooling zone,
	// the topology labels the controller manager reads.
	LabelRack        = "joulie.io/rack"
	LabelCoolingZone = "joulie.io/cooling-zone"
	// KWOKAnnotation (value KWOKValue) makes KWOK manage a Node, and the
	// NoSchedule taint KWOKTaintKey=KWOKValue keeps pods that do not
	// tolerate it off the fake Node.
	KWOKAnnotation = "kwok.x-k8s.io/node"
	KWOKTaintKey   = "kwok.x-k8s.io/node"
	KWOKValue      = "fake"
)

// Spec is one cluster spec document.
type Spec struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       SpecBody `json:"spec"`
}

// Metadata names the spec. The name is the value of LabelCluster.
type Metadata struct {
	Name string `json:"name"`
}

// SpecBody is the spec proper. Harness is opaque to this package: the
// harness decodes it, and validation never looks inside.
type SpecBody struct {
	Defaults Defaults        `json:"defaults,omitempty"`
	Facility Facility        `json:"facility,omitempty"`
	Pools    []Pool          `json:"pools"`
	Harness  json.RawMessage `json:"harness,omitempty"`
}

// Defaults apply to every pool. Kwok, when unset, means true. A pool's
// labels, annotations and baseLoad fields override these key by key.
type Defaults struct {
	Kwok        *bool             `json:"kwok,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	BaseLoad    *Load             `json:"baseLoad,omitempty"`
}

// Load is the background load of a node while no workload run drives it:
// CPUUtil per logical CPU and GPUUtil per GPU, each in 0..1, with the
// workload classes hwemu.Load takes. A nil field is unset, so a pool can
// override one field of the defaults.
type Load struct {
	CPUUtil         *float64 `json:"cpuUtil,omitempty"`
	GPUUtil         *float64 `json:"gpuUtil,omitempty"`
	MemoryIntensity *float64 `json:"memoryIntensity,omitempty"`
	CPUClass        string   `json:"cpuClass,omitempty"`
	GPUClass        string   `json:"gpuClass,omitempty"`
}

// Facility is the data centre around the nodes. An unset temperature
// parameter falls back to the simulator's SIM_FACILITY_* setting.
type Facility struct {
	AmbientC     *float64      `json:"ambientC,omitempty"`
	AmplitudeC   *float64      `json:"amplitudeC,omitempty"`
	PeriodH      *float64      `json:"periodH,omitempty"`
	CoolingZones []CoolingZone `json:"coolingZones,omitempty"`
}

// CoolingZone is one cooling zone; its nodes see the facility ambient plus
// OffsetC at their inlet.
type CoolingZone struct {
	Name    string  `json:"name"`
	OffsetC float64 `json:"offsetC,omitempty"`
}

// Pool is Count nodes of one profile, named <Prefix>-<StartIndex+i>; Prefix
// defaults to the pool name. CoolingZone must be declared under
// facility.coolingZones. Taints come after the profile's.
type Pool struct {
	Name        string            `json:"name"`
	Profile     string            `json:"profile"`
	Count       int               `json:"count"`
	Prefix      string            `json:"prefix,omitempty"`
	StartIndex  int               `json:"startIndex,omitempty"`
	Rack        string            `json:"rack,omitempty"`
	CoolingZone string            `json:"coolingZone,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Taints      []corev1.Taint    `json:"taints,omitempty"`
	Overrides   *Overrides        `json:"overrides,omitempty"`
	BaseLoad    *Load             `json:"baseLoad,omitempty"`
}

// Overrides change a pool's profile. Each set field replaces the profile
// value, with the provenance "assumed:cluster spec <spec> pool <pool>
// override", and the result must still validate. Keys are the profile's.
type Overrides struct {
	NFD  string        `json:"nfd,omitempty"`
	GPUs *GPUOverrides `json:"gpus,omitempty"`
}

// GPUOverrides change the profile's gpus block.
type GPUOverrides struct {
	PersistenceMode   string `json:"persistenceMode,omitempty"`
	Exposure          string `json:"exposure,omitempty"`
	MIGProfile        string `json:"migProfile,omitempty"`
	MIGDevicesPerGPU  *int   `json:"migDevicesPerGPU,omitempty"`
	TimesliceReplicas *int   `json:"timesliceReplicas,omitempty"`
}

// Member is one node of an expanded spec: its pool, topology, the profile
// with the pool's overrides applied, the Node object to create, and its
// background load with the defaults merged in.
type Member struct {
	Name        string         `json:"name"`
	Pool        string         `json:"pool"`
	Rack        string         `json:"rack,omitempty"`
	CoolingZone string         `json:"coolingZone,omitempty"`
	ZoneOffsetC float64        `json:"zoneOffsetC,omitempty"`
	Profile     *hwemu.Profile `json:"profile"`
	Node        *corev1.Node   `json:"node"`
	BaseLoad    Load           `json:"baseLoad"`
}
