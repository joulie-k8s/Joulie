package hwemu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// persistNVL renders the builtin NVL node with persistence mode mode, its
// limit starting at a 400 W default, so the tests do not depend on a current
// limit the builtin profile may set above its default.
func persistNVL(t *testing.T, mode string) (*Profile, *Tree) {
	t.Helper()
	p := ensureBuiltin(t, "nvidia-h100-nvl-8gpu")
	p.GPUs.PersistenceMode = Fact[string]{V: mode, Src: "assumed:test fixture"}
	p.GPUs.Limits.DefaultW = Fact[float64]{V: 400, Src: "assumed:test fixture"}
	p.GPUs.Limits.CurrentW = Fact[float64]{}
	return p, renderTestTree(t, p)
}

// persistKinds lists the event kinds of the last step on rel, in order.
func persistKinds(n *Node, rel string) string {
	var kinds []string
	for _, e := range nodeEvents(n, rel) {
		kinds = append(kinds, e.Kind)
	}
	return strings.Join(kinds, ",")
}

// H35: with persistence mode Disabled, an NVIDIA GPU no client holds (load 0)
// goes back to its default limit: a 250000 mW write to the H100 NVL is
// accepted and then reset to 400000 within one step, with a
// reset-persistence-off event naming gpu3, the old limit and the default.
// The default is the one power_default_limit_mw shows. The limit holds
// while the GPU has load, and with persistence Enabled; a GPU switched to
// Disabled at run time, as nvidia-smi -pm 0 does, resets at the next step,
// and the switch is an event naming gpu3.
func TestH35PersistenceOffResetsAnIdleGPU(t *testing.T) {
	t.Parallel()
	limit := nodeNVMLFile(3, "power_limit_mw")
	mode := nodeNVMLFile(3, "persistence_mode")

	t.Run("disabled at load 0", func(t *testing.T) {
		t.Parallel()
		p, tree := persistNVL(t, PersistenceDisabled)
		if got := nodeRead(t, tree, mode); got != "Disabled\n" {
			t.Fatalf("persistence_mode = %q, want Disabled", got)
		}
		n := nodeNewNode(t, p, tree)
		nodeStep(t, n, time.Second)
		if ev := n.State().Events; len(ev) != 0 {
			t.Fatalf("an idle GPU at its default limit had events: %+v", ev)
		}
		nodeWrite(t, tree, limit, "250000")
		nodeStep(t, n, time.Second)
		if got := nodeRead(t, tree, limit); got != "400000\n" {
			t.Errorf("power_limit_mw = %q after one step, want 400000 back", got)
		}
		if got := persistKinds(n, limit); got != EventAccepted+","+EventResetPersistenceOff {
			t.Fatalf("events on gpu3's limit: %s, want accepted then reset-persistence-off", got)
		}
		reset := nodeEvents(n, limit)[1]
		if reset.Component != "gpu3" || reset.Written != "250000" || reset.Effective != "400000" {
			t.Errorf("reset event %+v, want gpu3 from 250000 to 400000", reset)
		}
		if g := stateComponent(t, n.State(), "gpu3"); g.RequestedLimitW != 400 {
			t.Errorf("gpu3 requested limit %v after the reset, want 400", g.RequestedLimitW)
		}
		nodeStep(t, n, time.Second)
		if ev := n.State().Events; len(ev) != 0 {
			t.Errorf("a reset limit is reset again: %+v", ev)
		}
	})

	t.Run("disabled under load", func(t *testing.T) {
		t.Parallel()
		p, tree := persistNVL(t, PersistenceDisabled)
		n := nodeNewNode(t, p, tree)
		n.SetLoad(Load{GPUUtil: []float64{0.5}})
		nodeWrite(t, tree, limit, "250000")
		for i := 0; i < 3; i++ {
			nodeStep(t, n, time.Second)
		}
		if got := nodeRead(t, tree, limit); got != "250000\n" {
			t.Errorf("power_limit_mw = %q under load, want 250000 held", got)
		}
		// The last client leaves: the next step resets the limit.
		n.SetLoad(Load{})
		nodeStep(t, n, time.Second)
		if got, kinds := nodeRead(t, tree, limit), persistKinds(n, limit); got != "400000\n" || kinds != EventResetPersistenceOff {
			t.Errorf("after the load ends: power_limit_mw %q, events %s; want 400000 and one reset", got, kinds)
		}
	})

	// The default is the one the tree shows, as nvidia-smi reports it, not
	// the profile's: a tree whose power_default_limit_mw reads 350000 resets
	// to 350000.
	t.Run("default from the tree", func(t *testing.T) {
		t.Parallel()
		p, tree := persistNVL(t, PersistenceDisabled)
		def := filepath.Join(tree.Root, filepath.FromSlash(nodeNVMLFile(3, "power_default_limit_mw")))
		if err := os.Chmod(def, 0o644); err != nil {
			t.Fatal(err)
		}
		nodeWrite(t, tree, nodeNVMLFile(3, "power_default_limit_mw"), "350000\n")
		n := nodeNewNode(t, p, tree)
		nodeWrite(t, tree, limit, "250000")
		nodeStep(t, n, time.Second)
		if got := nodeRead(t, tree, limit); got != "350000\n" {
			t.Errorf("power_limit_mw = %q after the reset, want the tree's default 350000", got)
		}
		if ev := nodeEvents(n, limit); len(ev) != 2 || ev[1].Kind != EventResetPersistenceOff || ev[1].Effective != "350000" {
			t.Errorf("events on gpu3's limit: %+v, want accepted then a reset to 350000", ev)
		}
	})

	t.Run("enabled, then disabled at run time", func(t *testing.T) {
		t.Parallel()
		p, tree := persistNVL(t, PersistenceEnabled)
		if got := nodeRead(t, tree, mode); got != "Enabled\n" {
			t.Fatalf("persistence_mode = %q, want Enabled", got)
		}
		n := nodeNewNode(t, p, tree)
		nodeWrite(t, tree, limit, "250000")
		for i := 0; i < 3; i++ {
			nodeStep(t, n, time.Second)
		}
		if got := nodeRead(t, tree, limit); got != "250000\n" {
			t.Errorf("power_limit_mw = %q with persistence Enabled, want 250000 held", got)
		}
		nodeWrite(t, tree, mode, "Disabled\n")
		nodeStep(t, n, time.Second)
		if got := persistKinds(n, mode); got != EventAccepted {
			t.Errorf("events on persistence_mode: %s, want accepted", got)
		} else if e := nodeOneEvent(t, n, mode); e.Component != "gpu3" {
			t.Errorf("persistence_mode event names %q, want gpu3", e.Component)
		}
		if got := nodeRead(t, tree, limit); got != "400000\n" {
			t.Errorf("power_limit_mw = %q after persistence was disabled, want 400000", got)
		}
		for i := 0; i < 8; i++ {
			if i != 3 && len(nodeEvents(n, nodeNVMLFile(i, "power_limit_mw"))) != 0 {
				t.Errorf("gpu%d, at its default limit and still Enabled, had events", i)
			}
		}
		nodeWrite(t, tree, mode, "sometimes")
		nodeStep(t, n, time.Second)
		if got, e := nodeRead(t, tree, mode), nodeOneEvent(t, n, mode); got != "Disabled\n" || e.Kind != EventRejected || e.Errno != ErrnoEINVAL {
			t.Errorf("persistence_mode %q, event %+v after an unknown word; want Disabled kept and the word refused with EINVAL", got, e)
		}
	})
}

