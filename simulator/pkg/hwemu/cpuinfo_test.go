package hwemu

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/governor/discover"
)

// renderParseCPUInfo splits a rendered /proc/cpuinfo into blocks of key and
// value, cutting each line at its first colon and trimming both sides, the
// way the agent's readProcCPUInfo reads it.
func renderParseCPUInfo(t testing.TB, text string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, block := range strings.Split(strings.TrimRight(text, "\n"), "\n\n") {
		m := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				t.Fatalf("cpuinfo line %q has no colon", line)
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		out = append(out, m)
	}
	return out
}

// renderCPUInfoInt reads one integer field of a cpuinfo block.
func renderCPUInfoInt(t testing.TB, block map[string]string, key string) int {
	t.Helper()
	n, err := strconv.Atoi(block[key])
	if err != nil {
		t.Fatalf("processor %s: %s = %q: %v", block["processor"], key, block[key], err)
	}
	return n
}

// H02: the generated /proc/cpuinfo of every builtin profile is what the agent
// counts sockets from (distinct physical id, readProcCPUInfo in cmd/agent).
// It has one block per logical CPU numbered 0..n-1, one physical id per
// socket, siblings and cpu cores from the topology, each core holding
// threadsPerCore CPUs, and hypervisor in flags exactly on a virtual machine.
// On the Xeon, CPU k sits on socket (k mod 48)/24 as in the corpus numbering;
// the VM has one package per vCPU, as the corpus vm-no-rapl does.
func TestH02GeneratedCPUInfo(t *testing.T) {
	t.Parallel()
	for _, name := range renderBuiltinNames {
		t.Run(name, func(t *testing.T) {
			p, tree := renderSharedBuiltin(t, name)
			if p.CPU.CPUInfoVerbatim != "" {
				t.Fatal("a builtin profile carries a verbatim cpuinfo; this test covers the generated one")
			}
			b, err := os.ReadFile(tree.CPUInfoPath)
			if err != nil {
				t.Fatal(err)
			}
			blocks := renderParseCPUInfo(t, string(b))
			c := p.CPU
			n := c.Sockets.V * c.CoresPerSocket.V * c.ThreadsPerCore.V
			if len(blocks) != n || int64(n) != p.Node.Capacity.CPU.V {
				t.Fatalf("%d blocks, want sockets x cores x threads = %d = capacity.cpu %d", len(blocks), n, p.Node.Capacity.CPU.V)
			}
			sockets := map[int]bool{}
			threads := map[[2]int]int{}
			for k, blk := range blocks {
				if got := renderCPUInfoInt(t, blk, "processor"); got != k {
					t.Fatalf("block %d is processor %d", k, got)
				}
				if blk["vendor_id"] != c.VendorID.V || blk["model name"] != c.ModelName.V {
					t.Errorf("processor %d: vendor_id %q, model name %q", k, blk["vendor_id"], blk["model name"])
				}
				socket := renderCPUInfoInt(t, blk, "physical id")
				core := renderCPUInfoInt(t, blk, "core id")
				sockets[socket] = true
				threads[[2]int{socket, core}]++
				if got := renderCPUInfoInt(t, blk, "siblings"); got != c.CoresPerSocket.V*c.ThreadsPerCore.V {
					t.Errorf("processor %d: siblings %d, want %d", k, got, c.CoresPerSocket.V*c.ThreadsPerCore.V)
				}
				if got := renderCPUInfoInt(t, blk, "cpu cores"); got != c.CoresPerSocket.V {
					t.Errorf("processor %d: cpu cores %d, want %d", k, got, c.CoresPerSocket.V)
				}
				if got := renderContains(strings.Fields(blk["flags"]), "hypervisor"); got != c.Hypervisor {
					t.Errorf("processor %d: hypervisor in flags = %v, want %v", k, got, c.Hypervisor)
				}
			}
			if len(sockets) != c.Sockets.V {
				t.Errorf("%d distinct physical ids, want %d sockets", len(sockets), c.Sockets.V)
			}
			for s := 0; s < c.Sockets.V; s++ {
				if !sockets[s] {
					t.Errorf("no CPU with physical id %d", s)
				}
			}
			for key, got := range threads {
				if got != c.ThreadsPerCore.V {
					t.Errorf("physical id %d core id %d holds %d CPUs, want %d", key[0], key[1], got, c.ThreadsPerCore.V)
				}
			}
		})
	}

	t.Run("intel-xeon-2s-rapl numbering", func(t *testing.T) {
		_, tree := renderSharedBuiltin(t, "intel-xeon-2s-rapl")
		blocks := renderParseCPUInfo(t, renderReadFile(t, tree, "cpuinfo"))
		if len(blocks) != 96 {
			t.Fatalf("%d blocks, want 96", len(blocks))
		}
		for k, blk := range blocks {
			if got, want := renderCPUInfoInt(t, blk, "physical id"), (k%48)/24; got != want {
				t.Errorf("processor %d: physical id %d, want %d", k, got, want)
			}
			if got, want := renderCPUInfoInt(t, blk, "core id"), k%24; got != want {
				t.Errorf("processor %d: core id %d, want %d", k, got, want)
			}
			if blk["siblings"] != "48" || blk["cpu cores"] != "24" {
				t.Errorf("processor %d: siblings %s, cpu cores %s, want 48 and 24", k, blk["siblings"], blk["cpu cores"])
			}
		}
	})

	t.Run("vm-no-powercap one package per vCPU", func(t *testing.T) {
		_, tree := renderSharedBuiltin(t, "vm-no-powercap")
		blocks := renderParseCPUInfo(t, renderReadFile(t, tree, "cpuinfo"))
		ids := map[string]bool{}
		for _, blk := range blocks {
			ids[blk["physical id"]] = true
			if blk["siblings"] != "1" || blk["cpu cores"] != "1" {
				t.Errorf("processor %s: siblings %s, cpu cores %s, want 1 and 1", blk["processor"], blk["siblings"], blk["cpu cores"])
			}
			if !renderContains(strings.Fields(blk["flags"]), "hypervisor") {
				t.Errorf("processor %s: flags %q lack hypervisor", blk["processor"], blk["flags"])
			}
		}
		if len(blocks) != 8 || len(ids) != 8 {
			t.Errorf("%d blocks with %d distinct physical ids, want 8 and 8", len(blocks), len(ids))
		}
	})
}

