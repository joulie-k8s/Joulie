package inputs

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The facility queries the telemetry contract names, verbatim, under the
// default naming and window, except the node inlet: the contract read only
// the promoted node label, which leaves the inlet empty on a
// Prometheus that does not promote k8s.node.name, so the query also joins
// target_info for series without it, as the node power target_info form
// does.
var facilityContract = map[FacilityInput]Query{
	FacilityInputAmbientTemp:     {PromQL: `avg(joulie_facility_ambient_temperature_celsius)`},
	FacilityInputITPower:         {PromQL: `sum(rate(joulie_facility_it_energy_joules_total[5m])) or sum(joulie_facility_it_power_watts)`},
	FacilityInputCoolingPower:    {PromQL: `sum(rate(joulie_facility_cooling_energy_joules_total[5m])) or sum(joulie_facility_cooling_power_watts)`},
	FacilityInputZoneAmbientTemp: {PromQL: `avg by (joulie_cooling_zone_name) (joulie_cooling_zone_ambient_temperature_celsius)`, GroupLabel: "joulie_cooling_zone_name"},
	FacilityInputRackPower:       {PromQL: `sum by (joulie_rack_name) (rate(joulie_rack_energy_joules_total[5m])) or on (joulie_rack_name) sum by (joulie_rack_name) (joulie_rack_power_watts)`, GroupLabel: "joulie_rack_name"},
	FacilityInputNodeInletTemp:   {PromQL: `max by (k8s_node_name) (hw_host_ambient_temperature_celsius{k8s_node_name!=""} or hw_host_ambient_temperature_celsius{k8s_node_name=""} * on (job, instance) group_left (k8s_node_name) max by (job, instance, k8s_node_name) (target_info and on (job, instance) hw_host_ambient_temperature_celsius{k8s_node_name=""}))`, GroupLabel: "k8s_node_name"},
}

// facilityCases is every spec the golden file pins.
func facilityCases() []FacilitySpec {
	var specs []FacilitySpec
	for _, naming := range promname.Strategies() {
		specs = append(specs, FacilitySpec{Preset: FacilityPresetOTel, Naming: naming})
	}
	specs = append(specs,
		FacilitySpec{Preset: FacilityPresetOTel, Window: 2 * time.Minute},
		FacilitySpec{Preset: FacilityPresetLegacy},
		FacilitySpec{Preset: FacilityPresetLegacy, Overrides: map[FacilityInput]string{
			FacilityInputAmbientTemp: "site_outdoor_temperature_celsius",
			FacilityInputITPower:     `sum(ups_output_power_watts{site="a"})`,
		}},
		FacilitySpec{Preset: FacilityPresetCustom},
		FacilitySpec{Preset: FacilityPresetCustom, Naming: promname.NoUTF8EscapingWithSuffixes, Overrides: map[FacilityInput]string{
			FacilityInputCoolingPower:    "chiller_power_watts",
			FacilityInputZoneAmbientTemp: "crac_return_temperature_celsius",
			FacilityInputRackPower:       "pdu_power_watts",
			FacilityInputNodeInletTemp:   "ipmi_inlet_temperature_celsius",
		}},
		FacilitySpec{Preset: FacilityPresetOTel, Overrides: map[FacilityInput]string{
			FacilityInputRackPower:     `sum by (joulie_rack_name) (label_replace(pdu_power_watts, "joulie_rack_name", "$1", "pdu", "(.*)"))`,
			FacilityInputNodeInletTemp: "",
		}},
		FacilitySpec{Preset: FacilityPresetOTel,
			Overrides:       map[FacilityInput]string{FacilityInputCarbonIntensity: "electricitymaps_carbon_intensity"},
			CarbonIntensity: CarbonIntensitySpec{Signal: CarbonSignalAverage}},
		FacilitySpec{Preset: FacilityPresetCustom,
			Overrides:       map[FacilityInput]string{FacilityInputCarbonIntensity: `watttime_co2_moer{region="CAISO_NORTH"}`},
			CarbonIntensity: CarbonIntensitySpec{Unit: "lbsCO2/MWh", Signal: CarbonSignalMarginal}},
	)
	return specs
}

