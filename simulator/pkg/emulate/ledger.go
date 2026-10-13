package emulate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ledgerSchemaVersion is the schemaVersion of both ledger files.
const ledgerSchemaVersion = 1

// Temp file prefixes of the ledger writes. A temp file left by a crash
// between create and rename is removed by the next New.
const (
	ledgerTmpPrefix         = "ledger-"
	facilityLedgerTmpPrefix = ".facility-ledger-"
)

// nodeLedger is <root>/<node>/state/LedgerFile: the counters a node
// exports, the energies in J and the GPU bytes. They only grow: each tick
// adds the increase of the node's own counters since the last tick, so a new
// Node, whose counters restart at 0, continues them instead of resetting
// them (DR-32). StartTime is when the ledger was created, the
// startTimeUnixNano of every counter it holds. ComponentsJ and GPUIO are
// keyed by hw.id. A ledger written before GPUIO existed reads as one whose
// GPUs moved nothing yet, so the field needs no new schemaVersion.
//
// ioBase is the runner's byte counters at the last tick, per hw.id. It is
// not persisted: a new Node starts them at 0, as an empty ioBase says.
type nodeLedger struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Node          string                 `json:"node"`
	StartTime     time.Time              `json:"startTime"`
	HostJ         float64                `json:"hostJ"`
	ComponentsJ   map[string]float64     `json:"componentsJ"`
	GPUIO         map[string]gpuIOLedger `json:"gpuIO,omitempty"`

	ioBase map[string]gpuIOLedger
}

// gpuIOLedger is what one GPU moved over its peer link: the bytes it
// received from and sent to the other GPUs of its node.
type gpuIOLedger struct {
	PeerRxBytes uint64 `json:"peerRxBytes"`
	PeerTxBytes uint64 `json:"peerTxBytes"`
}

// accrueGPUIO adds to the ledger of GPU id the increase of the runner's byte
// counters, which read rx and tx now, as ledgerAccrue does for energy.
// wentBack reports a counter that went back.
func (l *nodeLedger) accrueGPUIO(id string, rx, tx uint64) (wentBack bool) {
	if l.GPUIO == nil {
		l.GPUIO = map[string]gpuIOLedger{}
	}
	if l.ioBase == nil {
		l.ioBase = map[string]gpuIOLedger{}
	}
	total, base := l.GPUIO[id], l.ioBase[id]
	rxBack := ledgerAccrueBytes(&total.PeerRxBytes, &base.PeerRxBytes, rx)
	txBack := ledgerAccrueBytes(&total.PeerTxBytes, &base.PeerTxBytes, tx)
	l.GPUIO[id], l.ioBase[id] = total, base
	return rxBack || txBack
}

// facilityLedger is <root>/FacilityLedgerFile: the facility counters, in J,
// with the same rule. ITJ grows by the hosts' ledger increases plus the
// legacy nodes' power over the tick, CoolingJ by the cooling power over the
// tick, and RacksJ by the ledger increases of each rack's hosts.
type facilityLedger struct {
	SchemaVersion int                `json:"schemaVersion"`
	Cluster       string             `json:"cluster"`
	StartTime     time.Time          `json:"startTime"`
	ITJ           float64            `json:"itJ"`
	CoolingJ      float64            `json:"coolingJ"`
	RacksJ        map[string]float64 `json:"racksJ"`
}

func (l facilityLedger) clone() facilityLedger {
	c := l
	c.RacksJ = make(map[string]float64, len(l.RacksJ))
	for k, v := range l.RacksJ {
		c.RacksJ[k] = v
	}
	return c
}

