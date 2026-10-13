package main

// Emulate mode (SIM_EMULATE=true). The nodes of a cluster spec are emulated
// by simulator/pkg/emulate: one hwemu tree per spec node under
// SIM_HWEMU_ROOT, all stepped to wall time on one tick, each tick published
// as an immutable snapshot. Every reader works from the latest snapshot: the
// OpenTelemetry push and events (simulator/pkg/telemetry), the compatibility
// exporters (simulator/pkg/compat), /debug/emulate, and the legacy routes,
// which answer for a spec node from the snapshot and never step it. Nodes
// outside the spec keep the label-based model, so every guard is per node
// (s.emulated).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/compat"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Variables of emulate mode, documented in the configuration reference
// ("Simulator emulate mode" and "Simulator telemetry export").
const (
	envEmulate           = "SIM_EMULATE"
	envClusterSpec       = "SIM_CLUSTER_SPEC"
	envHwemuRoot         = "SIM_HWEMU_ROOT"
	envEmulateTick       = "SIM_EMULATE_TICK"
	envEmulateWorkers    = "SIM_EMULATE_WORKERS"
	envFakeSMISource     = "SIM_FAKESMI_SOURCE"
	envExporters         = "SIM_EXPORTERS"
	envDCGMHostnameLabel = "SIM_DCGM_HOSTNAME_LABEL"
	envStandalone        = "SIM_STANDALONE"
	envWorkloadTrace     = "SIM_WORKLOAD_TRACE_PATH"
)

// Defaults of emulate mode. The fakesmi source is where the simulator image
// ships the binary.
const (
	defaultHwemuRoot     = "/var/lib/joulie-hwemu"
	defaultFakeSMISource = "/usr/local/lib/joulie/fakesmi"
	defaultEmulateTick   = time.Second
)

// checkModes refuses the combinations emulate mode cannot run: with
// SIM_STANDALONE, which has no Kubernetes and no trees; without
// SIM_CLUSTER_SPEC, which names the nodes to emulate; and with
// SIM_WORKLOAD_TRACE_PATH under SIM_WORKLOAD_MODEL=legacy, because a legacy
// trace cannot drive emulated nodes (X10). A trace with the model unset or
// v1 runs on the v1 engine; loadWorkloadOptions checks the model itself.
// Booleans read as boolEnv reads them.
func checkModes(getenv func(string) string) error {
	set := func(key string) bool {
		s := strings.TrimSpace(getenv(key))
		return strings.EqualFold(s, "true") || s == "1" || strings.EqualFold(s, "yes")
	}
	if !set(envEmulate) {
		return nil
	}
	var errs []error
	if set(envStandalone) {
		errs = append(errs, fmt.Errorf("%s=true cannot run with %s=true: standalone mode has no Kubernetes and no node trees", envEmulate, envStandalone))
	}
	if strings.TrimSpace(getenv(envClusterSpec)) == "" {
		errs = append(errs, fmt.Errorf("%s=true needs %s, the cluster spec that names the nodes to emulate", envEmulate, envClusterSpec))
	}
	if strings.TrimSpace(getenv(envWorkloadTrace)) != "" && strings.TrimSpace(getenv(workload.EnvModel)) == string(workload.ModelLegacy) {
		errs = append(errs, fmt.Errorf("%s=true cannot run with %s and %s=%s: a legacy trace cannot drive emulated nodes; leave %s unset for the v1 engine",
			envEmulate, envWorkloadTrace, workload.EnvModel, workload.ModelLegacy, workload.EnvModel))
	}
	return errors.Join(errs...)
}

// emulateOptions is what newEmulateRuntime needs; emulateOptionsFromEnv
// reads it from the environment. A zero Start means time.Now and a nil
// Registerer registers nothing.
type emulateOptions struct {
	SpecPath, Root, FakeSMISource string
	Tick                          time.Duration
	Workers                       int
	Exporters                     []string
	DCGMHostnameLabel             string
	Telemetry                     telemetry.Config
	Resource                      telemetry.Resource
	Start                         time.Time
	Registerer                    prometheus.Registerer
}

