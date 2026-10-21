package main

import (
	"fmt"
	"image/color"
	"math"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// The palette. Colors are true color; the writer downsamples them to what
// the terminal supports and strips them when the output is not a terminal.
var (
	colAccent   = lipgloss.Color("#FFC531")
	colPerf     = lipgloss.Color("#FF8A4C")
	colEco      = lipgloss.Color("#3DD68C")
	colDraining = lipgloss.Color("#F7C948")
	colUnknown  = lipgloss.Color("#8A8F98")
	colText     = lipgloss.Color("#E6E9EF")
	colMuted    = lipgloss.Color("#8C92A4")
	colFaint    = lipgloss.Color("#4A4F5E")
	colBorder   = lipgloss.Color("#5B6078")
	colGood     = lipgloss.Color("#3DD68C")
	colWarn     = lipgloss.Color("#F7C948")
	colBad      = lipgloss.Color("#FF5C6C")
	colCPU      = lipgloss.Color("#B79CFF")
	colGPU      = lipgloss.Color("#6CB6FF")
	colSelected = lipgloss.Color("#3B4A7A")
)

var (
	stTitle  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stText   = lipgloss.NewStyle().Foreground(colText)
	stBold   = lipgloss.NewStyle().Bold(true).Foreground(colText)
	stMuted  = lipgloss.NewStyle().Foreground(colMuted)
	stFaint  = lipgloss.NewStyle().Foreground(colFaint)
	stLabel  = lipgloss.NewStyle().Bold(true).Foreground(colMuted)
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(colAccent).Padding(0, 1)
	stCell   = lipgloss.NewStyle().Foreground(colText).Padding(0, 1)
	stBorder = lipgloss.NewStyle().Foreground(colBorder)
	stGood   = lipgloss.NewStyle().Foreground(colGood)
	stWarn   = lipgloss.NewStyle().Foreground(colWarn)
	stBad    = lipgloss.NewStyle().Bold(true).Foreground(colBad)
	stCPU    = lipgloss.NewStyle().Foreground(colCPU)
	stGPU    = lipgloss.NewStyle().Foreground(colGPU)
)

// classColor is the color of a schedulable class.
func classColor(class string) color.Color {
	switch class {
	case classPerformance:
		return colPerf
	case classEco:
		return colEco
	case classDraining:
		return colDraining
	}
	return colUnknown
}

// classBadge is a colored dot and the class name.
func classBadge(class string) string {
	return lipgloss.NewStyle().Foreground(classColor(class)).Render("● " + class)
}

// podClassBadge marks performance pods, which only performance nodes take.
func podClassBadge(class string) string {
	if class == podClassPerformance {
		return lipgloss.NewStyle().Foreground(colPerf).Render("▲ performance")
	}
	return stMuted.Render("· standard")
}

// levelColor grades a load-like value: green when low, amber, then red.
func levelColor(pct float64) color.Color {
	switch {
	case pct >= 85:
		return colBad
	case pct >= 60:
		return colWarn
	}
	return colGood
}

// bar is a rich-style progress bar of width cells for pct in 0..100: heavy
// line segments in the given color, half a segment for the rounding, and
// the rest of the track faint.
func bar(pct float64, width int, c color.Color) string {
	if width <= 0 {
		return ""
	}
	pct = clamp(pct, 0, 100)
	cells := pct / 100 * float64(width)
	full := int(cells)
	half := cells-float64(full) >= 0.5 && full < width
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("━", full)))
	rest := width - full
	if half {
		b.WriteString(lipgloss.NewStyle().Foreground(c).Render("╸"))
		rest--
	}
	b.WriteString(stFaint.Render(strings.Repeat("━", rest)))
	return b.String()
}

