package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The screens of the live dashboard.
const (
	screenNodes = iota
	screenNode
	screenPods
)

// The node orders the s key cycles through.
var sortNames = []string{"name", "power", "headroom", "cooling", "class"}

// historyLen is how many readings the sparklines keep per node.
const historyLen = 60

type snapMsg struct {
	c   cluster
	err error
}

type tickMsg time.Time

// dashboard is the bubbletea model of kubectl joulie top.
type dashboard struct {
	read     func(context.Context) (cluster, error)
	context  string
	interval time.Duration

	width, height int
	snap          cluster
	have          bool
	err           error
	history       map[string][]float64
	total         []float64
	seen          map[string]time.Time // the twin update each history last took

	screen int
	cursor int
	sortBy int
	node   string // the node of screenNode
}

func newDashboard(read func(context.Context) (cluster, error), contextName string, interval time.Duration, start int, node string) *dashboard {
	return &dashboard{read: read, context: contextName, interval: interval, screen: start, node: node,
		history: map[string][]float64{}, seen: map[string]time.Time{}}
}

// dashboardStart is the screen the dashboard opens on for a command: the
// node for node NAME, the pods for pods, else the node list.
func dashboardStart(o options) (int, string) {
	switch o.command {
	case "node":
		return screenNode, o.args[0]
	case "pods":
		return screenPods, ""
	}
	return screenNodes, ""
}

func runDashboard(c clients, contextName string, o options) error {
	read := func(ctx context.Context) (cluster, error) { return readCluster(ctx, c, podNamespace(o)) }
	start, node := dashboardStart(o)
	d := newDashboard(read, contextName, o.interval, start, node)
	opts := []tea.ProgramOption{}
	if o.noColor {
		opts = append(opts, tea.WithEnvironment(append(environ(), "NO_COLOR=1")))
	}
	_, err := tea.NewProgram(d, opts...).Run()
	return err
}

func (d *dashboard) fetch() tea.Cmd {
	return func() tea.Msg {
		c, err := d.read(context.Background())
		return snapMsg{c: c, err: err}
	}
}

func (d *dashboard) Init() tea.Cmd { return d.fetch() }

// record appends the readings of a snapshot to the sparkline histories: a
// node's reading when its twin was rewritten since the last one, so a poll
// faster than the controller manager adds no flat steps, and the cluster
// total whenever any node's did.
func (d *dashboard) record(c cluster) {
	var sum float64
	changed := false
	present := map[string]bool{}
	for _, n := range c.nodes {
		present[n.name] = true
		sum += n.measuredW
		if last, ok := d.seen[n.name]; ok && !n.updated.IsZero() && !n.updated.After(last) {
			continue
		}
		d.seen[n.name] = n.updated
		changed = true
		d.history[n.name] = appendCapped(d.history[n.name], n.measuredW)
	}
	for name := range d.history {
		if !present[name] {
			delete(d.history, name)
			delete(d.seen, name)
		}
	}
	if changed {
		d.total = appendCapped(d.total, sum)
	}
}

// appendCapped appends v and keeps the last historyLen values.
func appendCapped(h []float64, v float64) []float64 {
	h = append(h, v)
	if len(h) > historyLen {
		h = h[len(h)-historyLen:]
	}
	return h
}

// sorted is the nodes in the current order.
func (d *dashboard) sorted() []nodeView {
	nodes := append([]nodeView(nil), d.snap.nodes...)
	less := map[string]func(a, b nodeView) bool{
		"name":     func(a, b nodeView) bool { return a.name < b.name },
		"power":    func(a, b nodeView) bool { return a.measuredW > b.measuredW },
		"headroom": func(a, b nodeView) bool { return a.headroom < b.headroom },
		"cooling":  func(a, b nodeView) bool { return a.cooling > b.cooling },
		"class":    func(a, b nodeView) bool { return a.class+a.name < b.class+b.name },
	}[sortNames[d.sortBy]]
	sort.SliceStable(nodes, func(i, j int) bool { return less(nodes[i], nodes[j]) })
	return nodes
}

func (d *dashboard) rows() int {
	switch d.screen {
	case screenPods:
		return len(d.snap.pods)
	case screenNodes:
		return len(d.snap.nodes)
	}
	return 0
}

