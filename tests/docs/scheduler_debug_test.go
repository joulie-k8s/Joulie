package docs

// The /debug/scoring example on the scheduler page showed coefficients the
// scheduler never had (0.8, 0.6 and 0.9 where DefaultCoefficients sets 0.7,
// 0.65 and 0.85), under keys the endpoint does not write, and a node key
// gpuMaxWattsPerGpu where the endpoint writes gpuMaxWattsPerGPU.

import (
	"encoding/json"
	"go/ast"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/scheduler/powerest"
)

const (
	schedulerPage         = "website/content/en/docs/architecture/scheduler.md"
	schedulerMain         = "cmd/scheduler/main.go"
	debugEndpointHeading  = "## Debug endpoint"
	debugResponseType     = "debugScoringResponse"
	debugNodeResponseType = "debugNodeScoringEntry"
)

var jsonFence = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// TestSchedulerDebugScoringExample checks the page's /debug/scoring example
// against what the endpoint writes: its coefficients are the JSON of
// powerest.DefaultCoefficients, and its keys are the JSON names of the
// response types in cmd/scheduler.
func TestSchedulerDebugScoringExample(t *testing.T) {
	block := jsonFence.FindStringSubmatch(readSection(t, schedulerPage, debugEndpointHeading))
	if block == nil {
		t.Fatalf("%s %q has no json block", schedulerPage, debugEndpointHeading)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(block[1]), &top); err != nil {
		t.Fatalf("the example is not JSON: %v", err)
	}
	var example struct {
		Coefficients map[string]any   `json:"coefficients"`
		Nodes        []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(block[1]), &example); err != nil {
		t.Fatalf("the example does not decode: %v", err)
	}

	raw, err := json.Marshal(powerest.DefaultCoefficients())
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(example.Coefficients, want) {
		t.Errorf("the example's coefficients are %v; DefaultCoefficients writes %v", example.Coefficients, want)
	}

	f := parseGoFile(t, schedulerMain)
	if got, want := sortedKeys(top), structJSONNames(t, f, debugResponseType); !reflect.DeepEqual(got, want) {
		t.Errorf("the example's keys are %v; %s writes %v", got, debugResponseType, want)
	}
	if len(example.Nodes) == 0 {
		t.Fatal("the example has no node")
	}
	wantNode := structJSONNames(t, f, debugNodeResponseType)
	for i, n := range example.Nodes {
		if got := sortedKeys(n); !reflect.DeepEqual(got, wantNode) {
			t.Errorf("node %d of the example has keys %v; %s writes %v", i, got, debugNodeResponseType, wantNode)
		}
	}
}

// structJSONNames returns the sorted JSON names of the fields of the struct
// type name declared in f.
func structJSONNames(t *testing.T, f *ast.File, name string) []string {
	t.Helper()
	var out []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		found = true
		for _, fl := range st.Fields.List {
			for _, id := range fl.Names {
				key := id.Name
				if fl.Tag != nil {
					tag, err := strconv.Unquote(fl.Tag.Value)
					if err != nil {
						t.Fatalf("%s.%s tag: %v", name, id.Name, err)
					}
					if v, ok := reflect.StructTag(tag).Lookup("json"); ok {
						if j, _, _ := strings.Cut(v, ","); j != "" {
							key = j
						}
					}
				}
				out = append(out, key)
			}
		}
		return false
	})
	if !found {
		t.Fatalf("%s declares no struct %s", schedulerMain, name)
	}
	sort.Strings(out)
	return out
}
