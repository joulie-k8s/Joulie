package calib

import (
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Segment is one stretch of a sweep at the same setpoints: consecutive
// samples whose joulie.calibration.setpoint points agree in every knob,
// value, class and memory intensity. Times are the sample times, ascending.
// Setpoints holds each knob's value, and Classes and Memory the class and
// memory intensity of the load a knob drives, for the knobs that carry them.
type Segment struct {
	Times     []time.Time
	Setpoints map[string]float64
	Classes   map[string]string
	Memory    map[string]float64
}

// Start is the time of the segment's first sample.
func (s Segment) Start() time.Time { return s.Times[0] }

// End is the time of the segment's last sample.
func (s Segment) End() time.Time { return s.Times[len(s.Times)-1] }

// Steady is the steady window of the segment: from drop after its first
// sample to its last, and the number of samples in it. A segment starts at
// the first sample that shows its setpoints, so the window starts at least
// drop after the change.
func (s Segment) Steady(drop time.Duration) (from, to time.Time, n int) {
	from = s.Start().Add(drop)
	for _, t := range s.Times {
		if !t.Before(from) {
			n++
		}
	}
	return from, s.End(), n
}

// setpointState is what one sample holds: knob to value, class and memory
// intensity.
type setpointState struct {
	value map[string]float64
	class map[string]string
	mem   map[string]float64
}

func (a setpointState) equal(b setpointState) bool {
	if len(a.value) != len(b.value) || len(a.class) != len(b.class) || len(a.mem) != len(b.mem) {
		return false
	}
	for k, v := range a.value {
		if w, ok := b.value[k]; !ok || w != v {
			return false
		}
	}
	for k, v := range a.class {
		if b.class[k] != v {
			return false
		}
	}
	for k, v := range a.mem {
		if w, ok := b.mem[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Segments splits the sweep into its setpoint segments, in time order. Each
// sample time with joulie.calibration.setpoint points is one sample; a
// sweep records one point per knob it holds at every sample.
func Segments(d *Data) ([]Segment, error) {
	states := map[time.Time]setpointState{}
	for _, s := range d.Select(semconv.JoulieCalibrationSetpoint.Name, nil) {
		knob := s.Attrs[semconv.AttrCalibrationKnob]
		if knob == "" {
			continue
		}
		class := s.Attrs[semconv.AttrCalibrationClass]
		mem, hasMem := 0.0, false
		if txt, ok := s.Attrs[semconv.AttrCalibrationMemoryIntensity]; ok {
			if v, err := strconv.ParseFloat(txt, 64); err == nil {
				mem, hasMem = v, true
			}
		}
		for _, p := range s.Points {
			st, ok := states[p.Time]
			if !ok {
				st = setpointState{value: map[string]float64{}, class: map[string]string{}, mem: map[string]float64{}}
				states[p.Time] = st
			}
			st.value[knob] = p.Value
			if class != "" {
				st.class[knob] = class
			}
			if hasMem {
				st.mem[knob] = mem
			}
		}
	}
	if len(states) == 0 {
		return nil, errors.New("the sweep has no joulie.calibration.setpoint points: a sweep records the setpoints it holds, one point per knob at every sample")
	}
	times := make([]time.Time, 0, len(states))
	for t := range states {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	var segs []Segment
	var last setpointState
	for i, t := range times {
		st := states[t]
		if i == 0 || !st.equal(last) {
			segs = append(segs, Segment{Setpoints: st.value, Classes: st.class, Memory: st.mem})
			last = st
		}
		segs[len(segs)-1].Times = append(segs[len(segs)-1].Times, t)
	}
	return segs, nil
}
