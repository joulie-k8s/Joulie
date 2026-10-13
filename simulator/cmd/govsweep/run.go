package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// sweepEpoch is the virtual time every cell starts at, so a cell's output
// does not depend on when it ran.
var sweepEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Statuses of a row.
const (
	statusOK          = "ok"
	statusUnsupported = "unsupported" // the arm cannot be set: nothing ran
	statusTimeout     = "timeout"     // the job did not finish within the limit
	statusFailed      = "failed"      // the arm was set with another result
)

// Row is the outcome of one cell. Energies are in J over the measured
// window: the job from its start to its completion, or DurationS for an
// idle cell; TimeS is that window. Utilization and frequency are the means
// over it of what the node's CPU packages report.
type Row struct {
	Cell   Cell
	Status string
	Arm    armResult

	TimeS                      float64
	HostJ, CPUJ, MemoryJ, GPUJ float64
	MeanHostW                  float64
	MeanCPUUtil, MeanCPUMHz    float64
	BurstP50MS, BurstP95MS     float64
	PeerRxBytes                float64
	PeerGBps                   float64
	MeanComputeMHz, MeanMemMHz float64
	MinThroughput, MinPeerMul  float64
	CapW                       float64
}

// ledger is the energy of a node at one time.
type ledger struct {
	t                   time.Time
	host, cpu, mem, gpu float64
	peerRx              float64
}

func readLedger(s hwemu.State) ledger {
	l := ledger{t: s.Time, host: s.Host.EnergyJ}
	for _, c := range s.Components {
		switch c.Type {
		case "cpu":
			l.cpu += c.EnergyJ
		case "memory":
			l.mem += c.EnergyJ
		case "gpu":
			l.gpu += c.EnergyJ
			l.peerRx += float64(c.PeerRxBytes)
		}
	}
	return l
}

// lerp is the ledger a fraction f of the way from a to b, for a job that
// ends inside a step: the powers are constant within the step's substeps,
// so energy is close to linear in it.
func lerp(a, b ledger, f float64) ledger {
	m := func(x, y float64) float64 { return x + f*(y-x) }
	return ledger{
		t:    a.t.Add(time.Duration(f * float64(b.t.Sub(a.t)))),
		host: m(a.host, b.host), cpu: m(a.cpu, b.cpu), mem: m(a.mem, b.mem), gpu: m(a.gpu, b.gpu),
		peerRx: m(a.peerRx, b.peerRx),
	}
}

// runner runs cells against a set of profiles under a work directory.
type runner struct {
	cfg      *Config
	profiles func(variant string, sens func(*hwemu.Profile) bool, name string) (*hwemu.Profile, bool, error)
	work     string
	keep     bool
	resource telemetry.Resource
}

