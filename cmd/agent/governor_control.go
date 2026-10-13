package main

// Governor control: the agent sets the CPU and GPU governor presets of its
// NodeTwin (spec.cpu.governor, spec.gpu.governor) and reports the outcome in
// status.controlStatus.cpuGovernor and gpuGovernor. pkg/governor is the only
// place a preset becomes a native word; this file writes what it plans, reads
// every write back, and watches the result.
//
// No preset is written unless GOVERNOR_WRITES is true. The baseline, the
// state discovery found before the first write, lives in NodeHardware; the
// agent restores it once when a field disappears, when it runs with
// GOVERNOR_WRITES false after an earlier run changed the node (the only
// write it then makes) and, with GOVERNOR_RESTORE_ON_EXIT, when its pod
// stops (SIGTERM). Writes use the mounts and privileges the DVFS fallback
// already uses: cpufreq and amdgpu attributes under /host-sys,
// nvidia-smi -i <pci bus id> for NVIDIA.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// controlStatus components of the governors.
const (
	componentCPUGovernor = "cpuGovernor"
	componentGPUGovernor = "gpuGovernor"
)

// Timing of the control loop. A setting that drifted is applied again at
// most every governorReapplyEvery; governorDriftLimit drifts within
// governorDriftWindow mean another writer holds the setting, and the agent
// stops writing that component. A write the driver did not take is not
// retried for governorRetryAfter, unless the intent changes. NVIDIA locks
// are issued again every governorReapplyEvery, every reconcile while
// persistence mode is off: a driver that unloads loses them, and they
// cannot be read back.
const (
	governorReapplyEvery = 5 * time.Minute
	governorDriftWindow  = 30 * time.Minute
	governorDriftLimit   = 3
	governorRetryAfter   = 30 * time.Minute
)

// A clock lock has no getter, so the agent verifies one in use: a sample
// counts when the GPU is busy and its clocks sit within the lock, with
// nvidiaVerifyTolerance of slack; nvidiaVerifySamples samples per GPU turn
// applied-unverified into applied (both assumed, no published figure).
const (
	nvidiaVerifySamples   = 3
	nvidiaVerifyTolerance = 1.02
	// nvidiaReasonGPUIdle is the GPU idle bit of
	// clocks_event_reasons.active (NVML clocksEventReasonGpuIdle).
	nvidiaReasonGPUIdle = 0x1
)

// governorODSettle bounds the wait for pp_od_clk_voltage to print its table
// again after an overdrive edit, polled every governorODPoll. The kernel
// prints it as soon as write(2) returns; an emulated tree prints it at the
// emulator's next tick. The kernel takes one command per write, so "c"
// follows "s 1 <MHz>" only once the edit has been read back.
var (
	governorODSettle = 5 * time.Second
	governorODPoll   = 20 * time.Millisecond
)

// writeSysfsAttr writes value to a sysfs attribute in place: open without
// create, truncate, write. A missing attribute is an error rather than a new
// file, and the attribute is never replaced, which an emulated tree, read
// back in place, would not see. A variable so a test can watch or fail the
// writes.
var writeSysfsAttr = func(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// governorConfig holds the agent's governor switches.
type governorConfig struct {
	// writes is GOVERNOR_WRITES: without it the agent only discovers.
	writes bool
	// restoreOnExit is GOVERNOR_RESTORE_ON_EXIT: restore the baseline when
	// the agent stops on SIGTERM or SIGINT.
	restoreOnExit bool
}

func governorConfigFromEnv() governorConfig {
	return governorConfig{
		writes:        boolEnv("GOVERNOR_WRITES", false),
		restoreOnExit: boolEnv("GOVERNOR_RESTORE_ON_EXIT", true),
	}
}

// boolEnv reads a boolean variable; empty or invalid gives def, with a
// warning for an invalid value.
func boolEnv(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using %v", key, raw, def)
		return def
	}
	return v
}

var (
	governorMetricsOnce  sync.Once
	governorWritesMetric *prometheus.CounterVec
	governorStateMetric  *prometheus.GaugeVec
)

// governorMetrics registers the governor metrics once per process.
func governorMetrics() (*prometheus.CounterVec, *prometheus.GaugeVec) {
	governorMetricsOnce.Do(func() {
		governorWritesMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_agent_governor_writes_total",
			Help: "Governor writes (a preset applied or the baseline restored) per component, preset and result",
		}, []string{"node", "component", "preset", "result"})
		governorStateMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_agent_governor_state",
			Help: "Governor preset in effect per component (1), others 0",
		}, []string{"node", "component", "preset"})
		prometheus.MustRegister(governorWritesMetric, governorStateMetric)
	})
	return governorWritesMetric, governorStateMetric
}

// governorRestorePreset is the preset label of a restore in
// joulie_agent_governor_writes_total.
const governorRestorePreset = "baseline"

// governorStatus is one status.controlStatus governor entry.
type governorStatus struct {
	backend, result, message, applied string
}

// governorOutcome is what one apply or restore did: the status, the native
// writes or commands that succeeded, and whether there was anything to do
// (an attempt counts in joulie_agent_governor_writes_total).
type governorOutcome struct {
	status    governorStatus
	wrote     int
	attempted bool
}

// nvidiaLock is a graphics or memory lock in MHz, nil when unset.
type nvidiaLock = *[2]int

// nvidiaRecord is the agent's own record of the locks it set on one GPU: a
// lock cannot be read back, so a reset is issued only for a lock in it.
type nvidiaRecord struct {
	gpu, mem nvidiaLock
}

// governorComponent is the control state of the CPU or the GPU governor.
type governorComponent struct {
	// name is the controlStatus key, label the metric's component.
	name, label string
	// intent is the key of the intent last acted on, preset its preset.
	intent, preset string
	// expect holds the attributes the agent wrote and the values it reads
	// back while the setting holds.
	expect []governor.Write
	// od holds the overdrive file of each amdgpu device the agent gave a
	// compute ceiling, relative to /host-sys, and the ceiling in MHz.
	od map[string]int
	// odDevices lists the bus ids whose overdrive range the agent edited
	// since the baseline.
	odDevices map[string]bool
	// nv is the NVIDIA lock record per bus id; nvGood counts the verified
	// samples of each locked GPU under the current intent; nvSeed is the
	// applied field of the last status, the record of an earlier run until
	// the GPUs are discovered.
	nv     map[string]*nvidiaRecord
	nvGood map[string]int
	nvSeed string
	// changed is true once the agent wrote something since the baseline;
	// boostWritten once it wrote boost; globalBoostSeen is the global boost
	// file before the agent turned it on for a per-policy boost, "" when
	// untouched.
	changed, boostWritten bool
	globalBoostSeen       string
	// verified is true once a check after the last write read it back.
	verified bool
	// drifted is true while a drift is pending; drifts are the times of the
	// drifts seen in the last governorDriftWindow.
	drifted bool
	drifts  []time.Time
	// contended and refused stop writes: contended until the intent
	// changes, refused until the agent restarts.
	contended, refused bool
	// retryAt is when a write the driver did not take is tried again.
	retryAt   time.Time
	lastApply time.Time
	// status is what controlStatus shows; good is the outcome of the last
	// apply, which a drift that resolves itself returns to.
	status, good  governorStatus
	statusWritten bool
}

