package emulate

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Tick steps every member to now and publishes the snapshot.
//
// Each member, on a worker: its load is loads[name], plus its BaseLoad when
// applyBase is set, clamped to 0..1 per element; the node is stepped to now;
// its ledger adds the increase of each energy counter and is persisted
// before the snapshot is built. Then the facility: IT power is the sum of
// the hosts' power, passed to fac (the facility of Config.Facility when
// nil), the facility ledger integrates IT and cooling power, racks sum their
// hosts, zones are the ambient plus their offset, and each host's inlet is
// its zone's temperature, else the ambient. The facility ledger is
// persisted, Seq grows by 1 and the snapshot is published.
//
// A member whose step or ledger write fails keeps the readings of its last
// good step, with Err set, while the others advance; the error is counted,
// logged when it changes, and returned with the others, joined. The
// snapshot is published even then. A now before the last tick is refused
// and publishes nothing.
func (e *Emulator) Tick(now time.Time, loads map[string]hwemu.Load, applyBase bool, fac FacilityFunc) (*Snapshot, error) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if e.closed {
		return nil, errClosed
	}
	if now.Before(e.lastTick) {
		return nil, fmt.Errorf("emulate: tick at %v is before the last tick at %v", now, e.lastTick)
	}
	begin := time.Now()

	e.parallel(len(e.members), func(i int) {
		m := e.members[i]
		job, hasJob := loads[m.spec.Name]
		e.stepMember(m, now, emulatorLoad(job, hasJob, m.spec.BaseLoad, applyBase, m.sockets, m.gpus))
	})

	nodes := make([]NodeSnapshot, len(e.members))
	var errs []error
	for i, m := range e.members {
		nodes[i] = m.snap
		if m.err != nil {
			errs = append(errs, fmt.Errorf("emulate: node %s: %w", m.spec.Name, m.err))
		}
	}
	facSnap, err := e.tickFacility(now, nodes, fac)
	if err != nil {
		errs = append(errs, err)
	}

	e.seq++
	e.lastTick = now
	snap := &Snapshot{Seq: e.seq, Cluster: e.cluster, Time: now, Nodes: nodes, Facility: facSnap}
	e.latest.Store(snap)
	e.metrics.tickDuration.Observe(time.Since(begin).Seconds())
	return snap, errors.Join(errs...)
}

// stepMember steps one member to now with load l and leaves its snapshot,
// its host energy increase and its error in m.
func (e *Emulator) stepMember(m *member, now time.Time, l hwemu.Load) {
	if m.lastLoad == nil || !emulatorLoadEqual(*m.lastLoad, l) {
		m.runner.SetLoad(l)
		m.lastLoad = &l
	}
	err := m.runner.AdvanceTo(now)
	st := m.runner.State()
	if err == nil {
		m.accrue(st, e.logf)
		if werr := ledgerWrite(m.tmpDir, ledgerTmpPrefix, m.ledgerPath, m.ledger); werr != nil {
			err = fmt.Errorf("persist the ledger: %w", werr)
		}
	}

	prevHostJ := m.last.Host.EnergyJ
	if err == nil {
		m.last = m.reading(st)
	}
	snap := m.last
	snap.Events = append([]hwemu.Event(nil), st.Events...)
	if !m.published {
		snap.Started, snap.Fresh, snap.Added = true, m.report.Fresh, m.report.Added
		m.published = true
	}
	m.hostDelta = snap.Host.EnergyJ - prevHostJ
	m.err = err
	if err != nil {
		snap.Err = err.Error()
		e.metrics.stepErrors.WithLabelValues(m.spec.Name).Inc()
		if snap.Err != m.lastErr {
			e.logf("emulate: node %s: %v; keeping the readings of its last good step", m.spec.Name, err)
		}
	} else if m.lastErr != "" {
		e.logf("emulate: node %s steps again", m.spec.Name)
	}
	m.lastErr = snap.Err
	m.snap = snap
}

// accrue adds the increase of the runner's counters since the last tick to
// the member's ledger: the energy of the host and of each component, and the
// peer bytes of each GPU.
func (m *member) accrue(st hwemu.State, logf func(string, ...any)) {
	if ledgerAccrue(&m.ledger.HostJ, &m.hostBase, st.Host.EnergyJ) {
		logf("emulate: node %s: host energy went back to %g J; counted as no increase", m.spec.Name, st.Host.EnergyJ)
	}
	for _, c := range st.Components {
		total, base := m.ledger.ComponentsJ[c.ID], m.compBase[c.ID]
		if ledgerAccrue(&total, &base, c.EnergyJ) {
			logf("emulate: node %s: %s energy went back to %g J; counted as no increase", m.spec.Name, c.ID, c.EnergyJ)
		}
		m.ledger.ComponentsJ[c.ID], m.compBase[c.ID] = total, base
		if c.Type == semconv.HwTypeGPU && m.ledger.accrueGPUIO(c.ID, c.PeerRxBytes, c.PeerTxBytes) {
			logf("emulate: node %s: %s peer bytes went back to %d received and %d sent; counted as no increase",
				m.spec.Name, c.ID, c.PeerRxBytes, c.PeerTxBytes)
		}
	}
}

