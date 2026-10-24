package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// overviewDashboard is the Grafana dashboard the Joulie chart ships.
const overviewDashboard = "charts/joulie/dashboards/joulie-overview.json"

// shippedDashboards are the Grafana dashboards the charts ship, relative to
// the repository root.
const shippedDashboards = "charts/*/dashboards/*.json"

type dashboardGridPos struct {
	H, W, X, Y int
}

type dashboardPanel struct {
	ID        int              `json:"id"`
	Type      string           `json:"type"`
	Title     string           `json:"title"`
	GridPos   dashboardGridPos `json:"gridPos"`
	Collapsed bool             `json:"collapsed"`
	Panels    []dashboardPanel `json:"panels"`
}

func (p dashboardPanel) overlaps(q dashboardPanel) bool {
	a, b := p.GridPos, q.GridPos
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// No two panels of a dashboard a chart ships share grid cells, as Grafana
// lays them out: the top-level panels, collapsed rows included at their
// height of 1, and the panels inside each collapsed row, which Grafana places
// right below the row when it opens. A collapsed row of the overview left at
// y 46 sat on the Scheduler Extender panels, and its panels and those of the
// row above it opened at y 46 and 47 instead of below their rows.
func TestDashboardPanelsDoNotOverlap(t *testing.T) {
	root := repoRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, shippedDashboards))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, filepath.Join(root, overviewDashboard)) {
		t.Fatalf("%s matches %v, without %s; were the dashboards moved?", shippedDashboards, paths, overviewDashboard)
	}
	for _, path := range paths {
		rel, _ := filepath.Rel(root, path)
		t.Run(rel, func(t *testing.T) { checkDashboardLayout(t, path) })
	}
}

// checkDashboardLayout fails when two panels of the dashboard at path share
// grid cells, or a collapsed row's panels do not start right below it.
func checkDashboardLayout(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []dashboardPanel `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Panels) == 0 {
		t.Fatalf("%s has no panels", path)
	}
	checkNoOverlap := func(where string, ps []dashboardPanel) {
		for i := range ps {
			if g := ps[i].GridPos; g.W <= 0 || g.H <= 0 || g.X < 0 || g.Y < 0 || g.X+g.W > 24 {
				t.Errorf("%s: panel %d %q has gridPos %+v outside the 24-column grid", where, ps[i].ID, ps[i].Title, g)
			}
			for j := i + 1; j < len(ps); j++ {
				if ps[i].overlaps(ps[j]) {
					t.Errorf("%s: panel %d %q at %+v overlaps panel %d %q at %+v",
						where, ps[i].ID, ps[i].Title, ps[i].GridPos, ps[j].ID, ps[j].Title, ps[j].GridPos)
				}
			}
		}
	}
	checkNoOverlap("top level", d.Panels)
	for _, row := range d.Panels {
		if row.Type != "row" || !row.Collapsed {
			if len(row.Panels) > 0 {
				t.Errorf("panel %d %q holds %d panels but is not a collapsed row", row.ID, row.Title, len(row.Panels))
			}
			continue
		}
		where := "collapsed row " + row.Title
		checkNoOverlap(where, row.Panels)
		top := -1
		for _, p := range row.Panels {
			if top < 0 || p.GridPos.Y < top {
				top = p.GridPos.Y
			}
		}
		if want := row.GridPos.Y + row.GridPos.H; len(row.Panels) > 0 && top != want {
			t.Errorf("%s at y %d: its panels start at y %d, want %d, right below the row", where, row.GridPos.Y, top, want)
		}
	}
}