// startIntent forgets the state of the previous intent.
func (c *governorComponent) startIntent(key, preset string) {
	c.intent, c.preset = key, preset
	c.expect, c.od = nil, nil
	c.verified, c.drifted, c.contended = false, false, false
	c.drifts, c.retryAt = nil, time.Time{}
	c.nvGood = map[string]int{}
}

// governorControl is the governor control of one node.
type governorControl struct {
	nodeName string
	cfg      governorConfig
	// remote is true in pool mode, where the agent does not run on the
	// node it manages, so /host-sys is not that node's.
	remote bool
	now    func() time.Time

	// loaded is true once the stored baselines were read.
	loaded      bool
	cpuBaseline *v1alpha1.CPUGovernorBaseline
	gpuBaseline *v1alpha1.GPUGovernorBaseline
	cpu, gpu    governorComponent
	// last is the latest discovery, which the exit restore plans with.
	last governorDiscovery
}

func newGovernorControl(nodeName string) *governorControl {
	return &governorControl{
		nodeName: nodeName,
		cfg:      governorConfigFromEnv(),
		now:      time.Now,
		cpu:      governorComponent{name: componentCPUGovernor, label: "cpu"},
		gpu:      governorComponent{name: componentGPUGovernor, label: "gpu"},
	}
}

// governorControl returns the controller's governor control, built on first
// use for a controller made without newNodeController.
func (n *NodeController) governorControl() *governorControl {
	if n.governor == nil {
		n.governor = newGovernorControl(n.nodeName)
	}
	return n.governor
}

// governorResultWrote reports whether a status result means the agent may
// have changed the node: everything but nothing to do, discovery only and a
// restore.
func governorResultWrote(result string) bool {
	switch result {
	case "", joulie.GovernorResultUnsupported, joulie.GovernorResultDisabled, joulie.GovernorResultRestored:
		return false
	}
	return true
}

// prepare runs before NodeHardware is written. It reads the stored baselines
// once, through the typed status, records a baseline the object lacks from
// this discovery (so before the first write), and hands both to the
// NodeHardware status: a baseline is never recorded again over a state the
// agent may have set, also when one discovery comes back empty. A refused
// write sets writable false. The last status in the NodeTwin tells a
// restarted agent whether an earlier run changed the governors, and which
// NVIDIA locks it set.
func (g *governorControl) prepare(ctx context.Context, dyn dynamic.Interface, hw *HardwareInfo, desired *DesiredState) {
	if !g.loaded {
		obj, err := dyn.Resource(nodeHardwareGVR).Get(ctx, sanitizeNodeObjectName(g.nodeName), metav1.GetOptions{})
		switch {
		case err == nil:
			g.cpuBaseline, g.gpuBaseline = storedGovernorBaselines(obj)
			g.loaded = true
		case apierrors.IsNotFound(err):
			g.loaded = true
		default:
			log.Printf("warning: governor: NodeHardware of node=%s not read (%v); no governor write until it is", g.nodeName, err)
			return
		}
		if desired != nil {
			g.seed(desired)
		}
	}
	now := g.now().UTC().Format(time.RFC3339)
	gov := &hw.Governors
	// Without a stored baseline after an earlier run changed a component
	// (NodeHardware lost), the state seen now may be the agent's own: the
	// GPUs take the vendor default (amdgpu auto; an NVIDIA lock stays
	// unknown), the CPU the state seen now, since the driver's default
	// governor is unknowable.
	if g.cpuBaseline == nil && gov.CPU != nil {
		b := gov.CPU.baseline()
		b.RecordedAt = now
		if g.cpu.changed {
			log.Printf("warning: governor cpu node=%s: no stored baseline after an earlier run changed the governors; restoring to the state seen now", g.nodeName)
		}
		g.cpuBaseline = b
	}
	if g.gpuBaseline == nil && gov.GPU != nil {
		b := &v1alpha1.GPUGovernorBaseline{Devices: slices.Clone(gov.GPU.Devices), RecordedAt: now}
		if g.gpu.changed {
			for i := range b.Devices {
				if b.Devices[i].Level != joulie.GovernorStateUnknown {
					b.Devices[i].Level = amdLevelAuto
				}
			}
			log.Printf("warning: governor gpu node=%s: no stored baseline after an earlier run changed the GPUs; restoring to the vendor default", g.nodeName)
		}
		g.gpuBaseline = b
	}
	gov.CPUBaseline, gov.GPUBaseline = g.cpuBaseline, g.gpuBaseline
	if gov.CPU != nil && g.cpu.refused {
		gov.CPU.Inventory.Writable = false
	}
	if gov.GPU != nil {
		if g.gpu.refused {
			gov.GPU.Inventory.Writable = false
		}
		if len(gov.GPU.NVIDIA) > 0 && g.nvidiaLocked() && g.gpu.status.applied != "" {
			gov.GPU.Inventory.Current = g.gpu.status.applied
		}
	}
	g.last = *gov
}

// seed takes what an earlier run of the agent left in the NodeTwin status.
// That status is the agent's own, so it goes with the field as one this
// run wrote does.
func (g *governorControl) seed(desired *DesiredState) {
	for _, last := range []struct {
		c *governorComponent
		r *v1alpha1.ControlResult
	}{{&g.cpu, desired.LastCPUGovernor}, {&g.gpu, desired.LastGPUGovernor}} {
		if r := last.r; r != nil {
			last.c.status = governorStatus{backend: r.Backend, result: r.Result, message: r.Message, applied: r.Applied}
			last.c.statusWritten = true
		}
	}
	if r := desired.LastCPUGovernor; r != nil && governorResultWrote(r.Result) {
		g.cpu.changed = true
		g.cpu.boostWritten = strings.Contains(r.Applied, "boost=") || strings.Contains(r.Applied, "no_turbo=")
	}
	if r := desired.LastGPUGovernor; r != nil && governorResultWrote(r.Result) {
		g.gpu.changed = true
		switch r.Backend {
		case joulie.GovernorBackendNvidiaSMI:
			g.gpu.nvSeed = r.Applied
		case joulie.GovernorBackendAMDGPUSysfs:
			if strings.Contains(r.Applied, "sclk_max=") {
				g.gpu.odDevices = map[string]bool{"*": true}
			}
		}
	}
}

// governorEnv is what reconcileOnce knows about how the node is controlled.
type governorEnv struct {
	simulateOnly bool
	// cpuHTTP and gpuHTTP are true in the legacy HTTP control mode
	// (TELEMETRY_CPU_CONTROL, TELEMETRY_GPU_CONTROL http), which has no
	// governor action.
	cpuHTTP, gpuHTTP bool
	// nodeLabels are the Node's labels, which name the NVIDIA architecture.
	nodeLabels map[string]string
}

