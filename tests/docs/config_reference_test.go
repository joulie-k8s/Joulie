package docs

// The configuration reference and the input contract are what an adopter
// configures from, and both drifted before: the reference listed a
// TELEMETRY_CPU_SOURCE value nothing read, two FACILITY_*_TEMPLATE variables
// documented as "planned" for months, and no row for the variables the
// measured inputs read. These tests compare the pages with the code that
// reads the variables and spells the metric names, so a variable added,
// renamed or removed without its docs fails the build.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

const (
	configReferencePage  = "website/content/en/docs/getting-started/05-configuration-reference.md"
	telemetryPage        = "website/content/en/docs/architecture/telemetry.md"
	metricsPage          = "website/content/en/docs/architecture/metrics.md"
	controllerManagerDir = "cmd/controller-manager"

	controllerManagerHeading = "## Controller manager"
	inputMetricsHeading      = "## Input metrics (OpenTelemetry)"
	twinInputMetricsHeading  = "## Twin input metrics"
)

// envVarName is the shape of an environment variable name.
var envVarName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// backticked matches one `code span`.
var backticked = regexp.MustCompile("`([^`]+)`")

// TestControllerManagerEnvVarsAreDocumented checks both directions between
// the variables cmd/controller-manager reads and the tables of the
// configuration reference's "Controller manager" section: every variable
// read has a row, and every row names a variable that is read.
func TestControllerManagerEnvVarsAreDocumented(t *testing.T) {
	reads := controllerManagerEnvReads(t)
	section := readSection(t, configReferencePage, controllerManagerHeading)
	documented := map[string]bool{}
	for _, tb := range markdownTables(section) {
		for _, row := range tb.rows {
			if len(row) == 0 {
				continue
			}
			for _, m := range backticked.FindAllStringSubmatch(row[0], -1) {
				if envVarName.MatchString(m[1]) {
					documented[m[1]] = true
				}
			}
		}
	}

	for _, name := range sortedKeys(reads) {
		if !documented[name] {
			t.Errorf("%s reads %s (%s), but no table row under %q in %s names it. Fix: add | `%s` | <code default> | ... | to the table of that component.",
				controllerManagerDir, name, reads[name], controllerManagerHeading, configReferencePage, name)
		}
	}
	for _, name := range sortedKeys(documented) {
		if _, ok := reads[name]; !ok {
			t.Errorf("%s documents %s under %q, but %s does not read it. Fix: remove the row, and add a release note when it was a user-visible setting.",
				configReferencePage, name, controllerManagerHeading, controllerManagerDir)
		}
	}
}

// TestDeprecatedEnvAliasesAreDocumented checks every alias in
// cmd/controller-manager/deprecated.go: the old name is named in the
// configuration reference on a line that states the release removing it, the
// new name has a row, and the old name has no row of its own.
func TestDeprecatedEnvAliasesAreDocumented(t *testing.T) {
	file := parseGoFile(t, filepath.Join(controllerManagerDir, "deprecated.go"))
	maps := stringMapVars(file)
	consts := stringConsts(file)

	// Each alias map with the constant naming the release that removes it.
	groups := map[string]string{
		"legacyEnvAliases": "legacyEnvAliasesRemovedIn",
		"inputEnvAliases":  "inputAliasesRemovedIn",
	}
	for name := range maps {
		if strings.HasSuffix(name, "EnvAliases") {
			if _, ok := groups[name]; !ok {
				t.Errorf("deprecated.go declares the alias map %s, which this test does not know. Fix: add it to groups with the constant of its removal release.", name)
			}
		}
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot, configReferencePage))
	if err != nil {
		t.Fatalf("read %s: %v", configReferencePage, err)
	}
	lines := strings.Split(string(raw), "\n")
	section := readSection(t, configReferencePage, controllerManagerHeading)
	rowNames := map[string]bool{}
	for _, tb := range markdownTables(section) {
		for _, row := range tb.rows {
			if len(row) > 0 {
				for _, m := range backticked.FindAllStringSubmatch(row[0], -1) {
					rowNames[m[1]] = true
				}
			}
		}
	}

	for _, group := range sortedKeys(groups) {
		aliases, ok := maps[group]
		if !ok || len(aliases) == 0 {
			t.Errorf("deprecated.go has no map literal %s; update this test if the aliases moved", group)
			continue
		}
		removedIn, ok := consts[groups[group]]
		if !ok || removedIn == "" {
			t.Errorf("deprecated.go has no string constant %s naming when %s goes", groups[group], group)
			continue
		}
		for _, cur := range sortedKeys(aliases) {
			old := aliases[cur]
			if !rowNames[cur] {
				t.Errorf("%s: %s, which replaces the deprecated %s, has no table row under %q", configReferencePage, cur, old, controllerManagerHeading)
			}
			if rowNames[old] {
				t.Errorf("%s: the deprecated %s has a table row of its own; list it on the aliases line of %s instead", configReferencePage, old, cur)
			}
			found := false
			for _, l := range lines {
				if strings.Contains(l, "`"+old+"`") && strings.Contains(l, removedIn) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s names the deprecated %s (alias of %s, %s) on no line that says it is removed in %q. Fix: list it on a \"Deprecated aliases, removed in %s: ...\" line.",
					configReferencePage, old, cur, group, removedIn, removedIn)
			}
		}
	}
}