// sparkline draws the last width vals as block characters, each the share
// of ref it reaches and colored like the power bar, so a full red block is
// a reading at the cap. With no ref the series' own maximum is the
// reference.
func sparkline(vals []float64, width int, ref float64) string {
	if len(vals) == 0 || width <= 0 {
		return strings.Repeat(" ", max(width, 0))
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	if ref <= 0 {
		for _, v := range vals {
			ref = math.Max(ref, v)
		}
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		share := 0.0
		if ref > 0 {
			share = clamp(v/ref, 0, 1)
		}
		block := string(blocks[int(math.Round(share*float64(len(blocks)-1)))])
		b.WriteString(lipgloss.NewStyle().Foreground(levelColor(share * 100)).Render(block))
	}
	return b.String()
}

// watts formats a power in W, kW or MW with three significant digits.
func watts(w float64) string {
	switch a := math.Abs(w); {
	case a >= 1e6:
		return fmt.Sprintf("%.2f MW", w/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.1f kW", w/1e3)
	case a >= 1e3:
		return fmt.Sprintf("%.2f kW", w/1e3)
	}
	return fmt.Sprintf("%.0f W", w)
}

// trend is the power trend as an arrow and W/min, colored by direction.
func trend(wPerMin float64) string {
	switch {
	case wPerMin > 0.5:
		return stWarn.Render(fmt.Sprintf("↗ +%.0f", wPerMin))
	case wPerMin < -0.5:
		return stGood.Render(fmt.Sprintf("↘ %.0f", wPerMin))
	}
	return stMuted.Render("→ 0")
}

// percent formats 0..100 without decimals.
func percent(v float64) string { return fmt.Sprintf("%.0f%%", v) }

// ago is a short age: 12s, 4m, 3h, 2d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// shortModel turns a catalog model (INTEL_XEON_GOLD_6530) or a raw one
// (Intel(R) Xeon(R) Gold 6530 CPU @ 2.10GHz) into a short label: no vendor,
// no marks, no clock, memory size or form factor, product words in their
// usual case.
func shortModel(m string) string {
	m = strings.NewReplacer("_", " ", "(R)", "", "(TM)", "", "(tm)", "", "®", "", "™", "").Replace(m)
	if i := strings.Index(m, " @ "); i >= 0 {
		m = m[:i]
	}
	var words []string
	for _, w := range strings.Fields(m) {
		lw := strings.ToLower(w)
		switch {
		case lw == "intel" || lw == "amd" || lw == "nvidia" || lw == "processor" || lw == "cpu" || lw == "oam" || lw == "pcie":
			continue
		case noiseWord.MatchString(lw):
			continue
		}
		if c, ok := productCase[lw]; ok {
			w = c
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}

// noiseWord matches the words shortModel drops: core counts, memory sizes
// and memory kinds.
var noiseWord = regexp.MustCompile(`^(\d+-core|\d+gb|hbm\d*e?)$`)

// productCase spells product words that catalogs and firmware upper-case.
var productCase = map[string]string{
	"xeon": "Xeon", "gold": "Gold", "platinum": "Platinum", "silver": "Silver", "bronze": "Bronze",
	"instinct": "Instinct", "radeon": "Radeon", "pro": "Pro", "flex": "Flex", "max": "Max", "data": "Data", "center": "Center",
}

// hardwareCell is what sets the node apart: its GPUs when it has some,
// "8× H100 SXM", else its CPUs, "2× Xeon Gold 6530".
func hardwareCell(n nodeView) string {
	switch {
	case !n.hasHW:
		return stFaint.Render("no NodeHardware")
	case n.gpus > 0:
		return stGPU.Render(fmt.Sprintf("%d× %s", n.gpus, shortModel(n.gpuModel)))
	case n.cpuModel != "":
		s := shortModel(n.cpuModel)
		if n.sockets > 1 {
			s = fmt.Sprintf("%d× %s", n.sockets, s)
		}
		return stCPU.Render(s)
	}
	return stFaint.Render("-")
}

// agentCell is the AgentReporting condition: a check, a cross with the
// reason, or a question mark when the controller manager wrote none.
func agentCell(a agentState) string {
	switch {
	case !a.known:
		return stMuted.Render("?")
	case a.reporting:
		return stGood.Render("✓")
	}
	return stBad.Render("✗ " + a.reason)
}

// capCell shows the effective CPU and GPU caps, "100% · 60%", with a dash
// for a node without GPUs and caps below 100% in the eco color.
func capCell(n nodeView) string {
	style := func(v float64) string {
		if v < 100 {
			return stGood.Render(percent(v))
		}
		return stText.Render(percent(v))
	}
	if !n.capKnown {
		return stFaint.Render("- · -")
	}
	gpu := stFaint.Render("-")
	if n.hasGPU || n.gpuCapPlanned {
		gpu = style(n.gpuCapPct)
	}
	return style(n.cpuCapPct) + stFaint.Render(" · ") + gpu
}

// allocCell is a requested-over-allocatable percentage in the resource's
// color, faint when nothing is requested, or a dash when the node has none
// of the resource. Full is not an alarm: packing work is what saves energy.
func allocCell(v float64, has bool, st lipgloss.Style) string {
	switch {
	case !has:
		return stFaint.Render("-")
	case v < 0.5:
		return stFaint.Render("0%")
	}
	return st.Render(percent(v))
}

// powerCell is the measured power and a bar of it against the capped power
// (the TDP when no cap is known), or the power source when nothing measures.
func powerCell(n nodeView, width int) string {
	if n.measuredW <= 0 {
		src := n.powerSource
		if src == "" {
			src = "no reading"
		}
		return stFaint.Render(src)
	}
	ref := n.cappedW
	if ref <= 0 {
		ref = n.tdpW
	}
	frac := pct(n.measuredW, ref)
	return fmt.Sprintf("%8s ", watts(n.measuredW)) + bar(frac, width, levelColor(frac))
}

// gaugeCell is a short bar and its value; invert grades high values green.
func gaugeCell(v float64, width int, invert bool) string {
	level := v
	if invert {
		level = 100 - v
	}
	c := levelColor(level)
	return bar(v, width, c) + " " + lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf("%3.0f%%", v))
}

// nodeColumn is one column of the node table.
type nodeColumn struct {
	header   string
	optional int // the widest table drops the highest first; 0 never drops
	right    bool
	cell     func(n nodeView, hist []float64) string
}

func nodeColumns() []nodeColumn {
	return []nodeColumn{
		{header: "NODE", cell: func(n nodeView, _ []float64) string { return stBold.Render(n.name) }},
		{header: "HARDWARE", optional: 3, cell: func(n nodeView, _ []float64) string { return hardwareCell(n) }},
		{header: "CLASS", cell: func(n nodeView, _ []float64) string { return classBadge(n.class) }},
		{header: "POWER", cell: func(n nodeView, _ []float64) string { return powerCell(n, 10) }},
		{header: "TREND", optional: 4, right: true, cell: func(n nodeView, hist []float64) string {
			if len(hist) > 1 {
				return sparkline(hist, 8, firstPositive(n.cappedW, n.tdpW))
			}
			return trend(n.trendWMin)
		}},
		{header: "CAP CPU·GPU", right: true, cell: func(n nodeView, _ []float64) string { return capCell(n) }},
		{header: "HEADROOM", optional: 1, cell: func(n nodeView, _ []float64) string { return gaugeCell(n.headroom, 6, true) }},
		{header: "COOLING", optional: 2, cell: func(n nodeView, _ []float64) string { return gaugeCell(n.cooling, 6, false) }},
		{header: "CPU", right: true, cell: func(n nodeView, _ []float64) string { return allocCell(n.cpuAllocPct, true, stCPU) }},
		{header: "MEM", optional: 5, right: true, cell: func(n nodeView, _ []float64) string { return allocCell(n.memAllocPct, true, stText) }},
		{header: "GPU", right: true, cell: func(n nodeView, _ []float64) string { return allocCell(n.gpuAllocPct, n.hasGPU, stGPU) }},
		{header: "PODS", right: true, cell: func(n nodeView, _ []float64) string {
			if n.perfPods > 0 {
				return fmt.Sprintf("%d", n.pods) + lipgloss.NewStyle().Foreground(colPerf).Render(fmt.Sprintf(" ▲%d", n.perfPods))
			}
			return fmt.Sprintf("%d", n.pods)
		}},
		{header: "AGENT", cell: func(n nodeView, _ []float64) string { return agentCell(n.agent) }},
	}
}

// nodeTable renders the nodes as a rounded table no wider than width,
// dropping optional columns until it fits. selected is the highlighted row,
// -1 for none; history holds each node's recent measured power.
func nodeTable(nodes []nodeView, width, selected int, history map[string][]float64) string {
	cols := nodeColumns()
	for {
		out := renderNodeTable(nodes, cols, selected, history)
		if width <= 0 || lipgloss.Width(out) <= width {
			return out
		}
		drop, at := 0, -1
		for i, c := range cols {
			if c.optional > drop {
				drop, at = c.optional, i
			}
		}
		if at < 0 {
			return out
		}
		cols = append(cols[:at:at], cols[at+1:]...)
	}
}

func renderNodeTable(nodes []nodeView, cols []nodeColumn, selected int, history map[string][]float64) string {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	rows := make([][]string, len(nodes))
	for r, n := range nodes {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = c.cell(n, history[n.name])
		}
		rows[r] = row
	}
	highlight(rows, selected)
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(stBorder).
		BorderColumn(false).
		BorderRow(false).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				s := stHeader
				if cols[col].right {
					s = s.Align(lipgloss.Right)
				}
				return s
			}
			s := stCell
			if cols[col].right {
				s = s.Align(lipgloss.Right)
			}
			if selected >= 0 && row == selected {
				s = s.Inherit(stSelected)
			}
			return s
		})
	return withTitle(t.String(), "Nodes")
}