// reconcile acts on the governor intents of desired (none when nil): the
// CPU governor, then the GPU governor.
func (g *governorControl) reconcile(ctx context.Context, dyn dynamic.Interface, hw HardwareInfo, desired *DesiredState, env governorEnv) {
	if !g.loaded {
		return
	}
	var cpuIntent *joulie.CPUGovernorIntent
	var gpuIntent *joulie.GPUGovernorIntent
	if desired != nil {
		cpuIntent, gpuIntent = desired.CPUGovernor, desired.GPUGovernor
	}
	g.reconcileCPU(ctx, dyn, hw.Governors.CPU, cpuIntent, env)
	g.reconcileGPU(ctx, dyn, hw, gpuIntent, env)
}

// blocked is the status of an intent the agent will not write, and true,
// when writes are off or the mechanism is missing: missing, when not empty,
// says why there is no mechanism, with missingResult.
func (g *governorControl) blocked(env governorEnv, http bool, missing, missingResult string) (governorStatus, bool) {
	switch {
	case env.simulateOnly:
		return governorStatus{backend: "none", result: joulie.GovernorResultDisabled, message: "simulate-only mode: no governor write"}, true
	case !g.cfg.writes:
		return governorStatus{backend: "none", result: joulie.GovernorResultDisabled, message: "GOVERNOR_WRITES is false: discovery only"}, true
	case g.remote:
		return governorStatus{backend: "none", result: joulie.GovernorResultUnsupported, message: "pool mode: the agent does not run on this node"}, true
	case http:
		return governorStatus{backend: "http", result: joulie.GovernorResultUnsupported, message: "the HTTP control backend has no governor action"}, true
	case missing != "":
		return governorStatus{backend: "none", result: missingResult, message: missing}, true
	}
	return governorStatus{}, false
}

// governorOps are the mechanism-specific parts of one component's step.
type governorOps struct {
	// apply sets the intent.
	apply func() governorOutcome
	// check compares the node with what the agent set: ok while it holds;
	// reset when the device came back at its vendor default (a GPU reset),
	// which is applied again without counting a drift.
	check func() (ok, reset bool, msg string)
	// reissue reports whether to issue the setting again now although no
	// change shows (NVIDIA locks, which have no getter); nil never.
	reissue func(now time.Time) bool
}

// hold reports an intent the agent will not write.
func (g *governorControl) hold(ctx context.Context, dyn dynamic.Interface, c *governorComponent, preset string, st governorStatus) {
	c.intent, c.preset = "", preset
	c.status = st
	g.setState(c)
	g.publish(ctx, dyn, c)
}

// step acts on one intent: apply it when it is new, else check what the
// agent set and handle drift, refusals and retries.
func (g *governorControl) step(ctx context.Context, dyn dynamic.Interface, c *governorComponent, key, preset string, ops governorOps) {
	now := g.now()
	switch {
	case c.refused:
	case key != c.intent:
		c.startIntent(key, preset)
		g.applyNow(c, ops, now)
	case c.contended:
	case !c.retryAt.IsZero():
		if !now.Before(c.retryAt) {
			g.applyNow(c, ops, now)
		}
	case c.status.result == joulie.GovernorResultError:
		if now.Sub(c.lastApply) >= governorReapplyEvery {
			g.applyNow(c, ops, now)
		}
	case ops.reissue != nil && ops.reissue(now):
		g.applyNow(c, ops, now)
		// Locks issued again are the same locks: their samples still count.
		g.checkNow(c, ops, now)
	default:
		g.checkNow(c, ops, now)
	}
	g.publish(ctx, dyn, c)
}

func (g *governorControl) applyNow(c *governorComponent, ops governorOps, now time.Time) {
	out := ops.apply()
	c.lastApply = now
	if out.wrote > 0 {
		c.changed = true
	}
	c.status, c.good = out.status, out.status
	c.verified = len(c.expect) == 0 && len(c.od) == 0
	c.drifted = false
	c.retryAt = time.Time{}
	switch out.status.result {
	case joulie.GovernorResultRefused:
		c.refused = true
		log.Printf("warning: governor %s node=%s refused: %s; no retry until the agent restarts", c.label, g.nodeName, out.status.message)
	case joulie.GovernorResultUnsupported, joulie.GovernorResultNotApplied:
		c.retryAt = now.Add(governorRetryAfter)
	}
	if out.attempted {
		g.countWrite(c, c.preset, out.status.result)
	}
	g.setState(c)
}

func (g *governorControl) checkNow(c *governorComponent, ops governorOps, now time.Time) {
	ok, reset, msg := ops.check()
	switch {
	case ok:
		c.verified, c.drifted = true, false
		if c.status.result == joulie.GovernorResultDrift {
			c.status = c.good
		}
		g.setState(c)
		return
	case !c.verified:
		// The write read back right after it, then did not hold: the
		// driver dropped it, as an emulated driver does at its next tick.
		// This comes before a reset: a level the driver reverted reads its
		// default too.
		c.status = governorStatus{backend: c.good.backend, result: joulie.GovernorResultNotApplied, message: msg, applied: c.good.applied}
		c.retryAt = now.Add(governorRetryAfter)
		g.setState(c)
		return
	case reset:
		g.applyNow(c, ops, now)
		return
	case !c.drifted:
		c.drifted = true
		kept := c.drifts[:0]
		for _, t := range c.drifts {
			if now.Sub(t) < governorDriftWindow {
				kept = append(kept, t)
			}
		}
		c.drifts = append(kept, now)
		if len(c.drifts) >= governorDriftLimit {
			c.contended = true
			c.status = governorStatus{backend: c.good.backend, result: joulie.GovernorResultContended,
				message: fmt.Sprintf("%d drifts within %s: another writer holds the setting; no more writes for this intent (%s)", len(c.drifts), governorDriftWindow, msg),
				applied: c.good.applied}
			log.Printf("warning: governor %s node=%s contended: %s", c.label, g.nodeName, c.status.message)
			g.setState(c)
			return
		}
		c.status = governorStatus{backend: c.good.backend, result: joulie.GovernorResultDrift, message: msg, applied: c.good.applied}
		g.setState(c)
	}
	if now.Sub(c.lastApply) >= governorReapplyEvery {
		g.applyNow(c, ops, now)
	}
}

// mayRestore reports whether the agent may write c's baseline back. It
// does not ask for GOVERNOR_WRITES: putting back what an earlier run
// changed is the one write a discovery-only agent makes (design D8).
func (g *governorControl) mayRestore(c *governorComponent, env governorEnv) bool {
	return !env.simulateOnly && !g.remote && !c.refused
}

// restoreWritesOff reports whether c keeps an intent the agent will not
// write, because GOVERNOR_WRITES is false, over a change an earlier run
// made: the baseline then goes back once, as when the field disappears,
// before the intent is held as disabled.
func (g *governorControl) restoreWritesOff(c *governorComponent, env governorEnv) bool {
	return !g.cfg.writes && c.changed && !c.contended && g.mayRestore(c, env)
}

