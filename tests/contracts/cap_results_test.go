package contracts_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// TestCapResultsTheAgentWritesAreKnown: every GPU cap result the agent
// writes to NodeTwin status.controlStatus.gpu.result, a literal passed to
// updateTelemetryGPUStatus or returned by applyGPUIntentFrom, is one of
// pkg/api.CapResultValues. The twin reads blocked and error as a cap that
// did not apply (twin.GPUCapApplies); a result spelled another way would
// read as one that did.
func TestCapResultsTheAgentWritesAreKnown(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), "cmd", "agent", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]string{} // result -> first position
	record := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		if _, seen := found[v]; !seen {
			found[v] = fset.Position(lit.Pos()).String()
		}
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// updateTelemetryGPUStatus(ctx, dyn, cfg, node, backend, result, message, observed)
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "updateTelemetryGPUStatus" && len(n.Args) > 5 {
					record(n.Args[5])
				}
			case *ast.FuncDecl:
				if n.Name.Name != "applyGPUIntentFrom" || n.Body == nil {
					return true
				}
				// It returns (backend, result, message, observed, err), the
				// result a literal or a variable named result.
				ast.Inspect(n.Body, func(m ast.Node) bool {
					switch m := m.(type) {
					case *ast.ReturnStmt:
						if len(m.Results) == 5 {
							record(m.Results[1])
						}
					case *ast.AssignStmt:
						for i, lhs := range m.Lhs {
							if id, ok := lhs.(*ast.Ident); ok && id.Name == "result" && len(m.Rhs) == len(m.Lhs) {
								record(m.Rhs[i])
							}
						}
					}
					return true
				})
			}
			return true
		})
	}
	for _, want := range []string{joulie.CapResultBlocked, joulie.CapResultError, joulie.CapResultApplied} {
		if _, ok := found[want]; !ok {
			t.Errorf("found no GPU cap result %q in cmd/agent; the scan no longer sees the agent's writes (found %v)", want, found)
		}
	}
	for v, pos := range found {
		if !slices.Contains(joulie.CapResultValues, v) {
			t.Errorf("%s: the agent writes GPU cap result %q, which is not in pkg/api.CapResultValues %v. Fix: add it there and decide in twin.GPUCapApplies whether it means the cap applied.",
				pos, v, joulie.CapResultValues)
		}
	}
}