// H01: a cpu fact that cites a corpus machine holds a value that machine's
// cpuinfo has, so corpus provenance never covers an assumed number. sockets
// is the count of distinct physical ids and threadsPerCore is siblings over
// cpu cores, the way the agent and the kernel derive them.
func TestH01CorpusCPUFactsMatchCapture(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..", "..")
	checked := map[string]int{}
	for _, name := range renderBuiltinNames {
		p := renderBuiltin(t, name)
		c := p.CPU
		facts := []struct {
			field, src, value string
			corpus            func(blocks []map[string]string) map[string]bool
		}{
			{"vendorID", c.VendorID.Src, c.VendorID.V, renderCPUInfoValues("vendor_id")},
			{"modelName", c.ModelName.Src, c.ModelName.V, renderCPUInfoValues("model name")},
			{"family", c.Family.Src, strconv.Itoa(c.Family.V), renderCPUInfoValues("cpu family")},
			{"model", c.Model.Src, strconv.Itoa(c.Model.V), renderCPUInfoValues("model")},
			{"stepping", c.Stepping.Src, strconv.Itoa(c.Stepping.V), renderCPUInfoValues("stepping")},
			{"cpuMHz", c.CPUMHz.Src, fmt.Sprintf("%.3f", c.CPUMHz.V), renderCPUInfoValues("cpu MHz")},
			{"coresPerSocket", c.CoresPerSocket.Src, strconv.Itoa(c.CoresPerSocket.V), renderCPUInfoValues("cpu cores")},
			{"sockets", c.Sockets.Src, strconv.Itoa(c.Sockets.V), func(blocks []map[string]string) map[string]bool {
				ids := renderCPUInfoValues("physical id")(blocks)
				return map[string]bool{strconv.Itoa(len(ids)): true}
			}},
			{"threadsPerCore", c.ThreadsPerCore.Src, strconv.Itoa(c.ThreadsPerCore.V), func(blocks []map[string]string) map[string]bool {
				out := map[string]bool{}
				for _, b := range blocks {
					sib, err1 := strconv.Atoi(b["siblings"])
					cores, err2 := strconv.Atoi(b["cpu cores"])
					if err1 == nil && err2 == nil && cores > 0 {
						out[strconv.Itoa(sib/cores)] = true
					}
				}
				return out
			}},
		}
		for _, f := range facts {
			rest, ok := strings.CutPrefix(f.src, "corpus:")
			if !ok {
				continue
			}
			id, _, _ := strings.Cut(rest, "#")
			ref := p.Sources[id].Ref
			b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(ref), "cpuinfo"))
			if err != nil {
				t.Errorf("%s: cpu.%s cites corpus %q, which has no cpuinfo: %v", name, f.field, ref, err)
				continue
			}
			if have := f.corpus(renderParseCPUInfo(t, string(b))); !have[f.value] {
				t.Errorf("%s: cpu.%s = %s cites %q, but the capture has %v; cite derived: or assumed: instead",
					name, f.field, f.value, f.src, renderSortedStrings(renderKeys(have)))
			}
			checked[name]++
		}
	}
	// The two profiles built on a corpus capture must have been checked, so
	// the test cannot pass by finding nothing to compare.
	for _, name := range []string{"intel-xeon-2s-rapl", "vm-no-powercap"} {
		if checked[name] < 6 {
			t.Errorf("%s: only %d cpu facts checked against the corpus", name, checked[name])
		}
	}
}