// emulateRuntime is the emulate mode of a simulator: the emulator, the
// telemetry it feeds, and what the simulator keeps between snapshots.
type emulateRuntime struct {
	emu       *emulate.Emulator
	cluster   string
	names     []string // spec node names, sorted
	compat    *compat.Set
	sink      *telemetry.Sink
	pusher    *telemetry.Pusher
	resource  telemetry.Resource
	facParams facility.Params
	// start is when the emulated clock starts, emulateOptions.Start: zero
	// when the first tick starts it at the wall time.
	start time.Time
	// ready is set once the first snapshot is published: every tree then
	// exists and has stepped (/readyz, X20).
	ready atomic.Bool

	// Owned by publishEmulateSnapshot, on the emulator's goroutine.
	lastPublish time.Time
	lastHostJ   map[string]float64

	// The last error of the event sink, logged when it changes. Both the
	// emulator's and the pod polling goroutine emit events.
	sinkMu      sync.Mutex
	lastSinkErr string

	// The state of each spec node's Kubernetes Node as checkEmulatedNodes
	// last found it: "" when it matches the spec.
	mismatchMu sync.Mutex
	mismatch   map[string]string

	wg sync.WaitGroup
}

// emulated reports whether node is a spec node, emulated rather than
// modelled. It needs no lock: the members never change.
func (s *simulator) emulated(node string) bool {
	return s.emu != nil && s.emu.emu.Has(node)
}

// emulateOptionsFromEnv reads the emulate settings through lookup, normally
// os.LookupEnv. The telemetry warnings are logged; an invalid variable is an
// error. A set but empty SIM_FAKESMI_SOURCE skips the fakesmi install.
func (s *simulator) emulateOptionsFromEnv(lookup func(string) (string, bool)) (emulateOptions, error) {
	get := func(key string) string {
		v, _ := lookup(key)
		return strings.TrimSpace(v)
	}
	o := emulateOptions{
		SpecPath:          get(envClusterSpec),
		Root:              get(envHwemuRoot),
		FakeSMISource:     defaultFakeSMISource,
		Tick:              defaultEmulateTick,
		DCGMHostnameLabel: get(envDCGMHostnameLabel),
		Resource:          otlpTelemetryResource(),
		Registerer:        prometheus.DefaultRegisterer,
	}
	if o.Root == "" {
		o.Root = defaultHwemuRoot
	}
	if v, ok := lookup(envFakeSMISource); ok {
		o.FakeSMISource = strings.TrimSpace(v)
	}
	var errs []error
	if v := get(envEmulateTick); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s=%q is not a positive duration", envEmulateTick, v))
		}
		o.Tick = d
	}
	if v := get(envEmulateWorkers); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s=%q is not a whole number of at least 0", envEmulateWorkers, v))
		}
		o.Workers = n
	}
	if v := get(envExporters); v != "" {
		o.Exporters = strings.Split(v, ",")
	}
	tc, warnings, err := telemetry.ConfigFromEnv(lookup, s.persistDir)
	for _, w := range warnings {
		log.Printf("warning: %s", w)
	}
	if err != nil {
		errs = append(errs, err)
	}
	o.Telemetry = tc
	return o, errors.Join(errs...)
}