// loadNodeLedger reads the ledger at path, or starts one at start when there
// is none. A ledger that cannot be read is an error, never a reset: a reset
// would make every exported counter go back.
func loadNodeLedger(path, node string, start time.Time) (nodeLedger, error) {
	var l nodeLedger
	found, err := ledgerRead(path, &l)
	if err != nil {
		return nodeLedger{}, err
	}
	if !found {
		return nodeLedger{SchemaVersion: ledgerSchemaVersion, Node: node, StartTime: start, ComponentsJ: map[string]float64{}}, nil
	}
	if l.SchemaVersion != ledgerSchemaVersion {
		return nodeLedger{}, fmt.Errorf("ledger %s: schemaVersion %d, want %d", path, l.SchemaVersion, ledgerSchemaVersion)
	}
	if l.Node != node {
		return nodeLedger{}, fmt.Errorf("ledger %s belongs to node %q, not %q", path, l.Node, node)
	}
	if l.ComponentsJ == nil {
		l.ComponentsJ = map[string]float64{}
	}
	return l, nil
}

// loadFacilityLedger reads the facility ledger at path, or starts one at
// start. A ledger of another cluster is replaced by a new one: the facility
// counters of a renamed cluster are other series, and starting them again
// makes no counter go back.
func loadFacilityLedger(path, cluster string, start time.Time, logf func(string, ...any)) (facilityLedger, error) {
	fresh := facilityLedger{SchemaVersion: ledgerSchemaVersion, Cluster: cluster, StartTime: start, RacksJ: map[string]float64{}}
	var l facilityLedger
	found, err := ledgerRead(path, &l)
	if err != nil {
		return facilityLedger{}, err
	}
	if !found {
		return fresh, nil
	}
	if l.SchemaVersion != ledgerSchemaVersion {
		return facilityLedger{}, fmt.Errorf("ledger %s: schemaVersion %d, want %d", path, l.SchemaVersion, ledgerSchemaVersion)
	}
	if l.Cluster != cluster {
		logf("emulate: facility ledger %s belongs to cluster %q; starting a new one for %q", path, l.Cluster, cluster)
		return fresh, nil
	}
	if l.RacksJ == nil {
		l.RacksJ = map[string]float64{}
	}
	return l, nil
}

func ledgerRead(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("ledger %s: %w", path, err)
	}
	return true, nil
}

// ledgerWrite replaces path with v as JSON through a temp file in tmpDir,
// which must be on the same file system. A reader sees the old ledger or the
// new one, never a partial write. There is no fsync: the ledger has to
// survive a restart of the simulator, not of the machine. An error does not
// name the temp file, whose name is random, so a failure that repeats every
// tick reads the same and is logged once.
func ledgerWrite(tmpDir, prefix, path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, prefix)
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write(append(data, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(name)
		return ledgerTempErr("write a temp file for", path, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return ledgerTempErr("chmod a temp file for", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return ledgerTempErr("rename a temp file to", path, err)
	}
	return nil
}

// ledgerTempErr is err of an operation on a temp file for path, with the
// temp file's name left out.
func ledgerTempErr(op, path string, err error) error {
	var le *os.LinkError
	var pe *fs.PathError
	switch {
	case errors.As(err, &le):
		err = le.Err
	case errors.As(err, &pe):
		err = pe.Err
	}
	return fmt.Errorf("%s %s: %w", op, path, err)
}

// ledgerRemoveTemps removes the temp files a crash left in dir.
func ledgerRemoveTemps(dir, prefix string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), prefix) {
			errs = append(errs, os.Remove(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// ledgerAccrue adds to *total the increase of a runner counter that reads v
// now and read *base at the last tick, then moves *base to v. A counter that
// went back, as one does when its runner restarts, adds 0, and so does a
// value that is not finite; wentBack reports the first case so it can be
// logged.
func ledgerAccrue(total, base *float64, v float64) (wentBack bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	d := v - *base
	*base = v
	if d < 0 {
		return true
	}
	*total += d
	return false
}

// ledgerAccrueBytes is ledgerAccrue for a byte counter: it adds to *total
// the increase of a runner counter that reads v now and read *base at the
// last tick, then moves *base to v. A counter that went back adds 0 and
// reports wentBack.
func ledgerAccrueBytes(total, base *uint64, v uint64) (wentBack bool) {
	d := v - *base
	wentBack = v < *base
	*base = v
	if !wentBack {
		*total += d
	}
	return wentBack
}

// ledgerFinite is v when it is finite and not negative, else 0: what a
// facility counter may add in one tick.
func ledgerFinite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}
