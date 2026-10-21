package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	v1alpha1 "github.com/matbun/joulie/api/v1alpha1"
)

// findNode returns the node named name, or false.
func findNode(c cluster, name string) (nodeView, bool) {
	for _, n := range c.nodes {
		if n.name == name {
			return n, true
		}
	}
	return nodeView{}, false
}

// panel is a rounded box with a title in its top border.
func panel(title, body string, width int) string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colBorder).Padding(0, 1)
	if width > 0 {
		box = box.Width(width)
	}
	return withTitle(box.Render(body), title)
}

// pair puts two panels side by side, splitting width and giving both the
// height of the taller.
func pair(t1, b1, t2, b2 string, width int) string {
	half := (width - 1) / 2
	h := max(lipgloss.Height(b1), lipgloss.Height(b2))
	pad := func(b string) string { return b + strings.Repeat("\n", h-lipgloss.Height(b)) }
	return lipgloss.JoinHorizontal(lipgloss.Top, panel(t1, fit(pad(b1), half-4), half), " ", panel(t2, fit(pad(b2), width-half-5), width-half-1))
}

// fit cuts every line of s to width cells, with an ellipsis, so a long
// agent message stays on its line instead of wrapping to the panel edge.
func fit(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}

// stripANSI drops the escape sequences of s.
func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			esc = true
		case esc && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			esc = false
		case !esc:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// kv renders label value lines with the labels aligned.
func kv(rows [][2]string) string {
	w := 0
	for _, r := range rows {
		w = max(w, lipgloss.Width(r[0]))
	}
	var lines []string
	indent := strings.Repeat(" ", w+2)
	for _, r := range rows {
		value := strings.ReplaceAll(r[1], "\n", "\n"+indent)
		lines = append(lines, stLabel.Render(fmt.Sprintf("%-*s", w, r[0]))+"  "+value)
	}
	return strings.Join(lines, "\n")
}

// controlLine is one agent control result: a check and the backend when
// applied, a warning with the agent's message otherwise.
func controlLine(r *v1alpha1.ControlResult, planned string) string {
	if r == nil {
		return stMuted.Render(planned + "  ·  no report from the agent")
	}
	mark := stGood.Render("✓ " + r.Result)
	switch r.Result {
	case "applied", "equivalent", "restored":
	case "none", "disabled", "unsupported":
		mark = stMuted.Render("· " + r.Result)
	default:
		mark = stBad.Render("✗ " + r.Result)
	}
	s := stText.Render(planned) + "  " + mark
	if r.Backend != "" {
		s += stMuted.Render(" via " + r.Backend)
	}
	if r.Applied != "" {
		s += stMuted.Render(" (" + r.Applied + ")")
	}
	if r.Message != "" {
		s += "\n" + stMuted.Render(r.Message)
	}
	return s
}

