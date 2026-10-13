package hwemu_test

import (
	"math"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// CG13 (regression, defect W6-R-REPO F3/G13): CPUThroughput measured a job
// against the governor's own target, so a governor cost no throughput by
// construction: generic powersave on acpi-cpufreq, which holds every CPU at
// its 1.5 GHz minimum at full load, gave throughput 1. Measured against the
// governor-independent reference, performance at cpuinfo_max_freq with the
// profile's boost, the job runs at S(1500 MHz) / S(f_ref) < 1 for every
// class. Before the fix it was 1.
func TestCG13PowersaveCostsThroughputAgainstTheReference(t *testing.T) {
	t.Parallel()
	const src = "assumed:test fixture"
	n, _, p := hwemutest.NewNode(t, "amd-epyc-2s-energy-only", func(p *hwemu.Profile) {
		p.CPUFreq.Governor = hwemu.Fact[string]{V: "powersave", Src: src}
		p.CPU.CoresPerSocket = hwemu.Fact[int]{V: 2, Src: src}
		p.Node.Capacity.CPU = hwemu.Fact[int64]{V: int64(p.CPU.Sockets.V * 2 * p.CPU.ThreadsPerCore.V), Src: src}
	})
	throughputAnalytic(t, p)
	if d := p.CPUFreq.Driver.V; d != "acpi-cpufreq" || p.CPUFreq.Boost.V != 1 {
		t.Fatalf("%s: driver %s, boost %d; the case needs acpi-cpufreq with boost on", p.Name, d, p.CPUFreq.Boost.V)
	}
	minKHz, maxKHz := p.CPUFreq.CPUInfoMinKHz.V, p.CPUFreq.CPUInfoMaxKHz.V
	all := append(throughputSocketCPUs(p, 0), throughputSocketCPUs(p, 1)...)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	throughputStep(t, n, 10*time.Second)
	for _, class := range []string{"", "cpu.compute_bound", "cpu.memory_bound", "cpu.io_bound"} {
		want := throughputTM(p, float64(minKHz)/float64(maxKHz), class, 0, 0) / throughputTM(p, 1, class, 0, 0)
		if want >= 1 {
			t.Fatalf("class %q: S(%d)/S(%d) = %v; the case needs a minimum that costs throughput", class, minKHz, maxKHz, want)
		}
		if got := n.CPUThroughput(all, class, 0, 0); math.Abs(got-want) > throughputTol {
			t.Errorf("class %q, powersave at full load: throughput %v, want S(%d kHz)/S(%d kHz) = %v", class, got, minKHz, maxKHz, want)
		}
	}
}