// release handles a component whose intent disappeared: the baseline goes
// back once when the agent changed something, else a status the agent
// wrote is removed. restore is nil when the node cannot be planned with now
// (a GPU discovery that came back empty); the restore then waits.
func (g *governorControl) release(ctx context.Context, dyn dynamic.Interface, c *governorComponent, env governorEnv, restore func() governorOutcome) {
	now := g.now()
	canWrite := g.mayRestore(c, env)
	if c.contended {
		// The other writer owns the setting now.
		c.changed, c.contended = false, false
	}
	if c.changed && canWrite {
		if restore == nil || (!c.retryAt.IsZero() && now.Before(c.retryAt)) {
			return
		}
		out := restore()
		c.intent, c.preset = "", ""
		c.expect, c.od = nil, nil
		c.drifts, c.drifted = nil, false
		c.lastApply = now
		c.status, c.good = out.status, out.status
		c.retryAt = time.Time{}
		switch out.status.result {
		case joulie.GovernorResultRestored:
			c.changed, c.boostWritten, c.globalBoostSeen = false, false, ""
			c.odDevices = nil
		case joulie.GovernorResultRefused:
			c.refused = true
		default:
			c.retryAt = now.Add(governorRetryAfter)
		}
		if out.attempted {
			g.countWrite(c, governorRestorePreset, out.status.result)
		}
		g.setState(c)
		g.publish(ctx, dyn, c)
		return
	}
	c.intent, c.preset = "", ""
	c.expect, c.od = nil, nil
	c.retryAt = time.Time{}
	g.setState(c)
	if c.statusWritten && c.status.result != joulie.GovernorResultRestored {
		if err := updateNodeTwinGovernorStatus(ctx, dyn, g.nodeName, c.name, nil); err != nil {
			log.Printf("warning: governor %s node=%s: status not removed: %v", c.label, g.nodeName, err)
			return
		}
		c.statusWritten = false
		c.status = governorStatus{}
	}
}

// publish writes the component's status to the NodeTwin.
func (g *governorControl) publish(ctx context.Context, dyn dynamic.Interface, c *governorComponent) {
	s := c.status
	if err := updateNodeTwinGovernorStatus(ctx, dyn, g.nodeName, c.name, &s); err != nil {
		log.Printf("warning: governor %s node=%s: status not written: %v", c.label, g.nodeName, err)
		return
	}
	c.statusWritten = true
}

func (g *governorControl) countWrite(c *governorComponent, preset, result string) {
	writes, _ := governorMetrics()
	writes.WithLabelValues(g.nodeName, c.label, preset, result).Inc()
}

// setState sets joulie_agent_governor_state: 1 on the preset in effect, 0
// on the others.
func (g *governorControl) setState(c *governorComponent) {
	_, state := governorMetrics()
	presets := joulie.CPUGovernorPresetValues
	if c.label == "gpu" {
		presets = joulie.GPUGovernorPresetValues
	}
	inEffect := ""
	switch c.status.result {
	case joulie.GovernorResultApplied, joulie.GovernorResultAppliedUnverified, joulie.GovernorResultEquivalent:
		inEffect = c.preset
	}
	for _, p := range presets {
		v := 0.0
		if p == inEffect {
			v = 1
		}
		state.WithLabelValues(g.nodeName, c.label, p).Set(v)
	}
}

// writeVerified writes each attribute in order and reads it back before the
// next. It stops at the first write the driver refuses or does not take,
// and returns how many succeeded and, when one failed, its status result and
// message.
func writeVerified(writes []governor.Write) (wrote int, result, message string) {
	for _, w := range writes {
		path := hostSysPath(w.RelPath)
		if err := writeSysfsAttr(path, w.Value); err != nil {
			result, message = governor.Classify(err, w.Value, "")
			return wrote, result, message
		}
		wrote++
		got, _ := readSysfsWord(path)
		if result, message = governor.Classify(nil, w.Value, got); result != joulie.GovernorResultApplied {
			return wrote, result, w.RelPath + ": " + message
		}
	}
	return wrote, "", ""
}

// checkWrites reports whether every attribute still reads what the agent
// wrote, and names the first that does not.
func checkWrites(writes []governor.Write) (ok bool, msg string) {
	for _, w := range writes {
		got, _ := readSysfsWord(hostSysPath(w.RelPath))
		if got != w.Value {
			return false, fmt.Sprintf("%s reads %q, the agent wrote %q", w.RelPath, got, w.Value)
		}
	}
	return true, ""
}

// --- CPU ---------------------------------------------------------------

func (g *governorControl) reconcileCPU(ctx context.Context, dyn dynamic.Interface, d *cpuGovernorDiscovery, intent *joulie.CPUGovernorIntent, env governorEnv) {
	c := &g.cpu
	if intent == nil || g.restoreWritesOff(c, env) {
		var restore func() governorOutcome
		if d != nil {
			restore = func() governorOutcome { return g.restoreCPU(d) }
		}
		g.release(ctx, dyn, c, env, restore)
		return
	}
	missing := ""
	if d == nil {
		missing = "no cpufreq governor on this node"
	}
	if st, ok := g.blocked(env, env.cpuHTTP, missing, joulie.GovernorResultUnsupported); ok {
		g.hold(ctx, dyn, c, intent.Preset, st)
		return
	}
	g.step(ctx, dyn, c, intent.Preset+"|"+intent.Boost, intent.Preset, governorOps{
		apply: func() governorOutcome { return g.applyCPU(d, intent) },
		check: func() (bool, bool, string) {
			ok, msg := checkWrites(c.expect)
			return ok, false, msg
		},
	})
}

// globalBoostRel is the global cpufreq boost file.
const globalBoostRel = cpufreqRelDir + "/boost"

func (g *governorControl) applyCPU(d *cpuGovernorDiscovery, intent *joulie.CPUGovernorIntent) governorOutcome {
	c := &g.cpu
	backend := joulie.GovernorBackendCPUFreq
	plan, err := governor.PlanCPU(d.Facts, intent.Preset, intent.Boost)
	if err != nil {
		return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultError, message: err.Error()}}
	}
	if plan.Result == joulie.GovernorResultUnsupported {
		return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultUnsupported,
			message: fmt.Sprintf("preset %q (boost %q) is not supported by the %s driver of this node", intent.Preset, intent.Boost, d.Facts.Driver)}}
	}
	writes := plan.Writes
	native := plan.Native
	switch {
	case intent.Boost == joulie.CPUGovernorBoostOn && d.Facts.BoostScope == joulie.CPUBoostScopePerCPU:
		// A policy's boost turns on only while the driver's global boost is
		// on, where the global file exists (assumed from the cpufreq core's
		// local boost store): turn that on first, and remember to put it
		// back.
		if v, ok := readSysfsWord(hostSysPath(globalBoostRel)); ok && v == "0" {
			writes = append([]governor.Write{{RelPath: globalBoostRel, Value: "1"}}, writes...)
			if c.globalBoostSeen == "" {
				c.globalBoostSeen = "0"
			}
		}
	case intent.Boost == "" && c.boostWritten:
		// The boost field disappeared: boost goes back to the baseline.
		writes = append(slices.Clone(writes), cpuBaselineBoostWrites(d, g.cpuBaseline, c.globalBoostSeen)...)
	}
	wrote, result, msg := writeVerified(writes)
	if wrote > 0 && intent.Boost != "" {
		c.boostWritten = true
	}
	if result != "" {
		return governorOutcome{status: governorStatus{backend: backend, result: result, message: msg, applied: native}, wrote: wrote, attempted: true}
	}
	if intent.Boost == "" && c.boostWritten {
		c.boostWritten, c.globalBoostSeen = false, ""
	}
	c.expect = writes
	return governorOutcome{status: governorStatus{backend: backend, result: plan.Result, applied: native}, wrote: wrote, attempted: true}
}

