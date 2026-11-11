package docs

// Every alert the chart ships links to its runbook. A renamed heading or a
// moved page breaks the link without any error, and an alert that fires
// with a dead runbook_url leaves the operator, or the agent following it,
// with nothing to act on.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	alertRulesFile = "charts/joulie/alerts/joulie-alerts.yaml"
	runbooksPage   = "website/content/en/docs/getting-started/07-runbooks.md"
	docsSiteURL    = "https://joulie-k8s.github.io/Joulie/docs/"
)

// runbookSections are the "## " headings of the runbooks page, keyed by the
// anchor Hugo gives them: lowercased, spaces as hyphens, other punctuation
// dropped.
func runbookSections(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, runbooksPage))
	if err != nil {
		t.Fatal(err)
	}
	drop := regexp.MustCompile(`[^a-z0-9 -]`)
	out := map[string]string{}
	var anchor string
	for _, line := range strings.Split(string(b), "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			anchor = strings.ReplaceAll(drop.ReplaceAllString(strings.ToLower(h), ""), " ", "-")
			out[anchor] = ""
			continue
		}
		if anchor != "" {
			out[anchor] += line + "\n"
		}
	}
	return out
}

// TestAlertRunbooksExist: every alert's runbook_url is a section of the
// runbooks page named after the alert, and the section says what the alert
// means, how to confirm it, its causes and its fix, and quotes the
// expression the rule evaluates and its for: duration.
func TestAlertRunbooksExist(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, alertRulesFile))
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `json:"alert"`
				Expr        string            `json:"expr"`
				For         string            `json:"for"`
				Annotations map[string]string `json:"annotations"`
			} `json:"rules"`
		} `json:"groups"`
	}
	if err := yaml.Unmarshal(b, &rules); err != nil {
		t.Fatal(err)
	}
	pageURL := docsSiteURL + strings.TrimSuffix(strings.TrimPrefix(runbooksPage, "website/content/en/docs/"), ".md") + "/"
	sections := runbookSections(t)
	n := 0
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			n++
			want := pageURL + "#" + strings.ToLower(r.Alert)
			if got := r.Annotations["runbook_url"]; got != want {
				t.Errorf("%s: runbook_url %q, want %q", r.Alert, got, want)
			}
			section, ok := sections[strings.ToLower(r.Alert)]
			if !ok {
				t.Errorf("%s: %s has no section \"## %s\"", r.Alert, runbooksPage, r.Alert)
				continue
			}
			for _, part := range []string{"**Meaning.**", "**Expression.**", "**Confirm.**", "**Likely causes.**", "**Fix.**"} {
				if !strings.Contains(section, part) {
					t.Errorf("%s: its runbook has no %s paragraph", r.Alert, part)
				}
			}
			if expr := strings.TrimSpace(r.Expr); !strings.Contains(section, "`"+expr+"`") {
				t.Errorf("%s: its runbook does not quote the expression `%s`", r.Alert, expr)
			}
			if minutes := strings.TrimSuffix(r.For, "m"); !strings.Contains(section, "for "+minutes+" minutes") {
				t.Errorf("%s: its runbook does not give the for: duration as \"for %s minutes\"", r.Alert, minutes)
			}
		}
	}
	if n == 0 {
		t.Fatalf("%s holds no rule", alertRulesFile)
	}
}
