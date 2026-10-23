package simcluster

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// Check tolerances: 3 % of the truth, or 1 W when that is more.
const (
	CheckTolerance = 0.03
	CheckAbsFloorW = 1.0
)

// The truths of the Kepler 0.7 platform checks of mode kepler-legacy (E07c).
// Kepler reports only a Node with a measured CPU package (simulator/pkg/compat).
const (
	// TruthKepler07PlatformSeries is how many kepler_node_platform_joules_total
	// series Kepler 0.7 exports for a Node: 2, modes idle and dynamic.
	TruthKepler07PlatformSeries = "kepler-0.7-platform-series"
	// TruthKepler07Host is whole-node power, which Kepler 0.7 reports as
	// platform energy, plus the idle power its platform series report
	// beyond it.
	TruthKepler07Host = "kepler-0.7-host"
	// TruthKepler07Components is what the kepler-legacy preset reads: the
	// CPU packages, plus the GPUs when they are NVIDIA's, plus the idle
	// power those Kepler 0.7 series report beyond the measured one. A cap
	// that holds a series below the idle power it recorded at a lower load
	// leaves Kepler reporting that idle (simulator/pkg/compat, k07Split).
	TruthKepler07Components = "kepler-0.7-components"
)

// Truths returns every truth a Check may name.
func Truths() []string {
	return []string{TruthPackagesGPUs, TruthPackagesNVIDIAGPUs, TruthPackagesNVIDIAAMDGPUs, TruthHost, TruthKepler07PlatformSeries, TruthKepler07Host, TruthKepler07Components}
}

