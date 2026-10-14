package simcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	goyaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/compat"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

// The parts Telemetry renders, the values of the simcluster command's
// --part flag.
const (
	// PartPromConfig is Prometheus' configuration file.
	PartPromConfig = "prom-config"
	// PartOtelcolConfig is the Collector's configuration file, modes
	// otel-collector and otel-bridge only.
	PartOtelcolConfig = "otelcol-config"
	// PartSimValues is a values file of the joulie-simulator chart.
	PartSimValues = "sim-values"
	// PartJoulieValues is a values file of the joulie chart.
	PartJoulieValues = "joulie-values"
	// PartChecks is the CheckSet, as JSON.
	PartChecks = "checks"
	// PartWait is the Wait, as one line of JSON.
	PartWait = "wait"
)

// Parts returns every part, in the order the simcluster command lists them.
func Parts() []string {
	return []string{PartPromConfig, PartOtelcolConfig, PartSimValues, PartJoulieValues, PartChecks, PartWait}
}

// The Services of the simulated cluster the generated files point at
// (scripts/simcluster/lib.sh and manifests).
const (
	telPrometheusOTLP  = "http://prometheus-operated.monitoring:9090/api/v1/otlp/v1/metrics"
	telOtelcolOTLP     = "http://otelcol.monitoring:4318/v1/metrics"
	telOtelcolScrape   = "otelcol.monitoring:8889"
	telSimulatorTarget = "joulie-telemetry-sim.joulie-sim:80"
	telJoulieNamespace = "joulie-system"
)

// telJob is the job label of every series the simulator pushes:
// service.namespace/service.name.
const telJob = telemetry.ServiceNamespace + "/" + telemetry.DefaultServiceName

// telBridgeJob is one scrape job of the bridge of
// examples/11-otel-collector-bridge (collector.yaml): its name, which
// becomes the job label of the series it pushes, the compatibility
// exporter of the simulator that serves its format, and the series it
// keeps, as the example writes them.
type telBridgeJob struct {
	name, exporter string
	keep           promRelabel
}

// telBridgeJobs are the bridge's scrape jobs; the Kepler one carries the
// power of the CPU package zones, the others the GPUs.
var telBridgeJobs = []telBridgeJob{
	{"kepler", compat.Kepler012, promRelabel{SourceLabels: []string{"__name__", inputs.KeplerZoneLabel}, Regex: inputs.KeplerCPUWatts + ";" + inputs.KeplerPackageZones, Action: "keep"}},
	{"dcgm-exporter", compat.DCGM484, promRelabel{SourceLabels: []string{"__name__"}, Regex: "DCGM_FI_DEV_(TOTAL_ENERGY_CONSUMPTION|POWER_USAGE)", Action: "keep"}},
	{"amd-device-metrics-exporter", compat.AMDDME152, promRelabel{SourceLabels: []string{"__name__"}, Regex: "gpu_(energy_consumed|power_usage)", Action: "keep"}},
}

// Jobs returns the job label values of the node power series of mode m:
// the simulator's own in the otel modes, the bridge's scrape jobs in mode
// otel-bridge, the compatibility exporter's scrape job in a Kepler mode.
func (m Mode) Jobs() []string {
	switch {
	case m == ModeOTelBridge:
		out := make([]string, len(telBridgeJobs))
		for i, j := range telBridgeJobs {
			out[i] = j.name
		}
		return out
	case m.kepler():
		return telExporters(m)
	}
	return []string{telJob}
}

// telJobMatcher is the matcher of the job label that selects mode m's node
// power series.
func telJobMatcher(m Mode) [3]string {
	jobs := m.Jobs()
	if len(jobs) == 1 {
		return [3]string{promname.LabelJob, "=", jobs[0]}
	}
	return [3]string{promname.LabelJob, "=~", strings.Join(jobs, "|")}
}

// telKepler012GPUEnv makes the simulator's kepler-0.12 exporter serve the
// kepler_node_gpu_* series, as Kepler's experimental GPU feature does. The
// modes that serve kepler-0.12 turn it on: the kepler preset reads the GPUs
// from it, and the bridge drops them, as it must on such an install.
const telKepler012GPUEnv = "SIM_KEPLER012_GPU"

// telMinRateIntervals is how many samples a rate() window must cover.
const telMinRateIntervals = 4

// telOutOfOrderWindow is Prometheus' out-of-order window, which lets a
// restarted simulator push samples Prometheus has already passed.
const telOutOfOrderWindow = 30 * time.Minute

// CheckSet is the checks part: the Checks of a mode and scope, the label
// that names the Node in every series their queries return, and the truth
// Joulie's measured node power must equal (empty when the mode leaves
// Joulie without data, as otel-labels-unpromoted does).
type CheckSet struct {
	NodeLabel   string  `json:"nodeLabel"`
	JoulieTruth string  `json:"joulieTruth"`
	Checks      []Check `json:"checks"`
}