// newEmulateRuntime loads and expands the spec, then builds the exporters,
// the telemetry and the emulator, which takes the lock on o.Root and makes
// every tree exist. Facility temperatures the spec leaves unset fall back
// to SIM_FACILITY_*. Nothing runs until startEmulate.
func (s *simulator) newEmulateRuntime(o emulateOptions) (*emulateRuntime, error) {
	spec, err := clusterspec.LoadFile(o.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("cluster spec: %w", err)
	}
	members, err := spec.Expand(loadProfileSetFromEnv)
	if err != nil {
		return nil, fmt.Errorf("cluster spec %s: %w", o.SpecPath, err)
	}
	set, err := compat.NewSet(o.Exporters, compat.Options{DCGMHostnameLabel: o.DCGMHostnameLabel})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envExporters, err)
	}
	exp, err := otlpjson.NewExporter(o.Telemetry.Exporter)
	if err != nil {
		return nil, err
	}
	cluster := spec.Metadata.Name
	sink, err := telemetry.NewSink(telemetry.SinkConfig{
		Dir:        o.Telemetry.FileDir,
		Metrics:    o.Telemetry.FileMetrics,
		Cluster:    cluster,
		Resource:   o.Resource,
		Exporter:   exp,
		Registerer: o.Registerer,
	})
	if err != nil {
		return nil, err
	}

	fac := spec.Spec.Facility
	fac.AmbientC = emulateOr(fac.AmbientC, s.facilityBaseAmbientTempC)
	fac.AmplitudeC = emulateOr(fac.AmplitudeC, s.facilityTempAmplitudeC)
	fac.PeriodH = emulateOr(fac.PeriodH, s.facilityTempPeriodH)
	emu, err := emulate.New(emulate.Config{
		Root:          o.Root,
		Cluster:       cluster,
		Members:       members,
		Facility:      fac,
		Tick:          o.Tick,
		Workers:       o.Workers,
		FakeSMISource: o.FakeSMISource,
		Start:         o.Start,
		Registerer:    o.Registerer,
	})
	if err != nil {
		_ = sink.Close()
		return nil, err
	}
	pusher, err := telemetry.NewPusher(telemetry.PusherConfig{
		Latest:     emu.Latest,
		Resource:   o.Resource,
		Exporter:   exp,
		Sink:       sink,
		Interval:   o.Telemetry.Exporter.ExportInterval,
		Registerer: o.Registerer,
	})
	if err != nil {
		_ = emu.Close()
		_ = sink.Close()
		return nil, err
	}

	rt := &emulateRuntime{
		emu:      emu,
		cluster:  cluster,
		compat:   set,
		sink:     sink,
		pusher:   pusher,
		resource: o.Resource,
		facParams: facility.Params{
			BaseAmbientC: *fac.AmbientC,
			AmplitudeC:   *fac.AmplitudeC,
			PeriodH:      *fac.PeriodH,
		},
		start:     o.Start,
		lastHostJ: map[string]float64{},
		mismatch:  map[string]string{},
	}
	for _, m := range members {
		rt.names = append(rt.names, m.Name)
	}
	sort.Strings(rt.names)
	return rt, nil
}

// emulateOr returns v, or a pointer to def when v is nil.
func emulateOr(v *float64, def float64) *float64 {
	if v != nil {
		return v
	}
	return &def
}

// startEmulate builds the emulate runtime from the environment and runs the
// emulator, the metrics push and the events push until ctx ends;
// stopEmulate waits for them and releases the root.
func (s *simulator) startEmulate(ctx context.Context) error {
	o, err := s.emulateOptionsFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	rt, err := s.newEmulateRuntime(o)
	if err != nil {
		return err
	}
	s.emu = rt
	rt.wg.Add(3)
	go func() {
		defer rt.wg.Done()
		rt.sink.Run(ctx)
	}()
	go func() {
		defer rt.wg.Done()
		rt.pusher.Run(ctx)
	}()
	go func() {
		defer rt.wg.Done()
		rt.emu.Run(ctx, s.emulateLoads, s.emulateFacility, s.publishEmulateSnapshot)
	}()
	log.Printf("emulate mode cluster=%s nodes=%d root=%s tick=%s exporters=%s",
		rt.cluster, len(rt.names), o.Root, o.Tick, strings.Join(rt.compat.Enabled(), ","))
	return nil
}

// stopEmulate waits for the goroutines of startEmulate, which end with
// their context, then releases the root and closes the event files. The
// trees and ledgers stay for the next start.
func (s *simulator) stopEmulate() {
	if s.emu == nil {
		return
	}
	s.emu.wg.Wait()
	if err := s.emu.emu.Close(); err != nil {
		log.Printf("warning: emulate: %v", err)
	}
	if err := s.emu.sink.Close(); err != nil {
		log.Printf("warning: emulate: %v", err)
	}
}

// emulateLoads is the load callback of the emulator. While a v1 run is
// active it returns the run's loads with no base load, so the jobs alone
// drive the nodes (X9); otherwise no job load, with every node's baseLoad
// applied.
func (s *simulator) emulateLoads() (map[string]hwemu.Load, bool) {
	if r := s.run.Load(); r != nil {
		if loads, active := r.nodeLoads(); active {
			return loads, false
		}
	}
	return map[string]hwemu.Load{}, true
}

