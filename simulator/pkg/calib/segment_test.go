package calib

import (
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// setpointSeries is a setpoint series of knob with one point per second
// from t0, holding values.
func setpointSeries(knob, class string, t0 time.Time, values ...float64) *Series {
	s := &Series{Metric: semconv.JoulieCalibrationSetpoint.Name, Attrs: map[string]string{semconv.AttrCalibrationKnob: knob}}
	if class != "" {
		s.Attrs[semconv.AttrCalibrationClass] = class
	}
	for i, v := range values {
		s.Points = append(s.Points, Point{Time: t0.Add(time.Duration(i) * time.Second), Value: v})
	}
	return s
}

// A segment ends where any knob changes, and its steady window keeps the
// samples from drop after its first one.
func TestSegmentsSplitWhereAnyKnobChanges(t *testing.T) {
	t0 := time.Unix(1_760_000_000, 0)
	d := &Data{Series: []*Series{
		setpointSeries(semconv.KnobCPUUtilization, "cpu.compute_bound", t0, 0, 0, 0, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5),
		setpointSeries(semconv.KnobCPUFrequencyMax, "", t0, 2e9, 2e9, 2e9, 2e9, 2e9, 2e9, 1e9, 1e9, 1e9),
	}}
	segs, err := Segments(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("%d segments, want 3", len(segs))
	}
	for i, want := range []struct {
		n    int
		u, f float64
	}{{3, 0, 2e9}, {3, 0.5, 2e9}, {3, 0.5, 1e9}} {
		s := segs[i]
		if len(s.Times) != want.n || s.Setpoints[semconv.KnobCPUUtilization] != want.u || s.Setpoints[semconv.KnobCPUFrequencyMax] != want.f {
			t.Errorf("segment %d: %d samples at %v, want %d at u %v f %v", i, len(s.Times), s.Setpoints, want.n, want.u, want.f)
		}
		if s.Classes[semconv.KnobCPUUtilization] != "cpu.compute_bound" {
			t.Errorf("segment %d: classes %v", i, s.Classes)
		}
	}
	if from, to, n := segs[1].Steady(time.Second); n != 2 || !from.Equal(t0.Add(4*time.Second)) || !to.Equal(t0.Add(5*time.Second)) {
		t.Errorf("steady window %v to %v with %d samples, want 4 s to 5 s with 2", from.Sub(t0), to.Sub(t0), n)
	}
	if _, err := Segments(&Data{}); err == nil || !strings.Contains(err.Error(), "joulie.calibration.setpoint") {
		t.Errorf("a sweep without setpoints: err %v", err)
	}
}
