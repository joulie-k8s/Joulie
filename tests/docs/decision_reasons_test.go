package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

const policyPage = "website/content/en/docs/architecture/policy.md"

// TestDecisionReasonsAreDocumented checks that the policy page's "Why a
// node got its state" section names every decision condition and, for each,
// every reason pkg/api lists for it, so a reason added to the code without
// its row fails the build.
func TestDecisionReasonsAreDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, policyPage))
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	start := strings.Index(page, "### Why a node got its state")
	if start < 0 {
		t.Fatalf("%s has no \"Why a node got its state\" section", policyPage)
	}
	section := page[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, condition := range joulie.DecisionConditionTypes {
		at := strings.Index(section, "`"+condition+"`")
		if at < 0 {
			t.Errorf("%s does not name the condition %s", policyPage, condition)
			continue
		}
		// A condition's table follows its name, up to the next paragraph
		// that is not a table row.
		table := section[at:]
		if end := strings.Index(table, "|\n\n"); end >= 0 {
			table = table[:end]
		}
		for _, reason := range joulie.DecisionReasonValues[condition] {
			if !strings.Contains(table, "| `"+reason+"` |") {
				t.Errorf("%s: the table of %s has no row for reason %s", policyPage, condition, reason)
			}
		}
	}
}