// run runs one cell in its own root, cells/<id> under the work directory,
// and returns its row and its OTLP export. The root is removed afterwards
// unless keep is set.
func (r *runner) run(ctx context.Context, c Cell) (Row, otlpjson.MetricsData, error) {
	row := Row{Cell: c}
	p, ok, err := r.profiles(c.Sens, c.sensFn, c.Profile)
	if err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	if !ok {
		return row, otlpjson.MetricsData{}, errNoVariant
	}
	root := filepath.Join(r.work, "cells", c.ID())
	if err := os.RemoveAll(root); err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	if !r.keep {
		defer os.RemoveAll(root)
	}
	tree, err := hwemu.Render(p, root, hwemu.RenderOptions{NodeName: c.ID(), Seed: c.Seed})
	if err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	substep := time.Duration(r.cfg.SubstepMS) * time.Millisecond
	if c.Load == loadBursty {
		substep = time.Duration(r.cfg.CPU.Bursty.SubstepMS) * time.Millisecond
	}
	gate := c.Gate
	if gate == "" {
		gate = hwemu.ReceiverGateProfile
	}
	// The seed moves the start against the substep grid, by up to one
	// substep, so seeds see the job's edges at other phases of the physics.
	phase := time.Duration(rand.New(rand.NewPCG(c.Seed, 1)).Int64N(int64(substep)))
	node, err := hwemu.NewNode(p, tree, hwemu.Options{Substep: substep, Start: sweepEpoch.Add(phase), RenderCheckInterval: time.Hour, ReceiverGate: gate})
	if err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	if err := node.Step(0); err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	switch c.Grid {
	case gridCPU:
		row.Arm, err = applyCPU(node, tree, c)
	default:
		row.Arm, err = applyGPU(ctx, node, tree, p, c)
	}
	if err != nil {
		return row, otlpjson.MetricsData{}, fmt.Errorf("set the arm: %w", err)
	}
	switch row.Arm.Result {
	case joulie.GovernorResultUnsupported:
		row.Status = statusUnsupported
		return row, otlpjson.MetricsData{}, nil
	case "", joulie.GovernorResultApplied, joulie.GovernorResultEquivalent:
	default:
		row.Status = statusFailed
		return row, otlpjson.MetricsData{}, nil
	}

	cpus := p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * p.CPU.ThreadsPerCore.V
	idle := hwemu.Load{}
	if c.Grid == gridGPU {
		idle.CPUUtil = []float64{r.cfg.GPU.CPUUtil}
	}
	node.SetLoad(idle)
	if err := stepFor(node, time.Duration(r.cfg.SettleS*float64(time.Second)), time.Duration(r.cfg.StepS*float64(time.Second))); err != nil {
		return row, otlpjson.MetricsData{}, err
	}
	m := &meter{node: node}
	switch c.Load {
	case loadIdle:
		if c.Grid == gridGPU {
			if err := applyCap(ctx, node, tree, p, c); err != nil {
				return row, otlpjson.MetricsData{}, fmt.Errorf("set the cap: %w", err)
			}
		}
		err = r.runIdle(m, idle)
	case loadSteady:
		err = r.runSteady(m, c, cpus)
	case loadBursty:
		err = r.runBursty(m, c, &row)
	case loadJob:
		err = r.runGPUJob(ctx, m, c, tree, p, &row)
	}
	switch {
	case err == errTimeout:
		row.Status = statusTimeout
	case err != nil:
		return row, otlpjson.MetricsData{}, err
	}
	m.fill(&row)
	if row.Status == "" {
		row.Status = statusOK
	}
	return row, r.export(node, c, p), nil
}

// stepFor advances the node by d in steps of at most step.
func stepFor(n *hwemu.Node, d, step time.Duration) error {
	for d > 0 {
		s := min(step, d)
		if err := n.Step(s); err != nil {
			return err
		}
		d -= s
	}
	return nil
}

// meter accumulates what the measured window shows: its ledgers at start
// and end and time-weighted means of the CPU packages' utilization and
// frequency and of the GPUs' clocks.
type meter struct {
	node       *hwemu.Node
	start, end ledger
	w          float64 // seconds summed into the means
	util, mhz  float64
	gpuW       float64
	compute    float64
	mem        float64
	minThr     float64
	minPeer    float64
	started    bool
}

func (m *meter) begin() {
	m.start = readLedger(m.node.State())
	m.end = m.start
	m.minThr, m.minPeer = 1, 1
	m.started = true
}

// sample adds the state after a step of dt seconds to the means.
func (m *meter) sample(s hwemu.State, dt float64, gpus []int) {
	var util, mhz float64
	k := 0
	for _, c := range s.Components {
		if c.Type == "cpu" {
			util += c.Utilization
			mhz += c.SpeedHz / 1e6
			k++
		}
	}
	if k > 0 {
		m.util += dt * util / float64(k)
		m.mhz += dt * mhz / float64(k)
	}
	if len(gpus) > 0 {
		var cmp, mem float64
		for _, c := range s.Components {
			if c.Type == "gpu" && slices.Contains(gpus, c.Index) {
				cmp += float64(c.ClockHz["graphics"]) / 1e6
				mem += float64(c.ClockHz["memory"]) / 1e6
			}
		}
		m.compute += dt * cmp / float64(len(gpus))
		m.mem += dt * mem / float64(len(gpus))
		m.gpuW += dt
	}
	m.w += dt
}