// stSelected is the selected row: one background across the row, which
// the cells' own colors would break, so highlight drops them.
var stSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(colSelected)

// highlight strips the colors of the selected row, for stSelected.
func highlight(rows [][]string, selected int) {
	if selected < 0 || selected >= len(rows) {
		return
	}
	for i, cell := range rows[selected] {
		rows[selected][i] = stripANSI(cell)
	}
}

// firstPositive is the first value above zero, else 0.
func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// withTitle writes title into the top border of a rendered box:
// ╭─ Title ─────╮.
func withTitle(box, title string) string {
	lines := strings.SplitN(box, "\n", 2)
	if len(lines) < 2 {
		return box
	}
	top := []rune(stripANSI(lines[0]))
	label := " " + title + " "
	n := len([]rune(label))
	if len(top) < n+4 {
		return box
	}
	return stBorder.Render(string(top[:2])) + stTitle.Render(label) + stBorder.Render(string(top[2+n:])) + "\n" + lines[1]
}

// header is the title line: the name on the left, the context and the time
// on the right.
func header(title, context string, at time.Time, width int) string {
	left := stTitle.Render("⚡ JOULIE") + "  " + stMuted.Render(title)
	right := stMuted.Render(strings.TrimSpace(context + "  " + at.Format("15:04:05")))
	gap := width - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 4 {
		gap = 4
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

// summaryPanel is the cluster summary box above the node table; total is
// the recent cluster power, drawn as a sparkline when there is some.
func summaryPanel(c cluster, width int, total []float64) string {
	t := summarize(c)
	label := func(s string) string { return stLabel.Render(fmt.Sprintf("%-9s", s)) }
	sep := stFaint.Render("  ·  ")
	var lines []string

	barWidth := 32
	if width > 0 && width < 110 {
		barWidth = 18
	}
	ref, refName := t.cappedW, "capped"
	if ref <= 0 {
		ref, refName = t.tdpW, "TDP"
	}
	power := label("POWER") + stBold.Render(fmt.Sprintf("%9s ", watts(t.measuredW)))
	if ref > 0 {
		frac := pct(t.measuredW, ref)
		power += bar(frac, barWidth, levelColor(frac)) + " " + stText.Render(percent(frac)) + stMuted.Render(" of "+watts(ref)+" "+refName)
	}
	power += sep + trend(t.trendWMin) + stMuted.Render(" W/min")
	if len(total) > 1 {
		power += "  " + sparkline(total, 24, ref)
	}
	lines = append(lines, power)

	if t.tdpW > 0 && t.cappedW > 0 {
		held := t.tdpW - t.cappedW
		lines = append(lines, label("BUDGET")+stText.Render(watts(t.cappedW))+stMuted.Render(" capped of ")+
			stText.Render(watts(t.tdpW))+stMuted.Render(" TDP")+sep+stGood.Render("caps hold back "+watts(held)))
	}

	nodes := label("NODES") + stBold.Render(fmt.Sprintf("%d", t.nodes)) + "  "
	for _, k := range []struct {
		n     int
		class string
	}{{t.performance, classPerformance}, {t.eco, classEco}, {t.draining, classDraining}, {t.unknown, classUnknown}} {
		if k.n > 0 {
			nodes += lipgloss.NewStyle().Foreground(classColor(k.class)).Render(fmt.Sprintf("● %d %s", k.n, k.class))
			if w := t.classW[k.class]; w > 0 {
				nodes += stMuted.Render(" " + watts(w))
			}
			nodes += "  "
		}
	}
	agents := t.agentsReporting + t.agentsMissing
	if agents > 0 {
		a := fmt.Sprintf("agents %d/%d reporting", t.agentsReporting, agents)
		if t.agentsMissing > 0 {
			nodes += sep + stWarn.Render(a)
		} else {
			nodes += sep + stMuted.Render(a)
		}
	}
	lines = append(lines, nodes)

	pods := label("PODS") + stBold.Render(fmt.Sprintf("%d", t.pods)) + "  " +
		lipgloss.NewStyle().Foreground(colPerf).Render(fmt.Sprintf("▲ %d performance", t.perfPods)) +
		stMuted.Render(fmt.Sprintf("  · %d standard", t.pods-t.perfPods))
	switch {
	case c.podsErr != nil:
		pods = label("PODS") + stWarn.Render("pods unreadable: "+c.podsErr.Error())
	case t.misplaced > 0:
		pods += sep + stBad.Render(fmt.Sprintf("%d performance pods on eco nodes", t.misplaced))
	case t.perfPods > 0:
		pods += sep + stGood.Render("✓ every performance pod on a performance node")
	}
	lines = append(lines, pods)

	fac := label("FACILITY") + stMuted.Render("headroom ") + stText.Render(percent(t.headroom)) + sep +
		stMuted.Render("cooling ") + lipgloss.NewStyle().Foreground(levelColor(t.cooling)).Render(percent(t.cooling))
	if t.peakCoolingNode != "" {
		fac += stMuted.Render(fmt.Sprintf(" (peak %s %s)", percent(t.peakCooling), t.peakCoolingNode))
	}
	fac += sep + stMuted.Render("PSU ") + stText.Render(percent(t.psu))
	if t.pue > 0 {
		fac += sep + stMuted.Render("PUE ") + stText.Render(fmt.Sprintf("%.2f", t.pue))
	}
	lines = append(lines, fac)

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colBorder).Padding(0, 1)
	body := strings.Join(lines, "\n")
	if width > lipgloss.Width(box.Render(body)) {
		box = box.Width(width)
	}
	return withTitle(box.Render(body), "Cluster")
}

// statusView is the whole status screen: the title, the summary and the
// node table, all as wide as the table.
func statusView(c cluster, context string, width int, selected int, history map[string][]float64, total []float64) string {
	if len(c.nodes) == 0 {
		return header("cluster energy", context, c.taken, width) + "\n\n" +
			stMuted.Render("  No NodeTwin yet: the controller manager writes one per managed node (label joulie.io/managed=true).") + "\n"
	}
	return statusScreen(c, c.nodes, context, width, selected, history, total)
}

// statusScreen is statusView for a subset of the nodes, the rows the live
// dashboard has room for.
func statusScreen(c cluster, nodes []nodeView, context string, width int, selected int, history map[string][]float64, total []float64) string {
	tbl := nodeTable(nodes, width, selected, history)
	w := lipgloss.Width(tbl)
	return strings.Join([]string{
		header("cluster energy", context, c.taken, w),
		summaryPanel(c, w, total),
		tbl,
	}, "\n")
}
