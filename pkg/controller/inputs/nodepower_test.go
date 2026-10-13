package inputs

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/promname"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// checkGolden compares got with the golden file at path, or rewrites the
// file under -update.
func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./pkg/controller/inputs/ -update", err)
	}
	if string(want) == got {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			t.Fatalf("%s differs from the generated queries at line %d:\n got: %s\nwant: %s\nReview the change, then run go test ./pkg/controller/inputs/ -update", path, i+1, g, w)
		}
	}
}

// writeQuery appends one query of a golden case.
func writeQuery(b *strings.Builder, role string, q Query) {
	if q.PromQL == "" {
		return
	}
	fmt.Fprintf(b, "%s group=%q per_node=%t\n", role, q.GroupLabel, q.PerNode)
	for _, line := range strings.Split(q.PromQL, "\n") {
		fmt.Fprintf(b, "    %s\n", line)
	}
}

// The node power queries of section 1.6 of the OTEL-IN plan, verbatim: the
// default naming and window, labels form, target_info form, host scope and
// both Kepler presets. promqltest evaluates each of them in tests/promql.
// One change: the target_info of each join is restricted with "and on (job,
// instance)" to the targets of its left side, or another producer's
// target_info whose job and instance span two nodes fails the join.
var nodePowerContract = []struct {
	name      string
	spec      NodePowerSpec
	primary   string
	fallback  string
	wantGroup string
}{
	{
		name:      "otel components",
		spec:      NodePowerSpec{},
		primary:   `sum by (k8s_node_name) (max by (k8s_node_name, hw_type, hw_id) (rate(hw_energy_joules_total{hw_type=~"cpu|gpu"}[5m])) or on (k8s_node_name, hw_type, hw_id) max by (k8s_node_name, hw_type, hw_id) (hw_power_watts{hw_type=~"cpu|gpu"}))`,
		fallback:  `sum by (k8s_node_name) (max by (k8s_node_name, hw_type, hw_id) (rate(hw_energy_joules_total{hw_type=~"cpu|gpu"}[5m]) * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) rate(hw_energy_joules_total{hw_type=~"cpu|gpu"}[5m]))) or on (k8s_node_name, hw_type, hw_id) max by (k8s_node_name, hw_type, hw_id) (hw_power_watts{hw_type=~"cpu|gpu"} * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) hw_power_watts{hw_type=~"cpu|gpu"})))`,
		wantGroup: "k8s_node_name",
	},
	{
		name:      "otel host",
		spec:      NodePowerSpec{Scope: ScopeHost, ResourceAttributes: ResourceAttributesLabels},
		primary:   `max by (k8s_node_name) (rate(hw_host_energy_joules_total[5m])) or on (k8s_node_name) max by (k8s_node_name) (hw_host_power_watts)`,
		wantGroup: "k8s_node_name",
	},
	{
		name:      "kepler",
		spec:      NodePowerSpec{Preset: NodePowerPresetKepler},
		primary:   `sum by (node_name) (label_replace(rate(kepler_node_cpu_joules_total{zone="package"}[5m]), "joulie_component", "cpu", "", "") or label_replace(rate(kepler_node_gpu_joules_total[5m]), "joulie_component", "gpu", "", ""))`,
		wantGroup: "node_name",
	},
	{
		name:      "kepler-legacy components",
		spec:      NodePowerSpec{Preset: NodePowerPresetKeplerLegacy},
		primary:   `sum by (exported_instance) (label_replace(rate(kepler_node_package_joules_total[5m]), "joulie_component", "cpu", "", "") or label_replace(rate(kepler_node_gpu_joules_total[5m]), "joulie_component", "gpu", "", ""))`,
		wantGroup: "exported_instance",
	},
	{
		name:      "kepler-legacy host",
		spec:      NodePowerSpec{Preset: NodePowerPresetKeplerLegacy, Scope: ScopeHost},
		primary:   `sum by (exported_instance) (rate(kepler_node_platform_joules_total[5m]))`,
		wantGroup: "exported_instance",
	},
}

