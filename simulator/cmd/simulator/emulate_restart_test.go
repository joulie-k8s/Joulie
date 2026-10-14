//go:build unix

package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/compat"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// restartSmokeSpec is the smoke spec of the harness, relative to this
// package.
const restartSmokeSpec = "../../../tests/simcluster/specs/smoke.yaml"

// restartCounters is everything SE12 expects never to go back across a
// restart: each energy_uj file, by path under the root; each ledger value,
// by file and key; each DCGM energy series, by its labels.
type restartCounters struct {
	energyUJ map[string]int64
	rangeUJ  map[string]int64
	ledgers  map[string]float64
	dcgm     map[string]float64
}

// SE12: a second simulator over the root of a first, with the smoke spec,
// real hwemu trees and its own registry, resumes every node. Taking the
// root puts nothing back in place of what is there, so no entry under any
// node's sys/ changes inode; after its first tick every energy counter,
// unwrapped at max_energy_range_uj, every ledger value and every DCGM
// energy is at or above where the first left it; and the limits an agent
// wrote keep their bytes.
func TestSE12RestartKeepsCountersLimitsAndInodes(t *testing.T) {
	if _, err := os.Stat(restartSmokeSpec); err != nil {
		t.Fatalf("smoke spec: %v", err)
	}
	root := filepath.Join(t.TempDir(), "hwemu")
	cfg := emulateTestConfig{specPath: restartSmokeSpec, root: root, exporters: []string{compat.DCGM484}}
	a := newEmulateTest(t, cfg)
	at := func(s int) time.Time { return emulateTestStart.Add(time.Duration(s) * time.Second) }
	a.tick(t, at(1))

	// Limits as an agent writes them: in place, then read back by a tick.
	intel, nvl := a.node(t, "intel"), a.node(t, "nvl")
	limits := map[string]string{
		filepath.Join(root, intel, layout.SysDir, layout.PowercapZoneDir("intel-rapl", 0), "constraint_0_power_limit_uw"): "120000000",
		filepath.Join(root, nvl, layout.StateDir, layout.NVMLGPUDir(0), "power_limit_mw"):                                 "300000",
	}
	for path, v := range limits {
		if err := os.WriteFile(path, []byte(v+"\n"), 0); err != nil {
			t.Fatalf("write a limit: %v", err)
		}
	}
	a.tick(t, at(2))
	a.tick(t, at(3))
	written := map[string][]byte{}
	for path, v := range limits {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(b)) != v {
			t.Fatalf("%s holds %q after two ticks, want the written %s accepted", path, b, v)
		}
		written[path] = b
	}
	before := restartRead(t, a)
	if len(before.energyUJ) == 0 || len(before.ledgers) == 0 {
		t.Fatalf("%d energy_uj files and %d ledger values before the restart, want some of each", len(before.energyUJ), len(before.ledgers))
	}
	a.close()
	inodes := restartInodes(t, root, a.rt.names)

	b := newEmulateTest(t, emulateTestConfig{specPath: restartSmokeSpec, root: root, exporters: []string{compat.DCGM484}, start: at(10)})
	if got := restartInodes(t, root, b.rt.names); len(got) != len(inodes) {
		t.Errorf("sys/ entries: %d before the restart, %d after", len(inodes), len(got))
	} else {
		for path, ino := range inodes {
			if got[path] != ino {
				t.Errorf("%s: inode %d before the restart, %d after", path, ino, got[path])
			}
		}
	}
	snap := b.tick(t, at(11))
	for _, n := range snap.Nodes {
		if !n.Started || n.Fresh {
			t.Errorf("node %s: started %v fresh %v after the restart, want a resumed tree", n.Name, n.Started, n.Fresh)
		}
	}
	after := restartRead(t, b)

	for path, v := range before.energyUJ {
		got, ok := after.energyUJ[path]
		if !ok {
			t.Errorf("%s: gone after the restart", path)
			continue
		}
		if got < v {
			got += before.rangeUJ[path]
		}
		if got < v {
			t.Errorf("%s: %d before the restart, %d after (unwrapped)", path, v, got)
		}
	}
	for key, v := range before.ledgers {
		if got, ok := after.ledgers[key]; !ok || got < v {
			t.Errorf("ledger %s: %v before the restart, %v after (present %v)", key, v, got, ok)
		}
	}
	if len(before.dcgm) == 0 {
		t.Error("no DCGM energy series before the restart")
	}
	for key, v := range before.dcgm {
		if got, ok := after.dcgm[key]; !ok || got < v {
			t.Errorf("DCGM %s: %v before the restart, %v after (present %v)", key, v, got, ok)
		}
	}
	for path, want := range written {
		if got, err := os.ReadFile(path); err != nil || string(got) != string(want) {
			t.Errorf("%s: %q after the restart, want the agent's %q (%v)", path, got, want, err)
		}
	}
}

// restartRead reads the counters of et's root and its DCGM exporter.
func restartRead(t *testing.T, et *emulateTest) restartCounters {
	t.Helper()
	c := restartCounters{energyUJ: map[string]int64{}, rangeUJ: map[string]int64{}, ledgers: map[string]float64{}, dcgm: map[string]float64{}}
	for _, node := range et.rt.names {
		sys := filepath.Join(et.root, node, layout.SysDir)
		err := filepath.WalkDir(sys, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.Name() != "energy_uj" || !d.Type().IsRegular() {
				return err
			}
			c.energyUJ[path] = restartInt(t, path)
			c.rangeUJ[path] = restartInt(t, filepath.Join(filepath.Dir(path), "max_energy_range_uj"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var l struct {
			HostJ       float64            `json:"hostJ"`
			ComponentsJ map[string]float64 `json:"componentsJ"`
		}
		restartJSON(t, filepath.Join(et.root, node, layout.StateDir, emulate.LedgerFile), &l)
		c.ledgers[node+"/host"] = l.HostJ
		for id, v := range l.ComponentsJ {
			c.ledgers[node+"/"+id] = v
		}
	}
	var f struct {
		ITJ, CoolingJ float64
		RacksJ        map[string]float64
	}
	restartJSON(t, filepath.Join(et.root, emulate.FacilityLedgerFile), &f)
	c.ledgers["facility/it"], c.ledgers["facility/cooling"] = f.ITJ, f.CoolingJ
	for r, v := range f.RacksJ {
		c.ledgers["facility/rack/"+r] = v
	}

	w := emulateTestDo(t, et.s.newMux(), http.MethodGet, compat.Path(compat.DCGM484), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d", compat.Path(compat.DCGM484), w.Code)
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		c.dcgm[line[:i]] = v
	}
	return c
}

// restartInodes is the inode of every entry under each node's sys/, links
// included and not followed.
func restartInodes(t *testing.T, root string, nodes []string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, node := range nodes {
		err := filepath.WalkDir(filepath.Join(root, node, layout.SysDir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Skip("no inode numbers on this platform")
			}
			out[path] = st.Ino
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func restartInt(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func restartJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