func describeFacilitySpec(s FacilitySpec) string {
	parts := []string{"preset=" + string(s.Preset)}
	if s.Naming != "" {
		parts = append(parts, "naming="+string(s.Naming))
	}
	if s.Window != 0 {
		parts = append(parts, "window="+s.Window.String())
	}
	var keys []string
	for k := range s.Overrides {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("override.%s=%q", k, s.Overrides[FacilityInput(k)]))
	}
	if s.CarbonIntensity.Unit != "" {
		parts = append(parts, fmt.Sprintf("carbon.unit=%q", s.CarbonIntensity.Unit))
	}
	if s.CarbonIntensity.Signal != "" {
		parts = append(parts, "carbon.signal="+string(s.CarbonIntensity.Signal))
	}
	return strings.Join(parts, " ")
}

// TestFacilityQueriesGolden pins the facility queries: the contract queries
// verbatim, then every case in testdata/facility_queries.golden (-update
// rewrites it).
func TestFacilityQueriesGolden(t *testing.T) {
	got, err := FacilitySpec{}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(facilityContract) {
		t.Errorf("default spec reads %d inputs, want %d", len(got), len(facilityContract))
	}
	for input, want := range facilityContract {
		if got[input] != want {
			t.Errorf("%s:\n got: %+v\nwant: %+v", input, got[input], want)
		}
	}

	var b strings.Builder
	b.WriteString("# Facility queries of package inputs, one case per block: the spec, then the query of\n")
	b.WriteString("# each input it reads, in FacilityInputs order.\n")
	b.WriteString("# Generated by TestFacilityQueriesGolden; rewrite with go test ./pkg/controller/inputs/ -update.\n")
	for _, s := range facilityCases() {
		qs, err := s.Queries()
		if err != nil {
			t.Fatalf("%s: %v", describeFacilitySpec(s), err)
		}
		fmt.Fprintf(&b, "\n%s\n", describeFacilitySpec(s))
		for _, input := range FacilityInputs() {
			if q, ok := qs[input]; ok {
				writeQuery(&b, string(input), q)
			}
		}
	}
	checkGolden(t, "testdata/facility_queries.golden", b.String())
}

// TestFacilityOverrides: a bare metric name is aggregated as its input
// is (avg for a temperature and the carbon intensity, sum for a power, by
// the key label of a keyed input), any other query is kept as written, an
// empty one is no override, and an unknown input or preset is an error, as
// is a carbon intensity query whose unit or signal is unknown or missing.
func TestFacilityOverrides(t *testing.T) {
	qs, err := FacilitySpec{Preset: FacilityPresetLegacy, Overrides: map[FacilityInput]string{
		FacilityInputAmbientTemp:     "outdoor_temp",
		FacilityInputITPower:         "it:power:watts",
		FacilityInputCoolingPower:    `max(cooling_watts)`,
		FacilityInputZoneAmbientTemp: "zone_temp",
		FacilityInputRackPower:       "rack_watts",
		FacilityInputNodeInletTemp:   "",
		FacilityInputCarbonIntensity: "grid_carbon_intensity",
	}, CarbonIntensity: CarbonIntensitySpec{Signal: CarbonSignalAverage}}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	want := map[FacilityInput]Query{
		FacilityInputAmbientTemp:     {PromQL: "avg(outdoor_temp)"},
		FacilityInputITPower:         {PromQL: "sum(it:power:watts)"},
		FacilityInputCoolingPower:    {PromQL: "max(cooling_watts)"},
		FacilityInputZoneAmbientTemp: {PromQL: "avg by (joulie_cooling_zone_name) (zone_temp)", GroupLabel: "joulie_cooling_zone_name"},
		FacilityInputRackPower:       {PromQL: "sum by (joulie_rack_name) (rack_watts)", GroupLabel: "joulie_rack_name"},
		FacilityInputCarbonIntensity: {PromQL: "avg(grid_carbon_intensity)"},
	}
	if len(qs) != len(want) {
		t.Errorf("got %d queries, want %d: %+v", len(qs), len(want), qs)
	}
	for input, w := range want {
		if qs[input] != w {
			t.Errorf("%s: got %+v, want %+v", input, qs[input], w)
		}
	}

	if qs, err := (FacilitySpec{Preset: FacilityPresetCustom}).Queries(); err != nil || len(qs) != 0 {
		t.Errorf("custom without overrides: %+v, %v; want no query", qs, err)
	}
	carbon := map[FacilityInput]string{FacilityInputCarbonIntensity: "grid_carbon_intensity"}
	for _, s := range []FacilitySpec{
		{Preset: "datacenter"},
		{Overrides: map[FacilityInput]string{"pue": "x"}},
		{Naming: "utf8"},
		{Window: time.Second},
		{Overrides: carbon},
		{Overrides: carbon, CarbonIntensity: CarbonIntensitySpec{Signal: "forecast"}},
		{Overrides: carbon, CarbonIntensity: CarbonIntensitySpec{Unit: "gCO2eq/MJ", Signal: CarbonSignalMarginal}},
	} {
		if _, err := s.Queries(); err == nil {
			t.Errorf("%s: no error", describeFacilitySpec(s))
		}
	}
	// Without a carbon intensity query its unit and signal are not read.
	if _, err := (FacilitySpec{CarbonIntensity: CarbonIntensitySpec{Unit: "furlongs"}}).Queries(); err != nil {
		t.Errorf("a carbon unit without a carbon query: %v", err)
	}
}