// parseCPUList reads the kernel's list form, such as 0-63 or 0,2,4-7.
func parseCPUList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				continue
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out
}

// boostBit is the content of a boost file for a boost word: 1 for on, the
// inverse on intel_pstate's no_turbo.
func boostBit(word string, inverted bool) string {
	if (word == joulie.CPUGovernorBoostOn) != inverted {
		return "1"
	}
	return "0"
}

// differing keeps the writes whose attribute does not already read the
// value.
func differing(writes []governor.Write) []governor.Write {
	var out []governor.Write
	for _, w := range writes {
		if got, _ := readSysfsWord(hostSysPath(w.RelPath)); got != w.Value {
			out = append(out, w)
		}
	}
	return out
}

// cpuBaselineWrites lists, for the policies of d, the baseline value of one
// policy attribute (governor or EPP) by policy, from the groups of b.
func cpuBaselineWrites(d *cpuGovernorDiscovery, b *v1alpha1.CPUGovernorBaseline, file string, value func(v1alpha1.CPUGovernorState) string) []governor.Write {
	if b == nil {
		return nil
	}
	present := map[int]bool{}
	for _, id := range d.Facts.Policies {
		present[id] = true
	}
	var out []governor.Write
	for _, grp := range b.Groups {
		v := value(grp)
		if v == "" {
			continue
		}
		for _, id := range parseCPUList(grp.Policies) {
			if present[id] {
				out = append(out, governor.Write{RelPath: fmt.Sprintf("%s/policy%d/%s", cpufreqRelDir, id, file), Value: v})
			}
		}
	}
	return out
}

// cpuBaselineBoostWrites are the writes that put boost back as the baseline
// found it, skipping a file that already reads it: the global file, the
// inverted no_turbo, or each policy's boost and then the global file the
// agent turned on (globalSeen).
func cpuBaselineBoostWrites(d *cpuGovernorDiscovery, b *v1alpha1.CPUGovernorBaseline, globalSeen string) []governor.Write {
	if b == nil || len(b.Groups) == 0 {
		return nil
	}
	var writes []governor.Write
	first := b.Groups[0].Boost
	switch d.Facts.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		if first != "" {
			writes = append(writes, governor.Write{RelPath: globalBoostRel, Value: boostBit(first, false)})
		}
	case joulie.CPUBoostScopeNoTurbo:
		if first != "" {
			writes = append(writes, governor.Write{RelPath: intelPstateRelDir + "/no_turbo", Value: boostBit(first, true)})
		}
	case joulie.CPUBoostScopePerCPU:
		writes = cpuBaselineWrites(d, b, "boost", func(s v1alpha1.CPUGovernorState) string {
			if s.Boost == "" {
				return ""
			}
			return boostBit(s.Boost, false)
		})
		if globalSeen != "" {
			writes = append(writes, governor.Write{RelPath: globalBoostRel, Value: globalSeen})
		}
	}
	return differing(writes)
}

// restoreCPU writes the baseline back: each policy's governor, then its
// energy-performance preference (read again after the governor, which can
// change it), then boost when the agent wrote it. Only an attribute that
// differs is written.
func (g *governorControl) restoreCPU(d *cpuGovernorDiscovery) governorOutcome {
	backend := joulie.GovernorBackendCPUFreq
	b := g.cpuBaseline
	if b == nil {
		return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultError, message: "no baseline to restore"}}
	}
	total := 0
	phases := []func() []governor.Write{
		func() []governor.Write {
			return differing(cpuBaselineWrites(d, b, "scaling_governor", func(s v1alpha1.CPUGovernorState) string { return s.Governor }))
		},
		func() []governor.Write {
			return differing(cpuBaselineWrites(d, b, "energy_performance_preference", func(s v1alpha1.CPUGovernorState) string { return s.EPP }))
		},
		func() []governor.Write {
			// Boost goes back only when the agent wrote it.
			if !g.cpu.boostWritten && g.cpu.globalBoostSeen == "" {
				return nil
			}
			return cpuBaselineBoostWrites(d, b, g.cpu.globalBoostSeen)
		},
	}
	for _, phase := range phases {
		wrote, result, msg := writeVerified(phase())
		total += wrote
		if result != "" {
			return governorOutcome{status: governorStatus{backend: backend, result: result, message: "restore: " + msg}, wrote: total, attempted: true}
		}
	}
	return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultRestored,
		message: "baseline restored", applied: cpuBaselineNative(b)}, wrote: total, attempted: total > 0}
}

// cpuBaselineNative names the baseline the way a plan names its state: the
// governor and preference of every group, mixed when the groups differ.
func cpuBaselineNative(b *v1alpha1.CPUGovernorBaseline) string {
	var words []string
	for _, grp := range b.Groups {
		w := grp.Governor
		if grp.EPP != "" {
			w += " epp=" + grp.EPP
		}
		words = append(words, w)
	}
	return uniformOrMixed(slices.Compact(words))
}

// --- GPU ---------------------------------------------------------------

func (g *governorControl) reconcileGPU(ctx context.Context, dyn dynamic.Interface, hw HardwareInfo, intent *joulie.GPUGovernorIntent, env governorEnv) {
	c := &g.gpu
	gov := hw.Governors
	d := gov.GPU
	if d != nil && len(d.NVIDIA) > 0 {
		g.seedNvidia(d)
	}
	if intent == nil || g.restoreWritesOff(c, env) {
		var restore func() governorOutcome
		if d != nil {
			restore = func() governorOutcome { return g.restoreGPU(ctx, d) }
		}
		g.release(ctx, dyn, c, env, restore)
		return
	}
	// No GPU governor: none on this node, or a tool that failed this time
	// (an error, as on the cap path). An NVIDIA GPU with a mapping and no
	// clock query means nvidia-smi listed no GPU: a missing tool.
	missing, missingResult := "", joulie.GovernorResultUnsupported
	if d == nil {
		missing = "no GPU governor on this node"
		switch {
		case len(gov.GPUWarnings) > 0:
			missing, missingResult = strings.Join(gov.GPUWarnings, "; "), joulie.GovernorResultError
		case hw.GPUVendor == "nvidia" && nvidiaArch(env.nodeLabels, hw.GPURawModel) != "":
			missing, missingResult = "nvidia-smi listed no GPU: NVIDIA governor presets need nvidia-smi in the agent", joulie.GovernorResultError
		}
	}
	if st, ok := g.blocked(env, env.gpuHTTP, missing, missingResult); ok {
		g.hold(ctx, dyn, c, intent.Preset, st)
		return
	}
	ceiling := governor.DefaultCeilingPctOfMax
	if intent.ComputeClockCeilingPctOfMax != nil {
		ceiling = *intent.ComputeClockCeilingPctOfMax
	}
	key := intent.Preset + "|" + strconv.FormatFloat(ceiling, 'f', -1, 64)
	if len(d.NVIDIA) > 0 {
		g.step(ctx, dyn, c, key, intent.Preset, governorOps{
			apply:   func() governorOutcome { return g.applyNvidia(ctx, d, intent.Preset, ceiling) },
			check:   func() (bool, bool, string) { return g.verifyNvidia(ctx), false, "" },
			reissue: func(now time.Time) bool { return g.nvidiaReissueDue(ctx, now) },
		})
		return
	}
	g.step(ctx, dyn, c, key, intent.Preset, governorOps{
		apply: func() governorOutcome { return g.applyAMD(ctx, d, intent.Preset, ceiling) },
		check: g.checkAMD,
	})
}