func (m *meter) fill(row *Row) {
	d := m.end
	s := m.start
	row.TimeS = d.t.Sub(s.t).Seconds()
	row.HostJ, row.CPUJ, row.MemoryJ, row.GPUJ = d.host-s.host, d.cpu-s.cpu, d.mem-s.mem, d.gpu-s.gpu
	row.PeerRxBytes = d.peerRx - s.peerRx
	if row.TimeS > 0 {
		row.MeanHostW = row.HostJ / row.TimeS
		row.PeerGBps = row.PeerRxBytes / row.TimeS / 1e9
	}
	if m.w > 0 {
		row.MeanCPUUtil, row.MeanCPUMHz = m.util/m.w, m.mhz/m.w
	}
	if m.gpuW > 0 {
		row.MeanComputeMHz, row.MeanMemMHz = m.compute/m.gpuW, m.mem/m.gpuW
	}
	row.MinThroughput, row.MinPeerMul = m.minThr, m.minPeer
}

// runIdle measures the node idle for DurationS.
func (r *runner) runIdle(m *meter, idle hwemu.Load) error {
	m.node.SetLoad(idle)
	m.begin()
	step := time.Duration(r.cfg.StepS * float64(time.Second))
	for left := time.Duration(r.cfg.DurationS * float64(time.Second)); left > 0; {
		s := min(step, left)
		if err := m.node.Step(s); err != nil {
			return err
		}
		m.sample(m.node.State(), s.Seconds(), nil)
		left -= s
	}
	m.end = readLedger(m.node.State())
	return nil
}

// cpuRate is the v1 workload engine's CPU rate (workload.Rates) of a job on
// one core at base speed 1 with the node's CPU throughput mul: work units per
// second, 1 at the reference.
func cpuRate(c Cell, util, mul float64) float64 {
	rate, _ := workload.Rates(workload.ModelV1, workload.RateInputs{
		BaseSpeed: 1, CPURequest: 1, CPUUtil: util,
		MemoryIntensity: c.Intensity.MemoryIntensity, IOIntensity: c.Intensity.IOIntensity,
		SensitivityCPU: 1, SensitivityGPU: 1, CPUMul: mul, GPUMul: 1, GPUCapFactor: 1, CPUShare: 1, GPUShare: 1,
	})
	return rate
}

// runSteady runs a job of DurationS work units at Pct percent of every
// logical CPU: the work the job does at the reference in DurationS seconds,
// progressing at the v1 engine's rate under the node's CPU throughput,
// which a slow governor lowers (hwemu.Node.CPUThroughput, against a
// governor-independent reference).
func (r *runner) runSteady(m *meter, c Cell, cpus int) error {
	u := c.Pct / 100
	all := make([]int, cpus)
	for i := range all {
		all[i] = i
	}
	m.node.SetLoad(hwemu.Load{CPUUtil: []float64{u}, CPUClass: c.Class, MemoryIntensity: c.Intensity.MemoryIntensity})
	m.begin()
	work := r.cfg.DurationS
	step := time.Duration(r.cfg.StepS * float64(time.Second))
	limit := m.start.t.Add(time.Duration(r.cfg.LimitFactor * r.cfg.DurationS * float64(time.Second)))
	prev := m.start
	for done := 0.0; ; {
		if err := m.node.Step(step); err != nil {
			return err
		}
		st := m.node.State()
		now := readLedger(st)
		mul := m.node.CPUThroughput(all, c.Class, c.Intensity.MemoryIntensity, c.Intensity.IOIntensity)
		m.minThr = math.Min(m.minThr, mul)
		d := cpuRate(c, u, mul) * step.Seconds()
		if done+d >= work {
			f := (work - done) / d
			m.sample(st, f*step.Seconds(), nil)
			m.end = lerp(prev, now, f)
			return nil
		}
		m.sample(st, step.Seconds(), nil)
		done += d
		prev = now
		if !now.t.Before(limit) {
			m.end = now
			return errTimeout
		}
	}
}

var errTimeout = fmt.Errorf("the job did not finish within the limit")