// TestFacilityMetricNamesCoverEveryPresetQuery: for each input,
// FacilityMetricNames lists no name twice, every name it lists is selected
// by the otel or legacy preset query under some strategy, and every such
// query selects one of them. Across inputs the names are exactly
// semconv.FacilityInputs under every strategy plus the legacy names, so a
// name dropped from an input whose query selects another one fails too.
// tests/promql checks each input's exact set with the PromQL parser
// (TestFacilityMetricNamesMatchPresetQueries).
func TestFacilityMetricNamesCoverEveryPresetQuery(t *testing.T) {
	for _, input := range FacilityInputs() {
		names := FacilityMetricNames(input)
		if input == FacilityInputCarbonIntensity {
			// No preset reads it, so no preset query selects a name: the
			// controller manager exports joulie.grid.carbon_intensity
			// itself and would read its own export back.
			if len(names) != 0 {
				t.Errorf("%s: names %v, want none", input, names)
			}
			specs := []FacilitySpec{{Preset: FacilityPresetLegacy}}
			for _, naming := range promname.Strategies() {
				specs = append(specs, FacilitySpec{Naming: naming})
			}
			for _, s := range specs {
				if qs, err := s.Queries(); err != nil || qs[input].PromQL != "" {
					t.Errorf("%s: %s queries %q (%v), want no query", input, describeFacilitySpec(s), qs[input].PromQL, err)
				}
			}
			for _, m := range append(semconv.NodeInputs(), semconv.FacilityInputs()...) {
				if m.Name == semconv.JoulieGridCarbonIntensity.Name {
					t.Errorf("%s is an input metric; the controller manager exports it, so no producer may write it", m.Name)
				}
			}
			continue
		}
		if len(names) == 0 {
			t.Fatalf("%s: no names", input)
		}
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				t.Errorf("%s: %q listed twice", input, n)
			}
			seen[n] = true
		}
		used := map[string]bool{}
		for _, s := range []FacilitySpec{{Preset: FacilityPresetLegacy}} {
			qs, _ := s.Queries()
			markUsed(t, input, qs[input], names, used)
		}
		for _, naming := range promname.Strategies() {
			qs, err := FacilitySpec{Naming: naming}.Queries()
			if err != nil {
				t.Fatal(err)
			}
			markUsed(t, input, qs[input], names, used)
		}
		for _, n := range names {
			if !used[n] {
				t.Errorf("%s: %q is listed but no preset query selects it", input, n)
			}
		}
	}
	if got := FacilityMetricNames("pue"); got != nil {
		t.Errorf("unknown input: %v, want nil", got)
	}

	want := map[string]bool{legacyAmbientTemp: true, legacyITPower: true, legacyCoolingPower: true}
	for _, naming := range promname.Strategies() {
		for _, m := range semconv.FacilityInputs() {
			want[naming.MetricName(m)] = true
		}
	}
	got := map[string]bool{}
	for _, input := range FacilityInputs() {
		for _, n := range FacilityMetricNames(input) {
			got[n] = true
		}
	}
	var missing, extra []string
	for n := range want {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	for n := range got {
		if !want[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("FacilityMetricNames across inputs: missing %v, not a facility input %v", missing, extra)
	}
}

// markUsed records which of names q selects, and fails when it selects
// none: a query whose metric is missing from FacilityMetricNames.
func markUsed(t *testing.T, input FacilityInput, q Query, names []string, used map[string]bool) {
	t.Helper()
	if q.PromQL == "" {
		return
	}
	found := false
	for _, n := range names {
		for _, sel := range []string{n + "{", n + ")", n + "[", `"` + n + `"`} {
			if strings.Contains(q.PromQL, sel) {
				used[n] = true
				found = true
			}
		}
	}
	if !found {
		t.Errorf("%s: query %q selects no metric of FacilityMetricNames %v", input, q.PromQL, names)
	}
}