// emulateFacility is the facility callback of the emulator: the facility
// model over the IT power of the spec nodes plus that of the legacy
// (non-spec) nodes, from the simulator's start. It also sets the legacy
// facility gauges.
func (s *simulator) emulateFacility(specITPowerW float64, now time.Time) facility.Values {
	legacyW := 0.0
	s.mu.RLock()
	for node, st := range s.state {
		if st == nil || s.emulated(node) {
			continue
		}
		if st.TotalAvgPowerWatts > 0 {
			legacyW += st.TotalAvgPowerWatts
		}
	}
	s.mu.RUnlock()
	s.facilityMu.Lock()
	v, peak := facility.Compute(s.emu.facParams, now.Sub(s.facilityStart()), specITPowerW+legacyW, s.facilityPeakITPowerW)
	s.facilityPeakITPowerW = peak
	s.facilityMu.Unlock()
	s.setFacilityGauges(v)
	return v
}

// publishEmulateSnapshot is the publish callback of the emulator, on its
// goroutine after each tick: the compatibility exporters observe the
// snapshot, the spec nodes' legacy state mirrors it, the legacy nodes
// step over the tick (stepLegacyNodes), the timeseries gets a row, and the
// snapshot's events go to the event files and push and to /debug/events.
// While a v1 run is active, the run then ticks on this goroutine, where it
// may read the nodes' throughput (X9), and copies the hardware events into
// its own logs.
func (s *simulator) publishEmulateSnapshot(snap *emulate.Snapshot) {
	rt := s.emu
	rt.compat.Observe(snap)
	s.applyEmulateSnapshot(snap)
	var dt time.Duration
	if !rt.lastPublish.IsZero() {
		dt = snap.Time.Sub(rt.lastPublish)
		s.stepLegacyNodes(dt)
	}
	rt.lastPublish = snap.Time
	s.appendTimeseriesRow(snap.Time)
	recs := telemetry.HardwareEvents(snap)
	s.emitEmulateEvents(recs)
	if r := s.run.Load(); r != nil {
		r.tick(snap.Time, dt, snap, runtimeNodes{rt}, recs)
	}
	rt.ready.Store(true)
}

// emitEmulateEvents writes recs to the event sink, logging a failure when
// it differs from the last, and records each in /debug/events.
func (s *simulator) emitEmulateEvents(recs []telemetry.EventRecord) {
	if len(recs) == 0 {
		return
	}
	rt := s.emu
	err := rt.sink.Emit(recs...)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	rt.sinkMu.Lock()
	if msg != rt.lastSinkErr {
		if msg != "" {
			log.Printf("warning: emulate events: %v (logged when it changes)", err)
		} else {
			log.Printf("emulate events are written again")
		}
	}
	rt.lastSinkErr = msg
	rt.sinkMu.Unlock()
	for _, r := range recs {
		s.recordEvent(r.Event.Name, r.Node, emulateEventPayload(r))
	}
}

// emulateEventPayload is an event record as /debug/events shows it: its
// attributes by name, and its body.
func emulateEventPayload(r telemetry.EventRecord) map[string]any {
	p := make(map[string]any, len(r.Attributes)+1)
	for _, kv := range r.Attributes {
		v := kv.Value
		switch {
		case v.String != nil:
			p[kv.Key] = *v.String
		case v.Bool != nil:
			p[kv.Key] = *v.Bool
		case v.Int != nil:
			p[kv.Key] = int64(*v.Int)
		case v.Double != nil:
			p[kv.Key] = *v.Double
		}
	}
	p["body"] = r.Body
	return p
}

// emulateNodeSums is one node of a snapshot in the units of the legacy
// state: the CPU packages and GPUs summed, their utilization averaged, and
// the frequency scale as the mean of each package's SpeedHz/SpeedMaxHz (1
// for a package without a maximum, and with no package).
type emulateNodeSums struct {
	cpuW, gpuW               float64
	cpuLimitW, cpuRequestedW float64
	gpuLimitW, gpuRequestedW float64 // means per GPU
	cpuUtil, gpuUtil         float64
	freqScale                float64
	packages, gpus           []emulate.ComponentReading
}

