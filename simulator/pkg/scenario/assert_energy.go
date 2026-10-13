package scenario

import "github.com/matbun/joulie/pkg/otel/semconv"

// observeEnergy sums joulie.sim.workload.energy at its last snapshot over
// the scope and hardware type, per completed job of the scope with perJob.
func observeEnergy(ev *evaluation, a Assertion) map[string]observation {
	e := a.Energy
	kind, name, err := ParseEnergyScope(e.Scope)
	if err != nil {
		return map[string]observation{"": noData("%v", err)}
	}
	var joules float64
	n := 0
	for _, p := range latestPerSeries(ev.rd.Points, semconv.JoulieSimWorkloadEnergy.Name) {
		if hw, _ := p.Attributes.String(semconv.AttrHwType); e.HWType != "" && e.HWType != "all" && hw != e.HWType {
			continue
		}
		switch kind {
		case EnergyScopeClass:
			if c, _ := p.Attributes.String(semconv.AttrJoulieWorkloadClass); c != name {
				continue
			}
		case EnergyScopeNode:
			if node, _ := p.Attributes.String(semconv.AttrK8sNodeName); node != name {
				continue
			}
		}
		joules += p.Value
		n++
	}
	if n == 0 {
		return map[string]observation{"": noData("no %s series in scope %s", semconv.JoulieSimWorkloadEnergy.Name, e.Scope)}
	}
	if !e.PerJob {
		return map[string]observation{"": observed(joules, "%s J over %d series", formatFloat(joules), n)}
	}
	jobs := 0
	for _, j := range ev.jobs {
		if j.Outcome != semconv.JobOutcomeCompleted {
			continue
		}
		if (kind == EnergyScopeClass && j.Class != name) || (kind == EnergyScopeNode && j.Node != name) {
			continue
		}
		jobs++
	}
	if jobs == 0 {
		return map[string]observation{"": noData("no completed job in scope %s", e.Scope)}
	}
	return map[string]observation{"": observed(joules/float64(jobs), "%s J over %d completed jobs", formatFloat(joules), jobs)}
}
