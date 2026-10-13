package calib

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// CB02: the CSV form of a sweep fits exactly as its OTLP form. The CSV is
// written from the decoded OTLP lines by a converter of its own, so the
// two readers are compared, not one reader with itself.
func TestCB02CSVFitEqualsOTLPFit(t *testing.T) {
	t.Parallel()
	sw := epycPerformanceSweep(t)
	fromOTLP := fit(t, sw, Options{})

	d, err := ReadCSV(bytes.NewReader(csvFromLines(t, sw.lines)))
	if err != nil {
		t.Fatalf("read the CSV: %v", err)
	}
	fromCSV, err := Fit(d, sw.profile, Options{})
	if err != nil {
		t.Fatalf("fit the CSV: %v", err)
	}
	if len(fromCSV.Params) != len(fromOTLP.Params) || fromCSV.Segments != fromOTLP.Segments || fromCSV.Steady != fromOTLP.Steady {
		t.Fatalf("CSV fit has %d params, %d segments, %d steady; OTLP %d, %d, %d",
			len(fromCSV.Params), fromCSV.Segments, fromCSV.Steady, len(fromOTLP.Params), fromOTLP.Segments, fromOTLP.Steady)
	}
	fitted := 0
	for i, o := range fromOTLP.Params {
		c := fromCSV.Params[i]
		if c.Path != o.Path || c.Fitted != o.Fitted || c.N != o.N || c.Reason != o.Reason ||
			math.Abs(c.Value-o.Value) > 1e-9*math.Max(1, math.Abs(o.Value)) {
			t.Errorf("%s: CSV %+v, OTLP %+v", o.Path, c, o)
		}
		if o.Fitted {
			fitted++
		}
	}
	if fitted < 5 {
		t.Errorf("only %d parameters fitted; the comparison needs the CPU fit at least", fitted)
	}
}

// The CSV reader names the line of a bad row, rejects values that are not
// finite and requires its three columns.
func TestReadCSVRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", "header"},
		{"no value column", "time_unix_nano,metric\n1,hw.power\n", "no value column"},
		{"bad time", "time_unix_nano,metric,value\nsoon,hw.power,3\n", "line 2"},
		{"bad value", "time_unix_nano,metric,value\n1,hw.power,lots\n", "line 2"},
		// ParseFloat reads both, and one NaN in a steady window made the
		// whole CPU fit NaN.
		{"NaN value", "time_unix_nano,metric,value\n1,hw.power,3\n2,hw.power,NaN\n", "line 3: value \"NaN\" is not a finite number"},
		{"Inf value", "time_unix_nano,metric,value\n1,hw.power,+Inf\n", "line 2: value \"+Inf\" is not a finite number"},
		{"duplicate column", "time_unix_nano,metric,value,hw.id,hw.id\n", "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadCSV(strings.NewReader(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want one containing %q", err, tc.want)
			}
		})
	}
	d, err := ReadCSV(strings.NewReader("time_unix_nano,metric,unit,value,hw.id,hw.type\n" +
		"1000000000,hw.energy,J,10,cpu0,cpu\n2000000000,hw.energy,J,30,cpu0,cpu\n3000000000,hw.energy,J,5,cpu0,cpu\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := d.Select("hw.energy", map[string]string{"hw.id": "cpu0"})
	if len(s) != 1 || !s[0].Counter || len(s[0].Points) != 3 {
		t.Fatalf("series %+v, want one counter of 3 points", s)
	}
	if w, n := s[0].rate(s[0].Points[0].Time, s[0].Points[2].Time); n != 1 || w != 20 {
		t.Errorf("rate %v over %d intervals, want 20 W over 1: the fall is a reset", w, n)
	}
}
