package simcluster

import (
	"fmt"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// ScaleSpec writes a valid spec of n managed Nodes of the profile, which
// Expand renders with the builtin profiles.
func TestScaleSpec(t *testing.T) {
	b, err := ScaleSpec(200, "intel-xeon-2s-rapl")
	if err != nil {
		t.Fatal(err)
	}
	s, err := clusterspec.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	if s.Metadata.Name != ScaleSpecName || len(members) != 200 {
		t.Fatalf("spec %s with %d Nodes, want %s with 200", s.Metadata.Name, len(members), ScaleSpecName)
	}
	names := map[string]bool{}
	for _, m := range members {
		names[m.Name] = true
		if m.Node.Labels["joulie.io/managed"] != "true" || m.Profile.Name != "intel-xeon-2s-rapl" {
			t.Fatalf("Node %s: managed %q, profile %s", m.Name, m.Node.Labels["joulie.io/managed"], m.Profile.Name)
		}
	}
	for _, n := range []string{"intel-xeon-2s-rapl-0", "intel-xeon-2s-rapl-199"} {
		if !names[n] {
			t.Errorf("no Node %s", n)
		}
	}
	if len(WaitNodes(members)) != 200 {
		t.Errorf("%d of 200 Nodes have measured CPU packages", len(WaitNodes(members)))
	}

	for _, tc := range []struct {
		n       int
		profile string
		want    string
	}{
		{0, "intel-xeon-2s-rapl", "want at least 1"},
		{1, "Intel_Xeon", "profile name"},
		{1, "x, count: 5}", "profile name"},
		{1, strings.Repeat("a", 64), "profile name"},
	} {
		if _, err := ScaleSpec(tc.n, tc.profile); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ScaleSpec(%d, %q): error %v, want one containing %q", tc.n, tc.profile, err, tc.want)
		}
	}
	// A profile name of 61 characters leaves no room for "-10": Validate
	// refuses the Node names.
	if _, err := ScaleSpec(11, strings.Repeat("a", 61)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("Node name %q", strings.Repeat("a", 61)+"-10")) {
		t.Errorf("ScaleSpec with Node names over 63 characters: error %v", err)
	}
}