// nodePowerCases is every combination the golden file pins: the otel preset
// under each scope, strategy and resource attributes mode, the compatibility
// presets, a node label override, a window in seconds and the custom forms.
func nodePowerCases() []NodePowerSpec {
	var specs []NodePowerSpec
	for _, scope := range []Scope{ScopeComponents, ScopeHost} {
		for _, naming := range promname.Strategies() {
			for _, ra := range []ResourceAttributes{ResourceAttributesAuto, ResourceAttributesLabels, ResourceAttributesTargetInfo} {
				specs = append(specs, NodePowerSpec{Preset: NodePowerPresetOTel, Scope: scope, Naming: naming, ResourceAttributes: ra})
			}
		}
	}
	specs = append(specs,
		NodePowerSpec{Preset: NodePowerPresetOTel, NodeLabel: "node", Window: 90 * time.Second},
		NodePowerSpec{Preset: NodePowerPresetOTel, Naming: promname.NoUTF8EscapingWithSuffixes, NodeLabel: "node"},
		NodePowerSpec{Preset: NodePowerPresetKepler},
		NodePowerSpec{Preset: NodePowerPresetKepler, NodeLabel: "instance", Naming: promname.NoTranslation, ResourceAttributes: ResourceAttributesTargetInfo},
		NodePowerSpec{Preset: NodePowerPresetKeplerLegacy},
		NodePowerSpec{Preset: NodePowerPresetKeplerLegacy, Scope: ScopeHost},
		NodePowerSpec{CustomQuery: `sum(rate(node_energy_joules_total{node="{node}"}[2m]))`},
		NodePowerSpec{CustomQuery: `rate(node_energy_joules_total[2m])`},
		NodePowerSpec{Preset: NodePowerPresetCustom, CustomQuery: `rate(node_energy_joules_total[2m])`, NodeLabel: "nodename"},
		NodePowerSpec{Preset: NodePowerPresetCustom, CustomQuery: "rate(node_energy_joules_total[2m]) # per node", Naming: promname.NoUTF8EscapingWithSuffixes},
	)
	return specs
}

func describeNodePowerSpec(s NodePowerSpec) string {
	parts := []string{"preset=" + string(s.Preset)}
	if s.Scope != "" {
		parts = append(parts, "scope="+string(s.Scope))
	}
	if s.Naming != "" {
		parts = append(parts, "naming="+string(s.Naming))
	}
	if s.ResourceAttributes != "" {
		parts = append(parts, "resource_attributes="+string(s.ResourceAttributes))
	}
	if s.Window != 0 {
		parts = append(parts, "window="+s.Window.String())
	}
	if s.NodeLabel != "" {
		parts = append(parts, "node_label="+s.NodeLabel)
	}
	if s.CustomQuery != "" {
		parts = append(parts, fmt.Sprintf("query=%q", s.CustomQuery))
	}
	return strings.Join(parts, " ")
}

// TestNodePowerQueryGolden pins the node power queries: the contract
// queries verbatim, then every case in testdata/nodepower_queries.golden
// (-update rewrites it).
func TestNodePowerQueryGolden(t *testing.T) {
	for _, c := range nodePowerContract {
		primary, fallback, err := c.spec.Query()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if primary.PromQL != c.primary {
			t.Errorf("%s primary:\n got: %s\nwant: %s", c.name, primary.PromQL, c.primary)
		}
		if fallback.PromQL != c.fallback {
			t.Errorf("%s fallback:\n got: %s\nwant: %s", c.name, fallback.PromQL, c.fallback)
		}
		if primary.GroupLabel != c.wantGroup || primary.PerNode {
			t.Errorf("%s: group %q per node %t, want %q false", c.name, primary.GroupLabel, primary.PerNode, c.wantGroup)
		}
	}

	var b strings.Builder
	b.WriteString("# Node power queries of package inputs, one case per block: the spec, then the\n")
	b.WriteString("# primary query and, under resource attributes auto, the target_info fallback.\n")
	b.WriteString("# Generated by TestNodePowerQueryGolden; rewrite with go test ./pkg/controller/inputs/ -update.\n")
	for _, s := range nodePowerCases() {
		primary, fallback, err := s.Query()
		if err != nil {
			t.Fatalf("%s: %v", describeNodePowerSpec(s), err)
		}
		fmt.Fprintf(&b, "\n%s\n", describeNodePowerSpec(s))
		writeQuery(&b, "primary", primary)
		writeQuery(&b, "fallback", fallback)
	}
	checkGolden(t, "testdata/nodepower_queries.golden", b.String())
}

// TestKeplerHostScopeRejected: Kepler 0.10 and later have no host energy
// with one meaning across sources, so the kepler preset with scope host is
// an error, while kepler-legacy reads its platform counter.
func TestKeplerHostScopeRejected(t *testing.T) {
	_, _, err := NodePowerSpec{Preset: NodePowerPresetKepler, Scope: ScopeHost}.Query()
	if err == nil {
		t.Fatal("kepler preset with scope host: no error")
	}
	if !strings.Contains(err.Error(), "kepler-legacy") {
		t.Errorf("error %q does not point at kepler-legacy", err)
	}
	if _, _, err := (NodePowerSpec{Preset: NodePowerPresetKeplerLegacy, Scope: ScopeHost}).Query(); err != nil {
		t.Errorf("kepler-legacy with scope host: %v", err)
	}
}