// renderCPUInfoValues returns the set of values one cpuinfo key takes across
// the blocks.
func renderCPUInfoValues(key string) func([]map[string]string) map[string]bool {
	return func(blocks []map[string]string) map[string]bool {
		out := map[string]bool{}
		for _, b := range blocks {
			if v, ok := b[key]; ok {
				out[v] = true
			}
		}
		return out
	}
}

// renderKeys lists a set's members.
func renderKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// H52: the generated cpuinfo of every builtin profile carries the flags its
// facts imply: hwp on every block exactly where cpufreq.hwp is true, with
// hwp_epp under intel_pstate, and governor discovery reads HWP from that
// flag alone. The preference files intel_pstate renders with HWP are left
// out of discovery's view, because their presence also says HWP and hid
// that the generated cpuinfo had no flags line at all.
func TestH52GeneratedCPUInfoFlagsFollowTheHWPFact(t *testing.T) {
	t.Parallel()
	hwpProfiles := 0
	for _, name := range renderBuiltinNames {
		t.Run(name, func(t *testing.T) {
			p, tree := renderSharedBuiltin(t, name)
			wantHWP := p.CPUFreq != nil && p.CPUFreq.HWP.V
			wantEPP := wantHWP && cpuGovIntel(p.CPUFreq.Driver.V)
			if wantHWP {
				hwpProfiles++
			}
			for k, blk := range renderParseCPUInfo(t, renderReadFile(t, tree, "cpuinfo")) {
				flags := strings.Fields(blk["flags"])
				if got := renderContains(flags, "hwp"); got != wantHWP {
					t.Fatalf("processor %d: hwp in flags = %v, want %v (cpufreq.hwp); flags %q", k, got, wantHWP, blk["flags"])
				}
				if got := renderContains(flags, "hwp_epp"); got != wantEPP {
					t.Fatalf("processor %d: hwp_epp in flags = %v, want %v", k, got, wantEPP)
				}
			}
			if p.CPUFreq == nil {
				return
			}
			sys := copyWithoutEPP(t, tree.SysRoot)
			c := discover.ReadCPU(sys, tree.CPUInfoPath)
			if c == nil {
				t.Fatal("discovery found no cpufreq policy")
			}
			if c.Facts.HWP != wantHWP {
				t.Errorf("discovery reads HWP %v from the cpuinfo, want %v", c.Facts.HWP, wantHWP)
			}
		})
	}
	t.Cleanup(func() {
		if hwpProfiles == 0 {
			t.Error("no builtin profile has cpufreq.hwp true; the test checks nothing")
		}
	})
}

// copyWithoutEPP copies the regular files of devices/system/cpu under sys
// into a new sys root, without the energy-performance preference pair.
func copyWithoutEPP(t *testing.T, sys string) string {
	t.Helper()
	out := t.TempDir()
	src := filepath.Join(sys, "devices", "system", "cpu")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), "energy_performance_") {
			return err
		}
		rel, _ := filepath.Rel(sys, path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(out, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(out, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