// H35: gpus.persistenceMode takes enabled or disabled, on NVIDIA GPUs only,
// and an empty value means enabled.
func TestH35PersistenceModeValidation(t *testing.T) {
	t.Parallel()
	nvl := renderBuiltin(t, "nvidia-h100-nvl-8gpu")
	if v := nvl.GPUs.PersistenceMode.V; v != PersistenceEnabled {
		t.Errorf("NVL persistenceMode %q, want enabled", v)
	}
	if v := renderBuiltin(t, "nvidia-h100-sxm-8gpu").GPUs.PersistenceMode.V; v != PersistenceEnabled {
		t.Errorf("SXM persistenceMode %q, want enabled, inherited from NVL", v)
	}
	nvl.GPUs.PersistenceMode = Fact[string]{V: "on", Src: "assumed:test fixture"}
	if err := nvl.Validate(); err == nil || !strings.Contains(err.Error(), `persistenceMode "on"`) {
		t.Errorf("persistenceMode on: %v, want rejected", err)
	}
	amd := renderBuiltin(t, "amd-instinct-mi300x-8gpu")
	amd.GPUs.PersistenceMode = Fact[string]{V: PersistenceEnabled, Src: "assumed:test fixture"}
	if err := amd.Validate(); err == nil || !strings.Contains(err.Error(), "NVIDIA GPUs only") {
		t.Errorf("persistenceMode on AMD: %v, want rejected", err)
	}
	if persistModeText("") != "Enabled" || persistModeText(PersistenceEnabled) != "Enabled" || persistModeText(PersistenceDisabled) != "Disabled" {
		t.Errorf("persistence_mode words: %q %q %q", persistModeText(""), persistModeText(PersistenceEnabled), persistModeText(PersistenceDisabled))
	}
}

// L05: the NVL and SXM layout goldens list persistence_mode of every GPU as
// an agent-writable file, 0644, next to power_limit_mw, and no other golden
// has it.
func TestL05GoldensListPersistenceMode(t *testing.T) {
	t.Parallel()
	for _, name := range renderBuiltinNames {
		b, err := os.ReadFile(filepath.Join("testdata", "layout", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		golden := string(b)
		nvidia := strings.HasPrefix(name, "nvidia-")
		gpus := 8
		if g := renderBuiltin(t, name).GPUs; g != nil && g.Count.V > 0 {
			gpus = g.Count.V
		}
		for i := 0; i < gpus; i++ {
			line := fmt.Sprintf("state/nvml/gpu%d/persistence_mode A file 0644\n", i)
			if strings.Contains(golden, line) != nvidia {
				t.Errorf("%s: lists %q: %v, want %v", name, strings.TrimSpace(line), !nvidia, nvidia)
			}
		}
		if !nvidia && strings.Contains(golden, "persistence_mode") {
			t.Errorf("%s lists a persistence_mode", name)
		}
	}
}