// amdLevelAuto is the amdgpu performance level of the vendor default.
const amdLevelAuto = "auto"

// amdDeviceDir is an amdgpu device's directory relative to /host-sys.
func amdDeviceDir(bus string) string {
	return pciDevicesRelDir + "/" + strings.ToLower(bus) + "/"
}

// isODWrite reports whether w edits pp_od_clk_voltage.
func isODWrite(w governor.Write) bool {
	return strings.HasSuffix(w.RelPath, "/pp_od_clk_voltage")
}

// odSclkMax reads the sclk maximum, the "1:" line of the OD_SCLK section,
// from pp_od_clk_voltage.
func odSclkMax(text string) (int, bool) {
	in := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, ":") {
			in = t == "OD_SCLK:"
			continue
		}
		key, value, ok := strings.Cut(t, ":")
		if !in || !ok || strings.TrimSpace(key) != "1" {
			continue
		}
		v := strings.TrimSpace(value)
		v = strings.TrimSuffix(strings.TrimSuffix(v, "Mhz"), "MHz")
		mhz, err := strconv.Atoi(strings.TrimSpace(v))
		return mhz, err == nil
	}
	return 0, false
}

// waitODReadback waits until each overdrive file no longer reads the
// command written to it, that is until the driver printed its table again.
func waitODReadback(ctx context.Context, writes []governor.Write) error {
	deadline := time.Now().Add(governorODSettle)
	for {
		pending := 0
		for _, w := range writes {
			if got, _ := readSysfsWord(hostSysPath(w.RelPath)); got == strings.TrimSpace(w.Value) {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d pp_od_clk_voltage file(s) did not print the table again within %s after %q", pending, governorODSettle, writes[0].Value)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(governorODPoll):
		}
	}
}

// writeAMDSteps runs the writes of every device in lockstep: the first
// write of each device, then the second, and so on, each read back before
// the next. Overdrive commands are not read back as written (the file
// prints the table): the step waits for every device's table instead, so
// "c" follows "s 1 <MHz>" only once the edit was taken, and the devices
// share one wait. It returns the writes it read back equal, the
// successful writes, and the failure as a status result and message.
func writeAMDSteps(ctx context.Context, steps [][]governor.Write) (expect []governor.Write, wrote int, result, message string) {
	for k := 0; ; k++ {
		var od []governor.Write
		more := false
		for _, writes := range steps {
			if k >= len(writes) {
				continue
			}
			more = true
			w := writes[k]
			if isODWrite(w) {
				if err := writeSysfsAttr(hostSysPath(w.RelPath), w.Value); err != nil {
					result, message = governor.Classify(err, w.Value, "")
					return expect, wrote, result, w.RelPath + ": " + message
				}
				wrote++
				od = append(od, w)
				continue
			}
			n, res, msg := writeVerified([]governor.Write{w})
			wrote += n
			if res != "" {
				return expect, wrote, res, msg
			}
			expect = append(expect, w)
		}
		if !more {
			return expect, wrote, "", ""
		}
		if len(od) > 0 {
			if err := waitODReadback(ctx, od); err != nil {
				return expect, wrote, joulie.GovernorResultError, err.Error()
			}
		}
	}
}

func (g *governorControl) applyAMD(ctx context.Context, d *gpuGovernorDiscovery, preset string, ceiling float64) governorOutcome {
	c := &g.gpu
	backend := joulie.GovernorBackendAMDGPUSysfs
	var steps [][]governor.Write
	var natives []string
	result := joulie.GovernorResultApplied
	for _, f := range d.AMD {
		plan, err := governor.PlanAMD(f, preset, ceiling)
		if err != nil {
			return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultError, message: err.Error()}}
		}
		if plan.Result == joulie.GovernorResultUnsupported {
			return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultUnsupported,
				message: fmt.Sprintf("preset %q is not supported by the GPU at %s (device %s)", preset, f.BusID, f.DeviceID)}}
		}
		if plan.Result == joulie.GovernorResultEquivalent {
			result = joulie.GovernorResultEquivalent
		}
		steps = append(steps, plan.Writes)
		natives = append(natives, plan.Native)
	}
	native := uniformOrMixed(natives)
	expect, wrote, res, msg := writeAMDSteps(ctx, steps)
	od := map[string]int{}
	for i, writes := range steps {
		for _, w := range writes {
			if mhz, ok := strings.CutPrefix(w.Value, "s 1 "); ok && isODWrite(w) && wrote > 0 {
				if v, err := strconv.Atoi(mhz); err == nil {
					od[w.RelPath] = v
					if c.odDevices == nil {
						c.odDevices = map[string]bool{}
					}
					c.odDevices[d.AMD[i].BusID] = true
				}
			}
		}
	}
	if res != "" {
		return governorOutcome{status: governorStatus{backend: backend, result: res, message: msg, applied: native}, wrote: wrote, attempted: true}
	}
	for rel, mhz := range od {
		text, _ := readSysfsWord(hostSysPath(rel))
		if got, ok := odSclkMax(text); !ok || got != mhz {
			return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultNotApplied,
				message: fmt.Sprintf("%s: wrote an sclk maximum of %d MHz but OD_SCLK reads %d", rel, mhz, got), applied: native}, wrote: wrote, attempted: true}
		}
	}
	c.expect, c.od = expect, od
	return governorOutcome{status: governorStatus{backend: backend, result: result, applied: native}, wrote: wrote, attempted: true}
}

// checkAMD compares the levels and overdrive ceilings the agent set with
// the devices. A level back at auto is a GPU reset or a driver reload.
func (g *governorControl) checkAMD() (ok, reset bool, msg string) {
	c := &g.gpu
	for _, w := range c.expect {
		got, _ := readSysfsWord(hostSysPath(w.RelPath))
		if got != w.Value {
			return false, got == amdLevelAuto, fmt.Sprintf("%s reads %q, the agent wrote %q", w.RelPath, got, w.Value)
		}
	}
	for rel, mhz := range c.od {
		text, _ := readSysfsWord(hostSysPath(rel))
		if got, ok := odSclkMax(text); !ok || got != mhz {
			return false, false, fmt.Sprintf("%s: OD_SCLK maximum reads %d, the agent set %d", rel, got, mhz)
		}
	}
	return true, false, ""
}