// reading is the member's snapshot for state st, with the ledger's energy
// and GPU bytes, and the governor states the components left. The facility
// fills Host.InletC.
func (m *member) reading(st hwemu.State) NodeSnapshot {
	r := m.last
	r.Started, r.Fresh, r.Added, r.Events, r.Err = false, false, nil, nil, ""
	r.DriverVersion = st.DriverVersion
	r.Host = HostReading{
		Measured: st.Host.Measured,
		PowerW:   st.Host.PowerW,
		EnergyJ:  m.ledger.HostJ,
		OtherW:   st.Host.OtherW,
	}
	r.Components = make([]ComponentReading, len(st.Components))
	for i, c := range st.Components {
		cr := ComponentReading(c)
		cr.EnergyJ = m.ledger.ComponentsJ[c.ID]
		io := m.ledger.GPUIO[c.ID]
		cr.PeerRxBytes, cr.PeerTxBytes = io.PeerRxBytes, io.PeerTxBytes
		r.Components[i] = cr
	}
	r.PreviousGovernors = readingPreviousGovernors(m.last, r.Components)
	return r
}

// readingPreviousGovernors is PreviousGovernors of a reading with comps after
// the last good reading last. A component whose governor state changed since
// last left the state last shows, when it had one; any other keeps the
// state last says it left. A state equal to the current one is no state left.
// The map is new, since a published snapshot is never changed, and nil when
// it would be empty.
func readingPreviousGovernors(last NodeSnapshot, comps []ComponentReading) map[string]GovernorState {
	was := make(map[string]GovernorState, len(last.Components))
	for _, c := range last.Components {
		was[c.ID] = c.Governor()
	}
	var out map[string]GovernorState
	for _, c := range comps {
		now := c.Governor()
		left, ok := last.PreviousGovernors[c.ID]
		if prev := was[c.ID]; prev != now && prev != (GovernorState{}) {
			left, ok = prev, true
		}
		if ok && left != now {
			if out == nil {
				out = map[string]GovernorState{}
			}
			out[c.ID] = left
		}
	}
	return out
}

// tickFacility computes the facility after the members stepped, fills each
// node's inlet temperature, and persists the facility ledger. When the
// ledger cannot be written, the snapshot keeps the energies last written,
// so a published counter is never ahead of the disk.
func (e *Emulator) tickFacility(now time.Time, nodes []NodeSnapshot, fac FacilityFunc) (FacilitySnapshot, error) {
	specITW, hostDeltaJ := 0.0, 0.0
	rackW := map[string]float64{}
	rackDeltaJ := map[string]float64{}
	for i, m := range e.members {
		specITW += nodes[i].Host.PowerW
		d := ledgerFinite(m.hostDelta)
		hostDeltaJ += d
		if r := m.spec.Rack; r != "" {
			rackW[r] += nodes[i].Host.PowerW
			rackDeltaJ[r] += d
		}
	}
	var v facility.Values
	if fac != nil {
		v = fac(specITW, now)
	} else {
		v, e.peakITPowerW = facility.Compute(e.facParams, now.Sub(e.start), specITW, e.peakITPowerW)
	}

	dt := now.Sub(e.lastTick).Seconds()
	legacyW := math.Max(0, v.ITPowerW-specITW)
	e.facLedger.ITJ += hostDeltaJ + ledgerFinite(legacyW*dt)
	e.facLedger.CoolingJ += ledgerFinite(v.CoolingPowerW * dt)
	for r, d := range rackDeltaJ {
		e.facLedger.RacksJ[r] += d
	}
	var err error
	path := filepath.Join(e.root, FacilityLedgerFile)
	if werr := ledgerWrite(e.root, facilityLedgerTmpPrefix, path, e.facLedger); werr != nil {
		err = fmt.Errorf("emulate: persist the facility ledger: %w", werr)
		if err.Error() != e.facErr {
			e.logf("%v; the facility energies stay at their last written values", err)
		}
		e.facErr = err.Error()
	} else {
		if e.facErr != "" {
			e.logf("emulate: the facility ledger is written again")
		}
		e.facErr = ""
		e.facPublished = e.facLedger.clone()
	}

	zoneC := make(map[string]float64, len(e.zones))
	zones := make([]ZoneReading, len(e.zones))
	for i, z := range e.zones {
		zoneC[z.Name] = v.AmbientC + z.OffsetC
		zones[i] = ZoneReading{Name: z.Name, AmbientC: zoneC[z.Name]}
	}
	for i, m := range e.members {
		nodes[i].Host.InletC = v.AmbientC
		if c, ok := zoneC[m.spec.CoolingZone]; ok {
			nodes[i].Host.InletC = c
		}
	}
	rackNames := make([]string, 0, len(rackW))
	for r := range rackW {
		rackNames = append(rackNames, r)
	}
	sort.Strings(rackNames)
	var racks []RackReading
	for _, r := range rackNames {
		racks = append(racks, RackReading{Name: r, PowerW: rackW[r], EnergyJ: e.facPublished.RacksJ[r]})
	}

	return FacilitySnapshot{
		StartTime:      e.facPublished.StartTime,
		AmbientC:       v.AmbientC,
		ITPowerW:       v.ITPowerW,
		ITEnergyJ:      e.facPublished.ITJ,
		PUE:            v.PUE,
		CoolingPowerW:  v.CoolingPowerW,
		CoolingEnergyJ: e.facPublished.CoolingJ,
		Zones:          zones,
		Racks:          racks,
	}, err
}

