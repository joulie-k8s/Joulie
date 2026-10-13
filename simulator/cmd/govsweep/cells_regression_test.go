package main

// Regression test of the efficient arm's default ceiling: an efficient arm
// that names no ceilingPct swept 75 % of the maximum compute clock, a number
// written into the cell expansion, after the preset's default had moved to
// governor.DefaultCeilingPctOfMax.

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
)

// An efficient arm without ceilingPct expands to cells at the preset's
// default ceiling.
func TestEfficientArmWithoutCeilingUsesTheDefault(t *testing.T) {
	c, err := loadConfig(writeConfig(t, `
name: efficient-default
seeds: [7]
durationS: 6
settleS: 2
gpu:
  profiles: [nvidia-l40s-4gpu]
  arms: [{preset: efficient}]
  capsPct: [100]
  receiverGates: [profile]
  classes: {gpu.memory_bound: {memoryIntensity: 0.85}}
  jobs: [{gpus: 2}]
  cpuUtil: 0.05
`))
	if err != nil {
		t.Fatal(err)
	}
	cells := c.expand()
	if len(cells) == 0 {
		t.Fatal("the config expands to no cell")
	}
	for _, cell := range cells {
		if cell.Preset == joulie.GPUGovernorPresetEfficient && cell.CeilingPct != governor.DefaultCeilingPctOfMax {
			t.Errorf("cell %s: ceiling %v %%, want the default %v %%", cell.ID(), cell.CeilingPct, governor.DefaultCeilingPctOfMax)
		}
	}
}
