package hwemu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// gvGovernorKeys are the profile keys of the governor schema, by the path of
// the mapping that holds them.
var gvGovernorKeys = map[string][]string{
	"cpufreq":     {"driverMode", "hwp", "epp", "boostScope", "tunables", "uncore"},
	"gpus":        {"dpm", "clocks", "transfer"},
	"physics.cpu": {"idleGovernorDeltaW", "eppGain", "eppCeilingDrop"},
	"physics.gpu": {"clockBeta", "memoryKneeFrac", "memoryKneeGamma", "dataPathHoldW", "gatingOffIdleW", "dataPathGamma"},
}

// gvWithoutGovernorFacts is a copy of p that sets no governor fact.
func gvWithoutGovernorFacts(t testing.TB, p *Profile) *Profile {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var q Profile
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	if f := q.CPUFreq; f != nil {
		f.DriverMode, f.HWP, f.EPP, f.BoostScope, f.Tunables, f.Uncore = Fact[string]{}, Fact[bool]{}, nil, Fact[string]{}, nil, nil
	}
	if g := q.GPUs; g != nil {
		g.DPM, g.Clocks, g.Transfer = nil, nil, nil
	}
	c := &q.Physics.CPU
	c.IdleGovernorDeltaW, c.EPPGain, c.EPPCeilingDrop = Fact[float64]{}, nil, nil
	g := &q.Physics.GPU
	g.ClockBeta, g.MemoryKneeFrac, g.MemoryKneeGamma = Fact[float64]{}, Fact[float64]{}, Fact[float64]{}
	g.DataPathHoldW, g.GatingOffIdleW, g.DataPathGamma = Fact[float64]{}, Fact[float64]{}, Fact[float64]{}
	return &q
}

// GV06: the governor schema changes no render. A builtin profile that sets
// no governor fact renders exactly the tree of its layout golden (H02 checks
// every profile as it ships), and every builtin profile stripped of its
// governor facts has the digest it would have without the governor keys in
// the schema at all, so a tree rendered before they existed still resumes
// (Ensure compares the digest).
func TestGV06GovernorSchemaLeavesRendersAndDigestsUnchanged(t *testing.T) {
	t.Parallel()
	for _, name := range renderBuiltinNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := renderBuiltin(t, name)
			q := gvWithoutGovernorFacts(t, p)

			b, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for at, keys := range gvGovernorKeys {
				var cur any = m
				for _, k := range strings.Split(at, ".") {
					if mm, ok := cur.(map[string]any); ok {
						cur = mm[k]
					}
				}
				if mm, ok := cur.(map[string]any); ok {
					for _, k := range keys {
						if _, has := mm[k]; !has {
							t.Errorf("%s.%s is not a profile key; gvGovernorKeys is stale", at, k)
						}
						delete(mm, k)
					}
				}
			}
			stripped, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ensureDigestJSON(stripped)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ensureProfileDigest(q)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("unset governor facts move the profile digest: %s, without the keys %s", got, want)
			}

			pd, err := ensureProfileDigest(p)
			if err != nil {
				t.Fatal(err)
			}
			if pd != got {
				t.Skip("the profile sets governor facts; H02 holds its tree to the golden")
			}
			listing := renderLayoutListing(t, renderTestTree(t, q))
			golden, err := os.ReadFile(filepath.Join("testdata", "layout", name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if listing != string(golden) {
				t.Fatalf("a profile without governor facts renders another tree than its golden:\n%s", renderLineDiff(string(golden), listing))
			}
		})
	}
}

// GV07: peer transfers on a node whose profile has no transfer or clock
// facts change nothing: State after the same steps is identical with and
// without them, events included.
func TestGV07PeerTransfersWithoutFactsLeaveStateUnchanged(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"nvidia-l40s-4gpu", "amd-radeon-pro-w7900-4gpu"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := gvWithoutGovernorFacts(t, ensureBuiltin(t, name))
			load := Load{CPUUtil: []float64{0.4}, GPUUtil: []float64{0.9, 0.1, 0, 0.5}, GPUClass: "gpu.memory_bound", MemoryIntensity: 0.6}
			states := make([]State, 2)
			for i, transfers := range [][]PeerTransfer{nil, {{Src: 0, Dst: 1, BytesPerS: 25e9}, {Src: 3, Dst: 2, BytesPerS: 4e9}}} {
				n := nodeNewNode(t, p, renderTestTree(t, p))
				l := load
				l.PeerTransfers = transfers
				n.SetLoad(l)
				for k := 0; k < 5; k++ {
					nodeStep(t, n, 700*time.Millisecond)
				}
				states[i] = n.State()
			}
			if !reflect.DeepEqual(states[0], states[1]) {
				t.Fatalf("peer transfers changed State on a profile without transfer facts:\nwithout %+v\nwith    %+v", states[0], states[1])
			}
			for _, c := range states[1].Components {
				if c.ClockHz != nil || c.GovernorNative != "" || c.GovernorPreset != "" || c.PeerRxBytes != 0 || c.PeerTxBytes != 0 {
					t.Errorf("%s reports governor state without a model: %+v", c.ID, c)
				}
			}
		})
	}
}