// nodeDetail is the screen of one node: its hardware, power, twin scores,
// controls and pods.
func nodeDetail(c cluster, n nodeView, context string, width int, history []float64) string {
	if width <= 0 {
		width = 120
	}

	// Hardware.
	var hw [][2]string
	if n.hw != nil {
		st := n.hw.Status
		if cpu := st.CPU; cpu != nil {
			s := stCPU.Render(fmt.Sprintf("%d× %s", max(cpu.Sockets, 1), shortModel(firstNonEmpty(cpu.Model, cpu.RawModel))))
			if cpu.TotalCores > 0 {
				s += stMuted.Render(fmt.Sprintf("  %d threads", cpu.TotalCores))
			}
			hw = append(hw, [2]string{"CPU", s})
			if r := cpu.CapRange; r != nil && r.MaxWattsPerSocket > 0 {
				hw = append(hw, [2]string{"", stMuted.Render(fmt.Sprintf("cap %s to %s per socket (%s)", watts(r.MinWattsPerSocket), watts(r.MaxWattsPerSocket), r.Type))})
			} else if !cpu.ControlAvailable {
				hw = append(hw, [2]string{"", stMuted.Render("no CPU power cap control")})
			}
		}
		if gpu := st.GPU; gpu != nil && gpu.Present {
			hw = append(hw, [2]string{"GPU", stGPU.Render(fmt.Sprintf("%d× %s", gpu.Count, shortModel(firstNonEmpty(gpu.Model, gpu.RawModel))))})
			if r := gpu.CapRangePerGPU; r != nil && r.MaxWatts > 0 {
				hw = append(hw, [2]string{"", stMuted.Render(fmt.Sprintf("cap %s to %s per GPU", watts(r.MinWatts), watts(r.MaxWatts)))})
			}
		}
		if mem := st.Memory; mem != nil && mem.TotalBytes > 0 {
			hw = append(hw, [2]string{"MEMORY", stText.Render(fmt.Sprintf("%.0f GiB", float64(mem.TotalBytes)/(1<<30)))})
		}
	} else {
		hw = append(hw, [2]string{"", stMuted.Render("no NodeHardware: no agent publishes this node's hardware")})
	}

	// Power.
	ref := n.cappedW
	if ref <= 0 {
		ref = n.tdpW
	}
	frac := pct(n.measuredW, ref)
	power := [][2]string{
		{"MEASURED", stBold.Render(watts(n.measuredW)) + "  " + bar(frac, 16, levelColor(frac)) + " " + stText.Render(percent(frac))},
		{"CAPPED", stText.Render(watts(n.cappedW)) + stMuted.Render(" of "+watts(n.tdpW)+" TDP")},
		{"TREND", trend(n.trendWMin) + stMuted.Render(" W/min")},
		{"SOURCE", stMuted.Render(firstNonEmpty(n.powerSource, "none"))},
	}
	if len(history) > 1 {
		power = append(power, [2]string{"HISTORY", sparkline(history, 30, ref)})
	}

	// Twin.
	scores := [][2]string{
		{"HEADROOM", gaugeCell(n.headroom, 16, true)},
		{"COOLING", gaugeCell(n.cooling, 16, false)},
		{"PSU", gaugeCell(n.psu, 16, false)},
	}
	if n.pue > 0 {
		scores = append(scores, [2]string{"PUE", stText.Render(fmt.Sprintf("%.2f", n.pue))})
	}

	// Control.
	cpuPlan, gpuPlan := "cap -", "cap -"
	if n.capKnown {
		cpuPlan = "cap " + percent(n.cpuCapPct)
		if n.hasGPU || n.gpuCapPlanned {
			gpuPlan = "cap " + percent(n.gpuCapPct)
		}
	}
	ctl := [][2]string{
		{"CPU", controlLine(n.cpuControl, cpuPlan)},
		{"GPU", controlLine(n.gpuControl, gpuPlan)},
	}
	if n.twin != nil {
		if g := n.twin.Spec.CPU; g != nil && g.Governor != nil {
			ctl = append(ctl, [2]string{"CPU GOV", controlLine(n.cpuGovernor, g.Governor.Preset)})
		}
		if g := n.twin.Spec.GPU; g != nil && g.Governor != nil {
			ctl = append(ctl, [2]string{"GPU GOV", controlLine(n.gpuGovernor, g.Governor.Preset)})
		}
	}

	agent := agentCell(n.agent)
	if n.agent.known && n.agent.reporting {
		agent = stGood.Render("✓ agent reporting")
	}
	updated := ""
	if !n.updated.IsZero() {
		updated = stMuted.Render("twin updated " + ago(c.taken.Sub(n.updated)) + " ago")
	}
	title := stBold.Render(n.name) + "  " + classBadge(n.class)
	if n.profile != "" && n.profile != n.class {
		title += stMuted.Render("  (profile " + n.profile + ")")
	}
	title += "  " + agent + "  " + updated

	top := pair("Hardware", kv(hw), "Power", kv(power), width)
	mid := pair("Twin", kv(scores), "Control", kv(ctl), width)

	var pods []podView
	for _, p := range c.pods {
		if p.node == n.name {
			pods = append(pods, p)
		}
	}
	return strings.Join([]string{
		header("node", context, c.taken, width),
		" " + title,
		top,
		mid,
		podTable(pods, c.taken, false, -1),
	}, "\n")
}

