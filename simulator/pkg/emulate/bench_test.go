package emulate

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// BenchmarkEM01Tick times one 1 s tick of real hwemu nodes on fresh roots,
// with the default workers and render check interval, at a 0.6 CPU base
// load. It reports the median tick as p50-ms. The roots are under
// os.TempDir, so TMPDIR picks the file system. Nearly all of a tick is
// hwemu's system calls on the node's files: the readback, one lstat of
// every A file and a read of each one written since its last read, and the
// write of every changed S file (AdvanceTo). An S file goes in place
// under a write lease; a file a non-root caller may not write, as here
// outside the simulator's root container, also costs two chmods per write,
// so the harness's numbers come from a root run (see "Tick cost" in
// website/content/en/docs/simulator/emulate-mode.md). The -live cases start
// the nodes with hwemu.Options.Live, as FUSE mode does, without a mount or a
// reader: their tick reads back no file, and only writes what changed.
func BenchmarkEM01Tick(b *testing.B) {
	for _, bc := range []struct {
		name, profile string
		nodes         int
		live          bool
	}{
		{"intel-16", "intel-xeon-2s-rapl", 16, false},
		{"intel-200", "intel-xeon-2s-rapl", 200, false},
		{"epyc-16", "amd-epyc-2s-energy-only", 16, false},
		{"intel-16-live", "intel-xeon-2s-rapl", 16, true},
		{"intel-200-live", "intel-xeon-2s-rapl", 200, true},
		{"epyc-16-live", "amd-epyc-2s-energy-only", 16, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			profiles, err := hwemu.BuiltinProfiles()
			if err != nil {
				b.Fatal(err)
			}
			p := profiles[bc.profile]
			if p == nil {
				b.Fatalf("builtin profile %s is missing", bc.profile)
			}
			members := make([]clusterspec.Member, bc.nodes)
			for i := range members {
				members[i] = clusterspec.Member{
					Name:     fmt.Sprintf("%s-%d", bc.name, i),
					Pool:     bc.name,
					Rack:     fmt.Sprintf("rack-%d", i%4),
					Profile:  p,
					BaseLoad: clusterspec.Load{CPUUtil: emuFloat(0.6)},
				}
			}
			e, err := New(Config{
				Root:    filepath.Join(b.TempDir(), "hwemu"),
				Cluster: "bench",
				Members: members,
				Start:   emuStart,
				Logf:    func(string, ...any) {},
				NewRunner: func(p *hwemu.Profile, t *hwemu.Tree, o hwemu.Options) (Runner, error) {
					o.Live = bc.live
					return hwemu.NewNode(p, t, o)
				},
			})
			if err != nil {
				b.Fatal(err)
			}
			defer e.Close()

			took := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 1; i <= b.N; i++ {
				begin := time.Now()
				if _, err := e.Tick(emuStart.Add(time.Duration(i)*time.Second), nil, true, nil); err != nil {
					b.Fatal(err)
				}
				took = append(took, time.Since(begin))
			}
			b.StopTimer()
			sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
			b.ReportMetric(float64(took[len(took)/2].Microseconds())/1000, "p50-ms")
		})
	}
}