func emulateSum(n *emulate.NodeSnapshot) emulateNodeSums {
	var out emulateNodeSums
	scale := 0.0
	for _, c := range n.Components {
		switch c.Type {
		case semconv.HwTypeCPU:
			out.packages = append(out.packages, c)
			out.cpuW += c.PowerW
			out.cpuLimitW += c.LimitW
			out.cpuRequestedW += c.RequestedLimitW
			out.cpuUtil += c.Utilization
			scale += emulateFreqScale(c)
		case semconv.HwTypeGPU:
			out.gpus = append(out.gpus, c)
			out.gpuW += c.PowerW
			out.gpuLimitW += c.LimitW
			out.gpuRequestedW += c.RequestedLimitW
			out.gpuUtil += c.Utilization
		}
	}
	out.freqScale = 1
	if k := float64(len(out.packages)); k > 0 {
		out.cpuUtil /= k
		out.freqScale = scale / k
	}
	if k := float64(len(out.gpus)); k > 0 {
		out.gpuUtil /= k
		out.gpuLimitW /= k
		out.gpuRequestedW /= k
	}
	return out
}

// emulateFreqScale is a package's SpeedHz/SpeedMaxHz, 1 without a maximum.
func emulateFreqScale(c emulate.ComponentReading) float64 {
	if c.SpeedMaxHz <= 0 {
		return 1
	}
	return c.SpeedHz / c.SpeedMaxHz
}

// emulatedCapacity is what a spec node offers workloads: its profile's
// logical CPUs and GPUs, whatever its labels say.
func emulatedCapacity(m clusterspec.Member) (cpus, gpus float64) {
	if m.Profile == nil {
		return 0, 0
	}
	cpus = float64(m.Profile.Node.Capacity.CPU.V)
	if m.Profile.GPUs != nil {
		gpus = float64(m.Profile.GPUs.Count.V)
	}
	return cpus, gpus
}

// simNow is the time of the emulated clock: that of the latest snapshot,
// before the first one the clock's start, and the wall time when no start
// was set, as the first tick of Run takes it.
func (rt *emulateRuntime) simNow() time.Time {
	if snap := rt.emu.Latest(); snap != nil {
		return snap.Time
	}
	if !rt.start.IsZero() {
		return rt.start
	}
	return time.Now().UTC()
}

// newEmulatedNodeState is the legacy state of spec node m before its first
// snapshot: idle, unthrottled, with its profile's capacity, at simTime on
// the emulated clock (/debug/nodes used to show the zero time).
func newEmulatedNodeState(m clusterspec.Member, simTime time.Time) *nodeState {
	cpus, gpus := emulatedCapacity(m)
	return &nodeState{
		SimTime:            simTime,
		FreqScale:          1,
		CPUWorkClass:       "cpu.mixed",
		LastAction:         "none",
		LastResult:         "none",
		LastUpdate:         time.Now().UTC(),
		ByIntentClass:      map[string]int{},
		GPUWorkClass:       "gpu.mixed",
		GPUPerfMultiplier:  1.0,
		CPUCapacityCores:   cpus,
		GPUCapacityDevices: gpus,
	}
}

