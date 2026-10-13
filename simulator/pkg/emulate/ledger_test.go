package emulate

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// emuCounters lists every energy counter of a snapshot by a stable key.
func emuCounters(s *Snapshot) map[string]float64 {
	c := map[string]float64{
		"facility/it":      s.Facility.ITEnergyJ,
		"facility/cooling": s.Facility.CoolingEnergyJ,
	}
	for _, r := range s.Facility.Racks {
		c["rack/"+r.Name] = r.EnergyJ
	}
	for _, n := range s.Nodes {
		c[n.Name+"/host"] = n.Host.EnergyJ
		for _, comp := range n.Components {
			c[n.Name+"/"+comp.ID] = comp.EnergyJ
		}
	}
	return c
}

func emuFacility(specW float64, _ time.Time) facility.Values {
	return facility.Values{AmbientC: 20, ITPowerW: specW + 40, PUE: 1.25, CoolingPowerW: 0.25 * (specW + 40)}
}

// EM03: across Close and a second New on the same root, whose runners start
// their counters at 0 again, every exported energy (host, component, IT,
// cooling, rack) continues from its value before the restart, and every
// counter keeps its start time (DR-32). No energy is counted for the time
// the emulator was down, the known deviation.
func TestEM03CountersSurviveARestart(t *testing.T) {
	root := t.TempDir()
	members := []clusterspec.Member{emuMember("a", "rack-1", "", 1, 0), emuMember("b", "rack-1", "", 2, 2)}

	f1 := newEmuFakes()
	c := emuConfig(root, f1, &emuLog{}, members...)
	c.Registerer = prometheus.NewRegistry()
	e1, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	var before *Snapshot
	for i := 1; i <= 3; i++ {
		before = emuTick(t, e1, time.Duration(i)*time.Second, nil, true, emuFacility)
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	f2 := newEmuFakes()
	c.Ensure, c.NewRunner = f2.ensure, f2.newRunner
	c.Start = emuStart.Add(time.Minute)
	e2 := emuNew(t, c)
	if r := f2.runner(t, "a"); r.State().Host.EnergyJ != 0 {
		t.Fatalf("the restarted runner's counter starts at %v, want 0", r.State().Host.EnergyJ)
	}
	after, err := e2.Tick(c.Start.Add(time.Second), nil, true, emuFacility)
	if err != nil {
		t.Fatal(err)
	}

	was, now := emuCounters(before), emuCounters(after)
	if len(now) != len(was) {
		t.Fatalf("counters before %v, after %v", was, now)
	}
	for k, v := range was {
		if now[k] < v {
			t.Errorf("%s went back across the restart: %v, then %v", k, v, now[k])
		}
	}
	// One second of each node's power after the restart, nothing for the
	// minute it was down.
	emuNear(t, "a host energy after the restart", now["a/host"], was["a/host"]+160)
	emuNear(t, "b gpu1 energy after the restart", now["b/gpu1"], was["b/gpu1"]+200)
	emuNear(t, "IT energy after the restart", now["facility/it"], was["facility/it"]+160+560+40)
	emuNear(t, "rack-1 energy after the restart", now["rack/rack-1"], was["rack/rack-1"]+160+560)

	for _, n := range after.Nodes {
		if !n.StartTime.Equal(emuStart) {
			t.Errorf("node %s start time after the restart = %v, want %v", n.Name, n.StartTime, emuStart)
		}
		if !n.Started || n.Fresh {
			t.Errorf("node %s after the restart: Started %v, Fresh %v; want a started, resumed node", n.Name, n.Started, n.Fresh)
		}
	}
	if !after.Facility.StartTime.Equal(emuStart) {
		t.Errorf("facility start time after the restart = %v, want %v", after.Facility.StartTime, emuStart)
	}
}

// EM04: each tick replaces each ledger with a new file, renamed into place,
// so a reader never sees a partial write, and leaves no temp file behind.
func TestEM04LedgersAreReplacedEveryTick(t *testing.T) {
	root := t.TempDir()
	e := emuNew(t, emuConfig(root, newEmuFakes(), &emuLog{}, emuMember("a", "rack-1", "", 1, 0)))
	paths := []string{filepath.Join(root, "a", layout.StateDir, LedgerFile), filepath.Join(root, FacilityLedgerFile)}
	last := make([]os.FileInfo, len(paths))
	for i := 1; i <= 3; i++ {
		emuTick(t, e, time.Duration(i)*time.Second, nil, true, nil)
		for j, p := range paths {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if last[j] != nil && os.SameFile(last[j], fi) {
				t.Errorf("tick %d: %s kept its inode; it was written in place", i, p)
			}
			last[j] = fi
		}
	}
	for dir, prefix := range map[string]string{filepath.Join(root, "a", layout.StateDir, layout.TmpDir): ledgerTmpPrefix, root: facilityLedgerTmpPrefix} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), prefix) {
				t.Errorf("temp file %s left in %s", en.Name(), dir)
			}
		}
	}
}

