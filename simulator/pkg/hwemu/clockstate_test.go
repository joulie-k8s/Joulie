package hwemu

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// Tests of the GPU clock maxima State reports, ClockMaxHz and ClockLimitHz,
// which joulie.hw.gpu.speed.limit exports as max and throttled. Before they
// existed State had no per-domain maximum and the series was never emitted.

// clockMHz is a ClockHz-style map of MHz values per domain, in Hz.
func clockMHz(mhz map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(mhz))
	for d, v := range mhz {
		out[d] = v * 1e6
	}
	return out
}

// checkClockMaxima compares a GPU's maxima in State with want, and checks
// that every CPU package and DRAM zone has none.
func checkClockMaxima(t *testing.T, s State, id string, wantMax, wantLimit map[string]int64) {
	t.Helper()
	c := stateComponent(t, s, id)
	if !reflect.DeepEqual(c.ClockMaxHz, wantMax) {
		t.Errorf("%s ClockMaxHz %v, want %v", id, c.ClockMaxHz, wantMax)
	}
	if !reflect.DeepEqual(c.ClockLimitHz, wantLimit) {
		t.Errorf("%s ClockLimitHz %v, want %v", id, c.ClockLimitHz, wantLimit)
	}
	for _, o := range s.Components {
		if o.Type != semconv.HwTypeGPU && (o.ClockMaxHz != nil || o.ClockLimitHz != nil) {
			t.Errorf("%s has GPU clock maxima %v, %v", o.ID, o.ClockMaxHz, o.ClockLimitHz)
		}
	}
}

// On the W7900 the hardware maximum is the top of each pp_dpm_* table on
// every GPU, and the maximum in effect what the level allows a busy GPU:
// the top under auto and high, the bottom of each table under low. An idle
// GPU at auto runs at the bottom while its maximum in effect stays the top.
func TestGPUClockMaximaFollowTheAMDLevel(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, w7900Profile)
	tb := p.GPUs.DPM.Tables
	top := clockMHz(map[string]int64{
		semconv.ClockDomainGraphics: tb.Sclk.MaxMHz.V, semconv.ClockDomainMemory: slices.Max(tb.Mclk.V),
		semconv.ClockDomainFabric: slices.Max(tb.Fclk.V), semconv.ClockDomainSoC: slices.Max(tb.Socclk.V),
	})
	bottom := clockMHz(map[string]int64{
		semconv.ClockDomainGraphics: tb.Sclk.MinMHz.V, semconv.ClockDomainMemory: slices.Min(tb.Mclk.V),
		semconv.ClockDomainFabric: slices.Min(tb.Fclk.V), semconv.ClockDomainSoC: slices.Min(tb.Socclk.V),
	})
	dpmSetLevel(t, tr, n, 1, "low")
	dpmSetLevel(t, tr, n, 2, "high")
	s := n.State()
	for i, limit := range []map[string]int64{top, bottom, top, top} {
		checkClockMaxima(t, s, stateID(semconv.HwTypeGPU, i), top, limit)
	}
	if hz := stateComponent(t, s, "gpu0").ClockHz; !reflect.DeepEqual(hz, bottom) {
		t.Errorf("idle gpu0 at auto ClockHz %v, want the bottom of each table %v", hz, bottom)
	}
}

// On the MI300X an overdrive ceiling under manual is the graphics maximum
// in effect; the other domains keep their top, and so does every domain of
// a GPU left at auto.
func TestGPUClockMaximaFollowTheOverdriveCeiling(t *testing.T) {
	t.Parallel()
	p, tr, n := dpmNode(t, mi300xProfile)
	tb := p.GPUs.DPM.Tables
	top := map[string]int64{semconv.ClockDomainGraphics: tb.Sclk.MaxMHz.V, semconv.ClockDomainMemory: slices.Max(tb.Mclk.V)}
	if len(tb.Fclk.V) > 0 {
		top[semconv.ClockDomainFabric] = slices.Max(tb.Fclk.V)
	}
	if len(tb.Socclk.V) > 0 {
		top[semconv.ClockDomainSoC] = slices.Max(tb.Socclk.V)
	}
	const ceilMHz = 1300
	if ceilMHz >= tb.Sclk.MaxMHz.V || ceilMHz <= tb.Sclk.MinMHz.V {
		t.Fatalf("the test ceiling %d MHz is outside the profile's sclk range %d to %d", ceilMHz, tb.Sclk.MinMHz.V, tb.Sclk.MaxMHz.V)
	}
	dpmSetLevel(t, tr, n, 1, "manual")
	nodeWrite(t, tr, dpmRel(1, layout.AMDGPUODFile), "s 1 1300\nc")
	nodeStep(t, n, time.Second)
	limit := map[string]int64{}
	for d, v := range top {
		limit[d] = v
	}
	limit[semconv.ClockDomainGraphics] = ceilMHz
	s := n.State()
	checkClockMaxima(t, s, "gpu0", clockMHz(top), clockMHz(top))
	checkClockMaxima(t, s, "gpu1", clockMHz(top), clockMHz(limit))
}

// On NVIDIA the hardware maximum is NVML's maximum clock and the maximum in
// effect the clock a busy GPU runs at: the sustained maximum, lowered by a
// graphics or memory lock, never raised by a lock above it.
func TestGPUClockMaximaFollowTheNVIDIALocks(t *testing.T) {
	t.Parallel()
	p := nvclockProfile(t, "nvidia-l40s-4gpu", true)
	tree := renderTestTree(t, p)
	n := nodeNewNode(t, p, tree)
	nvclockMustSMI(t, tree, "-i", "1", "-lgc", "300,900")
	nvclockMustSMI(t, tree, "-i", "2", "-lmc", "300,600")
	nvclockMustSMI(t, tree, "-i", "3", "-lgc", "600,1800")
	nodeStep(t, n, time.Second)
	top := clockMHz(map[string]int64{semconv.ClockDomainGraphics: 1500, semconv.ClockDomainMemory: 1200})
	for i, limit := range []map[string]int64{
		{semconv.ClockDomainGraphics: 1400, semconv.ClockDomainMemory: 1200},
		{semconv.ClockDomainGraphics: 900, semconv.ClockDomainMemory: 1200},
		{semconv.ClockDomainGraphics: 1400, semconv.ClockDomainMemory: 600},
		{semconv.ClockDomainGraphics: 1400, semconv.ClockDomainMemory: 1200},
	} {
		checkClockMaxima(t, n.State(), stateID(semconv.HwTypeGPU, i), top, clockMHz(limit))
	}
}
