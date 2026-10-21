package emulate

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse/hwfusetest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// fuseClock is the clock of a FUSE mode test: the ticks and the reads of the
// views both take it, and the test moves it.
type fuseClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fuseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fuseClock) set(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = emuStart.Add(d)
	return c.t
}

// fuseMember is a member of a builtin profile cut to two cores per socket.
func fuseMember(t *testing.T, name, profile string) clusterspec.Member {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[profile]
	if p == nil {
		t.Fatalf("no builtin profile %s", profile)
	}
	const src = "assumed:test fixture, two cores per socket keep the tree small"
	p.CPU.CoresPerSocket = hwemu.Fact[int]{V: 2, Src: src}
	p.Node.Capacity.CPU = hwemu.Fact[int64]{V: int64(p.CPU.Sockets.V * 2 * p.CPU.ThreadsPerCore.V), Src: src}
	one := 1.0
	return clusterspec.Member{Name: name, Pool: "pool", Profile: p, BaseLoad: clusterspec.Load{CPUUtil: &one}}
}

func fuseReadInt(t *testing.T, p string) int64 {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("%s: %q: %v", p, b, err)
	}
	return v
}

// EM20: in FUSE mode every member's sys and state are served at ViewDir: a
// write the kernel refuses fails there with its errno and is a rejected
// event of the next snapshot, an accepted limit is the next snapshot's,
// and energy_uj moves between ticks; Close unmounts, and a new emulator
// over the same root serves counters no lower than any reader saw.
func TestEM20FUSEViewsServeEveryMember(t *testing.T) {
	hwfusetest.Require(t)
	if os.Geteuid() == 0 {
		t.Skip("as root the views are bind mounts that outlive Close, by design; this test runs as another user")
	}
	root := filepath.Join(t.TempDir(), "hwemu")
	c := &fuseClock{t: emuStart}
	cfg := Config{
		Root: root, Cluster: "fuse", Start: emuStart, FUSE: true, Clock: c.now, Logf: func(string, ...any) {},
		Members: []clusterspec.Member{fuseMember(t, "intel-0", "intel-xeon-2s-rapl"), fuseMember(t, "gpu-0", "nvidia-h100-nvl-8gpu")},
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sys := filepath.Join(ViewDir(root, "intel-0"), layout.SysDir)
	zone := func(name string) string {
		return filepath.Join(sys, filepath.FromSlash(path.Join(layout.PowercapZoneDir("intel-rapl", 0), name)))
	}
	if _, err := e.Tick(c.set(time.Second), nil, true, nil); err != nil {
		t.Fatal(err)
	}
	first := fuseReadInt(t, zone("energy_uj"))
	c.set(1500 * time.Millisecond)
	if mid := fuseReadInt(t, zone("energy_uj")); mid <= first {
		t.Errorf("energy_uj %d half a tick after %d, want it to move", mid, first)
	}
	dram := filepath.Join(sys, filepath.FromSlash(path.Join(layout.PowercapZoneDir("intel-rapl", 0, 0), "constraint_0_power_limit_uw")))
	if err := os.WriteFile(dram, []byte("10000000"), 0); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("a write to a DRAM zone that refuses writes: %v, want ENODATA", err)
	}
	if err := os.WriteFile(zone("constraint_0_power_limit_uw"), []byte("100000000"), 0); err != nil {
		t.Fatalf("PL1: %v", err)
	}
	nvml := filepath.Join(ViewDir(root, "gpu-0"), layout.StateDir, filepath.FromSlash(layout.NVMLGPUDir(0)), "power_limit_mw")
	if err := os.WriteFile(nvml, []byte("250000\n"), 0); err != nil {
		t.Fatalf("an NVML limit through the state view: %v", err)
	}
	snap, err := e.Tick(c.set(2*time.Second), nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := emuNode(t, snap, "intel-0")
	var rejected, accepted bool
	for _, ev := range n.Events {
		rejected = rejected || ev.Kind == hwemu.EventRejected && ev.Errno == hwemu.ErrnoENODATA
		accepted = accepted || ev.Kind == hwemu.EventAccepted && strings.HasSuffix(ev.File, "intel-rapl:0/constraint_0_power_limit_uw")
	}
	if !rejected || !accepted {
		t.Errorf("events of the next snapshot %+v, want the refused DRAM write with ENODATA and the PL1 write", n.Events)
	}
	for _, comp := range n.Components {
		if comp.ID == "cpu0" && comp.RequestedLimitW != 100 {
			t.Errorf("cpu0 requested limit %v W after the PL1 write, want 100", comp.RequestedLimitW)
		}
	}
	for _, comp := range emuNode(t, snap, "gpu-0").Components {
		if comp.ID == "gpu0" && comp.RequestedLimitW != 250 {
			t.Errorf("gpu0 requested limit %v W after the NVML write, want 250", comp.RequestedLimitW)
		}
	}
	c.set(2700 * time.Millisecond)
	seen := fuseReadInt(t, zone("energy_uj"))
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(zone("energy_uj")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the view after Close: %v, want it unmounted", err)
	}

	cfg.Start = c.set(3 * time.Second)
	again, err := New(cfg)
	if err != nil {
		t.Fatalf("New over the same root: %v", err)
	}
	defer again.Close()
	if got := fuseReadInt(t, zone("energy_uj")); got < seen {
		t.Errorf("the restarted emulator serves energy_uj %d, below the %d a reader saw before", got, seen)
	}
	if b, _ := os.ReadFile(zone("constraint_0_power_limit_uw")); string(b) != "100000000\n" {
		t.Errorf("PL1 reads %q after the restart, want the 100000000 written through the view", b)
	}
}

// FUSE mode needs runners that serve a live view, and no member may be
// named after the views' directory.
func TestEM21FUSEModeNeedsLiveNodes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	f := newEmuFakes()
	cfg := emuConfig(root, f, &emuLog{}, emuMember("a", "", "", 1, 0))
	cfg.FUSE = true
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "does not serve a live view") {
		t.Errorf("FUSE mode over fake runners: %v, want an error naming the node", err)
	}
	cfg = emuConfig(filepath.Join(t.TempDir(), "root"), f, &emuLog{}, emuMember(FUSEDir, "", "", 1, 0))
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "fuse directory") {
		t.Errorf("a member named %s: %v, want it refused", FUSEDir, err)
	}
}
