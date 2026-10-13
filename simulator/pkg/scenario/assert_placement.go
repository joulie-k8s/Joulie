package scenario

import (
	"fmt"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// observePlacement counts the binds of pods of the class to a node whose
// schedulable class at the bind is forbidden. Such a bind is excused only
// when joulie.node.class_since shows the node had held that class for less
// than the grace: the extender scores with a cache up to CACHE_TTL old,
// refreshed after a controller reconcile, so a bind just after a flip is
// the cache's lag, not a violation. A bind without class_since is not
// known to be recent, so it counts. A bind whose node class is unknown
// (neither the schedulable class nor the power profile) cannot be judged
// and is reported; with no bind judged there is no data.
func observePlacement(ev *evaluation, a Assertion) map[string]observation {
	p := a.Placement
	grace := ev.env.PlacementGrace().Seconds()
	if p.GraceSeconds != nil {
		grace = *p.GraceSeconds
	}
	forbidden := map[string]bool{}
	for _, c := range p.Forbidden {
		forbidden[c] = true
	}
	var judged, violations, noSince, recent, unjudged int
	for _, e := range ev.rd.Events {
		if e.Name != semconv.EventSimJobScheduled.Name {
			continue
		}
		if c, _ := e.Attributes.String(semconv.AttrJoulieWorkloadClass); c != p.Class {
			continue
		}
		class, _ := e.Attributes.String(semconv.AttrJoulieNodeSchedulableClass)
		if class == "" {
			class, _ = e.Attributes.String(semconv.AttrJoulieNodePowerProfile)
		}
		if class == "" {
			unjudged++
			continue
		}
		judged++
		if !forbidden[class] {
			continue
		}
		switch since, ok := e.Attributes.Float(semconv.AttrJoulieNodeClassSince); {
		case !ok:
			violations++
			noSince++
		case since >= grace:
			violations++
		default:
			recent++
		}
	}
	if judged == 0 {
		return map[string]observation{"": noData("no %s pod was bound to a node of known class (%d without a node class)", p.Class, unjudged)}
	}
	msg := fmt.Sprintf("%d of %d %s binds on a forbidden class held for at least %s", violations, judged, p.Class,
		time.Duration(grace*float64(time.Second)))
	if noSince > 0 {
		msg += fmt.Sprintf(" (%d without %s)", noSince, semconv.AttrJoulieNodeClassSince)
	}
	msg += fmt.Sprintf("; %d more within the grace", recent)
	if unjudged > 0 {
		msg += fmt.Sprintf("; %d without a node class", unjudged)
	}
	return map[string]observation{"": observed(float64(violations), "%s", msg)}
}