// applyEmulateSnapshot mirrors snap into the legacy state of each spec
// node, so /debug/nodes, /debug/energy, the per-node gauges, the facility
// and the timeseries see emulated nodes as they see modelled ones. Host
// power stands for the node's average power; energy grows by the increase
// of the host ledger since the last snapshot.
func (s *simulator) applyEmulateSnapshot(snap *emulate.Snapshot) {
	rt := s.emu
	type gauges struct {
		node                          string
		powerW, capW, util, freqScale float64
	}
	out := make([]gauges, 0, len(snap.Nodes))
	s.mu.Lock()
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		st := s.state[n.Name]
		if st == nil {
			m, _ := rt.emu.Member(n.Name)
			st = newEmulatedNodeState(m, snap.Time)
			s.state[n.Name] = st
		}
		sum := emulateSum(n)
		st.CPUInstantPowerWatts = sum.cpuW
		st.CPUAvgPowerWatts = sum.cpuW
		st.GPUPowerWatts = sum.gpuW
		st.TotalAvgPowerWatts = n.Host.PowerW
		st.CPUUtil = sum.cpuUtil
		st.GPUUtil = sum.gpuUtil
		st.FreqScale = sum.freqScale
		st.CapWatts = sum.cpuLimitW
		st.TargetCapWatts = sum.cpuRequestedW
		st.GPUCapWattsPerGpu = sum.gpuLimitW
		st.GPUTargetCapWattsPerGpu = sum.gpuRequestedW
		// New slices: /debug/nodes encodes a copy of the state after the
		// lock is released.
		st.CPUSockets = make([]cpuSocketState, 0, len(sum.packages))
		for _, c := range sum.packages {
			st.CPUSockets = append(st.CPUSockets, cpuSocketState{
				Index: c.Index, CapWatts: c.LimitW, Utilization: c.Utilization, PerfMultiplier: emulateFreqScale(c),
			})
		}
		st.GPUDevices = make([]gpuDeviceState, 0, len(sum.gpus))
		for _, c := range sum.gpus {
			st.GPUDevices = append(st.GPUDevices, gpuDeviceState{
				Index: c.Index, CapWatts: c.LimitW, TargetCapWatts: c.RequestedLimitW,
				PowerWatts: c.PowerW, AvgPowerWatts: c.PowerW, PerfMultiplier: 1,
			})
		}
		st.LastUpdate = snap.Time
		st.SimTime = snap.Time
		if prev, ok := rt.lastHostJ[n.Name]; ok && n.Host.EnergyJ > prev {
			s.energyJByNode[n.Name] += n.Host.EnergyJ - prev
			s.energyTotalJ += n.Host.EnergyJ - prev
		}
		rt.lastHostJ[n.Name] = n.Host.EnergyJ
		out = append(out, gauges{node: n.Name, powerW: n.Host.PowerW, capW: sum.cpuLimitW, util: sum.cpuUtil, freqScale: sum.freqScale})
	}
	s.energyLastTs = time.Now().UTC()
	s.mu.Unlock()
	for _, g := range out {
		s.nodePowerW.WithLabelValues(g.node).Set(math.Round(g.powerW*100) / 100)
		s.nodeCapW.WithLabelValues(g.node).Set(g.capW)
		s.nodeRaplCapW.WithLabelValues(g.node).Set(g.capW)
		s.nodeUtilCPU.WithLabelValues(g.node).Set(g.util)
		s.nodeFreqScale.WithLabelValues(g.node).Set(g.freqScale)
	}
}

// latestNode returns the latest snapshot and node's entry in it, nil
// before the first tick. Snapshot nodes are sorted by name.
func (rt *emulateRuntime) latestNode(node string) (*emulate.Snapshot, *emulate.NodeSnapshot) {
	snap := rt.emu.Latest()
	if snap == nil {
		return nil, nil
	}
	i := sort.Search(len(snap.Nodes), func(i int) bool { return snap.Nodes[i].Name >= node })
	if i == len(snap.Nodes) || snap.Nodes[i].Name != node {
		return snap, nil
	}
	return snap, &snap.Nodes[i]
}

// emulateNoSnapshot answers a read of a spec node before the first tick.
func emulateNoSnapshot(w http.ResponseWriter, status *int) {
	*status = http.StatusServiceUnavailable
	http.Error(w, "emulate mode: no snapshot yet", *status)
}

