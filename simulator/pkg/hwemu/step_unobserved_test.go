package hwemu

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// treeFiles is every regular file of a tree under root, by its path
// relative to root, with its content.
func treeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// StepUnobserved advances a node exactly as Step does: two nodes of the
// same profile, one stepped with Step and one with StepUnobserved through
// the same load changes, report the same State after every step, and once
// the second is flushed their trees hold the same files.
func TestStepUnobservedMatchesStep(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"intel-xeon-6530-2s-rapl", "amd-epyc-9534-2s-energy-only", "nvidia-l40s-4gpu", mi300xProfile} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ensureBuiltin(t, name)
			trees := [2]*Tree{renderTestTree(t, p), renderTestTree(t, p)}
			var nodes [2]*Node
			for i, tr := range trees {
				n, err := NewNode(p, tr, Options{Start: nodeStart, Substep: 10 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				nodes[i] = n
			}
			gpus := 0
			if p.GPUs != nil {
				gpus = p.GPUs.Count.V
			}
			for k := 0; k < 300; k++ {
				if k%40 == 0 {
					l := Load{}
					if k%80 == 0 {
						util := make([]float64, gpus)
						for i := range util {
							util[i] = 1
						}
						l = Load{CPUUtil: []float64{1}, GPUUtil: util, MemoryIntensity: 0.5}
					}
					nodes[0].SetLoad(l)
					nodes[1].SetLoad(l)
				}
				if err := nodes[0].Step(10 * time.Millisecond); err != nil {
					t.Fatal(err)
				}
				if err := nodes[1].StepUnobserved(10 * time.Millisecond); err != nil {
					t.Fatal(err)
				}
				if a, b := nodes[0].State(), nodes[1].State(); !reflect.DeepEqual(a, b) {
					t.Fatalf("step %d: State after StepUnobserved differs from State after Step:\n%+v\n%+v", k, b, a)
				}
			}
			if err := nodes[1].Flush(); err != nil {
				t.Fatal(err)
			}
			a, b := treeFiles(t, trees[0].Root), treeFiles(t, trees[1].Root)
			for rel, c := range a {
				if b[rel] != c {
					t.Errorf("%s holds %q after StepUnobserved and Flush, %q after Step", rel, b[rel], c)
				}
			}
			if len(a) != len(b) {
				t.Errorf("%d files after StepUnobserved and Flush, %d after Step", len(b), len(a))
			}
		})
	}
}