// Wait is the wait part: Query returns one series per Node that has
// reported, Label holds the Node's name, and Nodes are the Nodes that must
// report (WaitNodes).
type Wait struct {
	Query string   `json:"query"`
	Label string   `json:"label"`
	Nodes []string `json:"nodes"`
}

// TelemetryParts is everything one mode, scope and timing set up.
// OtelcolConfig is nil outside the Collector modes, and Wait.Nodes is empty
// until the caller sets it from the spec.
type TelemetryParts struct {
	Mode          Mode
	Scope         Scope
	Timing        Timing
	PromConfig    []byte
	OtelcolConfig []byte
	SimValues     []byte
	JoulieValues  []byte
	Checks        CheckSet
	Wait          Wait
}

// Telemetry renders the parts of a telemetry mode, a power scope and a
// timing. Every metric and label name in a query is promname's spelling of
// a semconv entry under the mode's translation strategy, or a pinned Kepler
// name.
//
//   - prom-config: the timing's scrape interval, a 30 minute out-of-order
//     window, the OTLP receiver's promotion of k8s.node.name and host.name
//     (none in the two unpromoted modes) and translation strategy
//     (Mode.Strategy), a scrape of the Joulie Services that carry
//     prometheus.io/scrape, and per mode a scrape of the Collector
//     (honor_labels true) or of the simulator's Kepler exporter
//     (honor_labels false).
//   - otelcol-config: in mode otel-collector, an OTLP receiver and a
//     prometheus exporter that puts k8s.node.name and host.name on every
//     series; in mode otel-bridge, the file the Collector reads after
//     examples/11-otel-collector-bridge/collector.yaml: the bridge's scrape
//     jobs pointed at the simulator's compatibility exporters at the
//     timing's scrape interval.
//   - sim-values: the OTLP endpoint (Prometheus, the Collector, or none in
//     mode otel-bridge), the export interval and the compatibility
//     exporters.
//   - joulie-values: the timing on the controller manager and the agent,
//     NODE_POWER_SOURCE=prometheus, ENABLE_FACILITY_METRICS=true where the
//     mode carries facility metrics (Mode.Facility), and the input settings
//     of the mode and scope beyond Joulie's defaults.
//   - checks and wait: see CheckSet and Wait.
//
// Scope host needs a mode that carries whole-node power: Joulie reads no
// host power from Kepler 0.12 or from the bridge, and mode kepler-legacy
// checks Kepler 0.7's platform power against the host truth under scope
// components already.
func Telemetry(mode Mode, scope Scope, timing Timing) (TelemetryParts, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return TelemetryParts{}, err
	}
	if _, err := ParseScope(string(scope)); err != nil {
		return TelemetryParts{}, err
	}
	if scope == ScopeHost && !mode.hostPower() {
		return TelemetryParts{}, fmt.Errorf("power scope %s needs an otel mode that carries whole-node power, not %s", scope, mode)
	}
	if err := timing.validate(mode.scraped()); err != nil {
		return TelemetryParts{}, err
	}
	window, err := inputs.PromDuration(timing.RateWindow)
	if err != nil {
		return TelemetryParts{}, fmt.Errorf("timing %s: %w", timing.Name, err)
	}
	t := TelemetryParts{Mode: mode, Scope: scope, Timing: timing}
	head := func(part string) string {
		return fmt.Sprintf("# Generated by simcluster telemetry --part %s --mode %s --scope %s --timing %s.\n", part, mode, scope, timing.Name)
	}
	if t.PromConfig, err = telYAML(head(PartPromConfig), telPromConfig(mode, timing)); err != nil {
		return TelemetryParts{}, err
	}
	switch mode {
	case ModeOTelCollector:
		if t.OtelcolConfig, err = telYAML(head(PartOtelcolConfig), telOtelcolConfig()); err != nil {
			return TelemetryParts{}, err
		}
	case ModeOTelBridge:
		if t.OtelcolConfig, err = telYAML(head(PartOtelcolConfig), telBridgeConfig(timing)); err != nil {
			return TelemetryParts{}, err
		}
	}
	if t.SimValues, err = telYAML(head(PartSimValues), telSimValues(mode, timing)); err != nil {
		return TelemetryParts{}, err
	}
	if t.JoulieValues, err = telYAML(head(PartJoulieValues), telJoulieValues(mode, scope, timing)); err != nil {
		return TelemetryParts{}, err
	}
	if t.Checks, err = telChecks(mode, scope, timing.RateWindow, window); err != nil {
		return TelemetryParts{}, err
	}
	t.Wait = telWait(mode)
	return t, nil
}

