package semconv

import (
	"reflect"
	"testing"
)

// The calibration names are the contract between a sweep collector and
// hwcalib fit: a sweep recorded today must still fit after a release, so
// each name is pinned here.
func TestCalibrationNamesFrozen(t *testing.T) {
	frozen := []struct {
		v           Metric
		name, unit  string
		owner       string
		kind        Kind
		value       ValueType
		attrs       []string
		description string
	}{
		{SystemCPUUtilization, "system.cpu.utilization", "1", OwnerSemconv, Gauge, Double, []string{"cpu.mode", "cpu.logical_number"}, "semconv v1.44.0 model/system/metrics.yaml"},
		{JoulieCalibrationSetpoint, "joulie.calibration.setpoint", "{setpoint}", OwnerJoulie, Gauge, Double,
			[]string{"joulie.calibration.knob", "joulie.calibration.class", "joulie.calibration.memory_intensity"}, "Joulie's own"},
	}
	registered := indexMetrics()
	for _, f := range frozen {
		if f.v.Name != f.name || f.v.Unit != f.unit || f.v.Owner != f.owner || f.v.Kind != f.kind || f.v.Value != f.value {
			t.Errorf("%s (%s): got (%q, %q, %q, %v, %v)", f.name, f.description, f.v.Name, f.v.Unit, f.v.Owner, f.v.Kind, f.v.Value)
		}
		if !reflect.DeepEqual(sortedCopy(f.v.Attributes), sortedCopy(f.attrs)) {
			t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
		}
		if got, ok := registered[f.name]; !ok || !reflect.DeepEqual(got, f.v) {
			t.Errorf("%s: registered %+v (%v), exported variable %+v", f.name, got, ok, f.v)
		}
	}

	attrs := map[string]bool{}
	for _, a := range Attributes() {
		attrs[a] = true
	}
	for got, want := range map[string]string{
		AttrCPUMode: "cpu.mode", AttrCPULogicalNumber: "cpu.logical_number",
		AttrCalibrationKnob: "joulie.calibration.knob", AttrCalibrationClass: "joulie.calibration.class",
		AttrCalibrationMemoryIntensity: "joulie.calibration.memory_intensity",
	} {
		if got != want {
			t.Errorf("attribute constant %q, want %q", got, want)
		}
		if !attrs[got] {
			t.Errorf("attribute %q is not registered", got)
		}
	}
	want := []string{"cpu.utilization", "cpu.frequency.max", "cpu.power.limit", "gpu.utilization", "gpu.power.limit"}
	if got := Knobs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Knobs() = %v, want %v", got, want)
	}
	if CPUModeIdle != "idle" {
		t.Errorf("CPUModeIdle = %q, want idle", CPUModeIdle)
	}
}