// restoreGPU writes the GPU baseline back.
func (g *governorControl) restoreGPU(ctx context.Context, d *gpuGovernorDiscovery) governorOutcome {
	if len(d.NVIDIA) > 0 {
		return g.restoreNvidia(ctx, d)
	}
	return g.restoreAMD(ctx, d)
}

// restoreAMD puts each device's baseline level back. Leaving manual for
// auto drops the overdrive range with it; for any other baseline the agent
// first resets the range it edited ("r", then "c").
func (g *governorControl) restoreAMD(ctx context.Context, d *gpuGovernorDiscovery) governorOutcome {
	c := &g.gpu
	backend := joulie.GovernorBackendAMDGPUSysfs
	b := g.gpuBaseline
	if b == nil {
		return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultError, message: "no baseline to restore"}}
	}
	present := map[string]governor.AMDFacts{}
	for _, f := range d.AMD {
		present[strings.ToLower(f.BusID)] = f
	}
	var odSteps, levels [][]governor.Write
	var natives []string
	for _, dev := range b.Devices {
		f, ok := present[strings.ToLower(dev.BusID)]
		if !ok || dev.Level == "" {
			continue
		}
		natives = append(natives, dev.Level)
		dir := amdDeviceDir(f.BusID)
		if f.OD && dev.Level != amdLevelAuto && (c.odDevices[f.BusID] || c.odDevices["*"]) {
			odSteps = append(odSteps, []governor.Write{{RelPath: dir + "pp_od_clk_voltage", Value: "r"}, {RelPath: dir + "pp_od_clk_voltage", Value: "c"}})
		}
		if w := differing([]governor.Write{{RelPath: dir + "power_dpm_force_performance_level", Value: dev.Level}}); len(w) > 0 {
			levels = append(levels, w)
		}
	}
	total := 0
	for _, steps := range [][][]governor.Write{odSteps, levels} {
		_, wrote, res, msg := writeAMDSteps(ctx, steps)
		total += wrote
		if res != "" {
			return governorOutcome{status: governorStatus{backend: backend, result: res, message: "restore: " + msg}, wrote: total, attempted: true}
		}
	}
	return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultRestored, message: "baseline restored",
		applied: uniformOrMixed(natives)}, wrote: total, attempted: total > 0}
}

// --- NVIDIA ------------------------------------------------------------

// seedNvidia turns the lock record of an earlier run (the applied field of
// its last status) into a record per GPU, once the GPUs are known.
func (g *governorControl) seedNvidia(d *gpuGovernorDiscovery) {
	c := &g.gpu
	if c.nvSeed == "" {
		return
	}
	if c.nv == nil {
		c.nv = map[string]*nvidiaRecord{}
	}
	locked := &[2]int{}
	for _, n := range d.NVIDIA {
		r := &nvidiaRecord{}
		if strings.Contains(c.nvSeed, "-lgc") {
			r.gpu = locked
		}
		if strings.Contains(c.nvSeed, "-lmc") {
			r.mem = locked
		}
		c.nv[n.BusID] = r
	}
	c.nvSeed = ""
}

// nvidiaLocked reports whether the record holds any lock.
func (g *governorControl) nvidiaLocked() bool {
	for _, r := range g.gpu.nv {
		if r.gpu != nil || r.mem != nil {
			return true
		}
	}
	return false
}

// nvidiaFacts is f with the agent's record of the GPU's locks.
func (g *governorControl) nvidiaFacts(n nvidiaGovernorFacts) governor.NVFacts {
	f := n.Facts
	if r := g.gpu.nv[n.BusID]; r != nil {
		f.LockedGPU, f.LockedMem = r.gpu != nil, r.mem != nil
	}
	return f
}

// parseLock reads "<min>,<max>" as nvidia-smi is told a lock.
func parseLock(v string) nvidiaLock {
	lo, hi, ok := strings.Cut(v, ",")
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if !ok || err1 != nil || err2 != nil {
		return nil
	}
	return &[2]int{a, b}
}

// recordNvidia updates the record after one successful command.
func (g *governorControl) recordNvidia(bus string, args []string) {
	c := &g.gpu
	if c.nv == nil {
		c.nv = map[string]*nvidiaRecord{}
	}
	r := c.nv[bus]
	if r == nil {
		r = &nvidiaRecord{}
		c.nv[bus] = r
	}
	for i, a := range args {
		switch a {
		case "-lgc", "-lmc":
			if i+1 < len(args) {
				if a == "-lgc" {
					r.gpu = parseLock(args[i+1])
				} else {
					r.mem = parseLock(args[i+1])
				}
			}
		case "-rgc":
			r.gpu = nil
		case "-rmc":
			r.mem = nil
		}
	}
}

// runNvidiaPlans runs every GPU's commands in order and records each lock.
func (g *governorControl) runNvidiaPlans(ctx context.Context, plans map[string]governor.Plan, order []string) (issued int, result, message string) {
	for _, bus := range order {
		for _, args := range plans[bus].Args {
			if _, err := runCommand(ctx, "nvidia-smi", args...); err != nil {
				result, message = governor.Classify(err, "", "")
				return issued, result, message
			}
			issued++
			g.recordNvidia(bus, args)
		}
	}
	return issued, "", ""
}

func (g *governorControl) applyNvidia(ctx context.Context, d *gpuGovernorDiscovery, preset string, ceiling float64) governorOutcome {
	backend := joulie.GovernorBackendNvidiaSMI
	plans := map[string]governor.Plan{}
	var order, natives []string
	result := joulie.GovernorResultApplied
	for _, n := range d.NVIDIA {
		plan, err := governor.PlanNvidia(g.nvidiaFacts(n), n.BusID, preset, ceiling)
		if err != nil {
			return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultError, message: err.Error()}}
		}
		if plan.Result == joulie.GovernorResultUnsupported {
			return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultUnsupported,
				message: fmt.Sprintf("preset %q is not supported by the GPU at %s", preset, n.BusID)}}
		}
		if plan.Result == joulie.GovernorResultEquivalent {
			result = joulie.GovernorResultEquivalent
		}
		plans[n.BusID] = plan
		order = append(order, n.BusID)
		natives = append(natives, plan.Native)
	}
	native := uniformOrMixed(natives)
	issued, res, msg := g.runNvidiaPlans(ctx, plans, order)
	if res != "" {
		return governorOutcome{status: governorStatus{backend: backend, result: res, message: msg, applied: native}, wrote: issued, attempted: true}
	}
	if result == joulie.GovernorResultApplied && g.nvidiaLocked() && !g.nvidiaVerified() {
		result = joulie.GovernorResultAppliedUnverified
	}
	return governorOutcome{status: governorStatus{backend: backend, result: result, applied: native}, wrote: issued, attempted: true}
}