// runBursty runs Bursts bursts, each BurstMS of work at full load on every
// CPU, arriving every PeriodMS plus a seeded jitter, in steps of one
// substep. Between bursts the node idles. TimeS ends with the last burst;
// the burst latencies are arrival to completion.
func (r *runner) runBursty(m *meter, c Cell, row *Row) error {
	b := r.cfg.CPU.Bursty
	rng := rand.New(rand.NewPCG(c.Seed, 2))
	period := time.Duration(b.PeriodMS) * time.Millisecond
	step := time.Duration(b.SubstepMS) * time.Millisecond
	arrivals := make([]time.Duration, b.Bursts)
	for i := range arrivals {
		var j time.Duration
		if b.JitterMS > 0 {
			// Whole steps, so a burst arrives at a step boundary.
			j = time.Duration(rng.Int64N(int64(b.JitterMS))) * time.Millisecond / step * step
		}
		arrivals[i] = time.Duration(i)*period + j
	}
	cpus := r.cpuList(c)
	burstWork := float64(b.BurstMS) / 1000
	limit := time.Duration(r.cfg.LimitFactor*float64(b.Bursts)) * period
	var lat []float64
	var queue []time.Duration // arrival times of the bursts not yet done
	next, left, busy := 0, 0.0, false
	m.begin()
	for t := time.Duration(0); ; t += step {
		for next < len(arrivals) && arrivals[next] <= t {
			queue = append(queue, arrivals[next])
			if len(queue) == 1 {
				left = burstWork
			}
			next++
		}
		if want := len(queue) > 0; want != busy {
			l := hwemu.Load{}
			if want {
				l = hwemu.Load{CPUUtil: []float64{1}, CPUClass: c.Class, MemoryIntensity: c.Intensity.MemoryIntensity}
			}
			m.node.SetLoad(l)
			busy = want
		}
		if err := m.node.Step(step); err != nil {
			return err
		}
		s := m.node.State()
		m.sample(s, step.Seconds(), nil)
		m.end = readLedger(s)
		if busy {
			mul := m.node.CPUThroughput(cpus, c.Class, c.Intensity.MemoryIntensity, c.Intensity.IOIntensity)
			m.minThr = math.Min(m.minThr, mul)
			for d := cpuRate(c, 1, mul) * step.Seconds(); d > 0 && len(queue) > 0; {
				if d < left {
					left -= d
					break
				}
				d -= left
				lat = append(lat, (t+step-queue[0]).Seconds()*1000)
				queue = queue[1:]
				left = burstWork
			}
		}
		if next == len(arrivals) && len(queue) == 0 {
			break
		}
		if t >= limit {
			return errTimeout
		}
	}
	row.BurstP50MS, row.BurstP95MS = percentile(lat, 50), percentile(lat, 95)
	return nil
}