// TestAutoReturnsFallback: only resource attributes auto of the otel
// preset returns a fallback, the target_info form of the labels form it
// returns as primary; labels and target_info return their own form alone,
// and the other presets never return a fallback.
func TestAutoReturnsFallback(t *testing.T) {
	for _, scope := range []Scope{ScopeComponents, ScopeHost} {
		for _, naming := range promname.Strategies() {
			base := NodePowerSpec{Scope: scope, Naming: naming}
			auto := base
			auto.ResourceAttributes = ResourceAttributesAuto
			labels := base
			labels.ResourceAttributes = ResourceAttributesLabels
			ti := base
			ti.ResourceAttributes = ResourceAttributesTargetInfo

			ap, af, err := auto.Query()
			if err != nil {
				t.Fatal(err)
			}
			lp, lf, err := labels.Query()
			if err != nil {
				t.Fatal(err)
			}
			tp, tf, err := ti.Query()
			if err != nil {
				t.Fatal(err)
			}
			name := string(scope) + "/" + string(naming)
			if af.PromQL == "" {
				t.Errorf("%s auto: no fallback", name)
			}
			if lf != (Query{}) || tf != (Query{}) {
				t.Errorf("%s: labels or target_info returned a fallback: %+v %+v", name, lf, tf)
			}
			if ap != lp {
				t.Errorf("%s: auto primary %q is not the labels form %q", name, ap.PromQL, lp.PromQL)
			}
			if af != tp {
				t.Errorf("%s: auto fallback %q is not the target_info form %q", name, af.PromQL, tp.PromQL)
			}
			if strings.Contains(ap.PromQL, promname.TargetInfo) || !strings.Contains(af.PromQL, promname.TargetInfo) {
				t.Errorf("%s: primary must not join target_info and the fallback must", name)
			}
			if ap.GroupLabel == "" || ap.GroupLabel != af.GroupLabel {
				t.Errorf("%s: group labels %q and %q, want the same node label", name, ap.GroupLabel, af.GroupLabel)
			}
		}
	}
	for _, s := range []NodePowerSpec{
		{Preset: NodePowerPresetKepler},
		{Preset: NodePowerPresetKeplerLegacy, Scope: ScopeHost},
		{Preset: NodePowerPresetCustom, CustomQuery: "x"},
	} {
		if _, fb, err := s.Query(); err != nil || fb != (Query{}) {
			t.Errorf("%s: fallback %+v, err %v; want none", describeNodePowerSpec(s), fb, err)
		}
	}
}

// TestNodePowerSpecDefaultsAndValidation: the zero spec is the otel preset
// under the defaults, a query alone selects custom, and unknown values or a
// query with another preset are errors rather than a silent default.
func TestNodePowerSpecDefaultsAndValidation(t *testing.T) {
	zero, zeroFB, err := NodePowerSpec{}.Query()
	if err != nil {
		t.Fatal(err)
	}
	explicit, explicitFB, err := NodePowerSpec{
		Preset: NodePowerPresetOTel, Scope: ScopeComponents, Naming: promname.Default,
		ResourceAttributes: ResourceAttributesAuto, Window: DefaultWindow,
	}.Query()
	if err != nil {
		t.Fatal(err)
	}
	if zero != explicit || zeroFB != explicitFB {
		t.Errorf("zero spec differs from the explicit defaults:\n%+v\n%+v", zero, explicit)
	}
	lower, _, err := NodePowerSpec{Naming: "noutf8escapingwithsuffixes"}.Query()
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _ := NodePowerSpec{Naming: promname.NoUTF8EscapingWithSuffixes}.Query()
	if lower != canonical {
		t.Errorf("a strategy in lower case gives %q, want %q", lower.PromQL, canonical.PromQL)
	}
	if q, _, err := (NodePowerSpec{CustomQuery: "x"}).Query(); err != nil || q.PromQL != "sum by (k8s_node_name) (x)" {
		t.Errorf("query without preset: %+v, %v; want the custom preset", q, err)
	}

	for _, s := range []NodePowerSpec{
		{Preset: "prometheus"},
		{Preset: NodePowerPresetOTel, CustomQuery: "x"},
		{Preset: NodePowerPresetKepler, CustomQuery: "x"},
		{Preset: NodePowerPresetCustom},
		{Preset: NodePowerPresetCustom, CustomQuery: "  "},
		{Scope: "node"},
		{ResourceAttributes: "promoted"},
		{Naming: "Underscore"},
		{Window: 5 * time.Second},
		{Window: 1500 * time.Millisecond},
	} {
		if _, _, err := s.Query(); err == nil {
			t.Errorf("%s (scope %q, window %s): no error", describeNodePowerSpec(s), s.Scope, s.Window)
		}
	}
}
