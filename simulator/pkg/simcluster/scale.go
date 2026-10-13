package simcluster

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// ScaleSpecName is the metadata.name of every scale spec.
const ScaleSpecName = "scale"

// ScaleSpec writes a cluster spec of n managed Nodes of one profile, in the
// schema of package clusterspec: one pool named after the profile, so its
// Nodes are <profile>-0 to <profile>-<n-1>, under the background load of
// the medium spec. The spec is validated (clusterspec Validate); whether
// the profile exists is for Expand to say, with the caller's profiles. The
// profile name is the pool name, so it must be a DNS-1123 label.
func ScaleSpec(n int, profile string) ([]byte, error) {
	if n < 1 {
		return nil, fmt.Errorf("scale spec: %d Nodes, want at least 1", n)
	}
	if msgs := validation.IsDNS1123Label(profile); len(msgs) > 0 {
		return nil, fmt.Errorf("scale spec: profile name %q: %s", profile, strings.Join(msgs, "; "))
	}
	b := []byte(fmt.Sprintf(`# Scale cluster spec: %d Nodes of profile %s, written by simcluster
# scale-spec.
apiVersion: %s
kind: %s
metadata: {name: %s}
spec:
  defaults:
    labels: {joulie.io/managed: "true"}
    baseLoad: {cpuUtil: 0.8, gpuUtil: 0.9, memoryIntensity: 0.2, cpuClass: cpu.compute_bound, gpuClass: gpu.compute_bound}
  pools:
  - {name: %s, profile: %s, count: %d}
`, n, profile, clusterspec.APIVersion, clusterspec.Kind, ScaleSpecName, profile, profile, n))
	s, err := clusterspec.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("scale spec of profile %q: %w", profile, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("scale spec of profile %q: %w", profile, err)
	}
	return b, nil
}