// A runner counter that goes back, as one does when hwemu restarts a node,
// adds nothing to the ledger and is logged; the ledger grows again from the
// new value.
func TestLedgerCountsACounterGoingBackAsNoIncrease(t *testing.T) {
	f := newEmuFakes()
	log := &emuLog{}
	e := emuNew(t, emuConfig(t.TempDir(), f, log, emuMember("a", "", "", 1, 0)))
	s1 := emuTick(t, e, 2*time.Second, nil, true, nil)
	r := f.runner(t, "a")
	r.mu.Lock()
	r.backJ = 1000
	r.mu.Unlock()
	s2 := emuTick(t, e, 3*time.Second, nil, true, nil)
	if got, want := emuNode(t, s2, "a").Host.EnergyJ, emuNode(t, s1, "a").Host.EnergyJ; got != want {
		t.Errorf("host energy after the counter went back = %v, want it unchanged at %v", got, want)
	}
	if log.count("went back") == 0 {
		t.Error("a counter going back was not logged")
	}
	s3 := emuTick(t, e, 4*time.Second, nil, true, nil)
	emuNear(t, "host energy a tick later", emuNode(t, s3, "a").Host.EnergyJ, emuNode(t, s2, "a").Host.EnergyJ+160)
}

// A ledger that cannot be read stops New, naming the node and the file,
// rather than starting the counters again at 0; the root is released.
func TestLedgerUnreadableStopsNew(t *testing.T) {
	root := t.TempDir()
	c := emuConfig(root, newEmuFakes(), &emuLog{}, emuMember("a", "", "", 1, 0))
	e := emuNew(t, c)
	emuTick(t, e, time.Second, nil, true, nil)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a", layout.StateDir, LedgerFile)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Ensure, c.NewRunner = newEmuFakes().ensure, newEmuFakes().newRunner
	if _, err := New(c); err == nil || !strings.Contains(err.Error(), "node a") || !strings.Contains(err.Error(), LedgerFile) {
		t.Fatalf("New over an unreadable ledger: %v, want an error naming node a and the ledger", err)
	}
	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}
	e2, err := New(c)
	if err != nil {
		t.Fatalf("New after the ledger was repaired: %v (was the root left locked?)", err)
	}
	_ = e2.Close()
}

// A ledger that cannot be written leaves the node's published readings at
// those of its last persisted step, with Err set, so a published counter is
// never ahead of the disk; the energy is published once a write succeeds.
// A failure that repeats is logged once, though each try uses another temp
// file.
func TestLedgerWriteFailureKeepsThePublishedEnergy(t *testing.T) {
	root := t.TempDir()
	log := &emuLog{}
	e := emuNew(t, emuConfig(root, newEmuFakes(), log, emuMember("a", "", "", 1, 0)))
	s1 := emuTick(t, e, time.Second, nil, true, nil)

	tmp := filepath.Join(root, "a", layout.StateDir, layout.TmpDir)
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := e.Tick(emuStart.Add(2*time.Second), nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "persist the ledger") {
		t.Errorf("Tick with an unwritable ledger: %v", err)
	}
	a1, a2 := emuNode(t, s1, "a"), emuNode(t, s2, "a")
	if !strings.Contains(a2.Err, "persist the ledger") || a2.Host.EnergyJ != a1.Host.EnergyJ {
		t.Errorf("after a failed ledger write: Err %q, energy %v; want the error and the last persisted %v", a2.Err, a2.Host.EnergyJ, a1.Host.EnergyJ)
	}
	onDisk, err := loadNodeLedger(filepath.Join(root, "a", layout.StateDir, LedgerFile), "a", emuStart)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.HostJ != a2.Host.EnergyJ {
		t.Errorf("published host energy %v, on disk %v", a2.Host.EnergyJ, onDisk.HostJ)
	}

	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	s3 := emuTick(t, e, 3*time.Second, nil, true, nil)
	if a3 := emuNode(t, s3, "a"); a3.Err != "" {
		t.Errorf("Err after a good write = %q", a3.Err)
	} else {
		emuNear(t, "host energy once written again", a3.Host.EnergyJ, 160*3)
	}

	// A directory where the ledger goes makes every rename fail.
	ledger := filepath.Join(root, "a", layout.StateDir, LedgerFile)
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 4; i <= 5; i++ {
		if _, err := e.Tick(emuStart.Add(time.Duration(i)*time.Second), nil, true, nil); err == nil {
			t.Errorf("tick %d with the ledger a directory succeeded", i)
		}
	}
	if got := log.count("keeping the readings of its last good step"); got != 2 {
		t.Errorf("%d log lines for two failures, the second one twice; want 2: %q", got, log.lines)
	}
}