// EvalTruth returns the value of truth for one Node:
//
//   - TruthPackagesGPUs: the CPU packages plus every GPU;
//   - TruthPackagesNVIDIAGPUs: the CPU packages, plus the GPUs when they
//     are NVIDIA's;
//   - TruthPackagesNVIDIAAMDGPUs: the CPU packages, plus the GPUs when they
//     are NVIDIA's or AMD's;
//   - TruthHost: whole-node power;
//   - TruthKepler07PlatformSeries: 2;
//   - TruthKepler07Host: whole-node power, plus Kepler07PlatformExcessW;
//   - TruthKepler07Components: TruthPackagesNVIDIAGPUs plus
//     Kepler07ExcessW.
//
// The Kepler truths (packages+nvidia-gpus and every kepler-0.7 one) are 0
// for a Node without a measured CPU package, for which no Kepler release
// reports anything.
func EvalTruth(truth string, n NodeTruth) (float64, error) {
	kepler := len(n.PackagesW) > 0
	switch truth {
	case TruthPackagesGPUs:
		return checkSum(n.PackagesW) + checkSum(n.GPUsW), nil
	case TruthPackagesNVIDIAGPUs, TruthKepler07Components:
		if !kepler {
			return 0, nil
		}
		v := checkSum(n.PackagesW)
		if strings.EqualFold(n.GPUVendor, agentVendorNVIDIA) {
			v += checkSum(n.GPUsW)
		}
		if truth == TruthKepler07Components {
			v += n.Kepler07ExcessW
		}
		return v, nil
	case TruthPackagesNVIDIAAMDGPUs:
		v := checkSum(n.PackagesW)
		if bridgeScrapesGPUs(n.GPUVendor) {
			v += checkSum(n.GPUsW)
		}
		return v, nil
	case TruthHost:
		return n.HostW, nil
	case TruthKepler07PlatformSeries:
		if kepler {
			return 2, nil
		}
		return 0, nil
	case TruthKepler07Host:
		if kepler {
			return n.HostW + n.Kepler07PlatformExcessW, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("unknown truth %q; truths are %s", truth, strings.Join(Truths(), ", "))
}

// bridgeScrapesGPUs reports whether the otel-bridge Collector scrapes the
// GPUs of vendor: by vendor name or PCI vendor id, as the simulator's
// dcgm-exporter and AMD device-metrics-exporter formats select them.
func bridgeScrapesGPUs(vendor string) bool {
	v := strings.ToLower(vendor)
	return strings.Contains(v, "nvidia") || v == "0x10de" || strings.Contains(v, "amd") || v == "0x1002"
}

func checkSum(vs []float64) float64 {
	var s float64
	for _, v := range vs {
		s += v
	}
	return s
}

// Within reports whether observed is within the check's tolerance of want:
// |observed - want| is at most Tolerance x |want|, or AbsFloorW, whichever
// is larger. The floor keeps a zero truth checkable.
func (c Check) Within(observed, want float64) bool {
	if math.IsNaN(observed) || math.IsInf(observed, 0) {
		return false
	}
	return math.Abs(observed-want) <= math.Max(c.Tolerance*math.Abs(want), c.AbsFloorW)
}

// CheckResult is one Check on one Node. Found is false when the query
// returned no series for the Node, HasTruth false when the truth does not
// know the Node.
type CheckResult struct {
	ID       string  `json:"id"`
	Node     string  `json:"node"`
	Observed float64 `json:"observed"`
	Found    bool    `json:"found"`
	Want     float64 `json:"want"`
	HasTruth bool    `json:"hasTruth"`
	Pass     bool    `json:"pass"`
}

func (r CheckResult) String() string {
	switch {
	case !r.HasTruth:
		return fmt.Sprintf("%s %s: observed %.3f, but the truth has no such Node", r.ID, r.Node, r.Observed)
	case !r.Found:
		return fmt.Sprintf("%s %s: no series, want %.3f", r.ID, r.Node, r.Want)
	}
	return fmt.Sprintf("%s %s: observed %.3f, want %.3f", r.ID, r.Node, r.Observed, r.Want)
}

// Evaluate compares what c's query returned, observed by Node, with the
// truth by Node, and returns one result per Node of either map, sorted by
// Node. A Node without a series passes when its truth is zero, as for a
// Node no collector can read; a series of a Node the truth does not know
// fails.
//
// The error says what is wrong with the run: for a Check without a
// KnownDefect, every failing Node; for one with a KnownDefect, that it
// passes on every Node, since the defect is then gone and the KnownDefect
// must go too. A Check with nothing to compare is an error as well.
func Evaluate(c Check, observed map[string]float64, truth map[string]NodeTruth) ([]CheckResult, error) {
	names := map[string]bool{}
	for n := range observed {
		names[n] = true
	}
	for n := range truth {
		names[n] = true
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("check %s: no Node to compare", c.ID)
	}
	nodes := agentSortedKeys(names)
	results := make([]CheckResult, 0, len(nodes))
	var failed []string
	for _, node := range nodes {
		r := CheckResult{ID: c.ID, Node: node}
		r.Observed, r.Found = observed[node]
		var nt NodeTruth
		nt, r.HasTruth = truth[node]
		if r.HasTruth {
			want, err := EvalTruth(c.Truth, nt)
			if err != nil {
				return nil, fmt.Errorf("check %s: %w", c.ID, err)
			}
			r.Want = want
		}
		switch {
		case !r.HasTruth:
			r.Pass = false
		case !r.Found:
			r.Pass = r.Want == 0
		default:
			r.Pass = c.Within(r.Observed, r.Want)
		}
		if !r.Pass {
			failed = append(failed, r.String())
		}
		results = append(results, r)
	}
	switch {
	case c.KnownDefect != "" && len(failed) == 0:
		return results, fmt.Errorf("check %s passes on every Node, so known defect %s no longer reproduces: remove it from tests/simcluster/known_defects_test.go and drop the check's KnownDefect", c.ID, c.KnownDefect)
	case c.KnownDefect == "" && len(failed) > 0:
		return results, fmt.Errorf("check %s (%s, tolerance %g, floor %g W) fails on %d of %d Nodes:\n  %s", c.ID, c.Truth, c.Tolerance, c.AbsFloorW, len(failed), len(results), strings.Join(failed, "\n  "))
	}
	return results, nil
}

// NodeTruths returns the truth of every Node of a snapshot, from its
// instantaneous power readings: see nodeTruth.
func NodeTruths(s *emulate.Snapshot) map[string]NodeTruth {
	out := make(map[string]NodeTruth, len(s.Nodes))
	for i := range s.Nodes {
		n := &s.Nodes[i]
		out[n.Name] = nodeTruth(n, nil, 0)
	}
	return out
}

// NodeTruthsOver returns the truth of every Node of both snapshots over
// the time between them, as rate() reads it: each power is the energy the
// ledger added divided by the time, (E(to) - E(from)) / (to - from). The
// limits and the platform's other power are those of to. A Node missing
// from either snapshot is left out.
func NodeTruthsOver(from, to *emulate.Snapshot) (map[string]NodeTruth, error) {
	dt := to.Time.Sub(from.Time)
	if dt <= 0 {
		return nil, fmt.Errorf("truth: snapshot %d at %s is not after snapshot %d at %s", to.Seq, to.Time.Format(time.RFC3339Nano), from.Seq, from.Time.Format(time.RFC3339Nano))
	}
	before := make(map[string]*emulate.NodeSnapshot, len(from.Nodes))
	for i := range from.Nodes {
		before[from.Nodes[i].Name] = &from.Nodes[i]
	}
	out := make(map[string]NodeTruth, len(to.Nodes))
	for i := range to.Nodes {
		n := &to.Nodes[i]
		if b := before[n.Name]; b != nil {
			out[n.Name] = nodeTruth(n, b, dt.Seconds())
		}
	}
	return out, nil
}

// WithKepler07Excess returns truth with the idle excess of mode
// kepler-legacy set per Node: components and platform are the results of
// the queries of CheckSet.Kepler07Excess by Node, and a Node missing from
// one keeps 0 there.
func WithKepler07Excess(truth map[string]NodeTruth, components, platform map[string]float64) map[string]NodeTruth {
	out := make(map[string]NodeTruth, len(truth))
	for name, n := range truth {
		n.Kepler07ExcessW, n.Kepler07PlatformExcessW = components[name], platform[name]
		out[name] = n
	}
	return out
}

// nodeTruth is the truth of n: the CPU packages, DRAM zones and GPUs a
// collector can read (Measured), in index order, and the host when it is
// measured. With from and dt, each power is the energy added since from
// over dt seconds; without, the reading itself.
func nodeTruth(n, from *emulate.NodeSnapshot, dt float64) NodeTruth {
	power := func(c *emulate.ComponentReading) float64 {
		if from == nil {
			return c.PowerW
		}
		for i := range from.Components {
			if p := &from.Components[i]; p.ID == c.ID {
				return (c.EnergyJ - p.EnergyJ) / dt
			}
		}
		return 0
	}
	comps := make([]*emulate.ComponentReading, 0, len(n.Components))
	for i := range n.Components {
		if n.Components[i].Measured {
			comps = append(comps, &n.Components[i])
		}
	}
	sort.SliceStable(comps, func(i, j int) bool { return comps[i].Index < comps[j].Index })
	var t NodeTruth
	for _, c := range comps {
		switch c.Type {
		case semconv.HwTypeCPU:
			t.PackagesW = append(t.PackagesW, power(c))
			t.PackageCapW = append(t.PackageCapW, c.LimitW)
		case semconv.HwTypeGPU:
			t.GPUsW = append(t.GPUsW, power(c))
			t.GPULimitW = append(t.GPULimitW, c.LimitW)
			t.GPUVendor = c.Vendor
		case semconv.HwTypeMemory:
			t.DRAMW += power(c)
		}
	}
	t.PlatformOtherW = n.Host.OtherW
	if n.Host.Measured {
		t.HostW = n.Host.PowerW
		if from != nil {
			t.HostW = (n.Host.EnergyJ - from.Host.EnergyJ) / dt
		}
	}
	return t
}
