package simcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
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
	// ModeOTelBridge: the simulator pushes no OTLP of its own and serves
	// the Kepler 0.12, dcgm-exporter 4.8.4 and AMD device-metrics-exporter
	// 1.5.2 formats instead; an OpenTelemetry Collector (contrib) runs the
	// bridge of examples/04-otel-collector-bridge, which scrapes them and
	// pushes hw.* over OTLP to Prometheus. Joulie runs with its defaults,
	// facility metrics off as the chart has them, since no facility series
	// reaches Prometheus in this mode.
	ModeOTelBridge Mode = "otel-bridge"
	// ModeKepler: the simulator also serves Kepler 0.12 series, and Joulie
	// reads them with its kepler preset.
	ModeKepler Mode = "kepler"
	// ModeKeplerLegacy: the simulator also serves Kepler 0.7 series, and
	// Joulie reads them with its kepler-legacy preset.
	ModeKeplerLegacy Mode = "kepler-legacy"
)

// Modes returns every mode, the default first.
func Modes() []Mode {
	return []Mode{ModeOTel, ModeOTelUnpromoted, ModeOTelLabelsUnpromoted, ModeOTelUTF8, ModeOTelCollector, ModeOTelBridge, ModeKepler, ModeKeplerLegacy}
}

// Strategy is the translation strategy Prometheus stores the OTLP names
// with in mode m, and Joulie reads them with: NoUTF8EscapingWithSuffixes in
// otel-utf8, the default otherwise.
func (m Mode) Strategy() promname.Strategy {
	if m == ModeOTelUTF8 {
		return promname.NoUTF8EscapingWithSuffixes
	}
	return promname.Default
}

// InputPreset is the node power preset Joulie runs with in mode m: the
// Kepler preset of a Kepler mode, the default otel preset otherwise.
func (m Mode) InputPreset() inputs.NodePowerPreset {
	switch m {
	case ModeKepler:
		return inputs.NodePowerPresetKepler
	case ModeKeplerLegacy:
		return inputs.NodePowerPresetKeplerLegacy
	}
	return inputs.NodePowerPresetOTel
}

// ModeForPreset is the mode a run with Joulie's node power preset needs: a
// Kepler preset reads series only its Kepler mode serves, and every other
// preset reads what the default mode exports. It inverts InputPreset.
func ModeForPreset(preset string) Mode {
	for _, m := range []Mode{ModeKepler, ModeKeplerLegacy} {
		if string(m.InputPreset()) == preset {
			return m
		}
	}
	return ModeOTel
}

// kepler reports whether m reads Kepler series.
func (m Mode) kepler() bool { return m == ModeKepler || m == ModeKeplerLegacy }

// scraped reports whether m's node power reaches Prometheus by a scrape,
// whose interval then bounds the rate window too.
func (m Mode) scraped() bool {
	return m.kepler() || m == ModeOTelCollector || m == ModeOTelBridge
}

// promoted reports whether Prometheus promotes the node attributes.
func (m Mode) promoted() bool {
	return m != ModeOTelUnpromoted && m != ModeOTelLabelsUnpromoted
}

// hostPower reports whether m carries whole-node power (hw.host.*), which
// scope host reads: the simulator's own OTLP does, the Kepler releases and
// the bridge do not.
func (m Mode) hostPower() bool { return !m.kepler() && m != ModeOTelBridge }

// Facility reports whether Joulie reads facility metrics in mode m
// (ENABLE_FACILITY_METRICS=true): they come from the simulator's own OTLP,
// which mode otel-bridge turns off, so there Joulie keeps the chart default,
// off, and every twin runs with the ambient unknown.
func (m Mode) Facility() bool { return m != ModeOTelBridge }

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
	// under load. The otel modes that carry it only, otel-bridge not.
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

// Placement is what the agent DaemonSet render says about where the agent
// runs and what it finds there: the pod's node selector and its own
// tolerations (the DaemonSet controller adds its automatic ones), whether it
// uses the host network, the architecture of its image, and how the GPU
// tools reach it: a runtime class (agent.runtimeClassName) and the volumes
// beyond the chart's own (agent.extraVolumes).
type Placement struct {
	NodeSelector     map[string]string
	Tolerations      []corev1.Toleration
	HostNetwork      bool
	ImageArch        string
	RuntimeClassName string
	ExtraVolumes     []string
}

// ToolDelivery is the tool delivery of a pool the spec says nothing about:
// what the install gives the agent on a real node. The agent image ships no
// GPU tools, so they reach it only through a runtime class or an extra
// volume; with neither, the default install, the agent's NVIDIA GPU
// inventory fails (AMD GPUs need no tool), and so the emulated agent gets
// ToolDeliveryNone.
func (p Placement) ToolDelivery() string {
	if p.RuntimeClassName != "" || len(p.ExtraVolumes) > 0 {
		return ToolDeliveryMounted
	}
	return ToolDeliveryNone
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
	ToolDeliveryMounted = "mounted"
	// ToolDeliveryNone mounts nothing, so the tools fail as on a node
	// without the driver, and the agent's NVIDIA GPU inventory fails with
	// them; AMD GPUs need no tool.
	ToolDeliveryNone = "none"
)

// Harness is the spec.harness section of a cluster spec: per-pool settings
// of the harness, keyed by pool name.
type Harness struct {
	Pools map[string]HarnessPool `json:"pools,omitempty"`
}

// HarnessPool is the harness setting of one pool. An empty ToolDelivery
// follows the install (Placement.ToolDelivery).
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

// ToolDelivery returns the tool delivery of pool: its own setting, or
// install, the tool delivery of the Joulie install (Placement.ToolDelivery),
// when the pool is not listed or leaves it empty.
func (h Harness) ToolDelivery(pool, install string) string {
	if d := h.Pools[pool].ToolDelivery; d != "" {
		return d
	}
	return install
}

// The truths a Check compares with, each computed per Node from the
// emulator's snapshot.
const (
	// TruthPackagesGPUs is the sum of the CPU packages and every GPU.
	TruthPackagesGPUs = "packages+gpus"
	// TruthPackagesNVIDIAGPUs is the sum of the CPU packages and the NVIDIA
	// GPUs, the GPUs a Kepler release reports.
	TruthPackagesNVIDIAGPUs = "packages+nvidia-gpus"
	// TruthPackagesNVIDIAAMDGPUs is the sum of the CPU packages and the
	// NVIDIA and AMD GPUs, the GPUs the otel-bridge Collector scrapes
	// (dcgm-exporter, AMD device-metrics-exporter); an Intel GPU has no
	// exporter there.
	TruthPackagesNVIDIAAMDGPUs = "packages+nvidia-amd-gpus"
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
// platform's other power, and whole-node power. In mode kepler-legacy the
// caller adds, from the exporter rather than the snapshot, the idle power
// Kepler 0.7 reports beyond the measured one, for its package and GPU
// series and for its platform series (CheckSet.Kepler07Excess).
type NodeTruth struct {
	PackagesW               []float64 `json:"packagesW"`
	PackageCapW             []float64 `json:"packageCapW"`
	GPUsW                   []float64 `json:"gpusW,omitempty"`
	GPULimitW               []float64 `json:"gpuLimitW,omitempty"`
	GPUVendor               string    `json:"gpuVendor,omitempty"`
	DRAMW                   float64   `json:"dramW"`
	PlatformOtherW          float64   `json:"platformOtherW"`
	HostW                   float64   `json:"hostW"`
	Kepler07ExcessW         float64   `json:"kepler07ExcessW,omitempty"`
	Kepler07PlatformExcessW float64   `json:"kepler07PlatformExcessW,omitempty"`
}
