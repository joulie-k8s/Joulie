package main

// The example node classes the simulator README points to gave the L40S 30 W
// and the W7900 25 W idle per board, while the emulated profiles of the same
// products measure 36.3 W and 10.8 W, the values the generated classes take.

import (
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

const exampleNodeClasses = "../../config/node-classes.yaml"

// TestExampleClassesIdleMatchesProfiles checks that every GPU class of the
// example gives the idle power per board that the builtin profiles of its
// product measure. Products compare as the hardware catalog does, ignoring
// case: the W7900's profile spells it "AMD Radeon Pro W7900".
func TestExampleClassesIdleMatchesProfiles(t *testing.T) {
	clearSimEnv(t)
	classes, err := loadNodeClasses(exampleNodeClasses)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range classes {
		g := c.Model.GPU
		if g == nil || g.Product == nil || g.IdleWattsPerGPU == nil {
			continue
		}
		matched := 0
		for name, p := range profiles {
			if p.GPUs == nil || !strings.EqualFold(labelSafe(p.GPUs.Product.V), labelSafe(*g.Product)) {
				continue
			}
			matched++
			if want := p.Physics.GPU.IdleW.V; *g.IdleWattsPerGPU != want {
				t.Errorf("class %s (%s): idleWattsPerGpu %g, profile %s measures %g", c.Name, *g.Product, *g.IdleWattsPerGPU, name, want)
			}
		}
		if matched == 0 {
			t.Errorf("class %s: no builtin profile has GPU product %q", c.Name, *g.Product)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("%s has no GPU class with a product and an idle power", exampleNodeClasses)
	}
}