// podTable lists pods; withNode adds the node and its class.
func podTable(pods []podView, now time.Time, withNode bool, selected int) string {
	if len(pods) == 0 {
		return stMuted.Render("  no pods")
	}
	headers := []string{"NAMESPACE", "POD", "CLASS"}
	if withNode {
		headers = append(headers, "NODE", "NODE CLASS")
	}
	headers = append(headers, "CPU", "GPU", "AGE")
	right := map[int]bool{len(headers) - 3: true, len(headers) - 2: true, len(headers) - 1: true}
	rows := make([][]string, 0, len(pods))
	for _, p := range pods {
		row := []string{stMuted.Render(p.namespace), stText.Render(p.name), podClassBadge(p.class)}
		if withNode {
			nc := classBadge(p.nodeClass)
			if p.misplaced() {
				nc = stBad.Render("✗ " + p.nodeClass)
			}
			row = append(row, stBold.Render(p.node), nc)
		}
		gpu := stFaint.Render("-")
		if p.gpus > 0 {
			gpu = stGPU.Render(trimFloat(p.gpus))
		}
		age := stFaint.Render("-")
		if !p.started.IsZero() {
			age = stMuted.Render(ago(now.Sub(p.started)))
		}
		row = append(row, stCPU.Render(trimFloat(float64(p.cpuMilli)/1000)), gpu, age)
		rows = append(rows, row)
	}
	highlight(rows, selected)
	tbl := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(stBorder).
		BorderColumn(false).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := stCell
			if row == table.HeaderRow {
				s = stHeader
			}
			if right[col] {
				s = s.Align(lipgloss.Right)
			}
			if selected >= 0 && row == selected {
				s = s.Inherit(stSelected)
			}
			return s
		}).String()
	return withTitle(tbl, fmt.Sprintf("Pods (%d)", len(pods)))
}

// trimFloat prints a count without trailing zeros: 4, 0.5, 1.25.
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// podsView is the placement screen: every pod on a managed node, the
// performance ones first, with a line on what the scheduler kept apart.
func podsView(c cluster, context string, width int, selected int) string {
	t := summarize(c)
	byClass := map[string]map[string]int{}
	for _, p := range c.pods {
		if byClass[p.class] == nil {
			byClass[p.class] = map[string]int{}
		}
		byClass[p.class][p.nodeClass]++
	}
	var parts []string
	for _, pc := range []string{podClassPerformance, podClassStandard} {
		m := byClass[pc]
		if len(m) == 0 {
			continue
		}
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var on []string
		for _, k := range keys {
			on = append(on, lipgloss.NewStyle().Foreground(classColor(k)).Render(fmt.Sprintf("%d on %s", m[k], k)))
		}
		parts = append(parts, podClassBadge(pc)+stMuted.Render(": ")+strings.Join(on, stMuted.Render(", ")))
	}
	verdict := stGood.Render("✓ no performance pod on an eco node")
	switch {
	case c.podsErr != nil:
		verdict = stWarn.Render("pods unreadable: " + c.podsErr.Error())
	case t.misplaced > 0:
		verdict = stBad.Render(fmt.Sprintf("✗ %d performance pods on eco nodes", t.misplaced))
	}
	summary := " " + stBold.Render(fmt.Sprintf("%d pods", t.pods)) + "   " + strings.Join(parts, "   ") + "   " + verdict
	tbl := podTable(c.pods, c.taken, true, selected)
	return strings.Join([]string{
		header("workload placement", context, c.taken, max(lipgloss.Width(tbl), lipgloss.Width(summary))),
		summary,
		tbl,
	}, "\n")
}