// Part returns one part as the simcluster command prints it: a YAML file,
// the CheckSet as indented JSON, or the Wait as one line of JSON.
func (t TelemetryParts) Part(name string) ([]byte, error) {
	switch name {
	case PartPromConfig:
		return t.PromConfig, nil
	case PartOtelcolConfig:
		if t.OtelcolConfig == nil {
			return nil, fmt.Errorf("mode %s runs no Collector; %s is for modes %s and %s", t.Mode, PartOtelcolConfig, ModeOTelCollector, ModeOTelBridge)
		}
		return t.OtelcolConfig, nil
	case PartSimValues:
		return t.SimValues, nil
	case PartJoulieValues:
		return t.JoulieValues, nil
	case PartChecks:
		b, err := json.MarshalIndent(t.Checks, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	case PartWait:
		w := t.Wait
		if w.Nodes == nil {
			w.Nodes = []string{}
		}
		b, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
	return nil, fmt.Errorf("unknown part %q; parts are %s", name, strings.Join(Parts(), ", "))
}

// WaitNodes returns the members whose CPU packages are measured, sorted:
// the Nodes whose series the wait query must return. A hypervisor guest or
// a node without RAPL package zones exports no hw.* metric and no Kepler
// package series.
func WaitNodes(members []clusterspec.Member) []string {
	out := []string{}
	for _, m := range members {
		if m.Profile != nil && measuredCPU(m.Profile) {
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out
}

// measuredCPU reports whether a profile has a measured CPU package: a
// powercap zone named package-<n> or package-<n>-die-<m>, every one of
// which has energy_uj (hwemu renderZone), as hwemu's ComponentState
// Measured has it.
func measuredCPU(p *hwemu.Profile) bool {
	if p.Powercap == nil {
		return false
	}
	for _, ct := range p.Powercap.ControlTypes {
		for _, z := range ct.Zones {
			if telPackageZone.MatchString(strings.ReplaceAll(z.NameFormat, "%d", "0")) {
				return true
			}
		}
	}
	return false
}

var telPackageZone = regexp.MustCompile(`^package-[0-9]+(-die-[0-9]+)?$`)

// telExporters are the compatibility exporters mode serves.
func telExporters(mode Mode) []string {
	switch mode {
	case ModeKepler:
		return []string{compat.Kepler012}
	case ModeKeplerLegacy:
		return []string{compat.Kepler07}
	case ModeOTelBridge:
		out := make([]string, len(telBridgeJobs))
		for i, j := range telBridgeJobs {
			out[i] = j.exporter
		}
		return out
	}
	return []string{}
}

// telDuration writes d as Prometheus and Go both read it: whole hours,
// minutes or seconds, else milliseconds.
func telDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}

// telYAML writes v as YAML with two-space indents after head.
func telYAML(head string, v any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(head)
	enc := goyaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Prometheus configuration, the keys Prometheus v3.15.0 reads.
type promConfig struct {
	Global        promGlobal   `yaml:"global"`
	Storage       promStorage  `yaml:"storage"`
	OTLP          promOTLP     `yaml:"otlp"`
	ScrapeConfigs []promScrape `yaml:"scrape_configs"`
}

type promGlobal struct {
	ScrapeInterval string `yaml:"scrape_interval"`
}

type promStorage struct {
	TSDB promTSDB `yaml:"tsdb"`
}

type promTSDB struct {
	OutOfOrderTimeWindow string `yaml:"out_of_order_time_window"`
}

type promOTLP struct {
	PromoteResourceAttributes []string `yaml:"promote_resource_attributes,flow"`
	TranslationStrategy       string   `yaml:"translation_strategy"`
}

type promScrape struct {
	JobName              string        `yaml:"job_name"`
	ScrapeInterval       string        `yaml:"scrape_interval,omitempty"`
	HonorLabels          *bool         `yaml:"honor_labels,omitempty"`
	MetricsPath          string        `yaml:"metrics_path,omitempty"`
	StaticConfigs        []promStatic  `yaml:"static_configs,omitempty"`
	KubernetesSDConfigs  []promKubeSD  `yaml:"kubernetes_sd_configs,omitempty"`
	RelabelConfigs       []promRelabel `yaml:"relabel_configs,omitempty"`
	MetricRelabelConfigs []promRelabel `yaml:"metric_relabel_configs,omitempty"`
}

type promStatic struct {
	Targets []string `yaml:"targets,flow"`
}

type promKubeSD struct {
	Role       string         `yaml:"role"`
	Namespaces promNamespaces `yaml:"namespaces"`
}

type promNamespaces struct {
	Names []string `yaml:"names,flow"`
}

type promRelabel struct {
	SourceLabels []string `yaml:"source_labels,flow,omitempty"`
	Regex        string   `yaml:"regex,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
	TargetLabel  string   `yaml:"target_label,omitempty"`
	Action       string   `yaml:"action,omitempty"`
}

func telPromConfig(mode Mode, timing Timing) promConfig {
	c := promConfig{
		Global:  promGlobal{ScrapeInterval: telDuration(timing.ScrapeInterval)},
		Storage: promStorage{TSDB: promTSDB{OutOfOrderTimeWindow: telDuration(telOutOfOrderWindow)}},
		OTLP: promOTLP{
			PromoteResourceAttributes: []string{},
			TranslationStrategy:       string(mode.Strategy()),
		},
	}
	if mode.promoted() {
		c.OTLP.PromoteResourceAttributes = []string{semconv.AttrK8sNodeName, semconv.AttrHostName}
	}
	// The Joulie Services that ask for a scrape (the chart's
	// prometheus.io annotations), as a Prometheus Helm chart scrapes them.
	c.ScrapeConfigs = append(c.ScrapeConfigs, promScrape{
		JobName:             "joulie",
		KubernetesSDConfigs: []promKubeSD{{Role: "endpointslice", Namespaces: promNamespaces{Names: []string{telJoulieNamespace}}}},
		RelabelConfigs: []promRelabel{
			{SourceLabels: []string{"__meta_kubernetes_service_annotation_prometheus_io_scrape"}, Regex: "true", Action: "keep"},
			{SourceLabels: []string{"__meta_kubernetes_service_annotation_prometheus_io_path"}, Regex: "(.+)", TargetLabel: "__metrics_path__"},
			{SourceLabels: []string{"__address__", "__meta_kubernetes_service_annotation_prometheus_io_port"}, Regex: `([^:]+)(?::\d+)?;(\d+)`, Replacement: "$1:$2", TargetLabel: "__address__"},
			{SourceLabels: []string{"__meta_kubernetes_namespace"}, TargetLabel: "namespace"},
			{SourceLabels: []string{"__meta_kubernetes_service_name"}, TargetLabel: "service"},
			{SourceLabels: []string{"__meta_kubernetes_pod_name"}, TargetLabel: "pod"},
		},
	})
	switch {
	case mode == ModeOTelCollector:
		// The Collector's exporter sets job and instance from each
		// resource, as the OTLP receiver would.
		honor := true
		c.ScrapeConfigs = append(c.ScrapeConfigs, promScrape{
			JobName:       "otelcol",
			HonorLabels:   &honor,
			StaticConfigs: []promStatic{{Targets: []string{telOtelcolScrape}}},
		})
	case mode.kepler():
		// honor_labels false: Kepler 0.7's instance label becomes
		// exported_instance, as on a real install.
		honor := false
		exporter := telExporters(mode)[0]
		c.ScrapeConfigs = append(c.ScrapeConfigs, promScrape{
			JobName:       exporter,
			HonorLabels:   &honor,
			MetricsPath:   compat.Path(exporter),
			StaticConfigs: []promStatic{{Targets: []string{telSimulatorTarget}}},
		})
	}
	return c
}

// OpenTelemetry Collector configuration, core distribution v0.162.0.
type otelcolConfig struct {
	Receivers struct {
		OTLP struct {
			Protocols struct {
				HTTP struct {
					Endpoint string `yaml:"endpoint"`
				} `yaml:"http"`
			} `yaml:"protocols"`
		} `yaml:"otlp"`
	} `yaml:"receivers"`
	Exporters struct {
		Prometheus struct {
			Endpoint               string `yaml:"endpoint"`
			ResourceConstantLabels struct {
				Included []string `yaml:"included,flow"`
			} `yaml:"resource_constant_labels"`
		} `yaml:"prometheus"`
	} `yaml:"exporters"`
	Service struct {
		Pipelines struct {
			Metrics struct {
				Receivers []string `yaml:"receivers,flow"`
				Exporters []string `yaml:"exporters,flow"`
			} `yaml:"metrics"`
		} `yaml:"pipelines"`
	} `yaml:"service"`
}

func telOtelcolConfig() otelcolConfig {
	var c otelcolConfig
	c.Receivers.OTLP.Protocols.HTTP.Endpoint = "0.0.0.0:4318"
	c.Exporters.Prometheus.Endpoint = "0.0.0.0:8889"
	c.Exporters.Prometheus.ResourceConstantLabels.Included = []string{semconv.AttrK8sNodeName, semconv.AttrHostName}
	c.Service.Pipelines.Metrics.Receivers = []string{"otlp"}
	c.Service.Pipelines.Metrics.Exporters = []string{"prometheus"}
	return c
}

// The bridge's Collector configuration: what the harness changes in
// examples/11-otel-collector-bridge/collector.yaml, and the health check
// the readiness probe of the bridge's Deployment reads. The Collector
// merges this file over the example and replaces a list whole, so each job
// repeats the example's metric_relabel_configs.
type bridgeConfig struct {
	Receivers struct {
		Prometheus struct {
			Config struct {
				ScrapeConfigs []promScrape `yaml:"scrape_configs"`
			} `yaml:"config"`
		} `yaml:"prometheus"`
	} `yaml:"receivers"`
	Extensions struct {
		HealthCheck struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"health_check"`
	} `yaml:"extensions"`
	Service struct {
		Extensions []string `yaml:"extensions,flow"`
	} `yaml:"service"`
}

// telBridgeHealth is where the bridge Collector answers its health check
// (scripts/simcluster/manifests/otelcol-bridge.yaml probes it).
const telBridgeHealth = "0.0.0.0:13133"

// telBridgeConfig points the bridge's scrape jobs at the simulator's
// compatibility exporters, in place of the example's pod discovery, at the
// timing's scrape interval, and turns on the health check.
func telBridgeConfig(timing Timing) bridgeConfig {
	var c bridgeConfig
	c.Extensions.HealthCheck.Endpoint = telBridgeHealth
	c.Service.Extensions = []string{"health_check"}
	for _, j := range telBridgeJobs {
		c.Receivers.Prometheus.Config.ScrapeConfigs = append(c.Receivers.Prometheus.Config.ScrapeConfigs, promScrape{
			JobName:              j.name,
			ScrapeInterval:       telDuration(timing.ScrapeInterval),
			MetricsPath:          compat.Path(j.exporter),
			StaticConfigs:        []promStatic{{Targets: []string{telSimulatorTarget}}},
			MetricRelabelConfigs: []promRelabel{j.keep},
		})
	}
	return c
}

func telSimValues(mode Mode, timing Timing) map[string]any {
	var endpoint string
	switch mode {
	case ModeOTelCollector:
		endpoint = telOtelcolOTLP
	case ModeOTelBridge:
		// The bridge alone feeds Prometheus.
	default:
		endpoint = telPrometheusOTLP
	}
	v := map[string]any{
		"otel": map[string]any{
			"metricsEndpoint":      endpoint,
			"exportIntervalMillis": timing.OTLPExportInterval.Milliseconds(),
		},
		"emulate": map[string]any{"exporters": telExporters(mode)},
	}
	for _, e := range telExporters(mode) {
		if e == compat.Kepler012 {
			v["extraEnv"] = map[string]any{telKepler012GPUEnv: "true"}
		}
	}
	return v
}

func telJoulieValues(mode Mode, scope Scope, timing Timing) map[string]any {
	cm := map[string]string{
		"RECONCILE_INTERVAL":     telDuration(timing.ControllerReconcile),
		"NODE_POWER_SOURCE":      "prometheus",
		"PROMETHEUS_RATE_WINDOW": telDuration(timing.RateWindow),
	}
	if mode.Facility() {
		cm["ENABLE_FACILITY_METRICS"] = "true"
		cm["FACILITY_POLL_INTERVAL"] = telDuration(timing.FacilityPoll)
	}
	if mode == ModeOTelLabelsUnpromoted {
		cm["PROMETHEUS_RESOURCE_ATTRIBUTES"] = string(inputs.ResourceAttributesLabels)
	}
	if s := mode.Strategy(); s != promname.Default {
		cm["PROMETHEUS_NAME_TRANSLATION"] = string(s)
	}
	if p := mode.InputPreset(); p != inputs.NodePowerPresetOTel {
		cm["NODE_POWER_PRESET"] = string(p)
	}
	if scope == ScopeHost {
		cm["NODE_POWER_SCOPE"] = string(inputs.ScopeHost)
	}
	agent := map[string]string{
		"RECONCILE_INTERVAL": telDuration(timing.AgentReconcile),
		"SPEC_READ_TIMEOUT":  telDuration(timing.SpecReadTimeout),
	}
	return map[string]any{
		"controllerManager": map[string]any{"env": cm},
		"agent":             map[string]any{"env": agent},
	}
}

// telNames is the Prometheus spelling of what the otel checks read.
type telNames struct {
	energy, power, hostEnergy string
	node, hwType, hwID        string
}

func telNamesFor(s promname.Strategy) telNames {
	return telNames{
		energy:     s.MetricName(semconv.HwEnergy),
		power:      s.MetricName(semconv.HwPower),
		hostEnergy: s.MetricName(semconv.HwHostEnergy),
		node:       s.LabelName(semconv.AttrK8sNodeName),
		hwType:     s.LabelName(semconv.AttrHwType),
		hwID:       s.LabelName(semconv.AttrHwID),
	}
}

var telLegacyName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// telLabel writes a label name as PromQL needs it: quoted unless it is a
// legacy name.
func telLabel(name string) string {
	if telLegacyName.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}

// telLabels writes the parenthesised label list of by, on and group_left.
func telLabels(names ...string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = telLabel(n)
	}
	return "(" + strings.Join(out, ", ") + ")"
}

// telSelector writes a selector of metric with the matchers, each
// {label, op, value}; a metric name that is not a legacy name goes inside
// the braces, quoted.
func telSelector(metric string, matchers ...[3]string) string {
	var parts []string
	quoted := !telLegacyName.MatchString(metric)
	if quoted {
		parts = append(parts, strconv.Quote(metric))
	}
	for _, m := range matchers {
		parts = append(parts, telLabel(m[0])+m[1]+strconv.Quote(m[2]))
	}
	switch {
	case quoted:
		return "{" + strings.Join(parts, ", ") + "}"
	case len(parts) == 0:
		return metric
	}
	return metric + "{" + strings.Join(parts, ", ") + "}"
}

func telRate(sel, window string) string { return fmt.Sprintf("rate(%s[%s])", sel, window) }

// telJoin attaches the node label from target_info to x by job and
// instance, target_info of the jobs job matches. The max by collapses
// target_info series of one target that differ in another attribute, as
// after a simulator restart with a new service.version.
func telJoin(x, node string, job [3]string) string {
	return fmt.Sprintf("%s * on %s group_left %s max by %s (%s)",
		x, telLabels(promname.LabelJob, promname.LabelInstance), telLabels(node),
		telLabels(promname.LabelJob, promname.LabelInstance, node), telSelector(promname.TargetInfo, job))
}

// telOTelComponents is Σ cpu packages + Σ gpus per node from the energy
// counters of the jobs job matches, each component once.
func telOTelComponents(n telNames, window string, job [3]string, join bool) string {
	x := telRate(telSelector(n.energy,
		[3]string{n.hwType, "=~", semconv.HwTypeCPU + "|" + semconv.HwTypeGPU}, job), window)
	if join {
		x = telJoin(x, n.node, job)
	}
	return fmt.Sprintf("sum by %s (max by %s (%s))", telLabels(n.node), telLabels(n.node, n.hwType, n.hwID), x)
}

// telOTelHost is whole-node power per node from the host energy counter.
func telOTelHost(n telNames, window string, job [3]string, join bool) string {
	x := telRate(telSelector(n.hostEnergy, job), window)
	if join {
		x = telJoin(x, n.node, job)
	}
	return fmt.Sprintf("max by %s (%s)", telLabels(n.node), x)
}

// telPresetQuery is the node power query Joulie runs with spec: the check
// of a mode whose series only Joulie's own preset reads right, such as the
// Kepler counters that wrap or the bridge's CPU power gauges.
func telPresetQuery(spec inputs.NodePowerSpec) (string, error) {
	q, _, err := spec.Query()
	if err != nil {
		return "", fmt.Errorf("node power preset %s: %w", spec.Preset, err)
	}
	return q.PromQL, nil
}

func telCheck(id, query, truth string) Check {
	return Check{ID: id, Query: query, Truth: truth, Tolerance: CheckTolerance, AbsFloorW: CheckAbsFloorW}
}

// telChecks is the CheckSet of a mode and scope over window. An otel mode
// checks both scopes where it carries whole-node power, otherwise the
// components only, Joulie's form of each query first (the labels form
// where Prometheus promotes, else the target_info join) and, where it
// promotes, the target_info join as well. Mode otel-bridge checks Joulie's
// otel preset itself, in both forms: the bridge carries the CPU packages as
// power gauges, which only the preset's fallback to hw.power reads. Kepler
// modes check the components with the Kepler preset Joulie runs;
// kepler-legacy also checks the data shape of E07c: two platform series per
// node (idle and dynamic) summing to the host power.
func telChecks(mode Mode, scope Scope, rateWindow time.Duration, window string) (CheckSet, error) {
	switch mode {
	case ModeKepler:
		q, err := telPresetQuery(inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKepler, Window: rateWindow})
		if err != nil {
			return CheckSet{}, err
		}
		return CheckSet{NodeLabel: inputs.KeplerNodeLabel, JoulieTruth: TruthPackagesNVIDIAGPUs, Checks: []Check{
			telCheck("kepler-0.12-components", q, TruthPackagesNVIDIAGPUs),
		}}, nil
	case ModeKeplerLegacy:
		q, err := telPresetQuery(inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKeplerLegacy, Window: rateWindow})
		if err != nil {
			return CheckSet{}, err
		}
		node := inputs.KeplerLegacyNodeLabel
		platform := telRate(inputs.KeplerLegacyPlatformJoules, window)
		series := telCheck("kepler-0.7-platform-series", fmt.Sprintf("count by %s (%s)", telLabels(node), platform), TruthKepler07PlatformSeries)
		series.Tolerance, series.AbsFloorW = 0, 0
		return CheckSet{NodeLabel: node, JoulieTruth: TruthPackagesNVIDIAGPUs, Checks: []Check{
			telCheck("kepler-0.7-components", q, TruthPackagesNVIDIAGPUs),
			series,
			telCheck("kepler-0.7-platform-sum", fmt.Sprintf("sum by %s (%s)", telLabels(node), platform), TruthKepler07Host),
		}}, nil
	case ModeOTelBridge:
		n := telNamesFor(mode.Strategy())
		set := CheckSet{NodeLabel: n.node, JoulieTruth: TruthPackagesGPUs}
		for _, f := range []struct {
			form  string
			attrs inputs.ResourceAttributes
		}{{"labels", inputs.ResourceAttributesLabels}, {"target-info", inputs.ResourceAttributesTargetInfo}} {
			q, err := telPresetQuery(inputs.NodePowerSpec{Preset: inputs.NodePowerPresetOTel, Naming: mode.Strategy(), ResourceAttributes: f.attrs, Window: rateWindow})
			if err != nil {
				return CheckSet{}, err
			}
			set.Checks = append(set.Checks, telCheck("otel-components-"+f.form, q, TruthPackagesGPUs))
		}
		return set, nil
	}
	n := telNamesFor(mode.Strategy())
	set := CheckSet{NodeLabel: n.node, JoulieTruth: TruthPackagesGPUs}
	if scope == ScopeHost {
		set.JoulieTruth = TruthHost
	}
	if mode == ModeOTelLabelsUnpromoted {
		set.JoulieTruth = ""
	}
	type form struct {
		scope Scope
		truth string
		query func(telNames, string, [3]string, bool) string
	}
	forms := []form{{ScopeComponents, TruthPackagesGPUs, telOTelComponents}}
	if mode.hostPower() {
		forms = append(forms, form{ScopeHost, TruthHost, telOTelHost})
	}
	if scope == ScopeHost {
		forms[0], forms[1] = forms[1], forms[0]
	}
	job := telJobMatcher(mode)
	for _, f := range forms {
		if mode.promoted() {
			set.Checks = append(set.Checks, telCheck("otel-"+string(f.scope)+"-labels", f.query(n, window, job, false), f.truth))
		}
		set.Checks = append(set.Checks, telCheck("otel-"+string(f.scope)+"-target-info", f.query(n, window, job, true), f.truth))
	}
	return set, nil
}

