package calib

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// CSV columns. A sweep in CSV has one row per point and a header row that
// names the columns: these three, and any other column is a point attribute
// named by its header, such as hw.id, hw.type, hw.limit_type,
// joulie.hw.limit.state, cpu.logical_number, cpu.mode,
// joulie.calibration.knob, joulie.calibration.class and
// joulie.calibration.memory_intensity. An empty cell is an absent attribute.
const (
	csvTime   = "time_unix_nano"
	csvMetric = "metric"
	csvValue  = "value"
	// csvUnit and csvStartTime are optional: the unit of the metric and,
	// for a counter, the start of its series, whose change marks a reset.
	csvUnit      = "unit"
	csvStartTime = "start_time_unix_nano"
)

// ReadCSV reads a sweep of one node in CSV, with the metric names of the
// OTLP form. A counter is a metric the semconv table registers as one, and
// its values are cumulative; a value that falls is a reset. Every other
// metric is read as a gauge. A value must be a finite number: NaN and Inf
// are errors, since one in a steady window turns every mean it enters into
// NaN.
func ReadCSV(r io.Reader) (*Data, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 0
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("csv: empty input, want a header row")
		}
		return nil, fmt.Errorf("csv: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, fmt.Errorf("csv: header column %d is empty", i+1)
		}
		if _, dup := col[h]; dup {
			return nil, fmt.Errorf("csv: header names %q twice", h)
		}
		col[h] = i
		header[i] = h
	}
	for _, need := range []string{csvTime, csvMetric, csvValue} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("csv: the header has no %s column", need)
		}
	}
	counters := map[string]bool{}
	for _, m := range semconv.Metrics() {
		counters[m.Name] = m.Kind == semconv.Counter
	}

	bd := newDataBuilder()
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: %w", err)
		}
		line, _ := cr.FieldPos(0)
		cell := func(name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		metric := cell(csvMetric)
		if metric == "" {
			return nil, fmt.Errorf("csv line %d: no metric", line)
		}
		ns, err := strconv.ParseUint(cell(csvTime), 10, 64)
		if err != nil || ns == 0 {
			return nil, fmt.Errorf("csv line %d: %s %q is not a time in nanoseconds", line, csvTime, cell(csvTime))
		}
		// ParseFloat also reads NaN and Inf.
		v, err := strconv.ParseFloat(cell(csvValue), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("csv line %d: value %q is not a finite number", line, cell(csvValue))
		}
		p := Point{Time: unixNano(ns), Value: v}
		if s := cell(csvStartTime); s != "" {
			start, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("csv line %d: %s %q is not a time in nanoseconds", line, csvStartTime, s)
			}
			if start != 0 {
				p.Start = unixNano(start)
			}
		}
		attrs := map[string]string{}
		for i, h := range header {
			switch h {
			case csvTime, csvMetric, csvValue, csvUnit, csvStartTime:
				continue
			}
			if i < len(rec) {
				if s := strings.TrimSpace(rec[i]); s != "" {
					attrs[h] = s
				}
			}
		}
		bd.add(metric, cell(csvUnit), attrs, counters[metric], false, p)
	}
	return bd.data(""), nil
}
