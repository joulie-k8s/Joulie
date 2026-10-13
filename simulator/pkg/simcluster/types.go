package simcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Mode is a telemetry mode: how the emulated Nodes' power reaches
// Prometheus, and which Joulie input settings read it.
type Mode string

// The telemetry modes. ModeOTel is the default.
const (
	// ModeOTel: the simulator pushes OTLP straight to Prometheus, which
	// promotes k8s.node.name and host.name and uses the default name
	// translation. Joulie runs with its defaults.
	ModeOTel Mode = "otel"
	// ModeOTelUnpromoted: as ModeOTel, without promotion. Joulie's auto
	// resource-attribute mode falls back to joining target_info.
	ModeOTelUnpromoted Mode = "otel-unpromoted"
	// ModeOTelLabelsUnpromoted: as ModeOTelUnpromoted, with Joulie forced
	// to the labels form, so it reads no data and says why.
	ModeOTelLabelsUnpromoted Mode = "otel-labels-unpromoted"
	// ModeOTelUTF8: Prometheus and Joulie both translate names with
	// NoUTF8EscapingWithSuffixes.
	ModeOTelUTF8 Mode = "otel-utf8"
	// ModeOTelCollector: the simulator pushes OTLP to an OpenTelemetry
	// Collector, whose prometheus exporter Prometheus scrapes.
	ModeOTelCollector Mode = "otel-collector"
	// ModeKepler: the simulator also serves Kepler 0.12 series, and Joulie
	// reads them with its kepler preset.
	ModeKepler Mode = "kepler"
	// ModeKeplerLegacy: the simulator also serves Kepler 0.7 series, and
	// Joulie reads them with its kepler-legacy preset.
	ModeKeplerLegacy Mode = "kepler-legacy"
)

// Modes returns every mode, the default first.
func Modes() []Mode {
	return []Mode{ModeOTel, ModeOTelUnpromoted, ModeOTelLabelsUnpromoted, ModeOTelUTF8, ModeOTelCollector, ModeKepler, ModeKeplerLegacy}
}

// ParseMode returns the mode named s; an unknown name is an error that
// lists the modes.
func ParseMode(s string) (Mode, error) {
	modes := Modes()
	names := make([]string, len(modes))
	for i, m := range modes {
		if string(m) == s {
			return m, nil
		}
		names[i] = string(m)
	}
	return "", fmt.Errorf("unknown telemetry mode %q; modes are %s", s, strings.Join(names, ", "))
}

// Scope is the node power scope Joulie reads and the truth it is checked
// against.
type Scope string

// The power scopes. ScopeComponents is the default.
const (
	// ScopeComponents is the sum of the CPU packages and GPUs, the scope of
	// the twin's power budget.
	ScopeComponents Scope = "components"
	// ScopeHost is whole-node power (hw.host.*), which exceeds the budget
	// under load. The otel modes only.
	ScopeHost Scope = "host"
)

// Scopes returns every scope, the default first.
func Scopes() []Scope {
	return []Scope{ScopeComponents, ScopeHost}
}

// ParseScope returns the scope named s; an unknown name is an error that
// lists the scopes.
func ParseScope(s string) (Scope, error) {
	scopes := Scopes()
	names := make([]string, len(scopes))
	for i, sc := range scopes {
		if string(sc) == s {
			return sc, nil
		}
		names[i] = string(sc)
	}
	return "", fmt.Errorf("unknown power scope %q; scopes are %s", s, strings.Join(names, ", "))
}

// Timing names, the files scripts/simcluster/timing/<name>.env. TimingDev
// is the default; nightly runs use TimingProduction.
const (
	TimingDev        = "dev"
	TimingProduction = "production"
)

// Timing is one timing profile. Each field is one key of the timing file,
// mapped onto the real settings when values are generated:
//
//	ControllerReconcile  CONTROLLER_RECONCILE_INTERVAL  controller RECONCILE_INTERVAL
//	AgentReconcile       AGENT_RECONCILE_INTERVAL       agent RECONCILE_INTERVAL
//	SpecReadTimeout      AGENT_SPEC_READ_TIMEOUT        agent SPEC_READ_TIMEOUT
//	ScrapeInterval       PROM_SCRAPE_INTERVAL           Prometheus global scrape_interval
//	OTLPExportInterval   OTLP_EXPORT_INTERVAL_MS        simulator otel.exportIntervalMillis
//	RateWindow           RATE_WINDOW                    controller PROMETHEUS_RATE_WINDOW
//	FacilityPoll         FACILITY_POLL_INTERVAL         controller FACILITY_POLL_INTERVAL
//
// RateWindow must cover at least four export intervals, or rate() reads
// too few samples.
type Timing struct {
	Name                string
	ControllerReconcile time.Duration
	AgentReconcile      time.Duration
	SpecReadTimeout     time.Duration
	ScrapeInterval      time.Duration
	OTLPExportInterval  time.Duration
	RateWindow          time.Duration
	FacilityPoll        time.Duration
}

// Placement is what decides where the agent DaemonSet runs, read from its
// render: the pod's node selector and its own tolerations (the DaemonSet
// controller adds its automatic ones), whether it uses the host network,
// and the architecture of its image.
type Placement struct {
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	HostNetwork  bool
	ImageArch    string
}