// TestInputMetricNamesDocumented checks the input contract on the telemetry
// page against pkg/otel/semconv and pkg/otel/promname, and the controller
// manager's own input metrics against the metrics page:
//   - every metric of semconv.NodeInputs and semconv.FacilityInputs has a
//     contract table row with its default Prometheus name;
//   - the strategy table spells every node input metric and the key labels
//     the way promname does under each of the four strategies;
//   - every metric input_metrics.go registers is named under "Twin input
//     metrics".
func TestInputMetricNamesDocumented(t *testing.T) {
	section := readSection(t, telemetryPage, inputMetricsHeading)
	tables := markdownTables(section)

	var contract []markdownTable
	for _, tb := range tables {
		if tb.column("OTel name") == 0 && tb.columnContaining("Prometheus name") > 0 {
			contract = append(contract, tb)
		}
	}
	if len(contract) == 0 {
		t.Fatalf("%s: no table under %q with the columns OTel name and Prometheus name", telemetryPage, inputMetricsHeading)
	}
	inputMetrics := append(semconv.NodeInputs(), semconv.FacilityInputs()...)
	for _, m := range inputMetrics {
		want := promname.Default.MetricName(m)
		found := false
		for _, tb := range contract {
			prom := tb.columnContaining("Prometheus name")
			for _, row := range tb.rows {
				if cell(row, 0) == "`"+m.Name+"`" {
					found = true
					if got := cell(row, prom); got != "`"+want+"`" {
						t.Errorf("%s: the contract row of %s gives the Prometheus name %s, want `%s`", telemetryPage, m.Name, got, want)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: input metric %s (%s, %s) has no contract table row under %q. Fix: add | `%s` | %s | ... | `%s` | ... |",
				telemetryPage, m.Name, m.Kind, m.Unit, inputMetricsHeading, m.Name, m.Unit, want)
		}
	}

	var strategyTable *markdownTable
	for i := range tables {
		all := true
		for _, s := range promname.Strategies() {
			if tables[i].columnContaining(string(s)) < 0 {
				all = false
			}
		}
		if all {
			strategyTable = &tables[i]
			break
		}
	}
	if strategyTable == nil {
		t.Fatalf("%s: no table under %q with a column for each of the strategies %v", telemetryPage, inputMetricsHeading, promname.Strategies())
	}
	type spelled struct {
		first string
		spell func(promname.Strategy) string
	}
	var want []spelled
	for _, m := range semconv.NodeInputs() {
		m := m
		want = append(want, spelled{"`" + m.Name + "`", func(s promname.Strategy) string { return s.MetricName(m) }})
	}
	for _, a := range []string{semconv.AttrK8sNodeName, semconv.AttrHwType, semconv.AttrHwID, semconv.AttrJoulieCoolingZoneName, semconv.AttrJoulieRackName} {
		a := a
		want = append(want, spelled{"label `" + a + "`", func(s promname.Strategy) string { return s.LabelName(a) }})
	}
	for _, w := range want {
		var row []string
		for _, r := range strategyTable.rows {
			if cell(r, 0) == w.first {
				row = r
			}
		}
		if row == nil {
			t.Errorf("%s: the strategy table has no row %s", telemetryPage, w.first)
			continue
		}
		for _, s := range promname.Strategies() {
			if got, exp := cell(row, strategyTable.columnContaining(string(s))), "`"+w.spell(s)+"`"; got != exp {
				t.Errorf("%s: strategy table row %s, column %s: got %s, want %s", telemetryPage, w.first, s, got, exp)
			}
		}
	}

	twin := readSection(t, metricsPage, twinInputMetricsHeading)
	names := registeredMetricNames(t, filepath.Join(controllerManagerDir, "input_metrics.go"))
	if len(names) == 0 {
		t.Fatalf("%s/input_metrics.go registers no metric; update this test if they moved", controllerManagerDir)
	}
	for _, n := range names {
		if !strings.Contains(twin, "`"+n+"{") && !strings.Contains(twin, "`"+n+"`") {
			t.Errorf("%s: %s, registered in %s/input_metrics.go, is not named under %q", metricsPage, n, controllerManagerDir, twinInputMetricsHeading)
		}
	}
}

// controllerManagerEnvReads returns every environment variable the
// controller manager's non-test Go files read, each with the file and line
// of one read: string literals passed first to a function whose name
// contains "env" (envOrDefault, envOrDeprecated, boolEnv, os.Getenv ...),
// and FACILITY_<INPUT>_QUERY for every facility input, which
// facilityQueryEnv builds from inputs.FacilityInputs.
func controllerManagerEnvReads(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot, controllerManagerDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var fn string
			switch c := call.Fun.(type) {
			case *ast.Ident:
				fn = c.Name
			case *ast.SelectorExpr:
				fn = c.Sel.Name
			}
			if !strings.Contains(strings.ToLower(fn), "env") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil || !envVarName.MatchString(name) {
				return true
			}
			if _, seen := out[name]; !seen {
				pos := fset.Position(lit.Pos())
				out[name] = filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no environment variable read in %s; the parser no longer matches the code", controllerManagerDir)
	}
	for _, in := range inputs.FacilityInputs() {
		name := "FACILITY_" + strings.ToUpper(string(in)) + "_QUERY"
		if _, seen := out[name]; !seen {
			out[name] = "inputs_config.go facilityQueryEnv"
		}
	}
	return out
}

func parseGoFile(t *testing.T, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return f
}

// stringMapVars returns the package-level variables of f initialised with a
// map literal of string keys and values.
func stringMapVars(f *ast.File) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				cl, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				m := map[string]string{}
				for _, e := range cl.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, kok := stringLit(kv.Key)
					v, vok := stringLit(kv.Value)
					if kok && vok {
						m[k] = v
					}
				}
				if len(m) > 0 {
					out[name.Name] = m
				}
			}
		}
	}
	return out
}