// telWait is the wait query of mode: every node with a measured CPU
// package has a series, keyed by the label that carries its name.
func telWait(mode Mode) Wait {
	switch mode {
	case ModeKepler:
		return Wait{Query: fmt.Sprintf("count by %s (%s)", telLabels(inputs.KeplerNodeLabel), inputs.KeplerCPUJoules), Label: inputs.KeplerNodeLabel}
	case ModeKeplerLegacy:
		return Wait{Query: fmt.Sprintf("count by %s (%s)", telLabels(inputs.KeplerLegacyNodeLabel), inputs.KeplerLegacyPackageJoules), Label: inputs.KeplerLegacyNodeLabel}
	}
	// The CPU packages come from the first job: the simulator's energy
	// counters, or the power gauges of the bridge's Kepler job.
	n := telNamesFor(mode.Strategy())
	metric := n.energy
	if mode == ModeOTelBridge {
		metric = n.power
	}
	sel := telSelector(metric, [3]string{n.hwType, "=", semconv.HwTypeCPU}, [3]string{promname.LabelJob, "=", mode.Jobs()[0]})
	return Wait{Query: fmt.Sprintf("count by %s (%s)", telLabels(promname.LabelInstance), sel), Label: promname.LabelInstance}
}

// The keys of a timing file, the harness names of the settings.
const (
	timingControllerReconcile = "CONTROLLER_RECONCILE_INTERVAL"
	timingAgentReconcile      = "AGENT_RECONCILE_INTERVAL"
	timingSpecReadTimeout     = "AGENT_SPEC_READ_TIMEOUT"
	timingScrapeInterval      = "PROM_SCRAPE_INTERVAL"
	timingOTLPExportMS        = "OTLP_EXPORT_INTERVAL_MS"
	timingRateWindow          = "RATE_WINDOW"
	timingFacilityPoll        = "FACILITY_POLL_INTERVAL"
)