// A handler that refuses a value with rejectWrite gets an EventRejected
// carrying its errno, the file goes back to the value in effect, and an
// errno recorded while the Node seeded its model never reaches an event.
func TestRejectWriteCarriesTheErrnoToItsEvent(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "amd-epyc-2s-energy-only")
	tree := renderTestTree(t, p)
	n := nodeNewNode(t, p, tree)

	const rel = "sys/devices/test/level"
	abs := filepath.Join(tree.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.owners[rel] = layout.FileSpec{Rel: rel, Kind: layout.KindFile, Owner: layout.OwnerAgent, Mode: 0o644}
	level := "auto"
	handle := func(v string) (string, string) {
		if v == "high" {
			return n.rejectWrite(rel, ErrnoEOPNOTSUPP), level
		}
		level = v
		return EventAccepted, v
	}
	n.mu.Lock()
	n.rejectWrite("sys/devices/test/other", ErrnoEBUSY) // as if seeding had refused a value
	if n.addAFile(abs, "gpu0", handle, func() string { return level + "\n" }) == nil {
		n.mu.Unlock()
		t.Fatal("the test file is not agent-writable")
	}
	n.mu.Unlock()

	if err := os.WriteFile(abs, []byte("high\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nodeStep(t, n, time.Second)
	var got []Event
	for _, e := range n.State().Events {
		if e.Kind == EventRejected {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].File != rel || got[0].Errno != ErrnoEOPNOTSUPP || got[0].Written != "high" || got[0].Effective != "auto" || got[0].Component != "gpu0" {
		t.Fatalf("rejected events %+v, want one for %s with errno %s, written high, effective auto", got, rel, ErrnoEOPNOTSUPP)
	}
	if b, err := os.ReadFile(abs); err != nil || string(b) != "auto\n" {
		t.Fatalf("file reads %q (%v) after the step, want the value in effect, auto", b, err)
	}
	nodeStep(t, n, time.Second)
	for _, e := range n.State().Events {
		if e.Kind == EventRejected {
			t.Fatalf("a later step repeats the rejection: %+v", e)
		}
	}
}

// Profile validation checks gpus.transfer: a negative link rate and a
// receiver-gate fraction outside [0, 1] are rejected; a block within the
// bounds, gate off as in every repository profile, passes.
func TestGPUTransferValidation(t *testing.T) {
	t.Parallel()
	f := func(v float64) Fact[float64] { return Fact[float64]{V: v, Src: "assumed:test"} }
	valid := func() *GPUTransferSpec {
		return &GPUTransferSpec{PeerLinkGBps: f(20), ReceiverGate: ReceiverGateSpec{
			Enabled:      Fact[bool]{V: false, Src: "assumed:test"},
			SlowFraction: f(0.25), MidFraction: f(0.75), MidPairFraction: f(0.5),
		}}
	}
	p := renderBuiltin(t, "nvidia-l40s-4gpu")
	p.GPUs.Transfer = valid()
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid gpus.transfer block is rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*GPUTransferSpec)
		want   string
	}{
		{"negative link", func(s *GPUTransferSpec) { s.PeerLinkGBps = f(-1) }, "gpus.transfer.peerLinkGBps"},
		{"slow above 1", func(s *GPUTransferSpec) { s.ReceiverGate.SlowFraction = f(1.5) }, "gpus.transfer.receiverGate.slowFraction"},
		{"mid below 0", func(s *GPUTransferSpec) { s.ReceiverGate.MidFraction = f(-0.1) }, "gpus.transfer.receiverGate.midFraction"},
		{"pair above 1", func(s *GPUTransferSpec) { s.ReceiverGate.MidPairFraction = f(2) }, "gpus.transfer.receiverGate.midPairFraction"},
	} {
		p := renderBuiltin(t, "nvidia-l40s-4gpu")
		p.GPUs.Transfer = valid()
		tc.mutate(p.GPUs.Transfer)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", tc.name, err, tc.want)
		}
	}
}

// NewNode takes the three Options.ReceiverGate values and empty (the
// profile's setting), and refuses any other word instead of running a gate
// nobody asked for.
func TestNewNodeChecksReceiverGate(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-l40s-4gpu")
	for _, g := range []string{"", ReceiverGateProfile, ReceiverGateOff, ReceiverGateOn} {
		if _, err := NewNode(p, renderTestTree(t, p), Options{Start: nodeStart, ReceiverGate: g}); err != nil {
			t.Errorf("ReceiverGate %q: %v", g, err)
		}
	}
	if _, err := NewNode(p, renderTestTree(t, p), Options{Start: nodeStart, ReceiverGate: "auto"}); err == nil || !strings.Contains(err.Error(), "ReceiverGate") {
		t.Fatalf("ReceiverGate auto: err = %v, want a rejection naming Options.ReceiverGate", err)
	}
}