// cpuList is every logical CPU of c's profile.
func (r *runner) cpuList(c Cell) []int {
	p, _, _ := r.profiles(c.Sens, c.sensFn, c.Profile)
	n := p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * p.CPU.ThreadsPerCore.V
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// percentile is the q-th percentile of xs by nearest rank, 0 for none.
func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(q/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// runGPUJob runs a job on GPUs 0..GPUs-1 at full load (a gather job on
// 1..GPUs-1, see Cell.Transfer): DurationS x GPUs
// work units at the reference, progressing at the v1 engine's GPU rate
// (workload.Rates) under the slowest of its GPUs' throughput
// (hwemu.Node.GPUThroughput, which the cap and the compute ceiling lower)
// and, with transfers, the slowest transfer's rate over its demand
// (hwemu.Node.PeerTransferRates), as the simulator's workload engine counts
// them. Peer transfers form a ring over the job's GPUs, each demanding
// TransferIntensity of the peer link; NIC transfers come from the host into
// each GPU at the same demand and pass no receiver gate.
func (r *runner) runGPUJob(ctx context.Context, m *meter, c Cell, tree *hwemu.Tree, p *hwemu.Profile, row *Row) error {
	n := min(c.GPUs, p.GPUs.Count.V)
	gpus := make([]int, n)
	util := make([]float64, p.GPUs.Count.V)
	for i := range gpus {
		gpus[i] = i
		util[i] = 1
	}
	// A gather job computes on GPUs 1..n-1, which send to GPU 0: a
	// receiver that runs nothing of its own.
	workers := gpus
	if c.Transfer == transferGather {
		util[0] = 0
		workers = gpus[1:]
	}
	linkBps := 0.0
	if t := p.GPUs.Transfer; t != nil {
		linkBps = t.PeerLinkGBps.V * 1e9
	}
	if linkBps <= 0 {
		linkBps = 32e9 // a PCIe Gen4 x16 link, for a profile without one
	}
	var ts []hwemu.PeerTransfer
	demand := c.TransferIntensity * linkBps
	switch c.Transfer {
	case transferPeer:
		for k, d := range gpus {
			ts = append(ts, hwemu.PeerTransfer{Src: d, Dst: gpus[(k+1)%n], BytesPerS: demand})
		}
	case transferNIC:
		for _, d := range gpus {
			ts = append(ts, hwemu.PeerTransfer{Src: phys.HostSource, Dst: d, BytesPerS: demand})
		}
	case transferGather:
		for _, d := range workers {
			ts = append(ts, hwemu.PeerTransfer{Src: d, Dst: 0, BytesPerS: demand})
		}
	}
	m.node.SetLoad(hwemu.Load{CPUUtil: []float64{r.cfg.GPU.CPUUtil}, GPUUtil: util, GPUClass: c.Class,
		MemoryIntensity: c.Intensity.MemoryIntensity, PeerTransfers: ts})
	if err := applyCap(ctx, m.node, tree, p, c); err != nil {
		return fmt.Errorf("set the cap: %w", err)
	}
	for _, g := range m.node.State().GPUs {
		if slices.Contains(workers, g.Index) {
			row.CapW += g.LimitW / float64(len(workers))
		}
	}
	m.begin()
	work := r.cfg.DurationS * float64(len(workers))
	step := time.Duration(r.cfg.StepS * float64(time.Second))
	limit := m.start.t.Add(time.Duration(r.cfg.LimitFactor * r.cfg.DurationS * float64(time.Second)))
	prev := m.start
	for done := 0.0; ; {
		if err := m.node.Step(step); err != nil {
			return err
		}
		st := m.node.State()
		now := readLedger(st)
		thr := 1.0
		for _, g := range workers {
			thr = math.Min(thr, m.node.GPUThroughput(g))
		}
		peer := 1.0
		if len(ts) > 0 {
			rates := m.node.PeerTransferRates()
			if len(rates) == len(ts) {
				for i, t := range ts {
					peer = math.Min(peer, math.Max(0, math.Min(1, rates[i]/t.BytesPerS)))
				}
			}
		}
		m.minThr, m.minPeer = math.Min(m.minThr, thr), math.Min(m.minPeer, peer)
		in := workload.RateInputs{BaseSpeed: 1, GPURequest: float64(len(workers)), SensitivityCPU: 1, SensitivityGPU: 1,
			CPUMul: 1, GPUMul: thr, GPUCapFactor: 1, CPUShare: 1, GPUShare: 1}
		if len(ts) > 0 {
			in.PeerTransferIntensity, in.PeerMul = c.TransferIntensity, peer
		}
		_, rate := workload.Rates(workload.ModelV1, in)
		d := rate * step.Seconds()
		if done+d >= work {
			f := (work - done) / d
			m.sample(st, f*step.Seconds(), gpus)
			m.end = lerp(prev, now, f)
			return nil
		}
		m.sample(st, step.Seconds(), gpus)
		done += d
		prev = now
		if !now.t.Before(limit) {
			m.end = now
			return errTimeout
		}
	}
}

// export is the cell's OTLP metrics export: the node's state at the end of
// the cell, as the simulator exports an emulated node (telemetry.NodeRequest),
// under the semconv names the controller manager reads. The node is the
// cell: its host name is the cell id.
func (r *runner) export(n *hwemu.Node, c Cell, p *hwemu.Profile) otlpjson.MetricsData {
	st := n.State()
	snap := emulate.NodeSnapshot{
		Name: c.ID(), Pool: c.Grid, Profile: p.Name, StartTime: sweepEpoch, DriverVersion: st.DriverVersion,
		Host: emulate.HostReading{Measured: st.Host.Measured, PowerW: st.Host.PowerW, EnergyJ: st.Host.EnergyJ, OtherW: st.Host.OtherW},
	}
	for _, comp := range st.Components {
		snap.Components = append(snap.Components, emulate.ComponentReading(comp))
	}
	d, _ := telemetry.NodeRequest(snap, r.cfg.Name, st.Time, r.resource)
	return d
}