// handleEmulatedTelemetry serves /telemetry/{node}?format=legacy for a
// spec node from the latest snapshot, in the legacy JSON: packagePowerWatts
// is every CPU package plus every GPU, cpu.packagePowerWatts the packages
// alone, and hostPowerWatts the whole host. It steps nothing and changes no
// state.
func (s *simulator) handleEmulatedTelemetry(w http.ResponseWriter, node string, status *int) {
	snap, n := s.emu.latestNode(node)
	if n == nil {
		emulateNoSnapshot(w, status)
		return
	}
	sum := emulateSum(n)
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	resp := map[string]any{
		"node":              node,
		"emulated":          true,
		"seq":               snap.Seq,
		"packagePowerWatts": round(sum.cpuW + sum.gpuW),
		"hostPowerWatts":    round(n.Host.PowerW),
		"cpu": map[string]any{
			"packagePowerWatts": round(sum.cpuW),
			"utilization":       sum.cpuUtil,
			"freqScale":         sum.freqScale,
			"capWatts":          sum.cpuLimitW,
			"targetCapWatts":    sum.cpuRequestedW,
		},
		"gpu": map[string]any{
			"present":               len(sum.gpus) > 0,
			"count":                 len(sum.gpus),
			"powerWattsTotal":       round(sum.gpuW),
			"utilization":           sum.gpuUtil,
			"capWattsPerGpuApplied": sum.gpuLimitW,
			"capWattsPerGpuTarget":  sum.gpuRequestedW,
		},
		"ts": snap.Time.UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleEmulatedOTLPTelemetry serves /otlp/telemetry/{node} for a spec
// node: the node's part of the metrics export of the latest snapshot. Unlike
// the push, which stamps the export time, its points carry the snapshot time:
// a reader that polls twice between ticks then sees no new point, where
// export-time stamps would make otlpjson.PowerReader compute 0 W.
func (s *simulator) handleEmulatedOTLPTelemetry(w http.ResponseWriter, node string, status *int) {
	snap, n := s.emu.latestNode(node)
	if n == nil {
		emulateNoSnapshot(w, status)
		return
	}
	d, _ := telemetry.NodeRequest(*n, snap.Cluster, snap.Time, s.emu.resource)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// handleEmulatedState serves /state/{node} for a spec node: its entry of
// the latest snapshot, null before the first tick.
func (s *simulator) handleEmulatedState(w http.ResponseWriter, node string) {
	_, n := s.emu.latestNode(node)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"node": node, "emulated": true, "snapshot": n})
}

// handleDebugEmulate serves /debug/emulate: the latest snapshot, the truth
// every export is computed from (X4).
func (s *simulator) handleDebugEmulate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_emulate", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	snap := s.emu.emu.Latest()
	if snap == nil {
		emulateNoSnapshot(w, &status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleReadyz serves /readyz: 503 until the first snapshot is published,
// when every tree exists and has stepped, then 200 (X20).
func (s *simulator) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.emu == nil || !s.emu.ready.Load() {
		http.Error(w, "not ready: no snapshot published yet", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// nodeMetaListFunc lists the annotations of every Node by name; tests
// replace it.
var nodeMetaListFunc = listNodeAnnotations

func listNodeAnnotations(ctx context.Context, kube kubernetes.Interface) (map[string]map[string]string, error) {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		ann := map[string]string{}
		for k, v := range n.GetAnnotations() {
			ann[k] = v
		}
		out[n.GetName()] = ann
	}
	return out, nil
}

// checkEmulatedNodes compares each spec node with its Kubernetes Node: a
// missing Node, or one whose profile annotation names another profile,
// emits node.mismatch. Each node emits once per change of state, not once
// per poll; a Node that matches again is logged.
func (s *simulator) checkEmulatedNodes(ctx context.Context, kube kubernetes.Interface) {
	if s.emu == nil {
		return
	}
	rt := s.emu
	nodes, err := nodeMetaListFunc(ctx, kube)
	if err != nil {
		log.Printf("warning: emulated node check: %v", err)
		return
	}
	now := time.Now().UTC()
	var recs []telemetry.EventRecord
	var matches []string
	rt.mismatchMu.Lock()
	for _, name := range rt.names {
		m, _ := rt.emu.Member(name)
		profile := m.Profile.Name
		errorType, nodeProfile := "", ""
		ann, ok := nodes[name]
		switch {
		case !ok:
			errorType = semconv.ErrorTypeNodeMissing
		case ann[clusterspec.AnnotationProfile] != profile:
			errorType, nodeProfile = semconv.ErrorTypeProfileMismatch, ann[clusterspec.AnnotationProfile]
		}
		state := ""
		if errorType != "" {
			state = errorType + "/" + nodeProfile
		}
		prev := rt.mismatch[name]
		rt.mismatch[name] = state
		switch {
		case state == prev:
		case state == "":
			matches = append(matches, name)
		default:
			recs = append(recs, telemetry.NodeMismatch(name, errorType, profile, nodeProfile, now))
		}
	}
	rt.mismatchMu.Unlock()
	for _, r := range recs {
		log.Printf("warning: %s", r.Body)
	}
	for _, name := range matches {
		log.Printf("emulated node %s: its Node matches the spec again", name)
	}
	s.emitEmulateEvents(recs)
}