// A ledger New cannot continue stops it, naming the file, rather than
// starting its counters again: a node ledger of another node, as a copied
// tree holds, and a node or facility ledger of another schema version.
func TestLedgerOfAnotherNodeOrSchemaStopsNew(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		key        string
		value      any
		want       string
	}{
		{"node", filepath.Join("a", layout.StateDir, LedgerFile), "node", "b", `belongs to node "b", not "a"`},
		{"node schema", filepath.Join("a", layout.StateDir, LedgerFile), "schemaVersion", 2, "schemaVersion 2, want 1"},
		{"facility schema", FacilityLedgerFile, "schemaVersion", 2, "schemaVersion 2, want 1"},
	} {
		root := t.TempDir()
		c := emuConfig(root, newEmuFakes(), &emuLog{}, emuMember("a", "", "", 1, 0))
		e := emuNew(t, c)
		emuTick(t, e, time.Second, nil, true, nil)
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, tc.file)
		var m map[string]any
		data, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(data, &m)
		}
		if err != nil {
			t.Fatal(err)
		}
		m[tc.key] = tc.value
		if data, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		c.Ensure, c.NewRunner = newEmuFakes().ensure, newEmuFakes().newRunner
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: New = %v, want an error naming %s with %q", tc.name, err, path, tc.want)
		}
	}
}

// A facility ledger of another cluster is replaced, with a log line: the
// renamed cluster's facility counters start again, at the new start time.
// The node ledgers continue.
func TestFacilityLedgerOfAnotherClusterStartsANewOne(t *testing.T) {
	root := t.TempDir()
	log := &emuLog{}
	c := emuConfig(root, newEmuFakes(), log, emuMember("a", "rack-1", "", 1, 0))
	e := emuNew(t, c)
	emuTick(t, e, time.Second, nil, true, emuFacility)
	before := emuTick(t, e, 2*time.Second, nil, true, emuFacility)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	c.Cluster, c.Start = "renamed", emuStart.Add(time.Minute)
	c.Ensure, c.NewRunner = newEmuFakes().ensure, newEmuFakes().newRunner
	e2 := emuNew(t, c)
	after, err := e2.Tick(c.Start.Add(time.Second), nil, true, emuFacility)
	if err != nil {
		t.Fatal(err)
	}
	fs := after.Facility
	if !fs.StartTime.Equal(c.Start) {
		t.Errorf("facility start time = %v, want the new start %v", fs.StartTime, c.Start)
	}
	emuNear(t, "IT energy of the new facility", fs.ITEnergyJ, 160+40)
	emuNear(t, "rack-1 energy of the new facility", fs.Racks[0].EnergyJ, 160)
	if log.count(`belongs to cluster "test"`) != 1 {
		t.Errorf("the replaced facility ledger was not logged once: %q", log.lines)
	}
	emuNear(t, "a host energy", emuNode(t, after, "a").Host.EnergyJ, emuNode(t, before, "a").Host.EnergyJ+160)
}