func (d *dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.width, d.height = msg.Width, msg.Height
	case snapMsg:
		if msg.err != nil {
			d.err = msg.err
		} else {
			d.err, d.snap, d.have = nil, msg.c, true
			d.record(msg.c)
			if r := d.rows(); d.cursor >= r {
				d.cursor = max(r-1, 0)
			}
		}
		return d, tea.Tick(d.interval, func(t time.Time) tea.Msg { return tickMsg(t) })
	case tickMsg:
		return d, d.fetch()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return d, tea.Quit
		case "up", "k":
			if d.cursor > 0 {
				d.cursor--
			}
		case "down", "j":
			if d.cursor < d.rows()-1 {
				d.cursor++
			}
		case "home", "g":
			d.cursor = 0
		case "end", "G":
			d.cursor = max(d.rows()-1, 0)
		case "enter", "right", "l":
			if d.screen == screenNodes && d.cursor < len(d.snap.nodes) {
				d.node, d.screen = d.sorted()[d.cursor].name, screenNode
			} else if d.screen == screenPods && d.cursor < len(d.snap.pods) {
				d.node, d.screen = d.snap.pods[d.cursor].node, screenNode
			}
		case "esc", "backspace", "left", "h", "n":
			if d.screen != screenNodes {
				d.screen, d.cursor = screenNodes, d.indexOf(d.node)
			}
		case "p":
			d.screen, d.cursor = screenPods, 0
		case "s":
			if d.screen == screenNodes {
				d.sortBy = (d.sortBy + 1) % len(sortNames)
			}
		case "r":
			return d, d.fetch()
		}
	}
	return d, nil
}

// indexOf is the row of a node in the current order, 0 when absent.
func (d *dashboard) indexOf(name string) int {
	for i, n := range d.sorted() {
		if n.name == name {
			return i
		}
	}
	return 0
}

// tableChrome is the lines around the rows of a windowed list: the table's
// top border, header, header rule and bottom border, the "rows x to y of
// z" line and the footer.
const tableChrome = 6

// window is the slice [lo, hi) of rows that fits in the space left, kept
// around the cursor.
func window(cursor, total, room int) (int, int) {
	if room <= 0 || total <= room {
		return 0, total
	}
	lo := cursor - room/2
	lo = max(0, min(lo, total-room))
	return lo, lo + room
}

func (d *dashboard) footer() string {
	key := func(k, what string) string { return stTitle.Render(k) + " " + stMuted.Render(what) }
	var keys []string
	switch d.screen {
	case screenNodes:
		keys = []string{key("↑↓", "select"), key("enter", "details"), key("p", "pods"), key("s", "sort: "+sortNames[d.sortBy]), key("r", "refresh"), key("q", "quit")}
	case screenNode:
		keys = []string{key("esc", "back"), key("p", "pods"), key("r", "refresh"), key("q", "quit")}
	case screenPods:
		keys = []string{key("↑↓", "select"), key("enter", "node"), key("n", "nodes"), key("r", "refresh"), key("q", "quit")}
	}
	status := stMuted.Render(fmt.Sprintf("every %s", d.interval))
	if d.err != nil {
		status = stBad.Render("✗ " + d.err.Error())
	}
	return " " + strings.Join(keys, stFaint.Render("  ·  ")) + stFaint.Render("  ·  ") + status
}

func (d *dashboard) View() tea.View {
	var body string
	switch {
	case !d.have && d.err != nil:
		body = header("cluster energy", d.context, time.Now(), d.width) + "\n\n " + stBad.Render("✗ "+d.err.Error())
	case !d.have:
		body = header("cluster energy", d.context, time.Now(), d.width) + "\n\n " + stMuted.Render("reading the cluster…")
	case d.screen == screenNode:
		if n, ok := findNode(d.snap, d.node); ok {
			body = nodeDetail(d.snap, n, d.context, d.width, d.history[n.name])
		} else {
			body = stMuted.Render(" node " + d.node + " is gone")
		}
	case d.screen == screenPods:
		top := podsView(cluster{taken: d.snap.taken, nodes: d.snap.nodes}, d.context, d.width, -1)
		used := lipgloss.Height(top) - 1 // podsView without pods ends with one "no pods" line
		lo, hi := window(d.cursor, len(d.snap.pods), d.height-used-tableChrome)
		snap := d.snap
		snap.pods = d.snap.pods[lo:hi]
		body = podsView(snap, d.context, d.width, d.cursor-lo)
		if lo > 0 || hi < len(d.snap.pods) {
			body += "\n" + stMuted.Render(fmt.Sprintf(" rows %d to %d of %d", lo+1, hi, len(d.snap.pods)))
		}
	default:
		nodes := d.sorted()
		used := 1 + lipgloss.Height(summaryPanel(d.snap, 0, d.total))
		lo, hi := window(d.cursor, len(nodes), d.height-used-tableChrome)
		body = statusScreen(d.snap, nodes[lo:hi], d.context, d.width, d.cursor-lo, d.history, d.total)
		if lo > 0 || hi < len(nodes) {
			body += "\n" + stMuted.Render(fmt.Sprintf(" rows %d to %d of %d", lo+1, hi, len(nodes)))
		}
	}
	v := tea.NewView(body + "\n" + d.footer())
	v.AltScreen = true
	v.WindowTitle = "kubectl joulie"
	return v
}

// environ is the process environment; a variable so tests can pin it.
var environ = os.Environ
