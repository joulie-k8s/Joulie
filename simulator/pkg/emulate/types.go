// Package emulate runs the emulated nodes of a cluster spec on one fixed
// tick: it keeps each node's hwemu tree on disk, steps every node to the same
// time, keeps energy ledgers that never decrease across a simulator restart,
// and publishes the result as a Snapshot. Telemetry, the compatibility
// exporters and the debug endpoint all read the same Snapshot.
package emulate

import (
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Snapshot is every emulated node and the facility after tick Seq. A
// published Snapshot is immutable: the emulator builds a new one each tick,
// and readers may keep and share it without a lock.
type Snapshot struct {
	Seq      uint64           `json:"seq"`
	Cluster  string           `json:"cluster"`
	Time     time.Time        `json:"time"`
	Nodes    []NodeSnapshot   `json:"nodes"`
	Facility FacilitySnapshot `json:"facility"`
}

// NodeSnapshot is one emulated node after a tick.
//
// StartTime is the start of the node's energy ledger, the
// startTimeUnixNano of its counters; it survives restarts. Started is set
// on the node's first snapshot after the emulator started, Fresh when that
// start rendered the tree rather than resuming it, and Added lists the
// entries a resume put back. Err is the error of the node's last step; the
// readings are then those of its last good step.
//
// PreviousGovernors is, per hw.id, the governor state a component was in
// before its current one. It stays until the state changes again, so every
// export after a change, however far apart exports and ticks are, reports
// the state left (joulie.hw.governor at 0). It is not persisted: a restarted
// simulator starts with none.
type NodeSnapshot struct {
	Name          string             `json:"name"`
	Pool          string             `json:"pool"`
	Profile       string             `json:"profile"`
	Rack          string             `json:"rack,omitempty"`
	CoolingZone   string             `json:"coolingZone,omitempty"`
	Root          string             `json:"root"`
	StartTime     time.Time          `json:"startTime"`
	Started       bool               `json:"started,omitempty"`
	Fresh         bool               `json:"fresh,omitempty"`
	Added         []string           `json:"added,omitempty"`
	DriverVersion string             `json:"driverVersion,omitempty"`
	Host          HostReading        `json:"host"`
	Components    []ComponentReading `json:"components"`
	Events        []hwemu.Event      `json:"events,omitempty"`
	Err           string             `json:"err,omitempty"`

	PreviousGovernors map[string]GovernorState `json:"previousGovernors,omitempty"`
}

// GovernorState is the governor state of a CPU package or a GPU: Native in
// the driver's own words and Preset, the preset it maps to, empty when no
// single one does. The zero value is a component without a governor.
type GovernorState struct {
	Native string `json:"native"`
	Preset string `json:"preset,omitempty"`
}

// HostReading is the whole node. PowerW is the sum of every package, DRAM
// zone and GPU plus the platform's OtherW; PSU losses are not included.
// EnergyJ is the ledger value. Measured is false where no collector could
// read host power, as on a hypervisor guest; such a host exports no hw.*
// metric. InletC is the node's inlet temperature: its cooling zone's
// temperature, else the facility ambient.
type HostReading struct {
	Measured bool    `json:"measured"`
	PowerW   float64 `json:"powerW"`
	EnergyJ  float64 `json:"energyJ"`
	OtherW   float64 `json:"otherW"`
	InletC   float64 `json:"inletC"`
}

// ComponentReading is one component: a CPU package (Type cpu, ID
// cpu<socket>), its DRAM zone (memory, memory<socket>, Parent cpu<socket>)
// or a GPU (gpu, gpu<index>). Index is the socket or the GPU index.
//
// EnergyJ is the ledger value, monotonic across restarts; DeviceEnergyJ is
// the GPU's own counter (total_energy_mj / 1000), which a compatibility
// exporter reports as the device does. LimitW is the limit in effect and
// RequestedLimitW the one written; both are 0 when the component has no
// power limit. Speeds are in Hz and set for CPU packages with cpufreq.
// Utilization is in 0..1. Measured is false for a component no collector
// could read, such as a DRAM zone without an energy counter. ClockHz,
// ClockMaxHz, ClockLimitHz, GovernorNative and GovernorPreset are a GPU's
// current, hardware maximum and effective maximum clocks per domain and the
// governor state of a package or GPU, as hwemu.ComponentState documents
// them. PeerRxBytes and PeerTxBytes are the peer transfer bytes of a GPU as
// ledger values, like EnergyJ: monotonic across restarts.
//
// The fields are those of hwemu.ComponentState, in the same order, so a
// conversion copies one into the other.
type ComponentReading struct {
	ID              string           `json:"id"`
	Type            string           `json:"type"`
	Name            string           `json:"name"`
	Vendor          string           `json:"vendor,omitempty"`
	Model           string           `json:"model,omitempty"`
	UUID            string           `json:"uuid,omitempty"`
	PCIBusID        string           `json:"pciBusId,omitempty"`
	SysfsPath       string           `json:"sysfsPath,omitempty"`
	Parent          string           `json:"parent,omitempty"`
	Index           int              `json:"index"`
	Measured        bool             `json:"measured"`
	PowerW          float64          `json:"powerW"`
	EnergyJ         float64          `json:"energyJ"`
	DeviceEnergyJ   float64          `json:"deviceEnergyJ,omitempty"`
	LimitW          float64          `json:"limitW,omitempty"`
	RequestedLimitW float64          `json:"requestedLimitW,omitempty"`
	SpeedHz         float64          `json:"speedHz,omitempty"`
	SpeedMaxHz      float64          `json:"speedMaxHz,omitempty"`
	SpeedLimitHz    float64          `json:"speedLimitHz,omitempty"`
	Utilization     float64          `json:"utilization"`
	ClockHz         map[string]int64 `json:"clockHz,omitempty"`
	ClockMaxHz      map[string]int64 `json:"clockMaxHz,omitempty"`
	ClockLimitHz    map[string]int64 `json:"clockLimitHz,omitempty"`
	GovernorNative  string           `json:"governorNative,omitempty"`
	GovernorPreset  string           `json:"governorPreset,omitempty"`
	PeerRxBytes     uint64           `json:"peerRxBytes,omitempty"`
	PeerTxBytes     uint64           `json:"peerTxBytes,omitempty"`
}

// The conversion the ComponentReading comment promises.
var _ = ComponentReading(hwemu.ComponentState{})

// Governor is the component's governor state.
func (c ComponentReading) Governor() GovernorState {
	return GovernorState{Native: c.GovernorNative, Preset: c.GovernorPreset}
}

// FacilitySnapshot is the facility after a tick: the IT power of every
// emulated host (and of legacy nodes, through the facility callback), the
// model of package facility, and the per-zone and per-rack readings. Energy
// counters are ledger values that start at StartTime and survive restarts.
type FacilitySnapshot struct {
	StartTime      time.Time     `json:"startTime"`
	AmbientC       float64       `json:"ambientC"`
	ITPowerW       float64       `json:"itPowerW"`
	ITEnergyJ      float64       `json:"itEnergyJ"`
	PUE            float64       `json:"pue"`
	CoolingPowerW  float64       `json:"coolingPowerW"`
	CoolingEnergyJ float64       `json:"coolingEnergyJ"`
	Zones          []ZoneReading `json:"zones,omitempty"`
	Racks          []RackReading `json:"racks,omitempty"`
}

// ZoneReading is one cooling zone: the facility ambient plus its offset.
type ZoneReading struct {
	Name     string  `json:"name"`
	AmbientC float64 `json:"ambientC"`
}

// RackReading is one rack: the sum of its hosts' power, and the energy its
// hosts' ledgers added.
type RackReading struct {
	Name    string  `json:"name"`
	PowerW  float64 `json:"powerW"`
	EnergyJ float64 `json:"energyJ"`
}