// New removes the ledger temp files a crash between create and rename
// left, in each node's state/tmp and in the root, and nothing else.
func TestNewRemovesLeftoverLedgerTemps(t *testing.T) {
	root := t.TempDir()
	c := emuConfig(root, newEmuFakes(), &emuLog{}, emuMember("a", "", "", 1, 0))
	e := emuNew(t, c)
	emuTick(t, e, time.Second, nil, true, nil)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "a", layout.StateDir, layout.TmpDir)
	temps := []string{filepath.Join(tmp, ledgerTmpPrefix+"123"), filepath.Join(root, facilityLedgerTmpPrefix+"456")}
	kept := []string{filepath.Join(tmp, "other-123"), filepath.Join(root, "other-456")}
	for _, p := range append(append([]string(nil), temps...), kept...) {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.Ensure, c.NewRunner = newEmuFakes().ensure, newEmuFakes().newRunner
	emuNew(t, c)
	for _, p := range temps {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("leftover temp %s is still there", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// A facility ledger that cannot be written leaves the published IT,
// cooling and rack energies at their values on disk, with the error
// returned and logged once; once a write succeeds, the energy accumulated
// meanwhile is published.
func TestFacilityLedgerWriteFailureKeepsThePublishedEnergy(t *testing.T) {
	root := t.TempDir()
	log := &emuLog{}
	e := emuNew(t, emuConfig(root, newEmuFakes(), log, emuMember("a", "rack-1", "", 1, 0)))
	path := filepath.Join(root, FacilityLedgerFile)
	onDisk := func() facilityLedger {
		t.Helper()
		l, err := loadFacilityLedger(path, "test", emuStart, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	check := func(what string, s *Snapshot, l facilityLedger) {
		t.Helper()
		fs := s.Facility
		if fs.ITEnergyJ != l.ITJ || fs.CoolingEnergyJ != l.CoolingJ || len(fs.Racks) != 1 || fs.Racks[0].EnergyJ != l.RacksJ["rack-1"] {
			t.Errorf("%s: published IT %v, cooling %v, racks %+v; on disk %+v", what, fs.ITEnergyJ, fs.CoolingEnergyJ, fs.Racks, l)
		}
	}
	emuTick(t, e, time.Second, nil, true, emuFacility)
	written := onDisk()

	// A directory where the ledger goes makes the rename fail.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		s, err := e.Tick(emuStart.Add(time.Duration(i)*time.Second), nil, true, emuFacility)
		if err == nil || !strings.Contains(err.Error(), "persist the facility ledger") {
			t.Errorf("tick %d with an unwritable facility ledger: %v", i, err)
		}
		check(fmt.Sprintf("tick %d, not written", i), s, written)
	}
	if got := log.count("stay at their last written values"); got != 1 {
		t.Errorf("%d log lines for the failed writes, want 1: %q", got, log.lines)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s := emuTick(t, e, 4*time.Second, nil, true, emuFacility)
	check("written again", s, onDisk())
	emuNear(t, "IT energy once written again", s.Facility.ITEnergyJ, (160+40)*4)
	if log.count("written again") != 1 {
		t.Error("the recovered facility ledger write was not logged")
	}
}

// A runner counter that reads NaN adds nothing and fails nothing: the
// ledger keeps its value and its baseline, so the next finite reading
// counts the whole increase since the last finite one.
func TestLedgerSkipsANonFiniteCounter(t *testing.T) {
	f := newEmuFakes()
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 0)))
	s1 := emuTick(t, e, time.Second, nil, true, nil)
	r := f.runner(t, "a")
	r.mu.Lock()
	r.nonFinite = true
	r.mu.Unlock()
	s2 := emuTick(t, e, 2*time.Second, nil, true, nil)
	a1, a2 := emuNode(t, s1, "a"), emuNode(t, s2, "a")
	if a2.Err != "" || a2.Host.EnergyJ != a1.Host.EnergyJ || a2.Components[0].EnergyJ != a1.Components[0].EnergyJ {
		t.Errorf("after a NaN reading: Err %q, host %v, cpu0 %v; want no error and %v, %v", a2.Err, a2.Host.EnergyJ, a2.Components[0].EnergyJ, a1.Host.EnergyJ, a1.Components[0].EnergyJ)
	}
	r.mu.Lock()
	r.nonFinite = false
	r.mu.Unlock()
	a3 := emuNode(t, emuTick(t, e, 3*time.Second, nil, true, nil), "a")
	emuNear(t, "host energy after the NaN reading", a3.Host.EnergyJ, 160*3)
	emuNear(t, "cpu0 energy after the NaN reading", a3.Components[0].EnergyJ, 100*3)
	if math.IsNaN(a3.Host.EnergyJ) {
		t.Error("the ledger took the NaN")
	}
}