// nvidiaVerified reports whether every locked GPU has its samples.
func (g *governorControl) nvidiaVerified() bool {
	c := &g.gpu
	for bus, r := range c.nv {
		if (r.gpu != nil || r.mem != nil) && c.nvGood[bus] < nvidiaVerifySamples {
			return false
		}
	}
	return true
}

// nvidiaSample is one GPU's row of the clock query.
type nvidiaSample struct {
	smMHz, memMHz float64
	busy          bool
}

// nvidiaSamples runs the discovery's clock query: per bus id, the current
// clocks and whether the GPU is busy (the idle bit of the event reasons
// clear).
func nvidiaSamples(ctx context.Context) map[string]nvidiaSample {
	out, err := runCommand(ctx, "nvidia-smi", nvidiaClockQueryArgs...)
	if err != nil {
		return nil
	}
	res := map[string]nvidiaSample{}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := splitCSVLine(ln)
		if len(p) < 7 {
			continue
		}
		sm, err1 := strconv.ParseFloat(p[1], 64)
		mem, err2 := strconv.ParseFloat(p[2], 64)
		reasons, err3 := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(p[6]), "0x"), 16, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		res[p[0]] = nvidiaSample{smMHz: sm, memMHz: mem, busy: reasons&nvidiaReasonGPUIdle == 0}
	}
	return res
}

// verifyNvidia counts the busy samples that sit within each locked GPU's
// locks, and turns applied-unverified into applied once every locked GPU
// has nvidiaVerifySamples of them. A lock cannot be read back, so this is
// the only evidence one holds. It always reports the setting as holding.
func (g *governorControl) verifyNvidia(ctx context.Context) bool {
	c := &g.gpu
	if c.status.result != joulie.GovernorResultAppliedUnverified {
		return true
	}
	samples := nvidiaSamples(ctx)
	for bus, r := range c.nv {
		s, ok := samples[bus]
		if !ok || !s.busy || (r.gpu == nil && r.mem == nil) {
			continue
		}
		within := func(l nvidiaLock, v float64) bool {
			return l == nil || (v <= float64(l[1])*nvidiaVerifyTolerance && v >= float64(l[0])/nvidiaVerifyTolerance)
		}
		// A busy GPU runs at the top of its range, so a lock's floor says
		// nothing for the graphics clock; only its ceiling is checked.
		gpuOK := r.gpu == nil || s.smMHz <= float64(r.gpu[1])*nvidiaVerifyTolerance
		if gpuOK && within(r.mem, s.memMHz) {
			c.nvGood[bus]++
		} else {
			c.nvGood[bus] = 0
		}
	}
	if g.nvidiaVerified() {
		c.status.result = joulie.GovernorResultApplied
		c.status.message = fmt.Sprintf("verified by %d busy samples per GPU", nvidiaVerifySamples)
		c.good = c.status
		g.setState(c)
	}
	return true
}

// nvidiaPersistenceOff lists the GPUs whose persistence mode is Disabled:
// the driver unloads when no client holds them and may forget their locks.
func nvidiaPersistenceOff(ctx context.Context) (map[string]bool, error) {
	out, err := runCommand(ctx, "nvidia-smi", "--query-gpu=pci.bus_id,persistence_mode", "--format=csv,noheader")
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := splitCSVLine(ln)
		if len(p) >= 2 && strings.EqualFold(p[1], "Disabled") {
			res[p[0]] = true
		}
	}
	return res, nil
}

// nvidiaReissueDue reports whether the locks in the record are due again:
// every reconcile while a locked GPU has persistence mode off, else every
// governorReapplyEvery.
func (g *governorControl) nvidiaReissueDue(ctx context.Context, now time.Time) bool {
	if !g.nvidiaLocked() {
		return false
	}
	if now.Sub(g.gpu.lastApply) >= governorReapplyEvery {
		return true
	}
	off, err := nvidiaPersistenceOff(ctx)
	if err != nil {
		return false
	}
	for bus, r := range g.gpu.nv {
		if (r.gpu != nil || r.mem != nil) && off[bus] {
			return true
		}
	}
	return false
}

// restoreNvidia resets the locks in the agent's record (-rgc, -rmc); a lock
// the agent did not set is left alone.
func (g *governorControl) restoreNvidia(ctx context.Context, d *gpuGovernorDiscovery) governorOutcome {
	backend := joulie.GovernorBackendNvidiaSMI
	plans := map[string]governor.Plan{}
	var order []string
	for _, n := range d.NVIDIA {
		plan, err := governor.PlanNvidia(g.nvidiaFacts(n), n.BusID, joulie.GPUGovernorPresetAuto, 0)
		if err != nil || len(plan.Args) == 0 {
			continue
		}
		plans[n.BusID] = plan
		order = append(order, n.BusID)
	}
	issued, res, msg := g.runNvidiaPlans(ctx, plans, order)
	if res != "" {
		return governorOutcome{status: governorStatus{backend: backend, result: res, message: "restore: " + msg}, wrote: issued, attempted: true}
	}
	return governorOutcome{status: governorStatus{backend: backend, result: joulie.GovernorResultRestored, message: "the agent's clock locks reset",
		applied: joulie.GovernorStateUnlocked}, wrote: issued, attempted: issued > 0}
}

// --- exit --------------------------------------------------------------

// signalContext is the context the agent runs under: it ends on SIGTERM,
// which the kubelet sends when it stops the pod, or SIGINT.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

// sleepContext waits d, or less when ctx ends.
func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// governorExitTimeout bounds the restore on exit, well inside the pod's
// default termination grace period of 30 s.
const governorExitTimeout = 10 * time.Second

// restoreGovernorsOnExit restores the baseline of every component a
// controller changed, when GOVERNOR_RESTORE_ON_EXIT is true. A SIGKILL or an
// OOM kill restores nothing; the baseline in NodeHardware covers the next
// start.
func restoreGovernorsOnExit(dyn dynamic.Interface, controllers ...*NodeController) {
	ctx, cancel := context.WithTimeout(context.Background(), governorExitTimeout)
	defer cancel()
	for _, nc := range controllers {
		if nc == nil || nc.governor == nil {
			continue
		}
		nc.governor.restoreOnExit(ctx, dyn)
	}
}

func (g *governorControl) restoreOnExit(ctx context.Context, dyn dynamic.Interface) {
	if !g.cfg.restoreOnExit || !g.loaded {
		return
	}
	env := governorEnv{}
	d := g.last
	var cpuRestore, gpuRestore func() governorOutcome
	if d.CPU != nil {
		cpuRestore = func() governorOutcome { return g.restoreCPU(d.CPU) }
	}
	if d.GPU != nil {
		gpuRestore = func() governorOutcome { return g.restoreGPU(ctx, d.GPU) }
	}
	for _, p := range []struct {
		c       *governorComponent
		restore func() governorOutcome
	}{{&g.cpu, cpuRestore}, {&g.gpu, gpuRestore}} {
		if !p.c.changed {
			continue
		}
		p.c.retryAt = time.Time{}
		g.release(ctx, dyn, p.c, env, p.restore)
		log.Printf("governor %s node=%s on exit: %s %s", p.c.label, g.nodeName, p.c.status.result, p.c.status.message)
	}
}