var timingName = regexp.MustCompile(`^[a-z0-9-]+$`)

// TimingFile is the file of timing name under the repository root,
// scripts/simcluster/timing/<name>.env.
func TimingFile(repoRoot, name string) string {
	return filepath.Join(repoRoot, "scripts", "simcluster", "timing", name+".env")
}

// ReadTiming reads timing name from TimingFile(repoRoot, name).
func ReadTiming(repoRoot, name string) (Timing, error) {
	if !timingName.MatchString(name) {
		return Timing{}, fmt.Errorf("timing %q: a timing name is lower-case letters, digits and dashes", name)
	}
	path := TimingFile(repoRoot, name)
	b, err := os.ReadFile(path)
	if err != nil {
		return Timing{}, fmt.Errorf("timing %s: %w", name, err)
	}
	t, err := ParseTiming(name, b)
	if err != nil {
		return Timing{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// ParseTiming parses a timing file: KEY=VALUE lines, # comments and blank
// lines, as the scripts source it (ParseEnvFile). Every key of Timing must
// be set once, to one word, and no other key may appear; durations are Go
// durations (10s, 1m), and OTLP_EXPORT_INTERVAL_MS is whole milliseconds.
// Every value must be above zero, and RATE_WINDOW must cover at least four
// export intervals.
func ParseTiming(name string, b []byte) (Timing, error) {
	t := Timing{Name: name}
	durations := map[string]*time.Duration{
		timingControllerReconcile: &t.ControllerReconcile,
		timingAgentReconcile:      &t.AgentReconcile,
		timingSpecReadTimeout:     &t.SpecReadTimeout,
		timingScrapeInterval:      &t.ScrapeInterval,
		timingRateWindow:          &t.RateWindow,
		timingFacilityPoll:        &t.FacilityPoll,
	}
	vars, err := ParseEnvFile(b)
	if err != nil {
		return Timing{}, err
	}
	seen := map[string]bool{}
	var errs []error
	for _, v := range vars {
		line, key := v.Line, v.Key
		if seen[key] {
			errs = append(errs, fmt.Errorf("line %d: %s is set twice", line, key))
			continue
		}
		seen[key] = true
		if v.Array || len(v.Words) != 1 {
			errs = append(errs, fmt.Errorf("line %d: %s=%s is not one word", line, key, v.Value))
			continue
		}
		value := v.Words[0]
		if key == timingOTLPExportMS {
			ms, err := strconv.ParseInt(value, 10, 64)
			if err != nil || ms <= 0 {
				errs = append(errs, fmt.Errorf("line %d: %s=%s is not a positive number of milliseconds", line, key, value))
				continue
			}
			t.OTLPExportInterval = time.Duration(ms) * time.Millisecond
			continue
		}
		dst, known := durations[key]
		if !known {
			errs = append(errs, fmt.Errorf("line %d: unknown key %s", line, key))
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("line %d: %s=%s is not a positive duration", line, key, value))
			continue
		}
		*dst = d
	}
	keys := append(agentSortedKeys(durations), timingOTLPExportMS)
	sort.Strings(keys)
	for _, k := range keys {
		if !seen[k] {
			errs = append(errs, fmt.Errorf("%s is not set", k))
		}
	}
	if len(errs) > 0 {
		return Timing{}, errors.Join(errs...)
	}
	if err := t.validate(false); err != nil {
		return Timing{}, err
	}
	return t, nil
}

// validate checks what Telemetry needs of a timing: positive values, and a
// rate window of at least telMinRateIntervals export intervals, and of as
// many scrape intervals when scraped.
func (t Timing) validate(scraped bool) error {
	for _, d := range []struct {
		key string
		v   time.Duration
	}{
		{timingControllerReconcile, t.ControllerReconcile},
		{timingAgentReconcile, t.AgentReconcile},
		{timingSpecReadTimeout, t.SpecReadTimeout},
		{timingScrapeInterval, t.ScrapeInterval},
		{timingOTLPExportMS, t.OTLPExportInterval},
		{timingRateWindow, t.RateWindow},
		{timingFacilityPoll, t.FacilityPoll},
	} {
		if d.v <= 0 {
			return fmt.Errorf("timing %s: %s is not above zero", t.Name, d.key)
		}
	}
	if t.RateWindow < telMinRateIntervals*t.OTLPExportInterval {
		return fmt.Errorf("timing %s: %s %s covers fewer than %d OTLP export intervals of %s", t.Name, timingRateWindow, t.RateWindow, telMinRateIntervals, t.OTLPExportInterval)
	}
	if scraped && t.RateWindow < telMinRateIntervals*t.ScrapeInterval {
		return fmt.Errorf("timing %s: %s %s covers fewer than %d scrape intervals of %s", t.Name, timingRateWindow, t.RateWindow, telMinRateIntervals, t.ScrapeInterval)
	}
	return nil
}