// emulatorLoad is the load a member runs: job, plus base when applyBase is
// set, clamped to 0..1 per element. Without applyBase it is job alone,
// clamped.
//
// hwemu reads CPUUtil as one value for all CPUs, one per package or one per
// logical CPU, and GPUUtil as one value for all GPUs or one per GPU, with
// missing entries idle. The base is one value per logical CPU and per GPU,
// so it is added to every element, and a job vector shorter than the
// packages or the GPUs is first padded with idle entries, so the base
// reaches every package and every GPU. A job's classes and memory intensity
// win over the base's when set. The job's peer transfers pass as they are,
// in order, since the rates the node gives back follow that order; the base
// makes none, and a node without GPUs gets none.
func emulatorLoad(job hwemu.Load, hasJob bool, base clusterspec.Load, applyBase bool, sockets, gpus int) hwemu.Load {
	if !hasJob {
		job = hwemu.Load{}
	}
	var cpuBase, gpuBase, memBase float64
	var cpuClass, gpuClass string
	if applyBase {
		cpuBase, gpuBase, memBase = emulatorOr(base.CPUUtil, 0), emulatorOr(base.GPUUtil, 0), emulatorOr(base.MemoryIntensity, 0)
		cpuClass, gpuClass = base.CPUClass, base.GPUClass
	}
	l := hwemu.Load{
		CPUUtil:         emulatorAddUtil(job.CPUUtil, cpuBase, sockets),
		CPUClass:        cpuClass,
		GPUUtil:         emulatorAddUtil(job.GPUUtil, gpuBase, gpus),
		GPUClass:        gpuClass,
		MemoryIntensity: emulatorClamp(memBase),
	}
	if job.CPUClass != "" {
		l.CPUClass = job.CPUClass
	}
	if job.GPUClass != "" {
		l.GPUClass = job.GPUClass
	}
	if job.MemoryIntensity != 0 {
		l.MemoryIntensity = emulatorClamp(job.MemoryIntensity)
	}
	if gpus == 0 {
		l.GPUUtil = nil
	} else if len(job.PeerTransfers) > 0 {
		l.PeerTransfers = append([]hwemu.PeerTransfer(nil), job.PeerTransfers...)
	}
	return l
}

// emulatorAddUtil adds base to every element of job, padding job to width
// elements first when base is not 0, and clamps each to 0..1. An empty job
// is one value for all, as hwemu reads it.
func emulatorAddUtil(job []float64, base float64, width int) []float64 {
	switch {
	case len(job) == 0 && base == 0:
		return nil
	case len(job) == 0:
		return []float64{emulatorClamp(base)}
	case len(job) == 1:
		return []float64{emulatorClamp(job[0] + base)}
	}
	n := len(job)
	if base != 0 && n < width {
		n = width
	}
	out := make([]float64, n)
	for i := range out {
		v := base
		if i < len(job) {
			v += job[i]
		}
		out[i] = emulatorClamp(v)
	}
	return out
}

// emulatorClamp keeps v in 0..1; NaN is 0.
func emulatorClamp(v float64) float64 {
	switch {
	case v > 1:
		return 1
	case v > 0:
		return v
	}
	return 0
}

// emulatorLoadEqual reports whether a and b are the same load, peer
// transfers included, so a member's runner gets every change of load.
func emulatorLoadEqual(a, b hwemu.Load) bool {
	return a.CPUClass == b.CPUClass && a.GPUClass == b.GPUClass && a.MemoryIntensity == b.MemoryIntensity &&
		emulatorFloatsEqual(a.CPUUtil, b.CPUUtil) && emulatorFloatsEqual(a.GPUUtil, b.GPUUtil) &&
		slices.Equal(a.PeerTransfers, b.PeerTransfers)
}

func emulatorFloatsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