// Exclusion is a spec Node the agent DaemonSet would not reach, and why,
// such as "taint example.com/dedicated=batch:NoSchedule".
type Exclusion struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// EmulatedAgent is one entry of the Joulie chart's agent.emulated.nodes: the
// Node an emulated agent stands in for, its hardware profile, whether its
// tree has a /dev/nvidiactl to mount (NVIDIA GPUs), and the extra volumes
// its pool's tool delivery adds. The JSON keys are the chart's.
type EmulatedAgent struct {
	Name              string               `json:"name"`
	Profile           string               `json:"profile"`
	DevNvidiactl      bool                 `json:"devNvidiactl,omitempty"`
	ExtraVolumes      []corev1.Volume      `json:"extraVolumes,omitempty"`
	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`
}

// How an emulated agent of a pool gets what the emulated GPU tools need
// before they run (their loader and libraries).
const (
	// ToolDeliveryMounted mounts a stub at the first path of each tool
	// requirement, so the tools run as on a node with the driver installed.
	// The default.
	ToolDeliveryMounted = "mounted"
	// ToolDeliveryNone mounts nothing, so the tools fail as on a node
	// without the driver, and the agent's GPU inventory fails with them.
	ToolDeliveryNone = "none"
)

// Harness is the spec.harness section of a cluster spec: per-pool settings
// of the harness, keyed by pool name.
type Harness struct {
	Pools map[string]HarnessPool `json:"pools,omitempty"`
}

// HarnessPool is the harness setting of one pool. An empty ToolDelivery
// means ToolDeliveryMounted.
type HarnessPool struct {
	ToolDelivery string `json:"toolDelivery,omitempty"`
}

// ParseHarness decodes a spec.harness section strictly: an unknown key or
// an unknown tool delivery is an error naming it. An empty or null section
// is the zero Harness. Whether each pool exists is the caller's check,
// since only the spec knows its pools.
func ParseHarness(raw json.RawMessage) (Harness, error) {
	var h Harness
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return h, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return Harness{}, fmt.Errorf("spec.harness: %w", err)
	}
	var errs []error
	for name, p := range h.Pools {
		switch p.ToolDelivery {
		case "", ToolDeliveryMounted, ToolDeliveryNone:
		default:
			errs = append(errs, fmt.Errorf("spec.harness.pools.%s.toolDelivery %q is not %s or %s", name, p.ToolDelivery, ToolDeliveryMounted, ToolDeliveryNone))
		}
	}
	if len(errs) > 0 {
		return Harness{}, errors.Join(errs...)
	}
	return h, nil
}

// ToolDelivery returns the tool delivery of pool, ToolDeliveryMounted when
// the pool is not listed or leaves it empty.
func (h Harness) ToolDelivery(pool string) string {
	if d := h.Pools[pool].ToolDelivery; d != "" {
		return d
	}
	return ToolDeliveryMounted
}

// The truths a Check compares with, each computed per Node from the
// emulator's snapshot.
const (
	// TruthPackagesGPUs is the sum of the CPU packages and every GPU.
	TruthPackagesGPUs = "packages+gpus"
	// TruthPackagesNVIDIAGPUs is the sum of the CPU packages and the NVIDIA
	// GPUs, the GPUs a Kepler release reports.
	TruthPackagesNVIDIAGPUs = "packages+nvidia-gpus"
	// TruthHost is whole-node power.
	TruthHost = "host"
)

// Check is one comparison of a PromQL query with a truth. It passes when
// the observed value is within Tolerance (relative) of the truth, or within
// AbsFloorW watts, whichever is larger; the floor keeps a zero truth
// checkable. A Check with a KnownDefect (D1, D15, ...) is expected to fail:
// it passing is an error, because the defect is gone and the Check must
// drop the KnownDefect.
type Check struct {
	ID          string  `json:"id"`
	Query       string  `json:"query"`
	Truth       string  `json:"truth"`
	Tolerance   float64 `json:"tolerance"`
	AbsFloorW   float64 `json:"absFloorW"`
	KnownDefect string  `json:"knownDefect,omitempty"`
}

// NodeTruth is one Node as the emulator saw it, from the simulator's
// /debug/emulate snapshot: power and limit in effect per CPU package and
// per GPU in index order, the GPU vendor, the summed DRAM power, the
// platform's other power, and whole-node power.
type NodeTruth struct {
	PackagesW      []float64 `json:"packagesW"`
	PackageCapW    []float64 `json:"packageCapW"`
	GPUsW          []float64 `json:"gpusW,omitempty"`
	GPULimitW      []float64 `json:"gpuLimitW,omitempty"`
	GPUVendor      string    `json:"gpuVendor,omitempty"`
	DRAMW          float64   `json:"dramW"`
	PlatformOtherW float64   `json:"platformOtherW"`
	HostW          float64   `json:"hostW"`
}

// FacilityTruth is the facility as the emulator saw it.
type FacilityTruth struct {
	AmbientC      float64 `json:"ambientC"`
	ITPowerW      float64 `json:"itPowerW"`
	CoolingPowerW float64 `json:"coolingPowerW"`
}
