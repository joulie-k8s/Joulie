package scenario

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// energyCurve is one cumulative joulie.sim.workload.energy series from its
// start time: 0 at the start and before it, linear between its points, and
// its last value after them. Without a start time it is known only from
// its first point.
type energyCurve struct {
	start  time.Time
	times  []time.Time
	joules []float64
}

func (c energyCurve) at(t time.Time) (float64, bool) {
	i := sort.Search(len(c.times), func(i int) bool { return c.times[i].After(t) })
	switch {
	case i == len(c.times):
		return c.joules[i-1], true
	case i > 0:
		f := float64(t.Sub(c.times[i-1])) / float64(c.times[i].Sub(c.times[i-1]))
		return c.joules[i-1] + f*(c.joules[i]-c.joules[i-1]), true
	case c.start.IsZero():
		return 0, false
	case !t.After(c.start):
		return 0, true
	}
	return c.joules[0] * float64(t.Sub(c.start)) / float64(c.times[0].Sub(c.start)), true
}

// energySeries is one node's energy: the sum of its curves, read at the
// times any of them has a point or starts.
type energySeries struct {
	times  []time.Time
	curves []energyCurve
}

// at returns the node's energy at the latest of its times at or before t.
func (s energySeries) at(t time.Time) (time.Time, float64, bool) {
	i := sort.Search(len(s.times), func(i int) bool { return s.times[i].After(t) }) - 1
	if i < 0 {
		return time.Time{}, 0, false
	}
	sum := 0.0
	for _, c := range s.curves {
		v, ok := c.at(s.times[i])
		if !ok {
			return time.Time{}, 0, false
		}
		sum += v
	}
	return s.times[i], sum, true
}

// nodeEnergy groups joulie.sim.workload.energy by node, over every class and
// hardware type: the node's CPU package and GPU energy, the boundary of
// Joulie's default node power input. Each series (attributes, resource and
// start time, which a reset changes) is one curve, so series that start
// mid-run or are written at other times than the rest still sum to the
// node's energy at any time.
func nodeEnergy(points []Point) map[string]energySeries {
	type key struct {
		series string
		start  time.Time
	}
	byNode := map[string]map[key]map[time.Time]float64{}
	for _, p := range points {
		if p.Metric != semconv.JoulieSimWorkloadEnergy.Name {
			continue
		}
		node, _ := p.Attributes.String(semconv.AttrK8sNodeName)
		if byNode[node] == nil {
			byNode[node] = map[key]map[time.Time]float64{}
		}
		k := key{seriesKey(p), p.StartTime}
		if byNode[node][k] == nil {
			byNode[node][k] = map[time.Time]float64{}
		}
		byNode[node][k][p.Time] = p.Value
	}
	out := map[string]energySeries{}
	for node, curves := range byNode {
		var s energySeries
		seen := map[time.Time]bool{}
		addTime := func(t time.Time) {
			if !t.IsZero() && !seen[t] {
				seen[t] = true
				s.times = append(s.times, t)
			}
		}
		for k, values := range curves {
			c := energyCurve{start: k.start}
			for t := range values {
				c.times = append(c.times, t)
				addTime(t)
			}
			sort.Slice(c.times, func(i, j int) bool { return c.times[i].Before(c.times[j]) })
			for _, t := range c.times {
				c.joules = append(c.joules, values[t])
			}
			if c.start.Before(c.times[0]) {
				addTime(c.start)
			}
			s.curves = append(s.curves, c)
		}
		sort.Slice(s.times, func(i, j int) bool { return s.times[i].Before(s.times[j]) })
		out[node] = s
	}
	return out
}

// observeTwinPower compares each captured NodeTwin measured power after the
// warmup with the node's energy rate over the rate window ending at the
// sample, from the simulator's snapshots: the same window Joulie's rate()
// reads. The observed value is the quantile of the relative error over the
// samples; a sample whose node has no energy rate is skipped. Tolerance only
// adds to the message how many samples exceed it.
func observeTwinPower(ev *evaluation, a Assertion) map[string]observation {
	tp := a.TwinPower
	warmup := ev.env.TwinPowerWarmup()
	if tp.Warmup != nil {
		warmup = tp.Warmup.Duration
	}
	q := DefaultTwinPowerQuantile
	if tp.Quantile != nil {
		q = *tp.Quantile
	}
	window := ev.env.RateWindow
	nodes := map[string]bool{}
	for _, n := range tp.Nodes {
		nodes[n] = true
	}
	energy := nodeEnergy(ev.rd.Points)
	from := ev.rd.Start.Add(warmup)
	var errs []float64
	worst, worstNode, over := 0.0, "", 0
	for _, p := range ev.rd.Points {
		if p.Metric != semconv.JoulieCaptureNodeTwinMeasuredPower.Name || p.Time.Before(from) {
			continue
		}
		node, _ := p.Attributes.String(semconv.AttrK8sNodeName)
		if len(nodes) > 0 && !nodes[node] {
			continue
		}
		s, ok := energy[node]
		if !ok {
			continue
		}
		t2, e2, ok2 := s.at(p.Time)
		t1, e1, ok1 := s.at(p.Time.Add(-window))
		if !ok1 || !ok2 || !t2.After(t1) {
			continue
		}
		truth := (e2 - e1) / t2.Sub(t1).Seconds()
		if truth <= 0 {
			continue
		}
		rel := math.Abs(p.Value-truth) / truth
		errs = append(errs, rel)
		if rel > worst {
			worst, worstNode = rel, node
		}
		if tp.Tolerance != nil && rel > *tp.Tolerance {
			over++
		}
	}
	if len(errs) == 0 {
		return map[string]observation{"": noData("no NodeTwin sample after the %s warmup has a node energy rate to compare with", warmup)}
	}
	n := len(errs)
	msg := fmt.Sprintf("relative error quantile %s over %d samples after %s; worst %s on %s", formatFloat(q), n, warmup, formatFloat(worst), worstNode)
	if tp.Tolerance != nil {
		msg += fmt.Sprintf("; %d above the tolerance %s", over, formatFloat(*tp.Tolerance))
	}
	v := Quantile(errs, q)
	return map[string]observation{"": {Value: &v, Msg: msg}}
}