// stringConsts returns the string constants of f.
func stringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if v, ok := stringLit(vs.Values[i]); ok {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// registeredMetricNames returns the Name fields of the composite literals in
// a Go file, such as prometheus.CounterOpts{Name: "..."}.
func registeredMetricNames(t *testing.T, rel string) []string {
	t.Helper()
	var out []string
	ast.Inspect(parseGoFile(t, rel), func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Name" {
			if v, ok := stringLit(kv.Value); ok {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// readSection returns the part of a page from the line equal to heading up
// to the next heading of the same or a higher level.
func readSection(t *testing.T, page, heading string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, page))
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	level := strings.Index(heading, " ")
	var out []string
	in := false
	fence := false
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(l, "#") {
			hashes := len(l) - len(strings.TrimLeft(l, "#"))
			if in && hashes <= level && strings.HasPrefix(l[hashes:], " ") {
				break
			}
			if strings.TrimRight(l, " ") == heading {
				in = true
			}
		}
		if in {
			out = append(out, l)
		}
	}
	if !in {
		t.Fatalf("%s has no heading %q", page, heading)
	}
	return strings.Join(out, "\n")
}

// markdownTable is one pipe table: its header cells and its rows, without
// the separator line.
type markdownTable struct {
	header []string
	rows   [][]string
}

// column returns the index of the header cell equal to name, or -1.
func (tb markdownTable) column(name string) int {
	for i, h := range tb.header {
		if h == name {
			return i
		}
	}
	return -1
}

// columnContaining returns the index of the first header cell containing s,
// or -1.
func (tb markdownTable) columnContaining(s string) int {
	for i, h := range tb.header {
		if strings.Contains(h, s) {
			return i
		}
	}
	return -1
}

// markdownTables returns the pipe tables of text, outside code fences.
func markdownTables(text string) []markdownTable {
	var out []markdownTable
	var cur *markdownTable
	fence := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "```") {
			fence = !fence
		}
		line := strings.TrimSpace(l)
		if fence || !strings.HasPrefix(line, "|") {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			continue
		}
		cells := splitRow(line)
		switch {
		case cur == nil:
			cur = &markdownTable{header: cells}
		case len(cur.rows) == 0 && isSeparator(cells):
		default:
			cur.rows = append(cur.rows, cells)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// splitRow splits a table row on the pipes that are not escaped (\|), as
// GitHub Flavored Markdown does: a pipe inside a code span splits the cell
// too unless it is escaped.
func splitRow(line string) []string {
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	var cells []string
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line) && line[i+1] == '|':
			b.WriteByte('|')
			i++
		case c == '|':
			cells = append(cells, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(cells, strings.TrimSpace(b.String()))
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return row[i]
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
